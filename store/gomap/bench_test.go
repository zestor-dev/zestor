package gomap_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/zestor-dev/zestor/store"
	"github.com/zestor-dev/zestor/store/gomap"
)

// benchKindSize is large enough that a SetMany which reads the whole kind is
// obviously slower than one which looks up only the keys it writes.
const benchKindSize = 10_000

type benchVal struct {
	Name string
	N    int
}

func benchStore(b *testing.B) store.Store[benchVal] {
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
	return s
}

func BenchmarkGet(b *testing.B) {
	s := benchStore(b)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, _, err := s.Get(ctx, "k", "k00000"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkList10k(b *testing.B) {
	s := benchStore(b)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		got, err := s.List(ctx, "k")
		if err != nil {
			b.Fatal(err)
		}
		if len(got) != benchKindSize {
			b.Fatalf("List len = %d, want %d", len(got), benchKindSize)
		}
	}
}

func BenchmarkSet(b *testing.B) {
	s := benchStore(b)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		if _, err := s.Set(ctx, "k", "k00000", benchVal{Name: "s", N: i}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSetMany3Into10k(b *testing.B) {
	s := benchStore(b)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		err := s.SetMany(ctx, "k", map[string]benchVal{
			"k00000": {Name: "s", N: i},
			"k00001": {Name: "s", N: i},
			"k00002": {Name: "s", N: i},
		})
		if err != nil {
			b.Fatal(err)
		}
	}
}
