// Package watchhub implements the store.Watcher delivery contract once, so that
// every backend gets identical ordering, replay and overflow behaviour.
//
// The central invariant is that exactly one goroutine per subscriber writes to
// and closes that subscriber's channel. Publishers never touch the channel; they
// append to an internal bounded queue and return. That makes send-on-closed-channel
// structurally impossible rather than merely guarded, and it lets a publisher
// enqueue while holding its own write lock without ever blocking on a consumer.
//
// # Lock ordering
//
// Publish and Subscribe both take the hub lock. A backend that publishes while
// holding its own write lock therefore establishes the order
//
//	backend write lock -> hub lock
//
// and must take its write lock before calling Subscribe as well. The snapshot
// callback passed to Subscribe runs with the hub lock held, so it must not
// acquire the backend write lock itself — the caller already holds it.
//
// Backends whose events originate elsewhere (postgres, where a drain loop
// publishes in outbox-id order) need no write lock at all: the hub lock alone
// makes the snapshot atomic with respect to publication.
package watchhub

import (
	"context"
	"sync"

	"github.com/zestor-dev/zestor/store"
)

// Hub fans events out to the subscribers of a kind.
type Hub[T any] struct {
	mu     sync.Mutex
	subs   map[string]map[*sub[T]]struct{}
	closed bool
	wg     sync.WaitGroup
}

func New[T any]() *Hub[T] {
	return &Hub[T]{subs: make(map[string]map[*sub[T]]struct{})}
}

// SnapshotFunc returns the events that make up a watcher's initial replay. It is
// invoked with the hub lock held, so the events it returns are queued ahead of
// anything published afterwards.
type SnapshotFunc[T any] func() ([]*store.Event[T], error)

// Subscribe registers a watcher for kind and returns its event channel.
//
// If cfg.Initial is set and the watcher accepts create events, snapshot is
// called while publishers are locked out, and its events are queued before the
// subscription becomes visible. That is what makes the replay-precedes-live
// guarantee hold without an asynchronous replay goroutine.
//
// The returned channel is closed when ctx is cancelled, when the watch
// overflows, or when the hub is closed.
func (h *Hub[T]) Subscribe(ctx context.Context, kind string, cfg *store.WatchCfg[T], snapshot SnapshotFunc[T]) (<-chan *store.Event[T], error) {
	if kind == "" {
		return nil, store.ErrKindRequired
	}
	if cfg == nil {
		cfg = &store.WatchCfg[T]{}
	}
	capacity := cfg.BufferSize
	if capacity <= 0 {
		capacity = store.DefaultWatchBufferSize
	}

	s := &sub[T]{
		kind:       kind,
		eventTypes: cfg.EventTypes,
		capacity:   capacity,
		ch:         make(chan *store.Event[T], 1),
		notify:     make(chan struct{}, 1),
		stop:       make(chan struct{}),
	}

	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil, store.ErrClosed
	}
	if cfg.Initial && snapshot != nil && s.wants(store.EventTypeCreate) {
		evs, err := snapshot()
		if err != nil {
			h.mu.Unlock()
			return nil, err
		}
		for _, ev := range evs {
			s.enqueue(ev)
		}
	}
	if h.subs[kind] == nil {
		h.subs[kind] = make(map[*sub[T]]struct{})
	}
	h.subs[kind][s] = struct{}{}
	h.wg.Add(1)
	h.mu.Unlock()

	go func() {
		defer h.wg.Done()
		s.run(ctx)
		h.remove(kind, s)
	}()

	return s.ch, nil
}

// Publish queues evs for every subscriber of kind. It never blocks on a consumer,
// so it is safe to call while holding a write lock — and callers that want the
// ordering guarantee must do exactly that, so that queue order matches write order.
//
// Passing several events in one call keeps them contiguous for every subscriber,
// which is what bulk writes need.
func (h *Hub[T]) Publish(kind string, evs ...*store.Event[T]) {
	if len(evs) == 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for s := range h.subs[kind] {
		for _, ev := range evs {
			s.enqueue(ev)
		}
	}
}

// Close terminates every subscriber and waits for their channels to be closed,
// so that a caller returning from Close knows no watcher is still live.
func (h *Hub[T]) Close() {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	for _, m := range h.subs {
		for s := range m {
			close(s.stop)
		}
	}
	h.subs = nil
	h.mu.Unlock()

	h.wg.Wait()
}

func (h *Hub[T]) remove(kind string, s *sub[T]) {
	h.mu.Lock()
	defer h.mu.Unlock()
	m, ok := h.subs[kind]
	if !ok {
		return
	}
	delete(m, s)
	if len(m) == 0 {
		delete(h.subs, kind)
	}
}

type sub[T any] struct {
	kind       string
	eventTypes map[store.EventType]struct{}
	capacity   int

	// ch is written to and closed only by run.
	ch     chan *store.Event[T]
	notify chan struct{}
	stop   chan struct{}

	mu         sync.Mutex
	pending    []*store.Event[T]
	overflowed bool
}

func (s *sub[T]) wants(et store.EventType) bool {
	// Terminal signals are never filtered: a consumer that filtered this away
	// could not learn that its view had become incomplete.
	if et == store.EventTypeError {
		return true
	}
	if s.eventTypes == nil {
		return true
	}
	_, ok := s.eventTypes[et]
	return ok
}

// enqueue appends ev to the pending queue, or trips the overflow flag. It is
// called with the hub lock held and never blocks.
func (s *sub[T]) enqueue(ev *store.Event[T]) {
	if !s.wants(ev.EventType) {
		return
	}
	s.mu.Lock()
	if s.overflowed {
		s.mu.Unlock()
		return
	}
	if len(s.pending) >= s.capacity {
		// The consumer has to re-list anyway, so holding the backlog buys
		// nothing; drop it and let run deliver the terminal event.
		s.overflowed = true
		s.pending = nil
	} else {
		s.pending = append(s.pending, ev)
	}
	s.mu.Unlock()

	select {
	case s.notify <- struct{}{}:
	default:
	}
}

// run owns s.ch for its entire lifetime and is the only goroutine that writes to
// or closes it.
func (s *sub[T]) run(ctx context.Context) {
	defer close(s.ch)

	for {
		s.mu.Lock()
		batch := s.pending
		s.pending = nil
		overflowed := s.overflowed
		s.mu.Unlock()

		for _, ev := range batch {
			select {
			case s.ch <- ev:
			case <-ctx.Done():
				return
			case <-s.stop:
				return
			}
		}

		if overflowed {
			select {
			case s.ch <- &store.Event[T]{Kind: s.kind, EventType: store.EventTypeError, Err: store.ErrWatchOverflow}:
			case <-ctx.Done():
			case <-s.stop:
			}
			return
		}

		select {
		case <-s.notify:
		case <-ctx.Done():
			return
		case <-s.stop:
			return
		}
	}
}
