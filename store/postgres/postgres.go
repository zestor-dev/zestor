package postgres

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zestor-dev/zestor/codec"
	"github.com/zestor-dev/zestor/store"
	"github.com/zestor-dev/zestor/store/internal/watchhub"
)

const (
	defaultTimeout = 10 * time.Second

	// notifyChannel is the LISTEN/NOTIFY channel fed by zestor_notify_v2.
	notifyChannel = "zestor_events_v2"

	// schemaLockID serialises ensureSchema across processes. It is a
	// single-bigint advisory lock, a different space from the two-key lock
	// that serialises writes, so the two never block each other.
	schemaLockID int64 = 0x7a65737400000002
)

// listenIdleTimeout is how long the listener waits for a notification before
// looping. The wait ending is not a connection failure, and the session stays
// listening. Tests shorten it.
var listenIdleTimeout = 60 * time.Second

type Options struct {
	// Example: postgres://user:pass@host:5432/dbname?sslmode=disable
	ConnString string
	// Marshaling codec (json/yaml/etc).
	Codec codec.Codec
	// Optional namespace/tenant id. Defaults to 0.
	Namespace int64
	// Upper bound applied to each operation, on top of the caller's context.
	// Zero uses the default of 10s. A negative value means no cap: only the
	// caller's deadline applies, including to List, GetAll, and watch catch-up.
	Timeout time.Duration
	// OutboxRetention, when set, deletes outbox rows older than this age.
	// Rows this store has not yet consumed are kept. Zero leaves the outbox
	// untouched. Another process that is still behind the deleted rows will
	// miss them, so set this longer than any consumer's lag.
	OutboxRetention time.Duration
}

type storePG[T any] struct {
	pool        *pgxpool.Pool
	codec       codec.Codec
	ns          int64
	timeout     time.Duration
	retention   time.Duration
	validateFns map[string]store.ValidateFunc[T]
	// compareFn is nil unless the caller supplied one. Writes then decode the
	// stored value and let CompareFn decide; otherwise equality is the bytes.
	compareFn store.CompareFunc[T]

	// hub is fed by the drain loop, which reads the outbox in id order.
	// Publication therefore needs no extra write lock here; the hub lock alone
	// makes a Watch snapshot atomic with respect to publication.
	hub *watchhub.Hub[T]

	mu     sync.RWMutex
	closed bool
	// inflight counts operations that passed guard. Close waits for it before
	// closing the pool, so those operations never observe a closed pool.
	inflight sync.WaitGroup

	// cursorsMu guards the map of per-kind drain locks. The lock inside a
	// kindCursor serializes that kind's drain against a Watch of the same kind.
	// It is held across a whole batch — read, publish, advance the cursor —
	// and across Watch's catch-up plus subscribe. A drain that has read a
	// batch but not yet published it would otherwise deliver those events into
	// a subscription registered in the meantime.
	//
	// Kinds do not share this lock, so a slow Watch of one kind does not stall
	// delivery for the others. Lock order is kindCursor.mu -> hub lock.
	cursorsMu sync.Mutex
	cursors   map[string]*kindCursor

	// stateMu guards the per-kind drain bookkeeping only. It is separate from
	// the kind's drain lock so that a NOTIFY arriving during a long drain (or
	// during a Watch) can record itself without blocking the listen loop.
	stateMu  sync.Mutex
	draining map[string]*drainState
	// startID is where a kind's cursor begins when it is first seen: the outbox
	// high-water mark at the time this store opened. Starting from 0 instead
	// would make the first Watch replay the entire outbox history, which for an
	// unpruned table is both a flood and a query that outlives Options.Timeout.
	// Nothing written before this store existed can be owed to one of its
	// watchers.
	startID int64

	listenCancel context.CancelFunc
	listenWG     sync.WaitGroup

	// Closed after the first successful LISTEN so callers do not mutate before
	// NOTIFY delivery is possible (PostgreSQL drops NOTIFY if no session is listening yet).
	listenReady     chan struct{}
	listenReadyOnce sync.Once
}

// New opens a pool, ensures schema, starts the LISTEN loop, and returns a Store[T].
func New[T any](ctx context.Context, o Options) (store.Store[T], error) {
	return NewWithStoreOptions(ctx, o, store.StoreOptions[T]{})
}

// NewWithStoreOptions is New with per-kind validation and custom change
// detection. Without a CompareFn, a write is unchanged when its encoding is
// byte-for-byte equal to the stored one. With one, the stored value is decoded
// and CompareFn decides, which costs a decode per write to an existing key.
func NewWithStoreOptions[T any](ctx context.Context, o Options, so store.StoreOptions[T]) (store.Store[T], error) {
	if o.ConnString == "" {
		return nil, errors.New("postgres: ConnString required")
	}
	if o.Codec == nil {
		return nil, errors.New("postgres: Codec required")
	}

	cfg, err := pgxpool.ParseConfig(o.ConnString)
	if err != nil {
		return nil, err
	}
	if o.Timeout == 0 {
		o.Timeout = defaultTimeout
	}
	dialCtx, cancelDial := boundContext(ctx, o.Timeout)
	defer cancelDial()
	pool, err := pgxpool.NewWithConfig(dialCtx, cfg)
	if err != nil {
		return nil, err
	}

	// The listen loop outlives the constructor's context on purpose: it is torn
	// down by Close, not by whatever context happened to open the store.
	listenCtx, listenCancel := context.WithCancel(context.Background())
	s := &storePG[T]{
		pool:         pool,
		codec:        o.Codec,
		ns:           o.Namespace,
		timeout:      o.Timeout,
		retention:    o.OutboxRetention,
		validateFns:  maps.Clone(so.ValidateFns),
		compareFn:    so.CompareFn,
		hub:          watchhub.New[T](),
		cursors:      make(map[string]*kindCursor),
		draining:     make(map[string]*drainState),
		listenCancel: listenCancel,
		listenReady:  make(chan struct{}),
	}
	if err := s.ensureSchema(ctx); err != nil {
		listenCancel()
		pool.Close()
		return nil, err
	}
	if err := s.seedOutboxCursor(ctx); err != nil {
		listenCancel()
		pool.Close()
		return nil, err
	}

	s.listenWG.Add(1)
	go func() {
		defer s.listenWG.Done()
		s.listenLoop(listenCtx)
	}()
	if s.retention > 0 {
		s.listenWG.Add(1)
		go func() {
			defer s.listenWG.Done()
			s.pruneLoop(listenCtx)
		}()
	}

	waitCtx, waitCancel := boundContext(ctx, o.Timeout)
	defer waitCancel()
	select {
	case <-s.listenReady:
	case <-waitCtx.Done():
		listenCancel()
		s.listenWG.Wait()
		pool.Close()
		if o.Timeout > 0 {
			return nil, fmt.Errorf("postgres: listen loop did not become ready within %v", o.Timeout)
		}
		return nil, fmt.Errorf("postgres: listen loop did not become ready: %w", ctx.Err())
	}

	return s, nil
}

