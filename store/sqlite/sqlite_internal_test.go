package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/zestor-dev/zestor/codec"
)

// busy_timeout is per connection. Setting it once through the *sql.DB reaches
// whichever pooled connection served that call and no other, so every
// connection the pool opens has to be checked.
func TestBusyTimeoutAppliesToEveryConnection(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "zestor.db")

	for _, tc := range []struct{ name, dsn string }{
		{"FileURI", "file:" + path},
		{"FileURIWithQuery", "file:" + path + "?_pragma=foreign_keys(1)"},
		{"PlainPath", path},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := New[int](ctx, Options{
				DSN:         tc.dsn,
				Codec:       &codec.JSON{},
				BusyTimeout: 1234 * time.Millisecond,
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			defer st.Close()
			db := st.(*sqLiteStore[int]).db

			// Hold the connections simultaneously so the pool must open new ones.
			const n = 4
			conns := make([]*sql.Conn, 0, n)
			defer func() {
				for _, c := range conns {
					_ = c.Close()
				}
			}()
			for range n {
				c, err := db.Conn(ctx)
				if err != nil {
					t.Fatalf("Conn: %v", err)
				}
				conns = append(conns, c)
			}
			for i, c := range conns {
				var ms int
				if err := c.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&ms); err != nil {
					t.Fatalf("PRAGMA busy_timeout: %v", err)
				}
				if ms != 1234 {
					t.Errorf("connection %d has busy_timeout=%d, want 1234", i, ms)
				}
			}
		})
	}
}
