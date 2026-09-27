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
	if s.NPrompt != 297 || s.Processing {
		t.Fatalf("got %+v", s)
	}
}

func TestParseSlotsDecoded(t *testing.T) {
	s, err := ParseSlots([]byte(`[{"id":0,"is_processing":true,"n_ctx":131072,"n_prompt_tokens":697,"n_prompt_tokens_cache":0,"n_prompt_tokens_processed":697,"next_token":[{"has_next_token":true,"n_remain":851,"n_decoded":49}]}]`))
	if err != nil || s.NDecoded != 49 || !s.Processing {
		t.Fatalf("got %+v, %v", s, err)
	}
}

// A request placed in the fourth of four slots is the one read.
func TestParseSlotsPicksTheBusySlot(t *testing.T) {
	b := []byte(`[{"id":0,"is_processing":false,"n_prompt_tokens":0},{"id":1,"is_processing":false},
		{"id":2,"is_processing":false},{"id":3,"is_processing":true,"n_prompt_tokens":9,"next_token":[{"n_decoded":40}]}]`)
	s, err := ParseSlots(b)
	if err != nil || !s.Processing || s.NPrompt != 9 || s.NDecoded != 40 {
		t.Fatalf("got %+v %v", s, err)
	}
}
