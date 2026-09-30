package snapshot

import (
	"time"

	"github.com/benemon/llamesh/internal/discover"
	"github.com/benemon/llamesh/internal/mlxserver"
	pb "github.com/benemon/llamesh/internal/pb/llamesh/v1"
)

// MLXCollector holds one discovered mlx-vlm target and its load-time facts.
type MLXCollector struct {
	Client   *mlxserver.Client
	Log      *mlxserver.Follower
	Model    mlxserver.Model
	Build    string
	Local    string
	MemTotal int64
	health   mlxserver.Health
}

// Poll reads the mlx-vlm live sources once and returns the snapshot. The log is read before /metrics, so
// a request the log shows open while the server reports none in flight has ended.
func (c *MLXCollector) Poll() *pb.Snapshot {
	health, herr := c.Client.Health()
	if herr == nil {
		c.health = health
	}
	var requests []mlxserver.Request
	if c.Log != nil {
		requests = c.Log.Requests()
	}
	metrics, merr := c.Client.Metrics()
	if merr == nil && metrics.Summary.InFlight == 0 && c.Log != nil {
		c.Log.Idle()
		requests = nil
	}
	return c.build(requests, metrics, merr == nil, herr != nil)
}

func (c *MLXCollector) build(requests []mlxserver.Request, metrics mlxserver.Metrics, haveMetrics, stale bool) *pb.Snapshot {
	now := time.Now()
	s := &pb.Snapshot{
		T:      float64(now.UnixNano()) / 1e9,
		Source: c.Local,
		Model: &pb.Model{
			Path:   c.Model.Path,
			Name:   c.health.LoadedModel,
			NCtx:   int32(c.health.EffectiveContextLimit),
			Build:  c.Build,
			Engine: discover.EngineMLX,
			Structure: &pb.Structure{
				NLayer:      int32(c.Model.NLayer),
				NExpert:     int32(c.Model.NExpert),
				NExpertUsed: int32(c.Model.NExpertUsed),
			},
		},
		Totals: &pb.Totals{},
	}
	n := &pb.Node{Id: "local", Kind: pb.Kind_KIND_LLAMA_SERVER, Device: "MTL0", Label: c.Local, Stale: stale}
	if c.MemTotal > 0 {
		n.MemTotal = number(c.MemTotal)
	}
	if c.Model.Bytes > 0 {
		n.MemModel = number(c.Model.Bytes)
	}
	if c.Model.NLayer > 0 {
		n.Layers = &pb.Layers{First: 0, Last: int32(c.Model.NLayer - 1)}
	}
	if haveMetrics {
		processing, queued := int32(metrics.Summary.InFlight), int32(metrics.Server.RequestQueueDepth)
		n.RequestsProcessing, n.RequestsQueued = &processing, &queued
		s.Totals.TokensPredicted = metrics.Summary.GeneratedTokensTotal
	}
	var prompt, tokens float64
	promptCount, promptKnown, tokenCount, tokenKnown := 0, 0, 0, 0
	for _, request := range requests {
		if !request.Decoding {
			promptCount++
			if request.PromptTokensPerS != nil {
				prompt += *request.PromptTokensPerS
				promptKnown++
			}
		} else {
			tokenCount++
			if request.TokensPerS != nil {
				tokens += *request.TokensPerS
				tokenKnown++
			}
		}
	}
	// With the log followed, nothing in a phase is a known 0; a phase with a request whose rate is not yet
	// known is unknown.
	if c.Log != nil && (haveMetrics || len(requests) > 0) {
		if promptKnown == promptCount {
			n.PromptTokensPerS = &prompt
		}
		if tokenKnown == tokenCount {
			n.TokensPerS = &tokens
		}
	}
	if len(requests) > 0 {
		r := requests[0]
		n.Slot = &pb.Slot{
			Processing: true, NPrompt: int32(r.NPrompt), NCached: int32(r.NCached),
			NProcessed: int32(r.NProcessed), NDecoded: int32(r.NGenerated),
		}
	}
	s.Nodes = []*pb.Node{n}
	return s
}
