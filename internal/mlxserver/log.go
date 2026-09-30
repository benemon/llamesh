package mlxserver

import (
	"bufio"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Request is one request the log shows open, with its live figures; a rate is nil until it can be stated.
type Request struct {
	ID               string
	NPrompt          int
	NCached          int
	NProcessed       int
	NGenerated       int
	Decoding         bool
	PromptTokensPerS *float64
	TokensPerS       *float64
	opened           int
	progressAt       time.Time
	decode           []sample
}

type sample struct {
	at time.Time
	n  int
}

// Follower tails an mlx_vlm.server log from the target process's start.
type Follower struct {
	path     string
	offset   int64
	line     strings.Builder
	requests map[string]Request
	opened   int
}

var (
	queuedLine          = regexp.MustCompile(`Generation queued: request=(\S+) prompt_tokens=(\d+)`)
	prefillProgressLine = regexp.MustCompile(`Prefill progress: request=(\S+) tokens=(\d+)/(\d+)`)
	// The line's own rate counts tokens served from the cache, so it is not read.
	prefillCompletedLine = regexp.MustCompile(`Prefill completed: request=(\S+) prompt_tokens=(\d+) cached_tokens=(\d+)`)
	// The line's own rate alternates between true and absurd figures with MTP, so it is not read.
	decodeProgressLine  = regexp.MustCompile(`Decode progress: request=(\S+) generated_tokens=(\d+)`)
	decodeCompletedLine = regexp.MustCompile(`Decode completed: request=(\S+)`)
)

// OpenLog follows path from the last start of the server with this pid. The file is the LaunchAgent's
// stderr, shared with every earlier server on the same roster entry; without the pid's start line
// nothing before the end of the file is known to be this server's.
func OpenLog(path string, pid int) (*Follower, error) {
	f := &Follower{path: path, requests: map[string]Request{}}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }() // opened for reading
	st, err := file.Stat()
	if err != nil {
		return nil, err
	}
	start, ok := lastMarker(file, st.Size(), "INFO:     Started server process ["+strconv.Itoa(pid)+"]")
	if !ok {
		f.offset = st.Size()
		return f, nil
	}
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return nil, err
	}
	b, err := io.ReadAll(file)
	if err != nil {
		return nil, err
	}
	f.apply(string(b))
	f.offset = st.Size()
	return f, nil
}

// Requests reads what the log added and returns the open requests, oldest first.
func (f *Follower) Requests() []Request {
	f.follow()
	out := make([]Request, 0, len(f.requests))
	for _, r := range f.requests {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].opened < out[j].opened })
	return out
}

// Idle drops every open request: the server reported none in flight after the log was read, so any
// still open ended without a completion line.
func (f *Follower) Idle() {
	clear(f.requests)
}

func (f *Follower) follow() {
	file, err := os.Open(f.path)
	if err != nil {
		return
	}
	defer func() { _ = file.Close() }()
	st, err := file.Stat()
	if err != nil {
		return
	}
	if st.Size() < f.offset {
		f.offset = 0
		f.line.Reset()
		clear(f.requests)
	}
	if _, err := file.Seek(f.offset, io.SeekStart); err != nil {
		return
	}
	r := bufio.NewReader(file)
	var added strings.Builder
	for {
		line, err := r.ReadString('\n')
		added.WriteString(line)
		f.offset += int64(len(line))
		if err != nil {
			break
		}
	}
	f.apply(added.String())
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
}

func (f *Follower) applyLine(line string) {
	if len(line) < 23 {
		return
	}
	// Local wall clock to the millisecond; only differences are used.
	at, err := time.Parse("2006-01-02 15:04:05,000", line[:23])
	if err != nil {
		return
	}
	atoi := func(s string) int { n, _ := strconv.Atoi(s); return n }
	if m := queuedLine.FindStringSubmatch(line); m != nil {
		f.opened++
		f.requests[m[1]] = Request{ID: m[1], NPrompt: atoi(m[2]), opened: f.opened}
		return
	}
	if m := prefillProgressLine.FindStringSubmatch(line); m != nil {
		r, ok := f.requests[m[1]]
		if !ok {
			return
		}
		// The first line's tokens include those served from the cache, known only once prefill completes,
		// so a rate starts from the second line.
		n := atoi(m[2])
		if !r.progressAt.IsZero() && at.After(r.progressAt) {
			rate := float64(n-r.NProcessed) / at.Sub(r.progressAt).Seconds()
			r.PromptTokensPerS = &rate
		}
		r.NProcessed, r.progressAt = n, at
		f.requests[m[1]] = r
		return
	}
	if m := prefillCompletedLine.FindStringSubmatch(line); m != nil {
		r, ok := f.requests[m[1]]
		if !ok {
			return
		}
		r.NPrompt, r.NCached, r.NProcessed, r.Decoding, r.PromptTokensPerS = atoi(m[2]), atoi(m[3]), atoi(m[2]), true, nil
		f.requests[m[1]] = r
		return
	}
	if m := decodeProgressLine.FindStringSubmatch(line); m != nil {
		r, ok := f.requests[m[1]]
		if !ok {
			return
		}
		r.NGenerated = atoi(m[2])
		r.decode = append(r.decode, sample{at: at, n: r.NGenerated})
		cut := at.Add(-3 * time.Second)
		for len(r.decode) > 2 && r.decode[1].at.Before(cut) {
			r.decode = r.decode[1:]
		}
		if first := r.decode[0]; len(r.decode) >= 2 && at.After(first.at) {
			rate := float64(r.NGenerated-first.n) / at.Sub(first.at).Seconds()
			r.TokensPerS = &rate
		}
		f.requests[m[1]] = r
		return
	}
	if m := decodeCompletedLine.FindStringSubmatch(line); m != nil {
		delete(f.requests, m[1])
	}
}

// lastMarker returns the offset of the last line holding marker within the final 512 MiB.
func lastMarker(file *os.File, size int64, marker string) (int64, bool) {
	const chunk = int64(4 * 1048576)
	limit := size - 512*1048576
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
			return 0, false
		}
		if i := strings.LastIndex(string(buf[:n]), marker); i >= 0 {
			j := strings.LastIndexByte(string(buf[:i]), '\n')
			return start + int64(j+1), true
		}
		end = start + 256
		if start == limit {
			break
		}
	}
	return 0, false
}
