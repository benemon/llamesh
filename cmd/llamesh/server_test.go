package main

import (
	"encoding/json"
	"testing"

	pb "github.com/benemon/llamesh/internal/pb/llamesh/v1"
)

// The page compares kind by name and iterates links, so the JSON it receives carries the enum's name and
// empty lists as []; an RPC node on a host with its own collector takes that collector's name.
func TestHubRendersForThePage(t *testing.T) {
	h := newHub()
	h.announce(2, &pb.Hello{Host: "vega", Addresses: []string{"10.0.0.2"}})
	h.publish(1, &pb.Snapshot{Source: "orion", Target: "8896", Nodes: []*pb.Node{
		{Id: "local", Kind: pb.Kind_KIND_LLAMA_SERVER, Label: "orion"},
		{Id: "10.0.0.2:50052", Kind: pb.Kind_KIND_RPC, Label: "RPC0"},
	}})
	var got struct {
		Nodes []struct{ Kind, Label string }
		Links []any
	}
	if err := json.Unmarshal(h.latest["orion/8896"].json, &got); err != nil {
		t.Fatal(err)
	}
	if got.Nodes[0].Kind != "KIND_LLAMA_SERVER" || got.Nodes[1].Label != "vega" {
		t.Fatalf("nodes %+v", got.Nodes)
	}
	if got.Links == nil {
		t.Fatal("links must arrive as [], not be omitted")
	}
}

// A collector that reconnects before the server notices its old stream has ended: the old stream's late
// withdrawal must leave the new stream's picture and addresses alone.
func TestWithdrawLeavesANewerStreamsReport(t *testing.T) {
	h := newHub()
	hello := &pb.Hello{Host: "vega", Addresses: []string{"10.0.0.2"}}
	h.announce(1, hello)
	h.publish(1, &pb.Snapshot{Source: "vega", Target: "8891"})
	h.announce(2, hello)
	h.publish(2, &pb.Snapshot{Source: "vega", Target: "8891"})
	h.withdraw(1)
	if h.latest["vega/8891"].owner != 2 || h.hosts["10.0.0.2"].host != "vega" {
		t.Fatal("the old stream withdrew the new one's report")
	}
	h.forget(1, "vega/8891")
	if _, ok := h.latest["vega/8891"]; !ok {
		t.Fatal("a Gone from the old stream withdrew the new one's picture")
	}
	h.withdraw(2)
	if len(h.latest) != 0 || len(h.hosts) != 0 {
		t.Fatal("a stream's report must go with it")
	}
}
