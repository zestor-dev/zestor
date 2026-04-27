package gomap

import (
	"fmt"
	"maps"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/zestor-dev/zestor/store"
)

type memStore[T any] struct {
	mu sync.RWMutex
	// kind -> (key -> obj)
	kinds map[string]map[string]T
	// kind -> validation function
	validationFns map[string]store.ValidateFunc[T]
	// kind -> (watcherID -> chan)
	watchers map[string]map[string]*watcher[T]
	// compare func
	compareFn store.CompareFunc[T]
	closed    bool
	// counter for generating unique watcher IDs
	watcherID atomic.Uint64
}

type watcher[T any] struct {
	mu         sync.RWMutex
	ch         chan *store.Event[T]
	eventTypes map[store.EventType]struct{}
	done       chan struct{}
	closed     bool
}

func newWatcher[T any](bufferSize int, eventTypes map[store.EventType]struct{}) *watcher[T] {
	return &watcher[T]{
		ch:         make(chan *store.Event[T], bufferSize),
		eventTypes: eventTypes,
		done:       make(chan struct{}),
	}
}

func (w *watcher[T]) wants(eventType store.EventType) bool {
	if w.eventTypes == nil {
		return true
	}
	_, ok := w.eventTypes[eventType]
	return ok
}

func (w *watcher[T]) send(ev *store.Event[T]) {
	if !w.wants(ev.EventType) {
		return
	}
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.closed {
		return
	}
	select {
	case <-w.done:
	case w.ch <- ev:
	default:
	}
}

func (w *watcher[T]) close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}
	w.closed = true
	close(w.done)
	close(w.ch)
}

func NewMemStore[T any](opt store.StoreOptions[T]) store.Store[T] {
	ms := &memStore[T]{
		kinds:         make(map[string]map[string]T),
		watchers:      make(map[string]map[string]*watcher[T]),
		validationFns: make(map[string]store.ValidateFunc[T]),
		compareFn:     opt.CompareFn,
	}
	if ms.compareFn == nil {
		ms.compareFn = store.DefaultCompareFunc[T]
	}
	if opt.ValidateFns != nil {
		maps.Copy(ms.validationFns, opt.ValidateFns)
	}
	return ms
}

func (s *memStore[T]) ensureKind(kind string) {
	if _, ok := s.kinds[kind]; !ok {
		s.kinds[kind] = make(map[string]T)
	}
	if _, ok := s.watchers[kind]; !ok {
		s.watchers[kind] = make(map[string]*watcher[T])
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

func (s *memStore[T]) Get(kind, key string) (T, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		var zero T
		return zero, false, store.ErrClosed
	}
	m := s.kinds[kind]
	v, ok := m[key]
	return v, ok, nil
}

func (s *memStore[T]) List(kind string, filters ...store.FilterFunc[T]) (map[string]T, error) {
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

func (s *memStore[T]) Keys(kind string) ([]string, error) {
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

func (s *memStore[T]) Values(kind string) ([]store.KeyValue[T], error) {
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

func (s *memStore[T]) Count(kind string) (int, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return 0, store.ErrClosed
	}
	return len(s.kinds[kind]), nil
}

func (s *memStore[T]) Set(kind, key string, value T) (bool, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return false, store.ErrClosed
	}
	s.ensureKind(kind)

	if fn, ok := s.validationFns[kind]; ok {
		if err := fn(value); err != nil {
			s.mu.Unlock()
			return false, err
		}
	}

	prev, existed := s.kinds[kind][key]
	if existed && s.compareFn(prev, value) {
		s.mu.Unlock()
		return false, nil
	}
	s.kinds[kind][key] = value

	// copy watchers then unlock
	wchs := make([]*watcher[T], 0, len(s.watchers[kind]))
	for _, ch := range s.watchers[kind] {
		wchs = append(wchs, ch)
	}
	s.mu.Unlock()

	evType := store.EventTypeUpdate
	if !existed {
		evType = store.EventTypeCreate
	}
	ev := &store.Event[T]{Kind: kind, Name: key, EventType: evType, Object: value}
	for _, wch := range wchs {
		wch.send(ev)
	}
	return !existed, nil
}

func (s *memStore[T]) SetAll(kind string, values map[string]T) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return store.ErrClosed
	}
	s.ensureKind(kind)

	// validate all values first
	if fn, ok := s.validationFns[kind]; ok {
		for _, v := range values {
			if err := fn(v); err != nil {
				s.mu.Unlock()
				return err
			}
		}
	}

	// track which keys are created vs updated
	created := make(map[string]T)
	updated := make(map[string]T)
	for k, v := range values {
		prev, existed := s.kinds[kind][k]
		if existed && s.compareFn(prev, v) {
			continue
		}
		if existed {
			updated[k] = v
		} else {
			created[k] = v
		}
		s.kinds[kind][k] = v
	}

	// copy watchers then unlock
	wchs := make([]*watcher[T], 0, len(s.watchers[kind]))
	for _, wch := range s.watchers[kind] {
		wchs = append(wchs, wch)
	}
	s.mu.Unlock()

	for _, wch := range wchs {
		for k, v := range created {
			wch.send(&store.Event[T]{Kind: kind, Name: k, EventType: store.EventTypeCreate, Object: v})
		}
		for k, v := range updated {
			wch.send(&store.Event[T]{Kind: kind, Name: k, EventType: store.EventTypeUpdate, Object: v})
		}
	}
	return nil
}