// pruneLoop deletes consumed outbox rows older than OutboxRetention until ctx
// is cancelled. The first wait is one interval, which is the retention itself
// capped at a minute.
func (s *storePG[T]) pruneLoop(ctx context.Context) {
	interval := s.retention
	if interval > time.Minute {
		interval = time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = s.pruneOutbox(ctx)
		}
	}
}

// pruneOutbox removes rows older than the retention that this store has
// already moved its cursor past, plus rows that predate the store entirely.
func (s *storePG[T]) pruneOutbox(ctx context.Context) error {
	if s.retention <= 0 {
		return nil
	}
	ctx, cancel := s.bound(ctx)
	defer cancel()
	cutoff := time.Now().Add(-s.retention)

	if _, err := s.pool.Exec(ctx, `
DELETE FROM zestor_outbox
 WHERE ns_id = $1 AND at < $2 AND id <= $3`, s.ns, cutoff, s.startID); err != nil {
		return err
	}

	s.cursorsMu.Lock()
	type consumed struct {
		kind string
		id   int64
	}
	var ahead []consumed
	for kind, c := range s.cursors {
		c.mu.Lock()
		if c.seen && c.lastID > s.startID {
			ahead = append(ahead, consumed{kind, c.lastID})
		}
		c.mu.Unlock()
	}
	s.cursorsMu.Unlock()

	for _, c := range ahead {
		if _, err := s.pool.Exec(ctx, `
DELETE FROM zestor_outbox
 WHERE ns_id = $1 AND kind = $2 AND at < $3 AND id <= $4`,
			s.ns, c.kind, cutoff, c.id); err != nil {
			return err
		}
	}
	return nil
}

// seedOutboxCursor records the outbox high-water mark at open time.
func (s *storePG[T]) seedOutboxCursor(ctx context.Context) error {
	ctx, cancel := s.bound(ctx)
	defer cancel()
	var maxID int64
	if err := s.pool.QueryRow(ctx,
		`SELECT COALESCE(MAX(id), 0) FROM zestor_outbox WHERE ns_id=$1`, s.ns).Scan(&maxID); err != nil {
		return err
	}
	// Written before the listen goroutine starts, and never written again.
	s.startID = maxID
	return nil
}

// unmarshalAs decodes data into dest. When the store's type is already a
// pointer, &v is a pointer to a nil pointer. The codec module these builds
// require does not allocate that pointer for protobuf, so it is allocated
// here and Unmarshal receives the value itself.
func unmarshalAs[T any](c codec.Codec, data []byte, dest *T) error {
	rv := reflect.ValueOf(dest).Elem()
	if rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			rv.Set(reflect.New(rv.Type().Elem()))
		}
		return c.Unmarshal(data, rv.Interface())
	}
	return c.Unmarshal(data, dest)
}

func (s *storePG[T]) validate(kind string, v T) error {
	if fn, ok := s.validateFns[kind]; ok {
		return fn(v)
	}
	return nil
}

// unchanged reports whether writing next over the stored encoding cur would
// change nothing. CompareFn sees the decoded stored value.
func (s *storePG[T]) unchanged(cur, enc []byte, next T) (bool, error) {
	if s.compareFn == nil {
		return bytes.Equal(cur, enc), nil
	}
	var prev T
	if err := unmarshalAs(s.codec, cur, &prev); err != nil {
		return false, err
	}
	return s.compareFn(prev, next), nil
}

// kindCursor is one kind's drain lock and outbox cursor.
type kindCursor struct {
	mu     sync.Mutex
	lastID int64
	seen   bool
}

func (s *storePG[T]) cursor(kind string) *kindCursor {
	s.cursorsMu.Lock()
	defer s.cursorsMu.Unlock()
	c := s.cursors[kind]
	if c == nil {
		c = &kindCursor{}
		s.cursors[kind] = c
	}
	return c
}

// bound applies Options.Timeout on top of ctx. A negative timeout means no
// cap: the caller's deadline is the only one.
func (s *storePG[T]) bound(ctx context.Context) (context.Context, context.CancelFunc) {
	return boundContext(ctx, s.timeout)
}

func boundContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout < 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, timeout)
}

// guard rejects operations on a closed store, honours an already-cancelled
// context, and applies Options.Timeout as an upper bound on top of the caller's
// deadline. A negative timeout does not add a deadline.
func (s *storePG[T]) guard(ctx context.Context) (context.Context, context.CancelFunc, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, nil, store.ErrClosed
	}
	s.inflight.Add(1)
	s.mu.Unlock()

	opCtx, cancel := s.bound(ctx)
	var once sync.Once
	return opCtx, func() {
		once.Do(func() {
			cancel()
			s.inflight.Done()
		})
	}, nil
}

func requireKind(kind string) error {
	if kind == "" {
		return store.ErrKindRequired
	}
	return nil
}

func (s *storePG[T]) guardKind(ctx context.Context, kind string) (context.Context, context.CancelFunc, error) {
	if err := requireKind(kind); err != nil {
		return nil, nil, err
	}
	return s.guard(ctx)
}

