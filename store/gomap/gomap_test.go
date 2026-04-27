package gomap

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/zestor-dev/zestor/store"
)

type testData struct {
	Name  string
	Value int
}

func receiveEvent[T any](t *testing.T, ch <-chan *store.Event[T]) *store.Event[T] {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("watch channel closed unexpectedly")
		}
		return ev
	case <-time.After(200 * time.Millisecond):
		t.Fatal("timed out waiting for watch event")
		return nil
	}
}

func assertNoEvent[T any](t *testing.T, ch <-chan *store.Event[T]) {
	t.Helper()
	select {
	case ev, ok := <-ch:
		t.Fatalf("received unexpected event %v, ok=%v", ev, ok)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestMemStoreSet(t *testing.T) {
	tests := []struct {
		name    string
		kind    string
		key     string
		value   any
		want    bool
		wantErr bool
	}{
		{
			name:  "creates string value",
			kind:  "kind",
			key:   "k1",
			value: "v1",
			want:  true,
		},
		{
			name:  "creates zero value",
			kind:  "kind",
			key:   "k1",
			value: "",
			want:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ms := NewMemStore(store.StoreOptions[any]{})
			created, gotErr := ms.Set(tt.kind, tt.key, tt.value)
			if gotErr != nil {
				if !tt.wantErr {
					t.Errorf("Set() failed: %v", gotErr)
				}
				return
			}
			if tt.wantErr {
				t.Fatal("Set() succeeded unexpectedly")
			}
			if tt.want != created {
				t.Errorf("Set() = %v, want %v", created, tt.want)
			}
			got, ok, err := ms.Get(tt.kind, tt.key)
			if !ok {
				t.Fatal("Get() returned ok=false")
			}
			if got != tt.value {
				t.Errorf("Get() = %v, want %v", got, tt.value)
			}
			if err != nil {
				t.Errorf("Get() failed: %v", err)
			}
		})
	}
}

func TestMemStoreSetSuppressesNoopUpdateEvent(t *testing.T) {
	ms := NewMemStore(store.StoreOptions[testData]{})

	ch, cancel, err := ms.Watch("kind", store.WithEventTypes[testData](store.EventTypeUpdate))
	if err != nil {
		t.Fatalf("Watch() error = %v", err)
	}
	defer cancel()

	value := testData{Name: "same", Value: 1}
	if created, err := ms.Set("kind", "key", value); err != nil || !created {
		t.Fatalf("Set() created = %v, error = %v", created, err)
	}
	if created, err := ms.Set("kind", "key", value); err != nil || created {
		t.Fatalf("Set() created = %v, error = %v", created, err)
	}

	assertNoEvent(t, ch)
}

func TestMemStoreWatchAllowsNilOptions(t *testing.T) {
	ms := NewMemStore(store.StoreOptions[testData]{})

	ch, cancel, err := ms.Watch("kind", nil)
	if err != nil {
		t.Fatalf("Watch() error = %v", err)
	}
	defer cancel()

	if _, err := ms.Set("kind", "key", testData{Name: "created"}); err != nil {
		t.Fatalf("Set() error = %v", err)
	}
	ev := receiveEvent(t, ch)
	if ev.EventType != store.EventTypeCreate || ev.Name != "key" {
		t.Fatalf("event = %#v, want create for key", ev)
	}
}

func TestMemStoreSetFnValidatesAndSuppressesNoop(t *testing.T) {
	validateErr := errors.New("value must be non-negative")
	ms := NewMemStore(store.StoreOptions[testData]{
		ValidateFns: map[string]store.ValidateFunc[testData]{
			"kind": func(v testData) error {
				if v.Value < 0 {
					return validateErr
				}
				return nil
			},
		},
	})

	if _, err := ms.Set("kind", "key", testData{Name: "valid", Value: 1}); err != nil {
		t.Fatalf("Set() error = %v", err)
	}

	if _, err := ms.SetFn("kind", "key", func(v testData) (testData, error) {
		v.Value = -1
		return v, nil
	}); !errors.Is(err, validateErr) {
		t.Fatalf("SetFn() error = %v, want %v", err, validateErr)
	}

	got, ok, err := ms.Get("kind", "key")
	if err != nil || !ok {
		t.Fatalf("Get() error = %v, ok = %v", err, ok)
	}
	if got.Value != 1 {
		t.Fatalf("Get() value = %d, want original value 1", got.Value)
	}

	ch, cancel, err := ms.Watch("kind", store.WithEventTypes[testData](store.EventTypeUpdate))
	if err != nil {
		t.Fatalf("Watch() error = %v", err)
	}
	defer cancel()

	if changed, err := ms.SetFn("kind", "key", func(v testData) (testData, error) {
		return v, nil
	}); err != nil || changed {
		t.Fatalf("SetFn() changed = %v, error = %v", changed, err)
	}
	assertNoEvent(t, ch)
}

func TestMemStoreSetAllSuppressesNoopUpdateEvent(t *testing.T) {
	ms := NewMemStore(store.StoreOptions[testData]{})
	if _, err := ms.Set("kind", "same", testData{Name: "same", Value: 1}); err != nil {
		t.Fatalf("Set() error = %v", err)
	}

	ch, cancel, err := ms.Watch("kind")
	if err != nil {
		t.Fatalf("Watch() error = %v", err)
	}
	defer cancel()

	if err := ms.SetAll("kind", map[string]testData{
		"same": {Name: "same", Value: 1},
		"new":  {Name: "new", Value: 2},
	}); err != nil {
		t.Fatalf("SetAll() error = %v", err)
	}

	ev := receiveEvent(t, ch)
	if ev.EventType != store.EventTypeCreate || ev.Name != "new" {
		t.Fatalf("event = %#v, want one create event for new key", ev)
	}
	assertNoEvent(t, ch)
}

func TestMemStoreGetAllReturnsClone(t *testing.T) {
	ms := NewMemStore(store.StoreOptions[testData]{})
	value := testData{Name: "stored", Value: 1}
	if _, err := ms.Set("kind", "key", value); err != nil {
		t.Fatalf("Set() error = %v", err)
	}

	all, err := ms.GetAll()
	if err != nil {
		t.Fatalf("GetAll() error = %v", err)
	}
	if !reflect.DeepEqual(all, map[string]map[string]testData{"kind": {"key": value}}) {
		t.Fatalf("GetAll() = %#v", all)
	}

	all["kind"]["key"] = testData{Name: "mutated", Value: 99}
	got, ok, err := ms.Get("kind", "key")
	if err != nil || !ok {
		t.Fatalf("Get() error = %v, ok = %v", err, ok)
	}
	if got != value {
		t.Fatalf("Get() = %#v, want %#v", got, value)
	}
}

func TestMemStoreCloseClosesWatchersAndRejectsOperations(t *testing.T) {
	ms := NewMemStore(store.StoreOptions[testData]{})
	ch, cancel, err := ms.Watch("kind")
	if err != nil {
		t.Fatalf("Watch() error = %v", err)
	}
	defer cancel()

	if err := ms.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if _, ok := <-ch; ok {
		t.Fatal("watch channel remained open after Close()")
	}
	if _, err := ms.Set("kind", "key", testData{}); !errors.Is(err, store.ErrClosed) {
		t.Fatalf("Set() error = %v, want %v", err, store.ErrClosed)
	}
}
