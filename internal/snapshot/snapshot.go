// Package snapshot assembles what the page draws: nodes, links and totals, with rates computed from
// counter deltas between polls.
package snapshot

import (
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/benemon/llamesh/internal/discover"
	"github.com/benemon/llamesh/internal/link"
	"github.com/benemon/llamesh/internal/llamaserver"
	"github.com/benemon/llamesh/internal/loadlog"
)

type Model struct {
	Path      string            `json:"path"`
	Name      string            `json:"name"`
	NCtx      int               `json:"n_ctx"`
	Build     string            `json:"build"`
	Structure loadlog.Structure `json:"structure"`
}

type Node struct {
	ID                 string            `json:"id"`
	Kind               string            `json:"kind"`
	Device             string            `json:"device"`
	Label              string            `json:"label"`
	MemTotal           int64             `json:"mem_total"`
	MemModel           int64             `json:"mem_model"`
	MemContext         int64             `json:"mem_context"`
	MemCompute         int64             `json:"mem_compute"`
	Layers             *[2]int           `json:"layers,omitempty"` // derived from bytes; see loadlog.LayerRanges
	TokensPerS         *float64          `json:"tokens_per_s,omitempty"`
	PromptTokensPerS   *float64          `json:"prompt_tokens_per_s,omitempty"`
	RequestsProcessing *int              `json:"requests_processing,omitempty"`
	Slot               *llamaserver.Slot `json:"slot,omitempty"`
	Stale              bool              `json:"stale,omitempty"`
}

type Link struct {
	From         string  `json:"from"`
	To           string  `json:"to"`
	Iface        string  `json:"iface"`
	BytesOutPerS float64 `json:"bytes_out_per_s"`
	BytesInPerS  float64 `json:"bytes_in_per_s"`
	Stale        bool    `json:"stale,omitempty"`
}

type Totals struct {
	TokensPredicted float64 `json:"tokens_predicted"`
	PromptTokens    float64 `json:"prompt_tokens"`
	MemHeld         int64   `json:"mem_held"`
}

type Snapshot struct {
	T      float64 `json:"t"`
	Model  Model   `json:"model"`
	Nodes  []Node  `json:"nodes"`
	Links  []Link  `json:"links"`
	Totals Totals  `json:"totals"`
}

type Topology struct {
	Nodes []Node `json:"nodes"`
	Links []Link `json:"links"`
}

// Collector holds the discovered target and the previous poll's counters.
type Collector struct {
	Client   *llamaserver.Client
	Args     discover.Args
	Log      *loadlog.Follower
	Local    string            // hostname of the machine running llama-server
	Names    map[string]string // RPC node IP -> discovered hostname
	Ifaces   map[string]string // RPC node address -> interface
	props    llamaserver.Props
	lastT    time.Time
	lastM    map[string]float64
	lastLink map[string]link.Counters
	last     *Snapshot
}

func (c *Collector) SetProps(p llamaserver.Props) { c.props = p }

func (c *Collector) Topology() Topology {
	s := c.build(nil, llamaserver.Slot{}, false, nil)
	for i := range s.Nodes {
		s.Nodes[i].TokensPerS, s.Nodes[i].PromptTokensPerS, s.Nodes[i].RequestsProcessing, s.Nodes[i].Slot = nil, nil, nil, nil
	}
	return Topology{Nodes: s.Nodes, Links: s.Links}
}

// Poll reads the live sources once and returns the snapshot. A source that fails keeps its previous
// values and marks what it feeds as stale.
func (c *Collector) Poll() Snapshot {
	metrics, merr := c.Client.Metrics()
	slot, serr := c.Client.Slot()
	counters := map[string]link.Counters{}
	for addr, iface := range c.Ifaces {
		if cnt, err := link.Read(iface); err == nil {
			counters[addr] = cnt
		}
	}
	s := c.build(metrics, slot, merr != nil || serr != nil, counters)
	c.last = &s
	return s
}

