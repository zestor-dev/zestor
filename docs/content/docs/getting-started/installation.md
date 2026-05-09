---
title: "Installation"
linkTitle: "Installation"
weight: 1
description: >
  How to install Zestor in your Go project.
---

## Requirements

- Go 1.25 or later

## Install via Go Modules

Add Zestor to your project:

```bash
go get github.com/zestor-dev/zestor
```

## Import

Import the packages you need:

```go
import (
    "github.com/zestor-dev/zestor/store"
    "github.com/zestor-dev/zestor/store/gomap"  // In-memory store
)
```

For persistent storage with SQLite:

```go
import (
    "github.com/zestor-dev/zestor/store"
    "github.com/zestor-dev/zestor/store/sqlite"  // SQLite store
    "github.com/zestor-dev/zestor/codec"         // Serialization codecs
)
```

For PostgreSQL (server-backed persistence and cross-process watch):

```go
import (
    "github.com/zestor-dev/zestor/store"
    "github.com/zestor-dev/zestor/store/postgres"
    "github.com/zestor-dev/zestor/codec"
)
```

### Package Overview

| Package | Description |
|---------|-------------|
| `store` | Core interfaces, types, and options |
| `store/gomap` | In-memory implementation |
| `store/sqlite` | SQLite-backed persistent implementation |
| `store/postgres` | PostgreSQL-backed implementation |
| `codec` | Serialization codecs (JSON, Protobuf, YAML) |

## Verify Installation

Create a simple test file:

```go
package main

import (
    "fmt"
    "github.com/zestor-dev/zestor/store"
    "github.com/zestor-dev/zestor/store/gomap"
)

func main() {
    s := gomap.NewMemStore[string](store.StoreOptions[string]{})
    defer s.Close()
    
    s.Set("test", "hello", "world")
    val, ok, _ := s.Get("test", "hello")
    
    if ok {
        fmt.Println("Zestor is working:", val)
    }
}
```

Run it:

```bash
go run main.go
# Output: Zestor is working: world
```

You're ready to go! Continue to the [Quick Start](/docs/getting-started/quickstart/) guide.

