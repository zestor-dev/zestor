package remote_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"

	"github.com/zestor-dev/zestor/codec"
	"github.com/zestor-dev/zestor/remote"
	storev1 "github.com/zestor-dev/zestor/remote/gen/zestor/store/v1"
	"github.com/zestor-dev/zestor/remote/gen/zestor/store/v1/storev1connect"
	"github.com/zestor-dev/zestor/store"
	"github.com/zestor-dev/zestor/store/gomap"
	"github.com/zestor-dev/zestor/store/storetest"
)

// plainStore hides Pager, so the handler pages a full List instead.
type plainStore[T any] struct{ store.Store[T] }

func TestPages(t *testing.T) {
	for _, p := range protocols {
		t.Run(p.name, func(t *testing.T) {
			t.Run("pager", func(t *testing.T) { checkPages(t, p, false) })
			t.Run("fallback", func(t *testing.T) { checkPages(t, p, true) })
		})
	}
}

func checkPages(t *testing.T, p protocol, fallback bool) {
	t.Helper()
	ctx := context.Background()
	mem := gomap.NewMemStore[storetest.Value](store.StoreOptions[storetest.Value]{})
	t.Cleanup(func() { _ = mem.Close() })
	for _, kv := range []struct{ kind, key string }{
		{"k", ""}, {"k", "b"}, {"k", "a"}, {"k", "c"}, {"other", "a"},
	} {
		if _, err := mem.Set(ctx, kv.kind, kv.key, storetest.Value{Name: kv.key}); err != nil {
			t.Fatal(err)
		}
	}
	var backend store.Store[storetest.Value] = mem
	if fallback {
		backend = plainStore[storetest.Value]{mem}
	}
	path, handler := remote.NewHandler(backend, &codec.JSON{})
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	srv := httptest.NewUnstartedServer(mux)
	if p.http2 {
		srv.EnableHTTP2 = true
		srv.StartTLS()
	} else {
		srv.Start()
	}
	t.Cleanup(srv.Close)

	c := remote.NewClient[storetest.Value](srv.URL, &codec.JSON{},
		remote.WithHTTPClient(srv.Client()),
		remote.WithClientOptions(p.opts...))
	t.Cleanup(func() { _ = c.Close() })

	listed, err := c.List(ctx, "k")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(listed) != 4 {
		t.Fatalf("List len = %d, want 4", len(listed))
	}

	if _, _, err := c.ListPage(ctx, "k", "", 0); !errors.Is(err, store.ErrPageLimit) {
		t.Errorf("ListPage limit 0 returned %v, want ErrPageLimit", err)
	}
	if _, _, err := c.ListPage(ctx, "k", "nope", 1); err == nil || !strings.Contains(err.Error(), "bad page_token") {
		t.Errorf("ListPage bad token returned %v", err)
	}

	token := ""
	var got []string
	for range 6 {
		page, next, err := c.ListPage(ctx, "k", token, 1)
		if err != nil {
			t.Fatalf("ListPage: %v", err)
		}
		if len(page) > 1 {
			t.Fatalf("ListPage returned %d entries, limit is 1", len(page))
		}
		for _, e := range page {
			got = append(got, e.Key)
			if e.Value.Name != e.Key {
				t.Fatalf("entry %q has name %q", e.Key, e.Value.Name)
			}
		}
		if next == "" {
			break
		}
		if next == token {
			t.Fatal("page token did not advance")
		}
		token = next
	}
	if !slices.Equal(got, []string{"", "a", "b", "c"}) {
		t.Fatalf("ListPage walk = %#v", got)
	}

	// A zero page size is the whole kind, and the token is ignored.
	rpc := storev1connect.NewStoreServiceClient(srv.Client(), srv.URL, p.opts...)
	resp, err := rpc.List(ctx, connect.NewRequest(&storev1.ListRequest{
		Kind: "k", PageToken: token,
	}))
	if err != nil {
		t.Fatalf("unpaged List: %v", err)
	}
	if resp.Msg.GetNextPageToken() != "" {
		t.Fatalf("unpaged List next token = %q", resp.Msg.GetNextPageToken())
	}
	if len(resp.Msg.GetEntries()) != 4 || resp.Msg.GetEntries()[0].GetKey() != "" {
		t.Fatalf("unpaged List keys start %#v", keysOf(resp.Msg.GetEntries()))
	}
	neg, err := rpc.List(ctx, connect.NewRequest(&storev1.ListRequest{Kind: "k", PageSize: -1}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument || neg != nil {
		t.Errorf("negative page_size returned %v, %v", neg, err)
	}

	got = nil
	token = ""
	for range 6 {
		keys, next, err := c.KeysPage(ctx, "k", token, 2)
		if err != nil {
			t.Fatalf("KeysPage: %v", err)
		}
		got = append(got, keys...)
		if next == "" {
			break
		}
		token = next
	}
	if !slices.Equal(got, []string{"", "a", "b", "c"}) {
		t.Fatalf("KeysPage walk = %#v", got)
	}

	var cur *store.PageCursor
	got = nil
	for range 6 {
		rows, next, err := c.GetAllPage(ctx, cur, 2)
		if err != nil {
			t.Fatalf("GetAllPage: %v", err)
		}
		if len(rows) > 2 {
			t.Fatalf("GetAllPage returned %d entries, limit is 2", len(rows))
		}
		for _, e := range rows {
			got = append(got, e.Kind+"/"+e.Key)
		}
		if next == nil {
			break
		}
		cur = next
	}
	if !slices.Equal(got, []string{"k/", "k/a", "k/b", "k/c", "other/a"}) {
		t.Fatalf("GetAllPage walk = %#v", got)
	}

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := c.ListPage(canceled, "k", "", 1); !errors.Is(err, context.Canceled) {
		t.Errorf("ListPage on a canceled context returned %v, want Canceled", err)
	}
}

func keysOf(entries []*storev1.Entry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.GetKey()
	}
	return out
}
