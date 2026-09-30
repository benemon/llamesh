package snapshot

import (
	"encoding/json"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/benemon/llamesh/internal/discover"
	"github.com/benemon/llamesh/internal/llamaserver"
	"github.com/benemon/llamesh/internal/loadlog"
	pb "github.com/benemon/llamesh/internal/pb/llamesh/v1"
)

type fixed loadlog.Split

func (f fixed) Latest() loadlog.Split               { return loadlog.Split(f) }
func (f fixed) Request(int) (loadlog.Request, bool) { return loadlog.Request{}, false }

func split(t *testing.T, name string) fixed {
	b, err := os.ReadFile("../../testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return fixed(loadlog.Parse(string(b)))
}

type requestLog struct {
	request loadlog.Request
}

type requestLogs map[int]loadlog.Request

func (requestLogs) Latest() loadlog.Split { return loadlog.Split{} }
func (r requestLogs) Request(slot int) (loadlog.Request, bool) {
	v, ok := r[slot]
	return v, ok
}

func (r requestLog) Latest() loadlog.Split { return loadlog.Split{} }
func (r requestLog) Request(slot int) (loadlog.Request, bool) {
	return r.request, slot == 0
}

func requestFollower(t *testing.T, marker string) *loadlog.Follower {
	t.Helper()
	b, err := os.ReadFile("../../testdata/server-request.log")
	if err != nil {
		t.Fatal(err)
	}
	i := strings.Index(string(b), marker)
	if i < 0 {
		t.Fatalf("fixture missing %q", marker)
	}
	end := strings.IndexByte(string(b[i:]), '\n') + i + 1
	path := t.TempDir() + "/server.err"
	if err := os.WriteFile(path, b[:end], 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := loadlog.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func followerThrough(t *testing.T, name, marker string) *loadlog.Follower {
	t.Helper()
	b, err := os.ReadFile("../../testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	i := strings.Index(string(b), marker)
	if i < 0 {
		t.Fatalf("fixture missing %q", marker)
	}
	end := strings.IndexByte(string(b[i:]), '\n') + i + 1
	path := t.TempDir() + "/server.err"
	if err := os.WriteFile(path, b[:end], 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := loadlog.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func qwenSlot(t *testing.T) llamaserver.Slot {
	t.Helper()
	ss, err := llamaserver.ParseSlots([]byte(`[{"id":0,"is_processing":true,"id_task":861,"n_prompt_tokens":10305,"n_prompt_tokens_processed":10305,"n_prompt_tokens_cache":0,"next_token":[{"has_next_token":false,"n_remain":4096,"n_decoded":0}]}]`))
	if err != nil {
		t.Fatal(err)
	}
	return ss[0]
}

func TestSplitModelLayersAreUnknown(t *testing.T) {
	c := &Collector{Log: split(t, "server-lv4.log"), Args: discover.Args{RPC: []string{"10.0.0.2:50052"}}}
	s := c.build(nil, nil, false, nil)
	local, rpc := s.Nodes[0], s.Nodes[1]
	if local.Device != "MTL0" || local.Layers != nil {
		t.Fatalf("local %s %+v", local.Device, local.Layers)
	}
	if rpc.Layers != nil {
		t.Fatalf("rpc %+v", rpc.Layers)
	}
}

// A server on the CPU alone prints only host-memory rows, before and after load: the last are the local device, and the machine's
// RAM is its total.
func TestCPUOnlyServerIsItsHostRows(t *testing.T) {
	c := &Collector{Log: split(t, "server-cpu-lv4.log"), HostMem: 16 << 30}
	s := c.build(nil, nil, false, nil)
	n := s.Nodes[0]
	if n.Device != "CPU" || n.GetMemTotal() != 16<<30 || n.GetMemModel() != float64((3021+1349)*loadlog.MiB) {
		t.Fatalf("cpu node %s total %v model %v", n.Device, n.MemTotal, n.MemModel)
	}
	if n.Layers.First != 0 || n.Layers.Last != 34 || s.Model.Structure.NLayer != 35 {
		t.Fatalf("layers %+v of %d", n.Layers, s.Model.Structure.NLayer)
	}
}

// Without the machine's RAM, a CPU-only server's total is unknown, not 0.
func TestCPUOnlyServerWithoutHostMemory(t *testing.T) {
	c := &Collector{Log: split(t, "server-cpu-lv4.log")}
	if n := c.build(nil, nil, false, nil).Nodes[0]; n.MemTotal != nil || n.MemModel == nil {
		t.Fatalf("cpu node total %v model %v", n.MemTotal, n.MemModel)
	}
}

// A second GPU is a further node of the same server, and its memory counts in the server's total.
func TestSecondLocalDeviceIsItsOwnNode(t *testing.T) {
	sp := loadlog.Split{Devices: []loadlog.Device{
		{Name: "CUDA0", Total: 32 << 30, Model: 12 << 30},
		{Name: "CUDA1", Total: 32 << 30, Model: 12 << 30},
	}, Info: loadlog.Structure{NLayer: 48}}
	c := &Collector{Log: fixed(sp)}
	s := c.build(nil, nil, false, nil)
	if len(s.Nodes) != 2 || s.Nodes[1].Kind != pb.Kind_KIND_DEVICE || s.Nodes[1].Device != "CUDA1" || s.Nodes[1].Id != "local/CUDA1" {
		t.Fatalf("nodes %v", s.Nodes)
	}
	if s.Nodes[0].Layers != nil || s.Nodes[1].Layers != nil || s.Totals.GetMemHeld() != 24<<30 {
		t.Fatalf("layers %+v %+v held %v", s.Nodes[0].Layers, s.Nodes[1].Layers, s.Totals.MemHeld)
	}
}

// A llama-server without --metrics still shows the request in flight and its live rate, from the slot.
func TestSlotAloneGivesLiveRates(t *testing.T) {
	c := &Collector{}
	c.build(nil, []llamaserver.Slot{{Processing: true, NPrompt: 40, NProcessed: 40}}, false, nil)
	c.lastT = c.lastT.Add(-time.Second)
	s := c.build(nil, []llamaserver.Slot{{Processing: true, NPrompt: 40, NProcessed: 40, NDecoded: 30}}, false, nil)
	n := s.Nodes[0]
	if n.Slot == nil || n.RequestsProcessing == nil || *n.RequestsProcessing != 1 || n.TokensPerS == nil || *n.TokensPerS < 25 {
		t.Fatalf("slot %v requests %v tokens/s %v", n.Slot, n.RequestsProcessing, n.TokensPerS)
	}
}

// Recorded on Linux from the journal: a CPU server split to an RPC node lists the RPC device, then its
// host-memory rows. The RPC device takes the first layers and the CPU the rest.
func TestCPUServerSplitToAnRPCNode(t *testing.T) {
	c := &Collector{Log: split(t, "server-cpu-rpc-journal.log"), Args: discover.Args{RPC: []string{"127.0.0.1:50052"}}, HostMem: 16 << 30}
	s := c.build(nil, nil, false, nil)
	local, rpc := s.Nodes[0], s.Nodes[1]
	if local.Device != "CPU" || rpc.Device != "RPC0" || rpc.Layers != nil || local.Layers != nil {
		t.Fatalf("local %s %+v, rpc %s %+v", local.Device, local.Layers, rpc.Device, rpc.Layers)
	}
}

func TestLogPromptRateIsHeld(t *testing.T) {
	c := &Collector{Log: requestFollower(t, "n_tokens =  10305")}
	slot := qwenSlot(t)
	s := c.build(nil, []llamaserver.Slot{slot}, false, nil)
	n := s.Nodes[0]
	if n.Slot.NPrompt != 24155 || math.Abs(*n.PromptTokensPerS-2048/18.35) > 0.01 || *n.TokensPerS != 0 {
		t.Fatalf("first poll: slot %+v, prompt rate %v, generation rate %v", n.Slot, *n.PromptTokensPerS, *n.TokensPerS)
	}
	c.lastT = c.lastT.Add(-time.Second)
	s = c.build(nil, []llamaserver.Slot{slot}, false, nil)
	if math.Abs(*s.Nodes[0].PromptTokensPerS-2048/18.35) > 0.01 {
		t.Fatalf("held rate %v", *s.Nodes[0].PromptTokensPerS)
	}
}

func TestMismatchedLogTaskIsIgnored(t *testing.T) {
	c := &Collector{Log: requestLog{request: loadlog.Request{Task: 591, NPrompt: 24155, PromptTokensPerS: 111.6}}}
	slot := &llamaserver.Slot{ID: 0, Task: 861, Processing: true, NPrompt: 10305, NProcessed: 10305}
	s := c.build(nil, []llamaserver.Slot{*slot}, false, nil)
	if s.Nodes[0].Slot.NPrompt != 10305 || s.Nodes[0].PromptTokensPerS != nil {
		t.Fatalf("stale log used: slot %+v, rate %v", s.Nodes[0].Slot, s.Nodes[0].PromptTokensPerS)
	}
}

func TestSlotRateUntilTheFirstLogProgressLine(t *testing.T) {
	c := &Collector{Log: requestLog{request: loadlog.Request{Task: 861, NPrompt: 24155}}}
	slot := &llamaserver.Slot{ID: 0, Task: 861, Processing: true, NPrompt: 0}
	c.build(nil, []llamaserver.Slot{*slot}, false, nil)
	for poll := 1; poll <= 12; poll++ {
		c.lastT = time.Now().Add(-time.Second)
		if poll == 12 {
			slot.NProcessed = 2048
		}
		c.build(nil, []llamaserver.Slot{*slot}, false, nil)
	}
	c.lastT = time.Now().Add(-time.Second)
	s := c.build(nil, []llamaserver.Slot{*slot}, false, nil)
	n := s.Nodes[0]
	if n.Slot.NPrompt != 24155 || math.Abs(*n.PromptTokensPerS-2048.0/12) > 0.1 {
		t.Fatalf("slot %+v, prompt rate %v", n.Slot, *n.PromptTokensPerS)
	}
}

func TestLogGenerationRate(t *testing.T) {
	c := &Collector{Log: requestFollower(t, "n_gen =    137")}
	slot := qwenSlot(t)
	slot.NDecoded = 137
	s := c.build(nil, []llamaserver.Slot{slot}, false, nil)
	n := s.Nodes[0]
	if math.Abs(*n.TokensPerS-11.37) > 0.001 || *n.PromptTokensPerS != 0 || n.Slot.NPrompt != 24155 {
		t.Fatalf("rates generation %v prompt %v slot %+v", *n.TokensPerS, *n.PromptTokensPerS, n.Slot)
	}
}

func TestSlotPromptRateUsesTimeBetweenChangesAndIsHeld(t *testing.T) {
	c := &Collector{}
	slot := &llamaserver.Slot{ID: 0, Task: 861, Processing: true, NPrompt: 24155, NProcessed: 2048}
	c.build(nil, []llamaserver.Slot{*slot}, false, nil)
	for poll := 1; poll <= 36; poll++ {
		c.lastT = time.Now().Add(-time.Second)
		if poll == 18 {
			slot.NProcessed = 4096
		}
		if poll == 36 {
			slot.NProcessed = 6144
		}
		s := c.build(nil, []llamaserver.Slot{*slot}, false, nil)
		rate := s.Nodes[0].PromptTokensPerS
		if poll >= 18 && (rate == nil || math.Abs(*rate-2048.0/18) > 0.1) {
			t.Fatalf("poll %d: prompt rate %v", poll, rate)
		}
		if poll < 18 && rate != nil {
			t.Fatalf("poll %d: early prompt rate %v", poll, rate)
		}
	}
}

func TestEveryBusySlotIsSummedAndQueueIsReported(t *testing.T) {
	b, err := os.ReadFile("../../testdata/llama-parallel2-poll.json")
	if err != nil {
		t.Fatal(err)
	}
	var poll struct {
		Metrics map[string]float64 `json:"metrics"`
		Slots   json.RawMessage    `json:"slots"`
	}
	if err := json.Unmarshal(b, &poll); err != nil {
		t.Fatal(err)
	}
	slots, err := llamaserver.ParseSlots(poll.Slots)
	if err != nil {
		t.Fatal(err)
	}
	log := followerThrough(t, "llama-parallel2.log", "id  1 | task 64 | n_gen =    121")
	r0, ok0 := log.Request(0)
	r1, ok1 := log.Request(1)
	if !ok0 || !ok1 {
		t.Fatalf("fixture requests %v %v", r0, r1)
	}
	c := &Collector{Log: log}
	s := c.build(poll.Metrics, slots, false, nil)
	n := s.Nodes[0]
	expected := r0.TokensPerS + r1.TokensPerS
	t.Logf("llama concurrent %.3f/server slots %.3f error %.3f%%", n.GetTokensPerS(), expected, math.Abs(n.GetTokensPerS()-expected)/expected*100)
	if n.TokensPerS == nil || math.Abs(*n.TokensPerS-expected) > expected*0.05 || n.RequestsProcessing == nil || *n.RequestsProcessing != 2 || n.RequestsQueued == nil || *n.RequestsQueued != 1 {
		t.Fatalf("rates %v processing %v queued %v", n.TokensPerS, n.RequestsProcessing, n.RequestsQueued)
	}
	if n.Slot == nil || n.Slot.NPrompt != 4180 {
		t.Fatalf("oldest slot %+v", n.Slot)
	}
}

// Under heavy prefill /slots can miss a poll while /metrics answers: the busy slots carry for that poll.
func TestBusySlotsCarryOverOneMissedSlotsPoll(t *testing.T) {
	c := &Collector{Log: requestLogs{
		0: {Task: 66, NGenerated: 121, TokensPerS: 5.60},
		1: {Task: 64, NGenerated: 121, TokensPerS: 5.90},
	}}
	metrics := map[string]float64{"llamacpp:requests_processing": 2, "llamacpp:requests_deferred": 1}
	c.last = c.build(metrics, []llamaserver.Slot{{ID: 0, Task: 66, Processing: true, NDecoded: 121}, {ID: 1, Task: 64, Processing: true, NDecoded: 121}}, false, nil)
	c.last = c.build(metrics, nil, false, nil)
	n := c.last.Nodes[0]
	if n.TokensPerS == nil || math.Abs(*n.TokensPerS-11.5) > 0.001 || *n.RequestsProcessing != 2 || n.Slot == nil || !n.Slot.Processing {
		t.Fatalf("missed poll: %+v", n)
	}
	c.last = c.build(metrics, nil, false, nil)
	if n := c.last.Nodes[0]; n.Slot != nil {
		t.Fatalf("second missed poll still carried: %+v", n)
	}
}

// An idle server generates nothing, which is known: its rates are 0, not unknown.
func TestIdleRatesAreZero(t *testing.T) {
	c := &Collector{}
	idle := []llamaserver.Slot{{ID: 0}, {ID: 1}}
	for _, metrics := range []map[string]float64{nil, {"llamacpp:requests_processing": 0}} {
		c.lastT = time.Now().Add(-time.Second)
		n := c.build(metrics, idle, false, nil).Nodes[0]
		if n.TokensPerS == nil || *n.TokensPerS != 0 || n.PromptTokensPerS == nil || *n.PromptTokensPerS != 0 {
			t.Fatalf("metrics %v: idle rates %v %v", metrics, n.TokensPerS, n.PromptTokensPerS)
		}
	}
}

// A decoding slot whose count did not move between polls contributes 0; it does not make the sum unknown.
func TestStalledDecodeIsZeroNotUnknown(t *testing.T) {
	c := &Collector{}
	slot := llamaserver.Slot{ID: 0, Task: 5, Processing: true, NPrompt: 40, NProcessed: 40, NDecoded: 30}
	c.build(nil, []llamaserver.Slot{slot}, false, nil)
	c.lastT = c.lastT.Add(-time.Second)
	n := c.build(nil, []llamaserver.Slot{slot}, false, nil).Nodes[0]
	if n.TokensPerS == nil || *n.TokensPerS != 0 {
		t.Fatalf("stalled decode rate %v", n.TokensPerS)
	}
}

func TestQueueIsUnknownWithoutMetrics(t *testing.T) {
	c := &Collector{}
	s := c.build(nil, []llamaserver.Slot{{ID: 0}}, false, nil)
	if s.Nodes[0].RequestsQueued != nil {
		t.Fatalf("queued %v", *s.Nodes[0].RequestsQueued)
	}
}

func TestMetricsProcessingCountIsAuthoritative(t *testing.T) {
	c := &Collector{}
	s := c.build(map[string]float64{"llamacpp:requests_processing": 3}, []llamaserver.Slot{{ID: 0, Processing: true}}, false, nil)
	if s.Nodes[0].RequestsProcessing == nil || *s.Nodes[0].RequestsProcessing != 3 || s.Nodes[0].RequestsQueued != nil {
		t.Fatalf("processing %v queued %v", s.Nodes[0].RequestsProcessing, s.Nodes[0].RequestsQueued)
	}
}
