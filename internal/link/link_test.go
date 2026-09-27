package link

import (
	"os"
	"testing"
)

func TestParseRoute(t *testing.T) {
	b, _ := os.ReadFile("../../testdata/route-get.txt")
	iface, err := parseRoute(string(b))
	if err != nil || iface != "bridge0" {
		t.Fatalf("got %q, %v", iface, err)
	}
}

func TestParseNetstat(t *testing.T) {
	b, _ := os.ReadFile("../../testdata/netstat-ibn.txt")
	c, err := parseNetstat(string(b), "bridge0")
	if err != nil {
		t.Fatal(err)
	}
	if c.In != 95569386 || c.Out != 13303794448 {
		t.Fatalf("columns swapped or misread: %+v", c)
	}
	if _, err := parseNetstat(string(b), "nosuch0"); err == nil {
		t.Fatal("missing interface must error")
	}
}

// Recorded on an Ubuntu runner by the end to end suite.
func TestParseLinux(t *testing.T) {
	route, _ := os.ReadFile("../../testdata/ip-route-get.txt")
	if iface, err := parseIPRoute(string(route)); err != nil || iface != "lo" {
		t.Fatalf("ip route: %q %v", iface, err)
	}
	dev, _ := os.ReadFile("../../testdata/proc-net-dev.txt")
	c, err := parseProcNetDev(string(dev), "eth0")
	if err != nil || c.In != 182159543 || c.Out != 1850523 {
		t.Fatalf("/proc/net/dev: %+v %v", c, err)
	}
}
