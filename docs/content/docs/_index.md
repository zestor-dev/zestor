---
title: "Documentation"
linkTitle: "Documentation"
weight: 1
menu:
  main:
    weight: 20
---

Welcome to the Zestor documentation!

Zestor is a generic, type-safe, in-memory key-value store for Go with watch/subscribe capabilities. It's designed to be simple to use while providing powerful features for building reactive applications.

## Key Features

- **Generic** — Works with any type `T` using Go generics
- **Multi-kind** — Organize data by "kind" (like tables/collections)
- **Thread-safe** — Concurrent read/write with `sync.RWMutex`
- **Watch/Subscribe** — Real-time notifications for create, update, and delete events
- **Validation** — Per-kind validation functions
- **Change detection** — Configurable compare function to suppress duplicate events
- **Interface Segregation** — Split interfaces (`Reader`, `Writer`, `Watcher`) for better access control

## Where to Start

- **[Getting Started](/docs/getting-started/)** — Installation and your first Zestor application
- **[Concepts](/docs/concepts/)** — Understand the core concepts and architecture
- **[API Reference](/docs/api/)** — Detailed API documentation

## Requirements

- Go 1.21 or later

## Installation

```bash
go get github.com/zestor-dev/zestor
```

