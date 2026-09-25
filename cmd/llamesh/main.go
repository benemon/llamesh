// llamesh: a live picture of a llama.cpp mesh. Run it on a host with llama-servers; it discovers the rest.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/benemon/llamesh/internal/discover"
	"github.com/benemon/llamesh/internal/link"
	"github.com/benemon/llamesh/internal/llamaserver"
	"github.com/benemon/llamesh/internal/loadlog"
	"github.com/benemon/llamesh/internal/snapshot"
	"github.com/benemon/llamesh/internal/web"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:8899", "address to serve the page and API on")
	poll := flag.Duration("poll", time.Second, "how often the live sources are read")
	spec := flag.String("target", "", "llama-server base URL; every llama-server on the host is watched when unset")
	sourcesPath := flag.String("sources", "", "YAML file listing the other collectors this page composes (sources: [http://host:port, ...])")
	flag.Parse()

	local := discover.LocalHostName()
	var sources []source
	if *sourcesPath != "" {
		var err error
		if sources, err = readSources(*sourcesPath); err != nil {
			log.Fatal(err)
		}
		log.Printf("%d source(s) from %s", len(sources), *sourcesPath)
	}

	var cur atomic.Pointer[[]*target]
	empty := []*target{}
	cur.Store(&empty)
	rescan := func() {
		ls, err := listeners(*spec)
		if err != nil {
			log.Printf("listeners: %v", err)
			return
		}
		next := []*target{}
		for _, t := range *cur.Load() {
			// A target is one process: when it exits (a model swap, a crash) its picture goes with it.
			if cl, err := discover.CommandLine(t.pid); err != nil {
				log.Printf("target llama-server pid %d (port %d) has gone", t.pid, t.port)
				continue
			} else if a := discover.ParseArgs(cl); strings.Join(a.RPC, ",") != strings.Join(t.col.Args.RPC, ",") {
				t.col.Args = a
				refreshTopology(t.col, a)
			}
			next = append(next, t)
		}
		for _, l := range ls {
			known := false
			for _, t := range next {
				known = known || t.pid == l.PID
			}
			if known {
				continue
			}
			if t, err := bind(l); err == nil {
				next = append(next, t)
			} else {
				log.Printf("bind pid %d port %d: %v", l.PID, l.Port, err)
			}
		}
		if len(next) == 0 && len(*cur.Load()) > 0 {
			log.Printf("no llama-server is listening; waiting for one")
		}
		cur.Store(&next)
	}
	rescan()
	if len(*cur.Load()) == 0 {
		log.Printf("no llama-server is listening; waiting for one")
	}

	b := &broadcaster{subs: map[chan []byte]struct{}{}}
	go func() {
		tk := time.NewTicker(*poll)
		n := 0
		for range tk.C {
			n++
			if n%10 == 0 {
				rescan()
			}
			ts := *cur.Load()
			if len(ts) == 0 {
				s := snapshot.Snapshot{T: float64(time.Now().UnixNano()) / 1e9, Source: local, Nodes: []snapshot.Node{}, Links: []snapshot.Link{}}
				if buf, err := json.Marshal(s); err == nil {
					b.publish(buf)
				}
				continue
			}
			for _, t := range ts {
				s := t.col.Poll()
				s.Target = strconv.Itoa(t.port)
				if buf, err := json.Marshal(s); err == nil {
					b.publish(buf)
				}
			}
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/topology", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		out := []map[string]any{}
		for _, t := range *cur.Load() {
			top := t.col.Topology()
			out = append(out, map[string]any{"target": strconv.Itoa(t.port), "nodes": top.Nodes, "links": top.Links})
		}
		json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("/api/stream", b.serve)
	mux.HandleFunc("/api/sources", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		keys := []string{}
		for _, src := range sources {
			keys = append(keys, src.key)
		}
		json.NewEncoder(w).Encode(keys)
	})
	// /api/sources/<key>/stream and /topology are the other collectors' endpoints, proxied so the browser
	// talks to one origin: the sources sit on addresses it cannot reach (a Thunderbolt bridge).
	mux.HandleFunc("/api/sources/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/api/sources/")
		key, ep, ok := strings.Cut(rest, "/")
		for _, src := range sources {
			if ok && src.key == key && (ep == "stream" || ep == "topology") {
				src.proxy.ServeHTTP(w, r)
				return
			}
		}
		http.NotFound(w, r)
	})
	mux.Handle("/", web.Handler())
	log.Printf("listening on http://%s", *listen)
	log.Fatal(http.ListenAndServe(*listen, mux))
}

