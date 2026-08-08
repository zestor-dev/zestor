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
// The watch lives until this context is cancelled.
watchCtx, stopWatching := context.WithCancel(ctx)
defer stopWatching()

ch, err := s.Watch(watchCtx, "users")
if err != nil {
    log.Fatal(err)
} // cancelling the context is how you unsubscribe

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
watchCtx, stopWatching := context.WithCancel(ctx)
defer stopWatching()

ch, _ := s.Watch(watchCtx, "users",
    store.WithEventTypes[User](store.EventTypeDelete),
)

// Create and update only (no deletes)
ch, _ := s.Watch(watchCtx, "users",
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
ch, _ := s.Watch(watchCtx, "users",
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
ch, _ := s.Watch(watchCtx, "users",
    store.WithBufferSize[User](1024),
)
```

Default is 128. `BufferSize` sets how far the consumer may fall behind before the watch overflows and is terminated with `ErrWatchOverflow` — see [No silent gaps](#no-silent-gaps).

### Combining Options

Options can be combined:

```go
watchCtx, stopWatching := context.WithCancel(ctx)
defer stopWatching()

ch, _ := s.Watch(watchCtx, "users",
    store.WithInitialReplay[User](),
    store.WithEventTypes[User](store.EventTypeCreate, store.EventTypeDelete),
    store.WithBufferSize[User](256),
)
```

## Unsubscribing

`Watch` does not return a cancel function. The watch lives until the context you
passed it is cancelled — that is the only way to unsubscribe, and it means a
watch cannot outlive the scope that owns it by accident.

```go
watchCtx, stopWatching := context.WithCancel(ctx)

// Option 1: tied to the enclosing scope
defer stopWatching()

// Option 2: tied to a signal
go func() {
    <-shutdown
    stopWatching()
}()
```

Cancelling the context:

- closes the event channel, so a `range` over it terminates
- removes the watcher from the store
- is safe to do more than once, and safe to do concurrently with a write

Because the channel is closed exactly once, by the goroutine that owns it, a
consumer can always `range` over it without a separate done channel.

## Multiple Watchers

You can have multiple watchers on the same kind:

```go
// Watcher 1: Log all events
watchCtx, stopWatching := context.WithCancel(ctx)
defer stopWatching()

ch1, _ := s.Watch(watchCtx, "users")
go func() {
    for event := range ch1 {
        log.Printf("Event: %s %s", event.EventType, event.Name)
    }
}()

// Watcher 2: Only track deletes
ch2, _ := s.Watch(watchCtx, "users",
    store.WithEventTypes[User](store.EventTypeDelete),
)
go func() {
    for event := range ch2 {
        notifyUserDeleted(event.Object)
    }
}()
```

## Event Delivery Guarantees

Every backend gives you the same four guarantees.

### Ordering

Events for a kind arrive in the order the writes were applied. A consumer that
applies events in receive order converges on the state the store actually holds,
even when several goroutines write the same key concurrently.

### Replay precedes live

With `WithInitialReplay`, every replayed event is delivered before any event for
a write that happened after the `Watch` call. The snapshot is taken atomically
with the subscription, so a stale replayed value can never overwrite a newer one
you already received.

### No silent gaps

If your consumer falls roughly `BufferSize` events behind, the watch is
**terminated rather than degraded**: you receive a final event with
`EventType` `EventTypeError` and `Err` set to `store.ErrWatchOverflow`, and then
the channel closes.

```go
for event := range ch {
    if event.Err != nil {
        // Our view is now incomplete. Rebuild it.
        return resync()
    }
    apply(event)
}
```

This is the one case worth designing for. A store that silently dropped events
would leave your replica quietly wrong forever; instead you are told, and the
correct response is to re-`List` and re-`Watch`.

Event type filters never suppress the terminal event — a consumer that filtered
it away could not learn that its view had become incomplete.

To avoid overflowing in the first place:

- raise `BufferSize` for bursty workloads
- keep the handler fast, and offload heavy work to a worker pool

### Exactly-once close

The channel is closed exactly once — when the context is cancelled, when the
watch overflows, or when the store is closed.

### No-op writes emit nothing

If you `Set` a value equal to what is already stored (per the store's
`CompareFn`, where supported), nothing is written and no event is emitted.
`SetMany` skips unchanged keys the same way, so re-importing identical data is
silent.

```go
s.Set(ctx, "users", "alice", User{Name: "Alice"})  // create event
s.Set(ctx, "users", "alice", User{Name: "Alice"})  // no event, returns SetUnchanged
s.Set(ctx, "users", "alice", User{Name: "Alice!"}) // update event
```

## Patterns

### Watch + Initial State

```go
// mirror keeps a local replica of a kind until ctx is cancelled. It returns an
// error if the watch overflowed, because at that point the replica is incomplete
// and the caller has to start again.
func mirror(ctx context.Context, s store.Store[User], apply func(map[string]User)) error {
    ch, err := s.Watch(ctx, "users", store.WithInitialReplay[User]())
    if err != nil {
        return err
    }

    state := make(map[string]User)
    for event := range ch {
        if event.Err != nil {
            return event.Err // ErrWatchOverflow: re-list and re-watch
        }
        switch event.EventType {
        case store.EventTypeCreate, store.EventTypeUpdate:
            state[event.Name] = event.Object
        case store.EventTypeDelete:
            delete(state, event.Name)
        }
        apply(state)
    }
    return ctx.Err() // the channel closed because ctx was cancelled
}
```

### Event Fan-Out

```go
func fanOut[T any](ch <-chan *store.Event[T], handlers ...func(*store.Event[T])) {
    for event := range ch {
        if event.Err != nil {
            log.Printf("watch ended: %v", event.Err)
            return
        }
        for _, handler := range handlers {
            handler(event)
        }
    }
}

// Usage
ch, _ := s.Watch(watchCtx, "users")
go fanOut(ch,
    logEvent,
    updateMetrics,
    notifyWebsockets,
)
```

