package discover

import (
	"os"
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
	out := "p573\ncllama-ser\nn127.0.0.1:8891\np72342\ncllama-ser\nn127.0.0.1:8894\np9\nchaproxy\nn*:8443\n"
	ls := parseListeners(out)
	if len(ls) != 2 || ls[0].PID != 573 || ls[0].Port != 8891 || ls[1].Port != 8894 {
		t.Fatalf("got %+v", ls)
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
