package codec

import (
	"fmt"

	"google.golang.org/protobuf/proto"
)

type Protobuf struct {
}

func (p *Protobuf) Marshal(v any) ([]byte, error) {
	msg, ok := v.(proto.Message)
	if !ok {
		return nil, fmt.Errorf("protobuf: value must implement proto.Message")
	}
	return proto.Marshal(msg)
}

func (p *Protobuf) Unmarshal(data []byte, v any) error {
	msg, ok := v.(proto.Message)
	if !ok {
		return fmt.Errorf("protobuf: value must implement proto.Message")
	}
	return proto.Unmarshal(data, msg)
}
