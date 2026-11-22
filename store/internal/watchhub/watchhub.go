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

// Watching reports whether kind has a subscriber.
func (h *Hub[T]) Watching(kind string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return !h.closed && len(h.subs[kind]) > 0
}

// Kinds returns the kinds that currently have a subscriber.
func (h *Hub[T]) Kinds() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil
	}
	out := make([]string, 0, len(h.subs))
	for kind := range h.subs {
		out = append(out, kind)
	}
	return out
}

// Hooks are backend callbacks that run while publishers are locked out, so that
// everything they do is ordered ahead of the new subscription.
//
// A backend whose publication lags its writes must additionally ensure, by its
// own means, that nothing is mid-publish when it calls Subscribe. The hub can
// order a subscription against publishers that have not started, but not
// against a batch a publisher is already holding. See the drain lock in
// store/postgres.
type Hooks[T any] struct {
	// Snapshot returns the events that make up the initial replay. It is called
	// only when the watcher asked for a replay and accepts create events.
	Snapshot func() ([]*store.Event[T], error)
}

// Subscribe registers a watcher for kind and returns its event channel.
//
// The snapshot runs while publishers are locked out, and the subscription is
// only made visible afterwards. That ordering is what makes the
// replay-precedes-live guarantee hold without an asynchronous replay goroutine.
//
// The returned channel is closed when ctx is cancelled, when the watch
// overflows, or when the hub is closed.
func (h *Hub[T]) Subscribe(ctx context.Context, kind string, cfg *store.WatchCfg[T], hooks Hooks[T]) (<-chan *store.Event[T], error) {
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
	if cfg.Initial && hooks.Snapshot != nil && s.wants(store.EventTypeCreate) {
		evs, err := hooks.Snapshot()
		if err != nil {
			h.mu.Unlock()
			return nil, err
		}
		s.replay = evs
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

// Fail ends every subscriber of kind. Events already queued are delivered
// first, then a terminal EventTypeError carrying err, then the channel is
// closed. A nil err or a closed hub does nothing.
func (h *Hub[T]) Fail(kind string, err error) {
	if err == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	for s := range h.subs[kind] {
		s.fail(err)
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

	// replay is the initial snapshot, delivered by run ahead of pending. It is
	// kept out of pending so that capacity measures lag behind live writes
	// only: a snapshot larger than capacity must not overflow a watch that
	// has not had a chance to read anything yet.
	replay []*store.Event[T]

	mu sync.Mutex
	// pending is accepted events not yet taken for delivery. inflight is the
	// batch run is currently sending. Their sum is how far the consumer is
	// behind, and it is what capacity limits. The initial replay is neither.
	pending    []*store.Event[T]
	inflight   int
	overflowed bool
	// failed ends the watch after the events already queued. failErr is the
	// terminal error, set together with failed.
	failed  bool
	failErr error
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
	if s.overflowed || s.failed {
		s.mu.Unlock()
		return
	}
	// inflight counts too. Otherwise a batch already taken for delivery stops
	// counting, and a second full queue can build up behind it: the consumer
	// falls behind by about twice the buffer before the watch ends.
	if s.inflight+len(s.pending) >= s.capacity {
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

	replay := s.replay
	s.replay = nil
	for _, ev := range replay {
		select {
		case s.ch <- ev:
		case <-ctx.Done():
			return
		case <-s.stop:
			s.reportClosed(ctx)
			return
		}
	}

	for {
		s.mu.Lock()
		batch := s.pending
		s.pending = nil
		s.inflight = len(batch)
		overflowed := s.overflowed
		failed := s.failed
		failErr := s.failErr
		s.mu.Unlock()

		for _, ev := range batch {
			select {
			case s.ch <- ev:
				s.mu.Lock()
				s.inflight--
				s.mu.Unlock()
			case <-ctx.Done():
				return
			case <-s.stop:
				s.reportClosed(ctx)
				return
			}
		}

		if failed {
			select {
			case s.ch <- &store.Event[T]{Kind: s.kind, EventType: store.EventTypeError, Err: failErr}:
			case <-ctx.Done():
			case <-s.stop:
				s.reportClosed(ctx)
			}
			return
		}
		if overflowed {
			select {
			case s.ch <- &store.Event[T]{Kind: s.kind, EventType: store.EventTypeError, Err: store.ErrWatchOverflow}:
			case <-ctx.Done():
			case <-s.stop:
				s.reportClosed(ctx)
			}
			return
		}

		select {
		case <-s.notify:
		case <-ctx.Done():
			return
		case <-s.stop:
			s.reportClosed(ctx)
			return
		}
	}
}

// reportClosed delivers ErrClosed to a consumer that is waiting for an event.
// The send does not block. Close waits for run to return, so blocking here
// would deadlock it against a consumer that is not reading; a consumer that is
// waiting has room, because it has already taken whatever the channel held.
// fail marks the watch done once the events already queued have been
// delivered. It does not drop that queue: those events happened.
func (s *sub[T]) fail(err error) {
	s.mu.Lock()
	if s.failed || s.overflowed {
		s.mu.Unlock()
		return
	}
	s.failed = true
	s.failErr = err
	s.mu.Unlock()

	select {
	case s.notify <- struct{}{}:
	default:
	}
}

func (s *sub[T]) reportClosed(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	select {
	case s.ch <- &store.Event[T]{Kind: s.kind, EventType: store.EventTypeError, Err: store.ErrClosed}:
	default:
	}
}
