package postgres_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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
