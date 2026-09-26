import type { Snapshot, View } from "./types";

export type Listener = (v: View) => void;
export type StateListener = (connected: boolean) => void;

const API = new URL("./api/", document.baseURI).toString();

// A source is one llama-server's picture, keyed by collector and port; the page's own collector is
// proxied at api/, the others under api/sources/<key>/. A source that has gone quiet is shown stale
// after 3 s and dropped after 30 s.
interface Arrival { snap: Snapshot; at: number }

function compose(snaps: Map<string, Arrival>, primaryPrefix: string, now = Date.now()): View {
  const v: View = { sources: [], hosts: [], nodes: [], links: [], totals: { tokens_predicted: 0, mem_held: 0 } };
  // the primary is the largest server on the page's own host: a chat model beside an embedder, without
  // anyone having to say which
  const live = [...snaps.keys()].filter((k) => now - snaps.get(k)!.at < 30000);
  const held = (k: string) => snaps.get(k)!.snap.totals.mem_held;
  const keys = live.sort((a, b) => (a.startsWith(primaryPrefix) ? 0 : 1) - (b.startsWith(primaryPrefix) ? 0 : 1) || held(b) - held(a) || a.localeCompare(b));
  let primaryTaken = false;
  const hosts = new Map<string, number>();
  for (const key of keys) {
    const { snap: s, at } = snaps.get(key)!;
    if (s.nodes.length === 0) continue;
    const server = s.nodes.find((n) => n.kind === "llama-server");
    const quiet = now - at > 3000;
    // the KV cache holds the prompt and every token generated so far; llama.cpp keeps it between requests
    const fill = server?.slot && s.model.n_ctx ? Math.min(1, (server.slot.n_prompt + (server.slot.n_decoded ?? 0)) / s.model.n_ctx) : 0;
    const primary = !primaryTaken && key.startsWith(primaryPrefix);
    if (primary) primaryTaken = true;
    v.sources.push({ id: key, host: s.source, model: s.model, stale: quiet || !!server?.stale });
    for (const n of s.nodes) {
      // an RPC node's label is its discovered hostname; a node that stayed RPC0 is a host of its own
      const host = n.kind === "llama-server" ? s.source : (n.label.startsWith("RPC") ? `${key}/${n.id}` : n.label);
      hosts.set(host, Math.max(hosts.get(host) ?? 0, n.mem_total));
      v.nodes.push({ ...n, id: `${key}/${n.id}`, address: n.id, source: s.source, sourceKey: key, host, primary: primary && n.kind === "llama-server", n_ctx: s.model.n_ctx, n_layer: s.model.structure?.n_layer, model_name: s.model.name, build: s.model.build, ctx_fill: fill, server_slot: server?.slot, stale: n.stale || quiet });
    }
    for (const l of s.links) v.links.push({ ...l, from: `${key}/${l.from}`, to: `${key}/${l.to}` });
    v.totals.tokens_predicted += s.totals.tokens_predicted;
    v.totals.mem_held += s.totals.mem_held;
  }
  v.hosts = [...hosts].map(([id, mem_total]) => ({ id, mem_total }));
  return v;
}

// onState reports the connection for the page to grey out.
function subscribe(url: string, onSnapshot: (s: Snapshot) => void, onState: StateListener): () => void {
  let es: EventSource | null = null;
  let stopped = false;
  const open = () => {
    es = new EventSource(url);
    es.onopen = () => onState(true);
    es.onmessage = (e) => onSnapshot(JSON.parse(e.data));
    es.onerror = () => {
      onState(false);
      es?.close();
      if (!stopped) setTimeout(open, 2000);
    };
  };
  open();
  return () => { stopped = true; es?.close(); };
}

