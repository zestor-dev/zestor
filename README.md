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
- **Validation** — Per-kind validation functions
- **Change detection** — Configurable compare function to suppress duplicate events

## Requirements

- Go 1.21+

## Installation

```bash
go get github.com/zestore-dev/zestor
```

## Quick Start

```go
package main

import (
    "fmt"
    "github.com/zestore-dev/zestor/store"
    "github.com/zestore-dev/zestor/store/gomap"
)

type User struct {
    Name  string
    Email string
}

func main() {
    // Create a new store
    s := gomap.NewMemStore[User](store.StoreOptions[User]{})
    defer s.Close()

    // Set a value
    created, _ := s.Set("users", "alice", User{Name: "Alice", Email: "alice@example.com"})
    fmt.Println("Created:", created) // true

    // Get a value
    user, ok, _ := s.Get("users", "alice")
    if ok {
        fmt.Println("Found:", user.Name)
    }

    // List all users
    users, _ := s.List("users")
    fmt.Println("Total users:", len(users))

    // Delete
    existed, prev, _ := s.Delete("users", "alice")
    fmt.Println("Deleted:", existed, prev.Name)
}
```

## Watching for Changes

```go
// Watch for all events on "users" kind
ch, cancel, _ := s.Watch("users")
defer cancel()

go func() {
    for event := range ch {
        fmt.Printf("Event: %s %s/%s\n", event.EventType, event.Kind, event.Name)
    }
}()

// Watch with options
ch, cancel, _ = s.Watch("users",
    store.WithInitialReplay[User](),                    // Replay existing items as Create events
    store.WithEventTypes[User](store.EventTypeDelete), // Only delete events
)
```

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

| Method | Description |
|--------|-------------|
| `Get(kind, key)` | Get a single value |
| `List(kind, filters...)` | List all values, optionally filtered |
| `Keys(kind)` | Get all keys |
| `Values(kind)` | Get all key-value pairs |
| `Count(kind)` | Count items |
| `GetAll()` | Get all kinds and their data |

### Write Operations

| Method | Description |
|--------|-------------|
| `Set(kind, key, value)` | Create or update a value |
| `SetAll(kind, values)` | Bulk set multiple values |
| `SetFn(kind, key, fn)` | Update value using a transform function |
| `Delete(kind, key)` | Delete a value |

### Watch

| Method | Description |
|--------|-------------|
| `Watch(kind, opts...)` | Subscribe to changes |

### Lifecycle

| Method | Description |
|--------|-------------|
| `Close()` | Close the store and all watchers |
| `Dump()` | Debug dump of all data |

