---
title: "gomap package"
linkTitle: "gomap"
weight: 2
description: >
  API reference for the gomap (in-memory) implementation.
---

```go
import "github.com/zestor-dev/zestor/store/gomap"
```

The `gomap` package provides an in-memory implementation of the `store.Store` interface using Go maps.

---

## Functions

### NewMemStore

Creates a new in-memory store.

```go
func NewMemStore[T any](opt store.StoreOptions[T]) store.Store[T]
```

**Parameters:**
- `opt` — Store configuration options

**Returns:**
- `store.Store[T]` — A new store instance

**Example:**

```go
// Basic usage
s := gomap.NewMemStore[User](store.StoreOptions[User]{})

// With options
s := gomap.NewMemStore[User](store.StoreOptions[User]{
    CompareFn: func(a, b User) bool {
        return a.ID == b.ID && a.Email == b.Email
    },
    ValidateFns: map[string]store.ValidateFunc[User]{
        "users": func(u User) error {
            if u.Email == "" {
                return errors.New("email required")
            }
            return nil
        },
    },
})
```

---

## Methods

### Get

Retrieves a value by kind and key.

```go
func (s *memStore[T]) Get(kind, key string) (T, bool, error)
```

**Parameters:**
- `kind` — The kind/collection name
- `key` — The key to look up

**Returns:**
- `T` — The value (zero value if not found)
- `bool` — `true` if found, `false` otherwise
- `error` — `ErrClosed` if store is closed

**Example:**

```go
user, ok, err := s.Get("users", "alice")
if err != nil {
    log.Fatal(err)
}
if ok {
    fmt.Println(user.Name)
}
```

---

### List

Retrieves all values in a kind, optionally filtered.

```go
func (s *memStore[T]) List(kind string, filters ...store.FilterFunc[T]) (map[string]T, error)
```

**Parameters:**
- `kind` — The kind/collection name
- `filters` — Optional filter functions (AND logic)

**Returns:**
- `map[string]T` — Map of key to value for matching items
- `error` — `ErrClosed` if store is closed

**Example:**

```go
// All users
users, _ := s.List("users")

// Filtered
admins, _ := s.List("users", func(key string, u User) bool {
    return u.Role == "admin"
})
```

---

### Count

Returns the number of items in a kind.

```go
func (s *memStore[T]) Count(kind string) (int, error)
```

---

### Keys

Returns all keys in a kind.

```go
func (s *memStore[T]) Keys(kind string) ([]string, error)
```

---

### Values

Returns all key-value pairs in a kind.

```go
func (s *memStore[T]) Values(kind string) ([]store.KeyValue[T], error)
```

---

### GetAll

Returns all data from all kinds.

```go
func (s *memStore[T]) GetAll() (map[string]map[string]T, error)
```

---

### Set

Creates or updates a value.

```go
func (s *memStore[T]) Set(kind, key string, value T) (bool, error)
```

**Parameters:**
- `kind` — The kind/collection name
- `key` — The key
- `value` — The value to set

**Returns:**
- `bool` — `true` if created, `false` if updated
- `error` — Validation error or `ErrClosed`

**Events:**
- Emits `EventTypeCreate` if key didn't exist
- Emits `EventTypeUpdate` if key existed and value changed
- No event if value unchanged (per `CompareFn`)

---

### SetAll

Bulk set multiple values.

```go
func (s *memStore[T]) SetAll(kind string, values map[string]T) error
```

**Events:**
- Emits `EventTypeCreate` for new keys
- Emits `EventTypeUpdate` for existing keys

---

### SetFn

Updates a value using a transform function.

```go
func (s *memStore[T]) SetFn(kind, key string, fn func(v T) (T, error)) (bool, error)
```

**Parameters:**
- `fn` — Function that receives current value and returns new value

**Returns:**
- `bool` — Always `false` (only updates existing keys)
- `error` — `ErrKeyNotFound` if key doesn't exist, or transform error

**Example:**

```go
// Increment a counter
s.SetFn("counters", "visits", func(v int) (int, error) {
    return v + 1, nil
})
```

---

### Delete

Deletes a value.

```go
func (s *memStore[T]) Delete(kind, key string) (bool, T, error)
```

**Returns:**
- `bool` — `true` if key existed
- `T` — The previous value (zero if didn't exist)
- `error` — `ErrClosed` if store is closed

**Events:**
- Emits `EventTypeDelete` with previous value

---

### Watch

Subscribes to changes in a kind.

```go
func (s *memStore[T]) Watch(kind string, opts ...store.WatchOption[T]) (<-chan *store.Event[T], func(), error)
```

**Parameters:**
- `kind` — The kind to watch (required, non-empty)
- `opts` — Watch options

**Returns:**
- `<-chan *store.Event[T]` — Channel of events
- `func()` — Cancel function (must be called when done)
- `error` — `ErrKindRequired` or `ErrClosed`

**Example:**

```go
ch, cancel, err := s.Watch("users",
    store.WithInitialReplay[User](),
    store.WithEventTypes[User](store.EventTypeCreate),
    store.WithBufferSize[User](256),
)
if err != nil {
    return err
}
defer cancel()

for event := range ch {
    // Handle event
}
```

---

### Close

Closes the store and all watchers.

```go
func (s *memStore[T]) Close() error
```

After closing:
- All watcher channels are closed
- All operations return `ErrClosed`

---

### Dump

Returns a debug string representation of all data.

```go
func (s *memStore[T]) Dump() string
```

**Example output:**

```
users:
  alice: {Name:Alice Email:alice@example.com}
  bob: {Name:Bob Email:bob@example.com}
products:
  laptop: {Name:Laptop Price:999}
```

