package storetest

import (
	"context"
	"fmt"
	"maps"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zestor-dev/zestor/store"
)

// The three cases below are the contract under contention.
//
// ConcurrentIncrementsHaveNoGaps checks per-key order: SetFn assigns the next
// counter, and the watch has to deliver those values with none missing and
// none swapped. A lost intermediate update still leaves a final value that
// matches List, so the final map alone cannot catch it.
//
// ConcurrentWritesMatchTheWatch mixes Set, SetMany and Delete on the same
// keys. Writers count the events they actually emitted. The watch has to
// deliver that many, in an order that is legal for each key, and applying
// them has to reproduce List.
//
// SetManyIsAtomicToReaders lists the kind while a bulk write is in flight.
// Every snapshot contains every key of the batch or none of them, and the
// keys it does contain all carry one generation.

const concKind = "k"

func openPair(t *testing.T, cfg Config) (write, read store.Store[Value]) {
	t.Helper()
	if cfg.NewShared == nil {
		s := newStore(t, cfg)
		return s, s
	}
	write, read = cfg.NewShared(t)
	t.Cleanup(func() {
		_ = write.Close()
		if read != write {
			_ = read.Close()
		}
	})
	return write, read
}

// applied is the watcher's copy of a kind, built only from events.
type applied struct {
	mu     sync.Mutex
	values map[string]Value
	seen   int
	err    string
	closed bool
}

func (a *applied) status() (seen int, errStr string, closed bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.seen, a.err, a.closed
}

func (a *applied) snapshot() map[string]Value {
	a.mu.Lock()
	defer a.mu.Unlock()
	return maps.Clone(a.values)
}

// watchApplied starts a watch and folds events into st with apply. apply is
// called with st.mu held and must not lock it again. The buffer covers the
// whole run, so a slow read shows up as a late event rather than an overflow
// the test itself caused.
func watchApplied(t *testing.T, cfg Config, s store.Store[Value], buf int, apply func(*applied, *store.Event[Value])) *applied {
	t.Helper()
	ch := watchCtx(t, cfg, s, concKind, store.WithBufferSize[Value](buf))
	st := &applied{values: make(map[string]Value)}
	go func() {
		for ev := range ch {
			st.mu.Lock()
			if st.err == "" {
				apply(st, ev)
			}
			st.mu.Unlock()
		}
		st.mu.Lock()
		st.closed = true
		st.mu.Unlock()
	}()
	return st
}

func waitForEvents(t *testing.T, cfg Config, st *applied, want int) {
	t.Helper()
	deadline := time.NewTimer(cfg.eventDelay())
	defer deadline.Stop()
	tick := time.NewTicker(2 * time.Millisecond)
	defer tick.Stop()
	for {
		seen, errStr, closed := st.status()
		if errStr != "" {
			t.Fatalf("watch failed after %d of %d events: %s", seen, want, errStr)
		}
		if closed {
			t.Fatalf("watch closed after %d of %d events", seen, want)
		}
		if seen == want {
			return
		}
		if seen > want {
			t.Fatalf("watch delivered %d events, writers emitted %d", seen, want)
		}
		select {
		case <-deadline.C:
			t.Fatalf("watch delivered %d of %d events within %v", seen, want, cfg.eventDelay())
		case <-tick.C:
		}
	}
}

// settleWatch waits long enough that a duplicate event would have arrived,
// then returns the watcher's view of the kind.
func settleWatch(t *testing.T, cfg Config, st *applied, want int) map[string]Value {
	t.Helper()
	waitForEvents(t, cfg, st, want)
	time.Sleep(cfg.settleDelay())
	seen, errStr, closed := st.status()
	if errStr != "" {
		t.Fatalf("watch failed after the writers stopped: %s", errStr)
	}
	if closed {
		t.Fatal("watch closed after the writers stopped")
	}
	if seen != want {
		t.Fatalf("watch delivered %d events after the writers stopped, want %d", seen, want)
	}
	return st.snapshot()
}

