package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/zestor-dev/zestor/codec"
	"github.com/zestor-dev/zestor/store"
	"github.com/zestor-dev/zestor/store/internal/watchhub"
)

const (
	kvSchema = `
CREATE TABLE IF NOT EXISTS zestor_kv (
  kind       TEXT    NOT NULL,
  key        TEXT    NOT NULL,
  value      BLOB    NOT NULL,
  version    INTEGER NOT NULL DEFAULT 1,
  updated_at TEXT    NOT NULL DEFAULT (STRFTIME('%Y-%m-%dT%H:%M:%fZ','now')),
  PRIMARY KEY(kind, key)
);
CREATE INDEX IF NOT EXISTS idx_kv_kind ON zestor_kv(kind);
`

	getQuery    = `SELECT value FROM zestor_kv WHERE kind=? AND key=?;`
	listQuery   = `SELECT key, value FROM zestor_kv WHERE kind=?;`
	countQuery  = `SELECT COUNT(*) FROM zestor_kv WHERE kind=?;`
	keysQuery   = `SELECT key FROM zestor_kv WHERE kind=?;`
	valuesQuery = `SELECT key, value FROM zestor_kv WHERE kind=?;`
	setQuery    = `INSERT INTO zestor_kv(kind,key,value) VALUES(?,?,?) ON CONFLICT(kind,key) DO NOTHING;`

	updateQuery = `
UPDATE zestor_kv
SET value=?, version=version+1, updated_at=STRFTIME('%Y-%m-%dT%H:%M:%fZ','now')
WHERE kind=? AND key=?;`
)

type Options struct {
	// SQLite DSN.
	// modernc: "file:zestor.db?cache=shared&_pragma=busy_timeout(5000)"
	DSN string

	// Codec to use for marshaling/unmarshaling values.
	Codec codec.Codec

	// If > 0, PRAGMA busy_timeout (ms) will be set.
	BusyTimeout time.Duration

	// If true, WAL mode will be disabled.
	DisableWAL bool
}

type sqLiteStore[T any] struct {
	db    *sql.DB
	codec codec.Codec

	// writeMu serializes writes with their event publication. SQLite already
	// admits one writer at a time, so this costs nothing real, and it is what
	// makes queue order match commit order for watchers. Watch takes it too, so
	// an initial-replay snapshot cannot interleave with a concurrent write.
	//
	// Lock order is always writeMu -> hub.
	writeMu sync.Mutex
	hub     *watchhub.Hub[T]

	// closed flag
	mu     sync.RWMutex
	closed bool
}

// New creates/opens the DB, applies the schema, and returns a Store[T].
func New[T any](ctx context.Context, o Options) (store.Store[T], error) {
	if o.DSN == "" {
		return nil, errors.New("sqlite: Options.DSN is required")
	}
	if o.Codec == nil {
		return nil, errors.New("sqlite: Options.Codec is required")
	}

	db, err := sql.Open("sqlite", o.DSN)
	if err != nil {
		return nil, err
	}

	if !o.DisableWAL {
		if _, err := db.ExecContext(ctx, `PRAGMA journal_mode=WAL;`); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("enable WAL: %w", err)
		}
	}
	if o.BusyTimeout > 0 {
		ms := int(o.BusyTimeout / time.Millisecond)
		if _, err := db.ExecContext(ctx, fmt.Sprintf(`PRAGMA busy_timeout=%d;`, ms)); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("set busy_timeout: %w", err)
		}
	}

	// apply schema
	if _, err := db.ExecContext(ctx, kvSchema); err != nil {
		_ = db.Close()
		return nil, err
	}

	return &sqLiteStore[T]{
		db:    db,
		codec: o.Codec,
		hub:   watchhub.New[T](),
	}, nil
}

// guard rejects operations on a closed store, and honours an already-cancelled
// context before doing any I/O.
func (s *sqLiteStore[T]) guard(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return store.ErrClosed
	}
	return nil
}

func (s *sqLiteStore[T]) Get(ctx context.Context, kind, key string) (T, bool, error) {
	var zero T
	if err := s.guard(ctx); err != nil {
		return zero, false, err
	}

	var blob []byte
	row := s.db.QueryRowContext(ctx, getQuery, kind, key)
	if err := row.Scan(&blob); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return zero, false, nil
		}
		return zero, false, err
	}
	var v T
	if err := s.codec.Unmarshal(blob, &v); err != nil {
		return zero, false, err
	}
	return v, true, nil
}

func (s *sqLiteStore[T]) List(ctx context.Context, kind string, filter ...store.FilterFunc[T]) (map[string]T, error) {
	if err := s.guard(ctx); err != nil {
		return nil, err
	}

	rows, err := s.db.QueryContext(ctx, listQuery, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]T)
	for rows.Next() {
		var k string
		var blob []byte
		if err := rows.Scan(&k, &blob); err != nil {
			return nil, err
		}
		var v T
		if err := s.codec.Unmarshal(blob, &v); err != nil {
			return nil, err
		}
		include := true
		for _, f := range filter {
			if f != nil && !f(k, v) {
				include = false
				break
			}
		}
		if include {
			out[k] = v
		}
	}
	return out, rows.Err()
}

