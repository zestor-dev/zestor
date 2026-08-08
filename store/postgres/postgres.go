package postgres

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
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
)

type Options struct {
	// Example: postgres://user:pass@host:5432/dbname?sslmode=disable
	ConnString string
	// Marshaling codec (json/yaml/etc).
	Codec codec.Codec
	// Optional namespace/tenant id. Defaults to 0.
	Namespace int64
	// Upper bound applied to each operation, on top of the caller's context.
	Timeout time.Duration
}

type storePG[T any] struct {
	pool    *pgxpool.Pool
	codec   codec.Codec
	ns      int64
	timeout time.Duration

	// hub is fed by the drain loop, which reads the outbox in id order.
	// Publication therefore needs no extra write lock here; the hub lock alone
	// makes a Watch snapshot atomic with respect to publication.
	hub *watchhub.Hub[T]

	mu     sync.RWMutex
	closed bool

	// drainMu guards the outbox cursor and the per-kind drain state. Watch's
	// CatchUp hook shares them with the background drain, so that "everything
	// committed so far has been published" is a state both can establish.
	drainMu  sync.Mutex
	lastID   map[string]int64
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
	dialCtx, cancelDial := context.WithTimeout(ctx, o.Timeout)
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
		hub:          watchhub.New[T](),
		lastID:       make(map[string]int64),
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

	waitCtx, waitCancel := context.WithTimeout(ctx, o.Timeout)
	defer waitCancel()
	select {
	case <-s.listenReady:
	case <-waitCtx.Done():
		listenCancel()
		s.listenWG.Wait()
		pool.Close()
		return nil, fmt.Errorf("postgres: listen loop did not become ready within %v", o.Timeout)
	}

	return s, nil
}

// seedOutboxCursor records the outbox high-water mark at open time.
func (s *storePG[T]) seedOutboxCursor(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	var maxID int64
	if err := s.pool.QueryRow(ctx,
		`SELECT COALESCE(MAX(id), 0) FROM zestor_outbox WHERE ns_id=$1`, s.ns).Scan(&maxID); err != nil {
		return err
	}
	s.drainMu.Lock()
	s.startID = maxID
	s.drainMu.Unlock()
	return nil
}

// guard rejects operations on a closed store, honours an already-cancelled
// context, and applies Options.Timeout as an upper bound on top of the caller's
// deadline.
func (s *storePG[T]) guard(ctx context.Context) (context.Context, context.CancelFunc, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	s.mu.RLock()
	closed := s.closed
	s.mu.RUnlock()
	if closed {
		return nil, nil, store.ErrClosed
	}
	opCtx, cancel := context.WithTimeout(ctx, s.timeout)
	return opCtx, cancel, nil
}

