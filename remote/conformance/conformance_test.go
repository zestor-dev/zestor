// Package conformance runs the shared store suite through the remote client
// against the SQLite and Postgres backends. It is its own module so that the
// remote module does not depend on either database driver.
package conformance

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zestor-dev/zestor/codec"
	"github.com/zestor-dev/zestor/remote"
	"github.com/zestor-dev/zestor/store"
	"github.com/zestor-dev/zestor/store/postgres"
	"github.com/zestor-dev/zestor/store/sqlite"
	"github.com/zestor-dev/zestor/store/storetest"
)

type value = storetest.Value

// serve exposes s over HTTP until the test ends, and closes s first so the
// server's watch handlers return before it shuts down.
func serve(t *testing.T, s store.Store[value]) store.Store[value] {
	t.Helper()
	path, h := remote.NewHandler(s, &codec.JSON{}, remote.WithErrors(storetest.ErrInvalid))
	mux := http.NewServeMux()
	mux.Handle(path, h)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	t.Cleanup(func() { _ = s.Close() })
	return remote.NewClient[value](srv.URL, &codec.JSON{},
		remote.WithHTTPClient(srv.Client()),
		remote.WithErrors(storetest.ErrInvalid),
	)
}

func TestSQLite(t *testing.T) {
	open := func(t *testing.T, so store.StoreOptions[value]) store.Store[value] {
		s, err := sqlite.NewWithStoreOptions(context.Background(), sqlite.Options{
			DSN:         "file:" + filepath.Join(t.TempDir(), "zestor.db"),
			Codec:       &codec.JSON{},
			BusyTimeout: 5 * time.Second,
		}, so)
		if err != nil {
			t.Fatalf("sqlite: %v", err)
		}
		return serve(t, s)
	}
	storetest.Run(t, storetest.Config{
		New:            func(t *testing.T) store.Store[value] { return open(t, store.StoreOptions[value]{}) },
		NewWithOptions: open,
		EventDelay:     2 * time.Second,
		SettleDelay:    250 * time.Millisecond,
	})
}

var nsCounter atomic.Int64

// postgresConn returns the DSN, skipping the test when none is configured
// unless POSTGRES_REQUIRE asks for a failure instead.
func postgresConn(t *testing.T) string {
	t.Helper()
	conn := strings.TrimSpace(os.Getenv("POSTGRES_CONN"))
	if conn == "" {
		if os.Getenv("POSTGRES_REQUIRE") != "" {
			t.Fatal("POSTGRES_REQUIRE is set but POSTGRES_CONN is empty")
		}
		t.Skip("POSTGRES_CONN is not set")
	}
	return conn
}

func TestPostgres(t *testing.T) {
	conn := postgresConn(t)
	open := func(t *testing.T, ns int64, so store.StoreOptions[value]) store.Store[value] {
		s, err := postgres.NewWithStoreOptions(context.Background(), postgres.Options{
			ConnString: conn,
			Codec:      &codec.JSON{},
			Namespace:  ns,
			Timeout:    30 * time.Second,
		}, so)
		if err != nil {
			t.Fatalf("postgres: %v", err)
		}
		return serve(t, s)
	}
	nextNS := func() int64 { return time.Now().UnixNano() + nsCounter.Add(1) }
	storetest.Run(t, storetest.Config{
		New: func(t *testing.T) store.Store[value] {
			return open(t, nextNS(), store.StoreOptions[value]{})
		},
		NewWithOptions: func(t *testing.T, so store.StoreOptions[value]) store.Store[value] {
			return open(t, nextNS(), so)
		},
		// Two servers on one namespace, each with its own client: the
		// watching client sees writes that went through the other server.
		NewShared: func(t *testing.T) (store.Store[value], store.Store[value]) {
			ns := nextNS()
			return open(t, ns, store.StoreOptions[value]{}), open(t, ns, store.StoreOptions[value]{})
		},
		EventDelay:  15 * time.Second,
		SettleDelay: 2 * time.Second,
	})
}
