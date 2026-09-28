// Package snapshot assembles one llama-server's picture: nodes, links and totals, with rates computed from
// counter deltas between polls.
package snapshot

import (
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/benemon/llamesh/internal/discover"
	"github.com/benemon/llamesh/internal/link"
	"github.com/benemon/llamesh/internal/llamaserver"
	"github.com/benemon/llamesh/internal/loadlog"
	pb "github.com/benemon/llamesh/internal/pb/llamesh/v1"
)

// Collector holds the discovered target and the previous poll's counters.
type Collector struct {
	Client        *llamaserver.Client
	Args          discover.Args
	Log           loadlog.Source
	Local         string            // hostname of the machine running llama-server
	HostMem       int64             // the machine's RAM: the total of a server on the CPU alone
	Names         map[string]string // RPC node IP -> discovered hostname
	Ifaces        map[string]string // RPC node address -> interface
	props         llamaserver.Props
	lastT         time.Time
	lastM         map[string]float64
	lastSlot      llamaserver.Slot
	promptElapsed float64
	promptRate    float64
	lastLink      map[string]link.Counters
	last          *pb.Snapshot
}

// Poll reads the live sources once and returns the snapshot. A source that fails keeps its previous
// values and marks what it feeds as stale.
func (c *Collector) Poll() *pb.Snapshot {
	// /props answers 503 while the model loads; keep asking until it answers.
	if c.props.NCtx == 0 {
		if p, err := c.Client.Props(); err == nil {
			c.props = p
		}
	}
	metrics, merr := c.Client.Metrics() // served only when llama-server runs with --metrics
	var slot *llamaserver.Slot
	if sl, err := c.Client.Slot(); err == nil {
		slot = &sl
	}
	counters := map[string]link.Counters{}
	for addr, iface := range c.Ifaces {
		if cnt, err := link.Read(iface); err == nil {
			counters[addr] = cnt
		}
	}
	s := c.build(metrics, slot, merr != nil && slot == nil, counters)
	c.last = s
	return s
}

