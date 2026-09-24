package llamaserver

import (
	"os"
	"strings"
	"testing"
)

func fixture(t *testing.T, name string) []byte {
	b, err := os.ReadFile("../../testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseProps(t *testing.T) {
	p, err := ParseProps(fixture(t, "props.json"))
	if err != nil {
		t.Fatal(err)
	}
	if p.Build != "b10566-bb4caa754" || !strings.HasSuffix(p.ModelPath, "gpt-oss-20b-F16.gguf") || p.NCtx != 131072 {
		t.Fatalf("got %+v", p)
	}
}

func TestParseMetrics(t *testing.T) {
	m := ParseMetrics(string(fixture(t, "metrics.txt")))
	for _, name := range []string{"llamacpp:tokens_predicted_total", "llamacpp:prompt_tokens_total", "llamacpp:requests_processing"} {
		if _, ok := m[name]; !ok {
			t.Fatalf("missing %s", name)
		}
	}
	if m["llamacpp:requests_processing"] != 0 {
		t.Fatalf("idle server should have 0 requests processing, got %v", m["llamacpp:requests_processing"])
	}
	labelled := ParseMetrics("llamacpp:foo{bar=\"x\"} 1\n# HELP x\nllamacpp:baz 2.5\n")
	if labelled["llamacpp:foo"] != 1 || labelled["llamacpp:baz"] != 2.5 {
		t.Fatalf("labelled line not parsed: %v", labelled)
	}
}

func TestParseSlots(t *testing.T) {
	s, err := ParseSlots(fixture(t, "slots.json"))
	if err != nil {
		t.Fatal(err)
	}
	if s.NCtx != 131072 || s.Processing {
		t.Fatalf("got %+v", s)
	}
}
