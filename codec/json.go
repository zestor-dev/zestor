package codec

import "encoding/json"

type JSON struct {
}

func (j *JSON) Marshal(v any) ([]byte, error) {
	return json.Marshal(v)
}

func (j *JSON) Unmarshal(data []byte, v any) error {
	return json.Unmarshal(data, v)
}