func (s *memStore[T]) Delete(kind, key string) (bool, T, error) {
	var zero T

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return false, zero, store.ErrClosed
	}
	s.ensureKind(kind)

	prev, existed := s.kinds[kind][key]
	if existed {
		delete(s.kinds[kind], key)
	}

	if !existed {
		s.mu.Unlock()
		return false, zero, nil
	}

	// copy watchers then unlock
	wchs := make([]*watcher[T], 0, len(s.watchers[kind]))
	for _, ch := range s.watchers[kind] {
		wchs = append(wchs, ch)
	}
	s.mu.Unlock()

	ev := &store.Event[T]{Kind: kind, Name: key, EventType: store.EventTypeDelete, Object: prev}
	for _, wch := range wchs {
		wch.send(ev)
	}
	return existed, prev, nil
}

func (s *memStore[T]) SetFn(kind, key string, fn func(v T) (T, error)) (bool, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return false, store.ErrClosed
	}
	s.ensureKind(kind)

	prev, existed := s.kinds[kind][key]
	if !existed {
		s.mu.Unlock()
		return false, store.ErrKeyNotFound
	}
	value, err := fn(prev)
	if err != nil {
		s.mu.Unlock()
		return false, err
	}
	if fn, ok := s.validationFns[kind]; ok {
		if err := fn(value); err != nil {
			s.mu.Unlock()
			return false, err
		}
	}
	if s.compareFn(prev, value) {
		s.mu.Unlock()
		return false, nil
	}
	// update value
	s.kinds[kind][key] = value
	// copy watchers then unlock
	wchs := make([]*watcher[T], 0, len(s.watchers[kind]))
	for _, ch := range s.watchers[kind] {
		wchs = append(wchs, ch)
	}
	s.mu.Unlock()

	ev := &store.Event[T]{
		Kind:      kind,
		Name:      key,
		EventType: store.EventTypeUpdate,
		Object:    value,
	}
	for _, wch := range wchs {
		wch.send(ev)
	}
	return false, nil
}

func (s *memStore[T]) Watch(kind string, opts ...store.WatchOption[T]) (<-chan *store.Event[T], func(), error) {
	if kind == "" {
		return nil, nil, store.ErrKindRequired
	}
	cfg := &store.WatchCfg[T]{}
	for _, o := range opts {
		if o != nil {
			o(cfg)
		}
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, nil, store.ErrClosed
	}
	s.ensureKind(kind)

	bufSize := cfg.BufferSize
	if bufSize <= 0 {
		bufSize = store.DefaultWatchBufferSize
	}
	id := strconv.FormatUint(s.watcherID.Add(1), 10)
	wch := newWatcher[T](bufSize, cfg.EventTypes)
	s.watchers[kind][id] = wch

	// capture snapshot for optional initial replay
	var snap map[string]T
	if cfg.Initial {
		snap = cloneMap(s.kinds[kind])
	}
	s.mu.Unlock()

	// send initial snapshot (nil eventTypes means all events)
	sendInitial := wch.wants(store.EventTypeCreate)
	if cfg.Initial && len(snap) > 0 && sendInitial {
		go func(m map[string]T) {
			for k, v := range m {
				wch.send(&store.Event[T]{
					Kind:      kind,
					Name:      k,
					EventType: store.EventTypeCreate,
					Object:    v,
				})
			}
		}(snap)
	}

	// build cancel function
	cancel := func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if w, ok := s.watchers[kind]; ok {
			if wch, ok := w[id]; ok {
				delete(w, id)
				wch.close()
			}
		}
	}
	return wch.ch, cancel, nil
}

func (s *memStore[T]) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	for _, m := range s.watchers {
		for id, wch := range m {
			delete(m, id)
			wch.close()
		}
	}
	return nil
}

func (s *memStore[T]) Dump() string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	sb := strings.Builder{}
	for kind, m := range s.kinds {
		sb.WriteString(fmt.Sprintf("%s:\n", kind))
		for k, v := range m {
			sb.WriteString(fmt.Sprintf("  %s: %+v\n", k, v))
		}
	}
	return sb.String()
}

func (s *memStore[T]) GetAll() (map[string]map[string]T, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, store.ErrClosed
	}
	// deep clone: clone outer map and each inner map
	out := make(map[string]map[string]T, len(s.kinds))
	for kind, m := range s.kinds {
		out[kind] = cloneMap(m)
	}
	return out, nil
}
