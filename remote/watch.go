package remote

import (
	"context"
	"fmt"
	"sync"

	"connectrpc.com/connect"

	storev1 "github.com/zestor-dev/zestor/remote/gen/zestor/store/v1"
	"github.com/zestor-dev/zestor/store"
)

// Watch subscribes on the server and returns once the server confirms the
// subscription, so every write after Watch returns is delivered.
//
// The server and its connection buffer events the consumer has not read yet,
// so the client reads the stream as fast as it arrives and applies BufferSize
// itself. Replayed events do not count, as with a local store.
func (c *Client[T]) Watch(ctx context.Context, kind string, opts ...store.WatchOption[T]) (<-chan *store.Event[T], error) {
	if err := c.guard(ctx); err != nil {
		return nil, err
	}
	defer c.done()

	cfg := &store.WatchCfg[T]{}
	for _, o := range opts {
		if o != nil {
			o(cfg)
		}
	}
	req := &storev1.WatchRequest{Kind: kind, InitialReplay: cfg.Initial}
	if cfg.BufferSize > 0 {
		req.BufferSize = int32(min(cfg.BufferSize, 1<<31-1))
	}
	for et := range cfg.EventTypes {
		if pt := eventTypeToProto(et); pt != storev1.EventType_EVENT_TYPE_UNSPECIFIED {
			req.EventTypes = append(req.EventTypes, pt)
		}
	}
	// Only the error type was asked for, so no data event may arrive. An empty
	// list would mean all types; UNSPECIFIED matches none.
	if len(cfg.EventTypes) > 0 && len(req.EventTypes) == 0 {
		req.EventTypes = []storev1.EventType{storev1.EventType_EVENT_TYPE_UNSPECIFIED}
	}

	streamCtx, cancel := context.WithCancel(ctx)
	stream, err := c.rpc.Watch(streamCtx, connect.NewRequest(req))
	if err != nil {
		cancel()
		return nil, c.fail(ctx, err)
	}
	if !stream.Receive() {
		err := stream.Err()
		cancel()
		_ = stream.Close()
		if err == nil {
			err = ErrDisconnected
		}
		return nil, c.fail(ctx, err)
	}
	if stream.Msg().GetSubscribed() == nil {
		cancel()
		_ = stream.Close()
		return nil, fmt.Errorf("remote watch: server sent %T before confirming the subscription", stream.Msg().GetMessage())
	}

	capacity := cfg.BufferSize
	if capacity <= 0 {
		capacity = store.DefaultWatchBufferSize
	}
	w := &watch[T]{
		client:   c,
		kind:     kind,
		ctx:      ctx,
		cancel:   cancel,
		stream:   stream,
		capacity: capacity,
		out:      make(chan *store.Event[T], 1),
		notify:   make(chan struct{}, 1),
		stop:     make(chan struct{}),
	}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		cancel()
		_ = stream.Close()
		return nil, store.ErrClosed
	}
	c.watches[w] = struct{}{}
	c.mu.Unlock()

	w.wg.Add(2)
	go w.receive()
	go w.deliver()
	go func() {
		w.wg.Wait()
		c.mu.Lock()
		delete(c.watches, w)
		c.mu.Unlock()
	}()
	return w.out, nil
}

type queued[T any] struct {
	ev   *store.Event[T]
	live bool
}

type watch[T any] struct {
	client *Client[T]
	kind   string
	ctx    context.Context
	cancel context.CancelFunc
	stream *connect.ServerStreamForClient[storev1.WatchResponse]

	capacity int
	// out is written to and closed only by deliver.
	out      chan *store.Event[T]
	notify   chan struct{}
	stop     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup

	mu sync.Mutex
	// queue holds events received and not yet taken for delivery. live counts
	// the live ones among them, plus the one deliver is sending, if live. That
	// sum is how far the consumer is behind, and it is what capacity limits.
	queue []queued[T]
	live  int
	// ended is set once no more events will be queued. end is the terminal
	// event to deliver after the queue, or nil when the consumer cancelled.
	ended bool
	end   *store.Event[T]
}

func (w *watch[T]) wake() {
	select {
	case w.notify <- struct{}{}:
	default:
	}
}

