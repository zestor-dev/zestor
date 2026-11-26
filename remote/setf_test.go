package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/zestor-dev/zestor/codec"
	"github.com/zestor-dev/zestor/store"
	"github.com/zestor-dev/zestor/store/gomap"
)

func serveClient[T any](t *testing.T, s store.Store[T], c codec.Codec) *Client[T] {
	t.Helper()
	path, h := NewHandler(s, c)
	mux := http.NewServeMux()
	mux.Handle(path, h)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client := NewClient[T](srv.URL, c, WithHTTPClient(srv.Client()))
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// spacedJSON is a valid JSON codec whose encoding changes on every call.
// Leading spaces do not change the decoded value.
type spacedJSON struct{ n atomic.Int32 }

func (c *spacedJSON) Marshal(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	pad := bytes.Repeat([]byte{' '}, int(c.n.Add(1)))
	return append(pad, b...), nil
}

func (c *spacedJSON) Unmarshal(data []byte, v any) error {
	return json.Unmarshal(data, v)
}

func TestSetFnAllowsUnstableEncoding(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	backend := gomap.NewMemStore[storetestValue](store.StoreOptions[storetestValue]{})
	client := serveClient(t, backend, &spacedJSON{})
	if _, err := client.Set(ctx, "k", "a", storetestValue{N: 1}); err != nil {
		t.Fatal(err)
	}

	calls := 0
	changed, err := client.SetFn(ctx, "k", "a", func(v storetestValue) (storetestValue, error) {
		calls++
		v.N++
		return v, nil
	})
	if err != nil {
		t.Fatalf("SetFn: %v after %d calls", err, calls)
	}
	if !changed {
		t.Fatal("SetFn reported changed=false")
	}
	if calls != 1 {
		t.Fatalf("fn ran %d times, want 1", calls)
	}
	got, ok, err := client.Get(ctx, "k", "a")
	if err != nil || !ok || got.N != 2 {
		t.Fatalf("Get = %+v ok=%v err=%v, want N=2", got, ok, err)
	}
}

// conflictOnce reports one compare-and-set conflict and leaves the stored
// value alone. A concurrent writer can change a key and restore it before
// the client's next read, which is the same observation: conflict, then the
// value this attempt already saw. That has to be a retry.
type conflictOnce struct {
	store.Store[storetestValue]
	n atomic.Int32
}

func (c *conflictOnce) SetFn(ctx context.Context, kind, key string, fn func(storetestValue) (storetestValue, error)) (bool, error) {
	if c.n.Add(1) == 1 {
		return false, errConflict
	}
	return c.Store.SetFn(ctx, kind, key, fn)
}

func TestSetFnRetriesWhenConflictRestoresValue(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	backend := &conflictOnce{Store: gomap.NewMemStore[storetestValue](store.StoreOptions[storetestValue]{})}
	if _, err := backend.Set(ctx, "k", "a", storetestValue{N: 1}); err != nil {
		t.Fatal(err)
	}
	client := serveClient(t, backend, &codec.JSON{})

	calls := 0
	changed, err := client.SetFn(ctx, "k", "a", func(v storetestValue) (storetestValue, error) {
		calls++
		v.N++
		return v, nil
	})
	if err != nil {
		t.Fatalf("SetFn: %v after %d calls", err, calls)
	}
	if !changed {
		t.Fatal("SetFn reported changed=false")
	}
	if calls != 2 {
		t.Fatalf("fn ran %d times, want 2", calls)
	}
	got, ok, err := client.Get(ctx, "k", "a")
	if err != nil || !ok || got.N != 2 {
		t.Fatalf("Get = %+v ok=%v err=%v, want N=2", got, ok, err)
	}
}

type storetestValue struct {
	N int `json:"n"`
}

func TestProtobufPointerRoundTrip(t *testing.T) {
	ctx := context.Background()
	backend := gomap.NewMemStore[*wrapperspb.StringValue](store.StoreOptions[*wrapperspb.StringValue]{})
	client := serveClient(t, backend, &codec.Protobuf{})

	if _, err := client.Set(ctx, "k", "a", wrapperspb.String("protobuf")); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, ok, err := client.Get(ctx, "k", "a")
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if got.GetValue() != "protobuf" {
		t.Fatalf("Get = %q, want protobuf", got.GetValue())
	}
}
