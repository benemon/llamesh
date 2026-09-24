// Package loadlog reads the per-device memory split from a llama-server log written at -lv 4: the
// memory breakdown table after load, and the model and KV buffer lines. The last complete table wins;
// a load prints one before allocation and one after.
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
	Free     int64
	Model    int64
	Context  int64
	Compute  int64
}

type Split struct {
	Devices []Device
	// Buffers by device name, bytes, from the load_tensors / llama_kv_cache lines; secondary to the table.
	ModelBuf map[string]int64
	KVBuf    map[string]int64
	Info     Structure
}

// Structure is what print_info states about the model at load.
type Structure struct {
	Arch       string `json:"arch,omitempty"`
	Name       string `json:"name,omitempty"`
	Type       string `json:"type,omitempty"`
	Params     string `json:"params,omitempty"`
	NLayer     int    `json:"n_layer,omitempty"`
	NEmbd      int    `json:"n_embd,omitempty"`
	NHead      int    `json:"n_head,omitempty"`
	NHeadKV    int    `json:"n_head_kv,omitempty"`
	NExpert    int    `json:"n_expert,omitempty"`
	NExpertUse int    `json:"n_expert_used,omitempty"`
	NVocab     int    `json:"n_vocab,omitempty"`
	NCtxTrain  int    `json:"n_ctx_train,omitempty"`
}

var (
	tableHead = regexp.MustCompile(`common_memory_breakdown_print: \| memory breakdown \[MiB\]`)
	tableRow  = regexp.MustCompile(`common_memory_breakdown_print: \|\s+- (\w+)(?: \(([^)]*)\))?\s+\| (\d+) = (\d+) \+ \(\s*(\d+) =\s*(\d+) \+\s*(\d+) \+\s*(\d+)\)`)
	infoLine  = regexp.MustCompile(`print_info: (\S+(?: \S+)?)\s+= (.+)$`)
	modelBuf  = regexp.MustCompile(`(\w+?)(?:_Mapped)?(?:\[[^\]]*\])? model buffer size =\s+([\d.]+) MiB`)
	kvBuf     = regexp.MustCompile(`llama_kv_cache:\s+(\w+)(?:\[[^\]]*\])? KV buffer size =\s+([\d.]+) MiB`)
)

// Parse reads every table and buffer line in text and returns the state after the last table.
func Parse(text string) Split {
	s := Split{ModelBuf: map[string]int64{}, KVBuf: map[string]int64{}}
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
			current = append(current, Device{Name: m[1], Endpoint: m[2], Total: mib(3), Free: mib(4), Model: mib(6), Context: mib(7), Compute: mib(8)})
			s.Devices = current
			continue
		}
		if inTable && !strings.Contains(line, "common_memory_breakdown_print") {
			inTable = false
		}
		if m := modelBuf.FindStringSubmatch(line); m != nil {
			s.ModelBuf[m[1]] += mibf(m[2])
		} else if m := kvBuf.FindStringSubmatch(line); m != nil {
			s.KVBuf[m[1]] += mibf(m[2])
		} else if m := infoLine.FindStringSubmatch(line); m != nil {
			s.Info.set(strings.TrimSpace(m[1]), strings.TrimSpace(m[2]))
		}
	}
	return s
}

func (st *Structure) set(key, val string) {
	n, _ := strconv.Atoi(val)
	switch key {
	case "arch":
		st.Arch = val
	case "general.name":
		st.Name = val
	case "model type":
		st.Type = val
	case "model params":
		st.Params = val
	case "n_layer":
		st.NLayer = n
	case "n_embd":
		st.NEmbd = n
	case "n_head":
		st.NHead = n
	case "n_head_kv":
		st.NHeadKV = n
	case "n_expert":
		st.NExpert = n
	case "n_expert_used":
		st.NExpertUse = n
	case "n_vocab":
		st.NVocab = n
	case "n_ctx_train":
		st.NCtxTrain = n
	}
}

// LayerRanges derives which layers each device holds. llama.cpp assigns repeating layers contiguously in
// device order in proportion to bytes, so the range follows from each device's share of the model
// bytes; the log at -lv 4 prints no per-layer assignment. The last device also holds the output layer.
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

func mibf(v string) int64 {
	f, _ := strconv.ParseFloat(v, 64)
	return int64(f * MiB)
}

// Follower tails a log: Latest returns the split from the last table seen so far, reading new bytes
// on each call and starting over if the file was truncated or replaced.
type Follower struct {
	path   string
	offset int64
	buf    strings.Builder
	split  Split
}

// Open scans the tail of the file for the current split, then follows from the end.
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
	start := st.Size() - 2*MiB
	if start < 0 {
		start = 0
	}
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
