package sqlite

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/zestor-dev/zestor/codec"
	"github.com/zestor-dev/zestor/store"
)

type TestData struct {
	Name  string `json:"name"`
	Value int    `json:"value"`
}

func TestNew(t *testing.T) {
	tmpDir := t.TempDir()

	tests := []struct {
		name    string
		opts    Options
		wantErr bool
	}{
		{
			name: "valid options",
			opts: Options{
				DSN:   "file:" + filepath.Join(tmpDir, "test.db"),
				Codec: &codec.JSON{},
			},
			wantErr: false,
		},
		{
			name: "missing dsn",
			opts: Options{
				Codec: &codec.JSON{},
			},
			wantErr: true,
		},
		{
			name: "missing codec",
			opts: Options{
				DSN: "file:" + filepath.Join(tmpDir, "test2.db"),
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := New[TestData](tt.opts)
			if (err != nil) != tt.wantErr {
				t.Errorf("New() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if s != nil {
				_ = s.Close()
			}
		})
	}
}

func setupStore(t *testing.T) store.Store[TestData] {
	t.Helper()
	tmpDir := t.TempDir()
	s, err := New[TestData](Options{
		DSN:         "file:" + filepath.Join(tmpDir, "test.db"),
		Codec:       &codec.JSON{},
		BusyTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}
	return s
}

func TestGetSet(t *testing.T) {
	s := setupStore(t)
	defer s.Close()

	kind := "test"
	key := "key1"
	val := TestData{Name: "test1", Value: 42}

	_, ok, err := s.Get(kind, key)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if ok {
		t.Error("Get() returned ok=true for non-existent key")
	}

	created, err := s.Set(kind, key, val)
	if err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	if !created {
		t.Error("Set() should return created=true for new key")
	}

	got, ok, err := s.Get(kind, key)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if !ok {
		t.Error("Get() returned ok=false for existing key")
	}
	if got.Name != val.Name || got.Value != val.Value {
		t.Errorf("Get() = %v, want %v", got, val)
	}

	created, err = s.Set(kind, key, val)
	if err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	if created {
		t.Error("Set() should return created=false for no-op")
	}

	val2 := TestData{Name: "test2", Value: 84}
	created, err = s.Set(kind, key, val2)
	if err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	if created {
		t.Error("Set() should return created=false for update")
	}

	got, ok, err = s.Get(kind, key)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if !ok {
		t.Error("Get() returned ok=false")
	}
	if got.Name != val2.Name || got.Value != val2.Value {
		t.Errorf("Get() = %v, want %v", got, val2)
	}
}

func TestSetFn(t *testing.T) {
	s := setupStore(t)
	defer s.Close()

	kind := "test"
	key := "counter"

	// SetFn on non-existent key should return ErrKeyNotFound
	_, err := s.SetFn(kind, key, func(v TestData) (TestData, error) {
		return TestData{Name: "counter", Value: 1}, nil
	})
	if err != store.ErrKeyNotFound {
		t.Errorf("SetFn() on non-existent key should return ErrKeyNotFound, got %v", err)
	}

	// Create the key first
	_, err = s.Set(kind, key, TestData{Name: "counter", Value: 1})
	if err != nil {
		t.Fatalf("Set() error = %v", err)
	}

	// Now SetFn should work
	changed, err := s.SetFn(kind, key, func(v TestData) (TestData, error) {
		v.Value++
		return v, nil
	})
	if err != nil {
		t.Fatalf("SetFn() error = %v", err)
	}
	if changed {
		t.Error("SetFn() should return changed=false (it only updates)")
	}

	got, ok, err := s.Get(kind, key)
	if err != nil || !ok {
		t.Fatalf("Get() error = %v, ok = %v", err, ok)
	}
	if got.Value != 2 {
		t.Errorf("Get() value = %d, want 2", got.Value)
	}

	// No-op should return changed=false
	changed, err = s.SetFn(kind, key, func(v TestData) (TestData, error) {
		return v, nil
	})
	if err != nil {
		t.Fatalf("SetFn() error = %v", err)
	}
	if changed {
		t.Error("SetFn() should return changed=false for no-op")
	}
}

func TestDelete(t *testing.T) {
	s := setupStore(t)
	defer s.Close()

	kind := "test"
	key := "to_delete"
	val := TestData{Name: "delete_me", Value: 99}

	existed, prev, err := s.Delete(kind, key)
	if err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if existed {
		t.Error("Delete() should return existed=false for non-existent key")
	}

	_, err = s.Set(kind, key, val)
	if err != nil {
		t.Fatalf("Set() error = %v", err)
	}

	existed, prev, err = s.Delete(kind, key)
	if err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if !existed {
		t.Error("Delete() should return existed=true")
	}
	if prev.Name != val.Name || prev.Value != val.Value {
		t.Errorf("Delete() prev = %v, want %v", prev, val)
	}

	_, ok, err := s.Get(kind, key)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if ok {
		t.Error("Get() should return ok=false after delete")
	}
}

func TestList(t *testing.T) {
	s := setupStore(t)
	defer s.Close()

	kind := "test"
	data := map[string]TestData{
		"item1": {Name: "first", Value: 1},
		"item2": {Name: "second", Value: 2},
		"item3": {Name: "third", Value: 3},
	}

	for k, v := range data {
		if _, err := s.Set(kind, k, v); err != nil {
			t.Fatalf("Set() error = %v", err)
		}
	}

	got, err := s.List(kind)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(got) != len(data) {
		t.Errorf("List() len = %d, want %d", len(got), len(data))
	}
	for k, v := range data {
		if g, ok := got[k]; !ok {
			t.Errorf("List() missing key %s", k)
		} else if g.Name != v.Name || g.Value != v.Value {
			t.Errorf("List() [%s] = %v, want %v", k, g, v)
		}
	}

	filtered, err := s.List(kind, func(key string, val TestData) bool {
		return val.Value > 1
	})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(filtered) != 2 {
		t.Errorf("List() with filter len = %d, want 2", len(filtered))
	}
}

func TestCount(t *testing.T) {
	s := setupStore(t)
	defer s.Close()

	kind := "test"

	count, err := s.Count(kind)
	if err != nil {
		t.Fatalf("Count() error = %v", err)
	}
	if count != 0 {
		t.Errorf("Count() = %d, want 0", count)
	}

	for i := 0; i < 5; i++ {
		key := fmt.Sprintf("item%d", i)
		val := TestData{Name: key, Value: i}
		if _, err := s.Set(kind, key, val); err != nil {
			t.Fatalf("Set() error = %v", err)
		}
	}

	count, err = s.Count(kind)
	if err != nil {
		t.Fatalf("Count() error = %v", err)
	}
	if count != 5 {
		t.Errorf("Count() = %d, want 5", count)
	}
}

func TestKeys(t *testing.T) {
	s := setupStore(t)
	defer s.Close()

	kind := "test"
	expected := []string{"key1", "key2", "key3"}

	for _, k := range expected {
		val := TestData{Name: k, Value: 1}
		if _, err := s.Set(kind, k, val); err != nil {
			t.Fatalf("Set() error = %v", err)
		}
	}

	keys, err := s.Keys(kind)
	if err != nil {
		t.Fatalf("Keys() error = %v", err)
	}
	if len(keys) != len(expected) {
		t.Errorf("Keys() len = %d, want %d", len(keys), len(expected))
	}

	keyMap := make(map[string]bool)
	for _, k := range keys {
		keyMap[k] = true
	}
	for _, k := range expected {
		if !keyMap[k] {
			t.Errorf("Keys() missing key %s", k)
		}
	}
}

func TestValues(t *testing.T) {
	s := setupStore(t)
	defer s.Close()

	kind := "test"
	data := map[string]TestData{
		"item1": {Name: "first", Value: 1},
		"item2": {Name: "second", Value: 2},
	}

	for k, v := range data {
		if _, err := s.Set(kind, k, v); err != nil {
			t.Fatalf("Set() error = %v", err)
		}
	}

	values, err := s.Values(kind)
	if err != nil {
		t.Fatalf("Values() error = %v", err)
	}
	if len(values) != len(data) {
		t.Errorf("Values() len = %d, want %d", len(values), len(data))
	}

	for _, kv := range values {
		want, ok := data[kv.Key]
		if !ok {
			t.Errorf("Values() unexpected key %s", kv.Key)
			continue
		}
		if kv.Value.Name != want.Name || kv.Value.Value != want.Value {
			t.Errorf("Values() [%s] = %v, want %v", kv.Key, kv.Value, want)
		}
	}
}

func TestSetAll(t *testing.T) {
	s := setupStore(t)
	defer s.Close()

	kind := "test"
	data := map[string]TestData{
		"batch1": {Name: "first", Value: 1},
		"batch2": {Name: "second", Value: 2},
		"batch3": {Name: "third", Value: 3},
	}

	err := s.SetAll(kind, data)
	if err != nil {
		t.Fatalf("SetAll() error = %v", err)
	}

	for k, want := range data {
		got, ok, err := s.Get(kind, k)
		if err != nil {
			t.Fatalf("Get() error = %v", err)
		}
		if !ok {
			t.Errorf("Get(%s) returned ok=false", k)
			continue
		}
		if got.Name != want.Name || got.Value != want.Value {
			t.Errorf("Get(%s) = %v, want %v", k, got, want)
		}
	}
}

func TestWatch(t *testing.T) {
	s := setupStore(t)
	defer s.Close()

	kind := "test"

	ch, cancel, err := s.Watch(kind)
	if err != nil {
		t.Fatalf("Watch() error = %v", err)
	}
	defer cancel()

	val := TestData{Name: "watched", Value: 100}
	go func() {
		time.Sleep(100 * time.Millisecond)
		_, _ = s.Set(kind, "watch_key", val)
	}()

	select {
	case ev := <-ch:
		if ev.Kind != kind {
			t.Errorf("Event kind = %s, want %s", ev.Kind, kind)
		}
		if ev.Name != "watch_key" {
			t.Errorf("Event name = %s, want watch_key", ev.Name)
		}
		if ev.EventType != store.EventTypeCreate {
			t.Errorf("Event type = %s, want %s", ev.EventType, store.EventTypeCreate)
		}
		if ev.Object.Name != val.Name || ev.Object.Value != val.Value {
			t.Errorf("Event object = %v, want %v", ev.Object, val)
		}
	case <-time.After(2 * time.Second):
		t.Error("Timeout waiting for watch event")
	}
}

func TestWatchInitialReplay(t *testing.T) {
	s := setupStore(t)
	defer s.Close()

	kind := "test"

	initialData := map[string]TestData{
		"init1": {Name: "first", Value: 1},
		"init2": {Name: "second", Value: 2},
	}
	for k, v := range initialData {
		if _, err := s.Set(kind, k, v); err != nil {
			t.Fatalf("Set() error = %v", err)
		}
	}

	ch, cancel, err := s.Watch(kind, store.WithInitialReplay[TestData]())
	if err != nil {
		t.Fatalf("Watch() error = %v", err)
	}
	defer cancel()

	received := make(map[string]TestData)
	timeout := time.After(2 * time.Second)
	for len(received) < len(initialData) {
		select {
		case ev := <-ch:
			if ev.EventType != store.EventTypeCreate {
				t.Errorf("Initial event type = %s, want %s", ev.EventType, store.EventTypeCreate)
			}
			received[ev.Name] = ev.Object
		case <-timeout:
			t.Fatalf("Timeout waiting for initial events, got %d/%d", len(received), len(initialData))
		}
	}

	for k, want := range initialData {
		got, ok := received[k]
		if !ok {
			t.Errorf("Initial replay missing key %s", k)
			continue
		}
		if got.Name != want.Name || got.Value != want.Value {
			t.Errorf("Initial replay [%s] = %v, want %v", k, got, want)
		}
	}
}

func TestWatchNoOpNoEvent(t *testing.T) {
	s := setupStore(t)
	defer s.Close()

	kind := "test"
	key := "noop_key"

	val := TestData{Name: "noop", Value: 1}
	_, err := s.Set(kind, key, val)
	if err != nil {
		t.Fatalf("Set() error = %v", err)
	}

	ch, cancel, err := s.Watch(kind)
	if err != nil {
		t.Fatalf("Watch() error = %v", err)
	}
	defer cancel()

	go func() {
		time.Sleep(100 * time.Millisecond)
		_, _ = s.Set(kind, key, val)
	}()

	select {
	case ev := <-ch:
		t.Errorf("Received unexpected event for no-op: %+v", ev)
	case <-time.After(500 * time.Millisecond):
		// Expected: no event
	}
}

func TestClose(t *testing.T) {
	s := setupStore(t)

	ch, cancel, err := s.Watch("test")
	if err != nil {
		t.Fatalf("Watch() error = %v", err)
	}

	err = s.Close()
	if err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	_, ok := <-ch
	if ok {
		t.Error("Watch channel should be closed after Close()")
	}

	cancel()
}

func TestDump(t *testing.T) {
	s := setupStore(t)
	defer s.Close()

	kind := "test"
	for i := 0; i < 3; i++ {
		key := fmt.Sprintf("item%d", i)
		val := TestData{Name: key, Value: i}
		if _, err := s.Set(kind, key, val); err != nil {
			t.Fatalf("Set() error = %v", err)
		}
	}

	dump := s.Dump()
	if dump == "" {
		t.Error("Dump() returned empty string")
	}
	t.Logf("Dump output:\n%s", dump)
}

func TestMultipleKinds(t *testing.T) {
	s := setupStore(t)
	defer s.Close()

	kinds := []string{"kind1", "kind2", "kind3"}
	for _, kind := range kinds {
		for i := 0; i < 5; i++ {
			key := fmt.Sprintf("key%d", i)
			val := TestData{Name: kind, Value: i}
			if _, err := s.Set(kind, key, val); err != nil {
				t.Fatalf("Set() error = %v", err)
			}
		}
	}

	for _, kind := range kinds {
		count, err := s.Count(kind)
		if err != nil {
			t.Fatalf("Count() error = %v", err)
		}
		if count != 5 {
			t.Errorf("Count(%s) = %d, want 5", kind, count)
		}

		list, err := s.List(kind)
		if err != nil {
			t.Fatalf("List() error = %v", err)
		}
		for _, v := range list {
			if v.Name != kind {
				t.Errorf("List(%s) contains item from wrong kind: %s", kind, v.Name)
			}
		}
	}
}

func TestPersistence(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "persist.db")

	s1, err := New[TestData](Options{
		DSN:   "file:" + dbPath,
		Codec: &codec.JSON{},
	})
	if err != nil {
		t.Fatalf("Failed to create store: %v", err)
	}

	kind := "persist"
	key := "data"
	val := TestData{Name: "persisted", Value: 123}
	_, err = s1.Set(kind, key, val)
	if err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	s1.Close()

	s2, err := New[TestData](Options{
		DSN:   "file:" + dbPath,
		Codec: &codec.JSON{},
	})
	if err != nil {
		t.Fatalf("Failed to reopen store: %v", err)
	}
	defer s2.Close()

	got, ok, err := s2.Get(kind, key)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if !ok {
		t.Error("Get() returned ok=false after reopening")
	}
	if got.Name != val.Name || got.Value != val.Value {
		t.Errorf("Get() after reopen = %v, want %v", got, val)
	}
}

// Benchmarks
func BenchmarkSet(b *testing.B) {
	tmpDir := b.TempDir()
	s, _ := New[TestData](Options{
		DSN:   "file:" + filepath.Join(tmpDir, "bench.db"),
		Codec: &codec.JSON{},
	})
	defer s.Close()

	kind := "bench"
	val := TestData{Name: "benchmark", Value: 42}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		key := fmt.Sprintf("key%d", i)
		_, _ = s.Set(kind, key, val)
	}
}

func BenchmarkGet(b *testing.B) {
	tmpDir := b.TempDir()
	s, _ := New[TestData](Options{
		DSN:   "file:" + filepath.Join(tmpDir, "bench.db"),
		Codec: &codec.JSON{},
	})
	defer s.Close()

	kind := "bench"
	key := "key1"
	val := TestData{Name: "benchmark", Value: 42}
	_, _ = s.Set(kind, key, val)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _, _ = s.Get(kind, key)
	}
}

func BenchmarkSetFn(b *testing.B) {
	tmpDir := b.TempDir()
	s, _ := New[TestData](Options{
		DSN:   "file:" + filepath.Join(tmpDir, "bench.db"),
		Codec: &codec.JSON{},
	})
	defer s.Close()

	kind := "bench"
	key := "counter"

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = s.SetFn(kind, key, func(v TestData) (TestData, error) {
			v.Value++
			return v, nil
		})
	}
}
