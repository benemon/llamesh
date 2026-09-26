# llamesh

A live picture of a llama.cpp mesh: the llama-servers on the hosts you name and the RPC nodes they split
models across. A particle blob per process sized by the memory it holds, streams between them carrying the
bytes that move, a click for each blob's metrics, and the combined figures along the bottom. Run a collector
beside each llama-server; each discovers everything on its host, including which server.

## Sources, all read on the host running llama-server (verified against b10566, 2026-09-24)

| Fact | Source | Cadence |
|---|---|---|
| model, context, build | `GET /props` | on start and when model_path changes |
| tokens/s, prompt tokens/s, requests processing, totals | `GET /metrics` (Prometheus text, `llamacpp:*`) | every poll |
| current request: prompt tokens, cached, processed, generating | `GET /slots` | every poll |
| RPC nodes | the server process's `--rpc host:port,...` argument (`lsof` on the target port for the pid, `ps` for its command line) | on start and every 10 polls |
| each device's total, model, context and compute memory, local and every RPC node | the server's log at `-lv 4`: `common_memory_breakdown_print` prints one row per device after load. Exact, no inference, no connection to any node. | on load (tail the log) |
| link bytes | interface counters (`netstat -ibn`) on the interface the route to each RPC host uses (`route -n get`) | every poll |

Roster entries for split models carry `-lv 4` so the breakdown is printed; the default verbosity omits it.
`--list-devices` is not used at all.

`ggml-rpc-server` (b10566) serves one client at a time and does not notice a peer that vanishes without a
close: after llama-server aborted mid-token, and again after it was killed, the node held the dead connection
as ESTABLISHED and every new connection queued behind it and timed out, until the agent was restarted. So the
collector never connects to the RPC port itself; every per-node number comes from the server host's log. A load that hangs with the node listening means
the node's RPC server needs restarting. A load started while the node is not
yet accepting, or by a process macOS has not granted Local Network access (a LaunchAgent gets
EHOSTUNREACH to a bridge address until llama-server is allowed under Privacy & Security), puts the whole
model on the local device and prints no warning: the memory table then has
no RPC row, which the page shows honestly as one node.

