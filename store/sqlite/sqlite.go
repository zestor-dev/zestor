package sqlite

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
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
	// modernc: "file:zestor.db", or ":memory:" for a private in-memory database
	DSN string

	// Codec to use for marshaling/unmarshaling values.
	Codec codec.Codec

	// If > 0, PRAGMA busy_timeout (ms) is set on every connection. Do not also
	// set busy_timeout in DSN; New rejects that.
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

	validateFns map[string]store.ValidateFunc[T]
	// compareFn is nil unless the caller supplied one; writes then compare the
	// encoded bytes, which avoids decoding the stored value.
	compareFn store.CompareFunc[T]

	// closed flag
	mu     sync.RWMutex
	closed bool
}

// New creates/opens the DB, applies the schema, and returns a Store[T].
func New[T any](ctx context.Context, o Options) (store.Store[T], error) {
	return NewWithStoreOptions(ctx, o, store.StoreOptions[T]{})
}

// NewWithStoreOptions is New with per-kind validation and custom change
// detection. Without a CompareFn, a write is unchanged when its encoding is
// byte-for-byte equal to the stored one; with one, the stored value is decoded
// and CompareFn decides.
func NewWithStoreOptions[T any](ctx context.Context, o Options, so store.StoreOptions[T]) (store.Store[T], error) {
	if o.DSN == "" {
		return nil, errors.New("sqlite: Options.DSN is required")
	}
	if o.Codec == nil {
		return nil, errors.New("sqlite: Options.Codec is required")
	}

	dsn := o.DSN
	if o.BusyTimeout > 0 {
		// The driver gives no defined order between two busy_timeout pragmas.
		if strings.Contains(strings.ToLower(dsn), "busy_timeout") {
			return nil, errors.New("sqlite: set busy_timeout in Options.BusyTimeout or in the DSN, not both")
		}
		// busy_timeout is per connection, so it goes in the DSN, which the
		// driver applies to every connection the pool opens. Running the PRAGMA
		// once through the *sql.DB would reach a single pooled connection.
		dsn = withPragma(dsn, fmt.Sprintf("busy_timeout(%d)", o.BusyTimeout.Milliseconds()))
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if isMemoryDSN(o.DSN) {
		// Each connection to an in-memory database opens its own private,
		// empty one, so the pool must never hold more than a single connection.
		db.SetMaxOpenConns(1)
	}

	// journal_mode=WAL is persistent in the database file, so setting it once
	// is enough.
	if !o.DisableWAL {
		if _, err := db.ExecContext(ctx, `PRAGMA journal_mode=WAL;`); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("enable WAL: %w", err)
		}
	}

	// apply schema
	if _, err := db.ExecContext(ctx, kvSchema); err != nil {
		_ = db.Close()
		return nil, err
	}

	return &sqLiteStore[T]{
		db:          db,
		codec:       o.Codec,
		hub:         watchhub.New[T](),
		validateFns: maps.Clone(so.ValidateFns),
		compareFn:   so.CompareFn,
	}, nil
}

func (s *sqLiteStore[T]) validate(kind string, v T) error {
	if fn, ok := s.validateFns[kind]; ok {
		return fn(v)
	}
	return nil
}

// unchanged reports whether writing next, encoded as enc, over the stored
// encoding cur would change nothing.
func (s *sqLiteStore[T]) unchanged(cur, enc []byte, next T) (bool, error) {
	if s.compareFn == nil {
		return bytes.Equal(cur, enc), nil
	}
	var prev T
	if err := s.codec.Unmarshal(cur, &prev); err != nil {
		return false, err
	}
	return s.compareFn(prev, next), nil
}

// isMemoryDSN reports whether dsn names an in-memory database: ":memory:",
// a "file::memory:" URI, or a URI with mode=memory.
func isMemoryDSN(dsn string) bool {
	return strings.Contains(dsn, ":memory:") || strings.Contains(dsn, "mode=memory")
}

