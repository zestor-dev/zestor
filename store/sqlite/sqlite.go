package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/zestor-dev/zestor/codec"
	"github.com/zestor-dev/zestor/store"
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

type watcher[T any] struct {
	ch         chan *store.Event[T]
	eventTypes map[store.EventType]struct{}
}

type sqLiteStore[T any] struct {
	db    *sql.DB
	codec codec.Codec

	// in-proc pubsub for Watch(kind)
	muSubs sync.RWMutex
	subs   map[string]map[*watcher[T]]struct{}

	// closed flag
	mu     sync.RWMutex
	closed bool
}

// New creates/opens the DB, applies the schema, and returns a Store[T].
func New[T any](o Options) (store.Store[T], error) {
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

	ctx := context.Background()
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
		subs:  make(map[string]map[*watcher[T]]struct{}),
	}, nil
}

func (s *sqLiteStore[T]) Get(kind, key string) (T, bool, error) {
	var zero T
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return zero, false, store.ErrClosed
	}
	s.mu.RUnlock()

	var blob []byte
	row := s.db.QueryRow(getQuery, kind, key)
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

func (s *sqLiteStore[T]) List(kind string, filter ...store.FilterFunc[T]) (map[string]T, error) {
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return nil, store.ErrClosed
	}
	s.mu.RUnlock()

	out := make(map[string]T, 64)
	rows, err := s.db.Query(listQuery, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

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

func (s *sqLiteStore[T]) Count(kind string) (int, error) {
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return 0, store.ErrClosed
	}
	s.mu.RUnlock()

	var n int
	if err := s.db.QueryRow(countQuery, kind).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

func (s *sqLiteStore[T]) Keys(kind string) ([]string, error) {
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return nil, store.ErrClosed
	}
	s.mu.RUnlock()

	rows, err := s.db.Query(keysQuery, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	keys := make([]string, 0, 64)
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

func (s *sqLiteStore[T]) Values(kind string) ([]store.KeyValue[T], error) {
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return nil, store.ErrClosed
	}
	s.mu.RUnlock()

	rows, err := s.db.Query(valuesQuery, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]store.KeyValue[T], 0, 64)
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

func (s *sqLiteStore[T]) Set(kind, key string, value T) (bool, error) {
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return false, store.ErrClosed
	}
	s.mu.RUnlock()

	enc, err := s.codec.Marshal(value)
	if err != nil {
		return false, err
	}

	// to figure out if this was a create or update.
	// try INSERT: if conflict -> UPDATE.
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer func() { _ = rollbackIfNeeded(tx, &err) }()

	res, err := tx.Exec(setQuery, kind, key, enc)
	if err != nil {
		return false, err
	}
	createdRows, _ := res.RowsAffected()
	created := createdRows > 0

	if !created {
		// update only if bytes changed then bump version if changed
		var cur []byte
		row := tx.QueryRow(getQuery, kind, key)
		if err := row.Scan(&cur); err != nil {
			return false, err
		}
		if bytes.Equal(cur, enc) {
			// No-op
			if err = tx.Commit(); err != nil {
				return false, err
			}
			return false, nil
		}
		if _, err := tx.Exec(`
UPDATE zestor_kv
SET value=?, version=version+1, updated_at=STRFTIME('%Y-%m-%dT%H:%M:%fZ','now')
WHERE kind=? AND key=?;`, enc, kind, key); err != nil {
			return false, err
		}
	}

	if err = tx.Commit(); err != nil {
		return false, err
	}

	etype := store.EventTypeUpdate
	if created {
		etype = store.EventTypeCreate
	}
	s.publish(kind, &store.Event[T]{Kind: kind, Name: key, EventType: etype, Object: value})
	return created, nil
}

func (s *sqLiteStore[T]) SetFn(kind, key string, fn func(v T) (T, error)) (bool, error) {
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return false, store.ErrClosed
	}
	s.mu.RUnlock()

	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer func() { _ = rollbackIfNeeded(tx, &err) }()

	var cur T
	var curBytes []byte
	row := tx.QueryRow(getQuery, kind, key)
	scanErr := row.Scan(&curBytes)
	if errors.Is(scanErr, sql.ErrNoRows) {
		_ = tx.Rollback()
		return false, store.ErrKeyNotFound
	}
	if scanErr != nil {
		return false, scanErr
	}
	if err2 := s.codec.Unmarshal(curBytes, &cur); err2 != nil {
		return false, err2
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
		// no change
		if err = tx.Commit(); err != nil {
			return false, err
		}
		return false, nil
	}

	if _, err := tx.Exec(`
UPDATE zestor_kv
SET value=?, version=version+1, updated_at=STRFTIME('%Y-%m-%dT%H:%M:%fZ','now')
WHERE kind=? AND key=?;`, newBytes, kind, key); err != nil {
		return false, err
	}

	if err = tx.Commit(); err != nil {
		return false, err
	}

	s.publish(kind, &store.Event[T]{Kind: kind, Name: key, EventType: store.EventTypeUpdate, Object: nv})
	return false, nil
}

