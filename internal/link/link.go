// Package link reads the interface a host is reached through and that interface's byte counters: the
// routing table and netstat on macOS, `ip route` and /proc/net/dev on Linux. A dedicated link, such as a
// Thunderbolt bridge, carries nothing but RPC traffic, so its counters are the link's flow.
package link

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

func Interface(host string) (string, error) {
	if runtime.GOOS == "linux" {
		out, err := exec.Command("ip", "route", "get", host).Output()
		if err != nil {
			return "", fmt.Errorf("ip route: %w", err)
		}
		return parseIPRoute(string(out))
	}
	out, err := exec.Command("route", "-n", "get", host).Output()
	if err != nil {
		return "", fmt.Errorf("route: %w", err)
	}
	return parseRoute(string(out))
}

// parseIPRoute takes the word after "dev" in `ip route get` output.
func parseIPRoute(out string) (string, error) {
	f := strings.Fields(out)
	for i := 0; i+1 < len(f); i++ {
		if f[i] == "dev" {
			return f[i+1], nil
		}
	}
	return "", fmt.Errorf("no dev in ip route output")
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
	if runtime.GOOS == "linux" {
		b, err := os.ReadFile("/proc/net/dev")
		if err != nil {
			return Counters{}, err
		}
		return parseProcNetDev(string(b), iface)
	}
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

// parseProcNetDev takes the interface's row: after "iface:", field 1 is receive bytes and field 9 is
// transmit bytes.
func parseProcNetDev(out, iface string) (Counters, error) {
	for _, line := range strings.Split(out, "\n") {
		name, rest, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(name) != iface {
			continue
		}
		f := strings.Fields(rest)
		if len(f) < 9 {
			break
		}
		in, err1 := strconv.ParseInt(f[0], 10, 64)
		outb, err2 := strconv.ParseInt(f[8], 10, 64)
		if err1 != nil || err2 != nil {
			return Counters{}, fmt.Errorf("/proc/net/dev row for %s: %q", iface, line)
		}
		return Counters{In: in, Out: outb}, nil
	}
	return Counters{}, fmt.Errorf("no /proc/net/dev row for %s", iface)
}