// withPragma appends a _pragma query parameter to dsn.
func withPragma(dsn, pragma string) string {
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	return dsn + sep + "_pragma=" + url.QueryEscape(pragma)
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

func requireKind(kind string) error {
	if kind == "" {
		return store.ErrKindRequired
	}
	return nil
}

// guardKind is guard plus the empty-kind check that Watch already applied.
func (s *sqLiteStore[T]) guardKind(ctx context.Context, kind string) error {
	if err := requireKind(kind); err != nil {
		return err
	}
	return s.guard(ctx)
}

func (s *sqLiteStore[T]) Get(ctx context.Context, kind, key string) (T, bool, error) {
	var zero T
	if err := s.guardKind(ctx, kind); err != nil {
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
	if err := s.guardKind(ctx, kind); err != nil {
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
	if err := s.guardKind(ctx, kind); err != nil {
		return 0, err
	}

	var n int
	if err := s.db.QueryRowContext(ctx, countQuery, kind).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

func (s *sqLiteStore[T]) Keys(ctx context.Context, kind string) ([]string, error) {
	if err := s.guardKind(ctx, kind); err != nil {
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
	if err := s.guardKind(ctx, kind); err != nil {
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

func (s *sqLiteStore[T]) Set(ctx context.Context, kind, key string, value T) (store.SetResult, error) {
	if err := s.guardKind(ctx, kind); err != nil {
		return store.SetUnchanged, err
	}
	if err := s.validate(kind, value); err != nil {
		return store.SetUnchanged, err
	}

	enc, err := s.codec.Marshal(value)
	if err != nil {
		return store.SetUnchanged, err
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return store.SetUnchanged, err
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
		return store.SetUnchanged, err
	}
	inserted, err := res.RowsAffected()
	if err != nil {
		return store.SetUnchanged, err
	}
	result := store.SetCreated

	if inserted == 0 {
		var cur []byte
		if err := tx.QueryRowContext(ctx, getQuery, kind, key).Scan(&cur); err != nil {
			return store.SetUnchanged, err
		}
		same, err := s.unchanged(cur, enc, value)
		if err != nil {
			return store.SetUnchanged, err
		}
		if same {
			if err := tx.Commit(); err != nil {
				return store.SetUnchanged, err
			}
			committed = true
			return store.SetUnchanged, nil
		}
		if _, err := tx.ExecContext(ctx, updateQuery, enc, kind, key); err != nil {
			return store.SetUnchanged, err
		}
		result = store.SetUpdated
	}

	if err := tx.Commit(); err != nil {
		return store.SetUnchanged, err
	}
	committed = true

	etype := store.EventTypeCreate
	if result == store.SetUpdated {
		etype = store.EventTypeUpdate
	}
	s.hub.Publish(kind, &store.Event[T]{Kind: kind, Name: key, EventType: etype, Object: value})
	return result, nil
}

func (s *sqLiteStore[T]) SetFn(ctx context.Context, kind, key string, fn func(v T) (T, error)) (bool, error) {
	if err := s.guardKind(ctx, kind); err != nil {
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
	if err := s.validate(kind, nv); err != nil {
		return false, err
	}
	newBytes, err := s.codec.Marshal(nv)
	if err != nil {
		return false, err
	}
	same := bytes.Equal(curBytes, newBytes)
	if s.compareFn != nil {
		same = s.compareFn(cur, nv)
	}
	if same {
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

func (s *sqLiteStore[T]) SetMany(ctx context.Context, kind string, values map[string]T) error {
	if err := s.guardKind(ctx, kind); err != nil {
		return err
	}
	if len(values) == 0 {
		return nil
	}
	keys := slices.Sorted(maps.Keys(values))
	// Validate the whole batch before touching the database, so a rejected
	// batch writes nothing.
	for _, k := range keys {
		if err := s.validate(kind, values[k]); err != nil {
			return err
		}
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

	existing, err := existingRowsTx(ctx, tx, kind, keys)
	if err != nil {
		return err
	}

	stmtIns, err := tx.PrepareContext(ctx, `
INSERT INTO zestor_kv(kind,key,value) VALUES(?,?,?)
ON CONFLICT(kind,key) DO UPDATE SET
  value      = excluded.value,
  version    = zestor_kv.version + 1,
  updated_at = STRFTIME('%Y-%m-%dT%H:%M:%fZ','now');
`)
	if err != nil {
		return err
	}
	defer stmtIns.Close()

	// Apply in key order so the event stream is deterministic across runs.
	evs := make([]*store.Event[T], 0, len(values))
	for _, k := range keys {
		v := values[k]
		enc, err := s.codec.Marshal(v)
		if err != nil {
			return err
		}
		prev, existed := existing[k]
		if existed {
			same, err := s.unchanged(prev, enc, v)
			if err != nil {
				return err
			}
			if same {
				continue
			}
		}
		if _, err := stmtIns.ExecContext(ctx, kind, k, enc); err != nil {
			return err
		}
		etype := store.EventTypeCreate
		if existed {
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

// existingChunk bounds the keys bound into one IN (...) list, well under
// SQLite's limit on host parameters.
const existingChunk = 500

// existingRowsTx returns the stored encoding of each of keys that exists in
// kind. The encoded bytes, not just existence, are needed: a key whose value is
// unchanged must be skipped entirely, the way a single Set skips it. Only the
// requested keys are read, so the cost follows the batch, not the kind. Each
// chunk's rows are drained and closed before the next statement runs on tx.
func existingRowsTx(ctx context.Context, tx *sql.Tx, kind string, keys []string) (map[string][]byte, error) {
	out := make(map[string][]byte, len(keys))
	for chunk := range slices.Chunk(keys, existingChunk) {
		args := make([]any, 0, len(chunk)+1)
		args = append(args, kind)
		for _, k := range chunk {
			args = append(args, k)
		}
		q := `SELECT key, value FROM zestor_kv WHERE kind=? AND key IN (?` +
			strings.Repeat(",?", len(chunk)-1) + `);`
		if err := func() error {
			rows, err := tx.QueryContext(ctx, q, args...)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var k string
				var v []byte
				if err := rows.Scan(&k, &v); err != nil {
					return err
				}
				out[k] = v
			}
			return rows.Err()
		}(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *sqLiteStore[T]) Delete(ctx context.Context, kind, key string) (bool, T, error) {
	var zero T
	if err := s.guardKind(ctx, kind); err != nil {
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
	if err := s.guardKind(ctx, kind); err != nil {
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

// Dump implements store.Dumper.
func (s *sqLiteStore[T]) Dump(ctx context.Context, w io.Writer) error {
	if err := s.guard(ctx); err != nil {
		return err
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT kind, key, value, version, updated_at FROM zestor_kv ORDER BY kind, key;`)
	if err != nil {
		return err
	}
	defer rows.Close()

	bw := bufio.NewWriter(w)
	for rows.Next() {
		var kind, key, updated string
		var value []byte
		var ver int
		if err := rows.Scan(&kind, &key, &value, &ver, &updated); err != nil {
			return err
		}
		var decoded T
		if err := s.codec.Unmarshal(value, &decoded); err != nil {
			if _, err := fmt.Fprintf(bw, "%s/%s v%d (%dB) %s | value=<undecodable>\n",
				kind, key, ver, len(value), updated); err != nil {
				return err
			}
			continue
		}
		if _, err := fmt.Fprintf(bw, "%s/%s v%d (%dB) %s | value=%+v\n",
			kind, key, ver, len(value), updated, decoded); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return bw.Flush()
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
