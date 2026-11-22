package postgres

import (
	"testing"
	"time"
)

// A drain or Watch of one kind must not block another kind's drain lock.
func TestKindCursorsAreIndependent(t *testing.T) {
	s := &storePG[int]{cursors: map[string]*kindCursor{}}
	a := s.cursor("a")
	a.mu.Lock()
	done := make(chan struct{})
	go func() {
		b := s.cursor("b")
		b.mu.Lock()
		b.lastID = 1
		b.mu.Unlock()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("locking kind a blocked kind b")
	}
	a.mu.Unlock()
}
