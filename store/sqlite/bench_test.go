package sqlite_test

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/zestor-dev/zestor/codec"
	"github.com/zestor-dev/zestor/store"
	"github.com/zestor-dev/zestor/store/sqlite"
)

const benchKindSize = 10_000

func benchStore(b *testing.B) store.Store[Note] {
	b.Helper()
	s, err := sqlite.New[Note](context.Background(), sqlite.Options{
		DSN:         "file:" + filepath.Join(b.TempDir(), "bench.db"),
		Codec:       &codec.JSON{},
		BusyTimeout: 5 * time.Second,
	})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = s.Close() })
	seed := make(map[string]Note, benchKindSize)
	for i := range benchKindSize {
		seed[fmt.Sprintf("k%05d", i)] = Note{Title: "s"}
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
		if _, err := s.Set(ctx, "k", "k00000", Note{Title: "s", N: i}); err != nil {
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
		err := s.SetMany(ctx, "k", map[string]Note{
			"k00000": {Title: "s", N: i},
			"k00001": {Title: "s", N: i},
			"k00002": {Title: "s", N: i},
		})
		if err != nil {
			b.Fatal(err)
		}
	}
}
