package remote

import (
	"net/http"

	"connectrpc.com/connect"
)

// Option configures NewHandler or NewClient. Options that apply to only one
// of them are ignored by the other.
type Option func(*options)

type options struct {
	known       []error
	httpClient  connect.HTTPClient
	clientOpts  []connect.ClientOption
	handlerOpts []connect.HandlerOption
}

func buildOptions(opts []Option) options {
	o := options{httpClient: http.DefaultClient}
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	return o
}

// WithErrors names errors, such as the ones a validator returns, that should
// survive the network. Give the same errors to NewHandler and NewClient. The
// handler sends the text of the error a failure matches, and the client
// returns an error that errors.Is matches against it.
func WithErrors(errs ...error) Option {
	return func(o *options) {
		for _, err := range errs {
			if err != nil {
				o.known = append(o.known, err)
			}
		}
	}
}

// WithHTTPClient sets the HTTP client a Client uses. The default is
// http.DefaultClient, which speaks HTTP/1.1 to plain http URLs; gRPC needs a
// client that speaks HTTP/2.
func WithHTTPClient(c connect.HTTPClient) Option {
	return func(o *options) {
		if c != nil {
			o.httpClient = c
		}
	}
}

// WithClientOptions passes options to the underlying Connect client, for
// example connect.WithGRPC or an interceptor that adds credentials.
func WithClientOptions(opts ...connect.ClientOption) Option {
	return func(o *options) { o.clientOpts = append(o.clientOpts, opts...) }
}

// WithHandlerOptions passes options to the underlying Connect handler, for
// example an interceptor that checks credentials.
func WithHandlerOptions(opts ...connect.HandlerOption) Option {
	return func(o *options) { o.handlerOpts = append(o.handlerOpts, opts...) }
}