// schemaCurrentQuery is true when the tables, indexes, notify function, and
// v2 trigger are present and the legacy trigger is gone. It reads catalogs
// only, so it does not lock zestor_kv or zestor_outbox.
const schemaCurrentQuery = `
SELECT
    to_regclass('zestor_kv') IS NOT NULL
    AND to_regclass('zestor_outbox') IS NOT NULL
    AND EXISTS (
        SELECT 1 FROM pg_catalog.pg_indexes
        WHERE schemaname = current_schema() AND indexname = 'idx_kv_kind'
    )
    AND EXISTS (
        SELECT 1 FROM pg_catalog.pg_indexes
        WHERE schemaname = current_schema() AND indexname = 'idx_outbox_kind'
    )
    AND EXISTS (
        SELECT 1
        FROM pg_catalog.pg_proc p
        JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace
        WHERE n.nspname = current_schema() AND p.proname = 'zestor_notify_v2'
    )
    AND EXISTS (
        SELECT 1 FROM pg_catalog.pg_trigger
        WHERE NOT tgisinternal
          AND tgname = 'trg_zestor_outbox_notify_v2'
          AND tgrelid = to_regclass('zestor_outbox')
    )
    AND NOT EXISTS (
        SELECT 1 FROM pg_catalog.pg_trigger
        WHERE NOT tgisinternal
          AND tgname = 'trg_zestor_outbox_notify'
          AND tgrelid = to_regclass('zestor_outbox')
    )`

type rowQueryer interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func schemaCurrent(ctx context.Context, q rowQueryer) (bool, error) {
	var current bool
	if err := q.QueryRow(ctx, schemaCurrentQuery).Scan(&current); err != nil {
		return false, err
	}
	return current, nil
}

func (s *storePG[T]) ensureSchema(ctx context.Context) error {
	ctx, cancel := s.bound(ctx)
	defer cancel()

	// Opening a store must not take locks that conflict with writers once the
	// schema is in place. CREATE TABLE IF NOT EXISTS and CREATE INDEX IF NOT
	// EXISTS lock zestor_kv and zestor_outbox even when the objects already
	// exist, and a write that already holds those row locks deadlocks with
	// that (SQLSTATE 40P01). A current schema is a catalog read.
	current, err := schemaCurrent(ctx, s.pool)
	if err != nil {
		return err
	}
	if current {
		return nil
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	// First, before any table lock. Two stores creating the schema together
	// used to race on CREATE TABLE, and rebuilding the trigger on every start
	// took an exclusive lock that blocked writers for the whole statement.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, schemaLockID); err != nil {
		return err
	}
	current, err = schemaCurrent(ctx, tx)
	if err != nil {
		return err
	}
	if current {
		return tx.Commit(ctx)
	}

	stmts := []string{
		`CREATE TABLE IF NOT EXISTS zestor_kv (
		   ns_id      BIGINT  NOT NULL DEFAULT 0,
		   kind       TEXT    NOT NULL,
		   key        TEXT    NOT NULL,
		   value      BYTEA   NOT NULL,
		   version    BIGINT  NOT NULL DEFAULT 1,
		   updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		   PRIMARY KEY (ns_id, kind, key)
		 );`,
		`CREATE INDEX IF NOT EXISTS idx_kv_kind ON zestor_kv(ns_id, kind);`,

		`CREATE TABLE IF NOT EXISTS zestor_outbox (
		   id     BIGSERIAL PRIMARY KEY,
		   ns_id  BIGINT NOT NULL,
		   kind   TEXT   NOT NULL,
		   key        TEXT    NOT NULL,
		   etype  TEXT   NOT NULL CHECK (etype IN ('create','update','delete')),
		   value  BYTEA,
		   at     TIMESTAMPTZ NOT NULL DEFAULT now()
		 );`,
		`CREATE INDEX IF NOT EXISTS idx_outbox_kind ON zestor_outbox(ns_id, kind, id);`,

		// The payload carries only what the listener needs to pick a drain, kind
		// last so it may contain anything. This runs only when the schema is
		// not current: replacing the function locks it while a writer is inside
		// the trigger. A body change needs a new function name so the catalog
		// check fails and this path installs it. The trigger is created only
		// when it is missing: dropping it on every start takes an exclusive
		// lock and blocks writers.
		`CREATE OR REPLACE FUNCTION zestor_notify_v2() RETURNS TRIGGER AS $$
		   BEGIN
		     PERFORM pg_notify('` + notifyChannel + `', NEW.ns_id::text || '|' || NEW.kind);
		     RETURN NEW;
		   END; $$ LANGUAGE plpgsql;`,
	}
	for _, q := range stmts {
		if _, err := tx.Exec(ctx, q); err != nil {
			return err
		}
	}

	names, err := triggerNames(ctx, tx)
	if err != nil {
		return err
	}
	if _, ok := names["trg_zestor_outbox_notify_v2"]; !ok {
		if _, err := tx.Exec(ctx, `
CREATE TRIGGER trg_zestor_outbox_notify_v2
   AFTER INSERT ON zestor_outbox
   FOR EACH ROW EXECUTE PROCEDURE zestor_notify_v2()`); err != nil {
			return err
		}
	}
	// The unversioned trigger still fails writes whose key is long, so drop it
	// when an older process left it behind. Absent, this is a no-op and takes
	// no lock.
	if _, ok := names["trg_zestor_outbox_notify"]; ok {
		if _, err := tx.Exec(ctx, `DROP TRIGGER trg_zestor_outbox_notify ON zestor_outbox`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DROP FUNCTION IF EXISTS zestor_notify()`); err != nil {
			return err
		}
	}

	return tx.Commit(ctx)
}

func triggerNames(ctx context.Context, tx pgx.Tx) (map[string]struct{}, error) {
	rows, err := tx.Query(ctx, `
SELECT tgname FROM pg_trigger
 WHERE tgrelid = to_regclass('zestor_outbox') AND NOT tgisinternal`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	names := map[string]struct{}{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names[name] = struct{}{}
	}
	return names, rows.Err()
}

func (s *storePG[T]) Get(ctx context.Context, kind, key string) (T, bool, error) {
	var zero T
	ctx, cancel, err := s.guardKind(ctx, kind)
	if err != nil {
		return zero, false, err
	}
	defer cancel()

	var b []byte
	err = s.pool.QueryRow(ctx,
		`SELECT value FROM zestor_kv WHERE ns_id=$1 AND kind=$2 AND key=$3`,
		s.ns, kind, key).Scan(&b)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return zero, false, nil
		}
		return zero, false, err
	}
	var v T
	if err := unmarshalAs(s.codec, b, &v); err != nil {
		return zero, false, err
	}
	return v, true, nil
}

func (s *storePG[T]) List(ctx context.Context, kind string, filter ...store.FilterFunc[T]) (map[string]T, error) {
	ctx, cancel, err := s.guardKind(ctx, kind)
	if err != nil {
		return nil, err
	}
	defer cancel()

	rows, err := s.pool.Query(ctx,
		`SELECT key, value FROM zestor_kv WHERE ns_id=$1 AND kind=$2`, s.ns, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]T)
	for rows.Next() {
		var k string
		var b []byte
		if err := rows.Scan(&k, &b); err != nil {
			return nil, err
		}
		var v T
		if err := unmarshalAs(s.codec, b, &v); err != nil {
			return nil, err
		}
		ok := true
		for _, f := range filter {
			if f != nil && !f(k, v) {
				ok = false
				break
			}
		}
		if ok {
			out[k] = v
		}
	}
	return out, rows.Err()
}

func (s *storePG[T]) Count(ctx context.Context, kind string) (int, error) {
	ctx, cancel, err := s.guardKind(ctx, kind)
	if err != nil {
		return 0, err
	}
	defer cancel()

	var n int
	err = s.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM zestor_kv WHERE ns_id=$1 AND kind=$2`, s.ns, kind).Scan(&n)
	return n, err
}

func (s *storePG[T]) Keys(ctx context.Context, kind string) ([]string, error) {
	ctx, cancel, err := s.guardKind(ctx, kind)
	if err != nil {
		return nil, err
	}
	defer cancel()

	rows, err := s.pool.Query(ctx,
		`SELECT key FROM zestor_kv WHERE ns_id=$1 AND kind=$2`, s.ns, kind)
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

func (s *storePG[T]) Values(ctx context.Context, kind string) ([]store.KeyValue[T], error) {
	ctx, cancel, err := s.guardKind(ctx, kind)
	if err != nil {
		return nil, err
	}
	defer cancel()

	rows, err := s.pool.Query(ctx,
		`SELECT key, value FROM zestor_kv WHERE ns_id=$1 AND kind=$2`, s.ns, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]store.KeyValue[T], 0)
	for rows.Next() {
		var k string
		var b []byte
		if err := rows.Scan(&k, &b); err != nil {
			return nil, err
		}
		var v T
		if err := unmarshalAs(s.codec, b, &v); err != nil {
			return nil, err
		}
		out = append(out, store.KeyValue[T]{Key: k, Value: v})
	}
	return out, rows.Err()
}

func (s *storePG[T]) GetAll(ctx context.Context) (map[string]map[string]T, error) {
	ctx, cancel, err := s.guard(ctx)
	if err != nil {
		return nil, err
	}
	defer cancel()

	rows, err := s.pool.Query(ctx,
		`SELECT kind, key, value FROM zestor_kv WHERE ns_id=$1 ORDER BY kind, key`, s.ns)
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
		if err := unmarshalAs(s.codec, blob, &v); err != nil {
			return nil, err
		}
		if _, ok := out[kind]; !ok {
			out[kind] = make(map[string]T)
		}
		out[kind][key] = v
	}
	return out, rows.Err()
}

