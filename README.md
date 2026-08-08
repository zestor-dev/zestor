<p align="center">
  <img src="./docs/static/images/logo.svg" alt="Zestor Logo" width="180" style="margin-bottom: 1rem;" />
</p>

# Zestor

A generic, type-safe, in-memory key-value store for Go with watch/subscribe capabilities.

## Features

- **Generic** — Works with any type `T`
- **Multi-kind** — Organize data by "kind" (like tables/collections)
- **Thread-safe** — Concurrent read/write with `sync.RWMutex`
- **Watch/Subscribe** — Real-time notifications for create, update, and delete events
- **Ordered, gap-free watches** — see [Delivery guarantees](#delivery-guarantees)

### Backend support

| | `gomap` | `sqlite` | `postgres` |
|---|---|---|---|
| Persistence | — | ✅ | ✅ |
| Cross-process watch | — | — | ✅ |
| Per-kind validation (`ValidateFns`) | ✅ | — | — |
| Custom change detection (`CompareFn`) | ✅ | — | — |

## Requirements

- Go 1.25+

## Installation

```bash
go get github.com/zestor-dev/zestor
```

Besides [`store/gomap`](store/gomap/), optional backends include [`store/sqlite`](store/sqlite/) (embedded file) and [`store/postgres`](store/postgres/) (PostgreSQL with cross-process watch).

## Quick Start

```go
package main

import (
    "context"
    "fmt"

    "github.com/zestor-dev/zestor/store"
    "github.com/zestor-dev/zestor/store/gomap"
)

type User struct {
    Name  string
    Email string
}

func main() {
    ctx := context.Background()

    // Create a new store
    s := gomap.NewMemStore[User](store.StoreOptions[User]{})
    defer s.Close()

    // Set reports what it actually did: created, updated, or unchanged
    res, _ := s.Set(ctx, "users", "alice", User{Name: "Alice", Email: "alice@example.com"})
    fmt.Println("Set:", res) // created

    // Get a value
    user, ok, _ := s.Get(ctx, "users", "alice")
    if ok {
        fmt.Println("Found:", user.Name)
    }

    // List all users
    users, _ := s.List(ctx, "users")
    fmt.Println("Total users:", len(users))

    // Delete
    existed, prev, _ := s.Delete(ctx, "users", "alice")
    fmt.Println("Deleted:", existed, prev.Name)
}
```

## Watching for Changes

A watch lives until its context is cancelled — that is the only way to unsubscribe.

```go
// Watch for all events on "users" kind
watchCtx, stopWatching := context.WithCancel(ctx)
defer stopWatching()

ch, _ := s.Watch(watchCtx, "users")

go func() {
    for event := range ch {
        // A watcher that falls behind is told so rather than silently losing
        // events: this is the last event, and the channel closes next.
        if event.Err != nil {
            log.Printf("watch ended: %v — re-list and re-watch to resync", event.Err)
            return
        }
        fmt.Printf("Event: %s %s/%s\n", event.EventType, event.Kind, event.Name)
    }
}()

// Watch with options
ch, _ = s.Watch(watchCtx, "users",
    store.WithInitialReplay[User](),                   // Replay existing items as Create events
    store.WithEventTypes[User](store.EventTypeDelete), // Only delete events
)
```

### Delivery guarantees

- **Ordering** — events arrive in the order the writes were applied.
- **Replay precedes live** — with `WithInitialReplay`, every replayed event arrives before
  any event for a write that happened after the `Watch` call.
- **No silent gaps** — a consumer that falls roughly `BufferSize` events behind receives a
  final event carrying `ErrWatchOverflow` and then the channel closes, rather than quietly
  missing events. Event type filters never suppress that signal.

## Validation

```go
s := gomap.NewMemStore[User](store.StoreOptions[User]{
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

## Custom Compare Function

Avoid spurious update events when values haven't meaningfully changed:

```go
s := gomap.NewMemStore[User](store.StoreOptions[User]{
    CompareFn: func(prev, new User) bool {
        return prev.Email == new.Email // Only compare email
    },
})
```

## API Reference

### Read Operations

All methods take a `context.Context` as their first argument.

| Method | Description |
|--------|-------------|
| `Get(ctx, kind, key)` | Get a single value |
| `List(ctx, kind, filters...)` | List all values, optionally filtered |
| `Keys(ctx, kind)` | Get all keys |
| `Values(ctx, kind)` | Get all key-value pairs |
| `Count(ctx, kind)` | Count items |
| `GetAll(ctx)` | Get all kinds and their data |

### Write Operations

| Method | Description |
|--------|-------------|
| `Set(ctx, kind, key, value)` | Write a value; returns `SetCreated` / `SetUpdated` / `SetUnchanged` |
| `SetMany(ctx, kind, values)` | Bulk write (merges; leaves absent keys alone, skips unchanged ones) |
| `SetFn(ctx, kind, key, fn)` | Atomically update a value using a transform function |
| `Delete(ctx, kind, key)` | Delete a value |

### Watch

| Method | Description |
|--------|-------------|
| `Watch(ctx, kind, opts...)` | Subscribe to changes; cancel `ctx` to unsubscribe |

### Lifecycle

| Method | Description |
|--------|-------------|
| `Close()` | Close the store and all watchers |

`Dump` is not part of `Store`. Backends that support it implement the optional
`store.Dumper` interface:

```go
if d, ok := s.(store.Dumper); ok {
    _ = d.Dump(ctx, os.Stdout)
}
```

