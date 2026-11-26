package remote_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zestor-dev/zestor/codec"
	"github.com/zestor-dev/zestor/remote"
	"github.com/zestor-dev/zestor/store"
	"github.com/zestor-dev/zestor/store/gomap"
)

const benchKindSize = 10_000

type benchVal struct {
	Name string
	N    int
}

func benchClient(b *testing.B) *remote.Client[benchVal] {
	b.Helper()
	s := gomap.NewMemStore[benchVal](store.StoreOptions[benchVal]{})
	b.Cleanup(func() { _ = s.Close() })
	seed := make(map[string]benchVal, benchKindSize)
	for i := range benchKindSize {
		seed[fmt.Sprintf("k%05d", i)] = benchVal{Name: "s"}
	}
	if err := s.SetMany(context.Background(), "k", seed); err != nil {
		b.Fatal(err)
	}
	path, handler := remote.NewHandler(s, &codec.JSON{})
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	srv := httptest.NewServer(mux)
	b.Cleanup(srv.Close)
	c := remote.NewClient[benchVal](srv.URL, &codec.JSON{}, remote.WithHTTPClient(srv.Client()))
	b.Cleanup(func() { _ = c.Close() })
	return c
}

func BenchmarkClientList10k(b *testing.B) {
	c := benchClient(b)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		got, err := c.List(ctx, "k")
		if err != nil {
			b.Fatal(err)
		}
		if len(got) != benchKindSize {
			b.Fatalf("List len = %d, want %d", len(got), benchKindSize)
		}
	}
}

func BenchmarkClientListPage100(b *testing.B) {
	c := benchClient(b)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		got, _, err := c.ListPage(ctx, "k", "", 100)
		if err != nil {
			b.Fatal(err)
		}
		if len(got) != 100 {
			b.Fatalf("ListPage len = %d, want 100", len(got))
		}
	}
}