// The source list is re-read every 30 s; the view is recomposed once a second so quiet sources age out
// without a new arrival.
function stream(onSnapshot: Listener, onState: StateListener): () => void {
  const snaps = new Map<string, Arrival>();
  const subs = new Map<string, () => void>();
  const emit = () => onSnapshot(compose(snaps, "local/"));
  const take = (prefix: string) => (s: Snapshot) => { snaps.set(`${prefix}/${s.target}`, { snap: s, at: Date.now() }); emit(); };
  subs.set("local", subscribe(API + "stream", take("local"), onState));
  const refresh = async () => {
    let keys: string[] = [];
    try { const r = await fetch(API + "sources"); if (r.ok) keys = await r.json(); } catch { /* the page's own collector is down; the local stream reports that */ }
    for (const k of keys) if (!subs.has(k)) subs.set(k, subscribe(`${API}sources/${encodeURIComponent(k)}/stream`, take(k), () => {}));
    for (const k of [...subs.keys()]) if (k !== "local" && !keys.includes(k)) { subs.get(k)!(); subs.delete(k); for (const sk of [...snaps.keys()]) if (sk.startsWith(`${k}/`)) snaps.delete(sk); emit(); }
  };
  refresh();
  const timer = setInterval(refresh, 30000);
  const tick = setInterval(emit, 1000);
  return () => { clearInterval(timer); clearInterval(tick); for (const stop of subs.values()) stop(); };
}

// Mock source for working on the page without a collector: ?mock=<scenario>, one of single (one
// server), multi (two servers on one host), rpc (one server split to an RPC node), multi-rpc (both;
// also ?mock=1). The chat model cycles idle -> prefill burst -> generation with the rates measured on
// the split gpt-oss-20b test; &phase= holds one of them.
function mock(onSnapshot: Listener, onState: StateListener): () => void {
  const scenario = new URLSearchParams(location.search).get("mock") ?? "multi-rpc";
  const withRPC = scenario === "rpc" || scenario === "multi-rpc" || scenario === "1";
  const withEmbed = scenario === "multi" || scenario === "multi-rpc" || scenario === "1";
  const GiB = 1073741824;
  const host = { mem_total: 59392 * 1048576 };
  const server = { id: "local", kind: "llama-server" as const, device: "MTL0", label: "orion", ...host, mem_model: 27 * GiB, mem_context: 5.5 * GiB, mem_compute: 0.9 * GiB };
  const rpc = { id: "10.0.0.2:50052", kind: "rpc" as const, device: "RPC0", label: "vega", mem_total: 27264 * 1048576, mem_model: 12 * GiB, mem_context: 2 * GiB, mem_compute: 0.4 * GiB };
  const embed = { id: "local", kind: "llama-server" as const, device: "MTL0", label: "orion", ...host, mem_model: 7.5 * GiB, mem_context: 2.25 * GiB, mem_compute: 0.3 * GiB };
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
    snaps.set("local/8896", { at: now, snap: {
      t: now / 1000, source: "orion", target: "8896",
      model: { name: "Qwen3.8-27B-Q8_0", n_ctx: 163840, build: "b10566-bb4caa754", structure: { n_layer: 64 } },
      nodes: [
        { ...server, layers: (withRPC ? [0, 41] : [0, 63]) as [number, number], tokens_per_s: tps, prompt_tokens_per_s: pps, requests_processing: prefill || gen ? 1 : 0,
          slot: { processing: prefill || gen, n_prompt: 34813 + t * 120, n_cached: 30723, n_processed: prefill ? Math.min(4090, (phase - 5) * 1100) : 4090, n_decoded: gen ? (phase - 9) * 8 : 0 } },
        ...(withRPC ? [{ ...rpc, layers: [42, 63] as [number, number] }] : []),
      ],
      links: withRPC ? [{ from: "local", to: rpc.id, bytes_out_per_s: out, bytes_in_per_s: inb }] : [],
      totals: { tokens_predicted: predicted, mem_held: chatNodes.reduce((a, n) => a + n.mem_model + n.mem_context + n.mem_compute, 0) },
    } });
    if (withEmbed) snaps.set("local/8891", { at: now, snap: {
      t: now / 1000, source: "orion", target: "8891",
      model: { name: "Qwen3-Embedding-8B-Q8_0", n_ctx: 16384, build: "b10566-bb4caa754", structure: { n_layer: 36 } },
      nodes: [{ ...embed, tokens_per_s: 0, prompt_tokens_per_s: phase % 7 === 0 ? 900 : 0, requests_processing: phase % 7 === 0 ? 1 : 0, slot: { processing: phase % 7 === 0, n_prompt: 412, n_cached: 0, n_processed: 412 } }],
      links: [],
      totals: { tokens_predicted: 0, mem_held: embed.mem_model + embed.mem_context + embed.mem_compute },
    } });
    onSnapshot(compose(snaps, "local/", now));
  }, 1000);
  return () => clearInterval(id);
}

export function source(): (a: Listener, b: StateListener) => () => void {
  return new URLSearchParams(location.search).has("mock") ? mock : stream;
}
