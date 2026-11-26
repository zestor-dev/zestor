// Package remote serves a store.Store over the network, and provides a Client
// that is itself a store.Store.
//
// The API is the Connect service defined in proto/zestor/store/v1. One handler
// speaks gRPC, gRPC-Web, and the Connect protocol (plain HTTP with JSON or
// binary bodies), so it can be called from gRPC clients in any language, from
// browsers, and with curl.
//
//	path, handler := remote.NewHandler(s, &codec.JSON{})
//	mux.Handle(path, handler)
//
//	c := remote.NewClient[User]("https://zestor.example.com", &codec.JSON{})
//	defer c.Close()
//
// Values cross the network encoded with the codec. The server decodes them
// into its own value type before calling the store, so the store's
// validation and change detection apply to remote writes.
//
// A Client keeps the store contract, with these differences:
//
//   - SetFn runs fn on the client, against a value it has just read, and
//     writes the result only if the stored value still decodes equal to that
//     one. A codec may encode the same value as different bytes. If the
//     stored value changed, fn runs again, so it may run more than once and
//     must be safe to repeat. That includes a conflict whose next read is the
//     same value: another writer can change the key and restore it. The loop
//     runs until ctx ends, so a deadline bounds contention. fn does not run
//     under the store's write lock.
//   - List filters run on the client, after the whole kind has been fetched.
//     List, Keys, Values, and GetAll are each one call, so a bulk write stays
//     atomic to that read. ListPage, KeysPage, and GetAllPage read one page,
//     and a bulk write can land between pages.
//   - The store's own errors survive the network, so errors.Is works for
//     ErrClosed, ErrKeyNotFound and the others. Any other error arrives as
//     its message only, unless the same error is given to WithErrors on both
//     the handler and the client.
//   - Watch applies BufferSize on the client, to live events only, as a local
//     store does. A watch whose connection breaks ends with a terminal event
//     carrying ErrDisconnected.
//   - Event.Object is decoded from the network, never the writer's value.
//   - Close closes the client and its watches. The server's store stays open.
package remote
