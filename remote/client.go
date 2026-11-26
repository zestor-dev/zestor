package remote

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"sync"

	"connectrpc.com/connect"

	"github.com/zestor-dev/zestor/codec"
	storev1 "github.com/zestor-dev/zestor/remote/gen/zestor/store/v1"
	"github.com/zestor-dev/zestor/remote/gen/zestor/store/v1/storev1connect"
	"github.com/zestor-dev/zestor/store"
)

// Client is a store.Store backed by a server built with NewHandler. See the
// package documentation for where it differs from a local store.
type Client[T any] struct {
	rpc   storev1connect.StoreServiceClient
	codec codec.Codec
	known map[string]error

	mu       sync.Mutex
	closed   bool
	inflight sync.WaitGroup
	watches  map[*watch[T]]struct{}
}

var _ store.Store[struct{}] = (*Client[struct{}])(nil)

// NewClient returns a client for the server at baseURL, for example
// "https://zestor.example.com". Values are encoded with c, which must match
// the server's codec.
func NewClient[T any](baseURL string, c codec.Codec, opts ...Option) *Client[T] {
	o := buildOptions(opts)
	known := make(map[string]error, len(o.known))
	for _, err := range o.known {
		known[err.Error()] = err
	}
	return &Client[T]{
		rpc:     storev1connect.NewStoreServiceClient(o.httpClient, baseURL, o.clientOpts...),
		codec:   c,
		known:   known,
		watches: make(map[*watch[T]]struct{}),
	}
}

// guard rejects calls on a closed client and honours an already-cancelled
// context before any network I/O. A call that passes is waited for by Close.
func (c *Client[T]) guard(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return store.ErrClosed
	}
	c.inflight.Add(1)
	return nil
}

func (c *Client[T]) done() { c.inflight.Done() }

// fail maps an RPC error back to a store error. A call that failed because
// its own context ended reports the context's error.
func (c *Client[T]) fail(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return fromError(err, c.known)
}

func (c *Client[T]) decode(b []byte) (T, error) {
	var v T
	if err := c.codec.Unmarshal(b, &v); err != nil {
		return v, fmt.Errorf("decode value: %w", err)
	}
	return v, nil
}

func (c *Client[T]) Get(ctx context.Context, kind, key string) (T, bool, error) {
	var zero T
	if err := c.guard(ctx); err != nil {
		return zero, false, err
	}
	defer c.done()
	resp, err := c.rpc.Get(ctx, connect.NewRequest(&storev1.GetRequest{Kind: kind, Key: key}))
	if err != nil {
		return zero, false, c.fail(ctx, err)
	}
	if !resp.Msg.GetFound() {
		return zero, false, nil
	}
	v, err := c.decode(resp.Msg.GetValue())
	if err != nil {
		return zero, false, err
	}
	return v, true, nil
}

func (c *Client[T]) list(ctx context.Context, kind string) ([]*storev1.Entry, error) {
	if err := c.guard(ctx); err != nil {
		return nil, err
	}
	defer c.done()
	resp, err := c.rpc.List(ctx, connect.NewRequest(&storev1.ListRequest{Kind: kind}))
	if err != nil {
		return nil, c.fail(ctx, err)
	}
	return resp.Msg.GetEntries(), nil
}

// List fetches the whole kind in one call and applies filters on the client.
// The single call is one snapshot of the backend's read. For one page of a
// large kind, use ListPage.
func (c *Client[T]) List(ctx context.Context, kind string, filter ...store.FilterFunc[T]) (map[string]T, error) {
	entries, err := c.list(ctx, kind)
	if err != nil {
		return nil, err
	}
	out := make(map[string]T, len(entries))
	for _, e := range entries {
		v, err := c.decode(e.GetValue())
		if err != nil {
			return nil, err
		}
		include := true
		for _, f := range filter {
			if f != nil && !f(e.GetKey(), v) {
				include = false
				break
			}
		}
		if include {
			out[e.GetKey()] = v
		}
	}
	return out, nil
}

func (c *Client[T]) Count(ctx context.Context, kind string) (int, error) {
	if err := c.guard(ctx); err != nil {
		return 0, err
	}
	defer c.done()
	resp, err := c.rpc.Count(ctx, connect.NewRequest(&storev1.CountRequest{Kind: kind}))
	if err != nil {
		return 0, c.fail(ctx, err)
	}
	return int(resp.Msg.GetCount()), nil
}

func (c *Client[T]) Keys(ctx context.Context, kind string) ([]string, error) {
	if err := c.guard(ctx); err != nil {
		return nil, err
	}
	defer c.done()
	resp, err := c.rpc.Keys(ctx, connect.NewRequest(&storev1.KeysRequest{Kind: kind}))
	if err != nil {
		return nil, c.fail(ctx, err)
	}
	return resp.Msg.GetKeys(), nil
}

