// Package loadlog reads the per-device memory split from a llama-server log written at -lv 4: the
// memory breakdown table after load. The last complete table wins; a GPU load prints one before
// allocation and one after, a CPU-only load one.
package loadlog

import (
	"bufio"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const MiB = 1048576

type Device struct {
	Name     string // MTL0, RPC0, RPC1, ...
	Endpoint string // host:port for RPC devices, the Metal device name for the local one
	Total    int64  // bytes
	Model    int64
	Context  int64
	Compute  int64
}

// Split is the last table: Devices in llama.cpp's device order, which is the order layers are assigned in,
// and Host, the rows of host-memory buffers (Host, CPU_REPACK), which carry no device total.
type Split struct {
	Devices []Device
	Host    []Device
	Info    Structure
}

// Source is where a Split comes from: a log file, or the systemd journal.
type Source interface {
	Latest() Split
	Request(slot int) (Request, bool)
}

type Request struct {
	Task             int
	NPrompt          int
	NProcessed       int
	NGenerated       int
	PromptTokensPerS float64
	TokensPerS       float64
	progressT        float64
}

// Structure is what print_info states about the model at load, as far as the page draws it.
type Structure struct {
	NLayer     int `json:"n_layer,omitempty"`
	NExpert    int `json:"n_expert,omitempty"`
	NExpertUse int `json:"n_expert_used,omitempty"`
}

var (
	tableHead = regexp.MustCompile(`common_memory_breakdown_print: \| memory breakdown \[MiB\]`)
	hostRow   = regexp.MustCompile(`common_memory_breakdown_print: \|\s+- (\w+)\s+\|\s+(\d+) =\s+(\d+) \+\s+(\d+) \+\s+(\d+)\s+\|`)
	tableRow  = regexp.MustCompile(`common_memory_breakdown_print: \|\s+- (\w+)(?: \(([^)]*)\))?\s+\| ` +
		`(\d+) = (\d+) \+ \(\s*(\d+) =\s*(\d+) \+\s*(\d+) \+\s*(\d+)\)`)
	infoLine  = regexp.MustCompile(`print_info: (\S+(?: \S+)?)\s+= (.+)$`)
	newPrompt = regexp.MustCompile(`slot\s+operator\(\): id\s+(\d+) \| task\s+(-?\d+) \| new prompt,.*task\.n_tokens =\s+(\d+)`)
	progress  = regexp.MustCompile(`slot print_timing: id\s+(\d+) \| task\s+(-?\d+) \| prompt processing, ` +
		`n_tokens =\s+(\d+),.*t =\s+([\d.]+) s /\s+([\d.]+) tokens per second`)
	generation = regexp.MustCompile(`slot print_timing: id\s+(\d+) \| task\s+(-?\d+) \| n_gen =\s+(\d+),.*tg_3s =\s+([\d.]+) t/s`)
	release    = regexp.MustCompile(`slot\s+release: id\s+(\d+) \| task\s+(-?\d+) \| stop processing:`)
)

// Parse reads every table in text and returns the state after the last one.
func Parse(text string) Split {
	var s Split
	inTable := false
	for _, line := range strings.Split(text, "\n") {
		if tableHead.MatchString(line) {
			s.Devices, s.Host = nil, nil
			inTable = true
			continue
		}
		if m := tableRow.FindStringSubmatch(line); m != nil && inTable {
			mib := func(i int) int64 { v, _ := strconv.ParseInt(m[i], 10, 64); return v * MiB }
			s.Devices = append(s.Devices, Device{Name: m[1], Endpoint: m[2], Total: mib(3), Model: mib(6), Context: mib(7), Compute: mib(8)})
			continue
		}
		if m := hostRow.FindStringSubmatch(line); m != nil && inTable {
			mib := func(i int) int64 { v, _ := strconv.ParseInt(m[i], 10, 64); return v * MiB }
			s.Host = append(s.Host, Device{Name: m[1], Model: mib(3), Context: mib(4), Compute: mib(5)})
			continue
		}
		if inTable && !strings.Contains(line, "common_memory_breakdown_print") {
			inTable = false
		}
		if m := infoLine.FindStringSubmatch(line); m != nil {
			s.Info.set(strings.TrimSpace(m[1]), strings.TrimSpace(m[2]))
		}
	}
	return s
}

func (st *Structure) set(key, val string) {
	n, _ := strconv.Atoi(val)
	switch key {
	case "n_layer":
		st.NLayer = n
	case "n_expert":
		st.NExpert = n
	case "n_expert_used":
		st.NExpertUse = n
	}
}

// LayerRanges derives which layers each device holds. llama.cpp assigns repeating layers contiguously in
// device order in proportion to bytes, so the range follows from each device's share of the model
// bytes; the log at -lv 4 prints no per-layer assignment.
func LayerRanges(devs []Device, nLayer int) [][2]int {
	var total int64
	for _, d := range devs {
		total += d.Model
	}
	out := make([][2]int, len(devs))
	if total == 0 || nLayer == 0 {
		return out
	}
	next := 0
	for i, d := range devs {
		n := int(float64(nLayer)*float64(d.Model)/float64(total) + 0.5)
		if i == len(devs)-1 || next+n > nLayer {
			n = nLayer - next
		}
		out[i] = [2]int{next, next + n - 1}
		next += n
	}
	return out
}

// Follower tails a log written by a running llama-server.
type Follower struct {
	path     string
	offset   int64
	buf      strings.Builder
	line     strings.Builder
	split    Split
	requests map[int]Request
}

// startMarker is the first line a llama-server run writes at -lv 4; the current load's facts follow it.
const startMarker = "common_params_print_info: verbosity"

// Open parses the current load's lines, from the last server start to the end, then follows from the end.
// The structure lines come early in a load and the memory tables late, with thousands of Metal
// kernel-compile lines between, so the window is found by searching backwards for the start marker.
func Open(path string) (*Follower, error) {
	f := &Follower{path: path, requests: map[int]Request{}}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }() // opened for reading
	st, err := file.Stat()
	if err != nil {
		return nil, err
	}
	start := lastMarker(file, st.Size())
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return nil, err
	}
	b, err := io.ReadAll(file)
	if err != nil {
		return nil, err
	}
	text := string(b)
	f.split = Parse(text)
	f.apply(text)
	f.offset = st.Size()
	return f, nil
}

