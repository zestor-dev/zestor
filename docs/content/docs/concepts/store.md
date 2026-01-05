---
title: "Store & Kinds"
linkTitle: "Store & Kinds"
weight: 1
description: >
  Understanding the Store interface and how data is organized by kinds.
---

## The Store Interface

Zestor's core is the `Store` interface, which provides a complete set of operations for managing in-memory data:

```go
type Store[T any] interface {
    Reader[T]
    Writer[T]
    Watcher[T]
    Close() error
    Dump() string
}
```

The interface is **generic** — you specify your data type `T` when creating the store, and all operations are type-safe.

## Kinds: Organizing Your Data

Data in Zestor is organized by **kinds**. Think of kinds like tables in a database or collections in MongoDB.

```go
// Store users and products in the same store instance
s := gomap.NewMemStore[any](store.StoreOptions[any]{})

// "users" kind
s.Set("users", "alice", User{Name: "Alice"})
s.Set("users", "bob", User{Name: "Bob"})

// "products" kind  
s.Set("products", "laptop", Product{Name: "Laptop", Price: 999})
s.Set("products", "phone", Product{Name: "Phone", Price: 699})

// Query by kind
users, _ := s.List("users")     // Only users
products, _ := s.List("products") // Only products
```

### When to Use Multiple Kinds

Use multiple kinds when you have:
- Different logical groupings of data
- Data that needs different validation rules
- Data you want to watch independently

### Single Type vs Multiple Types

**Option 1: Single type per store (recommended)**
```go
// Separate stores for different types
userStore := gomap.NewMemStore[User](opts)
productStore := gomap.NewMemStore[Product](opts)
```

**Option 2: Interface type for mixed data**
```go
// Single store with interface{} or any
store := gomap.NewMemStore[any](opts)
store.Set("users", "alice", User{Name: "Alice"})
store.Set("config", "timeout", 30)
```

## Keys

Within each kind, data is stored by **string keys**. Keys must be unique within a kind.

```go
// Key "alice" in kind "users"
s.Set("users", "alice", User{Name: "Alice"})

// Same key "alice" in kind "admins" — no conflict
s.Set("admins", "alice", User{Name: "Alice Admin"})
```

## Store Options

Configure the store when creating it:

```go
s := gomap.NewMemStore[User](store.StoreOptions[User]{
    // Custom comparison function
    CompareFn: func(prev, new User) bool {
        return prev.Email == new.Email
    },
    
    // Per-kind validation
    ValidateFns: map[string]store.ValidateFunc[User]{
        "users": func(u User) error {
            if u.Email == "" {
                return errors.New("email required")
            }
            return nil
        },
    },
})
```

### CompareFn

The compare function determines if two values are "equal". When a value is set and the compare function returns `true`, no update event is emitted.

Default: `reflect.DeepEqual`

```go
// Only consider email changes as "real" updates
CompareFn: func(prev, new User) bool {
    return prev.Email == new.Email
}
```

### ValidateFns

Per-kind validation functions run before any write operation. If validation fails, the write is rejected.

```go
ValidateFns: map[string]store.ValidateFunc[User]{
    "users": func(u User) error {
        if u.Email == "" {
            return errors.New("email required")
        }
        if !strings.Contains(u.Email, "@") {
            return errors.New("invalid email format")
        }
        return nil
    },
    "admins": func(u User) error {
        if u.Role != "admin" {
            return errors.New("must have admin role")
        }
        return nil
    },
}
```

## Thread Safety

Zestor is fully thread-safe. You can read and write from multiple goroutines without external synchronization:

```go
s := gomap.NewMemStore[int](store.StoreOptions[int]{})

var wg sync.WaitGroup
for i := 0; i < 100; i++ {
    wg.Add(1)
    go func(n int) {
        defer wg.Done()
        s.Set("counters", fmt.Sprintf("counter-%d", n), n)
    }(i)
}
wg.Wait()

count, _ := s.Count("counters")
fmt.Println(count) // 100
```

## Closing the Store

Always close the store when done to clean up watchers:

```go
s := gomap.NewMemStore[User](opts)
defer s.Close()

// ... use the store ...
```

After closing:
- All watcher channels are closed
- Further operations return `store.ErrClosed`

