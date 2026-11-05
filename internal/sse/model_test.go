package sse

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/Foncifelo/sidecar-proxy/pkg/openai"
)

// Model names here are fictitious.
const agentModel = "agent/m<1>"

// agentVal is agentModel as json.Marshal encodes it: < and > escaped.
const agentVal = `"agent/m` + "\\" + `u003c1` + "\\" + `u003e"`

func TestModelRewriter(t *testing.T) {
	big := "data: {\"model\":\"prov\",\"x\":\"" + strings.Repeat("a", MaxEventSize) + "\"}\n\n"
	tests := []struct{ name, in, want string }{
		{"top-level model", "data: {\"id\":\"c1\",\"model\":\"prov/m\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n",
			"data: {\"id\":\"c1\",\"model\":" + agentVal + ",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"},
		{"spaces and order kept", "data:  { \"z\" : 1 , \"model\" :\t\"p\" }\r\n\r\n",
			"data:  { \"z\" : 1 , \"model\" :\t" + agentVal + " }\r\n\r\n"},
		{"no space after colon", "data:{\"model\":\"p\"}\n\n", "data:{\"model\":" + agentVal + "}\n\n"},
		{"duplicate model", "data: {\"model\":\"a\",\"model\":\"b\"}\n\n",
			"data: {\"model\":" + agentVal + ",\"model\":" + agentVal + "}\n\n"},
		{"escaped key", "data: {\"mod\\u0065l\":\"p\"}\n\n", "data: {\"mod\\u0065l\":" + agentVal + "}\n\n"},
		{"event id and comments kept", ": c\nevent: chunk\nid: 7\ndata: {\"model\":\"p\"}\n\n",
			": c\nevent: chunk\nid: 7\ndata: {\"model\":" + agentVal + "}\n\n"},
		{"multi-line data", "data: {\"a\":1,\ndata: \"model\":\"p\"}\n\n", "data: {\"a\":1,\ndata: \"model\":" + agentVal + "}\n\n"},
		{"value across lines is left intact", "data: {\"model\":{\ndata: \"x\":1}}\n\n", "data: {\"model\":{\ndata: \"x\":1}}\n\n"},
		{"usage chunk", "data: {\"model\":\"p\",\"choices\":[],\"usage\":{\"prompt_tokens\":1}}\n\n",
			"data: {\"model\":" + agentVal + ",\"choices\":[],\"usage\":{\"prompt_tokens\":1}}\n\n"},
		{"no model is not added", "data: {\"id\":\"c1\",\"choices\":[]}\n\n", "data: {\"id\":\"c1\",\"choices\":[]}\n\n"},
		{"nested model kept", "data: {\"x\":{\"model\":\"p\"}}\n\n", "data: {\"x\":{\"model\":\"p\"}}\n\n"},
		{"done", "data: [DONE]\n\n", "data: [DONE]\n\n"},
		{"invalid json", "data: {\"model\":\n\n", "data: {\"model\":\n\n"},
		{"array", "data: [{\"model\":\"p\"}]\n\n", "data: [{\"model\":\"p\"}]\n\n"},
		{"model outside data", "event: model\nid: model\n: model\n\n", "event: model\nid: model\n: model\n\n"},
		{"final block without blank line", "data: {\"model\":\"p\"}", "data: {\"model\":" + agentVal + "}"},
		{"several blocks", ": ping\n\ndata: {\"model\":\"p\"}\n\ndata: [DONE]\n\n",
			": ping\n\ndata: {\"model\":" + agentVal + "}\n\ndata: [DONE]\n\n"},
		{"oversized block uninspected", big, big},
	}
	for _, tt := range tests {
		for _, piece := range []int{len(tt.in), 1, 7} {
			t.Run(tt.name, func(t *testing.T) {
				if piece < len(tt.in) && len(tt.in) > 1<<16 {
					t.Skip("byte-by-byte write of an 8 MiB block is covered whole")
				}
				var dst bytes.Buffer
				m := NewModelRewriter(&dst, agentModel)
				for in := []byte(tt.in); len(in) > 0; {
					n := min(piece, len(in))
					if _, err := m.Write(in[:n]); err != nil {
						t.Fatal(err)
					}
					in = in[n:]
				}
				if err := m.Close(); err != nil {
					t.Fatal(err)
				}
				if dst.String() != tt.want {
					t.Errorf("piece %d\n got %q\nwant %q", piece, trim(dst.String()), trim(tt.want))
				}
			})
		}
	}
}

