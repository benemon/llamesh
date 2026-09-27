package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"

	"github.com/benemon/llamesh/internal/discover"
	"github.com/benemon/llamesh/internal/link"
	"github.com/benemon/llamesh/internal/llamaserver"
	"github.com/benemon/llamesh/internal/loadlog"
	pb "github.com/benemon/llamesh/internal/pb/llamesh/v1"
	"github.com/benemon/llamesh/internal/snapshot"
)

// collect watches the host's llama-servers and reports them to the server until the process is stopped.
// Polling runs whether or not the server is reachable; what was queued while it was not is discarded when
// it comes back, since the next poll is current.
func collect(server string, useTLS bool, token string, poll time.Duration, spec string) {
	port, err := targetPort(spec)
	if err != nil {
		log.Fatal(err)
	}
	local := discover.LocalHostName()
	hostMem := discover.MemTotal()
	out := make(chan *pb.ReportRequest, 16)
	go report(server, useTLS, token, local, out)
	// Never blocks polling: when the queue is full the oldest entry makes room, so a Gone is not lost.
	send := func(r *pb.ReportRequest) {
		for {
			select {
			case out <- r:
				return
			default:
				select {
				case <-out:
				default:
				}
			}
		}
	}

	var cur []*target
	rescan := func() {
		ls, err := listeners(port)
		if err != nil {
			log.Printf("listeners: %v", err)
			return
		}
		next := []*target{}
		for _, t := range cur {
			// A target is one process: when it exits (a model swap, a crash) its picture goes with it.
			if cl, err := discover.CommandLine(t.pid); err != nil {
				log.Printf("target llama-server pid %d (port %d) has gone", t.pid, t.port)
				send(&pb.ReportRequest{Body: &pb.ReportRequest_Gone{Gone: &pb.Gone{Target: strconv.Itoa(t.port)}}})
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
			if t, err := bind(l, local, hostMem); err == nil {
				next = append(next, t)
			} else {
				log.Printf("bind pid %d port %d: %v", l.PID, l.Port, err)
			}
		}
		if len(next) == 0 && (cur == nil || len(cur) > 0) {
			log.Printf("no llama-server is listening; waiting for one")
		}
		cur = next
	}
	rescan()

	tk := time.NewTicker(poll)
	for n := 1; ; n++ {
		<-tk.C
		if n%10 == 0 {
			rescan()
		}
		for _, t := range cur {
			s := t.col.Poll()
			s.Target = strconv.Itoa(t.port)
			send(&pb.ReportRequest{Body: &pb.ReportRequest_Snapshot{Snapshot: s}})
		}
	}
}

// report holds one stream open to the server, reconnecting with backoff, and sends a Hello at the start
// of each stream so the server knows the host and its addresses.
func report(server string, useTLS bool, token, local string, in chan *pb.ReportRequest) {
	creds := insecure.NewCredentials()
	if useTLS {
		creds = credentials.NewClientTLSFromCert(nil, "")
	}
	conn, err := grpc.NewClient(server, grpc.WithTransportCredentials(creds),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{Time: 30 * time.Second, Timeout: 10 * time.Second, PermitWithoutStream: true}))
	if err != nil {
		log.Fatalf("server %s: %v", server, err)
	}
	client := pb.NewIngestServiceClient(conn)
	backoff := time.Second
	for {
		sent, err := stream(client, token, local, in)
		if sent {
			backoff = time.Second
		}
		log.Printf("server %s: %v; retrying in %s", server, err, backoff)
		time.Sleep(backoff)
		backoff = min(backoff*2, 30*time.Second)
	}
}

