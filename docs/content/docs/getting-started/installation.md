---
title: "Installation"
linkTitle: "Installation"
weight: 1
description: >
  How to install Zestor in your Go project.
---

## Requirements

- Go 1.21 or later

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
    "github.com/zestor-dev/zestor/store/gomap"
)
```

- `store` — Contains interfaces, types, and options
- `store/gomap` — The in-memory implementation

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

