package remote

import (
	"reflect"

	"google.golang.org/protobuf/proto"

	"github.com/zestor-dev/zestor/codec"
)

// roundTrip canonicalises v through the codec. A value kept in memory, such
// as a gomap entry, can differ from its decoded form in ways that are not a
// change: a time.Time's monotonic clock, or a protocol buffer's internal
// state. Comparing the round trip with the bytes the client read ignores
// encoding differences that decode to the same value.
func roundTrip[T any](c codec.Codec, v T) (T, error) {
	var zero T
	b, err := c.Marshal(v)
	if err != nil {
		return zero, err
	}
	var out T
	if err := c.Unmarshal(b, &out); err != nil {
		return zero, err
	}
	return out, nil
}

func sameValue[T any](a, b T) bool {
	if am, ok := any(a).(proto.Message); ok {
		bm, ok := any(b).(proto.Message)
		return ok && proto.Equal(am, bm)
	}
	return reflect.DeepEqual(a, b)
}
