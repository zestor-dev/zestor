// Package storetest is the conformance suite for store.Store implementations.
//
// It is exported rather than internal so that out-of-tree backends can prove
// themselves against the same contract. A backend supplies a constructor and
// declares which optional capabilities it supports; everything it does not opt
// out of is mandatory.
//
//	func TestConformance(t *testing.T) {
//	    storetest.Run(t, storetest.Config{
//	        New: func(t *testing.T) store.Store[storetest.Value] { ... },
//	        NewWithOptions: ...,   // omit to skip validation / compare cases
//	    })
//	}
//
// TODO(FIX-PLAN): cases for T0.4 (phantom kinds), C4 (SetResult), C5 (SetMany
// rename + no-op suppression in bulk writes) and C6 (Dumper) land with those
// items. Everything below is the contract as of C1/C2/C3/W1.
package storetest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/zestor-dev/zestor/store"
)

// Value is the element type every backend is exercised with. The struct tags
// keep it usable with the JSON and YAML codecs.
type Value struct {
	Name string `json:"name" yaml:"name"`
	N    int    `json:"n" yaml:"n"`
}

// Config describes the backend under test.
type Config struct {
	// New returns a fresh, empty store. The suite closes it.
	New func(t *testing.T) store.Store[Value]

	// NewWithOptions returns a fresh, empty store honouring opts. Leave nil if
	// the backend does not support StoreOptions; the validation and compare
	// cases are then skipped, and the skip is visible in the test output.
	NewWithOptions func(t *testing.T, opts store.StoreOptions[Value]) store.Store[Value]

	// EventDelay bounds how long an event may take to reach a watcher. Backends
	// that publish through a database round-trip need a generous value.
	// Defaults to 2s.
	EventDelay time.Duration

	// SettleDelay is how long the suite waits before concluding that no event
	// arrived. Defaults to EventDelay.
	SettleDelay time.Duration
}

func (c Config) eventDelay() time.Duration {
	if c.EventDelay > 0 {
		return c.EventDelay
	}
	return 2 * time.Second
}

func (c Config) settleDelay() time.Duration {
	if c.SettleDelay > 0 {
		return c.SettleDelay
	}
	return c.eventDelay()
}

// Run executes the full conformance suite.
func Run(t *testing.T, cfg Config) {
	t.Helper()
	if cfg.New == nil {
		t.Fatal("storetest: Config.New is required")
	}

	cases := []struct {
		name string
		fn   func(*testing.T, Config)
	}{
		// --- read / write semantics -------------------------------------
		{"GetMissingKey", testGetMissingKey},
		{"SetCreatesThenUpdates", testSetCreatesThenUpdates},
		{"SetNoOpIsNotAChange", testSetNoOpIsNotAChange},
		{"DeleteReturnsPrevious", testDeleteReturnsPrevious},
		{"DeleteMissingKey", testDeleteMissingKey},
		{"SetFnMissingKey", testSetFnMissingKey},
		{"SetFnReportsChanged", testSetFnReportsChanged},
		{"SetFnPropagatesError", testSetFnPropagatesError},
		{"SetAllMergesIntoExisting", testSetAllMergesIntoExisting},
		{"ListAppliesFilters", testListAppliesFilters},
		{"ReadersAgree", testReadersAgree},
		{"KindsAreIsolated", testKindsAreIsolated},

		// --- context (C2) -----------------------------------------------
		{"CancelledContextIsRejected", testCancelledContextIsRejected},

		// --- watch contract (C1, W1) ------------------------------------
		{"WatchRequiresKind", testWatchRequiresKind},
		{"WatchDeliversAllEventTypes", testWatchDeliversAllEventTypes},
		{"WatchFiltersEventTypes", testWatchFiltersEventTypes},
		{"WatchInitialReplay", testWatchInitialReplay},
		{"WatchReplayPrecedesLive", testWatchReplayPrecedesLive},
		{"WatchPreservesWriteOrder", testWatchPreservesWriteOrder},
		{"WatchOverflowIsSignalled", testWatchOverflowIsSignalled},
		{"WatchOverflowBypassesTypeFilter", testWatchOverflowBypassesTypeFilter},
		{"WatchContextCancelClosesChannel", testWatchContextCancelClosesChannel},
		{"WatchMultipleWatchersAreIndependent", testWatchMultipleWatchersAreIndependent},

		// --- lifecycle ---------------------------------------------------
		{"CloseClosesWatchers", testCloseClosesWatchers},
		{"CloseRejectsOperations", testCloseRejectsOperations},
		{"CloseIsIdempotent", testCloseIsIdempotent},

		// --- optional capabilities ---------------------------------------
		{"ValidationRejectsWrites", testValidationRejectsWrites},
		{"CompareFnSuppressesUpdates", testCompareFnSuppressesUpdates},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { tc.fn(t, cfg) })
	}
}

