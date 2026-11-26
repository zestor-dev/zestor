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
	if err := unmarshalAs(c, b, &out); err != nil {
		return zero, err
	}
	return out, nil
}

// unmarshalAs decodes data into dest. Callers pass &v where v has the value
// type. When that type is already a pointer, &v is a pointer to a nil
// pointer. The codec version these modules require does not allocate that
// pointer for protobuf, so it is allocated here and the value itself is
// passed to Unmarshal.
func unmarshalAs[T any](c codec.Codec, data []byte, dest *T) error {
	rv := reflect.ValueOf(dest).Elem()
	if rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			rv.Set(reflect.New(rv.Type().Elem()))
		}
		return c.Unmarshal(data, rv.Interface())
	}
	return c.Unmarshal(data, dest)
}

func sameValue[T any](a, b T) bool {
	if am, ok := any(a).(proto.Message); ok {
		bm, ok := any(b).(proto.Message)
		return ok && proto.Equal(am, bm)
	}
	return reflect.DeepEqual(a, b)
}