// push queues ev, or ends the watch if a live event finds the consumer a full
// buffer behind. It reports whether receiving should continue.
func (w *watch[T]) push(ev *store.Event[T]) bool {
	w.mu.Lock()
	defer w.wake()
	defer w.mu.Unlock()
	if w.ended {
		return false
	}
	live := !ev.Replay
	if live && w.live >= w.capacity {
		// The consumer has to re-list anyway, so the backlog is dropped.
		w.queue = nil
		w.ended = true
		w.end = w.errorEvent(store.ErrWatchOverflow)
		w.cancel()
		return false
	}
	w.queue = append(w.queue, queued[T]{ev: ev, live: live})
	if live {
		w.live++
	}
	return true
}

// finish ends the watch after the events already queued. A nil err means the
// consumer cancelled, which sends no event.
func (w *watch[T]) finish(err error) {
	w.mu.Lock()
	if !w.ended {
		w.ended = true
		if err != nil {
			w.end = w.errorEvent(err)
		}
	}
	w.mu.Unlock()
	w.wake()
}

func (w *watch[T]) errorEvent(err error) *store.Event[T] {
	return &store.Event[T]{Kind: w.kind, EventType: store.EventTypeError, Err: err}
}

func (w *watch[T]) receive() {
	defer w.wg.Done()
	defer func() { _ = w.stream.Close() }()
	for w.stream.Receive() {
		switch m := w.stream.Msg().GetMessage().(type) {
		case *storev1.WatchResponse_Event:
			ev, err := w.decode(m.Event)
			if err != nil {
				w.finish(err)
				return
			}
			if !w.push(ev) {
				return
			}
		case *storev1.WatchResponse_Failure:
			w.finish(fromFailure(m.Failure, w.client.known))
			return
		}
	}
	select {
	case <-w.stop:
		// Close is ending the watch. deliver sees stop and reports ErrClosed;
		// marking the watch ended here could let it exit without doing so.
		return
	default:
	}
	if w.ctx.Err() != nil {
		w.finish(nil)
		return
	}
	err := w.stream.Err()
	if err == nil {
		w.finish(ErrDisconnected)
		return
	}
	w.finish(fmt.Errorf("%w: %v", ErrDisconnected, err))
}

func (w *watch[T]) decode(e *storev1.Event) (*store.Event[T], error) {
	et, ok := eventTypeFromProto(e.GetType())
	if !ok {
		return nil, fmt.Errorf("%w: unknown event type %v for %s/%s", store.ErrDecode, e.GetType(), w.kind, e.GetKey())
	}
	var v T
	if err := w.client.codec.Unmarshal(e.GetValue(), &v); err != nil {
		return nil, fmt.Errorf("%w: %s/%s: %v", store.ErrDecode, w.kind, e.GetKey(), err)
	}
	return &store.Event[T]{Kind: w.kind, Name: e.GetKey(), EventType: et, Object: v, Replay: e.GetReplay()}, nil
}

// deliver owns out for its entire lifetime and is the only goroutine that
// writes to or closes it.
func (w *watch[T]) deliver() {
	defer w.wg.Done()
	defer close(w.out)
	defer w.cancel()
	for {
		w.mu.Lock()
		if len(w.queue) > 0 {
			next := w.queue[0]
			w.queue[0] = queued[T]{}
			w.queue = w.queue[1:]
			w.mu.Unlock()

			select {
			case w.out <- next.ev:
			case <-w.ctx.Done():
				return
			case <-w.stop:
				w.reportClosed()
				return
			}
			if next.live {
				w.mu.Lock()
				w.live--
				w.mu.Unlock()
			}
			continue
		}
		if w.ended {
			end := w.end
			w.mu.Unlock()
			if end != nil {
				select {
				case w.out <- end:
				case <-w.ctx.Done():
				case <-w.stop:
					w.reportClosed()
				}
			}
			return
		}
		w.mu.Unlock()

		select {
		case <-w.notify:
		case <-w.ctx.Done():
			return
		case <-w.stop:
			w.reportClosed()
			return
		}
	}
}

// reportClosed delivers ErrClosed to a consumer that is waiting. The send
// does not block, so Close cannot hang on a consumer that is not reading.
func (w *watch[T]) reportClosed() {
	if w.ctx.Err() != nil {
		return
	}
	select {
	case w.out <- w.errorEvent(store.ErrClosed):
	default:
	}
}

// close ends the watch for Client.Close and waits for its goroutines.
func (w *watch[T]) close() {
	w.stopOnce.Do(func() { close(w.stop) })
	w.cancel()
	w.wg.Wait()
}
