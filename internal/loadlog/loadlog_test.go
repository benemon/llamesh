package loadlog

import (
	"os"
	"strings"
	"testing"
)

func TestParseFixture(t *testing.T) {
	b, err := os.ReadFile("../../testdata/server-lv4.log")
	if err != nil {
		t.Fatal(err)
	}
	s := Parse(string(b))
	byName := map[string]Device{}
	for _, d := range s.Devices {
		byName[d.Name] = d
	}
	rpc, ok := byName["RPC0"]
	if !ok {
		t.Fatalf("no RPC0 row: %+v", s.Devices)
	}
	if rpc.Endpoint != "10.0.0.2:50052" || rpc.Total != 27264*MiB || rpc.Model != 5921*MiB || rpc.Context != 106*MiB || rpc.Compute != 87*MiB {
		t.Fatalf("RPC0 %+v", rpc)
	}
	if rpc.Free != 21147*MiB {
		t.Fatalf("last table must win (free after allocation 21147), got %d MiB", rpc.Free/MiB)
	}
	mtl := byName["MTL0"]
	if mtl.Total != 59392*MiB || mtl.Model != 11124*MiB || mtl.Context != 103*MiB || mtl.Compute != 87*MiB {
		t.Fatalf("MTL0 %+v", mtl)
	}
	mib := float64(MiB)
	if s.ModelBuf["RPC0"] != int64(5921.53*mib) || s.ModelBuf["MTL0"] != int64(11124.19*mib) {
		t.Fatalf("model buffers %v", s.ModelBuf)
	}
	if s.KVBuf["RPC0"] != int64(96*mib)+int64(10.5*mib) {
		t.Fatalf("kv buffers %v", s.KVBuf)
	}
}

func TestMissingRowIsAbsent(t *testing.T) {
	text := "x common_memory_breakdown_print: | memory breakdown [MiB]     | total    free     self   model   context   compute    unaccounted |\n" +
		"x common_memory_breakdown_print: |   - MTL0 (Apple M4 Pro)    | 59392 = 48076 + (11315 = 11124 +     103 +      87) +           0 |\n"
	s := Parse(text)
	if len(s.Devices) != 1 || s.Devices[0].Name != "MTL0" {
		t.Fatalf("got %+v", s.Devices)
	}
}

func TestStructureAndLayers(t *testing.T) {
	b, _ := os.ReadFile("../../testdata/server-lv4.log")
	s := Parse(string(b))
	if s.Info.NLayer != 24 || s.Info.Arch != "gpt-oss" || s.Info.NExpert != 32 {
		t.Fatalf("structure %+v", s.Info)
	}
	// MTL0 11124 MiB and RPC0 5921 MiB of 24 layers: 16 and 8, contiguous, in device order
	devs := []Device{{Name: "MTL0", Model: 11124 * MiB}, {Name: "RPC0", Model: 5921 * MiB}}
	r := LayerRanges(devs, s.Info.NLayer)
	if r[0] != [2]int{0, 15} || r[1] != [2]int{16, 23} {
		t.Fatalf("ranges %v", r)
	}
}

func TestOpenFindsTheCurrentLoad(t *testing.T) {
	b, _ := os.ReadFile("../../testdata/server-lv4.log")
	// two loads back to back with 3 MB of noise between the second load's start and its tables
	noise := strings.Repeat("ggml_metal_library_compile_pipeline: loaded kernel_x 0x1 | th_max = 1024 | th_width = 32\n", 3*MiB/90)
	text := string(b) + strings.Replace(string(b), "n_layer               = 24", "n_layer               = 24", 1)
	parts := strings.SplitN(string(b), "load_tensors:", 2)
	text = string(b) + parts[0] + noise + "load_tensors:" + parts[1]
	tmp := t.TempDir() + "/server.err"
	if err := os.WriteFile(tmp, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := Open(tmp)
	if err != nil {
		t.Fatal(err)
	}
	sp := f.Latest()
	if sp.Info.NLayer != 24 || len(sp.Devices) == 0 {
		t.Fatalf("structure not read across the noise: n_layer %d, devices %d", sp.Info.NLayer, len(sp.Devices))
	}
}
