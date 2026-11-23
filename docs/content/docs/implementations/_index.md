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
| [postgres](postgres/) | PostgreSQL | ✅ Persistent | Servers, multi-instance apps, shared watch |
| [remote](remote/) | Any of the above, in another process | Same as the served store | Sharing one store across services and languages |

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

### Use postgres when:

- A PostgreSQL server is already part of your stack
- Multiple processes or replicas should share state and receive `Watch` notifications
- You want multi-tenant isolation via namespaces (`ns_id`)

### Use remote when:

- The store lives in another service, or callers are not written in Go
- Callers should not have database credentials or drivers
- You want to `curl` the store while debugging

## Interface Compatibility

All implementations satisfy `store.Store[T]`:

```go
// Works with any implementation
func processUsers(s store.Store[User]) error {
    users, err := s.List(ctx, "users")
    // ...
}

// In-memory
memStore := gomap.NewMemStore[User](opts)
processUsers(memStore)

// SQLite
sqlStore, _ := sqlite.New[User](ctx, sqlOpts)
processUsers(sqlStore)

// PostgreSQL
pgStore, _ := postgres.New[User](ctx, pgOpts)
processUsers(pgStore)
```

## Swapping Implementations

A common pattern is using gomap for tests and sqlite or postgres for production:

```go
func NewStore(cfg Config) (store.Store[User], error) {
    if cfg.Testing {
        return gomap.NewMemStore[User](store.StoreOptions[User]{}), nil
    }
    if cfg.DatabaseURL != "" {
        return postgres.New[User](ctx, postgres.Options{
            ConnString: cfg.DatabaseURL,
            Codec:      &codec.JSON{},
        })
    }
    return sqlite.New[User](ctx, sqlite.Options{
        DSN:   cfg.DatabasePath,
        Codec: &codec.JSON{},
    })
}
```

## Feature Comparison

| Feature | gomap | sqlite | postgres |
|---------|-------|--------|----------|
| Thread-safe | ✅ | ✅ | ✅ |
| Watch/Subscribe | ✅ | ✅ (in-process) | ✅ (cross-process via NOTIFY + outbox) |
| Validation hooks | ✅ | ✅ (`NewWithStoreOptions`) | ✅ (`NewWithStoreOptions`) |
| Compare function | ✅ | ✅ (`NewWithStoreOptions`; default: byte comparison) | ✅ (`NewWithStoreOptions`; default: byte comparison) |
| Persistence | ❌ | ✅ | ✅ |
| Version tracking | ❌ | ✅ | ✅ |
| Transactions | ❌ | ✅ | ✅ |
| Cross-process watch | ❌ | ❌ | ✅ |

## Coming Soon

Future implementations may include:

- Redis (distributed caching)
- etcd (distributed configuration)

