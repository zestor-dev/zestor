package codec

// Codec encodes values for storage and the remote API.
//
// Unmarshal receives a pointer. Stores decode with Unmarshal(data, &v).
// When the store's type is itself a pointer, that argument is a pointer to
// a nil pointer, and Unmarshal allocates the value, matching encoding/json.
//
// Marshal may encode one value differently on each call.
// Unmarshal(Marshal(v)) must still produce a value equal to v. Remote
// compare-and-set compares those values, not the bytes.
type Codec interface {
	Marshal(v any) ([]byte, error)
	Unmarshal(data []byte, v any) error
}
