# remote

Serve any Zestor store over the network, and use it from Go as a
`store.Store[T]`, or from any language over gRPC or HTTP.

```bash
go get github.com/zestor-dev/zestor/remote
```

```go
// Server
path, handler := remote.NewHandler(s, &codec.JSON{})
mux.Handle(path, handler)

// Client: a store.Store[User]
c := remote.NewClient[User]("http://localhost:8080", &codec.JSON{})
defer c.Close()
```

The API is the Connect service in
[`proto/zestor/store/v1/store.proto`](../proto/zestor/store/v1/store.proto):
one handler speaks gRPC, gRPC-Web, and the Connect protocol, so `curl` works
for single calls.

```bash
go run ./example     # an in-memory store on localhost:8080

curl -H 'Content-Type: application/json' \
  -d '{"kind":"users","key":"alice"}' \
  http://localhost:8080/zestor.store.v1.StoreService/Get
```

The client keeps the store contract, with a few differences — `SetFn` may run
`fn` more than once, `List` filters run on the client, and custom errors need
`WithErrors` on both sides. `ListPage`, `KeysPage`, and `GetAllPage` read one
page of a large kind. See the
[remote store documentation](https://docs.zestor.dev/docs/implementations/remote/)
for those, the options, and watching from the command line.

After changing the proto, run `make proto` from the repository root.