// stream reports until the stream fails, and says whether any snapshot got through.
func stream(client pb.IngestServiceClient, token, local string, in chan *pb.ReportRequest) (bool, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if token != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
	}
	st, err := client.Report(ctx)
	if err != nil {
		return false, err
	}
	for len(in) > 0 {
		<-in // queued while the server was away: stale, and a new stream starts from the next poll
	}
	hello := &pb.Hello{Host: local, Addresses: addresses(), Version: version}
	if err := st.Send(&pb.ReportRequest{Body: &pb.ReportRequest_Hello{Hello: hello}}); err != nil {
		return false, err
	}
	// The server sends nothing; Recv returns when it ends the stream, refuses it, or the connection drops,
	// which an idle collector would otherwise never notice.
	ended := make(chan error, 1)
	go func() {
		_, err := st.Recv()
		ended <- err
	}()
	log.Printf("reporting as %s", local)
	sent := false
	for {
		select {
		case err := <-ended:
			return sent, err
		case r := <-in:
			if err := st.Send(r); err != nil {
				if err == io.EOF {
					return sent, <-ended // the stream has ended; the reason is on the receive side
				}
				return sent, err
			}
			sent = true
		}
	}
}

// addresses is every address on the host's interfaces except loopback: an RPC node is named on the page
// after the collector whose addresses include the node's.
func addresses() []string {
	var out []string
	as, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	for _, a := range as {
		if ip, ok := a.(*net.IPNet); ok && !ip.IP.IsLoopback() {
			out = append(out, ip.IP.String())
		}
	}
	return out
}

// target is one llama-server being watched and the collector built from its command line and log.
type target struct {
	pid, port int
	col       *snapshot.Collector
}

// targetPort is the port of -target, or 0 when every llama-server is watched.
func targetPort(spec string) (int, error) {
	if spec == "" {
		return 0, nil
	}
	i := strings.LastIndexByte(spec, ':')
	p, err := strconv.Atoi(strings.TrimRight(spec[i+1:], "/"))
	if i < 0 || err != nil {
		return 0, fmt.Errorf("-target %q: need a URL ending in :PORT", spec)
	}
	return p, nil
}

// listeners is every llama-server listening on the host, or the one on port when it is not 0.
func listeners(port int) ([]discover.Listener, error) {
	ls, err := discover.Listeners()
	if err != nil || port == 0 {
		return ls, err
	}
	var out []discover.Listener
	for _, l := range ls {
		if l.Port == port {
			out = append(out, l)
		}
	}
	return out, nil
}

func bind(l discover.Listener, local string, hostMem int64) (*target, error) {
	cmdline, err := discover.CommandLine(l.PID)
	if err != nil {
		return nil, err
	}
	args := discover.ParseArgs(cmdline)
	col := &snapshot.Collector{
		Client:  llamaserver.New("http://"+args.Host+":"+strconv.Itoa(l.Port), args.APIKey),
		Args:    args,
		Local:   local,
		HostMem: hostMem,
		Names:   map[string]string{},
		Ifaces:  map[string]string{},
	}
	stderr, err := discover.Stderr(l.PID)
	switch {
	case err != nil:
		log.Printf("no log to read the memory split from: %v", err)
	case strings.HasPrefix(stderr, "/"):
		if f, err := loadlog.Open(stderr); err == nil {
			col.Log = f
		} else {
			log.Printf("log %s: %v", stderr, err)
		}
	default: // a service whose stderr is a socket to the systemd journal
		if j, err := loadlog.OpenJournal(l.PID); err == nil {
			col.Log, stderr = j, "the systemd journal"
		} else {
			log.Printf("stderr is %s and there is no journal to read: %v", stderr, err)
		}
	}
	refreshTopology(col, args)
	log.Printf("target llama-server pid %d port %d, %d rpc node(s), log %s", l.PID, l.Port, len(args.RPC), stderr)
	return &target{pid: l.PID, port: l.Port, col: col}, nil
}

func refreshTopology(col *snapshot.Collector, args discover.Args) {
	var ips []string
	for _, addr := range args.RPC {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			log.Printf("rpc address %q: %v", addr, err)
			continue
		}
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
