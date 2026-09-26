# llamesh

When a llama.cpp model is split across machines, or several models share one, there is no view of where
the layers went, how full each device is, or what the RPC link is carrying while a request runs. llamesh
draws that from the running servers. A collector on each host finds its llama-servers, reads their
metrics, slots and the memory table they print at load, and serves a page in which every model is a
particle body inside the envelope of the device holding it, with streams between bodies for the bytes an
RPC split moves.

## What it shows

One server on one host. The body is the model's weights and cache. The shell around it is the device's
memory. The gap between them is the headroom.

![one model](docs/single-model.jpg)

Two servers on one host, a chat model and an embedder, inside the same envelope.

![two models on one host](docs/multi-model.jpg)

A model split over llama.cpp's RPC backend to a second machine, during prompt processing. Each device
holds its range of layers, and the stream is the tensors crossing the link.

![an RPC split](docs/rpc-link.jpg)

Both together, during generation.

![two models and an RPC split](docs/multi-model-rpc-link.jpg)

## Reading the page

- The panel at the top left lists every server with its colour, host and port, context, layers, rate and
  request state. Roll over an entry to fade everything that is not part of that server's picture; click
  to pin the focus, click again to release it. The totals sit under the separator.
- Click a body for every figure the collector has for it. Tick a figure to pin it as a label on the body.
- Drag to orbit, two-finger drag or right-drag to pan, pinch or scroll to zoom. Zooming into a body
  passes through the shells of its layers to the context cache at its core.
- The toggle at the top right selects dark, light or the system theme.

## How it works

The collector is one Go binary built on the standard library. On its host it:

- lists the `llama-server` processes listening on TCP, and reads each one's command line for its bind
  address, API key and `--rpc` list, and its log file for the memory table printed at load;
- reads `/props` once the model has loaded, then `/metrics` and `/slots` on every poll;
- reads the interface counters on the route to each RPC node for the bytes moving;
- names RPC nodes from what they advertise over mDNS, with reverse DNS as the fallback;
- serves the page and a Server-Sent Events stream with one snapshot per server per poll.

Every per-device figure comes from the server's own log at `-lv 4`. The collector never connects to an
RPC node. `SPEC.md` lists each figure and the source it is read from.

The page composes several collectors. One collector is given the addresses of the others and proxies
their streams under its own API, so the browser talks to one origin even when the other hosts sit on a
link it cannot reach.

## Building

Prerequisites: Go 1.26 or later, Node 20 or 22.

1. `make` builds the page with Vite, embeds it, and produces `./llamesh` for the current platform.
2. `go test ./...` runs the parsers against the recorded outputs in `testdata/`.

### Working on the page without a collector

1. `cd web && npm run dev`.
2. Open the URL it prints with `?mock=1`.

`?mock=single`, `multi`, `rpc` or `multi-rpc` picks a scenario. `&phase=prefill`, `generating` or `idle`
holds one phase of the mock's cycle, for screenshots.

## Running

Prerequisites: serve split models with `-lv 4`, so the memory table is printed, and with `--no-mmap`, so
the local device does not map the whole file. `SPEC.md` explains both.

```
./llamesh
```

watches every llama-server on the host and serves the page on `http://127.0.0.1:8899`.

| Flag | Default | Meaning |
|---|---|---|
| `-listen` | `127.0.0.1:8899` | address for the page and API. A collector that another host's page composes listens on an address that host can reach. |
| `-poll` | `1s` | how often the live sources are read |
| `-target` | | `http://127.0.0.1:PORT`, to watch one server rather than all of them |
| `-sources` | | file listing other collectors to compose, see below |

### Composing hosts

The sources file has one line per collector, as a scrape list does:

```yaml
sources:
  - http://collector-2.example:8899
```

The collector reads the `- url` lines; the `sources:` key is not parsed. The page on the collector given
this file draws its own host's servers with theirs. Every name and figure comes from the collectors; the
file says only where they are.

### Deployment

1. Run each collector as a user service on its host, as the user the servers run as: it reads their log
   files.
2. Put a TLS proxy in front of the collector whose page people open. The page uses relative URLs, so it
   works at `/` and behind a path alike.
3. On macOS, a collector reaching another host's address needs the Local Network permission granted once
   for the binary, and a host with the application firewall on needs the binary allowed for incoming
   connections. See Apple's documentation on [Local Network privacy](https://developer.apple.com/documentation/technotes/tn3179-understanding-local-network-privacy).

## Contributing

Everything drawn is discovered from the running servers. A change that adds a figure adds the source it
is read from, verified against a real llama.cpp build, a recording of that source in `testdata/` with a
test, and a line in `SPEC.md`. Configuration names where collectors are, and nothing about what they draw.

- Go: standard library only.
- Page: TypeScript with Three.js. Keep the mock in `web/src/data.ts` able to show a change without a lab.

## Licence

Apache License 2.0. See `LICENSE`.