var _ store.Pager[struct{}] = (*storePG[struct{}])(nil)

// Page queries compare and order bytewise, matching Go string order and the
// fallback sort a remote handler uses for a store without Pager. "C" is a
// built-in collation, so this does not depend on the database default.
const (
	listPageQuery = `
SELECT key, value FROM zestor_kv
 WHERE ns_id=$1 AND kind=$2
 ORDER BY key COLLATE "C"
 LIMIT $3`
	listPageAfterQuery = `
SELECT key, value FROM zestor_kv
 WHERE ns_id=$1 AND kind=$2 AND key COLLATE "C" > $3
 ORDER BY key COLLATE "C"
 LIMIT $4`
	keysPageQuery = `
SELECT key FROM zestor_kv
 WHERE ns_id=$1 AND kind=$2
 ORDER BY key COLLATE "C"
 LIMIT $3`
	keysPageAfterQuery = `
SELECT key FROM zestor_kv
 WHERE ns_id=$1 AND kind=$2 AND key COLLATE "C" > $3
 ORDER BY key COLLATE "C"
 LIMIT $4`
	allPageQuery = `
SELECT kind, key, value FROM zestor_kv
 WHERE ns_id=$1
 ORDER BY kind COLLATE "C", key COLLATE "C"
 LIMIT $2`
	allPageAfterQuery = `
SELECT kind, key, value FROM zestor_kv
 WHERE ns_id=$1 AND (kind COLLATE "C" > $2 OR (kind COLLATE "C" = $2 AND key COLLATE "C" > $3))
 ORDER BY kind COLLATE "C", key COLLATE "C"
 LIMIT $4`
)

func (s *storePG[T]) ListPage(ctx context.Context, kind string, after *string, limit int) ([]store.KeyValue[T], error) {
	ctx, cancel, err := s.guardKind(ctx, kind)
	if err != nil {
		return nil, err
	}
	defer cancel()
	if limit < 1 {
		return nil, store.ErrPageLimit
	}

	var rows pgx.Rows
	if after == nil {
		rows, err = s.pool.Query(ctx, listPageQuery, s.ns, kind, limit)
	} else {
		rows, err = s.pool.Query(ctx, listPageAfterQuery, s.ns, kind, *after, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]store.KeyValue[T], 0, limit)
	for rows.Next() {
		var k string
		var b []byte
		if err := rows.Scan(&k, &b); err != nil {
			return nil, err
		}
		var v T
		if err := unmarshalAs(s.codec, b, &v); err != nil {
			return nil, err
		}
		out = append(out, store.KeyValue[T]{Key: k, Value: v})
	}
	return out, rows.Err()
}

