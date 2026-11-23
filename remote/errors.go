package remote

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	storev1 "github.com/zestor-dev/zestor/remote/gen/zestor/store/v1"
	"github.com/zestor-dev/zestor/store"
)

// ErrDisconnected ends a watch whose connection to the server broke. As with
// any terminal watch error, the consumer's view may be incomplete and must be
// rebuilt by re-listing and re-watching.
var ErrDisconnected = errors.New("remote watch disconnected: re-list required")

// errConflict is the CompareAndSet mismatch, returned by the function the
// server passes to SetFn. It never reaches a caller: the client retries.
var errConflict = errors.New("stored value changed")

// failureFor names err for the wire. known is checked after the store's own
// errors, so a validator cannot shadow them.
func failureFor(err error, known []error) *storev1.Failure {
	f := &storev1.Failure{Message: err.Error()}
	switch {
	case errors.Is(err, store.ErrKindRequired):
		f.Reason = storev1.FailureReason_FAILURE_REASON_KIND_REQUIRED
	case errors.Is(err, store.ErrKeyNotFound):
		f.Reason = storev1.FailureReason_FAILURE_REASON_KEY_NOT_FOUND
	case errors.Is(err, store.ErrClosed):
		f.Reason = storev1.FailureReason_FAILURE_REASON_CLOSED
	case errors.Is(err, store.ErrWatchOverflow):
		f.Reason = storev1.FailureReason_FAILURE_REASON_WATCH_OVERFLOW
	case errors.Is(err, store.ErrDecode):
		f.Reason = storev1.FailureReason_FAILURE_REASON_DECODE
	case errors.Is(err, errConflict):
		f.Reason = storev1.FailureReason_FAILURE_REASON_CONFLICT
	default:
		for _, k := range known {
			if errors.Is(err, k) {
				f.Reason = storev1.FailureReason_FAILURE_REASON_APPLICATION
				f.Code = k.Error()
				break
			}
		}
	}
	return f
}

func codeFor(r storev1.FailureReason) connect.Code {
	switch r {
	case storev1.FailureReason_FAILURE_REASON_KIND_REQUIRED:
		return connect.CodeInvalidArgument
	case storev1.FailureReason_FAILURE_REASON_KEY_NOT_FOUND:
		return connect.CodeNotFound
	case storev1.FailureReason_FAILURE_REASON_CLOSED:
		return connect.CodeUnavailable
	case storev1.FailureReason_FAILURE_REASON_WATCH_OVERFLOW:
		return connect.CodeResourceExhausted
	case storev1.FailureReason_FAILURE_REASON_DECODE:
		return connect.CodeDataLoss
	case storev1.FailureReason_FAILURE_REASON_CONFLICT:
		return connect.CodeAborted
	case storev1.FailureReason_FAILURE_REASON_APPLICATION:
		return connect.CodeFailedPrecondition
	}
	return connect.CodeUnknown
}

// toConnectError turns a store error into a Connect error that carries a
// Failure detail.
func toConnectError(err error, known []error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, context.Canceled):
		return connect.NewError(connect.CodeCanceled, err)
	case errors.Is(err, context.DeadlineExceeded):
		return connect.NewError(connect.CodeDeadlineExceeded, err)
	}
	f := failureFor(err, known)
	cerr := connect.NewError(codeFor(f.Reason), errors.New(f.Message))
	if d, derr := connect.NewErrorDetail(f); derr == nil {
		cerr.AddDetail(d)
	}
	return cerr
}

// remoteError keeps the server's message while letting errors.Is find the
// error it was mapped back to.
type remoteError struct {
	msg string
	err error
}

func (e *remoteError) Error() string { return e.msg }
func (e *remoteError) Unwrap() error { return e.err }

func fromFailure(f *storev1.Failure, known map[string]error) error {
	var target error
	switch f.GetReason() {
	case storev1.FailureReason_FAILURE_REASON_KIND_REQUIRED:
		target = store.ErrKindRequired
	case storev1.FailureReason_FAILURE_REASON_KEY_NOT_FOUND:
		target = store.ErrKeyNotFound
	case storev1.FailureReason_FAILURE_REASON_CLOSED:
		target = store.ErrClosed
	case storev1.FailureReason_FAILURE_REASON_WATCH_OVERFLOW:
		target = store.ErrWatchOverflow
	case storev1.FailureReason_FAILURE_REASON_DECODE:
		target = store.ErrDecode
	case storev1.FailureReason_FAILURE_REASON_CONFLICT:
		target = errConflict
	case storev1.FailureReason_FAILURE_REASON_APPLICATION:
		target = known[f.GetCode()]
	}
	return &remoteError{msg: f.GetMessage(), err: target}
}

// fromError maps an RPC error back to the store error it carries. An error
// without a Failure detail (a transport failure, say) is returned as is.
func fromError(err error, known map[string]error) error {
	var cerr *connect.Error
	if !errors.As(err, &cerr) {
		return err
	}
	for _, d := range cerr.Details() {
		v, derr := d.Value()
		if derr != nil {
			continue
		}
		if f, ok := v.(*storev1.Failure); ok {
			return fromFailure(f, known)
		}
	}
	return err
}
