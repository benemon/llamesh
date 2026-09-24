// Package link reads the interface a host is reached through and that interface's byte counters,
// from the local routing table and netstat. A Thunderbolt bridge carries nothing but RPC traffic, so
// its counters are the link's flow.
package link

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

func Interface(host string) (string, error) {
	out, err := exec.Command("route", "-n", "get", host).Output()
	if err != nil {
		return "", fmt.Errorf("route: %w", err)
	}
	return parseRoute(string(out))
}

func parseRoute(out string) (string, error) {
	for _, line := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "interface: "); ok {
			return strings.TrimSpace(v), nil
		}
	}
	return "", fmt.Errorf("no interface in route output")
}

type Counters struct {
	In, Out int64
}

func Read(iface string) (Counters, error) {
	out, err := exec.Command("netstat", "-ibn").Output()
	if err != nil {
		return Counters{}, fmt.Errorf("netstat: %w", err)
	}
	return parseNetstat(string(out), iface)
}

// parseNetstat takes the interface's <Link#> row: column 7 is ibytes and column 10 is obytes.
func parseNetstat(out, iface string) (Counters, error) {
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 10 || f[0] != iface || !strings.HasPrefix(f[2], "<Link#") {
			continue
		}
		in, err1 := strconv.ParseInt(f[6], 10, 64)
		outb, err2 := strconv.ParseInt(f[9], 10, 64)
		if err1 != nil || err2 != nil {
			return Counters{}, fmt.Errorf("netstat row for %s: %q", iface, line)
		}
		return Counters{In: in, Out: outb}, nil
	}
	return Counters{}, fmt.Errorf("no Link row for %s", iface)
}
