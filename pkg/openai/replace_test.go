package openai

import (
	"reflect"
	"testing"
)

func TestTopLevelSpans(t *testing.T) {
	tests := []struct {
		name string
		body string
		key  string
		want []Span
	}{
		{"one member", `{"model":"m","n":1}`, "model", []Span{{9, 12}}},
		{"spaces around", `{ "a":1 , "model" : "m" }`, "model", []Span{{20, 23}}},
		{"duplicate keeps both", `{"model":"a","model":"b"}`, "model", []Span{{9, 12}, {21, 24}}},
		{"escaped key", "{\"mod\\u0065l\":\"m\"}", "model", []Span{{14, 17}}},
		{"nested only", `{"x":{"model":"m"}}`, "model", nil},
		{"absent", `{"n":1}`, "model", nil},
		{"array", `[{"model":"m"}]`, "model", nil},
		{"invalid", `{"model":`, "model", nil},
		{"trailing data", `{"model":"m"}{}`, "model", nil},
		{"case differs", `{"Model":"m"}`, "model", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := TopLevelSpans([]byte(tt.body), tt.key); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestReplaceModel(t *testing.T) {
	const agent = "agent/m"
	tests := []struct{ name, body, want string }{
		{"top-level only", `{ "id" : "x", "model" : "prov/m" , "choices":[{"model":"keep"}] }`,
			`{ "id" : "x", "model" : "agent/m" , "choices":[{"model":"keep"}] }`},
		{"non-string value", `{"model":null}`, `{"model":"agent/m"}`},
		{"duplicate", `{"model":"a","x":1,"model":"b"}`, `{"model":"agent/m","x":1,"model":"agent/m"}`},
		{"absent is not added", `{"id":"x","nested":{"model":"keep"}}`, `{"id":"x","nested":{"model":"keep"}}`},
		{"not an object", `[{"model":"x"}]`, `[{"model":"x"}]`},
		{"invalid json", `{"model":`, `{"model":`},
		{"done sentinel", `[DONE]`, `[DONE]`},
		{"empty", ``, ``},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := string(ReplaceModel([]byte(tt.body), agent)); got != tt.want {
				t.Errorf("got %s, want %s", got, tt.want)
			}
		})
	}
}
