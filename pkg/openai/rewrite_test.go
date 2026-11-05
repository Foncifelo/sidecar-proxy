package openai

import (
	"bytes"
	"errors"
	"testing"
)

func TestSetModel(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		model   string
		want    string
		wantErr error
	}{
		{
			name:  "replace keeps every other byte",
			body:  `{ "messages" : [ {"role":"user","content":"x"} ] , "model" : "m1", "stream" : true }`,
			model: "provider/m2",
			want:  `{ "messages" : [ {"role":"user","content":"x"} ] , "model" : "provider/m2", "stream" : true }`,
		},
		{
			name:  "escaped model value",
			body:  `{"model":"a\"b","n":1}`,
			model: `q"z`,
			want:  `{"model":"q\"z","n":1}`,
		},
		{
			name:  "absent model is appended",
			body:  `{"n":1}`,
			model: "m",
			want:  `{"n":1,"model":"m"}`,
		},
		{
			name:  "empty object",
			body:  " {  }\n",
			model: "m",
			want:  " {  \"model\":\"m\"}\n",
		},
		{
			name:  "duplicate keys all replaced",
			body:  `{"model":"a","model":"b"}`,
			model: "c",
			want:  `{"model":"c","model":"c"}`,
		},
		{
			name:  "nested model untouched",
			body:  `{"metadata":{"model":"x"},"model":"a"}`,
			model: "b",
			want:  `{"metadata":{"model":"x"},"model":"b"}`,
		},
		{name: "array body", body: `[]`, model: "m", wantErr: ErrNotObject},
		{name: "invalid json", body: `{"model":}`, model: "m", wantErr: ErrNotObject},
		{name: "trailing data", body: `{"model":"a"} {}`, model: "m", wantErr: ErrNotObject},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := []byte(tt.body)
			orig := bytes.Clone(in)
			got, err := SetModel(in, tt.model)
			if !bytes.Equal(in, orig) {
				t.Fatal("input body was mutated")
			}
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if string(got) != tt.want {
				t.Fatalf("got  %s\nwant %s", got, tt.want)
			}
			info, err := ParseRequest(got)
			if err != nil || info.Model != tt.model {
				t.Fatalf("reparse model = %q, err %v", info.Model, err)
			}
		})
	}
}

func TestSetModelFixture(t *testing.T) {
	body := readFixture(t, "chat_request_full.json")
	got, err := SetModel(body, "other/model")
	if err != nil {
		t.Fatal(err)
	}
	want := bytes.Replace(body, []byte(`"example/fictional-model-70b"`), []byte(`"other/model"`), 1)
	if !bytes.Equal(got, want) {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestSetIncludeUsage(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		include bool
		want    string
		wantErr error
	}{
		{
			name:    "absent stream_options",
			body:    `{"model":"m", "stream":true}`,
			include: true,
			want:    `{"model":"m", "stream":true,"stream_options":{"include_usage":true}}`,
		},
		{
			name:    "null stream_options",
			body:    `{"stream_options": null}`,
			include: true,
			want:    `{"stream_options": {"include_usage":true}}`,
		},
		{
			name:    "empty stream_options",
			body:    `{"stream_options":{ }}`,
			include: true,
			want:    `{"stream_options":{ "include_usage":true}}`,
		},
		{
			name:    "keeps other options",
			body:    `{"stream_options":{"x":1}, "model":"m"}`,
			include: true,
			want:    `{"stream_options":{"x":1,"include_usage":true}, "model":"m"}`,
		},
		{
			name:    "flips existing value",
			body:    `{"stream_options":{"include_usage": true ,"x":1}}`,
			include: false,
			want:    `{"stream_options":{"include_usage": false ,"x":1}}`,
		},
		{
			name:    "empty top-level object",
			body:    `{}`,
			include: true,
			want:    `{"stream_options":{"include_usage":true}}`,
		},
		{name: "stream_options wrong type", body: `{"stream_options":true}`, include: true, wantErr: ErrInvalidField},
		{name: "not an object", body: `"x"`, include: true, wantErr: ErrNotObject},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := []byte(tt.body)
			orig := bytes.Clone(in)
			got, err := SetIncludeUsage(in, tt.include)
			if !bytes.Equal(in, orig) {
				t.Fatal("input body was mutated")
			}
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if string(got) != tt.want {
				t.Fatalf("got  %s\nwant %s", got, tt.want)
			}
			info, err := ParseRequest(got)
			if err != nil || info.IncludeUsage != tt.include {
				t.Fatalf("reparse include_usage = %v, err %v", info.IncludeUsage, err)
			}
		})
	}
}

func TestSetIncludeUsageFixture(t *testing.T) {
	body := readFixture(t, "chat_request_stream_no_options.json")
	got, err := SetIncludeUsage(body, true)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(got, body[:bytes.LastIndexByte(body, '}')]) {
		t.Fatalf("prefix changed: %s", got)
	}
	info, err := ParseRequest(got)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IncludeUsage || !info.Stream || info.Model != "m1" {
		t.Fatalf("unexpected info %+v", info)
	}
}

func TestSetMember(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		key     string
		val     string
		want    string
		wantErr error
	}{
		{
			name: "replace keeps every other byte",
			body: `{ "model" : "m", "provider" : {"order":["a"]} , "messages":[{"content":"<&> x"}] }`,
			key:  "provider",
			val:  `{"order":["b"],"allow_fallbacks":false}`,
			want: `{ "model" : "m", "provider" : {"order":["b"],"allow_fallbacks":false} , "messages":[{"content":"<&> x"}] }`,
		},
		{
			name: "absent key is appended",
			body: `{"z":1,"a":2}`,
			key:  "provider",
			val:  `{"order":["b"]}`,
			want: `{"z":1,"a":2,"provider":{"order":["b"]}}`,
		},
		{
			name: "empty object",
			body: "{}",
			key:  "k",
			val:  `null`,
			want: `{"k":null}`,
		},
		{
			name: "duplicate keys all replaced",
			body: `{"provider":1,"x":0,"provider":2}`,
			key:  "provider",
			val:  `3`,
			want: `{"provider":3,"x":0,"provider":3}`,
		},
		{
			name: "nested key untouched",
			body: `{"metadata":{"provider":"x"}}`,
			key:  "provider",
			val:  `"y"`,
			want: `{"metadata":{"provider":"x"},"provider":"y"}`,
		},
		{
			name: "key needing escape",
			body: `{"a":1}`,
			key:  `q"`,
			val:  `2`,
			want: `{"a":1,"q\"":2}`,
		},
		{name: "invalid value", body: `{}`, key: "k", val: `{`, wantErr: ErrInvalidField},
		{name: "empty value", body: `{}`, key: "k", val: ``, wantErr: ErrInvalidField},
		{name: "array body", body: `[]`, key: "k", val: `1`, wantErr: ErrNotObject},
		{name: "invalid json", body: `{"k":}`, key: "k", val: `1`, wantErr: ErrNotObject},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := []byte(tt.body)
			orig := bytes.Clone(in)
			got, err := SetMember(in, tt.key, []byte(tt.val))
			if !bytes.Equal(in, orig) {
				t.Fatal("input body was mutated")
			}
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("error = %v, want %v", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if string(got) != tt.want {
				t.Fatalf("got  %s\nwant %s", got, tt.want)
			}
		})
	}
}
