// Command llamesh is a live picture of the model servers on a set of hosts. Run it with -collector on each
// host beside its servers, pointed at one instance run with -server, which serves the page.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"time"
)

// version is set by the Makefile from git describe; collectors report it in their Hello.
var version = "dev"

func main() {
	server := flag.Bool("server", false, "run as the server: accept collectors and serve the page")
	collector := flag.String("collector", "", "run as a collector reporting to the server at this host:port")
	listen := flag.String("listen", "127.0.0.1:8899", "server: address to serve the page on")
	ingest := flag.String("ingest", ":8900", "server: address collectors connect to")
	cert := flag.String("tls-cert", "", "server: certificate for the ingest port; plaintext without it")
	key := flag.String("tls-key", "", "server: key for -tls-cert")
	useTLS := flag.Bool("tls", false, "collector: connect to the server over TLS, verified against the system roots")
	tokenTLS := flag.Bool("token-tls", false, "authenticate TLS with LLAMESH_TOKEN; no certificate files or CA")
	poll := flag.Duration("poll", time.Second, "collector: how often the live sources are read")
	target := flag.String("target", "", "collector: watch only the model server on this URL's port")
	flag.Usage = func() {
		_, _ = fmt.Fprintf(flag.CommandLine.Output(), "usage: llamesh -server [flags] | llamesh -collector host:port [flags]\n"+
			"LLAMESH_TOKEN authenticates collectors as a bearer token, or through -token-tls.\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	token := os.Getenv("LLAMESH_TOKEN") // the environment, since a flag would show in ps
	switch {
	case *tokenTLS && len(token) < minTokenTLSLen:
		log.Fatalf("-token-tls requires LLAMESH_TOKEN of at least %d characters", minTokenTLSLen)
	case *tokenTLS && (*useTLS || *cert != "" || *key != ""):
		log.Fatal("-token-tls is mutually exclusive with -tls and -tls-cert")
	case *server && *collector != "":
		log.Fatal("-server and -collector are exclusive; run one of each")
	case *server:
		log.Fatal(serve(*listen, *ingest, *cert, *key, token, *tokenTLS))
	case *collector != "":
		collect(*collector, *useTLS, token, *tokenTLS, *poll, *target)
	default:
		flag.Usage()
		os.Exit(2)
	}
}