// -----------------------------------------------------------------------------
// helpers
// -----------------------------------------------------------------------------

func newStore(t *testing.T, cfg Config) store.Store[Value] {
	t.Helper()
	s := cfg.New(t)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// watchCtx starts a watch whose lifetime is bound to the test.
func watchCtx(t *testing.T, cfg Config, s store.Store[Value], kind string, opts ...store.WatchOption[Value]) <-chan *store.Event[Value] {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ch, err := s.Watch(ctx, kind, opts...)
	if err != nil {
		t.Fatalf("Watch(%q): %v", kind, err)
	}
	return ch
}

// recv waits up to EventDelay for one event.
func recv(t *testing.T, cfg Config, ch <-chan *store.Event[Value]) *store.Event[Value] {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("watch channel closed while an event was expected")
		}
		return ev
	case <-time.After(cfg.eventDelay()):
		t.Fatalf("timed out after %v waiting for an event", cfg.eventDelay())
		return nil
	}
}

// recvN collects n events, failing if they do not all arrive.
func recvN(t *testing.T, cfg Config, ch <-chan *store.Event[Value], n int) []*store.Event[Value] {
	t.Helper()
	out := make([]*store.Event[Value], 0, n)
	for range n {
		out = append(out, recv(t, cfg, ch))
	}
	return out
}

// expectNoEvent fails if anything arrives within SettleDelay.
func expectNoEvent(t *testing.T, cfg Config, ch <-chan *store.Event[Value]) {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("watch channel closed while no event was expected")
		}
		t.Fatalf("unexpected event %s %s/%s", ev.EventType, ev.Kind, ev.Name)
	case <-time.After(cfg.settleDelay()):
	}
}

// drainUntilClosed reads every remaining event, failing if the channel does not
// close within EventDelay.
func drainUntilClosed(t *testing.T, cfg Config, ch <-chan *store.Event[Value]) []*store.Event[Value] {
	t.Helper()
	var out []*store.Event[Value]
	deadline := time.After(cfg.eventDelay())
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-deadline:
			t.Fatalf("watch channel was not closed within %v (got %d events)", cfg.eventDelay(), len(out))
			return out
		}
	}
}

func mustSet(t *testing.T, s store.Store[Value], kind, key string, v Value) {
	t.Helper()
	if _, err := s.Set(context.Background(), kind, key, v); err != nil {
		t.Fatalf("Set(%q,%q): %v", kind, key, err)
	}
}

// -----------------------------------------------------------------------------
// read / write semantics
// -----------------------------------------------------------------------------

func testGetMissingKey(t *testing.T, cfg Config) {
	s := newStore(t, cfg)
	v, ok, err := s.Get(context.Background(), "k", "nope")
	if err != nil {
		t.Fatalf("Get on a missing key must not error, got %v", err)
	}
	if ok {
		t.Errorf("Get reported ok=true for a missing key (value %+v)", v)
	}
}

