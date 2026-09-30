package snapshot

import (
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/benemon/llamesh/internal/mlxserver"
)

func TestMLXSnapshot(t *testing.T) {
	p1, p2, g1, g2 := 62.7, 62.8, 6.2, 6.6
	c := &MLXCollector{
		Model:    mlxserver.Model{Path: "/cache/snapshot", Bytes: 29952489264, NLayer: 64},
		Build:    "mlx-vlm 0.7.4",
		Local:    "mini",
		MemTotal: 59392 * 1048576,
		Log:      &mlxserver.Follower{},
		health:   mlxserver.Health{LoadedModel: "mlx-community/Qwen3.8-27B-8bit", EffectiveContextLimit: 262144},
	}
	metrics := mlxserver.Metrics{}
	metrics.Summary.InFlight = 3
	metrics.Summary.GeneratedTokensTotal = 2858
	metrics.Server.RequestQueueDepth = 1
	requests := []mlxserver.Request{
		{ID: "a", NPrompt: 4178, NProcessed: 4177, PromptTokensPerS: &p1},
		{ID: "b", NPrompt: 4177, NProcessed: 4176, PromptTokensPerS: &p2},
	}
	s := c.build(requests, metrics, true, false)
	n := s.Nodes[0]
	if s.Model.Engine != "mlx-vlm" || s.Model.Build != "mlx-vlm 0.7.4" || n.Device != "MTL0" || n.Layers.First != 0 || n.Layers.Last != 63 {
		t.Fatalf("model %+v node %+v", s.Model, n)
	}
	if n.GetMemTotal() != 59392*1048576 || n.GetMemModel() != 29952489264 || n.MemContext != nil || n.MemCompute != nil || s.Totals.MemHeld != nil {
		t.Fatalf("memory node %+v totals %+v", n, s.Totals)
	}
	if n.PromptTokensPerS == nil || math.Abs(*n.PromptTokensPerS-(p1+p2)) > 0.001 || *n.RequestsProcessing != 3 || *n.RequestsQueued != 1 || n.Slot.NPrompt != 4178 {
		t.Fatalf("live node %+v", n)
	}
	if n.TokensPerS == nil || *n.TokensPerS != 0 {
		t.Fatalf("generation rate while both prefill %v", n.TokensPerS)
	}
	requests[0], requests[1] = mlxserver.Request{ID: "a", Decoding: true, NGenerated: 150, TokensPerS: &g1}, mlxserver.Request{ID: "b", Decoding: true, NGenerated: 150, TokensPerS: &g2}
	s = c.build(requests, metrics, true, false)
	if s.Nodes[0].TokensPerS == nil || math.Abs(*s.Nodes[0].TokensPerS-(g1+g2)) > 0.001 || s.Nodes[0].PromptTokensPerS == nil || *s.Nodes[0].PromptTokensPerS != 0 {
		t.Fatalf("decode node %+v", s.Nodes[0])
	}
	// Prefill has completed but no two decode lines have come yet: the generation rate is not known.
	requests[1] = mlxserver.Request{ID: "b", Decoding: true}
	if s = c.build(requests, metrics, true, false); s.Nodes[0].TokensPerS != nil {
		t.Fatalf("generation rate before it can be stated %v", *s.Nodes[0].TokensPerS)
	}
	metrics.Summary.InFlight = 0
	s = c.build(nil, metrics, true, false)
	if n := s.Nodes[0]; n.TokensPerS == nil || *n.TokensPerS != 0 || n.PromptTokensPerS == nil || *n.PromptTokensPerS != 0 || n.Slot != nil {
		t.Fatalf("idle node %+v", n)
	}
}

// Without the log there is nothing to state the rates from, idle or not.
func TestMLXWithoutLogRatesAreUnknown(t *testing.T) {
	c := &MLXCollector{}
	if n := c.build(nil, mlxserver.Metrics{}, true, false).Nodes[0]; n.TokensPerS != nil || n.PromptTokensPerS != nil {
		t.Fatalf("rates without a log %v %v", n.TokensPerS, n.PromptTokensPerS)
	}
}

func TestMLXUnknownWiredLimit(t *testing.T) {
	c := &MLXCollector{Model: mlxserver.Model{Bytes: 1}, Build: "mlx-vlm", health: mlxserver.Health{LoadedModel: "model"}}
	s := c.build(nil, mlxserver.Metrics{}, false, true)
	if s.Nodes[0].MemTotal != nil || s.Nodes[0].RequestsProcessing != nil || s.Nodes[0].RequestsQueued != nil || !s.Nodes[0].Stale {
		t.Fatalf("node %+v", s.Nodes[0])
	}
}

// A request that ends without a completion line stays open in the log; the server's in-flight count of 0,
// read after the log, closes it.
func TestMLXRequestsCloseWhenNoneInFlight(t *testing.T) {
	b, err := os.ReadFile("../../testdata/stderr-switch.log")
	if err != nil {
		t.Fatal(err)
	}
	path := t.TempDir() + "/server.err"
	if err := os.WriteFile(path, b[:strings.Index(string(b), "2026-09-30 09:34:33,797")], 0o644); err != nil {
		t.Fatal(err)
	}
	log, err := mlxserver.OpenLog(path, 85726)
	if err != nil {
		t.Fatal(err)
	}
	inFlight := 1
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, `{"summary":{"in_flight":%d},"server":{},"loaded_model":"m"}`, inFlight)
	}))
	defer srv.Close()
	c := &MLXCollector{Client: mlxserver.New(srv.URL, ""), Log: log}
	if n := c.Poll().Nodes[0]; n.Slot == nil || n.Slot.NPrompt != 84 {
		t.Fatalf("open request not shown: %+v", n)
	}
	inFlight = 0
	c.Poll()
	inFlight = 1
	if n := c.Poll().Nodes[0]; n.Slot != nil {
		t.Fatalf("ended request still open: %+v", n.Slot)
	}
}
