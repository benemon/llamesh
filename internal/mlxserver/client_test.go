package mlxserver

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestClientUsesBearerTokenOnEveryEndpoint(t *testing.T) {
	f, err := os.Open("../../testdata/mlx-polls.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var poll struct {
		Health  json.RawMessage `json:"health"`
		Metrics json.RawMessage `json:"metrics"`
	}
	sc := bufio.NewScanner(f)
	if !sc.Scan() || json.Unmarshal(sc.Bytes(), &poll) != nil {
		t.Fatal("read recorded poll")
	}
	seen := map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("%s authorization %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		seen[r.URL.Path] = true
		switch r.URL.Path {
		case "/health":
			_, _ = w.Write(poll.Health)
		case "/metrics":
			_, _ = w.Write(poll.Metrics)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := New(srv.URL, "secret")
	h, herr := c.Health()
	m, merr := c.Metrics()
	if herr != nil || merr != nil {
		t.Fatalf("health %v metrics %v", herr, merr)
	}
	if h.LoadedModel != "mlx-community/Qwen3.8-27B-8bit" || h.EffectiveContextLimit != 262144 || m.Summary.InFlight != 0 || m.Server.RequestQueueDepth != 0 {
		t.Fatalf("health %+v metrics %+v", h, m)
	}
	for _, path := range []string{"/health", "/metrics"} {
		if !seen[path] {
			t.Fatalf("%s was not called", path)
		}
	}
}

func TestRecordedPollPhases(t *testing.T) {
	f, err := os.Open("../../testdata/mlx-polls.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	want := []struct {
		phase    string
		inFlight int
	}{{"idle", 0}, {"prefill", 2}, {"decode", 2}, {"complete", 0}}
	sc := bufio.NewScanner(f)
	for i, w := range want {
		if !sc.Scan() {
			t.Fatalf("missing phase %d", i)
		}
		var got struct {
			Phase   string  `json:"phase"`
			Metrics Metrics `json:"metrics"`
		}
		if err := json.Unmarshal(sc.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.Phase != w.phase || got.Metrics.Summary.InFlight != w.inFlight {
			t.Fatalf("phase %+v", got)
		}
	}
}

func TestClientWithoutKeySendsNoAuthorization(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := r.Header["Authorization"]; ok {
			t.Errorf("authorization %q sent without a key", r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	if _, err := New(srv.URL, "").Health(); err != nil {
		t.Fatal(err)
	}
}
