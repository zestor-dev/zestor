package remote

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"net/http"
	"slices"

	"connectrpc.com/connect"

	"github.com/zestor-dev/zestor/codec"
	storev1 "github.com/zestor-dev/zestor/remote/gen/zestor/store/v1"
	"github.com/zestor-dev/zestor/remote/gen/zestor/store/v1/storev1connect"
	"github.com/zestor-dev/zestor/store"
)

// NewHandler serves s. It returns the path to mount the handler on and the
// handler itself. Values are decoded with c, which must match the codec the
// clients use.
//
// The handler does not close s.
func NewHandler[T any](s store.Store[T], c codec.Codec, opts ...Option) (string, http.Handler) {
	o := buildOptions(opts)
	return storev1connect.NewStoreServiceHandler(&server[T]{store: s, codec: c, known: o.known}, o.handlerOpts...)
}

type server[T any] struct {
	store store.Store[T]
	codec codec.Codec
	known []error
}

func (h *server[T]) fail(err error) error { return toConnectError(err, h.known) }

func (h *server[T]) decode(b []byte) (T, error) {
	var v T
	if err := h.codec.Unmarshal(b, &v); err != nil {
		return v, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("decode value: %w", err))
	}
	return v, nil
}

func (h *server[T]) encode(v T) ([]byte, error) {
	b, err := h.codec.Marshal(v)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("encode value: %w", err))
	}
	return b, nil
}

func (h *server[T]) entries(m map[string]T) ([]*storev1.Entry, error) {
	out := make([]*storev1.Entry, 0, len(m))
	for _, k := range slices.Sorted(maps.Keys(m)) {
		b, err := h.encode(m[k])
		if err != nil {
			return nil, err
		}
		out = append(out, &storev1.Entry{Key: k, Value: b})
	}
	return out, nil
}

func (h *server[T]) Get(ctx context.Context, req *connect.Request[storev1.GetRequest]) (*connect.Response[storev1.GetResponse], error) {
	v, ok, err := h.store.Get(ctx, req.Msg.GetKind(), req.Msg.GetKey())
	if err != nil {
		return nil, h.fail(err)
	}
	if !ok {
		return connect.NewResponse(&storev1.GetResponse{}), nil
	}
	b, err := h.encode(v)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&storev1.GetResponse{Found: true, Value: b}), nil
}

func (h *server[T]) List(ctx context.Context, req *connect.Request[storev1.ListRequest]) (*connect.Response[storev1.ListResponse], error) {
	m, err := h.store.List(ctx, req.Msg.GetKind())
	if err != nil {
		return nil, h.fail(err)
	}
	entries, err := h.entries(m)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&storev1.ListResponse{Entries: entries}), nil
}

func (h *server[T]) Count(ctx context.Context, req *connect.Request[storev1.CountRequest]) (*connect.Response[storev1.CountResponse], error) {
	n, err := h.store.Count(ctx, req.Msg.GetKind())
	if err != nil {
		return nil, h.fail(err)
	}
	return connect.NewResponse(&storev1.CountResponse{Count: int64(n)}), nil
}

func (h *server[T]) Keys(ctx context.Context, req *connect.Request[storev1.KeysRequest]) (*connect.Response[storev1.KeysResponse], error) {
	keys, err := h.store.Keys(ctx, req.Msg.GetKind())
	if err != nil {
		return nil, h.fail(err)
	}
	return connect.NewResponse(&storev1.KeysResponse{Keys: keys}), nil
}

func (h *server[T]) GetAll(ctx context.Context, _ *connect.Request[storev1.GetAllRequest]) (*connect.Response[storev1.GetAllResponse], error) {
	all, err := h.store.GetAll(ctx)
	if err != nil {
		return nil, h.fail(err)
	}
	resp := &storev1.GetAllResponse{Kinds: make([]*storev1.KindEntries, 0, len(all))}
	for _, kind := range slices.Sorted(maps.Keys(all)) {
		entries, err := h.entries(all[kind])
		if err != nil {
			return nil, err
		}
		resp.Kinds = append(resp.Kinds, &storev1.KindEntries{Kind: kind, Entries: entries})
	}
	return connect.NewResponse(resp), nil
}

func (h *server[T]) Set(ctx context.Context, req *connect.Request[storev1.SetRequest]) (*connect.Response[storev1.SetResponse], error) {
	v, err := h.decode(req.Msg.GetValue())
	if err != nil {
		return nil, err
	}
	res, err := h.store.Set(ctx, req.Msg.GetKind(), req.Msg.GetKey(), v)
	if err != nil {
		return nil, h.fail(err)
	}
	out := storev1.SetResult_SET_RESULT_UNCHANGED
	switch res {
	case store.SetCreated:
		out = storev1.SetResult_SET_RESULT_CREATED
	case store.SetUpdated:
		out = storev1.SetResult_SET_RESULT_UPDATED
	}
	return connect.NewResponse(&storev1.SetResponse{Result: out}), nil
}

