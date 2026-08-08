// Package main demonstrates how to use the split interfaces (Reader, Writer, Watcher)
// to restrict access to only the operations you need.
package main

import (
	"context"
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
	ctx := context.Background()

	// Create a full Store
	s := gomap.NewMemStore[User](store.StoreOptions[User]{})
	defer s.Close()

	// Populate some data
	s.Set(ctx, "users", "alice", User{Name: "Alice", Email: "alice@example.com", Role: "admin"})
	s.Set(ctx, "users", "bob", User{Name: "Bob", Email: "bob@example.com", Role: "user"})
	s.Set(ctx, "users", "charlie", User{Name: "Charlie", Email: "charlie@example.com", Role: "user"})

	// Example 1: Pass as Reader (read-only)
	fmt.Println("=== Reader Example ===")
	printUserReport(ctx, s) // s implements Reader

	// Example 2: Pass as Writer (write-only)
	fmt.Println("\n=== Writer Example ===")
	deactivateUser(ctx, s, "users", "charlie") // s implements Writer

	// Example 3: Pass as Watcher (watch-only)
	fmt.Println("\n=== Watcher Example ===")
	// The watch lives until this context is cancelled — that is the only way to
	// unsubscribe.
	watchCtx, stopWatching := context.WithCancel(ctx)
	defer stopWatching()

	watching := make(chan struct{})
	go watchForNewUsers(watchCtx, s, watching)
	<-watching // the watcher is subscribed; events from here on are guaranteed

	// This will trigger the watcher
	s.Set(ctx, "users", "diana", User{Name: "Diana", Email: "diana@example.com", Role: "user"})

	time.Sleep(100 * time.Millisecond)

	// Example 4: Read-only service
	fmt.Println("\n=== Service with Reader ===")
	svc := NewUserQueryService(s)
	admins := svc.GetAdmins(ctx)
	fmt.Printf("Admins: %v\n", admins)
}

// =============================================================================
// Example 1: Function that only needs read access
// =============================================================================

// printUserReport only reads data - accepts store.Reader
func printUserReport(ctx context.Context, r store.Reader[User]) {
	count, _ := r.Count(ctx, "users")
	fmt.Printf("Total users: %d\n", count)

	users, _ := r.List(ctx, "users")
	for key, user := range users {
		fmt.Printf("  - %s: %s (%s)\n", key, user.Name, user.Role)
	}

	// r.Set(...) ← Would be a compile error! Reader has no Set method
}

// =============================================================================
// Example 2: Function that only needs write access
// =============================================================================

// deactivateUser only writes data - accepts store.Writer
func deactivateUser(ctx context.Context, w store.Writer[User], kind, key string) {
	existed, prev, _ := w.Delete(ctx, kind, key)
	if existed {
		fmt.Printf("Deactivated user: %s\n", prev.Name)
	}

	// w.Get(...) ← Would be a compile error! Writer has no Get method
}

// =============================================================================
// Example 3: Function that only needs watch access
// =============================================================================

// watchForNewUsers only watches - accepts store.Watcher. It closes ready once
// the subscription exists, so the caller knows subsequent writes will be seen.
func watchForNewUsers(ctx context.Context, w store.Watcher[User], ready chan<- struct{}) {
	ch, err := w.Watch(ctx, "users", store.WithEventTypes[User](store.EventTypeCreate))
	if err != nil {
		fmt.Println("Watch error:", err)
		close(ready)
		return
	}
	close(ready)

	// One event is enough for this example; a real consumer would range over ch.
	ev, ok := <-ch
	if !ok {
		fmt.Println("Watch ended: channel closed")
		return
	}
	if ev.Err != nil {
		// A watcher that falls behind is told so rather than silently losing
		// events: the channel closes right after this event.
		fmt.Println("Watch ended:", ev.Err, "- re-list and re-watch to resync")
		return
	}
	fmt.Printf("New user created: %s (%s)\n", ev.Name, ev.Object.Name)

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
func (s *UserQueryService) GetAdmins(ctx context.Context) []User {
	users, _ := s.reader.List(ctx, "users", func(key string, u User) bool {
		return u.Role == "admin"
	})

	result := make([]User, 0, len(users))
	for _, u := range users {
		result = append(result, u)
	}
	return result
}

// GetUserCount returns total user count
func (s *UserQueryService) GetUserCount(ctx context.Context) int {
	count, _ := s.reader.Count(ctx, "users")
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

func (s *SyncService) SyncUser(ctx context.Context, kind, key string, updated User) error {
	// Can read
	existing, ok, err := s.store.Get(ctx, kind, key)
	if err != nil {
		return err
	}

	// Can write
	if !ok || existing.Email != updated.Email {
		_, err = s.store.Set(ctx, kind, key, updated)
	}
	return err
}
