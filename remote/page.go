package remote

import (
	"cmp"
	"context"
	"errors"
	"maps"
	"math"
	"slices"
	"sort"
	"strings"

	"connectrpc.com/connect"

	storev1 "github.com/zestor-dev/zestor/remote/gen/zestor/store/v1"
	"github.com/zestor-dev/zestor/store"
)

// badPageSize rejects a negative page. Zero is the unpaged call.
func badPageSize() error {
	return connect.NewError(connect.CodeInvalidArgument, errors.New("page_size must be >= 0"))
}

// pageTokenPrefix keeps a continuation token distinct from "no token",
// including the token for the empty key ("v1:") and including JSON, which
// omits an empty string. Callers pass the token back unchanged.
const pageTokenPrefix = "v1:"

func encodeKeyToken(key string) string { return pageTokenPrefix + key }

func decodeKeyToken(token string) (*string, error) {
	if token == "" {
		return nil, nil
	}
	key, ok := strings.CutPrefix(token, pageTokenPrefix)
	if !ok {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("bad page_token"))
	}
	return &key, nil
}

func askLimit(limit int) int {
	if limit < math.MaxInt {
		return limit + 1
	}
	return limit
}

func (h *server[T]) encodeKVs(rows []store.KeyValue[T]) ([]*storev1.Entry, error) {
	out := make([]*storev1.Entry, len(rows))
	for i, row := range rows {
		b, err := h.encode(row.Value)
		if err != nil {
			return nil, err
		}
		out[i] = &storev1.Entry{Key: row.Key, Value: b}
	}
	return out, nil
}

func (h *server[T]) group(rows []store.KindValue[T]) ([]*storev1.KindEntries, error) {
	var out []*storev1.KindEntries
	for _, row := range rows {
		b, err := h.encode(row.Value)
		if err != nil {
			return nil, err
		}
		if len(out) == 0 || out[len(out)-1].GetKind() != row.Kind {
			out = append(out, &storev1.KindEntries{Kind: row.Kind})
		}
		last := out[len(out)-1]
		last.Entries = append(last.Entries, &storev1.Entry{Key: row.Key, Value: b})
	}
	return out, nil
}

func (h *server[T]) listPage(ctx context.Context, kind string, after *string, limit int) ([]store.KeyValue[T], bool, error) {
	if p, ok := h.store.(store.Pager[T]); ok {
		rows, err := p.ListPage(ctx, kind, after, askLimit(limit))
		if err != nil {
			return nil, false, err
		}
		if len(rows) > limit {
			return rows[:limit], true, nil
		}
		return rows, false, nil
	}
	m, err := h.store.List(ctx, kind)
	if err != nil {
		return nil, false, err
	}
	rows := make([]store.KeyValue[T], 0, len(m))
	for k, v := range m {
		rows = append(rows, store.KeyValue[T]{Key: k, Value: v})
	}
	slices.SortFunc(rows, func(a, b store.KeyValue[T]) int { return cmp.Compare(a.Key, b.Key) })
	page, more := cutPage(rows, after, limit, func(row store.KeyValue[T]) string { return row.Key })
	return page, more, nil
}

func (h *server[T]) keysPage(ctx context.Context, kind string, after *string, limit int) ([]string, bool, error) {
	if p, ok := h.store.(store.Pager[T]); ok {
		keys, err := p.KeysPage(ctx, kind, after, askLimit(limit))
		if err != nil {
			return nil, false, err
		}
		if len(keys) > limit {
			return keys[:limit], true, nil
		}
		return keys, false, nil
	}
	keys, err := h.store.Keys(ctx, kind)
	if err != nil {
		return nil, false, err
	}
	slices.Sort(keys)
	page, more := cutPage(keys, after, limit, func(k string) string { return k })
	return page, more, nil
}

func (h *server[T]) getAllPage(ctx context.Context, after *store.PageCursor, limit int) ([]store.KindValue[T], bool, error) {
	if p, ok := h.store.(store.Pager[T]); ok {
		rows, err := p.GetAllPage(ctx, after, askLimit(limit))
		if err != nil {
			return nil, false, err
		}
		if len(rows) > limit {
			return rows[:limit], true, nil
		}
		return rows, false, nil
	}
	all, err := h.store.GetAll(ctx)
	if err != nil {
		return nil, false, err
	}
	rows := make([]store.KindValue[T], 0)
	for _, kind := range slices.Sorted(maps.Keys(all)) {
		for _, key := range slices.Sorted(maps.Keys(all[kind])) {
			rows = append(rows, store.KindValue[T]{Kind: kind, Key: key, Value: all[kind][key]})
		}
	}
	start := 0
	if after != nil {
		start = sort.Search(len(rows), func(i int) bool {
			if rows[i].Kind != after.Kind {
				return rows[i].Kind > after.Kind
			}
			return rows[i].Key > after.Key
		})
	}
	page, more := cutAt(rows, start, limit)
	return page, more, nil
}

// cutPage returns the page of rows strictly after `after`. rows must be
// sorted by the key function, ascending.
func cutPage[T any](rows []T, after *string, limit int, key func(T) string) ([]T, bool) {
	start := 0
	if after != nil {
		start = sort.Search(len(rows), func(i int) bool { return key(rows[i]) > *after })
	}
	return cutAt(rows, start, limit)
}

func cutAt[T any](rows []T, start, limit int) ([]T, bool) {
	if start > len(rows) {
		start = len(rows)
	}
	end := start + limit
	more := true
	if end >= len(rows) {
		end = len(rows)
		more = false
	}
	out := make([]T, end-start)
	copy(out, rows[start:end])
	return out, more
}
