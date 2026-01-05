---
title: "Interface Segregation"
linkTitle: "Interface Segregation"
weight: 3
description: >
  Using Reader, Writer, and Watcher interfaces for better access control.
---

## Overview

Zestor follows the **Interface Segregation Principle** by splitting its functionality into focused interfaces. This allows you to pass only the access level your code needs.

## Available Interfaces

```go
// Reader provides read-only access
type Reader[T any] interface {
    Get(kind, key string) (val T, ok bool, err error)
    List(kind string, filter ...FilterFunc[T]) (map[string]T, error)
    Count(kind string) (int, error)
    Keys(kind string) ([]string, error)
    Values(kind string) ([]KeyValue[T], error)
    GetAll() (map[string]map[string]T, error)
}

// Writer provides write access
type Writer[T any] interface {
    Set(kind, key string, value T) (created bool, err error)
    SetFn(kind, key string, fn func(v T) (T, error)) (changed bool, err error)
    SetAll(kind string, values map[string]T) error
    Delete(kind, key string) (existed bool, prev T, err error)
}

// Watcher provides watch access
type Watcher[T any] interface {
    Watch(kind string, opts ...WatchOption[T]) (r <-chan *Event[T], cancel func(), err error)
}

// ReadWriter combines Reader and Writer
type ReadWriter[T any] interface {
    Reader[T]
    Writer[T]
}

// Store is the full interface
type Store[T any] interface {
    Reader[T]
    Writer[T]
    Watcher[T]
    Close() error
    Dump() string
}
```

## Why Interface Segregation?

### 1. Principle of Least Privilege

Pass only the access your code needs:

```go
// This function can't accidentally modify data
func generateReport(r store.Reader[User]) Report {
    users, _ := r.List("users")
    // r.Set(...) ← Compile error! Reader has no Set
    return buildReport(users)
}
```

### 2. Clearer Function Signatures

The interface type documents what the function does:

```go
// Obviously read-only
func countActiveUsers(r store.Reader[User]) int

// Obviously writes data
func importUsers(w store.Writer[User], users []User) error

// Obviously watches for changes
func streamEvents(w store.Watcher[User], out chan Event)
```

### 3. Easier Testing

Smaller interfaces are easier to mock:

```go
type mockReader struct {
    users map[string]User
}

func (m *mockReader) Get(kind, key string) (User, bool, error) {
    u, ok := m.users[key]
    return u, ok, nil
}

func (m *mockReader) List(kind string, filters ...store.FilterFunc[User]) (map[string]User, error) {
    return m.users, nil
}

// ... only need to implement Reader methods
```

## Usage Examples

### Read-Only Service

```go
type ReportService struct {
    store store.Reader[User]
}

func NewReportService(r store.Reader[User]) *ReportService {
    return &ReportService{store: r}
}

func (s *ReportService) GetUserCount() int {
    count, _ := s.store.Count("users")
    return count
}

func (s *ReportService) GetAdmins() []User {
    users, _ := s.store.List("users", func(k string, u User) bool {
        return u.Role == "admin"
    })
    // Convert to slice...
    return result
}
```

### Write-Only Importer

```go
type UserImporter struct {
    store store.Writer[User]
}

func NewUserImporter(w store.Writer[User]) *UserImporter {
    return &UserImporter{store: w}
}

func (i *UserImporter) Import(users map[string]User) error {
    return i.store.SetAll("users", users)
}

func (i *UserImporter) Delete(key string) error {
    _, _, err := i.store.Delete("users", key)
    return err
}
```

### Watch-Only Event Processor

```go
type EventProcessor struct {
    store store.Watcher[User]
}

func NewEventProcessor(w store.Watcher[User]) *EventProcessor {
    return &EventProcessor{store: w}
}

func (p *EventProcessor) ProcessEvents(ctx context.Context) error {
    ch, cancel, err := p.store.Watch("users")
    if err != nil {
        return err
    }
    defer cancel()

    for {
        select {
        case event, ok := <-ch:
            if !ok {
                return nil
            }
            p.handleEvent(event)
        case <-ctx.Done():
            return ctx.Err()
        }
    }
}
```

### Read-Write Without Watch

```go
type SyncService struct {
    store store.ReadWriter[User]
}

func NewSyncService(rw store.ReadWriter[User]) *SyncService {
    return &SyncService{store: rw}
}

func (s *SyncService) Upsert(key string, user User) error {
    existing, ok, _ := s.store.Get("users", key)
    if ok && existing.Email == user.Email {
        return nil // No change needed
    }
    _, err := s.store.Set("users", key, user)
    return err
}
```

## Passing Store as Different Interfaces

The `memStore` implementation satisfies all interfaces, so you can pass it wherever needed:

```go
func main() {
    // Create full Store
    s := gomap.NewMemStore[User](store.StoreOptions[User]{})
    defer s.Close()
    
    // Pass as Reader
    reportSvc := NewReportService(s)
    
    // Pass as Writer
    importer := NewUserImporter(s)
    
    // Pass as Watcher
    processor := NewEventProcessor(s)
    
    // Pass as ReadWriter
    syncSvc := NewSyncService(s)
    
    // All use the same underlying store instance
}
```

## Best Practices

1. **Use the narrowest interface possible** — If you only read, accept `Reader`

2. **Document access patterns** — The interface type serves as documentation

3. **Consider splitting large functions** — If a function needs both read and write, consider if it can be split

4. **Use `ReadWriter` for CRUD** — When you need read and write but not watch

5. **Accept interfaces, return concrete types** — Functions should accept interfaces but constructors can return the full `Store`