func (f *Follower) apply(text string) {
	for {
		i := strings.IndexByte(text, '\n')
		if i < 0 {
			break
		}
		line := text[:i]
		if f.line.Len() > 0 {
			f.line.WriteString(line)
			line = f.line.String()
			f.line.Reset()
		}
		f.applyLine(line)
		text = text[i+1:]
	}
	f.line.WriteString(text)
	if f.line.Len() > 4*MiB {
		f.line.Reset()
	}
}

func (f *Follower) applyLine(line string) {
	if !strings.Contains(line, "| task ") {
		return
	}
	atoi := func(s string) int { n, _ := strconv.Atoi(s); return n }
	atof := func(s string) float64 { n, _ := strconv.ParseFloat(s, 64); return n }
	if m := newPrompt.FindStringSubmatch(line); m != nil {
		f.requests[atoi(m[1])] = Request{Task: atoi(m[2]), NPrompt: atoi(m[3])}
		return
	}
	if m := progress.FindStringSubmatch(line); m != nil {
		slot, task := atoi(m[1]), atoi(m[2])
		r, ok := f.requests[slot]
		if !ok || r.Task != task {
			return
		}
		n, elapsed, cumulative := atoi(m[3]), atof(m[4]), atof(m[5])
		rate := cumulative
		if r.progressT > 0 && elapsed > r.progressT {
			rate = float64(n-r.NProcessed) / (elapsed - r.progressT)
		}
		r.NProcessed, r.PromptTokensPerS, r.progressT = n, rate, elapsed
		f.requests[slot] = r
		return
	}
	if m := generation.FindStringSubmatch(line); m != nil {
		slot, task := atoi(m[1]), atoi(m[2])
		r, ok := f.requests[slot]
		if !ok || r.Task != task {
			return
		}
		r.NGenerated, r.TokensPerS = atoi(m[3]), atof(m[4])
		f.requests[slot] = r
		return
	}
	if m := release.FindStringSubmatch(line); m != nil {
		slot, task := atoi(m[1]), atoi(m[2])
		if r, ok := f.requests[slot]; ok && r.Task == task {
			delete(f.requests, slot)
		}
	}
}

