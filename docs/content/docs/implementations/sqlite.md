---
title: "SQLite Store"
linkTitle: "sqlite"
weight: 2
description: >
  Persistent store backed by SQLite database.
---

```go
import "github.com/zestor-dev/zestor/store/sqlite"
```

The `sqlite` package provides a persistent SQLite-backed implementation of `store.Store` using [modernc.org/sqlite](https://pkg.go.dev/modernc.org/sqlite) — a pure Go SQLite driver (no CGo required).

## Features

- **Persistent Storage**: Data survives application restarts
- **ACID Transactions**: Full transactional support
- **Single File**: All data in one `.db` file
- **WAL Mode**: Write-Ahead Logging for better concurrency
- **Version Tracking**: Automatic version incrementing
- **No-op Detection**: Byte-level comparison prevents unnecessary updates
- **Pure Go**: No CGo, cross-platform compatible

## Quick Start

```go
import (
    "github.com/zestor-dev/zestor/codec"
    "github.com/zestor-dev/zestor/store/sqlite"
)

type User struct {
    Name  string `json:"name"`
    Email string `json:"email"`
}

func main() {
    s, err := sqlite.New[User](sqlite.Options{
        DSN:   "file:app.db?cache=shared",
        Codec: &codec.JSON{},
    })
    if err != nil {
        log.Fatal(err)
    }
    defer s.Close()

    // Same API as gomap
    s.Set("users", "alice", User{Name: "Alice", Email: "alice@example.com"})
    
    user, ok, _ := s.Get("users", "alice")
    if ok {
        fmt.Println(user.Name) // Alice
    }
}
```

## Configuration

### Options

```go
type Options struct {
    DSN         string        // SQLite connection string (required)
    Codec       codec.Codec   // Serialization codec (required)
    BusyTimeout time.Duration // Lock wait timeout (optional)
    DisableWAL  bool          // Disable WAL mode (optional)
}
```

### DSN Examples

| DSN | Description |
|-----|-------------|
| `file:app.db` | Simple file database |
| `file:app.db?cache=shared` | Shared cache (recommended) |
| `file:app.db?mode=rwc` | Read-write-create |
| `file::memory:?cache=shared` | In-memory (testing) |

### Full Configuration

```go
s, _ := sqlite.New[Config](sqlite.Options{
    DSN:         "file:config.db?cache=shared",
    Codec:       &codec.JSON{},
    BusyTimeout: 5 * time.Second,  // Wait up to 5s for locks
    DisableWAL:  false,            // Keep WAL enabled (default)
})
```

## Database Schema

The store automatically creates this schema on first use:

```sql
CREATE TABLE zestor_kv (
    kind       TEXT NOT NULL,
    key        TEXT NOT NULL,
    value      BLOB NOT NULL,
    version    INTEGER NOT NULL DEFAULT 1,
    updated_at TEXT NOT NULL,
    PRIMARY KEY(kind, key)
);

CREATE INDEX idx_kv_kind ON zestor_kv(kind);
```

## Codecs

SQLite stores require a codec for serialization. See [Codecs](/docs/concepts/codec/) for details.

```go
// JSON (recommended for most cases)
Codec: &codec.JSON{}

// Protobuf (for performance)
Codec: &codec.Protobuf{}

// YAML (for human-readable storage)
Codec: &codec.YAML{}
```

## Watch & Subscribe

Watch works via in-process pub/sub:

```go
ch, cancel, _ := s.Watch("users",
    store.WithInitialReplay[User](),
    store.WithEventTypes[User](store.EventTypeCreate),
)
defer cancel()

for event := range ch {
    fmt.Printf("New user: %s\n", event.Object.Name)
}
```

{{% alert title="Note" color="info" %}}
Watch events are only delivered within the same process. Changes made by other processes or direct SQL won't trigger events.
{{% /alert %}}

## WAL Mode

Write-Ahead Logging is enabled by default:

- Readers don't block writers
- Writers don't block readers  
- Better concurrent performance
- Slightly more disk space (`.db-wal`, `.db-shm` files)

Disable only if you have specific requirements:

```go
DisableWAL: true  // Not recommended
```

## Version Tracking

Each record has an auto-incrementing version:

```go
// First write: version = 1
s.Set("config", "app", Config{Debug: false})

// Update: version = 2
s.Set("config", "app", Config{Debug: true})

// No-op (same bytes): version stays 2
s.Set("config", "app", Config{Debug: true})
```

## Performance Characteristics

| Operation | Notes |
|-----------|-------|
| Get | Fast (indexed lookup) |
| Set | Good (single row upsert) |
| List | Good (indexed by kind) |
| SetAll | Batched in transaction |
| Watch | In-memory pub/sub |

### Optimizing Performance

1. **Use shared cache**: `?cache=shared`
2. **Keep WAL enabled**: Default setting
3. **Set busy timeout**: Prevents lock errors
4. **Batch writes**: Use `SetAll` for bulk operations

## Limitations

| Limitation | Impact |
|------------|--------|
| Single writer | Only one write at a time (WAL helps) |
| In-process watch | No cross-process notifications |
| File-based | Can't share across network easily |
| No validation hooks | Unlike gomap, no per-kind validation |

## Use Cases

**Good for:**
- Desktop applications
- CLI tools  
- Development/testing with persistence
- Configuration storage
- Single-user applications
- Embedded systems
- Local caching with durability

**Not ideal for:**
- High-write workloads
- Multi-process shared access needing watch
- Distributed systems
- Web applications with many concurrent users

## Troubleshooting

### Database Locked

Increase busy timeout:

```go
BusyTimeout: 10 * time.Second
```

### Slow Writes

Ensure WAL is enabled and batch operations:

```go
// Instead of multiple Sets
s.SetAll("items", map[string]Item{
    "a": itemA,
    "b": itemB,
    "c": itemC,
})
```

### Watch Not Receiving Events

Events are in-process only. If another process modifies the database, you won't see events.

## Complete Example

```go
package main

import (
    "fmt"
    "log"
    "time"

    "github.com/zestor-dev/zestor/codec"
    "github.com/zestor-dev/zestor/store"
    "github.com/zestor-dev/zestor/store/sqlite"
)

type Note struct {
    Title   string    `json:"title"`
    Content string    `json:"content"`
    Updated time.Time `json:"updated"`
}

func main() {
    s, err := sqlite.New[Note](sqlite.Options{
        DSN:         "file:notes.db?cache=shared",
        Codec:       &codec.JSON{},
        BusyTimeout: 5 * time.Second,
    })
    if err != nil {
        log.Fatal(err)
    }
    defer s.Close()

    // Watch for changes
    ch, cancel, _ := s.Watch("notes", store.WithInitialReplay[Note]())
    defer cancel()

    go func() {
        for ev := range ch {
            fmt.Printf("[%s] %s\n", ev.EventType, ev.Object.Title)
        }
    }()

    // Create notes
    s.Set("notes", "note-1", Note{
        Title:   "Meeting Notes",
        Content: "Discussed Q4 planning...",
        Updated: time.Now(),
    })

    s.Set("notes", "note-2", Note{
        Title:   "Ideas",
        Content: "New feature brainstorm...",
        Updated: time.Now(),
    })

    // List all notes
    notes, _ := s.List("notes")
    fmt.Printf("\nTotal notes: %d\n", len(notes))

    // Data persists! Restart the app and notes are still there.
    time.Sleep(100 * time.Millisecond)
}
```


