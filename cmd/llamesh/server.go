package main

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"

	pb "github.com/benemon/llamesh/internal/pb/llamesh/v1"
	"github.com/benemon/llamesh/internal/web"
)

// The page reads snake_case names, and every non-optional field present so empty lists arrive as [].
var pageJSON = protojson.MarshalOptions{UseProtoNames: true, EmitUnpopulated: true}

// A picture not refreshed for this long is left out of what a newly opened page receives: its collector is
// connected but no longer sending it.
const replayAge = 30 * time.Second

// serve accepts collector streams on ingest and serves the page, and its SSE stream of snapshots, on listen.
func serve(listen, ingest, cert, key, token string, tokenTLS bool) error {
	if (cert == "") != (key == "") {
		return errors.New("-tls-cert and -tls-key go together")
	}
	// Keepalives find a collector that vanished without closing (a sleep, a cable pulled) in about 40 s,
	// where TCP alone takes minutes.
	opts := []grpc.ServerOption{
		grpc.KeepaliveParams(keepalive.ServerParameters{Time: 30 * time.Second, Timeout: 10 * time.Second}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 20 * time.Second, PermitWithoutStream: true}),
	}
	if tokenTLS {
		creds, err := tokenTLSServerCredentials(token)
		if err != nil {
			return err
		}
		opts = append(opts, grpc.Creds(creds))
	} else if cert != "" {
		creds, err := credentials.NewServerTLSFromFile(cert, key)
		if err != nil {
			return err
		}
		opts = append(opts, grpc.Creds(creds))
	}
	if token == "" {
		log.Printf("LLAMESH_TOKEN is not set: any collector that reaches %s is accepted", ingest)
	}
	h := newHub()
	gs := grpc.NewServer(opts...)
	pb.RegisterIngestServiceServer(gs, &ingestServer{hub: h, token: token, tokenTLS: tokenTLS})
	ln, err := net.Listen("tcp", ingest)
	if err != nil {
		return err
	}
	go func() { log.Fatal(gs.Serve(ln)) }()
	log.Printf("accepting collectors on %s", ingest)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/stream", h.serve)
	mux.Handle("/", web.Handler())
	log.Printf("serving the page on http://%s", listen)
	return http.ListenAndServe(listen, mux)
}

type ingestServer struct {
	pb.UnimplementedIngestServiceServer
	hub      *hub
	token    string
	tokenTLS bool
	streams  atomic.Int64
}

// Report is one collector's stream. What it reports belongs to the stream, and is withdrawn when the stream
// ends unless a newer stream has taken it over.
func (s *ingestServer) Report(st grpc.BidiStreamingServer[pb.ReportRequest, pb.ReportResponse]) error {
	from := peerOf(st)
	if s.token != "" && !s.tokenTLS {
		md, _ := metadata.FromIncomingContext(st.Context())
		got := ""
		if v := md.Get("authorization"); len(v) == 1 {
			got = v[0]
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte("Bearer "+s.token)) != 1 {
			log.Printf("collector at %s refused: bad or missing token", from)
			return status.Error(codes.Unauthenticated, "bad or missing token")
		}
	}
	owner := s.streams.Add(1)
	var hello *pb.Hello
	defer func() {
		s.hub.withdraw(owner)
		if hello != nil {
			log.Printf("collector %s (%s) gone", hello.Host, from)
		}
	}()
	for {
		r, err := st.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		switch b := r.Body.(type) {
		case *pb.ReportRequest_Hello:
			if hello != nil {
				return status.Error(codes.InvalidArgument, "one Hello per stream")
			}
			hello = b.Hello
			s.hub.announce(owner, hello)
			log.Printf("collector %s (%s, %s) connected", hello.Host, from, hello.Version)
		case *pb.ReportRequest_Snapshot:
			if hello == nil {
				return status.Error(codes.InvalidArgument, "Hello first")
			}
			b.Snapshot.Source = hello.Host // a picture is keyed by the host that announced itself
			s.hub.publish(owner, b.Snapshot)
		case *pb.ReportRequest_Gone:
			if hello == nil {
				return status.Error(codes.InvalidArgument, "Hello first")
			}
			s.hub.forget(owner, hello.Host+"/"+b.Gone.Target)
		}
	}
}

func peerOf(st grpc.ServerStream) string {
	if p, ok := peer.FromContext(st.Context()); ok {
		return p.Addr.String()
	}
	return "?"
}

// hub fans each snapshot out to every page and keeps the latest per picture, so a page opened later gets
// every picture at once. It also knows every collector's addresses, to name RPC nodes by host.
type hub struct {
	mu     sync.Mutex
	subs   map[chan []byte]struct{}
	latest map[string]picture // source/target
	hosts  map[string]named   // address
}

type picture struct {
	owner int64
	at    time.Time
	json  []byte
}

type named struct {
	owner int64
	host  string
}

func newHub() *hub {
	return &hub{subs: map[chan []byte]struct{}{}, latest: map[string]picture{}, hosts: map[string]named{}}
}

func (h *hub) announce(owner int64, hello *pb.Hello) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, a := range hello.Addresses {
		h.hosts[a] = named{owner, hello.Host}
	}
}

func (h *hub) forget(owner int64, key string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.latest[key].owner == owner {
		delete(h.latest, key)
	}
}

func (h *hub) withdraw(owner int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for k, p := range h.latest {
		if p.owner == owner {
			delete(h.latest, k)
		}
	}
	for a, n := range h.hosts {
		if n.owner == owner {
			delete(h.hosts, a)
		}
	}
}

func (h *hub) publish(owner int64, s *pb.Snapshot) {
	h.mu.Lock()
	defer h.mu.Unlock()
	// A collector on the RPC node's own host is the authority on its name.
	for _, n := range s.Nodes {
		if n.Kind == pb.Kind_KIND_RPC {
			if ip, _, err := net.SplitHostPort(n.Id); err == nil && h.hosts[ip].host != "" {
				n.Label = h.hosts[ip].host
			}
		}
	}
	buf, err := pageJSON.Marshal(s)
	if err != nil {
		log.Printf("snapshot from %s: %v", s.Source, err)
		return
	}
	h.latest[s.Source+"/"+s.Target] = picture{owner, time.Now(), buf}
	for ch := range h.subs {
		select {
		case ch <- buf:
		default: // a page that is not reading drops this frame rather than stalling the collectors
		}
	}
}

func (h *hub) serve(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	ch := make(chan []byte, 64)
	var replay [][]byte
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	for _, p := range h.latest {
		if time.Since(p.at) < replayAge {
			replay = append(replay, p.json)
		}
	}
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.subs, ch)
		h.mu.Unlock()
	}()
	// Written outside the lock: a slow page must not hold up the collectors.
	for _, buf := range replay {
		if _, err := fmt.Fprintf(w, "data: %s\n\n", buf); err != nil {
			return
		}
	}
	fl.Flush()
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