func (s *sqLiteStore[T]) Count(ctx context.Context, kind string) (int, error) {
	if err := s.guard(ctx); err != nil {
		return 0, err
	}

	var n int
	if err := s.db.QueryRowContext(ctx, countQuery, kind).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

func (s *sqLiteStore[T]) Keys(ctx context.Context, kind string) ([]string, error) {
	if err := s.guard(ctx); err != nil {
		return nil, err
	}

	rows, err := s.db.QueryContext(ctx, keysQuery, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	keys := make([]string, 0)
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

func (s *sqLiteStore[T]) Values(ctx context.Context, kind string) ([]store.KeyValue[T], error) {
	if err := s.guard(ctx); err != nil {
		return nil, err
	}

	rows, err := s.db.QueryContext(ctx, valuesQuery, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]store.KeyValue[T], 0)
	for rows.Next() {
		var k string
		var blob []byte
		if err := rows.Scan(&k, &blob); err != nil {
			return nil, err
		}
		var v T
		if err := s.codec.Unmarshal(blob, &v); err != nil {
			return nil, err
		}
		out = append(out, store.KeyValue[T]{Key: k, Value: v})
	}
	return out, rows.Err()
}

func (s *sqLiteStore[T]) Set(ctx context.Context, kind, key string, value T) (bool, error) {
	if err := s.guard(ctx); err != nil {
		return false, err
	}

	enc, err := s.codec.Marshal(value)
	if err != nil {
		return false, err
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	// Rollback is driven by an explicit flag rather than by inspecting a named
	// error: every `if err := ...` below shadows the outer err, so anything that
	// keys off err would silently skip the rollback and leak the transaction —
	// which in SQLite means holding the write lock for the life of the process.
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	// Try INSERT; on conflict the row exists and we decide whether to update.
	res, err := tx.ExecContext(ctx, setQuery, kind, key, enc)
	if err != nil {
		return false, err
	}
	inserted, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	created := inserted > 0

	if !created {
		var cur []byte
		if err := tx.QueryRowContext(ctx, getQuery, kind, key).Scan(&cur); err != nil {
			return false, err
		}
		if bytes.Equal(cur, enc) {
			if err := tx.Commit(); err != nil {
				return false, err
			}
			committed = true
			return false, nil
		}
		if _, err := tx.ExecContext(ctx, updateQuery, enc, kind, key); err != nil {
			return false, err
		}
	}

	if err := tx.Commit(); err != nil {
		return false, err
	}
	committed = true

	etype := store.EventTypeUpdate
	if created {
		etype = store.EventTypeCreate
	}
	s.hub.Publish(kind, &store.Event[T]{Kind: kind, Name: key, EventType: etype, Object: value})
	return created, nil
}

func (s *sqLiteStore[T]) SetFn(ctx context.Context, kind, key string, fn func(v T) (T, error)) (bool, error) {
	if err := s.guard(ctx); err != nil {
		return false, err
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	var curBytes []byte
	if err := tx.QueryRowContext(ctx, getQuery, kind, key).Scan(&curBytes); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, store.ErrKeyNotFound
		}
		return false, err
	}
	var cur T
	if err := s.codec.Unmarshal(curBytes, &cur); err != nil {
		return false, err
	}

	nv, err := fn(cur)
	if err != nil {
		return false, err
	}
	newBytes, err := s.codec.Marshal(nv)
	if err != nil {
		return false, err
	}
	if bytes.Equal(curBytes, newBytes) {
		if err := tx.Commit(); err != nil {
			return false, err
		}
		committed = true
		return false, nil
	}

	if _, err := tx.ExecContext(ctx, updateQuery, newBytes, kind, key); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	committed = true

	s.hub.Publish(kind, &store.Event[T]{Kind: kind, Name: key, EventType: store.EventTypeUpdate, Object: nv})
	return true, nil
}

func (s *sqLiteStore[T]) SetAll(ctx context.Context, kind string, values map[string]T) error {
	if err := s.guard(ctx); err != nil {
		return err
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	existingKeys, err := existingKeysTx(ctx, tx, kind)
	if err != nil {
		return err
	}

	stmtIns, err := tx.PrepareContext(ctx, `
INSERT INTO zestor_kv(kind,key,value) VALUES(?,?,?)
ON CONFLICT(kind,key) DO UPDATE SET
  value      = excluded.value,
  version    = CASE WHEN zestor_kv.value != excluded.value
                    THEN zestor_kv.version + 1
                    ELSE zestor_kv.version
               END,
  updated_at = CASE WHEN zestor_kv.value != excluded.value
                    THEN STRFTIME('%Y-%m-%dT%H:%M:%fZ','now')
                    ELSE zestor_kv.updated_at
               END;
`)
	if err != nil {
		return err
	}
	defer stmtIns.Close()

	// Apply in key order so the event stream is deterministic across runs.
	evs := make([]*store.Event[T], 0, len(values))
	for _, k := range slices.Sorted(maps.Keys(values)) {
		v := values[k]
		enc, err := s.codec.Marshal(v)
		if err != nil {
			return err
		}
		if _, err := stmtIns.ExecContext(ctx, kind, k, enc); err != nil {
			return err
		}
		etype := store.EventTypeCreate
		if _, existed := existingKeys[k]; existed {
			etype = store.EventTypeUpdate
		}
		evs = append(evs, &store.Event[T]{Kind: kind, Name: k, EventType: etype, Object: v})
	}

	if err := tx.Commit(); err != nil {
		return err
	}
	committed = true

	s.hub.Publish(kind, evs...)
	return nil
}

// existingKeysTx reads and fully drains the key set, so the rows are closed
// before the caller issues further statements on the same transaction.
func existingKeysTx(ctx context.Context, tx *sql.Tx, kind string) (map[string]struct{}, error) {
	rows, err := tx.QueryContext(ctx, keysQuery, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]struct{})
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out[k] = struct{}{}
	}
	return out, rows.Err()
}

func (s *sqLiteStore[T]) Delete(ctx context.Context, kind, key string) (bool, T, error) {
	var zero T
	if err := s.guard(ctx); err != nil {
		return false, zero, err
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, zero, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	var prevBytes []byte
	if err := tx.QueryRowContext(ctx, getQuery, kind, key).Scan(&prevBytes); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, zero, nil
		}
		return false, zero, err
	}
	var prev T
	if err := s.codec.Unmarshal(prevBytes, &prev); err != nil {
		return false, zero, err
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM zestor_kv WHERE kind=? AND key=?;`, kind, key); err != nil {
		return false, zero, err
	}
	if err := tx.Commit(); err != nil {
		return false, zero, err
	}
	committed = true

	s.hub.Publish(kind, &store.Event[T]{Kind: kind, Name: key, EventType: store.EventTypeDelete, Object: prev})
	return true, prev, nil
}

func (s *sqLiteStore[T]) Watch(ctx context.Context, kind string, opts ...store.WatchOption[T]) (<-chan *store.Event[T], error) {
	if kind == "" {
		return nil, store.ErrKindRequired
	}
	if err := s.guard(ctx); err != nil {
		return nil, err
	}

	cfg := &store.WatchCfg[T]{}
	for _, o := range opts {
		if o != nil {
			o(cfg)
		}
	}

	// Holding writeMu across the snapshot is what guarantees that every replayed
	// event is queued before any event a concurrent write would publish. No
	// CatchUp hook is needed — a write publishes before it releases writeMu, so
	// nothing is ever in flight here.
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	return s.hub.Subscribe(ctx, kind, cfg, watchhub.Hooks[T]{
		Snapshot: func() ([]*store.Event[T], error) {
			return s.snapshot(ctx, kind)
		},
	})
}

func (s *sqLiteStore[T]) snapshot(ctx context.Context, kind string) ([]*store.Event[T], error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM zestor_kv WHERE kind=? ORDER BY key;`, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*store.Event[T]
	for rows.Next() {
		var k string
		var blob []byte
		if err := rows.Scan(&k, &blob); err != nil {
			return nil, err
		}
		var v T
		if err := s.codec.Unmarshal(blob, &v); err != nil {
			return nil, err
		}
		out = append(out, &store.Event[T]{Kind: kind, Name: k, EventType: store.EventTypeCreate, Object: v})
	}
	return out, rows.Err()
}

func (s *sqLiteStore[T]) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()

	s.hub.Close()
	return s.db.Close()
}

