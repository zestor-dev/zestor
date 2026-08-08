package store

import (
	"context"
	"errors"
	"io"
	"reflect"
)

var (
	ErrClosed       = errors.New("store closed")
	ErrKeyNotFound  = errors.New("key not found")
	ErrKindRequired = errors.New("kind required")

	// ErrWatchOverflow terminates a watch whose consumer fell too far behind.
	// It is delivered as the Err field of a final EventTypeError event, after
	// which the channel is closed. The consumer's view of the store is
	// incomplete at that point and must be rebuilt by re-listing and
	// re-watching. See the Watcher contract below.
	ErrWatchOverflow = errors.New("watch overflow: consumer fell behind, re-list required")
)

// Reader provides read-only access to the store.
type Reader[T any] interface {
	Get(ctx context.Context, kind, key string) (val T, ok bool, err error)
	List(ctx context.Context, kind string, filter ...FilterFunc[T]) (map[string]T, error)
	Count(ctx context.Context, kind string) (int, error)
	Keys(ctx context.Context, kind string) ([]string, error)
	Values(ctx context.Context, kind string) ([]KeyValue[T], error)
	GetAll(ctx context.Context) (map[string]map[string]T, error)
}

// SetResult reports what a write actually did. A bare "created" boolean could
// not distinguish an update from a write that changed nothing, which is
// information every backend computes and used to discard.
type SetResult uint8

const (
	// SetUnchanged means the value compared equal to what was already stored:
	// nothing was written and no event was emitted.
	SetUnchanged SetResult = iota
	// SetCreated means the key did not exist and now does.
	SetCreated
	// SetUpdated means the key existed and its value changed.
	SetUpdated
)

func (r SetResult) String() string {
	switch r {
	case SetCreated:
		return "created"
	case SetUpdated:
		return "updated"
	case SetUnchanged:
		return "unchanged"
	}
	return "unknown"
}

// Writer provides write access to the store.
type Writer[T any] interface {
	Set(ctx context.Context, kind, key string, value T) (SetResult, error)
	// SetFn atomically applies fn to the value stored at kind/key. It reports
	// whether the stored value actually changed: false means either fn returned
	// a value equal to the previous one, or an error occurred.
	//
	// fn runs while the store's write lock is held. It must not call back into
	// the store; doing so deadlocks.
	SetFn(ctx context.Context, kind, key string, fn func(v T) (T, error)) (changed bool, err error)

	// SetMany writes several values to one kind. It merges: keys already present
	// in the kind but absent from values are left alone, and no delete events
	// are emitted for them. Keys whose value compares equal to what is already
	// stored are skipped, exactly as a single Set would skip them, so a
	// re-import of unchanged data emits nothing.
	//
	// Events are emitted in key order, so the stream is deterministic.
	SetMany(ctx context.Context, kind string, values map[string]T) error
	Delete(ctx context.Context, kind, key string) (existed bool, prev T, err error)
}

// Watcher provides the ability to watch for changes.
//
// Every implementation guarantees:
//
//  1. Ordering — events for a given kind are delivered in the order the writes
//     were applied. A consumer that applies events in receive order converges on
//     the same state the store holds.
//
//  2. Replay precedes live — with WithInitialReplay, every replayed event is
//     delivered before every event for a write that happened after the call to
//     Watch. The snapshot is taken atomically with the subscription.
//
//  3. No silent gaps — if the consumer falls roughly BufferSize events behind,
//     the watch is terminated rather than degraded: a final event with
//     EventType EventTypeError and Err ErrWatchOverflow is delivered, then the
//     channel is closed. Event type filters never suppress this event.
//
//  4. Exactly-once close — the channel is closed exactly once, when ctx is
//     cancelled, when the watch overflows, or when the store is closed. The
//     consumer can therefore range over it safely.
//
// The watch lives until ctx is cancelled; cancelling ctx is the only way for a
// caller to unsubscribe.
type Watcher[T any] interface {
	Watch(ctx context.Context, kind string, opts ...WatchOption[T]) (<-chan *Event[T], error)
}

// ReadWriter combines Reader and Writer interfaces.
type ReadWriter[T any] interface {
	Reader[T]
	Writer[T]
}

// Store is the full interface combining all capabilities.
type Store[T any] interface {
	Reader[T]
	Writer[T]
	Watcher[T]
	Close() error
}

// Dumper is an optional debugging capability. It is deliberately not part of
// Store: it is not something callers build on, it can move an unbounded amount
// of data, and forcing it into the core interface obliged every implementation
// to provide one with no way to report failure.
//
//	if d, ok := s.(store.Dumper); ok {
//	    _ = d.Dump(ctx, os.Stdout)
//	}
type Dumper interface {
	Dump(ctx context.Context, w io.Writer) error
}

type KeyValue[T any] struct {
	Key   string
	Value T
}

type FilterFunc[T any] func(key string, val T) bool

type Event[T any] struct {
	Kind      string
	Name      string
	EventType EventType
	Object    T // for delete: previous value; for EventTypeError: the zero value
	// Err is set only on EventTypeError, the terminal event of a watch.
	Err error
}

type EventType string

const (
	EventTypeCreate EventType = "create"
	EventTypeUpdate EventType = "update"
	EventTypeDelete EventType = "delete"
	// EventTypeError is the terminal event of a watch. It carries the reason in
	// Event.Err and is followed by the channel closing. It is never filtered out
	// by WithEventTypes, because a consumer that filtered it away could not
	// learn that its view had become incomplete.
	EventTypeError EventType = "error"
)

// Watch options
type WatchOption[T any] func(*WatchCfg[T])

// DefaultWatchBufferSize is the default number of events a watcher may fall
// behind before the watch overflows.
const DefaultWatchBufferSize = 128

type WatchCfg[T any] struct {
	// send current keys as create events immediately
	Initial bool
	// only send events of the specified types
	EventTypes map[EventType]struct{}
	// how far the consumer may fall behind before ErrWatchOverflow
	// (0 means use DefaultWatchBufferSize)
	BufferSize int
}

func WithInitialReplay[T any]() WatchOption[T] {
	return func(w *WatchCfg[T]) {
		w.Initial = true
	}
}

func WithEventTypes[T any](eventTypes ...EventType) WatchOption[T] {
	return func(w *WatchCfg[T]) {
		if w.EventTypes == nil {
			w.EventTypes = make(map[EventType]struct{})
		}
		for _, eventType := range eventTypes {
			w.EventTypes[eventType] = struct{}{}
		}
	}
}

func WithBufferSize[T any](size int) WatchOption[T] {
	return func(w *WatchCfg[T]) {
		w.BufferSize = size
	}
}

type StoreOptions[T any] struct {
	CompareFn   CompareFunc[T]
	ValidateFns map[string]ValidateFunc[T]
}

type ValidateFunc[T any] func(v T) error

type CompareFunc[T any] func(prev, new T) bool

func DefaultCompareFunc[T any](prev, new T) bool {
	return reflect.DeepEqual(prev, new)
}
