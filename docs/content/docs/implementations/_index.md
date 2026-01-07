---
title: "Implementations"
linkTitle: "Implementations"
weight: 4
description: >
  Available store implementations and when to use each.
---

Zestor provides multiple store implementations, all sharing the same `store.Store[T]` interface. This means you can swap implementations without changing your application code.

## Available Implementations

| Implementation | Storage | Persistence | Best For |
|---------------|---------|-------------|----------|
| [gomap](gomap/) | Memory | ❌ Ephemeral | Caching, testing, high-speed access |
| [sqlite](sqlite/) | SQLite file | ✅ Persistent | Desktop apps, CLI tools, embedded |

## Choosing an Implementation

### Use gomap when:

- You need maximum speed (no serialization overhead)
- Data is ephemeral (cache, session data)
- You're writing tests
- Memory usage is acceptable

### Use sqlite when:

- Data must survive restarts
- You need ACID transactions
- Building desktop or CLI applications
- Single-file deployment is important

## Interface Compatibility

All implementations satisfy `store.Store[T]`:

```go
// Works with any implementation
func processUsers(s store.Store[User]) error {
    users, err := s.List("users")
    // ...
}

// In-memory
memStore := gomap.NewMemStore[User](opts)
processUsers(memStore)

// SQLite
sqlStore, _ := sqlite.New[User](sqlOpts)
processUsers(sqlStore)
```

## Swapping Implementations

A common pattern is using gomap for tests and sqlite for production:

```go
func NewStore(cfg Config) (store.Store[User], error) {
    if cfg.Testing {
        return gomap.NewMemStore[User](store.StoreOptions[User]{}), nil
    }
    return sqlite.New[User](sqlite.Options{
        DSN:   cfg.DatabasePath,
        Codec: &codec.JSON{},
    })
}
```

## Feature Comparison

| Feature | gomap | sqlite |
|---------|-------|--------|
| Thread-safe | ✅ | ✅ |
| Watch/Subscribe | ✅ | ✅ (in-process) |
| Validation hooks | ✅ | ❌ |
| Compare function | ✅ | ❌ (byte comparison) |
| Persistence | ❌ | ✅ |
| Version tracking | ❌ | ✅ |
| Transactions | ❌ | ✅ |
| Cross-process watch | ❌ | ❌ |

## Coming Soon

Future implementations may include:
- Redis (distributed caching)
- PostgreSQL (production databases)
- etcd (distributed configuration)


