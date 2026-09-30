package discover

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseArgsRPC(t *testing.T) {
	b, _ := os.ReadFile("../../testdata/ps-command-rpc.txt")
	a := ParseArgs(string(b))
	if len(a.RPC) != 1 || a.RPC[0] != "10.0.0.2:50052" {
		t.Fatalf("rpc: %v", a.RPC)
	}
	if a.Host != "127.0.0.1" || a.APIKey != "REDACTED" {
		t.Fatalf("got %+v", a)
	}
}

func TestParseArgsNoRPC(t *testing.T) {
	b, _ := os.ReadFile("../../testdata/ps-command.txt")
	a := ParseArgs(string(b))
	if len(a.RPC) != 0 || a.Host != "127.0.0.1" {
		t.Fatalf("got %+v", a)
	}
}

func TestParseListeners(t *testing.T) {
	b, _ := os.ReadFile("../../testdata/lsof-listen-mlx.txt")
	commands := map[int]string{
		85726: "/opt/homebrew/Python -m mlx_vlm.server --model mlx-community/Qwen3.8-27B-8bit",
		1743:  "/Users/benjaminholmes/.unsloth/studio/unsloth_studio/bin/python /Users/benjaminholmes/.unsloth/studio/unsloth_studio/bin/unsloth studio",
		2210:  "/opt/homebrew/Python -m mlx_vlm.chat_ui --model mlx-community/Qwen3.8-27B-8bit",
	}
	ls := parseListeners(string(b), func(pid int) (string, error) { return commands[pid], nil })
	if len(ls) != 2 || ls[0] != (Listener{PID: 85726, Port: 8896, Engine: EngineMLX}) || ls[1] != (Listener{PID: 573, Port: 8891, Engine: EngineLlama}) {
		t.Fatalf("got %+v", ls)
	}
}

func TestParseMLXArgs(t *testing.T) {
	b, _ := os.ReadFile("../../testdata/ps-command-mlx.txt")
	a := ParseArgs(string(b))
	if a.Host != "127.0.0.1" || a.APIKey != "<redacted>" || a.Model != "mlx-community/Qwen3.8-27B-8bit" || a.Draft != "mlx-community/Qwen3.8-27B-MTP-8bit" {
		t.Fatalf("got %+v", a)
	}
}

func TestMLXBuildFromMappedPackage(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "site-packages", "mlx_vlm-0.7.4.dist-info"), 0o755); err != nil {
		t.Fatal(err)
	}
	out := "n" + filepath.Join(dir, "site-packages", "mlx", "core.cpython-314-darwin.so") + "\n"
	if got := mlxBuild(out, filepath.Glob); got != "mlx-vlm 0.7.4" {
		t.Fatalf("build %q", got)
	}
}

func TestSRVLine(t *testing.T) {
	line := "vega._companion-link._tcp  SRV     0 0 49443 vega.local. ; Replace with unicast FQDN of target host\n"
	m := srvLine.FindStringSubmatch(line)
	if m == nil || m[1] != "vega.local" {
		t.Fatalf("got %v", m)
	}
}

func TestLinuxLookups(t *testing.T) {
	if got := parseMeminfo("MemTotal:       16318036 kB\nMemFree:         1234 kB\n"); got != 16318036*1024 {
		t.Fatalf("meminfo %d", got)
	}
	if got := secondField("10.0.0.2\tvega.local\n"); got != "vega.local" {
		t.Fatalf("avahi %q", got)
	}
	if got := secondField("10.0.0.2        vega.example vega\n"); got != "vega.example" {
		t.Fatalf("getent %q", got)
	}
}

func TestWiredLimit(t *testing.T) {
	b, _ := os.ReadFile("../../testdata/sysctl-wired-limit.txt")
	if got := parseWiredLimit(string(b)); got != 59392*1048576 {
		t.Fatalf("wired limit %d", got)
	}
}
