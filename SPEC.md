# llamesh

This document records what the collector reads, where each figure comes from, and what the page draws
from it. Everything is verified against llama.cpp b10566 on macOS between 2026-09-24 and 2026-09-27;
dated figures are observations from that setup.

## Sources

All read on the host running llama-server, by the user the server runs as. Two conditions on the servers:
they run with `-lv 4`, since the default verbosity omits the memory table, and split models run with
`--no-mmap` (see Operating notes).

| Fact | Source | Cadence |
|---|---|---|
| model path, name, context size, build | `GET /props` | until it answers; a model swap is a new process |
| tokens/s, prompt tokens/s, requests processing, tokens generated | `GET /metrics` (Prometheus text, `llamacpp:*`), served only with `--metrics`; without it the slot supplies the live rates and requests in flight, and no total | every poll |
| current request: prompt tokens, cached, processed, generated so far | `GET /slots` | every poll |
| the server's bind address, API key and RPC nodes | its command line (`ps -o command= -p PID`: `--host`, `--api-key`, `--rpc host:port,...`); its port from `lsof` | on start and every 10 polls |
| each device's total, model, context and compute memory, local and every RPC node | the server's log at `-lv 4`: `common_memory_breakdown_print` prints one row per device after load | a log file is tailed every poll, the journal read every 10 s; the values change only at load |
| a CPU-only server's model, context and compute memory | the same table's host-memory rows (`Host`, `CPU_REPACK`), which carry no device total | as above |
| a CPU-only server's total | the machine's RAM: `sysctl -n hw.memsize` on macOS, `/proc/meminfo` on Linux | on start |
| layers, experts, experts used | `print_info:` lines in the same log | as above |
| link bytes | on macOS `netstat -ibn` counters for the interface `route -n get` names for each RPC host; on Linux `/proc/net/dev` for the interface `ip route get` names | every poll |

The memory figures are the server's own. The table lists devices in llama.cpp's device order, RPC
devices first. The layer range shown for each device is derived. llama.cpp assigns repeating layers
contiguously in device order in proportion to bytes. It prints no per-layer assignment. The range
therefore follows from each device's share of the model bytes, in the table's order.

Every device that is not an RPC device is local: `MTL0`, `CUDA0`, `Vulkan0` and so on. The first is drawn
as the server; each further one, such as a second GPU, is a node of its own in the same host. A server
with no local device rows runs on the CPU alone, and its host-memory rows together are its local device,
`CPU`. On a host with a GPU the host-memory rows are left out, so a partial offload's CPU layers are not
shown. The link counters
are the whole interface's; two RPC nodes behind one interface show the same figures.

