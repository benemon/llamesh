// Package llamaserver reads a llama-server's HTTP API: /props, /metrics, /slots.
package llamaserver

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type Client struct {
	Base string
	Key  string
	http *http.Client
}

func New(base, key string) *Client {
	return &Client{Base: strings.TrimRight(base, "/"), Key: key, http: &http.Client{Timeout: 5 * time.Second}}
}

func (c *Client) get(path string) ([]byte, error) {
	req, err := http.NewRequest("GET", c.Base+path, nil)
	if err != nil {
		return nil, err
	}
	if c.Key != "" {
		req.Header.Set("Authorization", "Bearer "+c.Key)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("%s: %s", path, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

type Props struct {
	Build     string
	ModelPath string
	NCtx      int
}

func (c *Client) Props() (Props, error) {
	b, err := c.get("/props")
	if err != nil {
		return Props{}, err
	}
	return ParseProps(b)
}

func ParseProps(b []byte) (Props, error) {
	var raw struct {
		Build     string `json:"build_info"`
		ModelPath string `json:"model_path"`
		Gen       struct {
			NCtx int `json:"n_ctx"`
		} `json:"default_generation_settings"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return Props{}, err
	}
	return Props{Build: raw.Build, ModelPath: raw.ModelPath, NCtx: raw.Gen.NCtx}, nil
}

// Metrics returns every llamacpp:* sample by bare name; labels are dropped.
func (c *Client) Metrics() (map[string]float64, error) {
	b, err := c.get("/metrics")
	if err != nil {
		return nil, err
	}
	return ParseMetrics(string(b)), nil
}

func ParseMetrics(text string) map[string]float64 {
	m := map[string]float64{}
	for _, line := range strings.Split(text, "\n") {
		if line == "" || line[0] == '#' {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		name := fields[0]
		if i := strings.IndexByte(name, '{'); i >= 0 {
			name = name[:i]
		}
		v, err := strconv.ParseFloat(fields[len(fields)-1], 64)
		if err != nil {
			continue
		}
		m[name] = v
	}
	return m
}

type Slot struct {
	Processing bool `json:"processing"`
	NPrompt    int  `json:"n_prompt"`
	NCached    int  `json:"n_cached"`
	NProcessed int  `json:"n_processed"`
	NDecoded   int  `json:"n_decoded"` // tokens generated so far in the request in flight
}

func (c *Client) Slot() (Slot, error) {
	b, err := c.get("/slots")
	if err != nil {
		return Slot{}, err
	}
	return ParseSlots(b)
}

// ParseSlots reads the first slot; the collector serves single-slot servers.
func ParseSlots(b []byte) (Slot, error) {
	var raw []struct {
		Processing bool            `json:"is_processing"`
		NPrompt    int             `json:"n_prompt_tokens"`
		NCached    int             `json:"n_prompt_tokens_cache"`
		NProcessed int             `json:"n_prompt_tokens_processed"`
		NextToken  json.RawMessage `json:"next_token"` // a list of one object in b10566; an object in other builds
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return Slot{}, err
	}
	if len(raw) == 0 {
		return Slot{}, fmt.Errorf("no slots")
	}
	s := raw[0]
	slot := Slot{Processing: s.Processing, NPrompt: s.NPrompt, NCached: s.NCached, NProcessed: s.NProcessed}
	type nt struct {
		NDecoded int `json:"n_decoded"`
	}
	var list []nt
	var one nt
	if json.Unmarshal(s.NextToken, &list) == nil && len(list) > 0 {
		slot.NDecoded = list[0].NDecoded
	} else if json.Unmarshal(s.NextToken, &one) == nil {
		slot.NDecoded = one.NDecoded
	}
	return slot, nil
}