func (c *Client[T]) Values(ctx context.Context, kind string) ([]store.KeyValue[T], error) {
	entries, err := c.list(ctx, kind)
	if err != nil {
		return nil, err
	}
	out := make([]store.KeyValue[T], 0, len(entries))
	for _, e := range entries {
		v, err := c.decode(e.GetValue())
		if err != nil {
			return nil, err
		}
		out = append(out, store.KeyValue[T]{Key: e.GetKey(), Value: v})
	}
	return out, nil
}

func (c *Client[T]) GetAll(ctx context.Context) (map[string]map[string]T, error) {
	if err := c.guard(ctx); err != nil {
		return nil, err
	}
	defer c.done()
	resp, err := c.rpc.GetAll(ctx, connect.NewRequest(&storev1.GetAllRequest{}))
	if err != nil {
		return nil, c.fail(ctx, err)
	}
	out := make(map[string]map[string]T, len(resp.Msg.GetKinds()))
	for _, k := range resp.Msg.GetKinds() {
		m := make(map[string]T, len(k.GetEntries()))
		for _, e := range k.GetEntries() {
			v, err := c.decode(e.GetValue())
			if err != nil {
				return nil, err
			}
			m[e.GetKey()] = v
		}
		out[k.GetKind()] = m
	}
	return out, nil
}

func pageLimit(limit int) error {
	if limit < 1 || limit > math.MaxInt32 {
		return store.ErrPageLimit
	}
	return nil
}

// ListPage reads one page of kind in ascending key order. pageToken is empty
// to start at the first key, or the token returned with the previous page.
// limit is the most entries to return and must be positive. The returned
// token is empty on the last page.
//
// A walk of several pages is not one snapshot: a bulk write can land between
// pages. List is the single-call read.
func (c *Client[T]) ListPage(ctx context.Context, kind, pageToken string, limit int) ([]store.KeyValue[T], string, error) {
	if err := pageLimit(limit); err != nil {
		return nil, "", err
	}
	if err := c.guard(ctx); err != nil {
		return nil, "", err
	}
	defer c.done()
	resp, err := c.rpc.List(ctx, connect.NewRequest(&storev1.ListRequest{
		Kind: kind, PageSize: int32(limit), PageToken: pageToken,
	}))
	if err != nil {
		return nil, "", c.fail(ctx, err)
	}
	out := make([]store.KeyValue[T], 0, len(resp.Msg.GetEntries()))
	for _, e := range resp.Msg.GetEntries() {
		v, err := c.decode(e.GetValue())
		if err != nil {
			return nil, "", err
		}
		out = append(out, store.KeyValue[T]{Key: e.GetKey(), Value: v})
	}
	return out, resp.Msg.GetNextPageToken(), nil
}

// KeysPage reads one page of keys in ascending order. pageToken and limit
// are as for ListPage.
func (c *Client[T]) KeysPage(ctx context.Context, kind, pageToken string, limit int) ([]string, string, error) {
	if err := pageLimit(limit); err != nil {
		return nil, "", err
	}
	if err := c.guard(ctx); err != nil {
		return nil, "", err
	}
	defer c.done()
	resp, err := c.rpc.Keys(ctx, connect.NewRequest(&storev1.KeysRequest{
		Kind: kind, PageSize: int32(limit), PageToken: pageToken,
	}))
	if err != nil {
		return nil, "", c.fail(ctx, err)
	}
	return resp.Msg.GetKeys(), resp.Msg.GetNextPageToken(), nil
}

// GetAllPage reads one page of every kind in ascending (kind, key) order.
// cursor is nil to start at the beginning, or the cursor returned with the
// previous page. The returned cursor is nil on the last page.
func (c *Client[T]) GetAllPage(ctx context.Context, cursor *store.PageCursor, limit int) ([]store.KindValue[T], *store.PageCursor, error) {
	if err := pageLimit(limit); err != nil {
		return nil, nil, err
	}
	if err := c.guard(ctx); err != nil {
		return nil, nil, err
	}
	defer c.done()
	req := &storev1.GetAllRequest{PageSize: int32(limit)}
	if cursor != nil {
		req.PageToken = &storev1.GetAllCursor{Kind: cursor.Kind, Key: cursor.Key}
	}
	resp, err := c.rpc.GetAll(ctx, connect.NewRequest(req))
	if err != nil {
		return nil, nil, c.fail(ctx, err)
	}
	var out []store.KindValue[T]
	for _, k := range resp.Msg.GetKinds() {
		for _, e := range k.GetEntries() {
			v, err := c.decode(e.GetValue())
			if err != nil {
				return nil, nil, err
			}
			out = append(out, store.KindValue[T]{Kind: k.GetKind(), Key: e.GetKey(), Value: v})
		}
	}
	var next *store.PageCursor
	if cur := resp.Msg.GetNextPageToken(); cur != nil {
		next = &store.PageCursor{Kind: cur.GetKind(), Key: cur.GetKey()}
	}
	return out, next, nil
}

