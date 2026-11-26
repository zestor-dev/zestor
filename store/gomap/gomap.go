package gomap

import (
	"bufio"
	"cmp"
	"container/heap"
	"context"
	"fmt"
	"io"
	"maps"
	"slices"
	"sync"

	"github.com/zestor-dev/zestor/store"
	"github.com/zestor-dev/zestor/store/internal/watchhub"
)

type memStore[T any] struct {
	mu sync.RWMutex
	// kind -> (key -> obj)
	kinds map[string]map[string]T
	// kind -> validation function
	validationFns map[string]store.ValidateFunc[T]
	// compare func
	compareFn store.CompareFunc[T]
	closed    bool

	// hub is published to while mu is held, so queue order matches write order.
	hub *watchhub.Hub[T]
}

func NewMemStore[T any](opt store.StoreOptions[T]) store.Store[T] {
	ms := &memStore[T]{
		kinds:         make(map[string]map[string]T),
		validationFns: make(map[string]store.ValidateFunc[T]),
		compareFn:     opt.CompareFn,
		hub:           watchhub.New[T](),
	}
	if ms.compareFn == nil {
		ms.compareFn = store.DefaultCompareFunc[T]
	}
	if opt.ValidateFns != nil {
		maps.Copy(ms.validationFns, opt.ValidateFns)
	}
	return ms
}

func requireKind(kind string) error {
	if kind == "" {
		return store.ErrKindRequired
	}
	return nil
}

func (s *memStore[T]) ensureKind(kind string) {
	if _, ok := s.kinds[kind]; !ok {
		s.kinds[kind] = make(map[string]T)
	}
}

func (s *memStore[T]) dropEmptyKind(kind string) {
	if len(s.kinds[kind]) == 0 {
		delete(s.kinds, kind)
	}
}

func cloneMap[T any](in map[string]T) map[string]T {
	if in == nil {
		return map[string]T{}
	}
	out := make(map[string]T, len(in))
	maps.Copy(out, in)
	return out
}

func (s *memStore[T]) Get(ctx context.Context, kind, key string) (T, bool, error) {
	var zero T
	if err := ctx.Err(); err != nil {
		return zero, false, err
	}
	if err := requireKind(kind); err != nil {
		return zero, false, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return zero, false, store.ErrClosed
	}
	m := s.kinds[kind]
	v, ok := m[key]
	return v, ok, nil
}

func (s *memStore[T]) List(ctx context.Context, kind string, filters ...store.FilterFunc[T]) (map[string]T, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := requireKind(kind); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, store.ErrClosed
	}
	rs := make(map[string]T, len(s.kinds[kind]))
OUTER:
	for k, v := range s.kinds[kind] {
		for _, f := range filters {
			if f != nil && !f(k, v) {
				continue OUTER
			}
		}
		rs[k] = v
	}
	return rs, nil
}

func (s *memStore[T]) Keys(ctx context.Context, kind string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := requireKind(kind); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, store.ErrClosed
	}
	keys := make([]string, 0, len(s.kinds[kind]))
	for k := range s.kinds[kind] {
		keys = append(keys, k)
	}
	return keys, nil
}

func (s *memStore[T]) Values(ctx context.Context, kind string) ([]store.KeyValue[T], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := requireKind(kind); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, store.ErrClosed
	}
	values := make([]store.KeyValue[T], 0, len(s.kinds[kind]))
	for k, v := range s.kinds[kind] {
		values = append(values, store.KeyValue[T]{Key: k, Value: v})
	}
	return values, nil
}

func (s *memStore[T]) Count(ctx context.Context, kind string) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := requireKind(kind); err != nil {
		return 0, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return 0, store.ErrClosed
	}
	return len(s.kinds[kind]), nil
}

func (s *memStore[T]) Set(ctx context.Context, kind, key string, value T) (store.SetResult, error) {
	if err := ctx.Err(); err != nil {
		return store.SetUnchanged, err
	}
	if err := requireKind(kind); err != nil {
		return store.SetUnchanged, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return store.SetUnchanged, store.ErrClosed
	}

	if fn, ok := s.validationFns[kind]; ok {
		if err := fn(value); err != nil {
			return store.SetUnchanged, err
		}
	}
	s.ensureKind(kind)

	prev, existed := s.kinds[kind][key]
	if existed && s.compareFn(prev, value) {
		return store.SetUnchanged, nil
	}
	s.kinds[kind][key] = value

	result, evType := store.SetCreated, store.EventTypeCreate
	if existed {
		result, evType = store.SetUpdated, store.EventTypeUpdate
	}
	// Published under mu: two concurrent Sets to the same key enqueue in the
	// same order they were applied, so a watcher rebuilding state converges on
	// the value the store actually holds.
	s.hub.Publish(kind, &store.Event[T]{Kind: kind, Name: key, EventType: evType, Object: value})
	return result, nil
}

