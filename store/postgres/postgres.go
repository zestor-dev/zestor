package postgres

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/zestor-dev/zestor/codec"
	"github.com/zestor-dev/zestor/store"
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
	// Timeout for the db operations
	Timeout time.Duration
}

type watcher[T any] struct {
	ch         chan *store.Event[T]
	eventTypes map[store.EventType]struct{}
}

type storePG[T any] struct {
	pool    *pgxpool.Pool
	codec   codec.Codec
	ns      int64
	timeout time.Duration

	muSubs sync.RWMutex
	subs   map[string]map[*watcher[T]]struct{}

	mu     sync.RWMutex
	closed bool

	listenCancel context.CancelFunc
	listenWG     sync.WaitGroup

	// Closed after the first successful LISTEN so callers do not mutate before
	// NOTIFY delivery is possible (PostgreSQL drops NOTIFY if no session is listening yet).
	listenReady     chan struct{}
	listenReadyOnce sync.Once
}

// New opens a pool, ensures schema, starts the LISTEN loop, and returns a Store[T].
func New[T any](o Options) (store.Store[T], error) {
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
	ctx, cancel := context.WithTimeout(context.Background(), o.Timeout)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}

	listCtx, listenCancel := context.WithCancel(context.Background())
	s := &storePG[T]{
		pool:         pool,
		codec:        o.Codec,
		ns:           o.Namespace,
		timeout:      o.Timeout,
		subs:         make(map[string]map[*watcher[T]]struct{}),
		listenCancel: listenCancel,
		listenReady:  make(chan struct{}),
	}
	if err := s.ensureSchema(context.Background()); err != nil {
		listenCancel()
		pool.Close()
		return nil, err
	}

	s.listenWG.Add(1)
	go func() {
		defer s.listenWG.Done()
		s.listenLoop(listCtx)
	}()

	waitCtx, waitCancel := context.WithTimeout(context.Background(), o.Timeout)
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

func (s *storePG[T]) Get(kind, key string) (T, bool, error) {
	var zero T
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return zero, false, store.ErrClosed
	}
	s.mu.RUnlock()

	var b []byte
	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()
	err := s.pool.QueryRow(ctx,
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

func (s *storePG[T]) List(kind string, filter ...store.FilterFunc[T]) (map[string]T, error) {
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return nil, store.ErrClosed
	}
	s.mu.RUnlock()

	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()
	rows, err := s.pool.Query(ctx,
		`SELECT key, value FROM zestor_kv WHERE ns_id=$1 AND kind=$2`, s.ns, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]T, 64)
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

func (s *storePG[T]) Count(kind string) (int, error) {
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return 0, store.ErrClosed
	}
	s.mu.RUnlock()

	var n int
	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()
	err := s.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM zestor_kv WHERE ns_id=$1 AND kind=$2`, s.ns, kind).Scan(&n)
	return n, err
}

func (s *storePG[T]) Keys(kind string) ([]string, error) {
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return nil, store.ErrClosed
	}
	s.mu.RUnlock()

	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()
	rows, err := s.pool.Query(ctx,
		`SELECT key FROM zestor_kv WHERE ns_id=$1 AND kind=$2`, s.ns, kind)
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

func (s *storePG[T]) Values(kind string) ([]store.KeyValue[T], error) {
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return nil, store.ErrClosed
	}
	s.mu.RUnlock()

	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()
	rows, err := s.pool.Query(ctx,
		`SELECT key, value FROM zestor_kv WHERE ns_id=$1 AND kind=$2`, s.ns, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]store.KeyValue[T], 0, 64)
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

func (s *storePG[T]) GetAll() (map[string]map[string]T, error) {
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return nil, store.ErrClosed
	}
	s.mu.RUnlock()

	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
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

func (s *storePG[T]) Set(kind, key string, value T) (bool, error) {
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
	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()
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
	if _, err := tx.Exec(ctx,
		`INSERT INTO zestor_outbox(ns_id,kind,key,etype,value) VALUES($1,$2,$3,$4,$5)`,
		s.ns, kind, key, string(etype), enc); err != nil {
		return false, err
	}

	if err := tx.Commit(ctx); err != nil {
		return false, err
	}

	return created, nil
}

func (s *storePG[T]) SetFn(kind, key string, fn func(v T) (T, error)) (bool, error) {
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return false, store.ErrClosed
	}
	s.mu.RUnlock()

	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
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

	if _, err := tx.Exec(ctx,
		`INSERT INTO zestor_outbox(ns_id,kind,key,etype,value) VALUES($1,$2,$3,$4,$5)`,
		s.ns, kind, key, string(store.EventTypeUpdate), newBytes); err != nil {
		return false, err
	}

	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return false, nil
}

func (s *storePG[T]) SetAll(kind string, values map[string]T) error {
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return store.ErrClosed
	}
	s.mu.RUnlock()

	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	existingKeys := make(map[string]struct{})
	keyRows, err := tx.Query(ctx, `SELECT key FROM zestor_kv WHERE ns_id=$1 AND kind=$2`, s.ns, kind)
	if err != nil {
		return err
	}
	defer keyRows.Close()
	for keyRows.Next() {
		var k string
		if err := keyRows.Scan(&k); err != nil {
			return err
		}
		existingKeys[k] = struct{}{}
	}
	if err := keyRows.Err(); err != nil {
		return err
	}

	for k, v := range values {
		b, err := s.codec.Marshal(v)
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
		var etype store.EventType
		if _, existed := existingKeys[k]; existed {
			etype = store.EventTypeUpdate
		} else {
			etype = store.EventTypeCreate
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO zestor_outbox(ns_id,kind,key,etype,value) VALUES($1,$2,$3,$4,$5)`,
			s.ns, kind, k, string(etype), b); err != nil {
			return err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return err
	}

	return nil
}