func (s *storePG[T]) KeysPage(ctx context.Context, kind string, after *string, limit int) ([]string, error) {
	ctx, cancel, err := s.guardKind(ctx, kind)
	if err != nil {
		return nil, err
	}
	defer cancel()
	if limit < 1 {
		return nil, store.ErrPageLimit
	}

	var rows pgx.Rows
	if after == nil {
		rows, err = s.pool.Query(ctx, keysPageQuery, s.ns, kind, limit)
	} else {
		rows, err = s.pool.Query(ctx, keysPageAfterQuery, s.ns, kind, *after, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]string, 0, limit)
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (s *storePG[T]) GetAllPage(ctx context.Context, after *store.PageCursor, limit int) ([]store.KindValue[T], error) {
	ctx, cancel, err := s.guard(ctx)
	if err != nil {
		return nil, err
	}
	defer cancel()
	if limit < 1 {
		return nil, store.ErrPageLimit
	}

	var rows pgx.Rows
	if after == nil {
		rows, err = s.pool.Query(ctx, allPageQuery, s.ns, limit)
	} else {
		rows, err = s.pool.Query(ctx, allPageAfterQuery, s.ns, after.Kind, after.Key, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]store.KindValue[T], 0, limit)
	for rows.Next() {
		var kind, key string
		var b []byte
		if err := rows.Scan(&kind, &key, &b); err != nil {
			return nil, err
		}
		var v T
		if err := unmarshalAs(s.codec, b, &v); err != nil {
			return nil, err
		}
		out = append(out, store.KindValue[T]{Kind: kind, Key: key, Value: v})
	}
	return out, rows.Err()
}

func (s *storePG[T]) Set(ctx context.Context, kind, key string, value T) (store.SetResult, error) {
	ctx, cancel, err := s.guardKind(ctx, kind)
	if err != nil {
		return store.SetUnchanged, err
	}
	defer cancel()
	if err := s.validate(kind, value); err != nil {
		return store.SetUnchanged, err
	}

	enc, err := s.codec.Marshal(value)
	if err != nil {
		return store.SetUnchanged, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return store.SetUnchanged, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.lockOutbox(ctx, tx); err != nil {
		return store.SetUnchanged, err
	}
	if s.compareFn != nil {
		return s.setCompared(ctx, tx, kind, key, value, enc)
	}

	// One statement, so a delete cannot remove the row between the insert and
	// the read that used to follow it. DO NOTHING does not lock the existing
	// row; this update does. No row comes back when the stored bytes are
	// already equal, and version starts at 1, so etype = 'create' means the
	// key was inserted rather than updated.
	var created bool
	err = tx.QueryRow(ctx, `
WITH upsert AS (
    INSERT INTO zestor_kv(ns_id, kind, key, value)
    VALUES ($1, $2, $3, $4)
    ON CONFLICT (ns_id, kind, key) DO UPDATE
        SET value = EXCLUDED.value,
            version = zestor_kv.version + 1,
            updated_at = now()
        WHERE zestor_kv.value IS DISTINCT FROM EXCLUDED.value
    RETURNING kind, key, value, (version = 1) AS created
)
INSERT INTO zestor_outbox(ns_id, kind, key, etype, value)
SELECT $1, kind, key, CASE WHEN created THEN 'create' ELSE 'update' END, value
FROM upsert
RETURNING (etype = 'create')
`, s.ns, kind, key, enc).Scan(&created)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := tx.Commit(ctx); err != nil {
			return store.SetUnchanged, err
		}
		return store.SetUnchanged, nil
	}
	if err != nil {
		return store.SetUnchanged, err
	}
	if err := tx.Commit(ctx); err != nil {
		return store.SetUnchanged, err
	}
	if created {
		return store.SetCreated, nil
	}
	return store.SetUpdated, nil
}

// setCompared applies CompareFn. Byte equality is the wrong test here: a value
// can compare equal with different bytes, and that must not be written.
func (s *storePG[T]) setCompared(ctx context.Context, tx pgx.Tx, kind, key string, value T, enc []byte) (store.SetResult, error) {
	var cur []byte
	err := tx.QueryRow(ctx, `
SELECT value FROM zestor_kv
 WHERE ns_id=$1 AND kind=$2 AND key=$3 FOR UPDATE`,
		s.ns, kind, key).Scan(&cur)
	if errors.Is(err, pgx.ErrNoRows) {
		if _, err := tx.Exec(ctx, `
INSERT INTO zestor_kv(ns_id, kind, key, value) VALUES ($1,$2,$3,$4)`,
			s.ns, kind, key, enc); err != nil {
			return store.SetUnchanged, err
		}
		if err := s.appendOutbox(ctx, tx, kind, key, store.EventTypeCreate, enc); err != nil {
			return store.SetUnchanged, err
		}
		if err := tx.Commit(ctx); err != nil {
			return store.SetUnchanged, err
		}
		return store.SetCreated, nil
	}
	if err != nil {
		return store.SetUnchanged, err
	}
	same, err := s.unchanged(cur, enc, value)
	if err != nil {
		return store.SetUnchanged, err
	}
	if same {
		if err := tx.Commit(ctx); err != nil {
			return store.SetUnchanged, err
		}
		return store.SetUnchanged, nil
	}
	if _, err := tx.Exec(ctx, `
UPDATE zestor_kv
   SET value=$4, version=version+1, updated_at=now()
 WHERE ns_id=$1 AND kind=$2 AND key=$3`,
		s.ns, kind, key, enc); err != nil {
		return store.SetUnchanged, err
	}
	if err := s.appendOutbox(ctx, tx, kind, key, store.EventTypeUpdate, enc); err != nil {
		return store.SetUnchanged, err
	}
	if err := tx.Commit(ctx); err != nil {
		return store.SetUnchanged, err
	}
	return store.SetUpdated, nil
}

func (s *storePG[T]) SetFn(ctx context.Context, kind, key string, fn func(v T) (T, error)) (bool, error) {
	ctx, cancel, err := s.guardKind(ctx, kind)
	if err != nil {
		return false, err
	}
	defer cancel()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.lockOutbox(ctx, tx); err != nil {
		return false, err
	}

	var curBytes []byte
	var cur T
	err = tx.QueryRow(ctx,
		`SELECT value FROM zestor_kv WHERE ns_id=$1 AND kind=$2 AND key=$3 FOR UPDATE`,
		s.ns, kind, key).Scan(&curBytes)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, store.ErrKeyNotFound
	}
	if err != nil {
		return false, err
	}
	if err := unmarshalAs(s.codec, curBytes, &cur); err != nil {
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
		if err := tx.Commit(ctx); err != nil {
			return false, err
		}
		return false, nil
	}

	if _, err := tx.Exec(ctx,
		`UPDATE zestor_kv SET value=$4, version=version+1, updated_at=now()
              WHERE ns_id=$1 AND kind=$2 AND key=$3`,
		s.ns, kind, key, newBytes); err != nil {
		return false, err
	}

	if err := s.appendOutbox(ctx, tx, kind, key, store.EventTypeUpdate, newBytes); err != nil {
		return false, err
	}

	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// setManyStmt upserts one key and, only when the stored bytes change, appends
// the matching outbox row. Version starts at 1, so a returned version of 1 is
// a create. Unchanged keys are not sent here: the caller compares first.
const setManyStmt = `
WITH upsert AS (
    INSERT INTO zestor_kv(ns_id, kind, key, value)
    VALUES ($1, $2, $3, $4)
    ON CONFLICT (ns_id, kind, key) DO UPDATE
        SET value = EXCLUDED.value,
            version = zestor_kv.version + 1,
            updated_at = now()
        WHERE zestor_kv.value IS DISTINCT FROM EXCLUDED.value
    RETURNING kind, key, value, (version = 1) AS created
)
INSERT INTO zestor_outbox(ns_id, kind, key, etype, value)
SELECT $1, kind, key, CASE WHEN created THEN 'create' ELSE 'update' END, value
FROM upsert`

// setManyForceStmt is setManyStmt without the byte guard. CompareFn has already
// decided the value changed, which can be true even when the bytes match.
const setManyForceStmt = `
WITH upsert AS (
    INSERT INTO zestor_kv(ns_id, kind, key, value)
    VALUES ($1, $2, $3, $4)
    ON CONFLICT (ns_id, kind, key) DO UPDATE
        SET value = EXCLUDED.value,
            version = zestor_kv.version + 1,
            updated_at = now()
    RETURNING kind, key, value, (version = 1) AS created
)
INSERT INTO zestor_outbox(ns_id, kind, key, etype, value)
SELECT $1, kind, key, CASE WHEN created THEN 'create' ELSE 'update' END, value
FROM upsert`

func (s *storePG[T]) SetMany(ctx context.Context, kind string, values map[string]T) error {
	ctx, cancel, err := s.guardKind(ctx, kind)
	if err != nil {
		return err
	}
	defer cancel()
	if len(values) == 0 {
		return nil
	}
	keys := slices.Sorted(maps.Keys(values))
	for _, k := range keys {
		if err := s.validate(kind, values[k]); err != nil {
			return err
		}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.lockOutbox(ctx, tx); err != nil {
		return err
	}

	// Only the keys being written. A 3-key write must not read the whole kind.
	existing, err := s.existingValues(ctx, tx, kind, keys)
	if err != nil {
		return err
	}

	// Key order, so the outbox and the event stream stay deterministic.
	// One statement per real change, sent together.
	batch := &pgx.Batch{}
	for _, k := range keys {
		b, err := s.codec.Marshal(values[k])
		if err != nil {
			return err
		}
		if prev, existed := existing[k]; existed {
			same, err := s.unchanged(prev, b, values[k])
			if err != nil {
				return err
			}
			if same {
				continue
			}
		}
		stmt := setManyStmt
		if s.compareFn != nil {
			stmt = setManyForceStmt
		}
		batch.Queue(stmt, s.ns, kind, k, b)
	}
	if batch.Len() == 0 {
		return tx.Commit(ctx)
	}
	results := tx.SendBatch(ctx, batch)
	var batchErr error
	for range batch.Len() {
		if _, err := results.Exec(); err != nil {
			batchErr = err
			break
		}
	}
	if err := results.Close(); batchErr == nil {
		batchErr = err
	}
	if batchErr != nil {
		return batchErr
	}
	return tx.Commit(ctx)
}

func (s *storePG[T]) Delete(ctx context.Context, kind, key string) (bool, T, error) {
	var zero T
	ctx, cancel, err := s.guardKind(ctx, kind)
	if err != nil {
		return false, zero, err
	}
	defer cancel()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, zero, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := s.lockOutbox(ctx, tx); err != nil {
		return false, zero, err
	}

	var prevBytes []byte
	err = tx.QueryRow(ctx,
		`SELECT value FROM zestor_kv WHERE ns_id=$1 AND kind=$2 AND key=$3`,
		s.ns, kind, key).Scan(&prevBytes)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, zero, nil
		}
		return false, zero, err
	}
	var prev T
	if err := unmarshalAs(s.codec, prevBytes, &prev); err != nil {
		return false, zero, err
	}

	if _, err := tx.Exec(ctx,
		`DELETE FROM zestor_kv WHERE ns_id=$1 AND kind=$2 AND key=$3`,
		s.ns, kind, key); err != nil {
		return false, zero, err
	}
	if err := s.appendOutbox(ctx, tx, kind, key, store.EventTypeDelete, prevBytes); err != nil {
		return false, zero, err
	}

	if err := tx.Commit(ctx); err != nil {
		return false, zero, err
	}
	return true, prev, nil
}