func (s *memStore[T]) SetMany(ctx context.Context, kind string, values map[string]T) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := requireKind(kind); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return store.ErrClosed
	}
	if len(values) == 0 {
		return nil
	}

	// validate all values first
	if fn, ok := s.validationFns[kind]; ok {
		for _, v := range values {
			if err := fn(v); err != nil {
				return err
			}
		}
	}
	s.ensureKind(kind)

	// Apply in key order so the event stream is deterministic across runs.
	evs := make([]*store.Event[T], 0, len(values))
	for _, k := range slices.Sorted(maps.Keys(values)) {
		v := values[k]
		prev, existed := s.kinds[kind][k]
		if existed && s.compareFn(prev, v) {
			continue
		}
		evType := store.EventTypeUpdate
		if !existed {
			evType = store.EventTypeCreate
		}
		s.kinds[kind][k] = v
		evs = append(evs, &store.Event[T]{Kind: kind, Name: k, EventType: evType, Object: v})
	}

	s.hub.Publish(kind, evs...)
	return nil
}

func (s *memStore[T]) Delete(ctx context.Context, kind, key string) (bool, T, error) {
	var zero T
	if err := ctx.Err(); err != nil {
		return false, zero, err
	}
	if err := requireKind(kind); err != nil {
		return false, zero, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false, zero, store.ErrClosed
	}

	prev, existed := s.kinds[kind][key]
	if !existed {
		return false, zero, nil
	}
	delete(s.kinds[kind], key)
	s.dropEmptyKind(kind)

	s.hub.Publish(kind, &store.Event[T]{Kind: kind, Name: key, EventType: store.EventTypeDelete, Object: prev})
	return true, prev, nil
}

func (s *memStore[T]) SetFn(ctx context.Context, kind, key string, fn func(v T) (T, error)) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := requireKind(kind); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false, store.ErrClosed
	}

	prev, existed := s.kinds[kind][key]
	if !existed {
		return false, store.ErrKeyNotFound
	}
	// fn runs under the write lock, which is what makes read-modify-write
	// atomic. Documented on store.Writer: fn must not call back into the store.
	value, err := fn(prev)
	if err != nil {
		return false, err
	}
	if validate, ok := s.validationFns[kind]; ok {
		if err := validate(value); err != nil {
			return false, err
		}
	}
	if s.compareFn(prev, value) {
		return false, nil
	}
	s.kinds[kind][key] = value

	s.hub.Publish(kind, &store.Event[T]{
		Kind:      kind,
		Name:      key,
		EventType: store.EventTypeUpdate,
		Object:    value,
	})
	return true, nil
}

func (s *memStore[T]) Watch(ctx context.Context, kind string, opts ...store.WatchOption[T]) (<-chan *store.Event[T], error) {
	if err := requireKind(kind); err != nil {
		return nil, err
	}
	cfg := &store.WatchCfg[T]{}
	for _, o := range opts {
		if o != nil {
			o(cfg)
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, store.ErrClosed
	}

	// The snapshot runs under both mu (held here) and the hub lock, so it cannot
	// interleave with a concurrent write: every replayed event is queued before
	// any event that write would publish. No CatchUp hook is needed — events are
	// published under mu by the write itself, so nothing is ever in flight.
	return s.hub.Subscribe(ctx, kind, cfg, watchhub.Hooks[T]{
		Snapshot: func() ([]*store.Event[T], error) {
			snap := s.kinds[kind]
			out := make([]*store.Event[T], 0, len(snap))
			for _, k := range slices.Sorted(maps.Keys(snap)) {
				out = append(out, &store.Event[T]{
					Kind:      kind,
					Name:      k,
					EventType: store.EventTypeCreate,
					Object:    snap[k],
				})
			}
			return out, nil
		},
	})
}

func (s *memStore[T]) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()

	s.hub.Close()
	return nil
}

// Dump implements store.Dumper.
func (s *memStore[T]) Dump(ctx context.Context, w io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return store.ErrClosed
	}

	bw := bufio.NewWriter(w)
	for _, kind := range slices.Sorted(maps.Keys(s.kinds)) {
		if _, err := fmt.Fprintf(bw, "%s:\n", kind); err != nil {
			return err
		}
		m := s.kinds[kind]
		for _, k := range slices.Sorted(maps.Keys(m)) {
			if _, err := fmt.Fprintf(bw, "  %s: %+v\n", k, m[k]); err != nil {
				return err
			}
		}
	}
	return bw.Flush()
}