func (s *storePG[T]) ensureSchema(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()

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
		   key    TEXT   NOT NULL,
		   etype  TEXT   NOT NULL CHECK (etype IN ('create','update','delete')),
		   value  BYTEA,
		   at     TIMESTAMPTZ NOT NULL DEFAULT now()
		 );`,
		`CREATE INDEX IF NOT EXISTS idx_outbox_kind ON zestor_outbox(ns_id, kind, id);`,

		`CREATE OR REPLACE FUNCTION zestor_notify() RETURNS TRIGGER AS $$
		   DECLARE payload TEXT;
		   BEGIN
		     payload := NEW.ns_id::text || '|' || NEW.kind || '|' || NEW.key || '|' || NEW.etype || '|' || NEW.id::text;
		     PERFORM pg_notify('zestor_events', payload);
		     RETURN NEW;
		   END; $$ LANGUAGE plpgsql;`,

		`DROP TRIGGER IF EXISTS trg_zestor_outbox_notify ON zestor_outbox;`,
		`CREATE TRIGGER trg_zestor_outbox_notify
		   AFTER INSERT ON zestor_outbox
		   FOR EACH ROW EXECUTE PROCEDURE zestor_notify();`,
	}

	for _, q := range stmts {
		if _, err := tx.Exec(ctx, q); err != nil {
			return err
		}
	}

	return tx.Commit(ctx)
}

func (s *storePG[T]) Get(ctx context.Context, kind, key string) (T, bool, error) {
	var zero T
	ctx, cancel, err := s.guard(ctx)
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
	if err := s.codec.Unmarshal(b, &v); err != nil {
		return zero, false, err
	}
	return v, true, nil
}

func (s *storePG[T]) List(ctx context.Context, kind string, filter ...store.FilterFunc[T]) (map[string]T, error) {
	ctx, cancel, err := s.guard(ctx)
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
		if err := s.codec.Unmarshal(b, &v); err != nil {
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
	ctx, cancel, err := s.guard(ctx)
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
	ctx, cancel, err := s.guard(ctx)
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
	ctx, cancel, err := s.guard(ctx)
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
		if err := s.codec.Unmarshal(b, &v); err != nil {
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

func (s *storePG[T]) Set(ctx context.Context, kind, key string, value T) (bool, error) {
	ctx, cancel, err := s.guard(ctx)
	if err != nil {
		return false, err
	}
	defer cancel()

	enc, err := s.codec.Marshal(value)
	if err != nil {
		return false, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	ct, err := tx.Exec(ctx,
		`INSERT INTO zestor_kv(ns_id,kind,key,value) VALUES($1,$2,$3,$4)
         ON CONFLICT (ns_id,kind,key) DO NOTHING`,
		s.ns, kind, key, enc)
	if err != nil {
		return false, err
	}
	created := ct.RowsAffected() == 1

	if !created {
		var cur []byte
		err = tx.QueryRow(ctx,
			`SELECT value FROM zestor_kv WHERE ns_id=$1 AND kind=$2 AND key=$3`,
			s.ns, kind, key).Scan(&cur)
		if err != nil {
			return false, err
		}
		if bytes.Equal(cur, enc) {
			if err := tx.Commit(ctx); err != nil {
				return false, err
			}
			return false, nil
		}
		if _, err := tx.Exec(ctx,
			`UPDATE zestor_kv
               SET value=$4,
                   version=version+1,
                   updated_at=now()
             WHERE ns_id=$1 AND kind=$2 AND key=$3`,
			s.ns, kind, key, enc); err != nil {
			return false, err
		}
	}

	etype := store.EventTypeUpdate
	if created {
		etype = store.EventTypeCreate
	}
	if err := s.appendOutbox(ctx, tx, kind, key, etype, enc); err != nil {
		return false, err
	}

	if err := tx.Commit(ctx); err != nil {
		return false, err
	}

	return created, nil
}

func (s *storePG[T]) SetFn(ctx context.Context, kind, key string, fn func(v T) (T, error)) (bool, error) {
	ctx, cancel, err := s.guard(ctx)
	if err != nil {
		return false, err
	}
	defer cancel()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

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

func (s *storePG[T]) SetAll(ctx context.Context, kind string, values map[string]T) error {
	ctx, cancel, err := s.guard(ctx)
	if err != nil {
		return err
	}
	defer cancel()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	existingKeys, err := s.existingKeys(ctx, tx, kind)
	if err != nil {
		return err
	}

	// Apply in key order so the outbox — and therefore the event stream — is
	// deterministic across runs.
	for _, k := range slices.Sorted(maps.Keys(values)) {
		b, err := s.codec.Marshal(values[k])
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO zestor_kv(ns_id,kind,key,value) VALUES($1,$2,$3,$4)
             ON CONFLICT (ns_id,kind,key) DO UPDATE
             SET value=EXCLUDED.value,
                 version = CASE WHEN zestor_kv.value <> EXCLUDED.value
                                THEN zestor_kv.version+1
                                ELSE zestor_kv.version END,
                 updated_at = CASE WHEN zestor_kv.value <> EXCLUDED.value
                                   THEN now() ELSE zestor_kv.updated_at END`,
			s.ns, kind, k, b); err != nil {
			return err
		}
		etype := store.EventTypeCreate
		if _, existed := existingKeys[k]; existed {
			etype = store.EventTypeUpdate
		}
		if err := s.appendOutbox(ctx, tx, kind, k, etype, b); err != nil {
			return err
		}
	}

	return tx.Commit(ctx)
}