func confirmQuiet(t *testing.T, st *applied, want int) {
	t.Helper()
	seen, errStr, closed := st.status()
	if errStr != "" || closed || seen != want {
		t.Fatalf("watch changed while it was compared to the store: seen=%d want=%d err=%q closed=%v",
			seen, want, errStr, closed)
	}
}

// requireSameList returns the read store's contents. When write and read are
// different handles, the writer's List has to agree: both are looking at one
// database.
func requireSameList(t *testing.T, write, read store.Store[Value]) map[string]Value {
	t.Helper()
	ctx := context.Background()
	listed, err := read.List(ctx, concKind)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if write != read {
		other, err := write.List(ctx, concKind)
		if err != nil {
			t.Fatalf("List on the writer: %v", err)
		}
		if diff := stateDiff("writer", "watcher", other, listed); diff != "" {
			t.Fatalf("the two handles disagree on List:\n%s", diff)
		}
	}
	return listed
}

func stateDiff(gotLabel, wantLabel string, got, want map[string]Value) string {
	var b strings.Builder
	n := 0
	note := func(format string, args ...any) {
		if n >= 8 {
			if n == 8 {
				b.WriteString("...\n")
			}
			n++
			return
		}
		fmt.Fprintf(&b, format, args...)
		n++
	}
	for k, v := range want {
		g, ok := got[k]
		if !ok {
			note("%s is missing %s (%s has %+v)\n", gotLabel, k, wantLabel, v)
			continue
		}
		if g != v {
			note("%s: %s has %+v, %s has %+v\n", k, gotLabel, g, wantLabel, v)
		}
	}
	for k, v := range got {
		if _, ok := want[k]; !ok {
			note("%s has extra %s %+v\n", gotLabel, k, v)
		}
	}
	return b.String()
}

func numberedKeys(n int) []string {
	keys := make([]string, n)
	for i := range keys {
		keys[i] = fmt.Sprintf("k%02d", i)
	}
	return keys
}

func targetsOf(write, read store.Store[Value]) []store.Store[Value] {
	if write == read {
		return []store.Store[Value]{write}
	}
	return []store.Store[Value]{write, read}
}

func testConcurrentIncrementsHaveNoGaps(t *testing.T, cfg Config) {
	const (
		nWriters  = 4
		perWriter = 25
		nKeys     = 8
	)
	write, read := openPair(t, cfg)
	ctx := context.Background()
	keys := numberedKeys(nKeys)
	for _, k := range keys {
		mustSet(t, write, concKind, k, Value{Name: k})
	}

	// Seed is N=0 and happened before Watch, so the first delivered value for
	// a key is the update to 1. Anything else skipped or reordered an event.
	want := nWriters * perWriter
	st := watchApplied(t, cfg, read, want, func(st *applied, ev *store.Event[Value]) {
		if ev.Err != nil || ev.EventType == store.EventTypeError {
			if ev.Err != nil {
				st.err = ev.Err.Error()
			} else {
				st.err = "watch ended"
			}
			return
		}
		if ev.EventType != store.EventTypeUpdate {
			st.err = fmt.Sprintf("%s: got %s carrying %+v, want an update", ev.Name, ev.EventType, ev.Object)
			return
		}
		prev := st.values[ev.Name].N
		if ev.Object.N != prev+1 {
			st.err = fmt.Sprintf("%s went from %d to %d", ev.Name, prev, ev.Object.N)
			return
		}
		st.values[ev.Name] = ev.Object
		st.seen++
	})

	targets := targetsOf(write, read)
	errCh := make(chan error, nWriters)
	var wg sync.WaitGroup
	for id := range nWriters {
		wg.Add(1)
		go func() {
			defer wg.Done()
			target := targets[id%len(targets)]
			for i := range perWriter {
				key := keys[(id+i)%len(keys)]
				changed, err := target.SetFn(ctx, concKind, key, func(v Value) (Value, error) {
					v.N++
					return v, nil
				})
				if err != nil {
					errCh <- fmt.Errorf("writer %d SetFn: %w", id, err)
					return
				}
				if !changed {
					errCh <- fmt.Errorf("writer %d: increment of %s reported changed=false", id, key)
					return
				}
			}
		}()
	}
	wg.Wait()
	select {
	case err := <-errCh:
		t.Fatal(err)
	default:
	}

	snap := settleWatch(t, cfg, st, want)
	listed := requireSameList(t, write, read)
	if len(listed) != nKeys {
		t.Fatalf("List has %d keys, seeded %d", len(listed), nKeys)
	}
	sum := 0
	for _, k := range keys {
		v, ok := listed[k]
		if !ok {
			t.Errorf("List is missing seeded key %s", k)
			continue
		}
		sum += v.N
		got, watched := snap[k]
		if v.N == 0 {
			if watched {
				t.Errorf("watch has %+v for %s, which no increment touched", got, k)
			}
			continue
		}
		if !watched || got != v {
			t.Errorf("%s: watch has %+v, List has %+v", k, got, v)
		}
	}
	for k, v := range snap {
		if _, ok := listed[k]; !ok {
			t.Errorf("watch has extra %s %+v", k, v)
		}
	}
	if sum != want {
		t.Errorf("stored counters sum to %d, writers applied %d increments", sum, want)
	}
	confirmQuiet(t, st, want)
}

