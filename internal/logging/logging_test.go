package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestRedaction(t *testing.T) {
	t.Parallel()
	const key = "sk_live_THIS_MUST_NEVER_PRINT"
	cases := []struct {
		name string
		attr slog.Attr
	}{
		{"secret type", slog.Any("provider_key", Secret(key))},
		{"well-known key", slog.String("Authorization", "Bearer "+key)},
		{"api_key", slog.String("api_key", key)},
		{"body", slog.String("body", key)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			l := New(&buf, Options{Level: slog.LevelDebug, JSON: true})
			l.Info("x", tc.attr)
			if strings.Contains(buf.String(), key) {
				t.Fatalf("secret leaked into log: %s", buf.String())
			}
			if !strings.Contains(buf.String(), "***") {
				t.Fatalf("expected mask in log: %s", buf.String())
			}
		})
	}
}

func TestSecretNeverMarshals(t *testing.T) {
	t.Parallel()
	b, err := json.Marshal(struct{ K Secret }{K: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "abc") {
		t.Fatalf("secret leaked into JSON: %s", b)
	}
}