func (c *Collector) build(metrics map[string]float64, slot *llamaserver.Slot, serverStale bool,
	counters map[string]link.Counters) *pb.Snapshot {
	now := time.Now()
	dt := now.Sub(c.lastT).Seconds()
	split := loadlog.Split{}
	if c.Log != nil {
		split = c.Log.Latest()
	}
	// The table lists devices in llama.cpp's order, the order layers are assigned in. RPC devices are
	// matched by endpoint, since llama.cpp numbers them by the nodes that registered and a skipped node
	// shifts the numbering. A server on the CPU alone has no device rows, only host-memory rows.
	ordered := append([]loadlog.Device{}, split.Devices...)
	var locals []loadlog.Device
	byEndpoint := map[string]loadlog.Device{}
	for _, d := range ordered {
		if strings.HasPrefix(d.Name, "RPC") {
			byEndpoint[d.Endpoint] = d
		} else {
			locals = append(locals, d)
		}
	}
	if len(locals) == 0 && len(split.Host) > 0 {
		cpu := loadlog.Device{Name: "CPU", Total: c.HostMem}
		for _, h := range split.Host {
			cpu.Model, cpu.Context, cpu.Compute = cpu.Model+h.Model, cpu.Context+h.Context, cpu.Compute+h.Compute
		}
		ordered = append(ordered, cpu)
		locals = []loadlog.Device{cpu}
	}
	s := &pb.Snapshot{
		T:      float64(now.UnixNano()) / 1e9,
		Source: c.Local,
		Model: &pb.Model{
			Path:  c.props.ModelPath,
			Name:  strings.TrimSuffix(filepath.Base(c.props.ModelPath), ".gguf"),
			NCtx:  int32(c.props.NCtx),
			Build: c.props.Build,
			Structure: &pb.Structure{
				NLayer:      int32(split.Info.NLayer),
				NExpert:     int32(split.Info.NExpert),
				NExpertUsed: int32(split.Info.NExpertUse),
			},
		},
		Totals: &pb.Totals{},
	}
	layersOf := map[string]*pb.Layers{}
	for i, r := range loadlog.LayerRanges(ordered, split.Info.NLayer) {
		layersOf[ordered[i].Name] = &pb.Layers{First: int32(r[0]), Last: int32(r[1])}
	}
	memOf := func(n *pb.Node, d loadlog.Device) {
		n.MemTotal, n.MemModel, n.MemContext, n.MemCompute = float64(d.Total), float64(d.Model), float64(d.Context), float64(d.Compute)
		n.Layers = layersOf[d.Name]
	}

	local := &pb.Node{Id: "local", Kind: pb.Kind_KIND_LLAMA_SERVER, Label: c.Local}
	if len(locals) > 0 {
		local.Device = locals[0].Name
		memOf(local, locals[0])
	}
	c.rates(local, s, metrics, slot, dt)
	local.Stale = serverStale
	s.Nodes = append(s.Nodes, local)
	s.Totals.MemHeld = local.MemModel + local.MemContext + local.MemCompute
	for _, d := range locals[min(1, len(locals)):] {
		n := &pb.Node{Id: "local/" + d.Name, Kind: pb.Kind_KIND_DEVICE, Device: d.Name, Label: c.Local, Stale: serverStale}
		memOf(n, d)
		s.Nodes = append(s.Nodes, n)
		s.Totals.MemHeld += n.MemModel + n.MemContext + n.MemCompute
	}

	c.rpcNodes(s, byEndpoint, memOf, counters, dt)
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

// rates fills the server node's live figures. The metrics give counter rates, requests in flight and
// tokens generated; the slot gives the request in flight and its live rates. Either may be missing (a
// server without --metrics has no metrics); with neither, the last poll's figures carry forward.
func (c *Collector) rates(local *pb.Node, s *pb.Snapshot, metrics map[string]float64, slot *llamaserver.Slot, dt float64) {
	if metrics == nil && slot == nil {
		if c.last != nil {
			for _, n := range c.last.Nodes {
				if n.Id == "local" {
					local.TokensPerS, local.PromptTokensPerS = n.TokensPerS, n.PromptTokensPerS
					local.RequestsProcessing, local.Slot = n.RequestsProcessing, n.Slot
				}
			}
			s.Totals.TokensPredicted = c.last.Totals.TokensPredicted
		}
		return
	}
	zero := 0.0
	local.TokensPerS, local.PromptTokensPerS = &zero, &zero
	if metrics != nil {
		rate := func(name string) *float64 {
			v := 0.0
			if c.lastM != nil && dt > 0 {
				v = max(0, (metrics[name]-c.lastM[name])/dt)
			}
			return &v
		}
		local.TokensPerS = rate("llamacpp:tokens_predicted_total")
		local.PromptTokensPerS = rate("llamacpp:prompt_tokens_total")
		rp := int32(metrics["llamacpp:requests_processing"])
		local.RequestsProcessing = &rp
		s.Totals.TokensPredicted = metrics["llamacpp:tokens_predicted_total"]
	}
	if slot == nil {
		return
	}
	request, fromLog := loadlog.Request{}, false
	if c.Log != nil {
		request, fromLog = c.Log.Request(slot.ID)
		fromLog = fromLog && request.Task == slot.Task
	}
	nPrompt := slot.NPrompt
	if fromLog {
		nPrompt = request.NPrompt
	}
	// llamacpp's counters advance when a request completes; while one is in flight the log or slot
	// progress is the live rate.
	if slot.Processing && dt > 0 {
		if slot.NDecoded == 0 && (!fromLog || request.NGenerated == 0) {
			local.TokensPerS = &zero
			c.slotPromptRate(slot, dt)
			local.PromptTokensPerS = &c.promptRate
			// The log prints a chunk's line seconds after /slots shows it, and nothing before the first
			// chunk ends; until then the slot's own steps give the rate.
			if fromLog && request.PromptTokensPerS > 0 {
				local.PromptTokensPerS = &request.PromptTokensPerS
			}
		} else {
			c.promptElapsed, c.promptRate = 0, 0
			prev := c.lastSlot.NDecoded
			if slot.NDecoded < prev {
				prev = 0
			}
			g := float64(slot.NDecoded-prev) / dt
			if fromLog && request.NGenerated > 0 {
				g = request.TokensPerS
			}
			if g > 0 || *local.TokensPerS == 0 {
				local.TokensPerS = &g
			}
			local.PromptTokensPerS = &zero
		}
	} else if c.lastSlot.Processing {
		// The request just finished: its tokens were counted live as it ran, and the counters now jump
		// by the whole request at once, which would read as a burst.
		local.TokensPerS, local.PromptTokensPerS = &zero, &zero
		c.promptElapsed, c.promptRate = 0, 0
	}
	if metrics == nil {
		rp := int32(0)
		if slot.Processing {
			rp = 1
		}
		local.RequestsProcessing = &rp
	}
	c.lastSlot = *slot
	local.Slot = &pb.Slot{Processing: slot.Processing, NPrompt: int32(nPrompt), NCached: int32(slot.NCached),
		NProcessed: int32(slot.NProcessed), NDecoded: int32(slot.NDecoded)}
}

// slotPromptRate updates the prompt rate from /slots: prefill advances in ubatch chunks, many polls apart,
// so the rate is the tokens of a step over the time since the previous step, held until the next.
func (c *Collector) slotPromptRate(slot *llamaserver.Slot, dt float64) {
	last := c.lastSlot
	if !last.Processing || last.ID != slot.ID || last.Task != slot.Task || last.NDecoded != 0 {
		c.promptElapsed, c.promptRate = 0, 0
		return
	}
	c.promptElapsed += dt
	if slot.NProcessed != last.NProcessed {
		c.promptRate = float64(slot.NProcessed-last.NProcessed) / c.promptElapsed
		c.promptElapsed = 0
	}
}

// rpcNodes adds a node and a link per RPC address on the command line, in that order.
func (c *Collector) rpcNodes(s *pb.Snapshot, byEndpoint map[string]loadlog.Device, memOf func(*pb.Node, loadlog.Device),
	counters map[string]link.Counters, dt float64) {
	for i, addr := range c.Args.RPC {
		dev := "RPC" + strconv.Itoa(i)
		n := &pb.Node{Id: addr, Kind: pb.Kind_KIND_RPC, Device: dev, Label: dev}
		if d, ok := byEndpoint[addr]; ok {
			dev = d.Name
			n.Device = dev
			memOf(n, d)
		} else {
			n.Stale = true // listed on the command line, absent from the load: the server skipped it
		}
		if host, _, err := net.SplitHostPort(addr); err == nil && c.Names[host] != "" {
			n.Label = c.Names[host]
		} else if n.Label == dev {
			n.Label = n.Device
		}
		s.Nodes = append(s.Nodes, n)
		s.Totals.MemHeld += n.MemModel + n.MemContext + n.MemCompute
		l := &pb.Link{From: "local", To: addr, Iface: c.Ifaces[addr]}
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
}
