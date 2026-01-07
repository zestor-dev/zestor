package codec

import (
	"go.yaml.in/yaml/v2"
)

type YAML struct {
}

func (y *YAML) Marshal(v any) ([]byte, error) {
	return yaml.Marshal(v)
}

func (y *YAML) Unmarshal(data []byte, v any) error {
	return yaml.Unmarshal(data, v)
}
