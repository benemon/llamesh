package snapshot

import (
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

func qwenSlot(t *testing.T) llamaserver.Slot {
	t.Helper()
	s, err := llamaserver.ParseSlots([]byte(`[{"id":0,"is_processing":true,"id_task":861,"n_prompt_tokens":10305,"n_prompt_tokens_processed":10305,"n_prompt_tokens_cache":0,"next_token":[{"has_next_token":false,"n_remain":4096,"n_decoded":0}]}]`))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// Layers go to devices in the table's order, which lists the RPC device first: its 5921 MiB of the model
// is 8 of the 24 layers, and the local Metal device holds the rest.
func TestLayersFollowTheTablesOrder(t *testing.T) {
	c := &Collector{Log: split(t, "server-lv4.log"), Args: discover.Args{RPC: []string{"10.0.0.2:50052"}}}
	s := c.build(nil, nil, false, nil)
	local, rpc := s.Nodes[0], s.Nodes[1]
	if local.Device != "MTL0" || local.Layers.First != 8 || local.Layers.Last != 23 {
		t.Fatalf("local %s %+v", local.Device, local.Layers)
	}
	if rpc.Layers.First != 0 || rpc.Layers.Last != 7 {
		t.Fatalf("rpc %+v", rpc.Layers)
	}
}

// A server on the CPU alone prints only host-memory rows, before and after load: the last are the local device, and the machine's
// RAM is its total.
func TestCPUOnlyServerIsItsHostRows(t *testing.T) {
	c := &Collector{Log: split(t, "server-cpu-lv4.log"), HostMem: 16 << 30}
	s := c.build(nil, nil, false, nil)
	n := s.Nodes[0]
	if n.Device != "CPU" || n.MemTotal != 16<<30 || n.MemModel != float64((3021+1349)*loadlog.MiB) {
		t.Fatalf("cpu node %s total %v model %v", n.Device, n.MemTotal, n.MemModel)
	}
	if n.Layers.First != 0 || n.Layers.Last != 34 || s.Model.Structure.NLayer != 35 {
		t.Fatalf("layers %+v of %d", n.Layers, s.Model.Structure.NLayer)
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
	if s.Nodes[0].Layers.Last != 23 || s.Nodes[1].Layers.First != 24 || s.Totals.MemHeld != 24<<30 {
		t.Fatalf("layers %+v %+v held %v", s.Nodes[0].Layers, s.Nodes[1].Layers, s.Totals.MemHeld)
	}
}

// A llama-server without --metrics still shows the request in flight and its live rate, from the slot.
func TestSlotAloneGivesLiveRates(t *testing.T) {
	c := &Collector{}
	c.build(nil, &llamaserver.Slot{Processing: true, NPrompt: 40, NProcessed: 40}, false, nil)
	c.lastT = c.lastT.Add(-time.Second)
	s := c.build(nil, &llamaserver.Slot{Processing: true, NPrompt: 40, NProcessed: 40, NDecoded: 30}, false, nil)
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
	if local.Device != "CPU" || rpc.Device != "RPC0" || rpc.Layers.First != 0 || local.Layers.Last != 29 || local.Layers.First != rpc.Layers.Last+1 {
		t.Fatalf("local %s %+v, rpc %s %+v", local.Device, local.Layers, rpc.Device, rpc.Layers)
	}
}

func TestLogPromptRateIsHeld(t *testing.T) {
	c := &Collector{Log: requestFollower(t, "n_tokens =  10305")}
	slot := qwenSlot(t)
	s := c.build(nil, &slot, false, nil)
	n := s.Nodes[0]
	if n.Slot.NPrompt != 24155 || math.Abs(*n.PromptTokensPerS-2048/18.35) > 0.01 || *n.TokensPerS != 0 {
		t.Fatalf("first poll: slot %+v, prompt rate %v, generation rate %v", n.Slot, *n.PromptTokensPerS, *n.TokensPerS)
	}
	c.lastT = c.lastT.Add(-time.Second)
	s = c.build(nil, &slot, false, nil)
	if math.Abs(*s.Nodes[0].PromptTokensPerS-2048/18.35) > 0.01 {
		t.Fatalf("held rate %v", *s.Nodes[0].PromptTokensPerS)
	}
}

func TestMismatchedLogTaskIsIgnored(t *testing.T) {
	c := &Collector{Log: requestLog{request: loadlog.Request{Task: 591, NPrompt: 24155, PromptTokensPerS: 111.6}}}
	slot := &llamaserver.Slot{ID: 0, Task: 861, Processing: true, NPrompt: 10305, NProcessed: 10305}
	s := c.build(nil, slot, false, nil)
	if s.Nodes[0].Slot.NPrompt != 10305 || *s.Nodes[0].PromptTokensPerS != 0 {
		t.Fatalf("stale log used: slot %+v, rate %v", s.Nodes[0].Slot, *s.Nodes[0].PromptTokensPerS)
	}
}

func TestSlotRateUntilTheFirstLogProgressLine(t *testing.T) {
	c := &Collector{Log: requestLog{request: loadlog.Request{Task: 861, NPrompt: 24155}}}
	slot := &llamaserver.Slot{ID: 0, Task: 861, Processing: true, NPrompt: 0}
	c.build(nil, slot, false, nil)
	for poll := 1; poll <= 12; poll++ {
		c.lastT = time.Now().Add(-time.Second)
		if poll == 12 {
			slot.NProcessed = 2048
		}
		c.build(nil, slot, false, nil)
	}
	c.lastT = time.Now().Add(-time.Second)
	s := c.build(nil, slot, false, nil)
	n := s.Nodes[0]
	if n.Slot.NPrompt != 24155 || math.Abs(*n.PromptTokensPerS-2048.0/12) > 0.1 {
		t.Fatalf("slot %+v, prompt rate %v", n.Slot, *n.PromptTokensPerS)
	}
}

func TestLogGenerationRate(t *testing.T) {
	c := &Collector{Log: requestFollower(t, "n_gen =    137")}
	slot := qwenSlot(t)
	slot.NDecoded = 137
	s := c.build(nil, &slot, false, nil)
	n := s.Nodes[0]
	if math.Abs(*n.TokensPerS-11.37) > 0.001 || *n.PromptTokensPerS != 0 || n.Slot.NPrompt != 24155 {
		t.Fatalf("rates generation %v prompt %v slot %+v", *n.TokensPerS, *n.PromptTokensPerS, n.Slot)
	}
}

func TestSlotPromptRateUsesTimeBetweenChangesAndIsHeld(t *testing.T) {
	c := &Collector{}
	slot := &llamaserver.Slot{ID: 0, Task: 861, Processing: true, NPrompt: 24155, NProcessed: 2048}
	c.build(nil, slot, false, nil)
	for poll := 1; poll <= 36; poll++ {
		c.lastT = time.Now().Add(-time.Second)
		if poll == 18 {
			slot.NProcessed = 4096
		}
		if poll == 36 {
			slot.NProcessed = 6144
		}
		s := c.build(nil, slot, false, nil)
		rate := *s.Nodes[0].PromptTokensPerS
		if poll >= 18 && math.Abs(rate-2048.0/18) > 0.1 {
			t.Fatalf("poll %d: prompt rate %v", poll, rate)
		}
		if poll < 18 && rate != 0 {
			t.Fatalf("poll %d: early prompt rate %v", poll, rate)
		}
	}
}