// outboxLockClass is the first key of the two-key advisory lock taken by
// lockOutbox. PostgreSQL keeps the two-key and single-bigint advisory lock
// spaces separate, so this cannot collide with an application's bigint locks.
const outboxLockClass int32 = 0x7a657374 // "zest"

// lockOutbox serialises the write transactions of one namespace, across every
// process sharing the database. Outbox ids come from a sequence, which hands
// them out at INSERT time rather than at commit; without this lock a
// transaction holding a lower id can commit after the drain has already moved
// its cursor past a higher one, and its event is never delivered. The lock is
// released only after the commit is visible, so ids become visible in order.
//
// It must be the first statement of every write transaction. Taking it after a
// row lock would let two writers each hold what the other is waiting for.
func (s *storePG[T]) lockOutbox(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1, $2)`,
		outboxLockClass, int32(s.ns^(s.ns>>32)))
	return err
}

func (s *storePG[T]) appendOutbox(ctx context.Context, tx pgx.Tx, kind, key string, etype store.EventType, value []byte) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO zestor_outbox(ns_id,kind,key,etype,value) VALUES($1,$2,$3,$4,$5)`,
		s.ns, kind, key, string(etype), value)
	return err
}

// existingValues returns the stored encoding of the requested keys. Missing
// keys are absent from the map.
func (s *storePG[T]) existingValues(ctx context.Context, tx pgx.Tx, kind string, keys []string) (map[string][]byte, error) {
	rows, err := tx.Query(ctx, `
SELECT key, value FROM zestor_kv
 WHERE ns_id=$1 AND kind=$2 AND key = ANY($3)`, s.ns, kind, keys)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string][]byte, len(keys))
	for rows.Next() {
		var k string
		var v []byte
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

func (s *storePG[T]) Watch(ctx context.Context, kind string, opts ...store.WatchOption[T]) (<-chan *store.Event[T], error) {
	opCtx, cancel, err := s.guardKind(ctx, kind)
	if err != nil {
		return nil, err
	}
	defer cancel()

	cfg := &store.WatchCfg[T]{}
	for _, o := range opts {
		if o != nil {
			o(cfg)
		}
	}

	// Set returns as soon as its transaction commits, while the matching event
	// is still working its way through NOTIFY and the outbox drain. Subscribing
	// without first flushing that backlog hands the new watcher events for
	// writes that predate it — which is what the old test suite slept a second
	// to avoid.
	//
	// Holding this kind's drain lock across the catch-up and the subscribe is
	// what makes the flush stick: it locks out that kind's drain, including a
	// batch it has already read but not yet published. Other kinds are not
	// waiting on this lock.
	//
	// The watch itself is bound to ctx, not opCtx: Options.Timeout bounds the
	// catch-up and snapshot queries, not the lifetime of the watch.
	c := s.cursor(kind)
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := s.catchUpLocked(opCtx, c, kind); err != nil {
		return nil, err
	}

	return s.hub.Subscribe(ctx, kind, cfg, watchhub.Hooks[T]{
		Snapshot: func() ([]*store.Event[T], error) {
			return s.snapshot(opCtx, kind)
		},
	})
}

func (s *storePG[T]) snapshot(ctx context.Context, kind string) ([]*store.Event[T], error) {
	rows, err := s.pool.Query(ctx,
		`SELECT key, value FROM zestor_kv WHERE ns_id=$1 AND kind=$2 ORDER BY key`, s.ns, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*store.Event[T]
	for rows.Next() {
		var k string
		var b []byte
		if err := rows.Scan(&k, &b); err != nil {
			return nil, err
		}
		var v T
		if err := unmarshalAs(s.codec, b, &v); err != nil {
			return nil, err
		}
		out = append(out, &store.Event[T]{Kind: kind, Name: k, EventType: store.EventTypeCreate, Object: v})
	}
	return out, rows.Err()
}

const outboxBatch = 256

type drainState struct {
	// running is set while a drain goroutine is working on this kind. pending
	// records a NOTIFY that arrived while it was: without it, a notification
	// landing between the drain's last empty query and its exit is dropped, and
	// with no fallback poll those events wait for an unrelated later write.
	running bool
	pending bool
}

func (s *storePG[T]) signalListenReady() {
	s.listenReadyOnce.Do(func() {
		close(s.listenReady)
	})
}

// drainBatchLocked publishes one batch of outbox rows for kind and advances the
// cursor, reporting whether there was anything to read. The caller must hold
// c.mu for the whole call, which is what keeps a batch from being published
// into a subscription created while it was being read.
//
// The rows are fully collected and the connection released before publishing,
// so the hub lock is never held across I/O.
func (s *storePG[T]) drainBatchLocked(ctx context.Context, c *kindCursor, kind string) (bool, error) {
	cursor := s.startID
	if c.seen {
		cursor = c.lastID
	}

	qctx, qcancel := s.bound(ctx)
	rows, err := s.pool.Query(qctx, `
SELECT id, key, etype, value
  FROM zestor_outbox
 WHERE ns_id=$1 AND kind=$2 AND id > $3
 ORDER BY id
 LIMIT $4`, s.ns, kind, cursor, outboxBatch)
	if err != nil {
		qcancel()
		return false, err
	}

	var (
		evs       []*store.Event[T]
		highest   = cursor
		iterErr   error
		decodeErr error
	)
	for rows.Next() {
		var id int64
		var name, etype string
		var val []byte
		if err := rows.Scan(&id, &name, &etype, &val); err != nil {
			iterErr = err
			break
		}
		var obj T
		if err := unmarshalAs(s.codec, val, &obj); err != nil {
			// Skip the row. Leaving the cursor in front of it makes every
			// later drain retry it forever, and every later Watch fails.
			decodeErr = fmt.Errorf("%w: outbox row %d (%s/%s): %v", store.ErrDecode, id, kind, name, err)
			if id > highest {
				highest = id
			}
			break
		}
		evs = append(evs, &store.Event[T]{
			Kind:      kind,
			Name:      name,
			EventType: store.EventType(etype),
			Object:    obj,
		})
		if id > highest {
			highest = id
		}
	}
	if iterErr == nil {
		iterErr = rows.Err()
	}
	rows.Close()
	qcancel()
	if iterErr != nil {
		return false, iterErr
	}
	if len(evs) > 0 {
		s.hub.Publish(kind, evs...)
	}
	if highest > cursor {
		c.lastID = highest
		c.seen = true
	}
	if decodeErr != nil {
		// Events already queued are delivered first; then the watch ends.
		// The cursor is past the bad row, so a re-watch is not stuck on it.
		s.hub.Fail(kind, decodeErr)
		return true, nil
	}
	return len(evs) > 0, nil
}

// catchUpLocked drains kind to exhaustion. The caller must hold c.mu and keep
// holding it through Subscribe: on return, everything committed so far has been
// handed to the subscribers that already exist, and no drain of this kind can
// publish again until the lock is released.
func (s *storePG[T]) catchUpLocked(ctx context.Context, c *kindCursor, kind string) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		more, err := s.drainBatchLocked(ctx, c, kind)
		if err != nil {
			return err
		}
		if !more {
			return nil
		}
	}
}

