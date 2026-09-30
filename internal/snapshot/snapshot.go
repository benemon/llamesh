// Package snapshot assembles one model server's picture: nodes, links and totals, with rates computed from
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
	Client    *llamaserver.Client
	Args      discover.Args
	Log       loadlog.Source
	Local     string            // hostname of the machine running llama-server
	HostMem   int64             // the machine's RAM: the total of a server on the CPU alone
	Names     map[string]string // RPC node IP -> discovered hostname
	Ifaces    map[string]string // RPC node address -> interface
	props     llamaserver.Props
	lastT     time.Time
	lastM     map[string]float64
	lastSlots map[int]slotState
	carried   bool
	lastLink  map[string]link.Counters
	last      *pb.Snapshot
}

type slotState struct {
	slot          llamaserver.Slot
	promptElapsed float64
	promptRate    float64
}

func number(v int64) *float64 {
	n := float64(v)
	return &n
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
	var slots []llamaserver.Slot
	if sl, err := c.Client.Slots(); err == nil {
		slots = sl
	}
	counters := map[string]link.Counters{}
	for addr, iface := range c.Ifaces {
		if cnt, err := link.Read(iface); err == nil {
			counters[addr] = cnt
		}
	}
	s := c.build(metrics, slots, merr != nil && slots == nil, counters)
	c.last = s
	return s
}

