package codec

import (
	"fmt"
	"reflect"

	"google.golang.org/protobuf/proto"
)

type Protobuf struct {
}

func (p *Protobuf) Marshal(v any) ([]byte, error) {
	msg, ok := v.(proto.Message)
	if !ok || nilMessage(msg) {
		return nil, fmt.Errorf("protobuf: value must implement proto.Message")
	}
	return proto.Marshal(msg)
}

func (p *Protobuf) Unmarshal(data []byte, v any) error {
	msg, ok := messageAt(v)
	if !ok {
		return fmt.Errorf("protobuf: value must implement proto.Message")
	}
	return proto.Unmarshal(data, msg)
}

// messageAt returns the protocol message v points at. Stores decode with
// Unmarshal(data, &value). When the store's type is already a message
// pointer, v is a pointer to that pointer and does not implement
// proto.Message; a nil pointer is allocated here.
func messageAt(v any) (proto.Message, bool) {
	if msg, ok := v.(proto.Message); ok && !nilMessage(msg) {
		return msg, true
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return nil, false
	}
	elem := rv.Elem()
	if elem.Kind() != reflect.Pointer || !elem.CanSet() {
		return nil, false
	}
	if elem.IsNil() {
		created := reflect.New(elem.Type().Elem())
		if _, ok := created.Interface().(proto.Message); !ok {
			return nil, false
		}
		elem.Set(created)
	}
	msg, ok := elem.Interface().(proto.Message)
	if !ok || nilMessage(msg) {
		return nil, false
	}
	return msg, true
}

func nilMessage(msg proto.Message) bool {
	if msg == nil {
		return true
	}
	rv := reflect.ValueOf(msg)
	return rv.Kind() == reflect.Pointer && rv.IsNil()
}
