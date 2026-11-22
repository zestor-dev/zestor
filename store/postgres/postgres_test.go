package postgres_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zestor-dev/zestor/codec"
	"github.com/zestor-dev/zestor/store"
	"github.com/zestor-dev/zestor/store/postgres"
	"github.com/zestor-dev/zestor/store/storetest"
)

// postgresConnEnv returns the DSN and whether POSTGRES_CONN was set in the environment
// (as opposed to using the implicit localhost default).
func postgresConnEnv() (conn string, envExplicit bool) {
	conn = strings.TrimSpace(os.Getenv("POSTGRES_CONN"))
	if conn != "" {
		return conn, true
	}
	return "postgresql://postgres:postgres@127.0.0.1:5432/postgres?sslmode=disable", false
}

func getPostgresConn() string {
	conn, _ := postgresConnEnv()
	return conn
}

// postgresMustConnect is true when a failed dial should fail the test rather than skip:
// - POSTGRES_CONN is set, or
// - POSTGRES_REQUIRE is set to a truthy value (e.g. make test-postgres uses this).
// Set POSTGRES_REQUIRE=0|false|no|off to force skips even when POSTGRES_CONN is set.
func postgresMustConnect() bool {
	flag := strings.TrimSpace(os.Getenv("POSTGRES_REQUIRE"))
	if flag != "" {
		switch strings.ToLower(flag) {
		case "0", "false", "no", "off":
			return false
		default:
			return true
		}
	}
	_, explicit := postgresConnEnv()
	return explicit
}

func reportPostgresErr(tb testing.TB, err error) {
	tb.Helper()
	if postgresMustConnect() {
		tb.Fatalf(
			"postgres required but unreachable: %v\n"+
				"Use a reachable DSN (POSTGRES_CONN), publish Postgres to the host from Compose (e.g. ports \"5432:5432\"), "+
				"or unset POSTGRES_REQUIRE and POSTGRES_CONN to allow skips when localhost:5432 is closed.",
			err,
		)
	}
	tb.Skipf("postgres unavailable: %v", err)
}

// Every store gets its own namespace, so tests are isolated without truncating
// shared tables.
var nsCounter atomic.Int64

func nextNamespace() int64 {
	return time.Now().UnixNano() + nsCounter.Add(1)
}

func newStore[T any](tb testing.TB, ns int64) store.Store[T] {
	tb.Helper()
	s, err := postgres.New[T](context.Background(), postgres.Options{
		ConnString: getPostgresConn(),
		Codec:      &codec.JSON{},
		Namespace:  ns,
		Timeout:    30 * time.Second,
	})
	if err != nil {
		reportPostgresErr(tb, err)
	}
	return s
}

func TestConformance(t *testing.T) {
	storetest.Run(t, storetest.Config{
		New: func(t *testing.T) store.Store[storetest.Value] {
			return newStore[storetest.Value](t, nextNamespace())
		},
		// NewWithOptions is deliberately omitted: postgres does not accept
		// store.StoreOptions, so the validation and compare cases skip. See
		// FIX-PLAN X3.
		//
		// Events reach watchers through a commit, a NOTIFY and an outbox drain,
		// so the delays are far more generous than for the in-process backends.
		EventDelay:  15 * time.Second,
		SettleDelay: 2 * time.Second,
	})
}

// --- postgres-specific behaviour ---------------------------------------------

type TestData struct {
	Name  string `json:"name"`
	Value int    `json:"value"`
}

