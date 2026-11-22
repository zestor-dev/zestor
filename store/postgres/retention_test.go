package postgres

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/zestor-dev/zestor/codec"
)

type retainVal struct {
	Name string `json:"name"`
}

// Rows older than the retention stay when their id is still ahead of the
// cursor. Rows at or behind the cursor, and older than the retention, go.
func TestPruneKeepsRowsAheadOfCursor(t *testing.T) {
	connStr := os.Getenv("POSTGRES_CONN")
	if connStr == "" {
		t.Skip("POSTGRES_CONN not set")
	}
	ctx := context.Background()
	ns := time.Now().UnixNano()
	st, err := New[retainVal](ctx, Options{
		ConnString:      connStr,
		Codec:           &codec.JSON{},
		Namespace:       ns,
		Timeout:         10 * time.Second,
		OutboxRetention: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := st.(*storePG[retainVal])

	conn, err := pgx.Connect(ctx, connStr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	old := time.Now().Add(-2 * time.Hour)
	var first, second int64
	err = conn.QueryRow(ctx, `
INSERT INTO zestor_outbox(ns_id, kind, key, etype, value, at)
VALUES ($1, 'k', 'first', 'create', convert_to('{"name":"a"}','UTF8'), $2)
RETURNING id`, ns, old).Scan(&first)
	if err != nil {
		t.Fatal(err)
	}
	err = conn.QueryRow(ctx, `
INSERT INTO zestor_outbox(ns_id, kind, key, etype, value, at)
VALUES ($1, 'k', 'second', 'create', convert_to('{"name":"b"}','UTF8'), $2)
RETURNING id`, ns, old).Scan(&second)
	if err != nil {
		t.Fatal(err)
	}
	// Let the insert notifications finish moving the cursor before we put it
	// back. A later move would treat the second row as consumed.
	time.Sleep(300 * time.Millisecond)

	// Pretend only the first row has been consumed.
	c := s.cursor("k")
	c.mu.Lock()
	c.seen = true
	c.lastID = first
	c.mu.Unlock()

	if err := s.pruneOutbox(ctx); err != nil {
		t.Fatal(err)
	}
	if n := outboxCount(t, conn, ns, "first"); n != 0 {
		t.Errorf("consumed old row still present: %d", n)
	}
	if n := outboxCount(t, conn, ns, "second"); n != 1 {
		t.Errorf("unconsumed old row count = %d, want 1", n)
	}

	var fresh int64
	err = conn.QueryRow(ctx, `
INSERT INTO zestor_outbox(ns_id, kind, key, etype, value)
VALUES ($1, 'k', 'fresh', 'create', convert_to('{"name":"c"}','UTF8'))
RETURNING id`, ns).Scan(&fresh)
	if err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.lastID = fresh
	c.mu.Unlock()
	if err := s.pruneOutbox(ctx); err != nil {
		t.Fatal(err)
	}
	if n := outboxCount(t, conn, ns, "fresh"); n != 1 {
		t.Errorf("fresh row was deleted: count %d", n)
	}
	// The cursor now covers the second row, and it is older than the retention.
	if n := outboxCount(t, conn, ns, "second"); n != 0 {
		t.Errorf("consumed old row still present: count %d", n)
	}
}

func TestTinyTimeoutExpiresGet(t *testing.T) {
	connStr := os.Getenv("POSTGRES_CONN")
	if connStr == "" {
		t.Skip("POSTGRES_CONN not set")
	}
	ctx := context.Background()
	st, err := New[retainVal](ctx, Options{
		ConnString: connStr,
		Codec:      &codec.JSON{},
		Namespace:  time.Now().UnixNano(),
		Timeout:    10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := st.(*storePG[retainVal])
	s.timeout = time.Nanosecond
	if _, _, err := s.Get(ctx, "k", "a"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Get with a 1ns timeout returned %v, want DeadlineExceeded", err)
	}
}

func outboxCount(t *testing.T, conn *pgx.Conn, ns int64, key string) int {
	t.Helper()
	var n int
	err := conn.QueryRow(context.Background(), `
SELECT count(*) FROM zestor_outbox WHERE ns_id=$1 AND key=$2`, ns, key).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n
}
