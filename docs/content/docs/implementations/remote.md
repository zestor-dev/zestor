---
title: "Remote Store"
linkTitle: "remote"
weight: 4
description: >
  Serve any store over the network, and use it from Go as a store.Store, or from any language with gRPC or HTTP.
---

```go
import "github.com/zestor-dev/zestor/remote"
```

`remote` is not a storage backend. It puts a network API in front of any
`store.Store` — in-memory, SQLite, or Postgres — and gives Go programs a
client that is itself a `store.Store[T]`. Code written against the interface
works unchanged when the store moves into another process.

The API is a [Connect](https://connectrpc.com) service, defined in
`proto/zestor/store/v1/store.proto`. One handler speaks three protocols:

| Protocol | Transport | Typical caller |
|---|---|---|
| Connect | HTTP/1.1 or HTTP/2, JSON or binary bodies | `curl`, browsers, Connect clients |
| gRPC | HTTP/2 | gRPC clients in any language |
| gRPC-Web | HTTP/1.1 or HTTP/2 | browsers through gRPC-Web libraries |

## Server

```go
s := gomap.NewMemStore[User](store.StoreOptions[User]{})

path, handler := remote.NewHandler(s, &codec.JSON{})
mux := http.NewServeMux()
mux.Handle(path, handler)
http.ListenAndServe(":8080", mux)
```

Values travel as bytes encoded with the codec. The server decodes them into
`T` before calling the store, so the store's `ValidateFns` and `CompareFn`
apply to remote writes exactly as to local ones. The handler never closes the
store.

gRPC needs HTTP/2. Without TLS, enable unencrypted HTTP/2 on the server:

```go
protocols := new(http.Protocols)
protocols.SetHTTP1(true)
protocols.SetUnencryptedHTTP2(true)
srv := &http.Server{Addr: ":8080", Handler: mux, Protocols: protocols}
```

To shut down, close the store first, then call `srv.Shutdown`. An open watch
only ends when its store closes or its client goes away, so `Shutdown` would
otherwise wait for it indefinitely. Closing the store sends each watcher a
final `ErrClosed`.

## Go client

```go
c := remote.NewClient[User]("http://localhost:8080", &codec.JSON{})
defer c.Close()

res, err := c.Set(ctx, "users", "alice", User{Name: "Alice", Email: "alice@example.com"})
```

`*remote.Client[T]` implements `store.Store[T]`, including `Watch`. The shared
conformance suite runs through it against all three backends, over the Connect
protocol (binary and JSON) and over gRPC.

### Where it differs from a local store

- **`SetFn`** reads the value, runs `fn` on the client, and writes the result
  only if the stored value has not changed in the meantime. If it has, `fn`
  runs again on the newer value, so it may run more than once and must be safe
  to repeat. It does not hold the store's write lock, and may call the store.
- **`List` filters** run on the client, after the whole kind is fetched.
- **Errors.** The store's own errors keep their identity: `errors.Is(err,
  store.ErrKeyNotFound)` and the others work. Any other error, such as one a
  validator returns, arrives as its message only — unless you give the same
  error to `remote.WithErrors` on both sides:

  ```go
  var ErrEmailRequired = errors.New("email required")

  remote.NewHandler(s, &codec.JSON{}, remote.WithErrors(ErrEmailRequired))
  remote.NewClient[User](url, &codec.JSON{}, remote.WithErrors(ErrEmailRequired))
  ```

- **Watch.** The client reads the stream as it arrives and applies
  `BufferSize` itself, to live events only, as a local store does. The server
  and the connection would otherwise hide how far behind the consumer is. A
  watch whose connection breaks ends with a final event carrying
  `remote.ErrDisconnected`; as with any terminal error, re-list and re-watch.
- **`Event.Object`** is decoded from the network, never the writer's value.
- **`Close`** ends the client and its watches. The server's store stays open.

### Options

| Option | Applies to | Purpose |
|---|---|---|
| `WithErrors(errs...)` | both | Errors that keep their identity across the network |
| `WithHTTPClient(c)` | client | HTTP client; gRPC needs one that speaks HTTP/2 |
| `WithClientOptions(opts...)` | client | Connect client options, e.g. `connect.WithGRPC()` or an interceptor that adds credentials |
| `WithHandlerOptions(opts...)` | handler | Connect handler options, e.g. an interceptor that checks credentials |

There is no built-in authentication. Use TLS, and an interceptor or HTTP
middleware in front of the handler.

## From the command line

Unary calls are plain HTTP POSTs with JSON bodies. Values are bytes, so JSON
carries them base64-encoded.

```bash
B=http://localhost:8080/zestor.store.v1.StoreService
V=$(printf '{"name":"Alice","email":"alice@example.com"}' | base64)

curl -H 'Content-Type: application/json' \
  -d "{\"kind\":\"users\",\"key\":\"alice\",\"value\":\"$V\"}" $B/Set
# {"result":"SET_RESULT_CREATED"}

curl -H 'Content-Type: application/json' -d '{"kind":"users","key":"alice"}' $B/Get
# {"found":true, "value":"eyJuYW1lIjoiQWxpY2UiLC..."}
```

Read-only calls also accept GET, which proxies and browsers can cache:

```bash
curl --get --data-urlencode 'encoding=json' \
  --data-urlencode 'message={"kind":"users"}' $B/List
```

A failed call returns an error code, the message, and a `Failure` detail that
names the store error:

```json
{"code":"failed_precondition","message":"email required",
 "details":[{"type":"zestor.store.v1.Failure",
   "debug":{"reason":"FAILURE_REASON_APPLICATION","message":"email required","code":"email required"}}]}
```

`Watch` is a stream. Plain `curl` cannot decode its framing; use
[`buf curl`](https://buf.build/docs/reference/cli/buf/curl/) with the schema
from this repo:

```bash
buf curl --schema proto --data '{"kind":"users","initial_replay":true}' $B/Watch
```

The first message is always `subscribed`. Every write after it is delivered,
replayed events carry `"replay": true`, and a watch that ends on a store
error sends a final `failure`. Add `--protocol grpc --http2-prior-knowledge`
to watch over gRPC.

`remote/example` is a runnable server for trying these:

```bash
cd remote && go run ./example
```
