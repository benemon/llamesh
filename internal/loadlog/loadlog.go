// Package loadlog reads the per-device memory split from a llama-server log written at -lv 4: the
// memory breakdown table after load. The last complete table wins; a load prints one before allocation
// and one after.
package loadlog

import (
	"bufio"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
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

type Split struct {
	Devices []Device
	Info    Structure
}

// Structure is what print_info states about the model at load, as far as the page draws it.
type Structure struct {
	NLayer     int `json:"n_layer,omitempty"`
	NExpert    int `json:"n_expert,omitempty"`
	NExpertUse int `json:"n_expert_used,omitempty"`
}

var (
	tableHead = regexp.MustCompile(`common_memory_breakdown_print: \| memory breakdown \[MiB\]`)
	tableRow  = regexp.MustCompile(`common_memory_breakdown_print: \|\s+- (\w+)(?: \(([^)]*)\))?\s+\| (\d+) = (\d+) \+ \(\s*(\d+) =\s*(\d+) \+\s*(\d+) \+\s*(\d+)\)`)
	infoLine  = regexp.MustCompile(`print_info: (\S+(?: \S+)?)\s+= (.+)$`)
)

// Parse reads every table in text and returns the state after the last one.
func Parse(text string) Split {
	var s Split
	var current []Device
	inTable := false
	for _, line := range strings.Split(text, "\n") {
		if tableHead.MatchString(line) {
			current = nil
			inTable = true
			continue
		}
		if m := tableRow.FindStringSubmatch(line); m != nil && inTable {
			mib := func(i int) int64 { v, _ := strconv.ParseInt(m[i], 10, 64); return v * MiB }
			current = append(current, Device{Name: m[1], Endpoint: m[2], Total: mib(3), Model: mib(6), Context: mib(7), Compute: mib(8)})
			s.Devices = current
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
	path   string
	offset int64
	buf    strings.Builder
	split  Split
}

// startMarker is the first line a llama-server run writes at -lv 4; the current load's facts follow it.
const startMarker = "common_params_print_info: verbosity"

// Open parses the current load's lines, from the last server start to the end, then follows from the end.
// The structure lines come early in a load and the memory tables late, with thousands of Metal
// kernel-compile lines between, so the window is found by searching backwards for the start marker.
func Open(path string) (*Follower, error) {
	f := &Follower{path: path}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
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
	f.split = Parse(string(b))
	f.offset = st.Size()
	return f, nil
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
	defer file.Close()
	st, err := file.Stat()
	if err != nil {
		return f.split
	}
	if st.Size() < f.offset {
		f.offset = 0
		f.buf.Reset()
	}
	if _, err := file.Seek(f.offset, io.SeekStart); err != nil {
		return f.split
	}
	r := bufio.NewReader(file)
	for {
		line, err := r.ReadString('\n')
		f.buf.WriteString(line)
		f.offset += int64(len(line))
		if err != nil {
			break
		}
	}
	if f.buf.Len() > 0 && strings.Contains(f.buf.String(), "common_memory_breakdown_print") {
		text := f.buf.String()
		if !strings.HasSuffix(text, "\n") { // a table cut mid-line waits for the rest
			return f.split
		}
		if n := Parse(text); len(n.Devices) > 0 {
			f.split = n
			f.buf.Reset()
		}
	} else if f.buf.Len() > 4*MiB {
		f.buf.Reset()
	}
	return f.split
}
