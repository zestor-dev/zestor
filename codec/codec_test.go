package codec

import (
	"testing"

	"google.golang.org/protobuf/types/known/wrapperspb"
)

type sampleData struct {
	Name  string `json:"name" yaml:"name"`
	Value int    `json:"value" yaml:"value"`
}

func TestJSONRoundTrip(t *testing.T) {
	c := &JSON{}
	want := sampleData{Name: "json", Value: 42}

	data, err := c.Marshal(want)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var got sampleData
	if err := c.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if got != want {
		t.Fatalf("Unmarshal() = %#v, want %#v", got, want)
	}
}

func TestJSONUnmarshalInvalidInput(t *testing.T) {
	if err := (&JSON{}).Unmarshal([]byte(`{"name":`), &sampleData{}); err == nil {
		t.Fatal("Unmarshal() error = nil, want error")
	}
}

func TestYAMLRoundTrip(t *testing.T) {
	c := &YAML{}
	want := sampleData{Name: "yaml", Value: 84}

	data, err := c.Marshal(want)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	var got sampleData
	if err := c.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if got != want {
		t.Fatalf("Unmarshal() = %#v, want %#v", got, want)
	}
}

func TestYAMLUnmarshalInvalidInput(t *testing.T) {
	if err := (&YAML{}).Unmarshal([]byte("name: ["), &sampleData{}); err == nil {
		t.Fatal("Unmarshal() error = nil, want error")
	}
}

func TestProtobufRoundTrip(t *testing.T) {
	c := &Protobuf{}
	want := wrapperspb.String("protobuf")

	data, err := c.Marshal(want)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}

	got := &wrapperspb.StringValue{}
	if err := c.Unmarshal(data, got); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if got.Value != want.Value {
		t.Fatalf("Unmarshal() = %q, want %q", got.Value, want.Value)
	}
}

func TestProtobufRejectsNonMessages(t *testing.T) {
	c := &Protobuf{}
	if _, err := c.Marshal(sampleData{}); err == nil {
		t.Fatal("Marshal() error = nil, want error")
	}
	if err := c.Unmarshal(nil, &sampleData{}); err == nil {
		t.Fatal("Unmarshal() error = nil, want error")
	}
}
