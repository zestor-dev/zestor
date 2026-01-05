---
title: "Quick Start"
linkTitle: "Quick Start"
weight: 2
description: >
  Build your first Zestor application.
---

This guide walks you through building a simple application using Zestor.

## Create a Store

First, define your data type and create a store:

```go
package main

import (
    "fmt"
    "github.com/zestor-dev/zestor/store"
    "github.com/zestor-dev/zestor/store/gomap"
)

type User struct {
    Name  string
    Email string
    Role  string
}

func main() {
    // Create a new store for User types
    s := gomap.NewMemStore[User](store.StoreOptions[User]{})
    defer s.Close()
}
```

## Basic CRUD Operations

### Create / Update

Use `Set` to create or update values:

```go
// Create a user (returns created=true)
created, err := s.Set("users", "alice", User{
    Name:  "Alice",
    Email: "alice@example.com",
    Role:  "admin",
})
fmt.Println("Created:", created) // true

// Update the same user (returns created=false)
created, err = s.Set("users", "alice", User{
    Name:  "Alice Smith",
    Email: "alice@example.com",
    Role:  "admin",
})
fmt.Println("Created:", created) // false (it was an update)
```

### Read

Use `Get` to retrieve a single value:

```go
user, ok, err := s.Get("users", "alice")
if ok {
    fmt.Printf("Found: %s (%s)\n", user.Name, user.Email)
}
```

Use `List` to retrieve all values in a kind:

```go
users, err := s.List("users")
for key, user := range users {
    fmt.Printf("%s: %s\n", key, user.Name)
}
```

### Delete

Use `Delete` to remove a value:

```go
existed, previousUser, err := s.Delete("users", "alice")
if existed {
    fmt.Printf("Deleted: %s\n", previousUser.Name)
}
```

## Watching for Changes

One of Zestor's most powerful features is the ability to watch for changes:

```go
// Start watching before making changes
ch, cancel, err := s.Watch("users")
if err != nil {
    log.Fatal(err)
}
defer cancel()

// Process events in a goroutine
go func() {
    for event := range ch {
        switch event.EventType {
        case store.EventTypeCreate:
            fmt.Printf("Created: %s -> %s\n", event.Name, event.Object.Name)
        case store.EventTypeUpdate:
            fmt.Printf("Updated: %s -> %s\n", event.Name, event.Object.Name)
        case store.EventTypeDelete:
            fmt.Printf("Deleted: %s (was %s)\n", event.Name, event.Object.Name)
        }
    }
}()

// These operations will trigger events
s.Set("users", "bob", User{Name: "Bob", Email: "bob@example.com"})
s.Set("users", "bob", User{Name: "Bob Smith", Email: "bob@example.com"})
s.Delete("users", "bob")
```

### Watch Options

Filter events by type:

```go
// Only watch for delete events
ch, cancel, _ := s.Watch("users",
    store.WithEventTypes[User](store.EventTypeDelete),
)
```

Replay existing data on subscribe:

```go
// Receive all existing items as Create events, then continue watching
ch, cancel, _ := s.Watch("users",
    store.WithInitialReplay[User](),
)
```

Custom buffer size:

```go
// Use a larger buffer for high-throughput scenarios
ch, cancel, _ := s.Watch("users",
    store.WithBufferSize[User](1024),
)
```

## Filtering Data

Use filter functions to query data:

```go
// Get only admin users
admins, _ := s.List("users", func(key string, user User) bool {
    return user.Role == "admin"
})

// Combine multiple filters (AND logic)
activeAdmins, _ := s.List("users",
    func(key string, user User) bool { return user.Role == "admin" },
    func(key string, user User) bool { return user.Email != "" },
)
```

## Complete Example

```go
package main

import (
    "fmt"
    "time"
    
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

    // Set up watcher
    ch, cancel, _ := s.Watch("users", store.WithInitialReplay[User]())
    defer cancel()

    go func() {
        for event := range ch {
            fmt.Printf("[%s] %s: %s\n", event.EventType, event.Name, event.Object.Name)
        }
    }()

    // Add some users
    s.Set("users", "alice", User{Name: "Alice", Email: "alice@example.com"})
    s.Set("users", "bob", User{Name: "Bob", Email: "bob@example.com"})

    time.Sleep(100 * time.Millisecond)

    // Query
    count, _ := s.Count("users")
    fmt.Printf("\nTotal users: %d\n", count)
}
```

## Next Steps

- Learn about [Concepts](/docs/concepts/) like kinds, validation, and interface segregation
- Explore the full [API Reference](/docs/api/)