func testSetCreatesThenUpdates(t *testing.T, cfg Config) {
	s := newStore(t, cfg)
	ctx := context.Background()

	created, err := s.Set(ctx, "k", "a", Value{Name: "a", N: 1})
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	if !created {
		t.Error("first Set must report created=true")
	}

	created, err = s.Set(ctx, "k", "a", Value{Name: "a", N: 2})
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	if created {
		t.Error("second Set must report created=false")
	}

	got, ok, err := s.Get(ctx, "k", "a")
	if err != nil || !ok {
		t.Fatalf("Get: err=%v ok=%v", err, ok)
	}
	if got.N != 2 {
		t.Errorf("Get returned N=%d, want 2", got.N)
	}
}

func testSetNoOpIsNotAChange(t *testing.T, cfg Config) {
	s := newStore(t, cfg)
	v := Value{Name: "a", N: 1}
	mustSet(t, s, "k", "a", v)

	ch := watchCtx(t, cfg, s, "k")
	if _, err := s.Set(context.Background(), "k", "a", v); err != nil {
		t.Fatalf("Set: %v", err)
	}
	expectNoEvent(t, cfg, ch)
}

func testDeleteReturnsPrevious(t *testing.T, cfg Config) {
	s := newStore(t, cfg)
	ctx := context.Background()
	mustSet(t, s, "k", "a", Value{Name: "a", N: 7})

	existed, prev, err := s.Delete(ctx, "k", "a")
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if !existed {
		t.Fatal("Delete must report existed=true for a present key")
	}
	if prev.N != 7 {
		t.Errorf("Delete returned prev.N=%d, want 7", prev.N)
	}
	if _, ok, _ := s.Get(ctx, "k", "a"); ok {
		t.Error("key is still readable after Delete")
	}
}

func testDeleteMissingKey(t *testing.T, cfg Config) {
	s := newStore(t, cfg)
	existed, _, err := s.Delete(context.Background(), "k", "nope")
	if err != nil {
		t.Fatalf("Delete on a missing key must not error, got %v", err)
	}
	if existed {
		t.Error("Delete reported existed=true for a missing key")
	}
}

func testSetFnMissingKey(t *testing.T, cfg Config) {
	s := newStore(t, cfg)
	_, err := s.SetFn(context.Background(), "k", "nope", func(v Value) (Value, error) { return v, nil })
	if !errors.Is(err, store.ErrKeyNotFound) {
		t.Errorf("SetFn on a missing key returned %v, want ErrKeyNotFound", err)
	}
}

// testSetFnReportsChanged pins C3: the documented `changed` return must reflect
// whether the stored value actually changed.
func testSetFnReportsChanged(t *testing.T, cfg Config) {
	s := newStore(t, cfg)
	ctx := context.Background()
	mustSet(t, s, "k", "a", Value{Name: "a", N: 1})

	changed, err := s.SetFn(ctx, "k", "a", func(v Value) (Value, error) {
		v.N++
		return v, nil
	})
	if err != nil {
		t.Fatalf("SetFn: %v", err)
	}
	got, _, _ := s.Get(ctx, "k", "a")
	if got.N != 2 {
		t.Fatalf("SetFn did not apply the transform: N=%d, want 2", got.N)
	}
	if !changed {
		t.Error("SetFn changed the stored value but reported changed=false")
	}

	changed, err = s.SetFn(ctx, "k", "a", func(v Value) (Value, error) { return v, nil })
	if err != nil {
		t.Fatalf("SetFn: %v", err)
	}
	if changed {
		t.Error("SetFn reported changed=true for a no-op transform")
	}
}

