package postgres_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/zestor-dev/zestor/store"
)

const benchKindSize = 10_000

// benchStore seeds the kind with SQL. Seeding through SetMany would time the
// bulk-write path we want the benchmark itself to measure.
func benchStore(b *testing.B) store.Store[TestData] {
	b.Helper()
	ns := nextNamespace()
	s := newStore[TestData](b, ns)
	b.Cleanup(func() { _ = s.Close() })

	ctx := context.Background()
	conn, err := pgx.Connect(ctx, getPostgresConn())
	if err != nil {
		b.Fatal(err)
	}
	defer conn.Close(ctx)
	_, err = conn.Exec(ctx, `
INSERT INTO zestor_kv(ns_id, kind, key, value)
SELECT $1, 'k', 'k' || lpad(i::text, 5, '0'),
       convert_to('{"name":"s","value":0}', 'UTF8')
  FROM generate_series(0, $2) AS i`, ns, benchKindSize-1)
	if err != nil {
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
	p := s.(store.Pager[TestData])
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
	p := s.(store.Pager[TestData])
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
	p := s.(store.Pager[TestData])
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
		if _, err := s.Set(ctx, "k", "k00000", TestData{Name: "s", Value: i}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSetMany10k(b *testing.B) {
	s := benchStore(b)
	ctx := context.Background()
	vals := make(map[string]TestData, benchKindSize)
	keys := make([]string, benchKindSize)
	for i := range benchKindSize {
		keys[i] = fmt.Sprintf("k%05d", i)
		vals[keys[i]] = TestData{Name: "s"}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; b.Loop(); n++ {
		for _, k := range keys {
			v := vals[k]
			v.Value = n
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
		err := s.SetMany(ctx, "k", map[string]TestData{
			"k00000": {Name: "s", Value: i},
			"k00001": {Name: "s", Value: i},
			"k00002": {Name: "s", Value: i},
		})
		if err != nil {
			b.Fatal(err)
		}
	}
}
