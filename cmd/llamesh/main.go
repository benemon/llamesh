// llamesh: a live picture of a llama.cpp mesh. Run it beside a llama-server; it discovers the rest.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
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
	target := flag.String("target", "", "llama-server base URL; discovered when exactly one chat server is listening")
	flag.Parse()

	pid, port, err := pickTarget(*target)
	if err != nil {
		log.Fatal(err)
	}
	cmdline, err := discover.CommandLine(pid)
	if err != nil {
		log.Fatal(err)
	}
	args := discover.ParseArgs(cmdline)
	logPath, err := discover.LogPath(pid)
	if err != nil {
		log.Printf("no log to read the memory split from: %v", err)
	}
	col := &snapshot.Collector{
		Client: llamaserver.New("http://127.0.0.1:"+strconv.Itoa(port), args.APIKey),
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
	if p, err := col.Client.Props(); err == nil {
		col.SetProps(p)
	} else {
		log.Printf("props: %v", err)
	}
	refreshTopology(col, args)
	log.Printf("target llama-server pid %d port %d, %d rpc node(s), log %s", pid, port, len(args.RPC), logPath)

	b := &broadcaster{subs: map[chan []byte]struct{}{}}
	go func() {
		t := time.NewTicker(*poll)
		n := 0
		for range t.C {
			n++
			if n%10 == 0 {
				if cl, err := discover.CommandLine(pid); err == nil {
					if a := discover.ParseArgs(cl); strings.Join(a.RPC, ",") != strings.Join(col.Args.RPC, ",") {
						col.Args = a
						refreshTopology(col, a)
					}
				}
			}
			s := col.Poll()
			if buf, err := json.Marshal(s); err == nil {
				b.publish(buf)
			}
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/topology", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(col.Topology())
	})
	mux.HandleFunc("/api/stream", b.serve)
	mux.Handle("/", web.Handler())
	log.Printf("listening on http://%s", *listen)
	log.Fatal(http.ListenAndServe(*listen, mux))
}

// pickTarget chooses the llama-server: the -target port if given, else the one listener that is not
// an embedding server.
func pickTarget(target string) (pid, port int, err error) {
	ls, err := discover.Listeners()
	if err != nil {
		return 0, 0, err
	}
	if target != "" {
		i := strings.LastIndexByte(target, ':')
		p, perr := strconv.Atoi(strings.TrimRight(target[i+1:], "/"))
		if i < 0 || perr != nil {
			return 0, 0, fmt.Errorf("-target %q: need a URL ending in :PORT", target)
		}
		for _, l := range ls {
			if l.Port == p {
				return l.PID, l.Port, nil
			}
		}
		return 0, 0, fmt.Errorf("no llama-server listening on port %d", p)
	}
	var chat []discover.Listener
	for _, l := range ls {
		cl, err := discover.CommandLine(l.PID)
		if err != nil || discover.ParseArgs(cl).Embeddings {
			continue
		}
		chat = append(chat, l)
	}
	switch len(chat) {
	case 1:
		return chat[0].PID, chat[0].Port, nil
	case 0:
		return 0, 0, fmt.Errorf("no llama-server is listening (embedding servers excluded); use -target")
	}
	var ports []string
	for _, l := range chat {
		ports = append(ports, strconv.Itoa(l.Port))
	}
	return 0, 0, fmt.Errorf("%d llama-servers listening on ports %s; choose one with -target", len(chat), strings.Join(ports, ", "))
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
