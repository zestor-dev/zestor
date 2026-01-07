---
title: "Codecs"
linkTitle: "Codecs"
weight: 4
description: >
  Understanding serialization codecs for persistent stores.
---

## What is a Codec?

A **codec** (coder/decoder) handles serialization and deserialization of your data types. Persistent stores like SQLite need codecs to convert Go structs to bytes for storage and back again when reading.

```go
type Codec interface {
    Marshal(v any) ([]byte, error)
    Unmarshal(data []byte, v any) error
}
```

## Why Codecs Matter

In-memory stores like `gomap` store your Go values directly — no serialization needed. But persistent stores must convert values to bytes:

```
┌──────────────┐      Marshal       ┌──────────────┐
│  Go Struct   │  ───────────────►  │    []byte    │  ───► Storage
│  {Name:"X"}  │                    │  [123,34...] │
└──────────────┘                    └──────────────┘

┌──────────────┐      Unmarshal     ┌──────────────┐
│  Go Struct   │  ◄───────────────  │    []byte    │  ◄─── Storage
│  {Name:"X"}  │                    │  [123,34...] │
└──────────────┘                    └──────────────┘
```

## Available Codecs

### JSON

The most common choice. Human-readable, widely supported.

```go
import "github.com/zestor-dev/zestor/codec"

s, _ := sqlite.New[User](sqlite.Options{
    DSN:   "file:app.db",
    Codec: &codec.JSON{},
})
```

**Pros:**
- Human-readable data
- Easy debugging (can inspect DB with SQL tools)
- Wide language support
- No schema required

**Cons:**
- Larger storage size
- Slower than binary formats

**Best for:** Most applications, development, debugging.

---

### Protocol Buffers

High-performance binary format with schema support.

```go
import "github.com/zestor-dev/zestor/codec"

s, _ := sqlite.New[*pb.User](sqlite.Options{
    DSN:   "file:app.db",
    Codec: &codec.Protobuf{},
})
```

**Requirements:**
- Values must implement `proto.Message`
- Requires `.proto` schema files

**Pros:**
- Compact binary format
- Very fast serialization
- Strong typing via schema
- Cross-language support

**Cons:**
- Not human-readable
- Requires proto compilation step
- More setup overhead

**Best for:** High-performance applications, microservices, cross-language systems.

---

### YAML

Human-friendly format, great for configuration.

```go
import "github.com/zestor-dev/zestor/codec"

s, _ := sqlite.New[Config](sqlite.Options{
    DSN:   "file:config.db",
    Codec: &codec.YAML{},
})
```

**Pros:**
- Very human-readable
- Supports comments (in source, not stored)
- Good for complex nested structures

**Cons:**
- Larger than JSON
- Slower parsing
- Whitespace-sensitive

**Best for:** Configuration storage, human-editable data.

---

## Choosing a Codec

| Criteria | JSON | Protobuf | YAML |
|----------|------|----------|------|
| **Readability** | ✅ Good | ❌ Binary | ✅ Best |
| **Performance** | Good | ✅ Best | Slower |
| **Size** | Medium | ✅ Smallest | Largest |
| **Schema** | ❌ No | ✅ Yes | ❌ No |
| **Setup** | ✅ None | Requires proto | ✅ None |

**Quick Decision Guide:**

- **Default choice** → JSON
- **Need maximum performance** → Protobuf
- **Human-editable configs** → YAML
- **Debugging/development** → JSON

## Custom Codecs

Implement the `Codec` interface for custom serialization:

```go
import "github.com/vmihailenco/msgpack/v5"

type MsgpackCodec struct{}

func (c *MsgpackCodec) Marshal(v any) ([]byte, error) {
    return msgpack.Marshal(v)
}

func (c *MsgpackCodec) Unmarshal(data []byte, v any) error {
    return msgpack.Unmarshal(data, v)
}

// Use it
s, _ := sqlite.New[User](sqlite.Options{
    DSN:   "file:app.db",
    Codec: &MsgpackCodec{},
})
```

Popular alternatives you might implement:
- **MessagePack** — Compact binary JSON
- **CBOR** — Concise Binary Object Representation
- **Gob** — Go's native binary format

## Codec Consistency

{{% alert title="Important" color="warning" %}}
Once you choose a codec for a database, **stick with it**. Changing codecs on existing data will cause unmarshal errors.
{{% /alert %}}

If you must migrate:
1. Read all data with old codec
2. Write to new database with new codec
3. Switch over

## Struct Tags

Codecs use struct tags to control serialization:

```go
type User struct {
    ID        int    `json:"id" yaml:"id"`
    Email     string `json:"email" yaml:"email"`
    Password  string `json:"-" yaml:"-"`           // Excluded
    CreatedAt time.Time `json:"created_at,omitempty"`
}
```

- `json:"-"` — Exclude from JSON
- `json:",omitempty"` — Omit if zero value
- `json:"fieldName"` — Custom field name

For Protobuf, field mapping is defined in `.proto` files.