func (s *storePG[T]) triggerDrain(ctx context.Context, kind string) {
	if s.hub.Watching(kind) {
		s.launchDrain(ctx, kind)
		return
	}
	// Nobody is watching, so decoding the rows would be wasted. Remember how
	// far the outbox got instead. A later Watch catches up from this cursor
	// and does not receive the writes that predate it. Recheck under the
	// kind's lock: a watcher can arrive while this waits, and that watcher
	// needs a real drain.
	c := s.cursor(kind)
	c.mu.Lock()
	watching := s.hub.Watching(kind)
	if !watching {
		s.advanceCursorLocked(ctx, c, kind)
	}
	c.mu.Unlock()
	if watching {
		s.launchDrain(ctx, kind)
	}
}

// advanceCursorLocked moves kind's cursor to the newest committed outbox row.
// The caller holds c.mu. Writers in a namespace commit in id order, so a
// committed max cannot leave an older row still in flight.
func (s *storePG[T]) advanceCursorLocked(ctx context.Context, c *kindCursor, kind string) {
	cursor := s.startID
	if c.seen {
		cursor = c.lastID
	}
	var maxID int64
	err := s.pool.QueryRow(ctx, `
SELECT COALESCE(MAX(id), $3)
  FROM zestor_outbox
 WHERE ns_id = $1 AND kind = $2 AND id > $3`, s.ns, kind, cursor).Scan(&maxID)
	if err != nil || maxID <= cursor {
		return
	}
	c.lastID = maxID
	c.seen = true
}

