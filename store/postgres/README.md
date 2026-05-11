# PostgreSQL store

`store/postgres` implements `store.Store[T]` on PostgreSQL using [pgx](https://github.com/jackc/pgx) (`pgxpool`), byte-encoded values, and an **outbox + `LISTEN`/`NOTIFY`** pipeline so `Watch` can deliver ordered events across processes.

## Install

```bash
go get github.com/zestor-dev/zestor/store/postgres
```

## Usage

```go
import (
    "time"

    "github.com/zestor-dev/zestor/codec"
    "github.com/zestor-dev/zestor/store/postgres"
)

s, err := postgres.New[MyData](postgres.Options{
    ConnString: "postgresql://user:pass@localhost:5432/dbname?sslmode=disable",
    Codec:      &codec.JSON{},
    Namespace:  0,                    // optional multi-tenant column (default 0)
    Timeout:    10 * time.Second,     // optional (default 10s)
})
if err != nil {
    return err
}
defer s.Close()
```

## Behaviour notes

- **Schema**: On `New`, creates `zestor_kv`, `zestor_outbox`, and a trigger that `NOTIFY`s channel `zestor_events` after each outbox insert.
- **Watch**: Mutations append to the outbox; the dedicated listener drains rows for the notified kind and publishes to local subscribers (same semantics as other stores for `WatchOption`: buffer size, initial replay, event-type filter).
- **Multi-tenant**: Use distinct `Namespace` values for isolated keyspaces over the same physical tables.
- **Tests**: Set `POSTGRES_CONN` to a reachable DSN; if the database is unavailable, tests skip.

## Operational note

The outbox grows over time; prune old rows in production (retention job or periodic `DELETE` by age) so the table stays bounded.
