package mlxserver

import (
	"os"
	"path/filepath"
	"testing"
)

type sizedInfo struct {
	os.FileInfo
	size int64
}

func (s sizedInfo) Size() int64 { return s.size }

func TestLoadModelSnapshot(t *testing.T) {
	sizes := map[string]int64{
		"model-00001-of-00006.safetensors": 5317707581,
		"model-00002-of-00006.safetensors": 5354102610,
		"model-00003-of-00006.safetensors": 5354184694,
		"model-00004-of-00006.safetensors": 5337309653,
		"model-00005-of-00006.safetensors": 5292848464,
		"model-00006-of-00006.safetensors": 2845065477,
		"model.safetensors":                451270785,
	}
	stat := func(path string) (os.FileInfo, error) {
		info, err := os.Stat(path)
		return sizedInfo{FileInfo: info, size: sizes[filepath.Base(path)]}, err
	}
	m, err := LoadModel("../../testdata/huggingface", "mlx-community/Qwen3.8-27B-8bit", "mlx-community/Qwen3.8-27B-MTP-8bit", stat)
	if err != nil {
		t.Fatal(err)
	}
	if m.Bytes != 29501218479+451270785 || m.NLayer != 64 || m.NExpert != 0 || m.NExpertUsed != 0 {
		t.Fatalf("model %+v", m)
	}
	if filepath.Base(m.Path) != "815b83c0df8ffd1d1b5244cf75fd6ef14fca9ef9" {
		t.Fatalf("path %q", m.Path)
	}
}