// GetAll returns a copy of the kind and key maps. The values themselves are
// copied shallowly: if T contains a pointer, slice or map, the returned values
// alias data the store still owns, and mutating them races with other callers.
func (s *memStore[T]) GetAll(ctx context.Context) (map[string]map[string]T, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, store.ErrClosed
	}
	out := make(map[string]map[string]T, len(s.kinds))
	for kind, m := range s.kinds {
		out[kind] = cloneMap(m)
	}
	return out, nil
}

var _ store.Pager[struct{}] = (*memStore[struct{}])(nil)

func (s *memStore[T]) ListPage(ctx context.Context, kind string, after *string, limit int) ([]store.KeyValue[T], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := requireKind(kind); err != nil {
		return nil, err
	}
	if limit < 1 {
		return nil, store.ErrPageLimit
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, store.ErrClosed
	}
	keys := smallestKeys(s.kinds[kind], after, limit)
	out := make([]store.KeyValue[T], len(keys))
	m := s.kinds[kind]
	for i, k := range keys {
		out[i] = store.KeyValue[T]{Key: k, Value: m[k]}
	}
	return out, nil
}

func (s *memStore[T]) KeysPage(ctx context.Context, kind string, after *string, limit int) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := requireKind(kind); err != nil {
		return nil, err
	}
	if limit < 1 {
		return nil, store.ErrPageLimit
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, store.ErrClosed
	}
	return smallestKeys(s.kinds[kind], after, limit), nil
}

func (s *memStore[T]) GetAllPage(ctx context.Context, after *store.PageCursor, limit int) ([]store.KindValue[T], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit < 1 {
		return nil, store.ErrPageLimit
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, store.ErrClosed
	}
	h := &kindHeap[T]{rows: make([]store.KindValue[T], 0, limit)}
	for kind, m := range s.kinds {
		for key, v := range m {
			if after != nil && !afterCursor(kind, key, *after) {
				continue
			}
			row := store.KindValue[T]{Kind: kind, Key: key, Value: v}
			if h.Len() < limit {
				heap.Push(h, row)
				continue
			}
			if kindBefore(row, h.rows[0]) {
				h.rows[0] = row
				heap.Fix(h, 0)
			}
		}
	}
	out := h.rows
	slices.SortFunc(out, func(a, b store.KindValue[T]) int {
		return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Key, b.Key))
	})
	return out, nil
}

// smallestKeys returns the lowest limit keys of m that are strictly after
// after, in ascending order. It keeps only that many candidates, so a short
// page does not allocate a slice of the whole kind.
func smallestKeys[T any](m map[string]T, after *string, limit int) []string {
	h := make(maxStrings, 0, limit)
	for k := range m {
		if after != nil && k <= *after {
			continue
		}
		if len(h) < limit {
			heap.Push(&h, k)
			continue
		}
		if k < h[0] {
			h[0] = k
			heap.Fix(&h, 0)
		}
	}
	slices.Sort(h)
	return h
}

// maxStrings is a max-heap of strings, so the root is the greatest candidate
// and a smaller key can replace it.
type maxStrings []string

func (h maxStrings) Len() int           { return len(h) }
func (h maxStrings) Less(i, j int) bool { return h[i] > h[j] }
func (h maxStrings) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *maxStrings) Push(x any)        { *h = append(*h, x.(string)) }
func (h *maxStrings) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

// kindHeap is a max-heap of entries, ordered by kind then key.
type kindHeap[T any] struct {
	rows []store.KindValue[T]
}

func (h kindHeap[T]) Len() int { return len(h.rows) }
func (h kindHeap[T]) Less(i, j int) bool {
	return kindBefore(h.rows[j], h.rows[i])
}
func (h kindHeap[T]) Swap(i, j int) { h.rows[i], h.rows[j] = h.rows[j], h.rows[i] }
func (h *kindHeap[T]) Push(x any) {
	h.rows = append(h.rows, x.(store.KindValue[T]))
}
func (h *kindHeap[T]) Pop() any {
	old := h.rows
	n := len(old)
	x := old[n-1]
	h.rows = old[:n-1]
	return x
}

func kindBefore[T any](a, b store.KindValue[T]) bool {
	if a.Kind != b.Kind {
		return a.Kind < b.Kind
	}
	return a.Key < b.Key
}

// afterCursor reports whether kind/key is strictly after c.
func afterCursor(kind, key string, c store.PageCursor) bool {
	if kind != c.Kind {
		return kind > c.Kind
	}
	return key > c.Key
}
