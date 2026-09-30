package mlxserver

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

type Model struct {
	Path        string
	Bytes       int64
	NLayer      int
	NExpert     int
	NExpertUsed int
}

func LoadModel(cache, repo, draft string, stat func(string) (os.FileInfo, error)) (Model, error) {
	path, err := snapshotPath(cache, repo)
	if err != nil {
		return Model{}, err
	}
	b, err := os.ReadFile(filepath.Join(path, "config.json"))
	if err != nil {
		return Model{}, err
	}
	var cfg struct {
		Text struct {
			NLayer      int `json:"num_hidden_layers"`
			NExpert     int `json:"num_experts"`
			NExpertUsed int `json:"num_experts_per_tok"`
		} `json:"text_config"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return Model{}, err
	}
	m := Model{Path: path, NLayer: cfg.Text.NLayer, NExpert: cfg.Text.NExpert, NExpertUsed: cfg.Text.NExpertUsed}
	for _, repo := range []string{repo, draft} {
		if repo == "" {
			continue
		}
		dir, err := snapshotPath(cache, repo)
		if err != nil {
			return Model{}, err
		}
		files, err := filepath.Glob(filepath.Join(dir, "*.safetensors"))
		if err != nil {
			return Model{}, err
		}
		for _, file := range files {
			info, err := stat(file)
			if err != nil {
				return Model{}, err
			}
			m.Bytes += info.Size()
		}
	}
	return m, nil
}

func snapshotPath(cache, repo string) (string, error) {
	root := filepath.Join(cache, "models--"+strings.ReplaceAll(repo, "/", "--"))
	b, err := os.ReadFile(filepath.Join(root, "refs", "main"))
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "snapshots", strings.TrimSpace(string(b))), nil
}
