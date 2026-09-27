package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"io"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	pb "github.com/benemon/llamesh/internal/pb/llamesh/v1"
)

func TestTokenTLSSameToken(t *testing.T) {
	addr, srv := startTokenTLSServer(t, "shared")
	conn := dialTokenTLS(t, addr, tokenTLSClientCredentials("shared"))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	st, err := pb.NewIngestServiceClient(conn).Report(ctx)
	if err != nil {
		t.Fatal(err)
	}
	hello := &pb.Hello{Host: "collector", Addresses: []string{"192.0.2.1"}}
	if err := st.Send(&pb.ReportRequest{Body: &pb.ReportRequest_Hello{Hello: hello}}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		srv.hub.mu.Lock()
		defer srv.hub.mu.Unlock()
		return srv.hub.hosts["192.0.2.1"].host == "collector"
	})
}

func TestTokenTLSWrongClientToken(t *testing.T) {
	addr, srv := startTokenTLSServer(t, "right")
	conn, err := tls.Dial("tcp", addr, &tls.Config{
		MinVersion:         tls.VersionTLS13,
		NextProtos:         []string{"h2"},
		InsecureSkipVerify: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(500 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	state := conn.ConnectionState()
	ekm, err := state.ExportKeyingMaterial(tokenBindingLabel, nil, 32)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(tokenProof("wrong", "llamesh client", ekm)); err != nil {
		t.Fatal(err)
	}
	n, err := io.Copy(io.Discard, conn)
	if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
		t.Fatal("server kept the connection open after a wrong proof")
	}
	if n != 0 {
		t.Fatalf("server sent %d bytes to a client with the wrong token", n)
	}
	if got := srv.streams.Load(); got != 0 {
		t.Fatalf("Report invoked %d times", got)
	}
}

func TestTokenTLSWrongServerTokenSendsNothing(t *testing.T) {
	addr, resultc := startImpostor(t, func(_, ekm []byte) []byte { return tokenProof("wrong", "llamesh server", ekm) })
	clientRefuses(t, addr, resultc)
}

func TestTokenTLSRelayCannotForwardProof(t *testing.T) {
	realAddr, srv := startTokenTLSServer(t, "shared")
	proxyAddr, resultc := startTLSRelay(t, realAddr)
	if err := rejectedReport(proxyAddr, tokenTLSClientCredentials("shared")); err == nil {
		t.Fatal("client accepted a relayed token proof")
	}
	select {
	case got := <-resultc:
		if got.clientToServer != sha256.Size {
			t.Fatalf("relay forwarded %d client bytes, want %d", got.clientToServer, sha256.Size)
		}
		if got.serverToClient != 0 {
			t.Fatalf("relay forwarded %d server bytes", got.serverToClient)
		}
	case <-time.After(time.Second):
		t.Fatal("relay did not finish")
	}
	if got := srv.streams.Load(); got != 0 {
		t.Fatalf("Report invoked %d times", got)
	}
}

func TestTokenTLSTLS12Refused(t *testing.T) {
	addr, srv := startTokenTLSServer(t, "shared")
	inner := credentials.NewTLS(&tls.Config{
		MaxVersion:         tls.VersionTLS12,
		NextProtos:         []string{"h2"},
		InsecureSkipVerify: true,
	})
	creds := &tokenCredentials{inner: inner, token: "shared"}
	if err := rejectedReport(addr, creds); err == nil {
		t.Fatal("server accepted TLS 1.2")
	}
	if got := srv.streams.Load(); got != 0 {
		t.Fatalf("Report invoked %d times", got)
	}
}

func TestTokenTLSReflectionRefused(t *testing.T) {
	addr, resultc := startImpostor(t, func(clientProof, _ []byte) []byte { return clientProof })
	clientRefuses(t, addr, resultc)
}

func TestTokenTLSOmitsAuthorizationMetadata(t *testing.T) {
	mdc := make(chan metadata.MD, 1)
	interceptor := grpc.StreamInterceptor(func(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		md, _ := metadata.FromIncomingContext(ss.Context())
		mdc <- md
		return handler(srv, ss)
	})
	addr, _ := startTokenTLSServer(t, "shared", interceptor)
	conn := dialTokenTLS(t, addr, tokenTLSClientCredentials("shared"))
	done := make(chan struct{})
	go func() {
		defer close(done)
		in := make(chan *pb.ReportRequest)
		_, _ = stream(pb.NewIngestServiceClient(conn), "shared", true, "collector", in)
	}()
	select {
	case md := <-mdc:
		if got := md.Get("authorization"); len(got) != 0 {
			t.Fatalf("authorization metadata = %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("Report was not invoked")
	}
	conn.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("collector stream did not stop")
	}
}

func TestBearerTokenWithoutTokenTLS(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gs := grpc.NewServer()
	srv := &ingestServer{hub: newHub(), token: "right"}
	pb.RegisterIngestServiceServer(gs, srv)
	go gs.Serve(ln)
	t.Cleanup(func() {
		gs.Stop()
		ln.Close()
	})
	conn := dialTokenTLS(t, ln.Addr().String(), insecure.NewCredentials())
	report := func(bearer string) (grpc.BidiStreamingClient[pb.ReportRequest, pb.ReportResponse], context.CancelFunc) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		if bearer != "" {
			ctx = metadata.AppendToOutgoingContext(ctx, "authorization", bearer)
		}
		st, err := pb.NewIngestServiceClient(conn).Report(ctx)
		if err != nil {
			t.Fatal(err)
		}
		hello := &pb.Hello{Host: "collector", Addresses: []string{"192.0.2.1"}}
		_ = st.Send(&pb.ReportRequest{Body: &pb.ReportRequest_Hello{Hello: hello}})
		return st, cancel
	}
	for _, bearer := range []string{"", "Bearer wrong"} {
		st, cancel := report(bearer)
		_, err := st.Recv()
		cancel()
		if status.Code(err) != codes.Unauthenticated {
			t.Fatalf("bearer %q: got %v, want Unauthenticated", bearer, err)
		}
	}
	if got := srv.streams.Load(); got != 0 {
		t.Fatalf("Report accepted %d streams without the token", got)
	}
	_, cancel := report("Bearer right")
	defer cancel()
	waitFor(t, func() bool {
		srv.hub.mu.Lock()
		defer srv.hub.mu.Unlock()
		return srv.hub.hosts["192.0.2.1"].host == "collector"
	})
}

func startTokenTLSServer(t *testing.T, token string, opts ...grpc.ServerOption) (string, *ingestServer) {
	t.Helper()
	creds, err := tokenTLSServerCredentials(token)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	opts = append([]grpc.ServerOption{grpc.Creds(creds)}, opts...)
	gs := grpc.NewServer(opts...)
	srv := &ingestServer{hub: newHub(), token: token, tokenTLS: true}
	pb.RegisterIngestServiceServer(gs, srv)
	go gs.Serve(ln)
	t.Cleanup(func() {
		gs.Stop()
		ln.Close()
	})
	return ln.Addr().String(), srv
}

func dialTokenTLS(t *testing.T, addr string, creds credentials.TransportCredentials) *grpc.ClientConn {
	t.Helper()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(creds))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func rejectedReport(addr string, creds credentials.TransportCredentials) error {
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(creds))
	if err != nil {
		return err
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	st, err := pb.NewIngestServiceClient(conn).Report(ctx)
	if err != nil {
		return err
	}
	_ = st.Send(&pb.ReportRequest{Body: &pb.ReportRequest_Hello{Hello: &pb.Hello{Host: "collector"}}})
	_, err = st.Recv()
	return err
}

func waitFor(t *testing.T, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !ready() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for Report to receive Hello")
		}
		time.Sleep(time.Millisecond)
	}
}

