package sqlite_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zestor-dev/zestor/codec"
	"github.com/zestor-dev/zestor/store"
	"github.com/zestor-dev/zestor/store/sqlite"
	"github.com/zestor-dev/zestor/store/storetest"
)

func dsn(t *testing.T) string {
	t.Helper()
	return "file:" + filepath.Join(t.TempDir(), "zestor.db")
}

func TestConformance(t *testing.T) {
	storetest.Run(t, storetest.Config{
		New: func(t *testing.T) store.Store[storetest.Value] {
			s, err := sqlite.New[storetest.Value](context.Background(), sqlite.Options{
				DSN:         dsn(t),
				Codec:       &codec.JSON{},
				BusyTimeout: 5 * time.Second,
			})
			if err != nil {
				t.Fatalf("sqlite.New: %v", err)
			}
			return s
		},
		// NewWithOptions is deliberately omitted: sqlite does not accept
		// store.StoreOptions, so the validation and compare cases skip. See
		// FIX-PLAN X3.
		EventDelay:  2 * time.Second,
		SettleDelay: 250 * time.Millisecond,
	})
}

// --- sqlite-specific behaviour ----------------------------------------------

type Note struct {
	Title string `json:"title"`
	N     int    `json:"n"`
}

func newStore(t *testing.T, path string) store.Store[Note] {
	t.Helper()
	s, err := sqlite.New[Note](context.Background(), sqlite.Options{
		DSN:         path,
		Codec:       &codec.JSON{},
		BusyTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("sqlite.New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestNewValidatesOptions(t *testing.T) {
	ctx := context.Background()
	if _, err := sqlite.New[Note](ctx, sqlite.Options{Codec: &codec.JSON{}}); err == nil {
		t.Error("New without a DSN must fail")
	}
	if _, err := sqlite.New[Note](ctx, sqlite.Options{DSN: dsn(t)}); err == nil {
		t.Error("New without a Codec must fail")
	}
}

func TestPersistsAcrossReopen(t *testing.T) {
	ctx := context.Background()
	path := dsn(t)

	s := newStore(t, path)
	if _, err := s.Set(ctx, "notes", "a", Note{Title: "hello", N: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := newStore(t, path)
	got, ok, err := reopened.Get(ctx, "notes", "a")
	if err != nil || !ok {
		t.Fatalf("Get after reopen: err=%v ok=%v", err, ok)
	}
	if got.Title != "hello" {
		t.Errorf("Get after reopen returned %+v, want Title=hello", got)
	}
}

func TestDumpIncludesVersion(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, dsn(t))
	if _, err := s.Set(ctx, "notes", "a", Note{Title: "v1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Set(ctx, "notes", "a", Note{Title: "v2"}); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := s.(store.Dumper).Dump(ctx, &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "notes/a v2") {
		t.Errorf("Dump() = %q, want it to report version 2", buf.String())
	}
}

// failCodec marshals successfully until its budget runs out, then fails.
type failCodec struct{ budget atomic.Int64 }

func (c *failCodec) Marshal(v any) ([]byte, error) {
	if c.budget.Add(-1) < 0 {
		return nil, errors.New("codec budget exhausted")
	}
	return json.Marshal(v)
}

func (c *failCodec) Unmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

// A write that fails partway through must roll its transaction back. Regression
// test for FIX-PLAN T0.1: rollback used to be conditional on a named error that
// every `if err := ...` shadowed, so a failed write leaked the transaction, held
// the SQLite write lock forever and wedged the store for the life of the process.
func TestFailedWriteRollsBack(t *testing.T) {
	ctx := context.Background()
	c := &failCodec{}
	c.budget.Store(1000)

	s, err := sqlite.New[Note](ctx, sqlite.Options{
		DSN:         dsn(t),
		Codec:       c,
		BusyTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	if _, err := s.Set(ctx, "notes", "seed", Note{Title: "seed"}); err != nil {
		t.Fatal(err)
	}

	// Budget exactly one marshal so SetMany fails on its second key.
	c.budget.Store(1)
	err = s.SetMany(ctx, "notes", map[string]Note{"a": {Title: "a"}, "b": {Title: "b"}})
	if err == nil {
		t.Fatal("SetMany with a failing codec must return an error")
	}

	c.budget.Store(1000)
	done := make(chan error, 1)
	go func() {
		_, e := s.Set(ctx, "notes", "after", Note{Title: "after"})
		done <- e
	}()
	select {
	case e := <-done:
		if e != nil {
			t.Fatalf("a write after a failed SetMany errored (%v) — the failed transaction was not rolled back", e)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a write after a failed SetMany hung — the failed transaction still holds the write lock")
	}

	n, err := s.Count(ctx, "notes")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("Count=%d after a rolled-back SetMany, want 2 (seed + after) — the failed batch was partially committed", n)
	}
}

// Watch's initial replay used to send from an unsynchronised goroutine while
// cancel() closed the same channel, which panicked the process. The watch hub
// makes the subscriber goroutine the only writer and closer of its channel.
// Regression test for FIX-PLAN T0.2.
func TestWatchReplayCancelRace(t *testing.T) {
	ctx := context.Background()
	s := newStore(t, dsn(t))

	vals := make(map[string]Note, 200)
	for i := range 200 {
		vals[string(rune('a'+i%26))+string(rune('a'+i/26))] = Note{Title: "n", N: i}
	}
	if err := s.SetMany(ctx, "notes", vals); err != nil {
		t.Fatal(err)
	}

	for range 100 {
		wctx, cancel := context.WithCancel(ctx)
		if _, err := s.Watch(wctx, "notes",
			store.WithInitialReplay[Note](),
			store.WithBufferSize[Note](1024),
		); err != nil {
			cancel()
			t.Fatal(err)
		}
		cancel()
	}
}
