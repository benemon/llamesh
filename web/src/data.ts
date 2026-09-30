import { Kind, type Snapshot, type View } from "./types";

export type Listener = (v: View) => void;
export type StateListener = (connected: boolean) => void;

const API = new URL("./api/", document.baseURI).toString();

// A source is one llama-server's picture, keyed by collector host and port. A source that has gone quiet is
// shown stale after 3 s and dropped after 30 s.
interface Arrival { snap: Snapshot; at: number }

function compose(snaps: Map<string, Arrival>, now = Date.now()): View {
  const v: View = { sources: [], hosts: [], nodes: [], links: [], totals: { tokens_predicted: 0, link_bytes_per_s: 0 } };
  // the primary, drawn at the centre, is the server holding the most memory, of what is known
  const live = [...snaps.keys()].filter((k) => now - snaps.get(k)!.at < 30000);
  const held = (k: string) => snaps.get(k)!.snap.nodes.reduce((a, n) => a + (n.mem_model ?? 0) + (n.mem_context ?? 0) + (n.mem_compute ?? 0), 0);
  const keys = live.sort((a, b) => held(b) - held(a) || a.localeCompare(b));
  let primaryTaken = false;
  let totalHeld = 0, heldKnown = true;
  const hosts = new Map<string, number>();
  for (const key of keys) {
    const { snap: s, at } = snaps.get(key)!;
    if (s.nodes.length === 0 || !s.model || !s.totals) continue;
    const server = s.nodes.find((n) => n.kind === Kind.KIND_LLAMA_SERVER);
    const quiet = now - at > 3000;
    // the KV cache holds the prompt and every token generated so far; llama.cpp keeps it between requests
    const fill = server?.slot && s.model.n_ctx ? Math.min(1, (server.slot.n_prompt + server.slot.n_decoded) / s.model.n_ctx) : 0;
    const primary = !primaryTaken;
    primaryTaken = true;
    v.sources.push({ id: key, host: s.source, model: s.model, stale: quiet || !!server?.stale });
    // A host's envelope is its memory: one server's local devices (several GPUs) add up, while servers
    // sharing a device, and an RPC share of a host, report that device's total again.
    let local = 0;
    for (const n of s.nodes) if (n.kind !== Kind.KIND_RPC) local += n.mem_total ?? 0;
    hosts.set(s.source, Math.max(hosts.get(s.source) ?? 0, local));
    for (const n of s.nodes) {
      // an RPC node's label is its host's name; a node that stayed RPC0 is a host of its own
      const host = n.kind !== Kind.KIND_RPC ? s.source : (n.label.startsWith("RPC") ? `${key}/${n.id}` : n.label);
      if (n.kind === Kind.KIND_RPC) hosts.set(host, Math.max(hosts.get(host) ?? 0, n.mem_total ?? 0));
      v.nodes.push({ ...n, id: `${key}/${n.id}`, address: n.id, source: s.source, sourceKey: key, host, primary: primary && n.kind === Kind.KIND_LLAMA_SERVER, n_ctx: s.model.n_ctx, n_layer: s.model.structure?.n_layer, model_name: s.model.name, engine: s.model.engine, build: s.model.build, ctx_fill: fill, server_slot: server?.slot, stale: n.stale || quiet });
    }
    v.totals.tokens_predicted += s.totals.tokens_predicted;
    if (s.totals.mem_held !== undefined) totalHeld += s.totals.mem_held;
    else heldKnown = false;
  }
  if (heldKnown) v.totals.mem_held = totalHeld;
  // An interface's counters cannot be split between two nodes behind it: its figure counts once in the
  // total, and each of its links is drawn without a rate of its own.
  const interfaces = new Map<string, { links: typeof v.links; out: number; inb: number }>();
  for (const key of keys) {
    const s = snaps.get(key)!.snap;
    for (const l of s.links) {
      const id = l.iface ? `${s.source}/${l.iface}` : `${key}/${l.to}`; // no route found: a link of its own
      const entry = interfaces.get(id) ?? { links: [], out: l.bytes_out_per_s, inb: l.bytes_in_per_s };
      entry.links.push({ ...l, from: `${key}/${l.from}`, to: `${key}/${l.to}` });
      interfaces.set(id, entry);
    }
  }
  for (const entry of interfaces.values()) {
    v.totals.link_bytes_per_s += entry.out + entry.inb;
    for (const l of entry.links) v.links.push(entry.links.length === 1 ? l : { ...l, bytes_out_per_s: 0, bytes_in_per_s: 0 });
  }
  v.hosts = [...hosts].map(([id, mem_total]) => ({ id, mem_total }));
  return v;
}

// The server sends every collector's snapshots on one stream, the latest of each picture first. The view
// is recomposed once a second so quiet sources age out without a new arrival; while the stream itself is
// down, time stands still, so the last picture stays.
function stream(onSnapshot: Listener, onState: StateListener): () => void {
  const snaps = new Map<string, Arrival>();
  let lost: number | null = null;
  const emit = () => onSnapshot(compose(snaps, lost ?? Date.now()));
  let es: EventSource | null = null;
  let stopped = false;
  const open = () => {
    es = new EventSource(API + "stream");
    es.onopen = () => {
      // resume the clock: arrivals made before the loss are aged as if the outage had not happened
      if (lost !== null) for (const a of snaps.values()) a.at += Date.now() - lost;
      lost = null;
      onState(true);
    };
    es.onmessage = (e) => { const s: Snapshot = JSON.parse(e.data); snaps.set(`${s.source}/${s.target}`, { snap: s, at: Date.now() }); emit(); };
    es.onerror = () => {
      lost ??= Date.now();
      onState(false);
      es?.close();
      if (!stopped) setTimeout(open, 2000);
    };
  };
  open();
  const tick = setInterval(emit, 1000);
  return () => { stopped = true; clearInterval(tick); es?.close(); };
}