type relayResult struct {
	clientToServer int64
	serverToClient int64
}

func startTLSRelay(t *testing.T, realAddr string) (string, <-chan relayResult) {
	t.Helper()
	cert, err := tokenTLSCertificate()
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	resultc := make(chan relayResult, 1)
	go func() {
		clientRaw, err := ln.Accept()
		if err != nil {
			return
		}
		client := tls.Server(clientRaw, &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS13,
			NextProtos:   []string{"h2"},
		})
		if err := client.Handshake(); err != nil {
			client.Close()
			return
		}
		server, err := tls.Dial("tcp", realAddr, &tls.Config{
			MinVersion:         tls.VersionTLS13,
			NextProtos:         []string{"h2"},
			InsecureSkipVerify: true,
		})
		if err != nil {
			client.Close()
			return
		}
		type copied struct {
			clientToServer bool
			n              int64
		}
		copiedc := make(chan copied, 2)
		go func() {
			n, _ := io.Copy(server, client)
			copiedc <- copied{clientToServer: true, n: n}
		}()
		go func() {
			n, _ := io.Copy(client, server)
			copiedc <- copied{n: n}
		}()
		first := <-copiedc
		client.Close()
		server.Close()
		second := <-copiedc
		var result relayResult
		for _, got := range []copied{first, second} {
			if got.clientToServer {
				result.clientToServer = got.n
			} else {
				result.serverToClient = got.n
			}
		}
		resultc <- result
	}()
	return ln.Addr().String(), resultc
}