func (s *sqLiteStore[T]) Dump() string {
	var sb strings.Builder
	rows, err := s.db.Query(`SELECT kind, key, value, version, updated_at FROM zestor_kv ORDER BY kind, key;`)
	if err != nil {
		return err.Error()
	}
	defer rows.Close()
	for rows.Next() {
		var kind, key, updated string
		var value []byte
		var ver int
		if err := rows.Scan(&kind, &key, &value, &ver, &updated); err == nil {
			fmt.Fprintf(&sb, "%s/%s v%d (%dB) %s | value=%s\n", kind, key, ver, len(value), updated, string(value))
		}
	}
	return sb.String()
}

func (s *sqLiteStore[T]) GetAll(ctx context.Context) (map[string]map[string]T, error) {
	if err := s.guard(ctx); err != nil {
		return nil, err
	}

	rows, err := s.db.QueryContext(ctx, `SELECT kind, key, value FROM zestor_kv ORDER BY kind, key;`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]map[string]T)
	for rows.Next() {
		var kind, key string
		var blob []byte
		if err := rows.Scan(&kind, &key, &blob); err != nil {
			return nil, err
		}
		var v T
		if err := s.codec.Unmarshal(blob, &v); err != nil {
			return nil, err
		}
		if _, ok := out[kind]; !ok {
			out[kind] = make(map[string]T)
		}
		out[kind][key] = v
	}
	return out, rows.Err()
}