func (s *storePG[T]) launchDrain(ctx context.Context, kind string) {
	s.stateMu.Lock()
	st := s.draining[kind]
	if st == nil {
		st = &drainState{}
		s.draining[kind] = st
	}
	if st.running {
		st.pending = true
		s.stateMu.Unlock()
		return
	}
	st.running = true
	s.stateMu.Unlock()

	s.listenWG.Add(1)
	go func() {
		defer s.listenWG.Done()
		for {
			c := s.cursor(kind)
			for ctx.Err() == nil {
				c.mu.Lock()
				more, err := s.drainBatchLocked(ctx, c, kind)
				c.mu.Unlock()
				if err != nil {
					select {
					case <-ctx.Done():
					case <-time.After(200 * time.Millisecond):
					}
					continue
				}
				if !more {
					break
				}
			}

			s.stateMu.Lock()
			if st.pending && ctx.Err() == nil {
				// A NOTIFY landed while we were draining; go round again rather
				// than exiting and losing it.
				st.pending = false
				s.stateMu.Unlock()
				continue
			}
			st.running = false
			st.pending = false
			s.stateMu.Unlock()
			return
		}
	}()
}

func (s *storePG[T]) listenLoop(ctx context.Context) {
	backoff := 200 * time.Millisecond
	for ctx.Err() == nil {
		s.mu.RLock()
		closed := s.closed
		s.mu.RUnlock()
		if closed {
			return
		}

		acq, err := s.pool.Acquire(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			time.Sleep(backoff)
			backoff = min(backoff*2, 5*time.Second)
			continue
		}
		// The listener owns this session. Returning it to the pool on an idle
		// timeout lets the next Acquire hand out a different one, and a
		// notification sent to the old session is gone.
		conn := acq.Hijack()
		backoff = 200 * time.Millisecond
		err = s.serveListen(ctx, conn)
		conn.Close(context.Background())
		if err == nil || ctx.Err() != nil {
			return
		}
		time.Sleep(backoff)
		backoff = min(backoff*2, 5*time.Second)
	}
}

// serveListen listens until ctx is cancelled or the connection fails. A nil
// return is a clean shutdown. A wait that simply expires is not a failure:
// the session is still listening.
func (s *storePG[T]) serveListen(ctx context.Context, conn *pgx.Conn) error {
	if _, err := conn.Exec(ctx, `UNLISTEN *`); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	if _, err := conn.Exec(ctx, `LISTEN `+notifyChannel); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	s.signalListenReady()
	// Anything committed while this session was not listening has no NOTIFY
	// waiting for it. The outbox still has the rows.
	for _, kind := range s.hub.Kinds() {
		s.triggerDrain(ctx, kind)
	}

	for {
		if ctx.Err() != nil {
			return nil
		}
		s.mu.RLock()
		closed := s.closed
		s.mu.RUnlock()
		if closed {
			return nil
		}

		waitCtx, cancel := context.WithTimeout(ctx, listenIdleTimeout)
		ntf, err := conn.WaitForNotification(waitCtx)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, context.DeadlineExceeded) {
				continue
			}
			return err
		}
		if ntf == nil {
			continue
		}
		ns, kind, perr := parsePayload(ntf.Payload)
		if perr != nil || ns != s.ns {
			continue
		}
		s.triggerDrain(ctx, kind)
	}
}

// parsePayload splits a zestor_notify_v2 payload, "<ns>|<kind>". The kind is
// everything after the first separator and may itself contain '|'.
func parsePayload(p string) (ns int64, kind string, err error) {
	nsStr, kind, ok := strings.Cut(p, "|")
	if !ok {
		return 0, "", errors.New("invalid payload")
	}
	ns, err = strconv.ParseInt(nsStr, 10, 64)
	if err != nil {
		return 0, "", err
	}
	return ns, kind, nil
}

func (s *storePG[T]) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()

	// Stop the drain before closing the hub, so no goroutine is mid-publish when
	// subscribers are torn down.
	if s.listenCancel != nil {
		s.listenCancel()
	}
	s.listenWG.Wait()
	// In-flight calls finish against an open pool and return their real result.
	s.inflight.Wait()
	s.hub.Close()
	s.pool.Close()
	return nil
}

// Dump implements store.Dumper.
func (s *storePG[T]) Dump(ctx context.Context, w io.Writer) error {
	ctx, cancel, err := s.guard(ctx)
	if err != nil {
		return err
	}
	defer cancel()

	rows, err := s.pool.Query(ctx,
		`SELECT kind, key, value, version, updated_at
           FROM zestor_kv
          WHERE ns_id=$1
       ORDER BY kind, key`, s.ns)
	if err != nil {
		return err
	}
	defer rows.Close()

	bw := bufio.NewWriter(w)
	for rows.Next() {
		var kind, key string
		var value []byte
		var ver int
		var ts time.Time
		if err := rows.Scan(&kind, &key, &value, &ver, &ts); err != nil {
			return err
		}
		updated := ts.UTC().Format(time.RFC3339Nano)
		var decoded T
		if err := unmarshalAs(s.codec, value, &decoded); err != nil {
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