type impostorResult struct {
	proof int   // bytes of the client's proof read
	after int64 // bytes the client sent after it
	err   error
}

// startImpostor accepts one connection, reads the client's proof, answers with reply, and reports what the
// client sent after that.
func startImpostor(t *testing.T, reply func(clientProof, ekm []byte) []byte) (string, <-chan impostorResult) {
	t.Helper()
	cert, err := tokenTLSCertificate()
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	resultc := make(chan impostorResult, 1)
	go func() {
		raw, err := ln.Accept()
		if err != nil {
			resultc <- impostorResult{err: err}
			return
		}
		conn := tls.Server(raw, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13, NextProtos: []string{"h2"}})
		defer conn.Close()
		if err := conn.Handshake(); err != nil {
			resultc <- impostorResult{err: err}
			return
		}
		state := conn.ConnectionState()
		ekm, err := state.ExportKeyingMaterial(tokenBindingLabel, nil, 32)
		if err != nil {
			resultc <- impostorResult{err: err}
			return
		}
		proof := make([]byte, sha256.Size)
		n, err := io.ReadFull(conn, proof)
		if err != nil {
			resultc <- impostorResult{proof: n, err: err}
			return
		}
		if _, err := conn.Write(reply(proof, ekm)); err != nil {
			resultc <- impostorResult{proof: n, err: err}
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		after, err := io.Copy(io.Discard, conn)
		resultc <- impostorResult{proof: n, after: after, err: err}
	}()
	return ln.Addr().String(), resultc
}

// clientRefuses runs the real client against an impostor and checks it sent its proof and nothing more.
func clientRefuses(t *testing.T, addr string, resultc <-chan impostorResult) {
	t.Helper()
	raw, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if conn, _, err := tokenTLSClientCredentials("right").ClientHandshake(ctx, addr, raw); err == nil {
		conn.Close()
		t.Fatal("client accepted the impostor's proof")
	}
	select {
	case got := <-resultc:
		if got.proof != sha256.Size {
			t.Fatalf("impostor read %d bytes of proof (%v), want %d", got.proof, got.err, sha256.Size)
		}
		if got.after != 0 {
			t.Fatalf("client sent %d bytes after refusing the impostor", got.after)
		}
		if netErr, ok := got.err.(net.Error); ok && netErr.Timeout() {
			t.Fatal("client kept the connection open after refusing the impostor")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("impostor did not finish")
	}
}