// target is one llama-server being watched and the collector built from its command line and log.
type target struct {
	pid, port int
	col       *snapshot.Collector
}

// listeners is every llama-server listening on the host, or the one -target names.
func listeners(spec string) ([]discover.Listener, error) {
	ls, err := discover.Listeners()
	if err != nil {
		return nil, err
	}
	if spec == "" {
		return ls, nil
	}
	i := strings.LastIndexByte(spec, ':')
	p, perr := strconv.Atoi(strings.TrimRight(spec[i+1:], "/"))
	if i < 0 || perr != nil {
		return nil, fmt.Errorf("-target %q: need a URL ending in :PORT", spec)
	}
	var out []discover.Listener
	for _, l := range ls {
		if l.Port == p {
			out = append(out, l)
		}
	}
	return out, nil
}

func bind(l discover.Listener) (*target, error) {
	cmdline, err := discover.CommandLine(l.PID)
	if err != nil {
		return nil, err
	}
	args := discover.ParseArgs(cmdline)
	logPath, err := discover.LogPath(l.PID)
	if err != nil {
		log.Printf("no log to read the memory split from: %v", err)
	}
	col := &snapshot.Collector{
		Client: llamaserver.New("http://"+args.Host+":"+strconv.Itoa(l.Port), args.APIKey),
		Args:   args,
		Local:  discover.LocalHostName(),
		Names:  map[string]string{},
		Ifaces: map[string]string{},
	}
	if logPath != "" {
		if col.Log, err = loadlog.Open(logPath); err != nil {
			log.Printf("log %s: %v", logPath, err)
		}
	}
	refreshTopology(col, args)
	log.Printf("target llama-server pid %d port %d, %d rpc node(s), log %s", l.PID, l.Port, len(args.RPC), logPath)
	return &target{pid: l.PID, port: l.Port, col: col}, nil
}

func refreshTopology(col *snapshot.Collector, args discover.Args) {
	var ips []string
	for _, addr := range args.RPC {
		host := addr[:strings.LastIndexByte(addr, ':')]
		ips = append(ips, host)
		if iface, err := link.Interface(host); err == nil {
			col.Ifaces[addr] = iface
		} else {
			log.Printf("route to %s: %v", host, err)
		}
	}
	for ip, name := range discover.Names(ips, 2*time.Second) {
		col.Names[ip] = name
	}
}

// source is another collector whose stream this page composes; its key is the URL's host:port.
type source struct {
	key   string
	proxy *httputil.ReverseProxy
}

// readSources parses the sources file: a YAML mapping with one key, `sources`, a list of URLs. The file
// is short enough that a line scanner does, and the module stays on the standard library.
func readSources(path string) ([]source, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []source
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "- ") {
			continue
		}
		raw := strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "- ")), "\"'")
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return nil, fmt.Errorf("%s: %q is not a URL", path, raw)
		}
		key := u.Host
		p := httputil.NewSingleHostReverseProxy(u)
		p.FlushInterval = -1 // the stream is SSE: every event goes out as it arrives
		dir := p.Director
		p.Director = func(r *http.Request) {
			dir(r)
			r.URL.Path = "/api/" + strings.TrimPrefix(r.URL.Path, "/api/sources/"+key+"/")
			r.Host = u.Host
		}
		out = append(out, source{key: key, proxy: p})
	}
	return out, sc.Err()
}

// broadcaster fans one JSON snapshot per poll out to every SSE client and remembers the last one so a
// new client gets a picture immediately.
type broadcaster struct {
	mu   sync.Mutex
	subs map[chan []byte]struct{}
	last []byte
}

func (b *broadcaster) publish(buf []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.last = buf
	for ch := range b.subs {
		select {
		case ch <- buf:
		default: // a client that is not reading drops this frame rather than stalling the poll
		}
	}
}

func (b *broadcaster) serve(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	ch := make(chan []byte, 4)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	last := b.last
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.subs, ch)
		b.mu.Unlock()
	}()
	if last != nil {
		fmt.Fprintf(w, "data: %s\n\n", last)
		fl.Flush()
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case buf := <-ch:
			if _, err := fmt.Fprintf(w, "data: %s\n\n", buf); err != nil {
				return
			}
			fl.Flush()
		}
	}
}
