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

func BenchmarkListPage100(b *testing.B) {
	s := benchStore(b)
	p := s.(store.Pager[benchVal])
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		got, err := p.ListPage(ctx, "k", nil, 100)
		if err != nil {
			b.Fatal(err)
		}
		if len(got) != 100 {
			b.Fatalf("ListPage len = %d, want 100", len(got))
		}
	}
}

func BenchmarkKeysPage100(b *testing.B) {
	s := benchStore(b)
	p := s.(store.Pager[benchVal])
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		got, err := p.KeysPage(ctx, "k", nil, 100)
		if err != nil {
			b.Fatal(err)
		}
		if len(got) != 100 {
			b.Fatalf("KeysPage len = %d, want 100", len(got))
		}
	}
}

// BenchmarkListPages10k reads the whole kind as pages of 500. Compare it with
// BenchmarkList10k, which reads the kind in one call.
func BenchmarkListPages10k(b *testing.B) {
	const page = 500
	s := benchStore(b)
	p := s.(store.Pager[benchVal])
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		var after *string
		n := 0
		for {
			got, err := p.ListPage(ctx, "k", after, page)
			if err != nil {
				b.Fatal(err)
			}
			n += len(got)
			if len(got) < page {
				break
			}
			last := got[len(got)-1].Key
			after = &last
		}
		if n != benchKindSize {
			b.Fatalf("paged List len = %d, want %d", n, benchKindSize)
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

func BenchmarkSetMany10k(b *testing.B) {
	s := benchStore(b)
	ctx := context.Background()
	vals := make(map[string]benchVal, benchKindSize)
	keys := make([]string, benchKindSize)
	for i := range benchKindSize {
		keys[i] = fmt.Sprintf("k%05d", i)
		vals[keys[i]] = benchVal{Name: "s"}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; b.Loop(); n++ {
		for _, k := range keys {
			v := vals[k]
			v.N = n
			vals[k] = v
		}
		if err := s.SetMany(ctx, "k", vals); err != nil {
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
