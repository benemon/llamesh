package snapshot

import (
	"os"
	"testing"
	"time"

	"github.com/benemon/llamesh/internal/discover"
	"github.com/benemon/llamesh/internal/llamaserver"
	"github.com/benemon/llamesh/internal/loadlog"
	pb "github.com/benemon/llamesh/internal/pb/llamesh/v1"
)

type fixed loadlog.Split

func (f fixed) Latest() loadlog.Split { return loadlog.Split(f) }

func split(t *testing.T, name string) fixed {
	b, err := os.ReadFile("../../testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return fixed(loadlog.Parse(string(b)))
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