func (s *sqLiteStore[T]) SetAll(kind string, values map[string]T) error {
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return store.ErrClosed
	}
	s.mu.RUnlock()

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = rollbackIfNeeded(tx, &err) }()

	// check which keys already exist
	existingKeys := make(map[string]struct{})
	rows, err := tx.Query(`SELECT key FROM zestor_kv WHERE kind=?;`, kind)
	if err != nil {
		return err
	}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			rows.Close()
			return err
		}
		existingKeys[k] = struct{}{}
	}
	rows.Close()

	stmtIns, err := tx.Prepare(`
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

	// Track creates vs updates
	created := make(map[string]T)
	updated := make(map[string]T)
	for k, v := range values {
		enc, err := s.codec.Marshal(v)
		if err != nil {
			return err
		}
		if _, err := stmtIns.Exec(kind, k, enc); err != nil {
			return err
		}
		if _, existed := existingKeys[k]; existed {
			updated[k] = v
		} else {
			created[k] = v
		}
	}

	if err = tx.Commit(); err != nil {
		return err
	}

	// post-commit notifications with correct event types
	for k, v := range created {
		s.publish(kind, &store.Event[T]{Kind: kind, Name: k, EventType: store.EventTypeCreate, Object: v})
	}
	for k, v := range updated {
		s.publish(kind, &store.Event[T]{Kind: kind, Name: k, EventType: store.EventTypeUpdate, Object: v})
	}
	return nil
}

func (s *sqLiteStore[T]) Delete(kind, key string) (bool, T, error) {
	var zero T
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return false, zero, store.ErrClosed
	}
	s.mu.RUnlock()

	tx, err := s.db.Begin()
	if err != nil {
		return false, zero, err
	}
	defer func() { _ = rollbackIfNeeded(tx, &err) }()

	var prevBytes []byte
	row := tx.QueryRow(`SELECT value FROM zestor_kv WHERE kind=? AND key=?;`, kind, key)
	if err := row.Scan(&prevBytes); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			_ = tx.Rollback()
			return false, zero, nil
		}
		return false, zero, err
	}
	var prev T
	if err := s.codec.Unmarshal(prevBytes, &prev); err != nil {
		return false, zero, err
	}

	if _, err := tx.Exec(`DELETE FROM zestor_kv WHERE kind=? AND key=?;`, kind, key); err != nil {
		return false, zero, err
	}
	if err = tx.Commit(); err != nil {
		return false, zero, err
	}

	s.publish(kind, &store.Event[T]{Kind: kind, Name: key, EventType: store.EventTypeDelete, Object: prev})
	return true, prev, nil
}

func (s *sqLiteStore[T]) Watch(kind string, opts ...store.WatchOption[T]) (<-chan *store.Event[T], func(), error) {
	if kind == "" {
		return nil, nil, store.ErrKindRequired
	}

	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return nil, nil, store.ErrClosed
	}
	s.mu.RUnlock()

	cfg := &store.WatchCfg[T]{}
	for _, o := range opts {
		if o != nil {
			o(cfg)
		}
	}

	bufSize := cfg.BufferSize
	if bufSize <= 0 {
		bufSize = store.DefaultWatchBufferSize
	}

	w := &watcher[T]{
		ch:         make(chan *store.Event[T], bufSize),
		eventTypes: cfg.EventTypes,
	}

	s.muSubs.Lock()
	if s.subs[kind] == nil {
		s.subs[kind] = make(map[*watcher[T]]struct{})
	}
	s.subs[kind][w] = struct{}{}
	s.muSubs.Unlock()

	// initial replay (nil eventTypes means all events)
	sendInitial := cfg.EventTypes == nil
	if !sendInitial && cfg.EventTypes != nil {
		_, sendInitial = cfg.EventTypes[store.EventTypeCreate]
	}
	if cfg.Initial && sendInitial {
		go func() {
			m, err := s.List(kind)
			if err != nil {
				// TODO: channel is already returned
				return
			}
			for k, v := range m {
				select {
				case w.ch <- &store.Event[T]{Kind: kind, Name: k, EventType: store.EventTypeCreate, Object: v}:
				default:
					// buffer full, skip
				}
			}
		}()
	}

	cancel := func() {
		s.muSubs.Lock()
		defer s.muSubs.Unlock()
		if subs, ok := s.subs[kind]; ok {
			if _, exists := subs[w]; exists {
				delete(subs, w)
				if len(subs) == 0 {
					delete(s.subs, kind)
				}
				close(w.ch)
			}
		}
	}
	return w.ch, cancel, nil
}

func (s *sqLiteStore[T]) publish(kind string, ev *store.Event[T]) {
	s.muSubs.RLock()
	defer s.muSubs.RUnlock()
	for w := range s.subs[kind] {
		// check event type filter (nil means all events)
		if w.eventTypes != nil {
			if _, ok := w.eventTypes[ev.EventType]; !ok {
				continue
			}
		}
		select {
		case w.ch <- ev:
		default:
			// drop if slow consumer
		}
	}
}

func (s *sqLiteStore[T]) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()

	// close all watchers
	s.muSubs.Lock()
	for _, m := range s.subs {
		for w := range m {
			close(w.ch)
		}
	}
	s.subs = nil
	s.muSubs.Unlock()

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
		var kind, key, value, updated string
		var ver int
		if err := rows.Scan(&kind, &key, &value, &ver, &updated); err == nil {
			fmt.Fprintf(&sb, "%s/%s v%d (%dB) %s | value=%s\n", kind, key, ver, len(value), updated, string(value))
		}
	}
	return sb.String()
}

func (s *sqLiteStore[T]) GetAll() (map[string]map[string]T, error) {
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return nil, store.ErrClosed
	}
	s.mu.RUnlock()

	rows, err := s.db.Query(`SELECT kind, key, value FROM zestor_kv ORDER BY kind, key;`)
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

// defer helper
func rollbackIfNeeded(tx *sql.Tx, perr *error) error {
	if *perr != nil {
		_ = tx.Rollback()
	}
	return nil
}