func (s *storePG[T]) Delete(kind, key string) (bool, T, error) {
	var zero T
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return false, zero, store.ErrClosed
	}
	s.mu.RUnlock()

	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
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
	if _, err := tx.Exec(ctx,
		`INSERT INTO zestor_outbox(ns_id,kind,key,etype,value) VALUES($1,$2,$3,'delete',$4)`,
		s.ns, kind, key, prevBytes); err != nil {
		return false, zero, err
	}

	if err := tx.Commit(ctx); err != nil {
		return false, zero, err
	}
	return true, prev, nil
}

func (s *storePG[T]) Watch(kind string, opts ...store.WatchOption[T]) (<-chan *store.Event[T], func(), error) {
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

	sendInitial := cfg.EventTypes == nil
	if !sendInitial && cfg.EventTypes != nil {
		_, sendInitial = cfg.EventTypes[store.EventTypeCreate]
	}
	if cfg.Initial && sendInitial {
		go func() {
			m, err := s.List(kind)
			if err != nil {
				return
			}
			for k, v := range m {
				ev := &store.Event[T]{Kind: kind, Name: k, EventType: store.EventTypeCreate, Object: v}
				s.muSubs.Lock()
				if s.subs[kind] == nil {
					s.muSubs.Unlock()
					return
				}
				if _, subscribed := s.subs[kind][w]; !subscribed {
					s.muSubs.Unlock()
					return
				}
				select {
				case w.ch <- ev:
				default:
				}
				s.muSubs.Unlock()
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

type drainState struct {
	running bool
}

func (s *storePG[T]) signalListenReady() {
	s.listenReadyOnce.Do(func() {
		close(s.listenReady)
	})
}

func (s *storePG[T]) listenLoop(ctx context.Context) {
	var (
		mu     sync.Mutex
		lastID = make(map[string]int64)
		state  = make(map[string]*drainState)
	)

	triggerDrain := func(kind string) {
		mu.Lock()
		st := state[kind]
		if st == nil {
			st = &drainState{}
			state[kind] = st
		}
		if st.running {
			mu.Unlock()
			return
		}
		st.running = true
		mu.Unlock()

		go func(k string) {
			defer func() {
				mu.Lock()
				state[k].running = false
				mu.Unlock()
			}()

			const batch = 256
			for {
				mu.Lock()
				cursor := lastID[k]
				mu.Unlock()

				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				rows, err := s.pool.Query(ctx, `
SELECT id, key, etype, value
  FROM zestor_outbox
 WHERE ns_id=$1 AND kind=$2 AND id > $3
 ORDER BY id
 LIMIT $4`, s.ns, k, cursor, batch)
				cancel()
				if err != nil {
					time.Sleep(200 * time.Millisecond)
					continue
				}

				var done bool
				for rows.Next() {
					done = true
					var id int64
					var name, etype string
					var val []byte
					if err := rows.Scan(&id, &name, &etype, &val); err != nil {
						rows.Close()
						return
					}
					var obj T
					_ = s.codec.Unmarshal(val, &obj)

					s.publish(k, &store.Event[T]{
						Kind:      k,
						Name:      name,
						EventType: store.EventType(etype),
						Object:    obj,
					})

					mu.Lock()
					if id > lastID[k] {
						lastID[k] = id
					}
					mu.Unlock()
				}
				if err := rows.Err(); err != nil {
					rows.Close()
					time.Sleep(200 * time.Millisecond)
					continue
				}
				rows.Close()

				if !done {
					return
				}
			}
		}(kind)
	}

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
			backoff = minDur(backoff*2, 5*time.Second)
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
			backoff = minDur(backoff*2, 5*time.Second)
			continue
		}
		if _, err := conn.Exec(ctx, `LISTEN zestor_events`); err != nil {
			acq.Release()
			if ctx.Err() != nil {
				return
			}
			time.Sleep(backoff)
			backoff = minDur(backoff*2, 5*time.Second)
			continue
		}
		s.signalListenReady()

		for ctx.Err() == nil {
			s.mu.RLock()
			closed := s.closed
			s.mu.RUnlock()
			if closed {
				acq.Release()
				return
			}

			waitCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
			ntf, err := conn.WaitForNotification(waitCtx)
			cancel()
			if err != nil {
				acq.Release()
				if ctx.Err() != nil {
					return
				}
				break
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
			triggerDrain(kind)
		}

		if ctx.Err() != nil {
			return
		}
		time.Sleep(backoff)
		backoff = minDur(backoff*2, 5*time.Second)
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

func minDur(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func (s *storePG[T]) publish(kind string, ev *store.Event[T]) {
	s.muSubs.RLock()
	defer s.muSubs.RUnlock()
	for w := range s.subs[kind] {
		if w.eventTypes != nil {
			if _, ok := w.eventTypes[ev.EventType]; !ok {
				continue
			}
		}
		select {
		case w.ch <- ev:
		default:
		}
	}
}

func (s *storePG[T]) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()

	s.muSubs.Lock()
	for _, m := range s.subs {
		for w := range m {
			close(w.ch)
		}
	}
	s.subs = nil
	s.muSubs.Unlock()

	if s.listenCancel != nil {
		s.listenCancel()
	}
	s.listenWG.Wait()
	s.pool.Close()
	return nil
}

func (s *storePG[T]) Dump() string {
	var sb strings.Builder
	rows, err := s.pool.Query(context.Background(),
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
