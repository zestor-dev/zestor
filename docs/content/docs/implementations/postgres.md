---
title: "PostgreSQL Store"
linkTitle: "postgres"
weight: 3
description: >
  Persistent store backed by PostgreSQL with cross-process watch.
---

```go
import "github.com/zestor-dev/zestor/store/postgres"
```

The `postgres` package provides a PostgreSQL-backed implementation of `store.Store` using [jackc/pgx/v5](https://pkg.go.dev/github.com/jackc/pgx/v5) with connection pooling. Writes go to a KV table and an **outbox**; a trigger sends `NOTIFY` so a background listener can drain the outbox and fan out `Watch` events—so watchers on different application instances can observe the same kinds.

## Features

- **Persistent storage** with ACID transactions
- **Cross-process watch** via `LISTEN` / `NOTIFY` and an outbox (ordered by outbox id)
- **Namespace / tenant id** (`Namespace` option maps to `ns_id`)
- **Version and `updated_at`** maintained on the KV row
- **No-op `Set`**: skipped when serialized bytes are unchanged (no outbox row)

## Quick start

```go
import (
    "log"
    "time"

    "github.com/zestor-dev/zestor/codec"
    "github.com/zestor-dev/zestor/store"
    "github.com/zestor-dev/zestor/store/postgres"
)

type User struct {
    Name  string `json:"name"`
    Email string `json:"email"`
}

func main() {
    s, err := postgres.New[User](postgres.Options{
        ConnString: "postgresql://user:pass@localhost:5432/mydb?sslmode=disable",
        Codec:      &codec.JSON{},
        Timeout:    10 * time.Second,
    })
    if err != nil {
        log.Fatal(err)
    }
    defer s.Close()

    watchCtx, stopWatching := context.WithCancel(ctx)
    defer stopWatching()

    ch, err := s.Watch(watchCtx, "users", store.WithInitialReplay[User]())
    if err != nil {
        log.Fatal(err)
    }
    defer stopWatching()

    go func() {
        for ev := range ch {
            log.Printf("%s %s\n", ev.EventType, ev.Name)
        }
    }()

    _, _ = s.Set(ctx, "users", "alice", User{Name: "Alice", Email: "alice@example.com"})
}
```

## Configuration

### Options

```go
type Options struct {
    ConnString string        // PostgreSQL URL (required), e.g. postgres://...
    Codec      codec.Codec   // Required
    Namespace  int64         // Optional; default 0 — isolates rows per tenant
    Timeout    time.Duration // Per-operation timeout; default 10s
}
```

### Connection strings

Use any DSN accepted by pgx, for example:

- `postgresql://user:password@host:5432/dbname?sslmode=require`
- `postgresql:///dbname?host=/var/run/postgresql` (Unix socket)

## Database schema

The store creates (if missing):

- **`zestor_kv`**: `(ns_id, kind, key)` primary key, `value BYTEA`, `version`, `updated_at`
- **`zestor_outbox`**: append-only rows with `etype` in `create` / `update` / `delete`
- **Trigger** on `zestor_outbox` → `pg_notify('zestor_events', payload)`

Exact DDL is applied inside `New`; you normally do not manage these tables by hand except for **outbox retention** (delete aged rows in production).

## Watch

`Watch` supports the same options as other implementations:

- `store.WithInitialReplay[T]()`
- `store.WithEventTypes[T](...)`
- `store.WithBufferSize[T](n)`

{{% alert title="Delivery model" color="info" %}}
Events are delivered after the transaction commits and the listener drains the outbox. Latency is usually low but not synchronous with the `Set` return in the same goroutine.
{{% /alert %}}

## Codecs

Same as SQLite: pass any `codec.Codec` ([Codecs](/docs/concepts/codec/) — JSON, Protobuf, YAML, etc.).

## Limitations

| Topic | Notes |
|-------|--------|
| Outbox growth | Plan retention / compaction |
| `SetFn` | Requires existing key (`ErrKeyNotFound` otherwise), like `sqlite` / `gomap` |
| Validation / compare hooks | Not supported (same as `sqlite`) |

## Testing

```bash
export POSTGRES_CONN='postgresql://postgres:postgres@localhost:5432/postgres?sslmode=disable'
go test ./...
```

If PostgreSQL is not reachable, tests skip.

## See also

- [SQLite store](sqlite/) — embedded file, in-process watch only
- [Implementations overview](/docs/implementations/)