A split model must be served with `--no-mmap`: with mmap the local Metal device maps the whole file,
including the share the RPC node holds, and exceeds its wired ceiling at compute time ("Insufficient
Memory", then a compute error), while the memory table still shows the correct split. Seen 2026-09-24 on
gpt-oss-120b: 62 GB mapped on a 59 GB device with 41 GB of tensors actually local. The model buffer lines
distinguish the two: `MTL0_Mapped model buffer size` under mmap, `MTL0 model buffer size` without.

Measured on the split gpt-oss-20b test (2026-09-24, tensor-split 1,1, a Mac mini and a MacBook over a Thunderbolt bridge):
load with a cold node uploads 12.4 GB in 25 s; with the node's file cache warm, 0.2 GB in 16 s. Generation
36 tokens/s against 42 alone, with 110 to 126 KB crossing the bridge per generated token in both directions
combined. Prefill of a 4076-token prompt moved 170 MB out and 57 MB back, about 56 KB per prompt token, at
660 tokens/s. So the stream animation has real numbers to scale to: a steady 4 MB/s trickle during
generation and bursts of tens of MB at prefill.

Not available in this build and not shown: per-device KV split, GPU utilisation, remote-side activity
beyond the bytes that reach it. `/props` carries no device information and the load log does not
print per-device buffer sizes, so neither is read.

## Collector (Go, one binary, runs beside llama-server)

Nothing the page shows comes from configuration. The collector's own parameters are flags with defaults:
`-listen 127.0.0.1:8899` and `-poll 1s`; `-target http://127.0.0.1:PORT` is needed only when discovery is
ambiguous. The one file, `-sources path.yaml`, is routing, in the way a Prometheus scrape list is:

```yaml
sources:
  - http://10.0.0.2:8899
```

names the other collectors whose streams this collector's page composes. It proxies each under
`/api/sources/<host:port>/stream`, so the browser talks to one origin even when the
sources sit on a Thunderbolt bridge it cannot reach, and `GET /api/sources` lists the keys. A source's
hostname, model and figures all come from its own snapshot; the file carries none of them.

Target discovery: every `llama-server` process listening on TCP (`lsof -nP -iTCP -sTCP:LISTEN` filtered
by command name) is a target, chat models and embedders alike; `-target http://127.0.0.1:PORT` narrows it
to one. The listeners are re-read every 10 polls: a server that exits is dropped with its picture, a new
one is bound. With none the collector waits and publishes an empty picture. Each target's snapshot
carries `target`, its port; with `source` that is the page's key for one server's picture. From the target's pid, all read
by the same user with no privilege: its command line (`ps -o command= -p PID`) gives `--host`, `--api-key`
and `--rpc`, and lsof gave the port; its file descriptor 2 (`lsof -p PID -a -d 2`) gives the log the memory breakdown is read
from (verified 2026-09-24: the service manager's stderr file for the server). The key is used for the bearer header and never appears in any response or log line.

Endpoints: `GET /` the SPA (embedded); `GET /api/stream` Server-Sent Events, one JSON snapshot per
server per poll:

```json
{"t": 1727170000.0, "source": "orion", "target": "8896",
 "model": {"path": "...gguf", "name": "gpt-oss-120b-F16", "n_ctx": 131072, "build": "b10566-bb4caa754"},
 "nodes": [
   {"id": "local", "kind": "llama-server", "device": "MTL0", "label": "orion",
    "mem_total": 62277025792, "mem_model": 44000000000, "mem_context": 3600000000, "mem_compute": 400000000,
    "tokens_per_s": 21.4, "prompt_tokens_per_s": 0, "requests_processing": 1,
    "slot": {"processing": true, "n_prompt": 34813, "n_cached": 30723, "n_processed": 4090}},
   {"id": "10.0.0.2:50052", "kind": "rpc", "device": "RPC0", "label": "vega",
    "mem_total": 28588376064, "mem_model": 22000000000, "mem_context": 3000000000, "mem_compute": 400000000}
 ],
 "links": [{"from": "local", "to": "10.0.0.2:50052", "iface": "bridge0",
            "bytes_out_per_s": 512000, "bytes_in_per_s": 498000}],
 "totals": {"tokens_predicted": 12345, "prompt_tokens": 234567, "mem_held": 69170898432}}
```

Node names are discovered, never configured. An RPC node's `label` is the hostname of the Bonjour host
whose mDNS address matches the node's IP: enumerate the service types visible on the local links
(`dns-sd -B _services._dns-sd._udp local.`), browse each for instances (`dns-sd -Z <type> local.` gives
instance and SRV target in one pass; every Apple device advertises `_companion-link._tcp`, Avahi hosts
`_workstation._tcp`), resolve each SRV target with `dscacheutil -q host -a name <target>` (mDNS-aware),
and take the target whose address equals the node's IP, stripped of `.local`. A node on another subnet
advertises nothing on the server host's links, so an address still unnamed is put through a reverse lookup
(`dscacheutil -q host -a ip_address <ip>`, DNS or mDNS); a home router names hosts that way. Verified 2026-09-24:
a Mac on a Thunderbolt bridge advertises its local hostname, and that name resolves to its bridge address
while its SSH is off and nothing is installed on it. No match: the label is the device name
the log gives the node (`RPC0`). The local node's label is `scutil --get LocalHostName`. Discovery runs
at topology refresh; each `dns-sd` call is given a 2 s window and killed. `id` is always the address.

Rates are computed by the collector from counter deltas between polls. A poll that fails leaves the
previous value and sets `"stale": true` on the affected node or link; a node that leaves the `--rpc` list is removed at the next rescan.
When a request finishes, the counters jump by the whole request; its tokens were already counted from the
slot as it ran, so that frame reports no rate rather than a burst. Under heavy prefill the server answers
`/slots` slowly and a poll can take several seconds (5 s seen); the frame is late rather than wrong.

## SPA (TypeScript, Vite, Three.js; embedded in the binary)

- One blob per node: a sphere of grains, one per 4 MiB held (floor 400, ceiling 30000), in concentric
  shells, one per transformer layer the node holds, the first innermost; a dense ball of the same grains at
  the core is the context in use, its volume the tokens held, inside a faint shell at the size of the full
  window. Additive point sprites with a soft halo on a dark ground; on a light ground the same hues darkened
  and blended normally, without the glows. A node with a request in flight tightens and brightens.
- One stream per link: grains travelling along the arc from `from` to `to` at a rate proportional to
  `bytes_out_per_s`, and back for `bytes_in_per_s`, sized with the bodies they join. A weight upload reads
  as a torrent one way, prefill as a burst, generation as a steady trickle both ways.
- The page subscribes to its own collector's stream and to every key `GET /api/sources` lists, and
  composes one view keyed by collector and server port: node ids namespaced by that key, context fill per
  server. The primary is the server on the page's own host holding the most memory. A server that goes
  quiet is shown stale after 3 s and dropped after 30 s; the source list is re-read every 30 s.
- Hosts: every node belongs to a machine, a server to its collector's host and an RPC node to the host
  its discovered name resolves to (the same name that host's own collector reports, so its RPC share and
  its own servers share one host). Each host is a faint envelope sphere sized by its device's memory, which
  every server on the device reports identically, with its blobs inside: one at the centre, several on a
  ring each tangent to the envelope from within. Sizes are volumetric, a 64 GiB body having radius 170, so
  the dark space between blobs and envelope is the device's headroom.
- Layout: the primary's host at the centre; other hosts on a sphere around it, azimuth by the golden angle
  and elevation staggered, so more hosts fill space rather than a line.
- Click a blob: a panel with every field of that node from the latest snapshot. Each field has a tick;
  ticked fields render as a label attached to the blob and persist in `localStorage` per node id. Labels
  that would overlap stack.
- Models panel, top left, in the detail panel's idiom: one entry per server with its colour, model name,
  host and port, RPC node count, context, layers, experts, tokens/s and request state; scrolls past what
  fits. An entry is one server's picture: rolling over it fades everything from other sources, and hosts
  holding nothing of it; clicking pins the focus, clicking again releases it. The build string is in the
  server node's detail panel.
- Totals, a second section of the same panel under a separator: held across nodes, tokens/s, link
  throughput, tokens generated.
- Theme, top right: dark, light, or the system's choice, remembered per browser.
- Reconnects the SSE stream on drop; shows the last snapshot greyed while disconnected.

## Not in the first cut

GPU utilisation, per-device KV, history or charts, authentication of the SPA itself (it sits behind a TLS proxy that authenticates clients).

## Deployment

Each collector runs as a user service on its host (it reads the servers' log files, so it runs as their
user), listening on loopback where its page is served through a TLS proxy, and on an address the composing
host can reach where its stream is a source for another page. On macOS a service reaching another host's
address needs the Local Network permission granted once for the binary, and a host with the application
firewall on needs the binary allowed for incoming connections.
