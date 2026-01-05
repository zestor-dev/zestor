---
title: "store package"
linkTitle: "store"
weight: 1
description: >
  API reference for the store package.
---

```go
import "github.com/zestor-dev/zestor/store"
```

The `store` package defines interfaces, types, and options for Zestor stores.

---

## Interfaces

### Reader[T any]

Read-only access to the store.

```go
type Reader[T any] interface {
    Get(kind, key string) (val T, ok bool, err error)
    List(kind string, filter ...FilterFunc[T]) (map[string]T, error)
    Count(kind string) (int, error)
    Keys(kind string) ([]string, error)
    Values(kind string) ([]KeyValue[T], error)
    GetAll() (map[string]map[string]T, error)
}
```

### Writer[T any]

Write access to the store.

```go
type Writer[T any] interface {
    Set(kind, key string, value T) (created bool, err error)
    SetFn(kind, key string, fn func(v T) (T, error)) (changed bool, err error)
    SetAll(kind string, values map[string]T) error
    Delete(kind, key string) (existed bool, prev T, err error)
}
```

### Watcher[T any]

Watch access to the store.

```go
type Watcher[T any] interface {
    Watch(kind string, opts ...WatchOption[T]) (r <-chan *Event[T], cancel func(), err error)
}
```

### ReadWriter[T any]

Combined read and write access.

```go
type ReadWriter[T any] interface {
    Reader[T]
    Writer[T]
}
```

### Store[T any]

Full store interface with all capabilities.

```go
type Store[T any] interface {
    Reader[T]
    Writer[T]
    Watcher[T]
    Close() error
    Dump() string
}
```

---

## Types

### Event[T any]

Represents a change event.

```go
type Event[T any] struct {
    Kind      string    // The kind (e.g., "users")
    Name      string    // The key (e.g., "alice")
    EventType EventType // create, update, or delete
    Object    T         // The value (previous value for delete)
}
```

### EventType

```go
type EventType string

const (
    EventTypeCreate EventType = "create"
    EventTypeUpdate EventType = "update"
    EventTypeDelete EventType = "delete"
)
```

### KeyValue[T any]

Key-value pair returned by `Values()`.

```go
type KeyValue[T any] struct {
    Key   string
    Value T
}
```

### FilterFunc[T any]

Filter function for `List()`.

```go
type FilterFunc[T any] func(key string, val T) bool
```

### StoreOptions[T any]

Configuration options for creating a store.

```go
type StoreOptions[T any] struct {
    CompareFn   CompareFunc[T]
    ValidateFns map[string]ValidateFunc[T]
}
```

### CompareFunc[T any]

Compares two values for equality. Returns `true` if equal.

```go
type CompareFunc[T any] func(prev, new T) bool
```

### ValidateFunc[T any]

Validates a value before write. Returns error to reject.

```go
type ValidateFunc[T any] func(v T) error
```

### WatchCfg[T any]

Configuration for watchers.

```go
type WatchCfg[T any] struct {
    Initial    bool                    // Replay existing data
    EventTypes map[EventType]struct{} // Filter event types
    BufferSize int                     // Channel buffer size
}
```

---

## Functions

### DefaultCompareFunc

Default comparison using `reflect.DeepEqual`.

```go
func DefaultCompareFunc[T any](prev, new T) bool
```

---

## Watch Options

### WithInitialReplay

Replay existing data as Create events when subscribing.

```go
func WithInitialReplay[T any]() WatchOption[T]
```

### WithEventTypes

Filter to specific event types.

```go
func WithEventTypes[T any](eventTypes ...EventType) WatchOption[T]
```

### WithBufferSize

Set channel buffer size (default: 128).

```go
func WithBufferSize[T any](size int) WatchOption[T]
```

---

## Constants

```go
const DefaultWatchBufferSize = 128
```

---

## Errors

Sentinel errors for error checking with `errors.Is()`:

```go
var (
    ErrClosed       = errors.New("store closed")
    ErrKeyNotFound  = errors.New("key not found")
    ErrKindRequired = errors.New("kind required")
)
```

**Usage:**

```go
_, _, err := s.Get("users", "alice")
if errors.Is(err, store.ErrClosed) {
    // Handle closed store
}
```