func (c *Collector) build(metrics map[string]float64, slots []llamaserver.Slot, serverStale bool,
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
			Engine: discover.EngineLlama,
		},
		Totals: &pb.Totals{},
	}
	layersOf := map[string]*pb.Layers{}
	if len(ordered) == 1 && split.Info.NLayer > 0 {
		layersOf[ordered[0].Name] = &pb.Layers{First: 0, Last: int32(split.Info.NLayer - 1)}
	}
	memOf := func(n *pb.Node, d loadlog.Device) {
		n.MemTotal, n.MemModel, n.MemContext, n.MemCompute = number(d.Total), number(d.Model), number(d.Context), number(d.Compute)
		if d.Name == "CPU" && d.Total == 0 {
			n.MemTotal = nil
		}
		n.Layers = layersOf[d.Name]
	}

	local := &pb.Node{Id: "local", Kind: pb.Kind_KIND_LLAMA_SERVER, Label: c.Local}
	if len(locals) > 0 {
		local.Device = locals[0].Name
		memOf(local, locals[0])
	}
	c.rates(local, s, metrics, slots, dt)
	local.Stale = serverStale
	s.Nodes = append(s.Nodes, local)
	for _, d := range locals[min(1, len(locals)):] {
		n := &pb.Node{Id: "local/" + d.Name, Kind: pb.Kind_KIND_DEVICE, Device: d.Name, Label: c.Local, Stale: serverStale}
		memOf(n, d)
		s.Nodes = append(s.Nodes, n)
	}

	c.rpcNodes(s, byEndpoint, memOf, counters, dt)
	if metrics != nil {
		c.lastM = metrics
	}
	var held float64
	known := true
	for _, n := range s.Nodes {
		if n.MemModel == nil || n.MemContext == nil || n.MemCompute == nil {
			known = false
			break
		}
		held += *n.MemModel + *n.MemContext + *n.MemCompute
	}
	if known {
		s.Totals.MemHeld = &held
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
// waiting, and tokens generated; the slots give the requests in flight and their live rates, summed over
// busy slots. A /slots answer that misses one poll (it answers slowly under heavy prefill) carries the
// previous poll's figures; with neither source, the last poll's figures carry forward.
func (c *Collector) rates(local *pb.Node, s *pb.Snapshot, metrics map[string]float64, slots []llamaserver.Slot, dt float64) {
	if slots == nil && (metrics == nil || (!c.carried && c.busy())) {
		c.restoreRates(local, s)
		if metrics != nil {
			applyMetrics(local, s, metrics)
			c.carried = true
		}
		return
	}
	if slots != nil {
		c.carried = false
	}
	zero := 0.0
	if metrics != nil {
		applyMetrics(local, s, metrics)
		rate := func(name string) *float64 {
			v := 0.0
			if c.lastM != nil && dt > 0 {
				v = max(0, (metrics[name]-c.lastM[name])/dt)
			}
			return &v
		}
		local.TokensPerS = rate("llamacpp:tokens_predicted_total")
		local.PromptTokensPerS = rate("llamacpp:prompt_tokens_total")
	}
	if slots == nil {
		return
	}
	wasBusy := c.busy()
	prompt, tokens, busy := c.slotRates(slots, dt)
	switch {
	case len(busy) > 0:
		local.PromptTokensPerS, local.TokensPerS = prompt, tokens
	case wasBusy || metrics == nil:
		// The requests just finished: their tokens were counted live as they ran, and the counters now
		// jump by the whole requests at once, which would read as a burst.
		local.TokensPerS, local.PromptTokensPerS = &zero, &zero
	}
	if metrics == nil {
		rp := int32(len(busy))
		local.RequestsProcessing = &rp
	}
	selected := slots[0]
	for _, slot := range busy {
		if !selected.Processing || slot.Task < selected.Task {
			selected = slot
		}
	}
	nPrompt := selected.NPrompt
	if c.Log != nil {
		if request, ok := c.Log.Request(selected.ID); ok && request.Task == selected.Task {
			nPrompt = request.NPrompt
		}
	}
	local.Slot = &pb.Slot{
		Processing: selected.Processing, NPrompt: int32(nPrompt), NCached: int32(selected.NCached),
		NProcessed: int32(selected.NProcessed), NDecoded: int32(selected.NDecoded),
	}
}

// slotRates sums the live rates of the busy slots. A phase no busy slot is in is a known 0; a phase
// with a slot whose rate cannot be stated yet is unknown.
func (c *Collector) slotRates(slots []llamaserver.Slot, dt float64) (*float64, *float64, []llamaserver.Slot) {
	var prompt, tokens float64
	promptCount, promptKnown, tokenCount, tokenKnown := 0, 0, 0, 0
	var busy []llamaserver.Slot
	next := map[int]slotState{}
	for _, slot := range slots {
		state := c.lastSlots[slot.ID]
		if !slot.Processing {
			next[slot.ID] = slotState{slot: slot}
			continue
		}
		busy = append(busy, slot)
		request, fromLog := loadlog.Request{}, false
		if c.Log != nil {
			request, fromLog = c.Log.Request(slot.ID)
			fromLog = fromLog && request.Task == slot.Task
		}
		// llamacpp's counters advance when a request completes; while one is in flight the log or slot
		// progress is the live rate.
		if slot.NDecoded == 0 && (!fromLog || request.NGenerated == 0) {
			promptCount++
			state = promptState(state, slot, dt)
			rate := state.promptRate
			// The log prints a chunk's line seconds after /slots shows it, and nothing before the first
			// chunk ends; until then the slot's own steps give the rate.
			if fromLog && request.PromptTokensPerS > 0 {
				rate = request.PromptTokensPerS
			}
			if rate > 0 {
				prompt += rate
				promptKnown++
			}
		} else {
			tokenCount++
			state.promptElapsed, state.promptRate = 0, 0
			last := state.slot
			switch {
			case fromLog && request.NGenerated > 0:
				tokens += request.TokensPerS
				tokenKnown++
			case last.Processing && last.Task == slot.Task && dt > 0:
				tokens += float64(max(0, slot.NDecoded-last.NDecoded)) / dt
				tokenKnown++
			}
		}
		state.slot = slot
		next[slot.ID] = state
	}
	c.lastSlots = next
	return known(prompt, promptKnown, promptCount), known(tokens, tokenKnown, tokenCount), busy
}

func known(sum float64, stated, count int) *float64 {
	if stated < count {
		return nil
	}
	return &sum
}

func (c *Collector) busy() bool {
	for _, st := range c.lastSlots {
		if st.slot.Processing {
			return true
		}
	}
	return false
}

func (c *Collector) restoreRates(local *pb.Node, s *pb.Snapshot) {
	if c.last == nil {
		return
	}
	for _, n := range c.last.Nodes {
		if n.Id == "local" {
			local.TokensPerS, local.PromptTokensPerS = n.TokensPerS, n.PromptTokensPerS
			local.RequestsProcessing, local.RequestsQueued, local.Slot = n.RequestsProcessing, n.RequestsQueued, n.Slot
		}
	}
	s.Totals.TokensPredicted = c.last.Totals.TokensPredicted
}

func applyMetrics(local *pb.Node, s *pb.Snapshot, metrics map[string]float64) {
	if processing, ok := metrics["llamacpp:requests_processing"]; ok {
		rp := int32(processing)
		local.RequestsProcessing = &rp
	}
	if queued, ok := metrics["llamacpp:requests_deferred"]; ok {
		queued := int32(queued)
		local.RequestsQueued = &queued
	}
	s.Totals.TokensPredicted = metrics["llamacpp:tokens_predicted_total"]
}

// promptState updates a slot's prompt rate from /slots: prefill advances in ubatch chunks, many polls
// apart, so the rate is the tokens of a step over the time since the previous step, held until the next.
func promptState(state slotState, slot llamaserver.Slot, dt float64) slotState {
	last := state.slot
	if !last.Processing || last.Task != slot.Task || last.NDecoded != 0 {
		state.promptElapsed, state.promptRate = 0, 0
		return state
	}
	state.promptElapsed += dt
	if slot.NProcessed != last.NProcessed {
		state.promptRate = float64(slot.NProcessed-last.NProcessed) / state.promptElapsed
		state.promptElapsed = 0
	}
	return state
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