// Mock source for working on the page without a collector: ?mock=<scenario>, one of single (one
// server), multi (two servers on one host), rpc (one server split to an RPC node), multi-rpc (both;
// also ?mock=1). The chat model cycles idle -> prefill burst -> generation at 8 tok/s, with 60 MB/s on
// the link during prefill and 2.6 MB/s during generation; &phase= holds one of them.
function mock(onSnapshot: Listener, onState: StateListener): () => void {
  const scenario = new URLSearchParams(location.search).get("mock") ?? "multi-rpc";
  const withRPC = scenario === "rpc" || scenario === "multi-rpc" || scenario === "1";
  const withEmbed = scenario === "multi" || scenario === "multi-rpc" || scenario === "1";
  const GiB = 1073741824;
  const host = { mem_total: 59392 * 1048576 };
  const server = { id: "local", kind: Kind.KIND_LLAMA_SERVER, device: "MTL0", label: "orion", stale: false, ...host, mem_model: 27 * GiB, mem_context: 5.5 * GiB, mem_compute: 0.9 * GiB };
  const rpc = { id: "10.0.0.2:50052", kind: Kind.KIND_RPC, device: "RPC0", label: "vega", stale: false, mem_total: 27264 * 1048576, mem_model: 12 * GiB, mem_context: 2 * GiB, mem_compute: 0.4 * GiB };
  const embed = { id: "local", kind: Kind.KIND_LLAMA_SERVER, device: "MTL0", label: "orion", stale: false, ...host, mem_model: 7.5 * GiB, mem_context: 2.25 * GiB, mem_compute: 0.3 * GiB };
  const chatNodes = withRPC ? [server, rpc] : [server];
  let t = 0, predicted = 0;
  onState(true);
  const id = setInterval(() => {
    t += 1;
    // &phase=prefill|generating|idle holds one phase, for screenshots
    const lock = new URLSearchParams(location.search).get("phase");
    const phase = lock === "prefill" ? 6 : lock === "generating" ? 15 : lock === "idle" ? 2 : t % 40;
    const prefill = phase >= 5 && phase < 9;
    const gen = phase >= 9 && phase < 30;
    const tps = gen ? 8 + Math.sin(t / 3) : 0;
    const pps = prefill ? 140 : 0;
    predicted += tps;
    const out = prefill ? 45e6 : gen ? 1.4e6 : 0;
    const inb = prefill ? 15e6 : gen ? 1.2e6 : 0;
    const now = Date.now();
    const snaps = new Map<string, Arrival>();
    snaps.set("orion/8896", { at: now, snap: {
      t: now / 1000, source: "orion", target: "8896",
      model: { path: "/models/Qwen3.8-27B-Q8_0.gguf", name: "Qwen3.8-27B-Q8_0", n_ctx: 163840, build: "b10566-bb4caa754", engine: "llama.cpp", structure: { n_layer: 64, n_expert: 0, n_expert_used: 0 } },
      nodes: [
        { ...server, layers: withRPC ? { first: 0, last: 41 } : { first: 0, last: 63 }, tokens_per_s: tps, prompt_tokens_per_s: pps, requests_processing: prefill || gen ? 1 : 0,
          slot: { processing: prefill || gen, n_prompt: 34813 + t * 120, n_cached: 30723, n_processed: prefill ? Math.min(4090, (phase - 5) * 1100) : 4090, n_decoded: gen ? (phase - 9) * 8 : 0 } },
        ...(withRPC ? [{ ...rpc, layers: { first: 42, last: 63 } }] : []),
      ],
      links: withRPC ? [{ from: "local", to: rpc.id, iface: "bridge0", bytes_out_per_s: out, bytes_in_per_s: inb, stale: false }] : [],
      totals: { tokens_predicted: predicted, mem_held: chatNodes.reduce((a, n) => a + n.mem_model + n.mem_context + n.mem_compute, 0) },
    } });
    if (withEmbed) snaps.set("orion/8891", { at: now, snap: {
      t: now / 1000, source: "orion", target: "8891",
      model: { path: "/models/Qwen3-Embedding-8B-Q8_0.gguf", name: "Qwen3-Embedding-8B-Q8_0", n_ctx: 16384, build: "b10566-bb4caa754", engine: "llama.cpp", structure: { n_layer: 36, n_expert: 0, n_expert_used: 0 } },
      nodes: [{ ...embed, tokens_per_s: 0, prompt_tokens_per_s: phase % 7 === 0 ? 900 : 0, requests_processing: phase % 7 === 0 ? 1 : 0, slot: { processing: phase % 7 === 0, n_prompt: 412, n_cached: 0, n_processed: 412, n_decoded: 0 } }],
      links: [],
      totals: { tokens_predicted: 0, mem_held: embed.mem_model + embed.mem_context + embed.mem_compute },
    } });
    onSnapshot(compose(snaps, now));
  }, 1000);
  return () => clearInterval(id);
}

export function source(): (a: Listener, b: StateListener) => () => void {
  return new URLSearchParams(location.search).has("mock") ? mock : stream;
}
