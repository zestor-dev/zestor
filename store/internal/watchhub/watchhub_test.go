package watchhub

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/zestor-dev/zestor/store"
)

func ev(n int) *store.Event[int] {
	return &store.Event[int]{Kind: "k", Name: strconv.Itoa(n), EventType: store.EventTypeCreate, Object: n}
}

// A batch already taken for delivery counts toward the cap. One more live event
// overflows the watch instead of filling a second queue.
func TestInFlightCountsTowardCapacity(t *testing.T) {
	s := &sub[int]{
		kind:     "k",
		capacity: 4,
		ch:       make(chan *store.Event[int], 1),
		notify:   make(chan struct{}, 1),
		stop:     make(chan struct{}),
		inflight: 4,
	}
	s.enqueue(ev(99))
	if !s.overflowed || len(s.pending) != 0 {
		t.Fatalf("overflowed=%v pending=%d, want the in-flight batch to fill the cap", s.overflowed, len(s.pending))
	}

	// With nothing in flight the same event is accepted.
	s.overflowed = false
	s.inflight = 0
	s.enqueue(ev(1))
	if s.overflowed || len(s.pending) != 1 {
		t.Fatalf("overflowed=%v pending=%d, want the event queued", s.overflowed, len(s.pending))
	}
}

// The initial snapshot is not lag, so it is delivered whole even when it is
// larger than the buffer and the consumer has not read anything yet.
func TestFailDeliversQueuedEventsThenCloses(t *testing.T) {
	h := New[int]()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer h.Close()

	ch, err := h.Subscribe(ctx, "k", &store.WatchCfg[int]{BufferSize: 4}, Hooks[int]{})
	if err != nil {
		t.Fatal(err)
	}
	filtered, err := h.Subscribe(ctx, "k", &store.WatchCfg[int]{
		BufferSize: 4,
		EventTypes: map[store.EventType]struct{}{store.EventTypeDelete: {}},
	}, Hooks[int]{})
	if err != nil {
		t.Fatal(err)
	}
	h.Publish("k", ev(1))
	h.Fail("k", store.ErrDecode)

	got, ok := <-ch
	if !ok || got.Err != nil || got.Object != 1 {
		t.Fatalf("queued event: ok=%v got=%v", ok, got)
	}
	got, ok = <-ch
	if !ok || !errors.Is(got.Err, store.ErrDecode) {
		t.Fatalf("terminal event: ok=%v got=%v", ok, got)
	}
	if _, ok = <-ch; ok {
		t.Fatal("channel stayed open after ErrDecode")
	}
	got, ok = <-filtered
	if !ok || !errors.Is(got.Err, store.ErrDecode) {
		t.Fatalf("filtered watcher: ok=%v got=%v", ok, got)
	}
}

func TestReplayDoesNotCountTowardCapacity(t *testing.T) {
	h := New[int]()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer h.Close()

	const n = 10
	ch, err := h.Subscribe(ctx, "k", &store.WatchCfg[int]{BufferSize: 4, Initial: true}, Hooks[int]{
		Snapshot: func() ([]*store.Event[int], error) {
			out := make([]*store.Event[int], n)
			for i := range n {
				out[i] = ev(i)
			}
			return out, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := range n {
		select {
		case got, ok := <-ch:
			if !ok || got == nil || got.Err != nil || got.Object != i || !got.Replay {
				t.Fatalf("replay event %d: ok=%v got=%+v", i, ok, got)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out at replay event %d", i)
		}
	}
}
