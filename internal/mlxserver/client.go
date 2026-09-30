// Package mlxserver reads an mlx_vlm.server process, HTTP API, log and model snapshot.
package mlxserver

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type Client struct {
	base string
	key  string
	http *http.Client
}

func New(base, key string) *Client {
	return &Client{base: strings.TrimRight(base, "/"), key: key, http: &http.Client{Timeout: 5 * time.Second}}
}

func (c *Client) get(path string, dst any) error {
	req, err := http.NewRequest("GET", c.base+path, nil)
	if err != nil {
		return err
	}
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", path, resp.Status)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, dst)
}

type Health struct {
	LoadedModel           string `json:"loaded_model"`
	EffectiveContextLimit int    `json:"effective_context_limit"`
	APCEnabled            bool   `json:"apc_enabled"`
}

func (c *Client) Health() (Health, error) {
	var v Health
	err := c.get("/health", &v)
	return v, err
}

type Metrics struct {
	Summary struct {
		InFlight             int     `json:"in_flight"`
		GeneratedTokensTotal float64 `json:"generated_tokens_total"`
	} `json:"summary"`
	Server struct {
		RequestQueueDepth int `json:"request_queue_depth"`
	} `json:"server"`
}

func (c *Client) Metrics() (Metrics, error) {
	var v Metrics
	err := c.get("/metrics", &v)
	return v, err
}
