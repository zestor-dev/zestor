---
title: "In-Memory Store (gomap)"
linkTitle: "gomap"
weight: 1
description: >
  Fast in-memory store using Go maps.
---

```go
import "github.com/zestor-dev/zestor/store/gomap"
```

The `gomap` package provides a high-performance in-memory implementation of the `store.Store` interface using Go maps with `sync.RWMutex` for thread safety.

## Features

- **Zero Dependencies**: Pure Go, no external packages
- **Maximum Speed**: Direct memory access, no serialization
- **Thread-Safe**: `RWMutex` for concurrent access
- **Full Watch Support**: Real-time event notifications
- **Validation Hooks**: Per-kind validation functions
- **Custom Comparison**: Control when updates trigger events

## Quick Start

```go
import (
    "github.com/zestor-dev/zestor/store"
    "github.com/zestor-dev/zestor/store/gomap"
)

type User struct {
    Name  string
    Email string
}

func main() {
    s := gomap.NewMemStore[User](store.StoreOptions[User]{})
    defer s.Close()

    // CRUD operations
    s.Set("users", "alice", User{Name: "Alice", Email: "alice@example.com"})
    
    user, ok, _ := s.Get("users", "alice")
    if ok {
        fmt.Println(user.Name) // Alice
    }
}
```

## Configuration Options

### Basic Store

```go
s := gomap.NewMemStore[User](store.StoreOptions[User]{})
```

### With Validation

Validate data before writes:

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

// This will fail validation
_, err := s.Set("users", "bob", User{Name: "Bob"})
// err: "email required"
```

### With Custom Comparison

Control when updates trigger events:

```go
s := gomap.NewMemStore[User](store.StoreOptions[User]{
    CompareFn: func(prev, new User) bool {
        // Only consider it "equal" if email matches
        // Other field changes won't trigger update events
        return prev.Email == new.Email
    },
})
```

## Watch & Subscribe

Real-time notifications for data changes:

```go
ch, cancel, _ := s.Watch("users",
    store.WithInitialReplay[User](),    // Replay existing data
    store.WithEventTypes[User](         // Filter event types
        store.EventTypeCreate,
        store.EventTypeUpdate,
    ),
    store.WithBufferSize[User](256),    // Channel buffer size
)
defer cancel()

for event := range ch {
    switch event.EventType {
    case store.EventTypeCreate:
        fmt.Printf("Created: %s\n", event.Name)
    case store.EventTypeUpdate:
        fmt.Printf("Updated: %s\n", event.Name)
    case store.EventTypeDelete:
        fmt.Printf("Deleted: %s\n", event.Name)
    }
}
```

## Performance Characteristics

| Operation | Complexity | Notes |
|-----------|------------|-------|
| Get | O(1) | Direct map lookup |
| Set | O(1) | Map insert + broadcast |
| Delete | O(1) | Map delete + broadcast |
| List | O(n) | Iterates all items in kind |
| Count | O(1) | Map length |
| Watch | O(1) | Channel registration |

## Thread Safety

All operations are protected by `sync.RWMutex`:
- **Read operations** (Get, List, Count, Keys, Values): Use read lock (concurrent)
- **Write operations** (Set, Delete, SetFn, SetAll): Use write lock (exclusive)

```go
// Safe to call from multiple goroutines
var wg sync.WaitGroup
for i := 0; i < 100; i++ {
    wg.Add(1)
    go func(n int) {
        defer wg.Done()
        s.Set("items", fmt.Sprintf("item-%d", n), Item{ID: n})
    }(i)
}
wg.Wait()
```

## Memory Considerations

Data is stored directly in memory:
- No serialization overhead
- Values are stored by value (copied on write)
- Large datasets consume proportional memory
- No automatic eviction (implement manually if needed)

## Use Cases

✅ **Good for:**
- Caching layers
- Test fixtures
- Session storage
- High-frequency access patterns
- Temporary computation results

❌ **Not ideal for:**
- Data that must survive restarts
- Large datasets exceeding available RAM
- Multi-process shared state

## Complete Example

```go
package main

import (
    "fmt"
    "time"

    "github.com/zestor-dev/zestor/store"
    "github.com/zestor-dev/zestor/store/gomap"
)

type Task struct {
    Title     string
    Completed bool
}

func main() {
    s := gomap.NewMemStore[Task](store.StoreOptions[Task]{})
    defer s.Close()

    // Watch for changes
    ch, cancel, _ := s.Watch("tasks", store.WithInitialReplay[Task]())
    defer cancel()

    go func() {
        for ev := range ch {
            fmt.Printf("[%s] %s: %s\n", ev.EventType, ev.Name, ev.Object.Title)
        }
    }()

    // Add tasks
    s.Set("tasks", "task-1", Task{Title: "Buy groceries"})
    s.Set("tasks", "task-2", Task{Title: "Write docs"})

    // Update task
    s.SetFn("tasks", "task-1", func(t Task) (Task, error) {
        t.Completed = true
        return t, nil
    })

    // List incomplete
    tasks, _ := s.List("tasks", func(k string, t Task) bool {
        return !t.Completed
    })
    fmt.Printf("\nIncomplete: %d\n", len(tasks))

    time.Sleep(100 * time.Millisecond)
}
```

Output:
```
[create] task-1: Buy groceries
[create] task-2: Write docs
[update] task-1: Buy groceries

Incomplete: 1
```


