# llamesh

A llama.cpp model split across machines with the RPC backend gives no view of which layers each device
holds, how much memory each device has left, or what the link between them carries while a request
runs. Several models on one machine give no view of how they share it. llamesh draws both from the
running servers: a collector on each host finds the `llama-server` processes there, reads their
metrics, their slots and the memory table they print at load, and serves a page.

On the page each node of a model is a body of particles sized by the memory it holds, inside a faint
sphere sized by the memory of the device holding it. Streams of particles between bodies move at the rate of the
interface counters on the link to each RPC node.

![one model](docs/single-model.jpg)

![two models on one host](docs/multi-model.jpg)

![a model split to an RPC node, during prompt processing](docs/rpc-link.jpg)

![two models and an RPC split](docs/multi-model-rpc-link.jpg)

The panel at the top left lists the servers. Rolling over an entry fades everything that is not part of
that server's picture, and clicking pins the focus. Clicking a body opens its figures, and ticking a
figure pins it as a label on the body. Zooming into a body passes through one shell per layer to the
context cache at its core, with a read-out of the layer being passed. The theme toggle at the top right
selects dark, light or the system setting.

Everything drawn comes from the servers and from name discovery on the local network. `-sources` names
a file listing other collectors to show on the same page.

## Prerequisites

- Go 1.26 or later, and Node 20, or 22 or later, to build.
- Servers run with `-lv 4`. llama.cpp prints the per-device memory table only at that verbosity, and
  without it the collector has no memory figures for the bodies.
- Split models run with `--no-mmap`. `SPEC.md` records the failure seen without it.
- The collector runs as the user the servers run as. It reads their log files; without that access it
  still runs, but without per-device memory, layer ranges or expert counts.

## Running

```
make
./llamesh
```

`make` builds the page with Vite, embeds it, and produces `./llamesh`. The collector then watches every
llama-server on the host and serves the page on `http://127.0.0.1:8899`.

| Flag | Default | Meaning |
|---|---|---|
| `-listen` | `127.0.0.1:8899` | address for the page and API |
| `-poll` | `1s` | how often `/metrics` and `/slots` are read |
| `-target` | | watch only the server on this port; the port of the URL is used and the rest ignored |
| `-sources` | | file listing other collectors, see below |

### Several hosts

A collector runs on each host. One of them, the one whose page is opened, is given a file listing the
others:

```yaml
sources:
  - http://10.0.0.2:8899
```

Every line beginning `- ` is read as a collector URL; the `sources:` key is not parsed. The collector
proxies the listed streams under its own API, so the browser connects to one address. A listed
collector listens on an address the proxying host can reach, set with `-listen`.

### Deployment

On macOS, a collector reaching another host needs the Local Network permission for the binary, and a
host with the application firewall on needs the binary allowed; Apple documents the permission in
[TN3179](https://developer.apple.com/documentation/technotes/tn3179-understanding-local-network-privacy).
Then:

1. Run each collector as a user service on its host.
2. Put a TLS proxy in front of the collector whose page is opened. The page uses relative URLs and works
   at `/` or behind a path.

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

`go test ./...` runs the parsers against the recorded outputs in `testdata/`.

For the page without a collector:

1. `cd web && npm run dev`.
2. Open the URL it prints with `?mock=1`.

`?mock=single`, `multi`, `rpc` or `multi-rpc` selects a scenario. `&phase=prefill`, `generating` or
`idle` holds one phase of the mock's cycle.

The Go module depends on the standard library only. A change that adds a figure to the page adds its
source to `SPEC.md` and a recording of that source to `testdata/` with a test.

## Licence

Apache License 2.0. See `LICENSE`.