func testConcurrentWritesMatchTheWatch(t *testing.T, cfg Config) {
	const (
		nWriters = 4
		ops      = 20
		nKeys    = 8
		batch    = 4
	)
	write, read := openPair(t, cfg)
	ctx := context.Background()
	keys := numberedKeys(nKeys)

	// Every Set and SetMany value carries a fresh counter, so each of those
	// calls changes the stored bytes and emits one event per key. The counter
	// is taken before the write is serialised, so it is not the commit order.
	maxEvents := nWriters * ops * batch
	var seq atomic.Int64
	st := watchApplied(t, cfg, read, maxEvents, func(st *applied, ev *store.Event[Value]) {
		if ev.Err != nil || ev.EventType == store.EventTypeError {
			if ev.Err != nil {
				st.err = ev.Err.Error()
			} else {
				st.err = "watch ended"
			}
			return
		}
		cur, present := st.values[ev.Name]
		switch ev.EventType {
		case store.EventTypeCreate:
			if present {
				st.err = fmt.Sprintf("%s created, but the watch already had %+v", ev.Name, cur)
				return
			}
			st.values[ev.Name] = ev.Object
		case store.EventTypeUpdate:
			if !present {
				st.err = fmt.Sprintf("%s updated, but the watch had no value", ev.Name)
				return
			}
			st.values[ev.Name] = ev.Object
		case store.EventTypeDelete:
			if !present {
				st.err = fmt.Sprintf("%s deleted, but the watch had no value", ev.Name)
				return
			}
			if ev.Object != cur {
				st.err = fmt.Sprintf("%s delete carried %+v, watch had %+v", ev.Name, ev.Object, cur)
				return
			}
			delete(st.values, ev.Name)
		default:
			st.err = fmt.Sprintf("unexpected event %s %s", ev.EventType, ev.Name)
			return
		}
		st.seen++
	})

	targets := targetsOf(write, read)
	var emitted atomic.Int64
	errCh := make(chan error, nWriters)
	var wg sync.WaitGroup
	for id := range nWriters {
		wg.Add(1)
		go func() {
			defer wg.Done()
			target := targets[id%len(targets)]
			for i := range ops {
				key := keys[(id+i)%len(keys)]
				switch i % 5 {
				case 0:
					existed, _, err := target.Delete(ctx, concKind, key)
					if err != nil {
						errCh <- fmt.Errorf("writer %d Delete: %w", id, err)
						return
					}
					if existed {
						emitted.Add(1)
					}
				case 1:
					values := make(map[string]Value, batch)
					for j := range batch {
						k := keys[(id+i+j)%len(keys)]
						values[k] = Value{Name: k, N: int(seq.Add(1))}
					}
					if err := target.SetMany(ctx, concKind, values); err != nil {
						errCh <- fmt.Errorf("writer %d SetMany: %w", id, err)
						return
					}
					emitted.Add(int64(len(values)))
				default:
					n := int(seq.Add(1))
					res, err := target.Set(ctx, concKind, key, Value{Name: key, N: n})
					if err != nil {
						errCh <- fmt.Errorf("writer %d Set: %w", id, err)
						return
					}
					if res == store.SetUnchanged {
						errCh <- fmt.Errorf("writer %d: Set %s to N=%d reported unchanged", id, key, n)
						return
					}
					emitted.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	select {
	case err := <-errCh:
		t.Fatal(err)
	default:
	}

	want := int(emitted.Load())
	if want == 0 {
		t.Fatal("writers emitted no events")
	}
	snap := settleWatch(t, cfg, st, want)
	listed := requireSameList(t, write, read)
	if diff := stateDiff("watch", "List", snap, listed); diff != "" {
		t.Fatalf("applying the watch does not match the store:\n%s", diff)
	}
	confirmQuiet(t, st, want)
}

func testSetManyIsAtomicToReaders(t *testing.T, cfg Config) {
	const (
		batchKeys = 64
		rounds    = 4
	)
	write, read := openPair(t, cfg)
	ctx := context.Background()
	keys := make([]string, batchKeys)
	for i := range keys {
		keys[i] = fmt.Sprintf("b%03d", i)
	}

	stop := make(chan struct{})
	var stopOnce sync.Once
	var wg sync.WaitGroup
	failCh := make(chan string, 1)
	noteFail := func(msg string) {
		select {
		case failCh <- msg:
		default:
		}
	}
	started := make(chan struct{})
	var startOnce sync.Once
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			got, err := read.List(ctx, concKind)
			if err != nil {
				noteFail(fmt.Sprintf("List: %v", err))
				return
			}
			if msg := atomicBatchViolation(got, keys); msg != "" {
				noteFail(msg)
				return
			}
			startOnce.Do(func() { close(started) })
		}
	}()
	stopWait := func() {
		stopOnce.Do(func() {
			close(stop)
			wg.Wait()
		})
	}
	defer stopWait()

	select {
	case <-started:
	case msg := <-failCh:
		t.Fatal(msg)
	case <-time.After(cfg.eventDelay()):
		t.Fatal("reader took no snapshot before the writes")
	}

	for g := 1; g <= rounds; g++ {
		values := make(map[string]Value, len(keys))
		for _, k := range keys {
			values[k] = Value{Name: k, N: g}
		}
		if err := write.SetMany(ctx, concKind, values); err != nil {
			t.Fatalf("SetMany generation %d: %v", g, err)
		}
	}
	stopWait()
	select {
	case msg := <-failCh:
		t.Fatal(msg)
	default:
	}
}

// atomicBatchViolation reports a snapshot that shows part of the batch, or
// keys from more than one generation. An empty snapshot is the kind before
// the first commit.
func atomicBatchViolation(got map[string]Value, keys []string) string {
	want := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		want[k] = struct{}{}
	}
	gens := make(map[int]int)
	present := 0
	extras := 0
	for k, v := range got {
		if _, ok := want[k]; !ok {
			extras++
			continue
		}
		present++
		gens[v.N]++
	}
	if extras > 0 {
		return fmt.Sprintf("List returned %d key(s) outside the batch", extras)
	}
	if present == 0 {
		return ""
	}
	if present == len(keys) && len(gens) == 1 {
		return ""
	}
	return fmt.Sprintf("List showed %d of %d batch keys across generations %v", present, len(keys), gens)
}
