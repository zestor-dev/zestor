package postgres_test

import (
	"context"
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