func (h *server[T]) SetMany(ctx context.Context, req *connect.Request[storev1.SetManyRequest]) (*connect.Response[storev1.SetManyResponse], error) {
	values := make(map[string]T, len(req.Msg.GetEntries()))
	for _, e := range req.Msg.GetEntries() {
		if _, dup := values[e.GetKey()]; dup {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("key %q appears more than once", e.GetKey()))
		}
		v, err := h.decode(e.GetValue())
		if err != nil {
			return nil, err
		}
		values[e.GetKey()] = v
	}
	if err := h.store.SetMany(ctx, req.Msg.GetKind(), values); err != nil {
		return nil, h.fail(err)
	}
	return connect.NewResponse(&storev1.SetManyResponse{}), nil
}

func (h *server[T]) CompareAndSet(ctx context.Context, req *connect.Request[storev1.CompareAndSetRequest]) (*connect.Response[storev1.CompareAndSetResponse], error) {
	next, err := h.decode(req.Msg.GetValue())
	if err != nil {
		return nil, err
	}
	expected := req.Msg.GetExpected()
	changed, err := h.store.SetFn(ctx, req.Msg.GetKind(), req.Msg.GetKey(), func(cur T) (T, error) {
		b, err := h.codec.Marshal(cur)
		if err != nil {
			return cur, err
		}
		if !bytes.Equal(b, expected) {
			return cur, errConflict
		}
		return next, nil
	})
	if err != nil {
		return nil, h.fail(err)
	}
	return connect.NewResponse(&storev1.CompareAndSetResponse{Changed: changed}), nil
}

func (h *server[T]) Delete(ctx context.Context, req *connect.Request[storev1.DeleteRequest]) (*connect.Response[storev1.DeleteResponse], error) {
	existed, prev, err := h.store.Delete(ctx, req.Msg.GetKind(), req.Msg.GetKey())
	if err != nil {
		return nil, h.fail(err)
	}
	if !existed {
		return connect.NewResponse(&storev1.DeleteResponse{}), nil
	}
	b, err := h.encode(prev)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&storev1.DeleteResponse{Existed: true, Previous: b}), nil
}

func (h *server[T]) Watch(ctx context.Context, req *connect.Request[storev1.WatchRequest], stream *connect.ServerStream[storev1.WatchResponse]) error {
	var opts []store.WatchOption[T]
	if req.Msg.GetInitialReplay() {
		opts = append(opts, store.WithInitialReplay[T]())
	}
	if types := req.Msg.GetEventTypes(); len(types) > 0 {
		ets := make([]store.EventType, 0, len(types))
		for _, t := range types {
			if t == storev1.EventType_EVENT_TYPE_UNSPECIFIED {
				// Matches no data event. The terminal failure is never
				// filtered, so asking for the error type alone means this.
				ets = append(ets, store.EventTypeError)
				continue
			}
			et, ok := eventTypeFromProto(t)
			if !ok {
				return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("unknown event type %v", t))
			}
			ets = append(ets, et)
		}
		opts = append(opts, store.WithEventTypes[T](ets...))
	}
	if n := req.Msg.GetBufferSize(); n > 0 {
		opts = append(opts, store.WithBufferSize[T](int(n)))
	}

	// The handler's context ends when the client goes away, which ends the
	// store's watch with it.
	ch, err := h.store.Watch(ctx, req.Msg.GetKind(), opts...)
	if err != nil {
		return h.fail(err)
	}
	if err := stream.Send(&storev1.WatchResponse{
		Message: &storev1.WatchResponse_Subscribed{Subscribed: &storev1.Subscribed{}},
	}); err != nil {
		return err
	}

	for ev := range ch {
		if ev.EventType == store.EventTypeError {
			return stream.Send(&storev1.WatchResponse{
				Message: &storev1.WatchResponse_Failure{Failure: failureFor(ev.Err, h.known)},
			})
		}
		b, err := h.encode(ev.Object)
		if err != nil {
			return err
		}
		if err := stream.Send(&storev1.WatchResponse{
			Message: &storev1.WatchResponse_Event{Event: &storev1.Event{
				Type:   eventTypeToProto(ev.EventType),
				Key:    ev.Name,
				Value:  b,
				Replay: ev.Replay,
			}},
		}); err != nil {
			return err
		}
	}
	return nil
}

func eventTypeToProto(et store.EventType) storev1.EventType {
	switch et {
	case store.EventTypeCreate:
		return storev1.EventType_EVENT_TYPE_CREATE
	case store.EventTypeUpdate:
		return storev1.EventType_EVENT_TYPE_UPDATE
	case store.EventTypeDelete:
		return storev1.EventType_EVENT_TYPE_DELETE
	}
	return storev1.EventType_EVENT_TYPE_UNSPECIFIED
}

func eventTypeFromProto(t storev1.EventType) (store.EventType, bool) {
	switch t {
	case storev1.EventType_EVENT_TYPE_CREATE:
		return store.EventTypeCreate, true
	case storev1.EventType_EVENT_TYPE_UPDATE:
		return store.EventTypeUpdate, true
	case storev1.EventType_EVENT_TYPE_DELETE:
		return store.EventTypeDelete, true
	}
	return "", false
}