func trim(s string) string {
	if len(s) > 200 {
		return s[:200] + "..."
	}
	return s
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("gone") }

func TestModelRewriterWriteError(t *testing.T) {
	m := NewModelRewriter(failWriter{}, agentModel)
	if _, err := m.Write([]byte("data: {\"model\":\"p\"}\n\n")); err == nil {
		t.Error("write error not reported")
	}
	m = NewModelRewriter(failWriter{}, agentModel)
	if _, err := m.Write([]byte("data: x")); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err == nil {
		t.Error("close error not reported")
	}
}

func FuzzModelRewriter(f *testing.F) {
	seeds := []string{
		"data: {\"model\":\"p\",\"choices\":[]}\n\ndata: [DONE]\n\n",
		"data: {\"a\":1,\ndata: \"model\":\"p\"}\r\n\r\n",
		": ping\n\nevent: x\ndata: {\"x\":{\"model\":1}}\n\n",
		"data:{\"mod\\u0065l\":null}",
		"data: {\"model\":{\ndata: }}\n\n",
		"data: {\"model\":\"a\",\"model\":\"b\"}\n\nid\n",
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, in []byte) {
		var dst bytes.Buffer
		m := NewModelRewriter(&dst, agentModel)
		if _, err := m.Write(in); err != nil {
			t.Fatal(err)
		}
		if err := m.Close(); err != nil {
			t.Fatal(err)
		}
		inBlocks, outBlocks := readBlocks(t, in), readBlocks(t, dst.Bytes())
		if len(inBlocks) != len(outBlocks) {
			t.Fatalf("%d blocks in, %d out", len(inBlocks), len(outBlocks))
		}
		for i := range inBlocks {
			checkBlock(t, inBlocks[i], outBlocks[i])
		}
	})
}

func readBlocks(t *testing.T, b []byte) []Event {
	t.Helper()
	var evs []Event
	rd := NewReader(bytes.NewReader(b))
	for {
		ev, err := rd.Next()
		if errors.Is(err, io.EOF) {
			return evs
		}
		if err != nil {
			t.Fatal(err)
		}
		evs = append(evs, ev)
	}
}

// checkBlock asserts that out differs from in at most in the values of the
// top-level "model" of the data payload, each replaced by the agent's model.
func checkBlock(t *testing.T, in, out Event) {
	t.Helper()
	if in.Type != out.Type || in.ID != out.ID || in.HasData != out.HasData ||
		strings.Join(in.Comments, "\n") != strings.Join(out.Comments, "\n") {
		t.Fatalf("fields changed: %q -> %q", in.Raw, out.Raw)
	}
	inSpans := openai.TopLevelSpans([]byte(in.Data), "model")
	if bytes.Equal(in.Raw, out.Raw) {
		for _, s := range inSpans {
			if in.Data[s.Start:s.End] != agentVal && !strings.Contains(in.Data[s.Start:s.End], "\n") {
				t.Fatalf("model not rewritten in %q", in.Raw)
			}
		}
		return
	}
	outSpans := openai.TopLevelSpans([]byte(out.Data), "model")
	if len(inSpans) == 0 || len(inSpans) != len(outSpans) {
		t.Fatalf("block changed without a top-level model: %q -> %q", in.Raw, out.Raw)
	}
	for _, s := range outSpans {
		if out.Data[s.Start:s.End] != agentVal {
			t.Fatalf("model value %q", out.Data[s.Start:s.End])
		}
	}
	if !bytes.Equal(cut(in.Data, inSpans), cut(out.Data, outSpans)) {
		t.Fatalf("bytes other than model changed: %q -> %q", in.Raw, out.Raw)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out.Data), &top); err != nil || string(top["model"]) != agentVal {
		t.Fatalf("output does not decode to the agent model: %q", out.Data)
	}
}

// cut removes every span from s.
func cut(s string, spans []openai.Span) []byte {
	return openai.ReplaceSpans([]byte(s), spans, nil)
}
