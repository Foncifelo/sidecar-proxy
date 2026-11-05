package sse

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestUsageStripper(t *testing.T) {
	fixture := string(loadFixture(t))
	usageBlock := `data: {"id":"fict-1","object":"chat.completion.chunk","model":"fictional-model","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":2,"total_tokens":13}}` + "\n\n"
	tests := []struct {
		name         string
		in           string
		want         string
		wantStripped bool
	}{
		{"fixture", fixture, strings.Replace(fixture, usageBlock, "", 1), true},
		{"no usage chunk", "data: {\"choices\":[{}]}\n\ndata: [DONE]\n\n", "data: {\"choices\":[{}]}\n\ndata: [DONE]\n\n", false},
		{"usage with non-empty choices kept", "data: {\"choices\":[{}],\"usage\":{\"total_tokens\":1}}\n\n", "data: {\"choices\":[{}],\"usage\":{\"total_tokens\":1}}\n\n", false},
		{"null usage kept", "data: {\"choices\":[],\"usage\":null}\n\n", "data: {\"choices\":[],\"usage\":null}\n\n", false},
		{"missing choices kept", "data: {\"usage\":{}}\n\n", "data: {\"usage\":{}}\n\n", false},
		{"invalid json kept", "data: {\"usage\":\n\n", "data: {\"usage\":\n\n", false},
		{"other event type kept", "event: stats\ndata: {\"choices\":[],\"usage\":{}}\n\n", "event: stats\ndata: {\"choices\":[],\"usage\":{}}\n\n", false},
		{"crlf usage stripped", ": ka\r\n\r\ndata: {\"choices\":[],\"usage\":{\"total_tokens\":3}}\r\n\r\ndata: [DONE]\r\n\r\n", ": ka\r\n\r\ndata: [DONE]\r\n\r\n", true},
		{"final usage without blank line", "data: a\n\ndata: {\"choices\":[],\"usage\":{}}\n", "data: a\n\n", true},
		{"final partial block kept", "data: a\n\ndata: b", "data: a\n\ndata: b", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, chunk := range []int{len(tt.in) + 1, 1, 7} {
				var dst bytes.Buffer
				s := NewUsageStripper(&dst)
				for i := 0; i < len(tt.in); i += chunk {
					p := []byte(tt.in[i:min(i+chunk, len(tt.in))])
					n, err := s.Write(p)
					if err != nil || n != len(p) {
						t.Fatalf("Write = %d, %v", n, err)
					}
				}
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				if dst.String() != tt.want {
					t.Errorf("chunk %d: got %q, want %q", chunk, dst.String(), tt.want)
				}
				if s.Stripped() != tt.wantStripped {
					t.Errorf("Stripped = %v, want %v", s.Stripped(), tt.wantStripped)
				}
			}
		})
	}
}

func TestUsageStripperWithTee(t *testing.T) {
	fixture := loadFixture(t)
	var dst bytes.Buffer
	s := NewUsageStripper(&dst)
	sawUsage := false
	if _, err := Tee(s, bytes.NewReader(fixture), func(ev Event) error {
		sawUsage = sawUsage || strings.Contains(ev.Data, `"usage"`)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if !sawUsage {
		t.Error("callback did not see the usage chunk")
	}
	if bytes.Contains(dst.Bytes(), []byte(`"usage"`)) {
		t.Error("usage chunk reached the agent")
	}
}

func TestUsageStripperOversizeForwarded(t *testing.T) {
	big := "data: " + strings.Repeat("x", MaxEventSize) + "\n\n"
	var dst bytes.Buffer
	s := NewUsageStripper(&dst)
	if _, err := s.Write([]byte(big)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write([]byte(big[:10])); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write([]byte(strings.Repeat("y", MaxEventSize))); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if dst.Len() != len(big)+10+MaxEventSize {
		t.Errorf("forwarded %d bytes", dst.Len())
	}
}

func TestUsageStripperWriteError(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"complete block", "data: a\n\n"},
		{"oversize", strings.Repeat("x", MaxEventSize+1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := NewUsageStripper(errWriter{n: -1})
			if _, err := s.Write([]byte(tt.in)); err == nil {
				t.Fatal("expected error")
			}
		})
	}
	s := NewUsageStripper(errWriter{n: -1})
	if _, err := s.Write([]byte("data: tail")); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err == nil || errors.Unwrap(err) == nil {
		t.Fatalf("Close err = %v, want wrapped error", err)
	}
}
