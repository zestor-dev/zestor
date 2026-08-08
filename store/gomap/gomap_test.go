package gomap_test

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/zestor-dev/zestor/store"
	"github.com/zestor-dev/zestor/store/gomap"
	"github.com/zestor-dev/zestor/store/storetest"
)

func TestConformance(t *testing.T) {
	storetest.Run(t, storetest.Config{
		New: func(t *testing.T) store.Store[storetest.Value] {
			return gomap.NewMemStore[storetest.Value](store.StoreOptions[storetest.Value]{})
		},
		NewWithOptions: func(t *testing.T, opts store.StoreOptions[storetest.Value]) store.Store[storetest.Value] {
			return gomap.NewMemStore[storetest.Value](opts)
		},
		// In-memory: events are queued synchronously with the write.
		EventDelay:  time.Second,
		SettleDelay: 100 * time.Millisecond,
	})
}

// --- gomap-specific behaviour ------------------------------------------------

type item struct {
	Name string
	Tags []string
}

func newStore(t *testing.T) store.Store[item] {
	t.Helper()
	s := gomap.NewMemStore[item](store.StoreOptions[item]{})
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// GetAll copies the kind and key maps, so mutating the result cannot reach the
// store. (The values themselves are copied shallowly — see the method doc.)
func TestGetAllCopiesMaps(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	if _, err := s.Set(ctx, "k", "a", item{Name: "a"}); err != nil {
		t.Fatal(err)
	}

	all, err := s.GetAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	all["k"]["b"] = item{Name: "injected"}
	delete(all, "k")

	if _, ok, _ := s.Get(ctx, "k", "b"); ok {
		t.Error("writing to the map returned by GetAll reached the store")
	}
	if n, _ := s.Count(ctx, "k"); n != 1 {
		t.Errorf("Count=%d after mutating GetAll's result, want 1", n)
	}
}

// Watch tolerates nil options, which a caller building an option slice
// dynamically can easily produce.
func TestWatchIgnoresNilOptions(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := newStore(t)

	ch, err := s.Watch(ctx, "k", nil, store.WithBufferSize[item](8), nil)
	if err != nil {
		t.Fatalf("Watch with nil options: %v", err)
	}
	if _, err := s.Set(ctx, "k", "a", item{Name: "a"}); err != nil {
		t.Fatal(err)
	}
	if ev := <-ch; ev.Name != "a" {
		t.Errorf("got event for %q, want a", ev.Name)
	}
}

// Delete and SetFn on a kind that was never written must not bring it into
// existence (FIX-PLAN T0.4).
func TestMissingKindIsNotMaterialized(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	for _, kind := range []string{"never-1", "never-2", "never-3"} {
		if _, _, err := s.Delete(ctx, kind, "k"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.SetFn(ctx, kind, "k", func(v item) (item, error) { return v, nil }); err != store.ErrKeyNotFound {
			t.Fatalf("SetFn on a missing kind returned %v, want ErrKeyNotFound", err)
		}
	}

	all, err := s.GetAll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 0 {
		t.Errorf("GetAll reports %d kinds after only reads and misses, want 0: %v", len(all), all)
	}
}

func TestDumpIsSorted(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	for _, k := range []string{"c", "a", "b"} {
		if _, err := s.Set(ctx, "kind", k, item{Name: k}); err != nil {
			t.Fatal(err)
		}
	}

	var buf bytes.Buffer
	if err := s.(store.Dumper).Dump(ctx, &buf); err != nil {
		t.Fatal(err)
	}
	if want := "kind:\n  a: "; !strings.HasPrefix(buf.String(), want) {
		t.Errorf("Dump() = %q, want it to start with %q", buf.String(), want)
	}
}
