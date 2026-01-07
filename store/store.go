package store

import (
	"errors"
	"reflect"
)

var (
	ErrClosed       = errors.New("store closed")
	ErrKeyNotFound  = errors.New("key not found")
	ErrKindRequired = errors.New("kind required")
)

// Reader provides read-only access to the store.
type Reader[T any] interface {
	Get(kind, key string) (val T, ok bool, err error)
	List(kind string, filter ...FilterFunc[T]) (map[string]T, error)
	Count(kind string) (int, error)
	Keys(kind string) ([]string, error)
	Values(kind string) ([]KeyValue[T], error)
	GetAll() (map[string]map[string]T, error)
}

// Writer provides write access to the store.
type Writer[T any] interface {
	Set(kind, key string, value T) (created bool, err error)
	SetFn(kind, key string, fn func(v T) (T, error)) (changed bool, err error)
	SetAll(kind string, values map[string]T) error
	Delete(kind, key string) (existed bool, prev T, err error)
}

// Watcher provides the ability to watch for changes.
type Watcher[T any] interface {
	Watch(kind string, opts ...WatchOption[T]) (r <-chan *Event[T], cancel func(), err error)
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
	Dump() string
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
	Object    T // for delete: previous value
}

type EventType string

const (
	EventTypeCreate EventType = "create"
	EventTypeUpdate EventType = "update"
	EventTypeDelete EventType = "delete"
)

// Watch options
type WatchOption[T any] func(*WatchCfg[T])

// DefaultWatchBufferSize is the default channel buffer size for watchers.
const DefaultWatchBufferSize = 128

type WatchCfg[T any] struct {
	// send current keys as create events immediately
	Initial bool
	// only send events of the specified types
	EventTypes map[EventType]struct{}
	// channel buffer size (0 means use default)
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