The collector does not connect to an RPC node. `ggml-rpc-server` serves one client at a time and keeps a
connection whose peer vanished without a close, so a probe from the collector could block the server's
next load; see llama.cpp's [RPC server
documentation](https://github.com/ggml-org/llama.cpp/blob/master/tools/rpc/README.md).

## Operating notes

Split models run with `--no-mmap`. With mmap the local Metal device maps the whole file, including the
share the RPC node holds, and exceeds its wired ceiling at compute time while the memory table still
shows the correct split. Seen 2026-09-24 on gpt-oss-120b: 62 GB mapped on a 59 GB device with 41 GB of
tensors local.

Two llama.cpp behaviours affect what the page shows; both are described in the [RPC server
documentation](https://github.com/ggml-org/llama.cpp/blob/master/tools/rpc/README.md). A load that cannot
reach an RPC node puts the whole model on the local device without a warning, and the memory table then
has no RPC row, so the page shows one node. A load that hangs with the node listening means the node is
holding a dead connection; restarting the node's RPC server clears it.

Measured on a split gpt-oss-20b, 2026-09-24, tensor-split 1,1, a Mac mini and a MacBook over a
Thunderbolt bridge: load with a cold node uploaded 12.4 GB in 25 s, and 0.2 GB in 16 s with the node's
file cache warm. Generation ran at 36 tokens/s against 42 alone, with 110 to 126 KB crossing the bridge
per generated token in both directions combined. Prefill of a 4076-token prompt moved 170 MB out and 57
MB back, about 56 KB per prompt token, at 660 tokens/s. The stream animation is scaled to these figures.

## Collector and server

One Go binary runs in either mode. A collector runs on each host with llama-servers. It dials the server
and holds one gRPC stream open, contracted in `proto/llamesh/v1/llamesh.proto`: a `Hello` with the host's
name, its interface addresses and the build from `git describe`, then a `Snapshot` per llama-server per
poll, and a `Gone` when a llama-server exits. The stream is bidirectional though the server sends
nothing, so a collector with nothing to send still learns from its receive side that the server ended the
stream, refused it, or went away. It then reconnects with backoff from 1 s to 30 s. What was queued while
the server was unreachable is discarded on reconnecting, and gRPC keepalives on both ends find a peer
that vanished without closing in about 40 s.

Collector flags: `-collector host:port` (the server), `-tls` to connect over TLS verified against the
system roots, `-poll` (default `1s`), and `-target http://127.0.0.1:PORT` to watch only the server on
that port (the port is used; the rest of the URL is ignored). Server flags: `-server`, `-listen` for the
page (default `127.0.0.1:8899`), `-ingest` for collectors (default `:8900`), and `-tls-cert` with
`-tls-key` to serve the ingest port over TLS. When `LLAMESH_TOKEN` is set, the server refuses a stream
whose bearer token differs, and collectors present theirs; it is read from the environment so it stays
out of the process list.

The server refuses a snapshot before the stream's `Hello`, and keys each picture by the `Hello`'s host
and the snapshot's port. It names an RPC node after the collector whose `Hello` addresses include the
node's address, over the name the node's own server discovered. What a stream reported belongs to that
stream: a `Gone` withdraws one picture from what newly opened pages receive, and the end of the stream
withdraws the rest and its addresses, unless a newer stream from the same collector has taken them over.
Pages already open drop a withdrawn picture as they drop any quiet one. A page opened later receives the
latest snapshot of every picture refreshed in the last 30 s.

Target discovery: every `llama-server` process listening on TCP (`lsof -nP -iTCP -sTCP:LISTEN`, by
command name) is a target, including embedding servers. The listeners are re-read every 10 polls: a
server that has exited is dropped with its picture; a new one is bound. The process's file descriptor 2
(`lsof -p PID -a -d 2`, or `/proc/PID/fd/2` where `lsof` does not name it) gives the log the memory table
is read from. When it is a file, that file is tailed. When it is a socket, as for a service whose output
systemd sends to the journal, the table is read with `journalctl _PID=PID`. The API key goes in the bearer header and appears in no response or log line.

The server's endpoints: `GET /` the page, and `GET /api/stream`, Server-Sent Events carrying every
collector's snapshots, keyed on the page by `source` and `target`. Each is the protobuf JSON rendering of
`Snapshot` with the field names as written in the contract and every non-optional field present, so empty
lists arrive as `[]`. Byte counts and rates are doubles in the contract because the JSON rendering turns
64-bit integers into strings:

```json
{"t": 1790400000.0, "source": "orion", "target": "8896",
 "model": {"path": "...gguf", "name": "gpt-oss-120b-F16", "n_ctx": 131072, "build": "b10566-bb4caa754",
           "structure": {"n_layer": 36, "n_expert": 128, "n_expert_used": 4}},
 "nodes": [
   {"id": "local", "kind": "KIND_LLAMA_SERVER", "device": "MTL0", "label": "orion",
    "mem_total": 62277025792, "mem_model": 44000000000, "mem_context": 3600000000, "mem_compute": 400000000,
    "layers": {"first": 0, "last": 23}, "stale": false, "tokens_per_s": 21.4, "prompt_tokens_per_s": 0, "requests_processing": 1,
    "slot": {"processing": true, "n_prompt": 34813, "n_cached": 30723, "n_processed": 4090, "n_decoded": 120}},
   {"id": "10.0.0.2:50052", "kind": "KIND_RPC", "device": "RPC0", "label": "vega",
    "mem_total": 28588376064, "mem_model": 22000000000, "mem_context": 3000000000, "mem_compute": 400000000,
    "layers": {"first": 24, "last": 35}, "slot": null, "stale": false}
 ],
 "links": [{"from": "local", "to": "10.0.0.2:50052", "iface": "bridge0",
            "bytes_out_per_s": 512000, "bytes_in_per_s": 498000, "stale": false}],
 "totals": {"tokens_predicted": 12345, "mem_held": 73400000000}}
```

An RPC node's `label` is the hostname of the Bonjour host whose mDNS address matches the node's IP:

1. Enumerate the service types visible on the local links: `dns-sd -B _services._dns-sd._udp local.`.
2. Browse each type for instances and SRV targets: `dns-sd -Z <type> local.`.
3. Resolve each target: `dscacheutil -q host -a name <target>`.
4. Take the target whose address equals the node's IP, stripped of `.local`.
5. For an address still unnamed, try a reverse lookup: `dscacheutil -q host -a ip_address <ip>` on
   macOS; on Linux, where steps 1 to 4 have no `dns-sd`, `avahi-resolve-address <ip>` and then
   `getent hosts <ip>`.

With no match the label is `RPC<n>`, the node's position in the `--rpc` list. The local node's label is
`scutil --get LocalHostName`, or the host name without `.local` where `scutil` is absent. Each `dns-sd`
call is given a 2 s window and killed, since it does not exit on its own. An RPC node's `id` is always
its address.

Rates are counter deltas between polls. While a request is in flight the slot's own progress is the live
rate. When a request finishes the counters jump by the whole request; those tokens are counted from the
slot as they run, so that frame reports no rate. A poll that fails leaves the previous value and sets
`"stale": true` on the affected node or link; the server node is stale when neither `/metrics` nor
`/slots` answers. A node that leaves the `--rpc` list is removed at the next
rescan. Under heavy prefill the server answers `/slots` slowly and a poll can take several seconds (5 s
seen); the frame is late.

## Page

The page is TypeScript, built with Vite and Three.js, and embedded in the binary.

- One blob per node: a sphere of grains, one per 4 MiB held (floor 400, ceiling 30000), in concentric
  shells, one per layer the node holds, the first innermost. A dense ball of the same grains at the core
  is the context in use, its volume the tokens held, inside a faint shell at the size of the full window.
  On a dark ground the grains are additive point sprites with a soft halo; on a light ground the same hues
  are darkened and blended normally, without the glows. A node with a request in flight tightens and
  brightens.
- One stream per link: grains travelling along the arc from `from` to `to` at a rate proportional to
  `bytes_out_per_s`, and back for `bytes_in_per_s`, sized with the bodies they join.
- The page reads the server's one stream and composes one view keyed by collector host and server port,
  with node ids namespaced by that key and context fill per server. The primary, at the centre, is the
  server holding the most memory. A server that goes quiet is shown stale after 3 s and dropped after 30 s.
- Hosts: a server and its further local devices belong to its collector's host, an RPC node to the host
  its discovered name resolves to; a host's own collector reports the same name, so its RPC share and its
  own servers share one host. Each host is a faint envelope sphere sized by its memory: the sum of one
  server's local devices, the largest such sum where several servers share a device, since each reports
  the device's total again. Its blobs sit inside it: one at the centre, several on a ring each tangent to the
  envelope from within. Sizes are volumetric, a 64 GiB body having radius 170.
- Layout: the primary's host at the centre; other hosts on a sphere around it, azimuth by the golden angle
  and elevation staggered.
- Camera: orbit, pan and zoom; double-click resets it. Inside a blob, a read-out at the top names the layer
  being passed, or the core, with that layer's share of the node's weights and context. Past a zoom of 1.8
  each blob shows its layer range and context fill as labels without being pinned.
- Click a blob: a panel with every field of that node from the latest snapshot. Each field has a tick;
  ticked fields render as a label attached to the blob and persist in `localStorage` per node id. Labels
  that would overlap stack.
- Models panel, top left: one entry per server with its colour, model name, host and port, RPC node count,
  context, layers, experts, tokens/s and request state; it scrolls past what fits. Rolling over an entry
  fades everything from other sources, and hosts holding nothing of that source; clicking pins the focus,
  clicking again releases it. The build string is in the server node's panel.
- Totals, a second section of the same panel: held across nodes, tokens/s, link throughput, tokens
  generated.
- Theme, top right: dark, light, or the system's choice, remembered per browser.
- The SSE stream reconnects on drop; the last snapshot stays, greyed, while disconnected.

## Not shown

The page does not show the per-device split of context tokens, GPU utilisation, remote-side activity
beyond the bytes that reach it, history or charts, or authentication of the page itself (it sits behind a
TLS proxy that authenticates clients). `/props` carries no device information.

## Deployment

The server listens for the page on loopback, behind a TLS proxy, and for collectors on an address they
can reach. Collectors accept no connections. The service setup and the macOS permissions are in the
README.
