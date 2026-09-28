# llamesh

A llama.cpp model split across machines with the RPC backend gives no view of which layers each device
holds, how much memory each device has left, or what the link between them carries while a request
runs. Several models on one machine give no view of how they share it. llamesh draws both from the
running servers. A collector on each host finds the `llama-server` processes there and reads their
metrics, their slots and the memory table they print at load. Each collector streams what it reads to
one server, which serves the page.

On the page each node of a model is a body of particles sized by the memory it holds, inside a faint
sphere sized by the memory of the device holding it. Streams of particles between bodies move at the rate
of the interface counters on the link to each RPC node.

![one model](docs/single-model.jpg)

![two models on one host](docs/multi-model.jpg)

![a model split to an RPC node, during prompt processing](docs/rpc-link.jpg)

![two models and an RPC split](docs/multi-model-rpc-link.jpg)

The panel at the top left lists the servers. Rolling over an entry fades everything that is not part of
that server's picture, and clicking pins the focus. Clicking a body opens its figures, and ticking a
figure pins it as a label on the body. Zooming into a body passes through one shell per layer to the
context cache at its core, with a read-out of the layer being passed. The theme toggle at the top right
selects dark, light or the system setting.

Everything drawn comes from the servers and from name discovery on the local network. A collector needs
only the server's address; the server needs no list of collectors.

## Prerequisites

- The server and the collector run on macOS and Linux, WSL2 included. The collector needs `lsof`, and on
  Linux `ip`, and `journalctl` for a server run under systemd. CI runs the Linux path end to end on a CPU
  host. GPUs other than Apple's are expected to work the same way but are untested.
- The GitHub CLI, `gh`, signed in, to download a release and verify its attestation; `cosign` to verify
  its signature.
- Go 1.26 or later, and Node 20, or 22 or later, to build from source.
- Servers run with `-lv 4`. llama.cpp prints the per-device memory table only at that verbosity, and
  without it the collector has no memory figures for the bodies.
- Servers run with `--metrics` for token counters and the total generated; without it the live figures
  come from the slot alone.
- Split models run with `--no-mmap`. `SPEC.md` records the failure seen without it.
- The collector runs as the user the servers run as, or as root. It reads their log files or their
  journal; without that access it still runs, but without per-device memory, layer ranges or expert
  counts.

## Running

Each release carries the binary, with the page embedded, for `linux_amd64`, `linux_arm64`,
`darwin_amd64` and `darwin_arm64`. To install one:

```
V=v0.2.0 A=darwin_arm64
gh release download $V -R benemon/llamesh -p "llamesh_${V}_${A}.tar.gz" -p SHA256SUMS
sha256sum -c --ignore-missing SHA256SUMS
gh attestation verify "llamesh_${V}_${A}.tar.gz" -R benemon/llamesh
tar -xzf "llamesh_${V}_${A}.tar.gz"
```

`shasum -a 256` takes the place of `sha256sum` where it is absent. `gh attestation verify` checks that
the archive was built by this repository's release workflow. The release also signs `SHA256SUMS` with
cosign:

```
cosign verify-blob --bundle SHA256SUMS.cosign.bundle \
  --certificate-identity-regexp '^https://github.com/benemon/llamesh/\.github/workflows/release\.yml@' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com SHA256SUMS
```

The binary runs in one of two modes, which share a token. On one host, make a token and run the server:

```
export LLAMESH_TOKEN=$(openssl rand -base64 36)
./llamesh -server -token-tls
```

On each host with llama-servers, set the same `LLAMESH_TOKEN` and run a collector:

```
./llamesh -collector server.example:8900 -token-tls
```

`-server` accepts collectors on port 8900 and serves the page on `http://127.0.0.1:8899`. `-collector`
watches every llama-server on its host and streams to the server at the address given. It reconnects
when the server restarts, and it can run on the same host as the server. `LLAMESH_TOKEN` is read from
the environment so it does not appear in the process list.

