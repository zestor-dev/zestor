// Command example serves an in-memory store of users over the network. It
// speaks the Connect protocol over HTTP/1.1, so curl works, and gRPC over
// unencrypted HTTP/2. See remote/README.md for requests to try.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"

	"github.com/zestor-dev/zestor/codec"
	"github.com/zestor-dev/zestor/remote"
	"github.com/zestor-dev/zestor/store"
	"github.com/zestor-dev/zestor/store/gomap"
)

type User struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

var errEmailRequired = errors.New("email required")

func main() {
	addr := flag.String("addr", "localhost:8080", "listen address")
	flag.Parse()

	s := gomap.NewMemStore[User](store.StoreOptions[User]{
		ValidateFns: map[string]store.ValidateFunc[User]{
			"users": func(u User) error {
				if u.Email == "" {
					return errEmailRequired
				}
				return nil
			},
		},
	})

	// WithErrors lets a Go client match errEmailRequired with errors.Is.
	path, handler := remote.NewHandler(s, &codec.JSON{}, remote.WithErrors(errEmailRequired))
	mux := http.NewServeMux()
	mux.Handle(path, handler)

	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	srv := &http.Server{Addr: *addr, Handler: mux, Protocols: protocols}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	shutdown := make(chan struct{})
	go func() {
		defer close(shutdown)
		<-ctx.Done()
		// Closing the store first ends open watches; Shutdown waits for
		// them, and a watch otherwise never finishes on its own.
		_ = s.Close()
		_ = srv.Shutdown(context.Background())
	}()

	log.Printf("serving %s on http://%s", path, *addr)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	// ListenAndServe returns as soon as Shutdown starts. Waiting for it lets
	// open watches send their final message.
	<-shutdown
}