func (s *storePG[T]) Delete(ctx context.Context, kind, key string) (bool, T, error) {
	var zero T
	ctx, cancel, err := s.guard(ctx)
	if err != nil {
		return false, zero, err
	}
	defer cancel()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, zero, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

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
	if err := s.codec.Unmarshal(prevBytes, &prev); err != nil {
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

func (s *storePG[T]) appendOutbox(ctx context.Context, tx pgx.Tx, kind, key string, etype store.EventType, value []byte) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO zestor_outbox(ns_id,kind,key,etype,value) VALUES($1,$2,$3,$4,$5)`,
		s.ns, kind, key, string(etype), value)
	return err
}

func (s *storePG[T]) existingKeys(ctx context.Context, tx pgx.Tx, kind string) (map[string]struct{}, error) {
	rows, err := tx.Query(ctx, `SELECT key FROM zestor_kv WHERE ns_id=$1 AND kind=$2`, s.ns, kind)
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

func (s *storePG[T]) Watch(ctx context.Context, kind string, opts ...store.WatchOption[T]) (<-chan *store.Event[T], error) {
	if kind == "" {
		return nil, store.ErrKindRequired
	}
	opCtx, cancel, err := s.guard(ctx)
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

	// Both hooks run while the hub lock is held, which locks out the drain loop.
	//
	// CatchUp is what this backend needs and the in-process ones do not: Set
	// returns as soon as its transaction commits, while the matching event is
	// still working its way through NOTIFY and the outbox drain. Subscribing
	// without first flushing that backlog hands the new watcher events for
	// writes that predate it — which is what the old test suite was sleeping a
	// second to avoid.
	//
	// The watch itself is bound to ctx, not opCtx: Options.Timeout bounds the
	// catch-up and snapshot queries, not the lifetime of the watch.
	return s.hub.Subscribe(ctx, kind, cfg, watchhub.Hooks[T]{
		CatchUp: func(emit watchhub.EmitFunc[T]) error {
			return s.catchUp(opCtx, kind, emit)
		},
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
		if err := s.codec.Unmarshal(b, &v); err != nil {
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

// drainBatch publishes one batch of outbox rows for kind and advances the
// cursor, reporting whether there was anything to read.
//
// The rows are fully collected and the connection released before emit is
// called: emit may contend for the hub lock, and a Watch holds that lock while
// running its own queries, so publishing with a connection still checked out
// could starve the pool.
func (s *storePG[T]) drainBatch(ctx context.Context, kind string, emit watchhub.EmitFunc[T]) (bool, error) {
	s.drainMu.Lock()
	cursor, seen := s.lastID[kind]
	if !seen {
		cursor = s.startID
	}
	s.drainMu.Unlock()

	qctx, qcancel := context.WithTimeout(ctx, s.timeout)
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
		evs     []*store.Event[T]
		highest = cursor
		iterErr error
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
		if err := s.codec.Unmarshal(val, &obj); err != nil {
			iterErr = fmt.Errorf("outbox row %d (%s/%s): %w", id, kind, name, err)
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
	if len(evs) == 0 {
		return false, nil
	}

	emit(kind, evs...)

	s.drainMu.Lock()
	if cur, seen := s.lastID[kind]; !seen || highest > cur {
		s.lastID[kind] = highest
	}
	s.drainMu.Unlock()
	return true, nil
}

// catchUp drains kind to exhaustion. It is called from Watch's CatchUp hook with
// the hub lock held, so emit is the hub's locked publish and the caller is
// guaranteed that on return everything committed before the subscription has
// already been handed to the subscribers that existed before it.
func (s *storePG[T]) catchUp(ctx context.Context, kind string, emit watchhub.EmitFunc[T]) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		more, err := s.drainBatch(ctx, kind, emit)
		if err != nil {
			return err
		}
		if !more {
			return nil
		}
	}
}

func (s *storePG[T]) triggerDrain(ctx context.Context, kind string) {
	s.drainMu.Lock()
	st := s.draining[kind]
	if st == nil {
		st = &drainState{}
		s.draining[kind] = st
	}
	if st.running {
		st.pending = true
		s.drainMu.Unlock()
		return
	}
	st.running = true
	s.drainMu.Unlock()

	s.listenWG.Add(1)
	go func() {
		defer s.listenWG.Done()
		for {
			for ctx.Err() == nil {
				more, err := s.drainBatch(ctx, kind, s.hub.Publish)
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

			s.drainMu.Lock()
			if st.pending && ctx.Err() == nil {
				// A NOTIFY landed while we were draining; go round again rather
				// than exiting and losing it.
				st.pending = false
				s.drainMu.Unlock()
				continue
			}
			st.running = false
			st.pending = false
			s.drainMu.Unlock()
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
		backoff = 200 * time.Millisecond
		conn := acq.Conn()

		if _, err := conn.Exec(ctx, `UNLISTEN *`); err != nil {
			acq.Release()
			if ctx.Err() != nil {
				return
			}
			time.Sleep(backoff)
			backoff = min(backoff*2, 5*time.Second)
			continue
		}
		if _, err := conn.Exec(ctx, `LISTEN zestor_events`); err != nil {
			acq.Release()
			if ctx.Err() != nil {
				return
			}
			time.Sleep(backoff)
			backoff = min(backoff*2, 5*time.Second)
			continue
		}
		s.signalListenReady()

		// reacquire indicates the connection should be re-acquired (e.g. after a
		// WaitForNotification error). exitLoop indicates a clean shutdown of the
		// outer loop. In all cases we must Release acq exactly once via this block.
		exitLoop, reacquire := func() (bool, bool) {
			defer acq.Release()
			for {
				if ctx.Err() != nil {
					return true, false
				}
				s.mu.RLock()
				closed := s.closed
				s.mu.RUnlock()
				if closed {
					return true, false
				}

				waitCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
				ntf, err := conn.WaitForNotification(waitCtx)
				cancel()
				if err != nil {
					if ctx.Err() != nil {
						return true, false
					}
					return false, true
				}
				if ntf == nil {
					continue
				}
				ns, kind, _, _, _, perr := parsePayload(ntf.Payload)
				if perr != nil {
					continue
				}
				if ns != s.ns {
					continue
				}
				s.triggerDrain(ctx, kind)
			}
		}()
		if exitLoop {
			return
		}
		if reacquire {
			time.Sleep(backoff)
			backoff = min(backoff*2, 5*time.Second)
			continue
		}
	}
}

func parsePayload(p string) (ns int64, kind, key, etype string, id int64, err error) {
	parts := strings.Split(p, "|")
	if len(parts) != 5 {
		return 0, "", "", "", 0, errors.New("invalid payload")
	}
	ns, err = strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return
	}
	kind = parts[1]
	key = parts[2]
	etype = parts[3]
	id, err = strconv.ParseInt(parts[4], 10, 64)
	return
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
	s.hub.Close()
	s.pool.Close()
	return nil
}

func (s *storePG[T]) Dump() string {
	var sb strings.Builder
	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()
	rows, err := s.pool.Query(ctx,
		`SELECT kind, key, OCTET_LENGTH(value), version, updated_at
           FROM zestor_kv
          WHERE ns_id=$1
       ORDER BY kind, key`, s.ns)
	if err != nil {
		return err.Error()
	}
	defer rows.Close()
	for rows.Next() {
		var kind, key string
		var sz, ver int
		var ts time.Time
		if err := rows.Scan(&kind, &key, &sz, &ver, &ts); err == nil {
			fmt.Fprintf(&sb, "%s/%s v%d (%dB) %s\n", kind, key, ver, sz, ts.UTC().Format(time.RFC3339Nano))
		}
	}
	return sb.String()
}