// lastMarker returns the offset of the last start marker within the final 512 MiB, else the start of that
// window.
func lastMarker(file *os.File, size int64) int64 {
	const chunk = 4 * MiB
	limit := size - 512*MiB
	if limit < 0 {
		limit = 0
	}
	buf := make([]byte, chunk+256)
	for end := size; end > limit; {
		start := end - chunk
		if start < limit {
			start = limit
		}
		n, err := file.ReadAt(buf[:end-start], start)
		if err != nil && err != io.EOF {
			return limit
		}
		if i := strings.LastIndex(string(buf[:n]), startMarker); i >= 0 {
			j := strings.LastIndexByte(string(buf[:i]), '\n')
			return start + int64(j+1)
		}
		end = start + 256 // overlap so a marker split across chunks is still found
		if start == limit {
			break
		}
	}
	return limit
}

func (f *Follower) Latest() Split {
	file, err := os.Open(f.path)
	if err != nil {
		return f.split
	}
	defer func() { _ = file.Close() }() // opened for reading
	st, err := file.Stat()
	if err != nil {
		return f.split
	}
	if st.Size() < f.offset {
		f.offset = 0
		f.buf.Reset()
		f.line.Reset()
		clear(f.requests)
	}
	if _, err := file.Seek(f.offset, io.SeekStart); err != nil {
		return f.split
	}
	r := bufio.NewReader(file)
	var added strings.Builder
	for {
		line, err := r.ReadString('\n')
		f.buf.WriteString(line)
		added.WriteString(line)
		f.offset += int64(len(line))
		if err != nil {
			break
		}
	}
	f.apply(added.String())
	if f.buf.Len() > 0 && strings.Contains(f.buf.String(), "common_memory_breakdown_print") {
		text := f.buf.String()
		if !strings.HasSuffix(text, "\n") { // a table cut mid-line waits for the rest
			return f.split
		}
		if n := Parse(text); len(n.Devices)+len(n.Host) > 0 {
			f.split = n
			f.buf.Reset()
		}
	} else if f.buf.Len() > 4*MiB {
		f.buf.Reset()
	}
	return f.split
}

func (f *Follower) Request(slot int) (Request, bool) {
	r, ok := f.requests[slot]
	return r, ok
}

// Journal reads a server's table from the systemd journal, where a service's stderr goes when it is not a
// file. The table changes only at load, and a new load is a new process, so the journal is read at most
// every 10 s.
type Journal struct {
	pid   int
	read  time.Time
	split Split
}

func OpenJournal(pid int) (*Journal, error) {
	if _, err := exec.LookPath("journalctl"); err != nil {
		return nil, err
	}
	return &Journal{pid: pid}, nil
}

func (j *Journal) Latest() Split {
	if time.Since(j.read) < 10*time.Second {
		return j.split
	}
	j.read = time.Now()
	out, err := exec.Command("journalctl", "_PID="+strconv.Itoa(j.pid), "-o", "cat", "--no-pager").Output()
	if err != nil {
		return j.split
	}
	if n := Parse(string(out)); len(n.Devices)+len(n.Host) > 0 {
		j.split = n
	}
	return j.split
}

func (j *Journal) Request(int) (Request, bool) { return Request{}, false }
