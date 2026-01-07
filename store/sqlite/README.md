# SQLite Store for Zestor

An SQLite-backed implementation of the Zestor `Store[T]` interface. SQLite is a serverless, self-contained SQL database engine - perfect for embedded applications and development.

## Features

- **Embedded Database**: No separate server required
- **ACID Transactions**: Full transactional support
- **Single File**: All data in one .db file
- **WAL Mode**: Write-Ahead Logging for better concurrency
- **No-op Detection**: Byte-level comparison prevents unnecessary updates
- **Version Tracking**: Automatic version incrementing
- **Cross-Platform**: Works on Linux, macOS, Windows
- **Pure Go**: Uses modernc.org/sqlite (no CGo required)

## Installation

```bash
go get github.com/zestor-dev/zestor/store/sqlite
```

## Usage

```go
s, err := sqlite.New[MyData](sqlite.Options{
    DSN:         "file:zestor.db?cache=shared",
    Codec:       &codec.JSON{},
    BusyTimeout: 5 * time.Second,    // optional
    DisableWAL:  false,              // optional, default false
})
defer s.Close()
```

## Database Schema

```sql
CREATE TABLE zestor_kv (
    kind       TEXT NOT NULL,
    key        TEXT NOT NULL,
    value      BLOB NOT NULL,
    version    INTEGER NOT NULL DEFAULT 1,
    updated_at TEXT NOT NULL DEFAULT (STRFTIME('%Y-%m-%dT%H:%M:%fZ','now')),
    PRIMARY KEY(kind, key)
);

CREATE INDEX idx_kv_kind ON zestor_kv(kind);
```

## Options

```go
type Options struct {
    DSN         string        // SQLite DSN (required)
    Codec       codec.Codec   // Marshaling codec (required)
    BusyTimeout time.Duration // PRAGMA busy_timeout (optional)
    DisableWAL  bool          // Disable WAL mode (optional)
}
```

### DSN Examples

```
file:zestor.db                           # Simple file
file:zestor.db?cache=shared             # Shared cache
file:zestor.db?mode=rwc                  # Read-write-create
file::memory:?cache=shared               # In-memory shared
```

## Features

### WAL Mode (Write-Ahead Logging)

Enabled by default for better concurrency:
- Readers don't block writers
- Writers don't block readers
- Better performance for concurrent access

### Busy Timeout

Configure how long to wait when database is locked:
```go
BusyTimeout: 5 * time.Second  // Wait up to 5s for lock
```

## Advantages

- No server setup required
- Single file database
- ACID transactions
- SQL queries
- Very stable and reliable
- Cross-platform
- Great for development
- Pure Go (no CGo)

## Limitations

- Single writer (but WAL helps)
- Not for high-write workloads
- File-based (network access requires NFS/SMB)
- Watch: In-process pub/sub only

## Use Cases

Good for:
- Desktop applications
- Mobile apps
- Development and testing
- Single-user applications
- Configuration storage
- Embedded systems
- CLI tools
- Local caching

## Testing

No setup required - just run tests:

```bash
go test -v
go test -bench=. -benchmem
```

## Example

```go
package main

import (
    "github.com/zestor-dev/zestor/codec"
    "github.com/zestor-dev/zestor/store/sqlite"
)

type Config struct {
    Name  string `json:"name"`
    Value string `json:"value"`
}

func main() {
    s, _ := sqlite.New[Config](sqlite.Options{
        DSN:   "file:config.db",
        Codec: &codec.JSON{},
    })
    defer s.Close()

    // Write
    s.Set("app", "version", Config{Name: "version", Value: "1.0.0"})

    // Read
    cfg, ok, _ := s.Get("app", "version")
    if ok {
        println(cfg.Value) // 1.0.0
    }
}
```

## License

Same as parent Zestor project.

