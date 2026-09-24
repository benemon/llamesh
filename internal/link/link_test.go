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
