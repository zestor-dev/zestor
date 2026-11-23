package remote_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/zestor-dev/zestor/codec"
	"github.com/zestor-dev/zestor/remote"
	"github.com/zestor-dev/zestor/store"
	"github.com/zestor-dev/zestor/store/gomap"
	"github.com/zestor-dev/zestor/store/storetest"
)

type protocol struct {
	name  string
	http2 bool
	opts  []connect.ClientOption
}

var protocols = []protocol{
	{name: "Connect"},
	{name: "ConnectJSON", opts: []connect.ClientOption{connect.WithProtoJSON()}},
	{name: "GRPC", http2: true, opts: []connect.ClientOption{connect.WithGRPC()}},
}

// serve exposes s over HTTP and returns a function that opens clients to it.
// The server stops after the test; the caller decides when s closes.
func serve(t *testing.T, s store.Store[storetest.Value], p protocol) func() store.Store[storetest.Value] {
	t.Helper()
	path, h := remote.NewHandler(s, &codec.JSON{}, remote.WithErrors(storetest.ErrInvalid))
	mux := http.NewServeMux()
	mux.Handle(path, h)
	srv := httptest.NewUnstartedServer(mux)
	if p.http2 {
		srv.EnableHTTP2 = true
		srv.StartTLS()
	} else {
		srv.Start()
	}
	t.Cleanup(srv.Close)
	return func() store.Store[storetest.Value] {
		return remote.NewClient[storetest.Value](srv.URL, &codec.JSON{},
			remote.WithHTTPClient(srv.Client()),
			remote.WithClientOptions(p.opts...),
			remote.WithErrors(storetest.ErrInvalid),
		)
	}
}

// Every case of the shared suite runs through a client, the network, and a
// server wrapping an in-memory store.
func TestConformance(t *testing.T) {
	for _, p := range protocols {
		t.Run(p.name, func(t *testing.T) {
			open := func(t *testing.T, opts store.StoreOptions[storetest.Value]) store.Store[storetest.Value] {
				s := gomap.NewMemStore[storetest.Value](opts)
				client := serve(t, s, p)
				// Registered after the server, so it runs first: closing the
				// store ends the server's watch handlers, and the server can
				// then shut down.
				t.Cleanup(func() { _ = s.Close() })
				return client()
			}
			storetest.Run(t, storetest.Config{
				New: func(t *testing.T) store.Store[storetest.Value] {
					return open(t, store.StoreOptions[storetest.Value]{})
				},
				NewWithOptions: open,
				// Two clients of one server: one writes, the other watches.
				NewShared: func(t *testing.T) (store.Store[storetest.Value], store.Store[storetest.Value]) {
					s := gomap.NewMemStore[storetest.Value](store.StoreOptions[storetest.Value]{})
					client := serve(t, s, p)
					t.Cleanup(func() { _ = s.Close() })
					return client(), client()
				},
				EventDelay:  2 * time.Second,
				SettleDelay: 200 * time.Millisecond,
			})
		})
	}
}
