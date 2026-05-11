// Package main demonstrates how to use the split interfaces (Reader, Writer, Watcher)
// to restrict access to only the operations you need.
package main

import (
	"fmt"
	"time"

	"github.com/zestor-dev/zestor/store"
	"github.com/zestor-dev/zestor/store/gomap"
)

// User is our example data type
type User struct {
	Name  string
	Email string
	Role  string
}

func main() {
	// Create a full Store
	s := gomap.NewMemStore[User](store.StoreOptions[User]{})
	defer s.Close()

	// Populate some data
	s.Set("users", "alice", User{Name: "Alice", Email: "alice@example.com", Role: "admin"})
	s.Set("users", "bob", User{Name: "Bob", Email: "bob@example.com", Role: "user"})
	s.Set("users", "charlie", User{Name: "Charlie", Email: "charlie@example.com", Role: "user"})

	// Example 1: Pass as Reader (read-only)
	fmt.Println("=== Reader Example ===")
	printUserReport(s) // s implements Reader

	// Example 2: Pass as Writer (write-only)
	fmt.Println("\n=== Writer Example ===")
	deactivateUser(s, "users", "charlie") // s implements Writer

	// Example 3: Pass as Watcher (watch-only)
	fmt.Println("\n=== Watcher Example ===")
	go watchForNewUsers(s) // s implements Watcher

	// Give the watcher time to set up
	time.Sleep(10 * time.Millisecond)

	// This will trigger the watcher
	s.Set("users", "diana", User{Name: "Diana", Email: "diana@example.com", Role: "user"})

	time.Sleep(100 * time.Millisecond)

	// Example 4: Read-only service
	fmt.Println("\n=== Service with Reader ===")
	svc := NewUserQueryService(s)
	admins := svc.GetAdmins()
	fmt.Printf("Admins: %v\n", admins)
}

// =============================================================================
// Example 1: Function that only needs read access
// =============================================================================

// printUserReport only reads data - accepts store.Reader
func printUserReport(r store.Reader[User]) {
	count, _ := r.Count("users")
	fmt.Printf("Total users: %d\n", count)

	users, _ := r.List("users")
	for key, user := range users {
		fmt.Printf("  - %s: %s (%s)\n", key, user.Name, user.Role)
	}

	// r.Set(...) ← Would be a compile error! Reader has no Set method
}

// =============================================================================
// Example 2: Function that only needs write access
// =============================================================================

// deactivateUser only writes data - accepts store.Writer
func deactivateUser(w store.Writer[User], kind, key string) {
	existed, prev, _ := w.Delete(kind, key)
	if existed {
		fmt.Printf("Deactivated user: %s\n", prev.Name)
	}

	// w.Get(...) ← Would be a compile error! Writer has no Get method
}

// =============================================================================
// Example 3: Function that only needs watch access
// =============================================================================

// watchForNewUsers only watches - accepts store.Watcher
func watchForNewUsers(w store.Watcher[User]) {
	ch, cancel, err := w.Watch("users", store.WithEventTypes[User](store.EventTypeCreate))
	if err != nil {
		fmt.Println("Watch error:", err)
		return
	}
	defer cancel()

	// Only process one event for this example
	select {
	case ev := <-ch:
		fmt.Printf("New user created: %s (%s)\n", ev.Name, ev.Object.Name)
	case <-time.After(1 * time.Second):
		fmt.Println("No new users")
	}

	// w.Get(...) ← Would be a compile error! Watcher has no Get method
	// w.Set(...) ← Would be a compile error! Watcher has no Set method
}

// =============================================================================
// Example 4: Service struct with read-only dependency
// =============================================================================

// UserQueryService only needs read access to the store
type UserQueryService struct {
	reader store.Reader[User]
}

// NewUserQueryService creates a service with read-only store access
func NewUserQueryService(r store.Reader[User]) *UserQueryService {
	return &UserQueryService{reader: r}
}

// GetAdmins returns all admin users
func (s *UserQueryService) GetAdmins() []User {
	users, _ := s.reader.List("users", func(key string, u User) bool {
		return u.Role == "admin"
	})

	result := make([]User, 0, len(users))
	for _, u := range users {
		result = append(result, u)
	}
	return result
}

// GetUserCount returns total user count
func (s *UserQueryService) GetUserCount() int {
	count, _ := s.reader.Count("users")
	return count
}

// =============================================================================
// Example 5: Combining interfaces for specific needs
// =============================================================================

// SyncService needs both read and write, but not watch
type SyncService struct {
	store store.ReadWriter[User] // Reader + Writer combined
}

func NewSyncService(rw store.ReadWriter[User]) *SyncService {
	return &SyncService{store: rw}
}

func (s *SyncService) SyncUser(kind, key string, updated User) error {
	// Can read
	existing, ok, err := s.store.Get(kind, key)
	if err != nil {
		return err
	}

	// Can write
	if !ok || existing.Email != updated.Email {
		_, err = s.store.Set(kind, key, updated)
	}
	return err
}