func testSetFnPropagatesError(t *testing.T, cfg Config) {
	s := newStore(t, cfg)
	ctx := context.Background()
	mustSet(t, s, "k", "a", Value{Name: "a", N: 1})

	sentinel := errors.New("nope")
	changed, err := s.SetFn(ctx, "k", "a", func(v Value) (Value, error) {
		v.N = 99
		return v, sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("SetFn returned %v, want the transform's error", err)
	}
	if changed {
		t.Error("SetFn reported changed=true after the transform failed")
	}
	if got, _, _ := s.Get(ctx, "k", "a"); got.N != 1 {
		t.Errorf("a failed transform was persisted: N=%d, want 1", got.N)
	}
}

func testSetAllMergesIntoExisting(t *testing.T, cfg Config) {
	s := newStore(t, cfg)
	ctx := context.Background()
	mustSet(t, s, "k", "keep", Value{Name: "keep", N: 1})

	if err := s.SetAll(ctx, "k", map[string]Value{
		"a": {Name: "a", N: 1},
		"b": {Name: "b", N: 2},
	}); err != nil {
		t.Fatalf("SetAll: %v", err)
	}

	n, err := s.Count(ctx, "k")
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != 3 {
		t.Errorf("Count=%d, want 3 — SetAll merges, it does not replace the kind", n)
	}
	if _, ok, _ := s.Get(ctx, "k", "keep"); !ok {
		t.Error("SetAll removed a key that was not in the supplied map")
	}
}

func testListAppliesFilters(t *testing.T, cfg Config) {
	s := newStore(t, cfg)
	ctx := context.Background()
	for i := range 6 {
		mustSet(t, s, "k", fmt.Sprintf("k%d", i), Value{Name: "v", N: i})
	}

	got, err := s.List(ctx, "k", func(_ string, v Value) bool { return v.N%2 == 0 })
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("List with one filter returned %d values, want 3", len(got))
	}

	got, err = s.List(ctx, "k",
		func(_ string, v Value) bool { return v.N%2 == 0 },
		func(_ string, v Value) bool { return v.N >= 2 },
	)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	// {0,2,4} AND {2,3,4,5} == {2,4}
	if len(got) != 2 {
		t.Errorf("filters must compose with AND: got %d values, want 2", len(got))
	}
}

func testReadersAgree(t *testing.T, cfg Config) {
	s := newStore(t, cfg)
	ctx := context.Background()
	for i := range 4 {
		mustSet(t, s, "k", fmt.Sprintf("k%d", i), Value{Name: "v", N: i})
	}

	list, err := s.List(ctx, "k")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	count, err := s.Count(ctx, "k")
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	keys, err := s.Keys(ctx, "k")
	if err != nil {
		t.Fatalf("Keys: %v", err)
	}
	values, err := s.Values(ctx, "k")
	if err != nil {
		t.Fatalf("Values: %v", err)
	}
	all, err := s.GetAll(ctx)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}

	if len(list) != 4 || count != 4 || len(keys) != 4 || len(values) != 4 || len(all["k"]) != 4 {
		t.Errorf("readers disagree: List=%d Count=%d Keys=%d Values=%d GetAll=%d, want 4",
			len(list), count, len(keys), len(values), len(all["k"]))
	}
	for _, kv := range values {
		if list[kv.Key] != kv.Value {
			t.Errorf("Values[%q]=%+v disagrees with List[%q]=%+v", kv.Key, kv.Value, kv.Key, list[kv.Key])
		}
	}
}