| Flag | Mode | Default | Meaning |
|---|---|---|---|
| `-listen` | server | `127.0.0.1:8899` | address for the page |
| `-ingest` | server | `:8900` | address collectors connect to |
| `-token-tls` | both | | TLS authenticated by `LLAMESH_TOKEN`, with no certificates |
| `-tls-cert`, `-tls-key` | server | | serve the ingest port over TLS with this certificate |
| `-tls` | collector | | connect over TLS, verified against the system roots |
| `-poll` | collector | `1s` | how often `/metrics` and `/slots` are read |
| `-target` | collector | | watch only the server on this port; the port of the URL is used and the rest ignored |

### Connections

A collector connects to the server in one of three ways, and the server and its collectors must use the
same one.

- Token TLS, `-token-tls` on both: TLS 1.3 with no certificates to manage. The server makes a
  certificate when it starts and the collector does not check it. Instead, before anything else is
  sent, the collector and then the server prove they hold `LLAMESH_TOKEN`, bound to that TLS session.
  The token never crosses the network. Whatever a collector dials sees its proof and could guess at the
  token offline, so the token must be at least 32 characters; a random token of that length is out of
  reach.
- Certificate TLS, `-tls-cert` and `-tls-key` on the server and `-tls` on the collector: the collector
  verifies the server's certificate against its host's trusted roots and the address it dials, then
  sends the token inside TLS. The certificate must name that address, as an IP subject alternative name
  when the address is an IP.
- Plaintext, with none of those flags: the collector sends the token in the clear, and the server
  refuses any collector without it. When `LLAMESH_TOKEN` is unset, the server accepts any collector.

Token TLS rests everything on the token: whoever holds it can pose as the server as well as a collector.
Certificate TLS keeps the server's identity separate from the token, at the cost of issuing and renewing
a certificate that every collector's host trusts.

### From source

```
make
```

`make` builds the page with Vite, embeds it, and produces `./llamesh`.

### Deployment

On macOS, a collector connecting to a server on another host needs the Local Network permission for
the binary; Apple documents the permission in
[TN3179](https://developer.apple.com/documentation/technotes/tn3179-understanding-local-network-privacy).
A server host with the application firewall on needs the binary allowed for incoming connections.
Collectors accept no connections. Then:

1. Run the server as a service on one host, and put a TLS proxy in front of its page. The page uses
   relative URLs and works at `/` or behind a path.
2. Run a collector as a user service on each host with llama-servers.

The Ansible collection in [`ansible/`](ansible/README.md) does both from a release, with launchd on
macOS and systemd on Linux.

## Sources

- `lsof` lists the listening `llama-server` processes. `ps` gives each one's `--host`, `--api-key` and
  `--rpc` arguments. `lsof` on the process gives its stderr file.
- `/props` is read until the model has loaded. `/metrics` and `/slots` are read on every poll.
- The `common_memory_breakdown_print` table in the server's log gives the memory each device holds. The
  layer range drawn for each device is derived from its share of the model bytes; the log prints no
  per-layer assignment.
- Interface counters on the interface that routes to each RPC node give the link rates. Two nodes
  behind one interface show the same figures.
- mDNS gives the RPC nodes' names, with reverse DNS as the fallback.

The collector does not connect to the RPC nodes. `SPEC.md` lists every figure, its source, the
llama.cpp build each is checked against, and what is not readable.

## Development

`go test ./...` runs the parsers against the recorded outputs in `testdata/`, and the server's handling
of collector streams. `test/linux-e2e.sh` runs a CPU llama-server under systemd, split to an RPC node,
with a server and collector, and checks what the page receives; CI runs it on Ubuntu.

For the page without a collector:

1. `cd web && npm run dev`.
2. Open the URL it prints with `?mock=1`.

`?mock=single`, `multi`, `rpc` or `multi-rpc` selects a scenario. `&phase=prefill`, `generating` or
`idle` holds one phase of the mock's cycle.

The contract between collectors, the server and the page is `proto/llamesh/v1/llamesh.proto`. `make
proto` lints it with [buf](https://buf.build) and regenerates the Go types in `internal/pb` and the page's
types in `web/src/pb`; the generated code is committed. CI fails when the generated code is stale, and on
a failed vet, test, type-check or build, and it cross-compiles for Linux and macOS.

Outside the gRPC and protobuf modules the Go code uses the standard library only. A change that adds a
figure to the page adds it to the contract, its source to `SPEC.md`, and a recording of that source to
`testdata/` with a test.

## Licence

Apache License 2.0. See `LICENSE`.
