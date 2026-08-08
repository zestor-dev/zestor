package main

import (
	"context"
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
	ctx := context.Background()

	s, err := sqlite.New[Note](ctx, sqlite.Options{
		DSN:         "file:notes.db?cache=shared",
		Codec:       &codec.JSON{},
		BusyTimeout: 5 * time.Second,
	})
	if err != nil {
		log.Fatal(err)
	}
	defer s.Close()

	// Watch until watchCtx is cancelled. WithInitialReplay delivers what is
	// already stored before anything written afterwards, so the loop below sees
	// a consistent picture without a separate List.
	watchCtx, stopWatching := context.WithCancel(ctx)
	defer stopWatching()

	fmt.Println("- Watching for changes...")
	ch, err := s.Watch(watchCtx, "notes", store.WithInitialReplay[Note]())
	if err != nil {
		log.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range ch {
			if ev.Err != nil {
				// The consumer fell behind: this view is incomplete and must be
				// rebuilt with a fresh List plus a new Watch.
				log.Printf("watch ended: %v", ev.Err)
				return
			}
			fmt.Printf("[%s] %s: %+v\n", ev.EventType, ev.Object.Title, ev.Object)
		}
	}()

	fmt.Println("- Setting notes...")
	if _, err := s.Set(ctx, "notes", "note-1", Note{
		Title:   "Meeting Notes",
		Content: "Discussed Q4 planning...",
		Updated: time.Now(),
	}); err != nil {
		log.Fatal(err)
	}
	if _, err := s.Set(ctx, "notes", "note-2", Note{
		Title:   "Ideas",
		Content: "New feature brainstorm...",
		Updated: time.Now(),
	}); err != nil {
		log.Fatal(err)
	}

	notes, err := s.List(ctx, "notes")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("\nTotal notes: %d\n", len(notes))

	stopWatching()
	<-done
}
