package mlxserver

import (
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("../../testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func openText(t *testing.T, text string, pid int) (*Follower, string) {
	t.Helper()
	path := t.TempDir() + "/server.err"
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := OpenLog(path, pid)
	if err != nil {
		t.Fatal(err)
	}
	return f, path
}

func ids(rs []Request) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.ID)
	}
	return out
}

// The stderr file holds llama-server's exit memory table and an earlier mlx server's unfinished request
// before the target's start line; none of it is the target's.
func TestFollowerStartsAtTargetProcess(t *testing.T) {
	text := fixture(t, "stderr-switch.log")
	cut := strings.Index(text, "2026-09-30 09:34:33,797")
	f, _ := openText(t, text[:cut], 85726)
	if got := ids(f.Requests()); len(got) != 1 || got[0] != "113cab100" {
		t.Fatalf("open requests %v, want the target's 113cab100 only", got)
	}
	f, _ = openText(t, text[:cut], 62280)
	if got := ids(f.Requests()); len(got) != 2 || got[0] != "1122d7e10" {
		t.Fatalf("pid 62280: open requests %v", got)
	}
}

func TestFollowerWithoutTargetStartFollowsFromTheEnd(t *testing.T) {
	text := fixture(t, "stderr-switch.log")
	cut := strings.Index(text, "2026-09-30 09:34:32,054")
	next := cut + strings.IndexByte(text[cut:], '\n') + 1
	f, path := openText(t, text[:cut], 4711)
	if got := f.Requests(); len(got) != 0 {
		t.Fatalf("replayed earlier servers' requests: %v", ids(got))
	}
	if err := os.WriteFile(path, []byte(text[:next]), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := ids(f.Requests()); len(got) != 1 || got[0] != "113cab100" {
		t.Fatalf("new line not followed: %v", got)
	}
}

var (
	prefillStart = regexp.MustCompile(`Prefill started: request=(\S+)`)
	prefillStep  = regexp.MustCompile(`Prefill progress: request=(\S+) tokens=(\d+)/`)
	prefillDone  = regexp.MustCompile(`Prefill completed: request=(\S+) prompt_tokens=(\d+) cached_tokens=(\d+) elapsed=([\d.]+)s`)
	queued       = regexp.MustCompile(`Generation queued: request=(\S+)`)
	decodeStart  = regexp.MustCompile(`Decode started: request=(\S+)`)
	decodeStep   = regexp.MustCompile(`Decode progress: request=(\S+)`)
	decodeDone   = regexp.MustCompile(`Decode completed: request=(\S+) generated_tokens=\d+ elapsed=([\d.]+)s`)
	requestDone  = regexp.MustCompile(`Request completed: .* decode=([\d.]+) tok/s`)
)

// held is a live figure and when it was set.
type held struct {
	at   time.Time
	rate float64
}

type oracle struct {
	prompt, decode float64 // the server's end-of-request figures
	decodeElapsed  float64
	started        time.Time
	first          held // the first progress line, whose rate is known only at Prefill completed
	firstN         int
	prompts        []held
	decodeStarted  time.Time
	generations    []held
}

// replay feeds a fixture to a follower line by line, keeping each request's live rates as the collector
// would read them and its end-of-request figures from the server's own lines.
func replay(t *testing.T, name string, each func(f *Follower, at time.Time)) map[string]*oracle {
	t.Helper()
	f := &Follower{requests: map[string]Request{}}
	out := map[string]*oracle{}
	get := func(id string) *oracle {
		if out[id] == nil {
			out[id] = &oracle{}
		}
		return out[id]
	}
	lastDecoded, reused := "", 0
	for _, line := range strings.Split(fixture(t, name), "\n") {
		f.applyLine(line)
		if len(line) < 23 {
			continue
		}
		at, err := time.Parse("2006-01-02 15:04:05,000", line[:23])
		if err != nil {
			continue
		}
		// mlx-vlm reuses request ids: a request queued under an id already seen is a new one.
		if m := queued.FindStringSubmatch(line); m != nil && out[m[1]] != nil {
			reused++
			out[m[1]+"#"+strconv.Itoa(reused)] = out[m[1]]
			delete(out, m[1])
		}
		if m := prefillStart.FindStringSubmatch(line); m != nil {
			get(m[1]).started = at
		}
		if m := prefillStep.FindStringSubmatch(line); m != nil {
			o, r := get(m[1]), f.requests[m[1]]
			if o.first.at.IsZero() {
				o.first.at, o.firstN = at, r.NProcessed
			} else if r.PromptTokensPerS != nil {
				o.prompts = append(o.prompts, held{at, *r.PromptTokensPerS})
			}
		}
		if m := prefillDone.FindStringSubmatch(line); m != nil {
			p, _ := strconv.Atoi(m[2])
			c, _ := strconv.Atoi(m[3])
			e, _ := strconv.ParseFloat(m[4], 64)
			o := get(m[1])
			o.prompt = float64(p-c) / e
			o.first.rate = float64(o.firstN-c) / o.first.at.Sub(o.started).Seconds()
		}
		if m := decodeStart.FindStringSubmatch(line); m != nil {
			get(m[1]).decodeStarted = at
		}
		if m := decodeStep.FindStringSubmatch(line); m != nil {
			if r := f.requests[m[1]]; r.TokensPerS != nil {
				get(m[1]).generations = append(get(m[1]).generations, held{at, *r.TokensPerS})
			}
		}
		if m := decodeDone.FindStringSubmatch(line); m != nil {
			lastDecoded = m[1]
			get(m[1]).decodeElapsed, _ = strconv.ParseFloat(m[2], 64)
		}
		if m := requestDone.FindStringSubmatch(line); m != nil && lastDecoded != "" {
			get(lastDecoded).decode, _ = strconv.ParseFloat(m[1], 64)
			lastDecoded = ""
		}
		if each != nil {
			each(f, at)
		}
	}
	return out
}

// weighted is the mean of rates each set at the end of the interval it measures, the first interval
// starting at from, weighted by interval.
func weighted(hs []held, from time.Time) float64 {
	var sum, span float64
	for _, h := range hs {
		d := h.at.Sub(from).Seconds()
		sum += h.rate * d
		span += d
		from = h.at
	}
	return sum / span
}

func relativeError(got, want float64) float64 {
	return math.Abs(got-want) / want
}

// The oracle is the server's own end-of-request figures: (prompt - cached) over prefill's elapsed, and
// Request completed's decode rate. Each rate is weighted by the interval it measures. The first prefill
// line's rate, from Prefill started less the cached tokens, counts once prefill completes. A decode
// shorter than 10 s holds too few 3 s windows to average to the request's rate.
func TestLiveRatesMatchTheServersEndOfRequestFigures(t *testing.T) {
	for _, name := range []string{"mlx-requests.log", "mlx-concurrent.log"} {
		t.Run(name, func(t *testing.T) {
			prompts, decodes := 0, 0
			for id, o := range replay(t, name, nil) {
				if len(o.prompts) > 0 && o.prompt > 0 {
					prompts++
					got := weighted(append([]held{o.first}, o.prompts...), o.started)
					t.Logf("%s prompt %.2f / server %.2f (%.2f%%)", id, got, o.prompt, relativeError(got, o.prompt)*100)
					if relativeError(got, o.prompt) > 0.02 {
						t.Errorf("%s prompt rate %.2f, server %.2f", id, got, o.prompt)
					}
				}
				if len(o.generations) > 0 && o.decode > 0 && o.decodeElapsed >= 10 {
					decodes++
					got := weighted(o.generations, o.decodeStarted)
					t.Logf("%s decode %.2f / server %.2f (%.2f%%)", id, got, o.decode, relativeError(got, o.decode)*100)
					if relativeError(got, o.decode) > 0.05 {
						t.Errorf("%s generation rate %.2f, server %.2f", id, got, o.decode)
					}
				}
			}
			if prompts == 0 || decodes == 0 {
				t.Fatalf("checked %d prompts, %d decodes", prompts, decodes)
			}
		})
	}
}

// Two requests prefill as one batch: their summed rate is the tokens of both over the shared prefill time.
func TestConcurrentPromptRatesSum(t *testing.T) {
	var sums []held
	o := replay(t, "mlx-concurrent.log", func(f *Follower, at time.Time) {
		sum, known := 0.0, 0
		for _, r := range f.requests {
			if !r.Decoding && r.PromptTokensPerS != nil {
				sum += *r.PromptTokensPerS
				known++
			}
		}
		if known == 2 && (len(sums) == 0 || sums[len(sums)-1].rate != sum) {
			sums = append(sums, held{at, sum})
		}
	})
	a, b := o["11245da30"], o["768e78d9a0"]
	if len(sums) == 0 || a == nil || b == nil {
		t.Fatalf("no summed rate: %v", sums)
	}
	got := weighted(append([]held{{a.first.at, a.first.rate + b.first.rate}}, sums...), a.started)
	want := float64(4178+4177) / 66.572
	t.Logf("summed prompt %.2f / server %.2f (%.2f%%)", got, want, relativeError(got, want)*100)
	if relativeError(got, want) > 0.02 {
		t.Fatalf("summed prompt rate %.2f, want %.2f", got, want)
	}
}

func TestLoggedRatesAreNotUsedForLiveFigures(t *testing.T) {
	var peak, promptPeak float64
	o := replay(t, "mlx-requests.log", func(f *Follower, _ time.Time) {
		for _, r := range f.requests {
			if r.TokensPerS != nil {
				peak = max(peak, *r.TokensPerS)
			}
			if r.PromptTokensPerS != nil {
				promptPeak = max(promptPeak, *r.PromptTokensPerS)
			}
		}
	})
	if peak == 0 || peak > 100 {
		t.Fatalf("peak generation rate %.1f: the log's own rate= was used", peak)
	}
	// Prefill completed reports 347 to 456 tok/s for the cached turns, counting the cached tokens.
	if promptPeak == 0 || promptPeak > 200 {
		t.Fatalf("peak prompt rate %.1f: Prefill completed's rate= was used", promptPeak)
	}
	// 768e78dbe0 had 8,190 of 12,485 tokens cached; its first progress line, 10238/12485, counts them.
	r := o["768e78dbe0"]
	if r == nil || len(r.prompts) == 0 || !r.prompts[0].at.After(r.first.at) || weighted(r.prompts, r.first.at) > 130 {
		t.Fatalf("cached request prompt rates %+v", r)
	}
}