func (c *Collector) build(metrics map[string]float64, slot llamaserver.Slot, serverStale bool, counters map[string]link.Counters) Snapshot {
	now := time.Now()
	dt := now.Sub(c.lastT).Seconds()
	split := loadlog.Split{}
	if c.Log != nil {
		split = c.Log.Latest()
	}
	// Devices by name for the local one and by endpoint for the RPC ones: llama.cpp numbers RPC devices
	// by the nodes that registered, so a node that was skipped shifts the numbering.
	byDev := map[string]loadlog.Device{}
	byEndpoint := map[string]loadlog.Device{}
	for _, d := range split.Devices {
		byDev[d.Name] = d
		if strings.HasPrefix(d.Name, "RPC") {
			byEndpoint[d.Endpoint] = d
		}
	}
	s := Snapshot{T: float64(now.UnixNano()) / 1e9}
	s.Model = Model{Path: c.props.ModelPath, Name: strings.TrimSuffix(filepath.Base(c.props.ModelPath), ".gguf"), NCtx: c.props.NCtx, Build: c.props.Build, Structure: split.Info}
	// layer ranges in device order: MTL0 first, then the RPC devices as listed
	ordered := []loadlog.Device{}
	if d, ok := byDev["MTL0"]; ok {
		ordered = append(ordered, d)
	}
	for _, addr := range c.Args.RPC {
		if d, ok := byEndpoint[addr]; ok {
			ordered = append(ordered, d)
		}
	}
	layersOf := map[string]*[2]int{}
	for i, r := range loadlog.LayerRanges(ordered, split.Info.NLayer) {
		rr := r
		layersOf[ordered[i].Name] = &rr
	}

	local := Node{ID: "local", Kind: "llama-server", Device: "MTL0", Label: c.Local}
	if d, ok := byDev["MTL0"]; ok {
		local.MemTotal, local.MemModel, local.MemContext, local.MemCompute = d.Total, d.Model, d.Context, d.Compute
		local.Layers = layersOf["MTL0"]
		if local.Label == "" {
			local.Label = d.Endpoint
		}
	}
	if metrics != nil {
		rate := func(name string) *float64 {
			v := 0.0
			if c.lastM != nil && dt > 0 {
				v = (metrics[name] - c.lastM[name]) / dt
				if v < 0 {
					v = 0
				}
			}
			return &v
		}
		local.TokensPerS = rate("llamacpp:tokens_predicted_total")
		local.PromptTokensPerS = rate("llamacpp:prompt_tokens_total")
		rp := int(metrics["llamacpp:requests_processing"])
		local.RequestsProcessing = &rp
		sl := slot
		local.Slot = &sl
		s.Totals.TokensPredicted = metrics["llamacpp:tokens_predicted_total"]
		s.Totals.PromptTokens = metrics["llamacpp:prompt_tokens_total"]
	} else if c.last != nil {
		for _, n := range c.last.Nodes {
			if n.ID == "local" {
				local.TokensPerS, local.PromptTokensPerS, local.RequestsProcessing, local.Slot = n.TokensPerS, n.PromptTokensPerS, n.RequestsProcessing, n.Slot
			}
		}
		s.Totals = c.last.Totals
	}
	local.Stale = serverStale
	s.Nodes = append(s.Nodes, local)
	s.Totals.MemHeld = local.MemModel + local.MemContext + local.MemCompute

	for i, addr := range c.Args.RPC {
		dev := "RPC" + itoa(i)
		n := Node{ID: addr, Kind: "rpc", Device: dev, Label: dev}
		if d, ok := byEndpoint[addr]; ok {
			dev = d.Name
			n.Device = dev
			n.MemTotal, n.MemModel, n.MemContext, n.MemCompute = d.Total, d.Model, d.Context, d.Compute
			n.Layers = layersOf[dev]
		} else {
			n.Stale = true // listed on the command line, absent from the load: the server skipped it
		}
		if name, ok := c.Names[hostOf(addr)]; ok {
			n.Label = name
		} else if n.Label == dev {
			n.Label = n.Device
		}
		s.Nodes = append(s.Nodes, n)
		s.Totals.MemHeld += n.MemModel + n.MemContext + n.MemCompute
		l := Link{From: "local", To: addr, Iface: c.Ifaces[addr]}
		cnt, ok := counters[addr]
		if prev, had := c.lastLink[addr]; ok && had && dt > 0 {
			l.BytesOutPerS = float64(cnt.Out-prev.Out) / dt
			l.BytesInPerS = float64(cnt.In-prev.In) / dt
			if l.BytesOutPerS < 0 || l.BytesInPerS < 0 {
				l.BytesOutPerS, l.BytesInPerS = 0, 0
			}
		} else if !ok && c.last != nil {
			for _, pl := range c.last.Links {
				if pl.To == addr {
					l.BytesOutPerS, l.BytesInPerS, l.Stale = pl.BytesOutPerS, pl.BytesInPerS, true
				}
			}
		}
		s.Links = append(s.Links, l)
	}
	if metrics != nil {
		c.lastM = metrics
	}
	if counters != nil {
		if c.lastLink == nil {
			c.lastLink = map[string]link.Counters{}
		}
		for k, v := range counters {
			c.lastLink[k] = v
		}
	}
	c.lastT = now
	return s
}

func hostOf(addr string) string {
	if i := strings.LastIndexByte(addr, ':'); i >= 0 {
		return addr[:i]
	}
	return addr
}

func itoa(i int) string { return strconv.Itoa(i) }
