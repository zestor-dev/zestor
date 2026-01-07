package main

import (
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
	s, err := sqlite.New[Note](sqlite.Options{
		DSN:         "file:notes.db?cache=shared",
		Codec:       &codec.JSON{},
		BusyTimeout: 5 * time.Second,
	})
	if err != nil {
		log.Fatal(err)
	}
	defer s.Close()

	// watch for changes
	fmt.Println("- Watching for changes...")
	ch, cancel, _ := s.Watch("notes", store.WithInitialReplay[Note]())
	defer cancel()

	go func() {
		for ev := range ch {
			fmt.Printf("[%s] %s: %+v\n", ev.EventType, ev.Object.Title, ev.Object)
		}
	}()
	time.Sleep(5000 * time.Millisecond)
	fmt.Println("- Setting notes...")
	// create notes
	s.Set("notes", "note-1", Note{
		Title:   "Meeting Notes",
		Content: "Discussed Q4 planning...",
		Updated: time.Now(),
	})

	s.Set("notes", "note-2", Note{
		Title:   "Ideas",
		Content: "New feature brainstorm...",
		Updated: time.Now(),
	})
	time.Sleep(time.Second)
	// list all notes
	notes, _ := s.List("notes")
	fmt.Printf("\nTotal notes: %d\n", len(notes))
	<-time.After(time.Second)
}
