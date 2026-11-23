package remote_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"

	"github.com/zestor-dev/zestor/codec"
	"github.com/zestor-dev/zestor/remote"
	"github.com/zestor-dev/zestor/store"
	"github.com/zestor-dev/zestor/store/gomap"
)

type User struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

func Example() {
	// Server: any store.Store, here an in-memory one.
	s := gomap.NewMemStore[User](store.StoreOptions[User]{})
	defer s.Close()
	path, handler := remote.NewHandler(s, &codec.JSON{})
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Client: a store.Store that talks to the server.
	c := remote.NewClient[User](srv.URL, &codec.JSON{})
	defer c.Close()
	ctx := context.Background()

	res, _ := c.Set(ctx, "users", "alice", User{Name: "Alice", Email: "alice@example.com"})
	fmt.Println(res)

	changed, _ := c.SetFn(ctx, "users", "alice", func(u User) (User, error) {
		u.Email = "alice@example.org"
		return u, nil
	})
	u, _, _ := c.Get(ctx, "users", "alice")
	fmt.Println(changed, u.Email)

	// Output:
	// created
	// true alice@example.org
}