// Opening a store must not take an exclusive lock on the outbox. A session
// that is only reading (ACCESS SHARE, what a plain SELECT holds) has to keep
// writers moving, and New used to DROP TRIGGER on every start, which waits
// for that lock and then blocks every writer.
// PostgreSQL does not keep a notification that was sent while this session
// was not listening. Reconnecting used to resume from the next NOTIFY only,
// so a write in the gap never reached the watcher. After LISTEN the store
// has to read the outbox for the kinds it is already watching.
func TestWriteDuringListenReconnectIsDelivered(t *testing.T) {
	ctx := context.Background()
	s := newStore[TestData](t, nextNamespace())
	defer s.Close()

	watchCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	ch, err := s.Watch(watchCtx, "k")
	if err != nil {
		t.Fatal(err)
	}

	conn, err := pgx.Connect(ctx, getPostgresConn())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var pid int
	err = conn.QueryRow(ctx, `
SELECT pid FROM pg_stat_activity
 WHERE pid <> pg_backend_pid()
   AND query LIKE '%LISTEN zestor_events_v2%'
 LIMIT 1`).Scan(&pid)
	if err != nil {
		t.Fatalf("find listen connection: %v", err)
	}
	if _, err := conn.Exec(ctx, `SELECT pg_terminate_backend($1)`, pid); err != nil {
		t.Fatal(err)
	}
	// The old listener sleeps before it connects again. Write while nobody is
	// listening, which is the gap a NOTIFY does not cross.
	deadline := time.Now().Add(2 * time.Second)
	for {
		var n int
		if err := conn.QueryRow(ctx, `
SELECT count(*) FROM pg_stat_activity
 WHERE pid <> pg_backend_pid() AND query LIKE '%LISTEN zestor_events_v2%'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("listen connection was not dropped")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if _, err := s.Set(ctx, "k", "during", TestData{Name: "gap"}); err != nil {
		t.Fatal(err)
	}
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("watch closed before the write arrived")
		}
		if ev.Err != nil || ev.Name != "during" {
			t.Fatalf("got %s %q (err %v), want the write made while reconnecting", ev.EventType, ev.Name, ev.Err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a write made while the listener was reconnecting was never delivered")
	}
}

func TestNewDoesNotLockOutbox(t *testing.T) {
	ctx := context.Background()
	existing := newStore[TestData](t, nextNamespace())
	existing.Close()

	conn, err := pgx.Connect(ctx, getPostgresConn())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `LOCK TABLE zestor_outbox IN ACCESS SHARE MODE`); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		openCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		s, err := postgres.New[TestData](openCtx, postgres.Options{
			ConnString: getPostgresConn(),
			Codec:      &codec.JSON{},
			Namespace:  nextNamespace(),
			Timeout:    2 * time.Second,
		})
		if s != nil {
			s.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("New while a reader holds the outbox: %v", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("New blocked behind an ACCESS SHARE lock, so it is still taking an exclusive lock")
	}
}

// An older process installs the unversioned trigger, which breaks long keys.
// Starting a current store removes it and leaves the v2 trigger in place.
func TestNewReplacesLegacyTrigger(t *testing.T) {
	ctx := context.Background()
	s := newStore[TestData](t, nextNamespace())
	s.Close()

	conn, err := pgx.Connect(ctx, getPostgresConn())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	for _, q := range []string{
		`CREATE OR REPLACE FUNCTION zestor_notify() RETURNS TRIGGER AS $$ BEGIN
		   PERFORM pg_notify('zestor_events', NEW.ns_id::text); RETURN NEW; END; $$ LANGUAGE plpgsql;`,
		`DROP TRIGGER IF EXISTS trg_zestor_outbox_notify ON zestor_outbox;`,
		`CREATE TRIGGER trg_zestor_outbox_notify AFTER INSERT ON zestor_outbox
		   FOR EACH ROW EXECUTE PROCEDURE zestor_notify();`,
	} {
		if _, err := conn.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}

	current := newStore[TestData](t, nextNamespace())
	current.Close()

	rows, err := conn.Query(ctx, `
SELECT tgname FROM pg_trigger
 WHERE tgrelid = 'zestor_outbox'::regclass AND NOT tgisinternal`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "trg_zestor_outbox_notify_v2" {
		t.Fatalf("triggers = %v, want only trg_zestor_outbox_notify_v2", names)
	}
}

// A negative Timeout must not add a deadline of its own.
func TestNegativeTimeoutDisablesCap(t *testing.T) {
	ctx := context.Background()
	s, err := postgres.New[TestData](ctx, postgres.Options{
		ConnString: getPostgresConn(),
		Codec:      &codec.JSON{},
		Namespace:  nextNamespace(),
		Timeout:    -1,
	})
	if err != nil {
		reportPostgresErr(t, err)
	}
	defer s.Close()
	if _, _, err := s.Get(ctx, "k", "a"); err != nil {
		t.Fatalf("Get with no timeout cap returned %v", err)
	}
}

func TestNewValidatesOptions(t *testing.T) {
	ctx := context.Background()

	s, err := postgres.New[TestData](ctx, postgres.Options{
		ConnString: getPostgresConn(),
		Codec:      &codec.JSON{},
		Namespace:  nextNamespace(),
	})
	if err != nil {
		reportPostgresErr(t, err)
	}
	_ = s.Close()

	if _, err := postgres.New[TestData](ctx, postgres.Options{Codec: &codec.JSON{}}); err == nil {
		t.Error("New without a ConnString must fail")
	}
	if _, err := postgres.New[TestData](ctx, postgres.Options{ConnString: getPostgresConn()}); err == nil {
		t.Error("New without a Codec must fail")
	}
}

func TestNamespacesAreIsolated(t *testing.T) {
	ctx := context.Background()
	a := newStore[TestData](t, nextNamespace())
	defer a.Close()
	b := newStore[TestData](t, nextNamespace())
	defer b.Close()

	if _, err := a.Set(ctx, "k", "same", TestData{Name: "in-a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Set(ctx, "k", "same", TestData{Name: "in-b"}); err != nil {
		t.Fatal(err)
	}

	got, ok, err := a.Get(ctx, "k", "same")
	if err != nil || !ok {
		t.Fatalf("Get: err=%v ok=%v", err, ok)
	}
	if got.Name != "in-a" {
		t.Errorf("namespace a sees %+v, want in-a", got)
	}
	if n, _ := b.Count(ctx, "k"); n != 1 {
		t.Errorf("namespace b has Count=%d, want 1", n)
	}
}

func TestPersistsAcrossReopen(t *testing.T) {
	ctx := context.Background()
	ns := nextNamespace()

	s := newStore[TestData](t, ns)
	if _, err := s.Set(ctx, "k", "a", TestData{Name: "hello", Value: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := newStore[TestData](t, ns)
	defer reopened.Close()
	got, ok, err := reopened.Get(ctx, "k", "a")
	if err != nil || !ok {
		t.Fatalf("Get after reopen: err=%v ok=%v", err, ok)
	}
	if got.Name != "hello" {
		t.Errorf("Get after reopen returned %+v, want Name=hello", got)
	}
}

// The point of the postgres backend: a write in one process reaches a watcher in
// another. Two independent stores over the same namespace stand in for that.
// Two kinds are drained independently: a watcher of one still receives a
// write while the other kind also has a watcher.
func TestKindsDeliverIndependently(t *testing.T) {
	ctx := context.Background()
	s := newStore[TestData](t, nextNamespace())
	defer s.Close()

	watchCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	a, err := s.Watch(watchCtx, "a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Watch(watchCtx, "b")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Set(ctx, "a", "one", TestData{Name: "a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Set(ctx, "b", "two", TestData{Name: "b"}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		ch   <-chan *store.Event[TestData]
		name string
	}{{a, "one"}, {b, "two"}} {
		select {
		case ev, ok := <-tc.ch:
			if !ok {
				t.Fatalf("watch for %s closed", tc.name)
			}
			if ev.Err != nil || ev.Name != tc.name {
				t.Fatalf("got %s %q (err %v), want %s", ev.EventType, ev.Name, ev.Err, tc.name)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("event %s never arrived", tc.name)
		}
	}
}

func TestCrossInstanceWatch(t *testing.T) {
	ctx := context.Background()
	ns := nextNamespace()

	writer := newStore[TestData](t, ns)
	defer writer.Close()
	reader := newStore[TestData](t, ns)
	defer reader.Close()

	watchCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	ch, err := reader.Watch(watchCtx, "k")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := writer.Set(ctx, "k", "a", TestData{Name: "from-writer", Value: 7}); err != nil {
		t.Fatal(err)
	}

	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("watch channel closed before the event arrived")
		}
		if ev.Err != nil {
			t.Fatalf("watch terminated: %v", ev.Err)
		}
		if ev.EventType != store.EventTypeCreate || ev.Object.Name != "from-writer" {
			t.Errorf("got %s %+v, want a create carrying from-writer", ev.EventType, ev.Object)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("a write on one instance never reached a watcher on another")
	}
}

// A watcher must never see events for writes that happened before it
// subscribed. Set returns when its transaction commits, but the matching event
// is still travelling through NOTIFY and the outbox drain, so Watch has to flush
// that backlog — including a batch the drain has already read but not yet
// published — before the subscription becomes visible.
//
// Deliberately no sleep before Watch: the sleep is what used to hide this.
// Writes to a kind with no watcher must not be delivered to a watcher that
// arrives later, and a write after that watcher subscribes still must be.
func TestUnwatchedWritesAreNotReplayed(t *testing.T) {
	ctx := context.Background()
	s := newStore[TestData](t, nextNamespace())
	defer s.Close()

	for i := range 20 {
		if _, err := s.Set(ctx, "quiet", fmt.Sprintf("k%02d", i), TestData{Name: "before", Value: i}); err != nil {
			t.Fatal(err)
		}
	}
	// Let the notifications move the cursor before the watcher exists.
	time.Sleep(300 * time.Millisecond)

	watchCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	ch, err := s.Watch(watchCtx, "quiet")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("watch closed")
		}
		t.Fatalf("watcher received %s %s for a write that predates it", ev.EventType, ev.Name)
	case <-time.After(400 * time.Millisecond):
	}

	if _, err := s.Set(ctx, "quiet", "after", TestData{Name: "after"}); err != nil {
		t.Fatal(err)
	}
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("watch closed before the later write")
		}
		if ev.Err != nil || ev.Name != "after" {
			t.Fatalf("got %s %q (err %v), want the write made after subscribing", ev.EventType, ev.Name, ev.Err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a write made after subscribing never arrived")
	}
}

func TestWatchIgnoresWritesFromBeforeSubscribe(t *testing.T) {
	ctx := context.Background()
	s := newStore[TestData](t, nextNamespace())
	defer s.Close()

	for i := range 50 {
		if _, err := s.Set(ctx, "k", fmt.Sprintf("k%03d", i), TestData{Name: "before", Value: i}); err != nil {
			t.Fatal(err)
		}
	}

	watchCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	ch, err := s.Watch(watchCtx, "k")
	if err != nil {
		t.Fatal(err)
	}

	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("watch channel closed unexpectedly")
		}
		t.Fatalf("watcher received %s %s/%s (%+v) for a write that predates it",
			ev.EventType, ev.Kind, ev.Name, ev.Object)
	case <-time.After(3 * time.Second):
	}

	// And it still works: a write after subscribing does arrive.
	if _, err := s.Set(ctx, "k", "after", TestData{Name: "after", Value: 1}); err != nil {
		t.Fatal(err)
	}
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("watch channel closed before the post-subscribe write arrived")
		}
		if ev.Name != "after" {
			t.Errorf("got an event for %q, want the post-subscribe write %q", ev.Name, "after")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("a write made after subscribing never arrived")
	}
}

// Outbox ids are allocated when a row is inserted, not when its transaction
// commits. Concurrent writers can therefore commit out of id order, and a drain
// that advances its cursor past a row whose transaction has not committed yet
// skips that row forever — a silent gap. Every committed write must reach the
// watcher exactly once.
func TestConcurrentWritersLeaveNoGaps(t *testing.T) {
	ctx := context.Background()
	ns := nextNamespace()

	reader := newStore[TestData](t, ns)
	defer reader.Close()
	writers := []store.Store[TestData]{newStore[TestData](t, ns), newStore[TestData](t, ns)}
	for _, w := range writers {
		defer w.Close()
	}

	watchCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	const goroutines, perGoroutine = 8, 40
	ch, err := reader.Watch(watchCtx, "k", store.WithBufferSize[TestData](4*goroutines*perGoroutine))
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := writers[g%len(writers)]
			for i := range perGoroutine {
				if _, err := w.Set(ctx, "k", fmt.Sprintf("g%02d-%03d", g, i), TestData{Value: i}); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()

	const want = goroutines * perGoroutine
	seen := make(map[string]int, want)
	deadline := time.After(20 * time.Second)
	for len(seen) < want {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatalf("watch channel closed after %d of %d keys", len(seen), want)
			}
			if ev.Err != nil {
				t.Fatalf("watch terminated after %d of %d keys: %v", len(seen), want, ev.Err)
			}
			seen[ev.Name]++
		case <-deadline:
			n, _ := reader.Count(ctx, "k")
			t.Fatalf("watcher received %d of %d keys, but the store holds %d: committed writes were skipped",
				len(seen), want, n)
		}
	}
	for k, n := range seen {
		if n != 1 {
			t.Errorf("key %s delivered %d times, want once", k, n)
		}
	}
}

// Keys and kinds are arbitrary strings. The NOTIFY that wakes the drain must
// survive any of them: a payload that fails to parse leaves the event stranded
// until some later write to the kind, and one that exceeds pg_notify's 8000-byte
// limit aborts the write itself.
func TestNotifySurvivesArbitraryNames(t *testing.T) {
	for _, tc := range []struct {
		name, kind, key string
	}{
		{"PipeInKey", "k", "a|b|c|d|e"},
		{"PipeInKind", "a|b", "k"},
		{"LongKey", "k", strings.Repeat("x", 8000)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			s := newStore[TestData](t, nextNamespace())
			defer s.Close()

			watchCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			ch, err := s.Watch(watchCtx, tc.kind)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Set(ctx, tc.kind, tc.key, TestData{Name: "v"}); err != nil {
				t.Fatalf("Set: %v", err)
			}

			select {
			case ev, ok := <-ch:
				if !ok {
					t.Fatal("watch channel closed before the event arrived")
				}
				if ev.Err != nil || ev.Name != tc.key {
					t.Errorf("got %s for a %d-byte key (err %v), want a create for the written key",
						ev.EventType, len(ev.Name), ev.Err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the write's event was never delivered")
			}
		})
	}
}

// Set used to insert with DO NOTHING, then read the row back to decide
// whether to update. That insert does not lock the existing row, so a delete
// that does not take the store's write lock can remove it in between and Set
// fails with "no rows". Store deletes take the lock, so the racing delete here
// is a raw one.
func TestSetSurvivesConcurrentDelete(t *testing.T) {
	ctx := context.Background()
	ns := nextNamespace()
	s := newStore[TestData](t, ns)
	defer s.Close()
	if _, err := s.Set(ctx, "k", "a", TestData{Name: "seed"}); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for range 8 {
		conn, err := pgx.Connect(ctx, getPostgresConn())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close(ctx)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				// Put the row straight back, so the next Set takes the update
				// path, which is the path with the gap. A delete that only
				// removes the row makes almost every Set an insert, and the
				// gap never opens.
				if _, err := conn.Exec(ctx, `DELETE FROM zestor_kv WHERE ns_id=$1 AND kind='k' AND key='a'`, ns); err != nil {
					t.Errorf("raw delete: %v", err)
					return
				}
				if _, err := conn.Exec(ctx, `INSERT INTO zestor_kv(ns_id,kind,key,value) VALUES($1,'k','a','"seed"') ON CONFLICT DO NOTHING`, ns); err != nil {
					t.Errorf("raw insert: %v", err)
					return
				}
			}
		}()
	}

	noRows := 0
	deadline := time.Now().Add(3 * time.Second)
	for i := 0; time.Now().Before(deadline); i++ {
		_, err := s.Set(ctx, "k", "a", TestData{Name: "v", Value: i})
		if err == nil {
			continue
		}
		if errors.Is(err, pgx.ErrNoRows) {
			noRows++
			continue
		}
		t.Errorf("Set: %v", err)
	}
	close(stop)
	wg.Wait()
	if noRows > 0 {
		t.Fatalf("Set failed with no rows %d times while a delete raced it", noRows)
	}
}

// A row that cannot be decoded used to pin the outbox cursor: the drain
// retried it forever and every later Watch on the kind failed. The row is
// skipped, current watchers are told, and a new watch still receives later
// writes. The type filter must not hide the terminal error.
func TestUndecodableOutboxRowDoesNotStallKind(t *testing.T) {
	ctx := context.Background()
	ns := nextNamespace()
	s := newStore[TestData](t, ns)
	defer s.Close()

	watchCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	ch, err := s.Watch(watchCtx, "k")
	if err != nil {
		t.Fatal(err)
	}
	deletes, err := s.Watch(watchCtx, "k", store.WithEventTypes[TestData](store.EventTypeDelete))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Set(ctx, "k", "good", TestData{Name: "ok"}); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-ch:
		if ev.Err != nil || ev.Name != "good" {
			t.Fatalf("got %s %q (err %v), want the good write", ev.EventType, ev.Name, ev.Err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the good write was never delivered")
	}

	conn, err := pgx.Connect(ctx, getPostgresConn())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx,
		`INSERT INTO zestor_outbox(ns_id, kind, key, etype, value) VALUES($1, 'k', 'bad', 'update', $2)`,
		ns, []byte{0xff}); err != nil {
		t.Fatal(err)
	}

	for _, sub := range []<-chan *store.Event[TestData]{ch, deletes} {
		select {
		case ev, ok := <-sub:
			if !ok || !errors.Is(ev.Err, store.ErrDecode) {
				t.Fatalf("terminal event: ok=%v err=%v", ok, ev.Err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the undecodable row did not end the watch")
		}
		select {
		case _, ok := <-sub:
			if ok {
				t.Fatal("channel stayed open after ErrDecode")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("channel was not closed after ErrDecode")
		}
	}

	again, cancelAgain := context.WithCancel(ctx)
	defer cancelAgain()
	ch2, err := s.Watch(again, "k")
	if err != nil {
		t.Fatalf("Watch after the bad row: %v", err)
	}
	if _, err := s.Set(ctx, "k", "after", TestData{Name: "after"}); err != nil {
		t.Fatal(err)
	}
	select {
	case ev, ok := <-ch2:
		if !ok {
			t.Fatal("watch closed before the write after the bad row")
		}
		if ev.Err != nil || ev.Name != "after" {
			t.Fatalf("got %s %q (err %v), want the write after the bad row", ev.EventType, ev.Name, ev.Err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a write after the bad row was never delivered: the kind is still stuck")
	}
}

func TestSetManyEmpty(t *testing.T) {
	ctx := context.Background()
	s := newStore[TestData](t, nextNamespace())
	if err := s.SetMany(ctx, "k", nil); err != nil {
		t.Fatalf("empty SetMany: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMany(ctx, "k", nil); !errors.Is(err, store.ErrClosed) {
		t.Errorf("empty SetMany on a closed store returned %v, want ErrClosed", err)
	}
}

func TestOutboxRetentionDeletesOldRows(t *testing.T) {
	ctx := context.Background()
	ns := nextNamespace()
	s, err := postgres.New[TestData](ctx, postgres.Options{
		ConnString:      getPostgresConn(),
		Codec:           &codec.JSON{},
		Namespace:       ns,
		Timeout:         10 * time.Second,
		OutboxRetention: 200 * time.Millisecond,
	})
	if err != nil {
		reportPostgresErr(t, err)
	}
	defer s.Close()

	conn, err := pgx.Connect(ctx, getPostgresConn())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `
INSERT INTO zestor_outbox(ns_id, kind, key, etype, value, at)
VALUES ($1, 'k', 'stale', 'create', convert_to('{"name":"old","value":1}','UTF8'), now() - interval '1 hour')`, ns); err != nil {
		t.Fatal(err)
	}
	// The insert notifies, which moves the cursor past the row, and the
	// retention loop then deletes it.
	deadline := time.Now().Add(3 * time.Second)
	for {
		var n int
		if err := conn.QueryRow(ctx, `SELECT count(*) FROM zestor_outbox WHERE ns_id=$1 AND key='stale'`, ns).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("old outbox row was not deleted")
		}
		time.Sleep(50 * time.Millisecond)
	}

	if _, err := s.Set(ctx, "k", "fresh", TestData{Name: "fresh"}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM zestor_outbox WHERE ns_id=$1 AND key='fresh'`, ns).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("fresh outbox row count = %d, want 1", n)
	}
}

func TestDumpIncludesVersion(t *testing.T) {
	ctx := context.Background()
	s := newStore[TestData](t, nextNamespace())
	defer s.Close()

	if _, err := s.Set(ctx, "k", "a", TestData{Name: "v1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Set(ctx, "k", "a", TestData{Name: "v2"}); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := s.(store.Dumper).Dump(ctx, &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "k/a v2") {
		t.Errorf("Dump() = %q, want it to report version 2", buf.String())
	}
}
