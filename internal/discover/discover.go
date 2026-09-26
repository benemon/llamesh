// Package discover reads the llama-server's process, arguments, log and node names from the host.
package discover

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Listener struct {
	PID  int
	Port int
}

// Listeners returns the llama-server processes listening on TCP. lsof truncates the command name to
// nine characters, so it is matched by prefix.
func Listeners() ([]Listener, error) {
	out, err := exec.Command("lsof", "-nP", "-iTCP", "-sTCP:LISTEN", "-F", "pcn").Output()
	if err != nil {
		return nil, fmt.Errorf("lsof: %w", err)
	}
	return parseListeners(string(out)), nil
}

func parseListeners(out string) []Listener {
	var res []Listener
	var pid int
	var cmd string
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		switch line[0] {
		case 'p':
			pid, _ = strconv.Atoi(line[1:])
			cmd = ""
		case 'c':
			cmd = line[1:]
		case 'n':
			if !strings.HasPrefix(cmd, "llama-ser") {
				continue
			}
			i := strings.LastIndexByte(line, ':')
			if i < 0 {
				continue
			}
			port, err := strconv.Atoi(line[i+1:])
			if err != nil {
				continue
			}
			res = append(res, Listener{PID: pid, Port: port})
		}
	}
	return res
}

func CommandLine(pid int) (string, error) {
	out, err := exec.Command("ps", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return "", fmt.Errorf("ps: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

type Args struct {
	Host   string // the address the server binds; loopback unless --host says otherwise
	APIKey string
	RPC    []string
}

// ParseArgs reads the llama-server flags the collector needs from a ps command line. Values are single
// argv tokens; ps joins argv with spaces, and the paths in use carry none.
func ParseArgs(cmdline string) Args {
	tok := strings.Fields(cmdline)
	a := Args{Host: "127.0.0.1"}
	next := func(i int) string {
		if i+1 < len(tok) {
			return tok[i+1]
		}
		return ""
	}
	for i, t := range tok {
		switch t {
		case "--host":
			a.Host = next(i)
		case "--api-key":
			a.APIKey = next(i)
		case "--rpc":
			for _, s := range strings.Split(next(i), ",") {
				if s = strings.TrimSpace(s); s != "" {
					a.RPC = append(a.RPC, s)
				}
			}
		}
	}
	return a
}

// LogPath is the file the process writes stderr to: the LaunchAgent's StandardErrorPath.
func LogPath(pid int) (string, error) {
	out, err := exec.Command("lsof", "-p", strconv.Itoa(pid), "-a", "-d", "2", "-F", "n").Output()
	if err != nil {
		return "", fmt.Errorf("lsof: %w", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "n/") {
			return line[1:], nil
		}
	}
	return "", fmt.Errorf("pid %d: stderr is not a file", pid)
}

func LocalHostName() string {
	out, err := exec.Command("scutil", "--get", "LocalHostName").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

var srvLine = regexp.MustCompile(`\sSRV\s+\d+\s+\d+\s+\d+\s+(\S+?)\.?\s`)

// Names maps IP addresses to the Bonjour hostnames advertising on the local links. dns-sd never exits on
// its own, so each call gets a window and is killed.
func Names(ips []string, window time.Duration) map[string]string {
	want := map[string]bool{}
	for _, ip := range ips {
		want[ip] = true
	}
	names := map[string]string{}
	if len(want) == 0 {
		return names
	}
	types := map[string]bool{"_companion-link._tcp": true, "_workstation._tcp": true}
	for _, line := range strings.Split(dnssd(window, "-B", "_services._dns-sd._udp", "local."), "\n") {
		f := strings.Fields(line)
		// "18:22:16.484  Add  3  1 local.  _tcp.local.  _companion-link" -> type _companion-link._tcp
		if len(f) >= 7 && f[1] == "Add" {
			types[f[6]+"."+strings.TrimSuffix(f[5], ".local.")] = true
		}
	}
	hosts := map[string]bool{}
	for t := range types {
		for _, m := range srvLine.FindAllStringSubmatch(dnssd(window, "-Z", t, "local."), -1) {
			hosts[m[1]] = true
		}
	}
	for h := range hosts {
		for _, ip := range resolve(h) {
			if want[ip] {
				names[ip] = strings.TrimSuffix(strings.TrimSuffix(h, "."), ".local")
			}
		}
	}
	// A node on another subnet advertises nothing here; a reverse lookup (DNS or mDNS) may still name it.
	for ip := range want {
		if _, ok := names[ip]; !ok {
			if h := reverse(ip); h != "" {
				names[ip] = h
			}
		}
	}
	return names
}

func reverse(ip string) string {
	out, err := exec.Command("dscacheutil", "-q", "host", "-a", "ip_address", ip).Output()
	if err != nil {
		return ""
	}
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "name: "); ok {
			return strings.TrimSuffix(strings.TrimSuffix(strings.TrimSpace(v), "."), ".local")
		}
	}
	return ""
}

func dnssd(window time.Duration, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), window)
	defer cancel()
	out, _ := exec.CommandContext(ctx, "dns-sd", args...).Output()
	return string(out)
}

func resolve(host string) []string {
	out, err := exec.Command("dscacheutil", "-q", "host", "-a", "name", host).Output()
	if err != nil {
		return nil
	}
	var ips []string
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "ip_address: "); ok {
			ips = append(ips, strings.TrimSpace(v))
		}
	}
	return ips
}