func (c *Client[T]) Set(ctx context.Context, kind, key string, value T) (store.SetResult, error) {
	if err := c.guard(ctx); err != nil {
		return store.SetUnchanged, err
	}
	defer c.done()
	b, err := c.codec.Marshal(value)
	if err != nil {
		return store.SetUnchanged, err
	}
	resp, err := c.rpc.Set(ctx, connect.NewRequest(&storev1.SetRequest{Kind: kind, Key: key, Value: b}))
	if err != nil {
		return store.SetUnchanged, c.fail(ctx, err)
	}
	switch resp.Msg.GetResult() {
	case storev1.SetResult_SET_RESULT_CREATED:
		return store.SetCreated, nil
	case storev1.SetResult_SET_RESULT_UPDATED:
		return store.SetUpdated, nil
	case storev1.SetResult_SET_RESULT_UNCHANGED:
		return store.SetUnchanged, nil
	}
	return store.SetUnchanged, fmt.Errorf("server returned set result %v", resp.Msg.GetResult())
}

// SetFn reads the value, applies fn on the client, and writes the result only
// if the stored value is still the one fn saw. When another writer got there
// first, it reads again and repeats, so fn may run more than once. fn does not
// hold the store's write lock and may call the store.
func (c *Client[T]) SetFn(ctx context.Context, kind, key string, fn func(v T) (T, error)) (bool, error) {
	if err := c.guard(ctx); err != nil {
		return false, err
	}
	defer c.done()
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		got, err := c.rpc.Get(ctx, connect.NewRequest(&storev1.GetRequest{Kind: kind, Key: key}))
		if err != nil {
			return false, c.fail(ctx, err)
		}
		if !got.Msg.GetFound() {
			return false, store.ErrKeyNotFound
		}
		cur, err := c.decode(got.Msg.GetValue())
		if err != nil {
			return false, err
		}
		next, err := fn(cur)
		if err != nil {
			return false, err
		}
		b, err := c.codec.Marshal(next)
		if err != nil {
			return false, err
		}
		resp, err := c.rpc.CompareAndSet(ctx, connect.NewRequest(&storev1.CompareAndSetRequest{
			Kind:     kind,
			Key:      key,
			Expected: got.Msg.GetValue(),
			Value:    b,
		}))
		if err != nil {
			err = c.fail(ctx, err)
			if errors.Is(err, errConflict) {
				continue
			}
			return false, err
		}
		return resp.Msg.GetChanged(), nil
	}
}

func (c *Client[T]) SetMany(ctx context.Context, kind string, values map[string]T) error {
	if err := c.guard(ctx); err != nil {
		return err
	}
	defer c.done()
	if len(values) == 0 {
		return nil
	}
	entries := make([]*storev1.Entry, 0, len(values))
	for _, k := range slices.Sorted(maps.Keys(values)) {
		b, err := c.codec.Marshal(values[k])
		if err != nil {
			return err
		}
		entries = append(entries, &storev1.Entry{Key: k, Value: b})
	}
	if _, err := c.rpc.SetMany(ctx, connect.NewRequest(&storev1.SetManyRequest{Kind: kind, Entries: entries})); err != nil {
		return c.fail(ctx, err)
	}
	return nil
}

func (c *Client[T]) Delete(ctx context.Context, kind, key string) (bool, T, error) {
	var zero T
	if err := c.guard(ctx); err != nil {
		return false, zero, err
	}
	defer c.done()
	resp, err := c.rpc.Delete(ctx, connect.NewRequest(&storev1.DeleteRequest{Kind: kind, Key: key}))
	if err != nil {
		return false, zero, c.fail(ctx, err)
	}
	if !resp.Msg.GetExisted() {
		return false, zero, nil
	}
	prev, err := c.decode(resp.Msg.GetPrevious())
	if err != nil {
		return false, zero, err
	}
	return true, prev, nil
}

// Close ends every watch of this client and waits for calls already in
// flight. A watcher that is waiting receives ErrClosed. Close does not close
// the server's store and does not cancel watcher contexts.
func (c *Client[T]) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	watches := slices.Collect(maps.Keys(c.watches))
	c.mu.Unlock()

	for _, w := range watches {
		w.close()
	}
	c.inflight.Wait()
	return nil
}
