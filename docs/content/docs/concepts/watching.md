---
title: "Watching & Events"
linkTitle: "Watching & Events"
weight: 2
description: >
  Real-time notifications with the Watch API.
---

## Overview

Zestor's watch system lets you subscribe to changes in real-time. When data is created, updated, or deleted, watchers receive events through a channel.

## Basic Watching

```go
// Start watching the "users" kind
ch, cancel, err := s.Watch("users")
if err != nil {
    log.Fatal(err)
}
defer cancel() // Always cancel when done

// Process events
for event := range ch {
    fmt.Printf("%s: %s\n", event.EventType, event.Name)
}
```

## Event Types

Three event types are emitted:

| Event | When | `event.Object` contains |
|-------|------|------------------------|
| `EventTypeCreate` | New key is set | The new value |
| `EventTypeUpdate` | Existing key is modified | The new value |
| `EventTypeDelete` | Key is deleted | The previous value |

```go
for event := range ch {
    switch event.EventType {
    case store.EventTypeCreate:
        fmt.Printf("Created %s: %+v\n", event.Name, event.Object)
    case store.EventTypeUpdate:
        fmt.Printf("Updated %s: %+v\n", event.Name, event.Object)
    case store.EventTypeDelete:
        fmt.Printf("Deleted %s (was: %+v)\n", event.Name, event.Object)
    }
}
```

## Event Structure

```go
type Event[T any] struct {
    Kind      string    // The kind (e.g., "users")
    Name      string    // The key (e.g., "alice")
    EventType EventType // create, update, or delete
    Object    T         // The value (or previous value for delete)
}
```

## Watch Options

### Filter by Event Type

Only receive specific event types:

```go
// Only delete events
ch, cancel, _ := s.Watch("users",
    store.WithEventTypes[User](store.EventTypeDelete),
)

// Create and update only (no deletes)
ch, cancel, _ := s.Watch("users",
    store.WithEventTypes[User](
        store.EventTypeCreate,
        store.EventTypeUpdate,
    ),
)
```

### Initial Replay

Receive existing data as `Create` events when subscribing:

```go
// First receive all existing users, then continue watching
ch, cancel, _ := s.Watch("users",
    store.WithInitialReplay[User](),
)
```

This is useful for:
- Building initial state from existing data
- Ensuring you don't miss data that existed before watching
- Implementing "sync" patterns

### Buffer Size

Configure the channel buffer size:

```go
// Larger buffer for high-throughput scenarios
ch, cancel, _ := s.Watch("users",
    store.WithBufferSize[User](1024),
)
```

Default buffer size is 128. If the buffer fills up (slow consumer), events are **dropped** (non-blocking sends).

### Combining Options

Options can be combined:

```go
ch, cancel, _ := s.Watch("users",
    store.WithInitialReplay[User](),
    store.WithEventTypes[User](store.EventTypeCreate, store.EventTypeDelete),
    store.WithBufferSize[User](256),
)
```

## Cancel Function

The `cancel` function returned by `Watch` must be called when you're done watching:

```go
ch, cancel, _ := s.Watch("users")

// Option 1: defer
defer cancel()

// Option 2: explicit cancel
go func() {
    <-stopSignal
    cancel()
}()
```

Calling `cancel()`:
- Closes the event channel
- Removes the watcher from the store
- Is safe to call multiple times

## Multiple Watchers

You can have multiple watchers on the same kind:

```go
// Watcher 1: Log all events
ch1, cancel1, _ := s.Watch("users")
go func() {
    for event := range ch1 {
        log.Printf("Event: %s %s", event.EventType, event.Name)
    }
}()

// Watcher 2: Only track deletes
ch2, cancel2, _ := s.Watch("users",
    store.WithEventTypes[User](store.EventTypeDelete),
)
go func() {
    for event := range ch2 {
        notifyUserDeleted(event.Object)
    }
}()
```

## Event Delivery Guarantees

### Non-Blocking Sends

Events are sent with non-blocking channel sends:

```go
select {
case wch.ch <- ev:
    // delivered
default:
    // dropped (buffer full)
}
```

If your consumer is slow and the buffer fills up, **events will be dropped**. To avoid this:
- Increase buffer size for bursty workloads
- Ensure your event handler is fast
- Offload heavy processing to a worker pool

### No Duplicate Suppression Within Watch

If you call `Set` with the same value, and the store's `CompareFn` returns `true` (values are equal), **no event is emitted**. This prevents unnecessary notifications.

```go
s.Set("users", "alice", User{Name: "Alice"}) // Create event
s.Set("users", "alice", User{Name: "Alice"}) // No event (same value)
s.Set("users", "alice", User{Name: "Alice!"}) // Update event
```

## Patterns

### Watch + Initial State

```go
func syncUsers(s store.Store[User]) map[string]User {
    state := make(map[string]User)
    
    ch, cancel, _ := s.Watch("users",
        store.WithInitialReplay[User](),
    )
    defer cancel()
    
    for event := range ch {
        switch event.EventType {
        case store.EventTypeCreate, store.EventTypeUpdate:
            state[event.Name] = event.Object
        case store.EventTypeDelete:
            delete(state, event.Name)
        }
    }
    
    return state
}
```

### Event Fan-Out

```go
func fanOut[T any](ch <-chan *store.Event[T], handlers ...func(*store.Event[T])) {
    for event := range ch {
        for _, handler := range handlers {
            handler(event)
        }
    }
}

// Usage
ch, cancel, _ := s.Watch("users")
go fanOut(ch,
    logEvent,
    updateMetrics,
    notifyWebsockets,
)
```