func testKindsAreIsolated(t *testing.T, cfg Config) {
	s := newStore(t, cfg)
	ctx := context.Background()
	mustSet(t, s, "a", "same", Value{Name: "in-a", N: 1})
	mustSet(t, s, "b", "same", Value{Name: "in-b", N: 2})

	va, _, _ := s.Get(ctx, "a", "same")
	vb, _, _ := s.Get(ctx, "b", "same")
	if va.Name != "in-a" || vb.Name != "in-b" {
		t.Errorf("kinds are not isolated: a=%+v b=%+v", va, vb)
	}

	if _, _, err := s.Delete(ctx, "a", "same"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok, _ := s.Get(ctx, "b", "same"); !ok {
		t.Error("deleting from one kind removed the same key from another")
	}
}

// -----------------------------------------------------------------------------
// context (C2)
// -----------------------------------------------------------------------------

func testCancelledContextIsRejected(t *testing.T, cfg Config) {
	s := newStore(t, cfg)
	mustSet(t, s, "k", "a", Value{Name: "a", N: 1})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, _, err := s.Get(ctx, "k", "a"); !errors.Is(err, context.Canceled) {
		t.Errorf("Get with a cancelled context returned %v, want context.Canceled", err)
	}
	if _, err := s.Set(ctx, "k", "b", Value{Name: "b"}); !errors.Is(err, context.Canceled) {
		t.Errorf("Set with a cancelled context returned %v, want context.Canceled", err)
	}
	if _, err := s.List(ctx, "k"); !errors.Is(err, context.Canceled) {
		t.Errorf("List with a cancelled context returned %v, want context.Canceled", err)
	}
	if _, _, err := s.Delete(ctx, "k", "a"); !errors.Is(err, context.Canceled) {
		t.Errorf("Delete with a cancelled context returned %v, want context.Canceled", err)
	}
}

// -----------------------------------------------------------------------------
// watch contract (C1, W1)
// -----------------------------------------------------------------------------

func testWatchRequiresKind(t *testing.T, cfg Config) {
	s := newStore(t, cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := s.Watch(ctx, ""); !errors.Is(err, store.ErrKindRequired) {
		t.Errorf("Watch with an empty kind returned %v, want ErrKindRequired", err)
	}
}

func testWatchDeliversAllEventTypes(t *testing.T, cfg Config) {
	s := newStore(t, cfg)
	ctx := context.Background()
	ch := watchCtx(t, cfg, s, "k")

	mustSet(t, s, "k", "a", Value{Name: "a", N: 1})
	mustSet(t, s, "k", "a", Value{Name: "a", N: 2})
	if _, _, err := s.Delete(ctx, "k", "a"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	evs := recvN(t, cfg, ch, 3)
	want := []store.EventType{store.EventTypeCreate, store.EventTypeUpdate, store.EventTypeDelete}
	for i, ev := range evs {
		if ev.EventType != want[i] {
			t.Errorf("event %d is %s, want %s", i, ev.EventType, want[i])
		}
		if ev.Kind != "k" || ev.Name != "a" {
			t.Errorf("event %d has kind/name %s/%s, want k/a", i, ev.Kind, ev.Name)
		}
	}
	if evs[1].Object.N != 2 {
		t.Errorf("update event carries N=%d, want the new value 2", evs[1].Object.N)
	}
	if evs[2].Object.N != 2 {
		t.Errorf("delete event carries N=%d, want the previous value 2", evs[2].Object.N)
	}
}

func testWatchFiltersEventTypes(t *testing.T, cfg Config) {
	s := newStore(t, cfg)
	ctx := context.Background()
	ch := watchCtx(t, cfg, s, "k", store.WithEventTypes[Value](store.EventTypeDelete))

	mustSet(t, s, "k", "a", Value{Name: "a", N: 1})
	mustSet(t, s, "k", "a", Value{Name: "a", N: 2})
	if _, _, err := s.Delete(ctx, "k", "a"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	ev := recv(t, cfg, ch)
	if ev.EventType != store.EventTypeDelete {
		t.Fatalf("first delivered event is %s, want only delete events", ev.EventType)
	}
	expectNoEvent(t, cfg, ch)
}

func testWatchInitialReplay(t *testing.T, cfg Config) {
	s := newStore(t, cfg)
	for i := range 3 {
		mustSet(t, s, "k", fmt.Sprintf("k%d", i), Value{Name: "v", N: i})
	}

	ch := watchCtx(t, cfg, s, "k", store.WithInitialReplay[Value]())
	evs := recvN(t, cfg, ch, 3)
	seen := map[string]bool{}
	for _, ev := range evs {
		if ev.EventType != store.EventTypeCreate {
			t.Errorf("replayed event for %s is %s, want create", ev.Name, ev.EventType)
		}
		seen[ev.Name] = true
	}
	for i := range 3 {
		if !seen[fmt.Sprintf("k%d", i)] {
			t.Errorf("initial replay omitted k%d", i)
		}
	}
}

// testWatchReplayPrecedesLive pins C1 guarantee 2. A watcher that applies events
// in receive order must not have a replayed value overwrite a newer live one.
//
// An implementation that replays from its own goroutine only loses this race
// some of the time, so the subscribe-then-write window is exercised repeatedly.
// The rounds reuse one populated kind: each adds a single write, not a reload.
func testWatchReplayPrecedesLive(t *testing.T, cfg Config) {
	s := newStore(t, cfg)
	const n = 60
	for i := range n {
		mustSet(t, s, "k", fmt.Sprintf("k%03d", i), Value{Name: "old", N: i})
	}

	const rounds = 5
	for round := range rounds {
		// A distinct key per round, so a round's live write is still part of the
		// next round's snapshot.
		key := fmt.Sprintf("k%03d", round)

		wctx, cancel := context.WithCancel(context.Background())
		ch, err := s.Watch(wctx, "k",
			store.WithInitialReplay[Value](),
			store.WithBufferSize[Value](4*n),
		)
		if err != nil {
			cancel()
			t.Fatalf("Watch: %v", err)
		}

		// A live write that supersedes one of the replayed keys.
		mustSet(t, s, "k", key, Value{Name: "new", N: 1000 + round})

		state := map[string]Value{}
		got := 0
		deadline := time.After(cfg.eventDelay())
		for got < n+1 {
			select {
			case ev, ok := <-ch:
				if !ok {
					cancel()
					t.Fatalf("round %d: channel closed after %d of %d events", round, got, n+1)
				}
				if ev.Err != nil {
					cancel()
					t.Fatalf("round %d: watch terminated early: %v", round, ev.Err)
				}
				state[ev.Name] = ev.Object
				got++
			case <-deadline:
				cancel()
				t.Fatalf("round %d: received %d of %d events before timing out", round, got, n+1)
			}
		}
		cancel()

		if state[key].Name != "new" {
			t.Fatalf("round %d: the replayed value won: a consumer applying events in order ended up "+
				"with %+v for %s, but the store holds the newer value", round, state[key], key)
		}
	}
}

// testWatchPreservesWriteOrder pins C1 guarantee 1 against concurrent writers.
//
// Each round starts from an absent key, so two racing Sets always produce
// exactly two events (a create then an update) whichever order they land in.
// The last one delivered must agree with what the store ended up holding.
func testWatchPreservesWriteOrder(t *testing.T, cfg Config) {
	s := newStore(t, cfg)
	ctx := context.Background()

	const rounds = 25
	for round := range rounds {
		wctx, cancel := context.WithCancel(ctx)
		ch, err := s.Watch(wctx, "k", store.WithBufferSize[Value](64))
		if err != nil {
			cancel()
			t.Fatalf("Watch: %v", err)
		}

		var wg sync.WaitGroup
		for i := 1; i <= 2; i++ {
			wg.Add(1)
			go func(n int) {
				defer wg.Done()
				_, _ = s.Set(ctx, "k", "a", Value{Name: "a", N: n})
			}(i)
		}
		wg.Wait()

		evs := recvN(t, cfg, ch, 2)
		last := evs[1]
		if last.Err != nil {
			cancel()
			t.Fatalf("watch terminated: %v", last.Err)
		}

		final, _, err := s.Get(ctx, "k", "a")
		if err != nil {
			cancel()
			t.Fatalf("Get: %v", err)
		}
		if last.Object != final {
			cancel()
			t.Fatalf("round %d: the last event delivered was %+v but the store holds %+v — "+
				"a watcher rebuilding state is now permanently wrong", round, last.Object, final)
		}

		cancel()
		if _, _, err := s.Delete(ctx, "k", "a"); err != nil {
			t.Fatalf("Delete: %v", err)
		}
	}
}

// testWatchOverflowIsSignalled pins C1 guarantee 3: falling behind must never be
// a silent gap.
func testWatchOverflowIsSignalled(t *testing.T, cfg Config) {
	s := newStore(t, cfg)
	ch := watchCtx(t, cfg, s, "k", store.WithBufferSize[Value](4))

	// Never read while writing, so the consumer is guaranteed to fall behind.
	for i := range 100 {
		mustSet(t, s, "k", fmt.Sprintf("k%03d", i), Value{Name: "v", N: i})
	}

	evs := drainUntilClosed(t, cfg, ch)
	if len(evs) == 0 {
		t.Fatal("watch delivered nothing at all")
	}
	last := evs[len(evs)-1]
	if last.EventType != store.EventTypeError {
		t.Fatalf("a watcher that fell behind received %d events ending in %s, with no indication "+
			"that its view is incomplete; want a terminal EventTypeError",
			len(evs), last.EventType)
	}
	if !errors.Is(last.Err, store.ErrWatchOverflow) {
		t.Errorf("terminal event carries Err=%v, want ErrWatchOverflow", last.Err)
	}
	for _, ev := range evs[:len(evs)-1] {
		if ev.EventType == store.EventTypeError {
			t.Error("EventTypeError must be the final event on the channel")
		}
	}
}

// testWatchOverflowBypassesTypeFilter: a consumer that filtered the terminal
// event away could not learn that its view had become incomplete.
func testWatchOverflowBypassesTypeFilter(t *testing.T, cfg Config) {
	s := newStore(t, cfg)
	ch := watchCtx(t, cfg, s, "k",
		store.WithBufferSize[Value](4),
		store.WithEventTypes[Value](store.EventTypeCreate),
	)

	for i := range 100 {
		mustSet(t, s, "k", fmt.Sprintf("k%03d", i), Value{Name: "v", N: i})
	}

	evs := drainUntilClosed(t, cfg, ch)
	if len(evs) == 0 || evs[len(evs)-1].EventType != store.EventTypeError {
		t.Fatalf("a watcher filtering on create events received %d events and never learned it overflowed", len(evs))
	}
}

func testWatchContextCancelClosesChannel(t *testing.T, cfg Config) {
	s := newStore(t, cfg)
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := s.Watch(ctx, "k")
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	mustSet(t, s, "k", "a", Value{Name: "a", N: 1})
	recv(t, cfg, ch)

	cancel()
	drainUntilClosed(t, cfg, ch)

	// Cancelling again, and writing afterwards, must not panic.
	cancel()
	mustSet(t, s, "k", "b", Value{Name: "b", N: 2})
}

func testWatchMultipleWatchersAreIndependent(t *testing.T, cfg Config) {
	s := newStore(t, cfg)
	all := watchCtx(t, cfg, s, "k")
	deletes := watchCtx(t, cfg, s, "k", store.WithEventTypes[Value](store.EventTypeDelete))
	other := watchCtx(t, cfg, s, "other")

	mustSet(t, s, "k", "a", Value{Name: "a", N: 1})
	if _, _, err := s.Delete(context.Background(), "k", "a"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	evs := recvN(t, cfg, all, 2)
	if evs[0].EventType != store.EventTypeCreate || evs[1].EventType != store.EventTypeDelete {
		t.Errorf("unfiltered watcher saw %s,%s; want create,delete", evs[0].EventType, evs[1].EventType)
	}
	if ev := recv(t, cfg, deletes); ev.EventType != store.EventTypeDelete {
		t.Errorf("filtered watcher saw %s, want delete", ev.EventType)
	}
	expectNoEvent(t, cfg, other)
}

// -----------------------------------------------------------------------------
// lifecycle
// -----------------------------------------------------------------------------

func testCloseClosesWatchers(t *testing.T, cfg Config) {
	s := cfg.New(t)
	ch := watchCtx(t, cfg, s, "k")
	mustSet(t, s, "k", "a", Value{Name: "a", N: 1})

	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	drainUntilClosed(t, cfg, ch)
}

func testCloseRejectsOperations(t *testing.T, cfg Config) {
	s := cfg.New(t)
	mustSet(t, s, "k", "a", Value{Name: "a", N: 1})
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	ctx := context.Background()
	if _, _, err := s.Get(ctx, "k", "a"); !errors.Is(err, store.ErrClosed) {
		t.Errorf("Get after Close returned %v, want ErrClosed", err)
	}
	if _, err := s.Set(ctx, "k", "b", Value{Name: "b"}); !errors.Is(err, store.ErrClosed) {
		t.Errorf("Set after Close returned %v, want ErrClosed", err)
	}
	if _, err := s.List(ctx, "k"); !errors.Is(err, store.ErrClosed) {
		t.Errorf("List after Close returned %v, want ErrClosed", err)
	}
	if _, err := s.Count(ctx, "k"); !errors.Is(err, store.ErrClosed) {
		t.Errorf("Count after Close returned %v, want ErrClosed", err)
	}
	if _, _, err := s.Delete(ctx, "k", "a"); !errors.Is(err, store.ErrClosed) {
		t.Errorf("Delete after Close returned %v, want ErrClosed", err)
	}
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if _, err := s.Watch(wctx, "k"); !errors.Is(err, store.ErrClosed) {
		t.Errorf("Watch after Close returned %v, want ErrClosed", err)
	}
}

func testCloseIsIdempotent(t *testing.T, cfg Config) {
	s := cfg.New(t)
	if err := s.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// -----------------------------------------------------------------------------
// optional capabilities
// -----------------------------------------------------------------------------

func testValidationRejectsWrites(t *testing.T, cfg Config) {
	if cfg.NewWithOptions == nil {
		t.Skip("backend does not accept store.StoreOptions (see FIX-PLAN X3)")
	}
	sentinel := errors.New("invalid")
	s := cfg.NewWithOptions(t, store.StoreOptions[Value]{
		ValidateFns: map[string]store.ValidateFunc[Value]{
			"k": func(v Value) error {
				if v.Name == "" {
					return sentinel
				}
				return nil
			},
		},
	})
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	if _, err := s.Set(ctx, "k", "bad", Value{}); !errors.Is(err, sentinel) {
		t.Errorf("Set of an invalid value returned %v, want the validation error", err)
	}
	if _, ok, _ := s.Get(ctx, "k", "bad"); ok {
		t.Error("an invalid value was persisted")
	}
	if _, err := s.Set(ctx, "k", "ok", Value{Name: "fine"}); err != nil {
		t.Errorf("Set of a valid value returned %v", err)
	}
	// A kind with no validator is unconstrained.
	if _, err := s.Set(ctx, "other", "bad", Value{}); err != nil {
		t.Errorf("validation leaked into an unregistered kind: %v", err)
	}
}

func testCompareFnSuppressesUpdates(t *testing.T, cfg Config) {
	if cfg.NewWithOptions == nil {
		t.Skip("backend does not accept store.StoreOptions (see FIX-PLAN X3)")
	}
	s := cfg.NewWithOptions(t, store.StoreOptions[Value]{
		// Only Name is significant; N changes are not real changes.
		CompareFn: func(prev, next Value) bool { return prev.Name == next.Name },
	})
	t.Cleanup(func() { _ = s.Close() })
	mustSet(t, s, "k", "a", Value{Name: "a", N: 1})

	ch := watchCtx(t, cfg, s, "k")
	if _, err := s.Set(context.Background(), "k", "a", Value{Name: "a", N: 2}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	expectNoEvent(t, cfg, ch)
}
