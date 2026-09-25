import type { Snapshot, View, ViewNode } from "./types";

export type Listener = (v: View) => void;
export type StateListener = (connected: boolean) => void;

const API = new URL("./api/", document.baseURI).toString();

// The page's own collector is the primary; the others come from its sources file, proxied under
// api/sources/<key>/. Every source's latest snapshot is kept and the view recomposed on each arrival.
export function compose(snaps: Map<string, Snapshot>, primaryKey: string): View {
  const v: View = { t: 0, sources: [], nodes: [], links: [], totals: { tokens_predicted: 0, prompt_tokens: 0, mem_held: 0 } };
  const order = [primaryKey, ...[...snaps.keys()].filter((k) => k !== primaryKey).sort()];
  for (const key of order) {
    const s = snaps.get(key);
    if (!s) continue;
    v.t = Math.max(v.t, s.t);
    const server = s.nodes.find((n) => n.kind === "llama-server");
    // the KV cache holds the prompt and every token generated so far; llama.cpp keeps it between requests
    const fill = server?.slot && s.model.n_ctx ? Math.min(1, (server.slot.n_prompt + (server.slot.n_decoded ?? 0)) / s.model.n_ctx) : 0;
    v.sources.push({ id: s.source || key, model: s.model, stale: !!server?.stale });
    for (const n of s.nodes) {
      const vn: ViewNode = { ...n, id: `${key}/${n.id}`, address: n.id, source: s.source || key, primary: key === primaryKey && n.kind === "llama-server", n_ctx: s.model.n_ctx, n_layer: s.model.structure?.n_layer, ctx_fill: fill, server_slot: server?.slot };
      v.nodes.push(vn);
    }
    for (const l of s.links) v.links.push({ ...l, from: `${key}/${l.from}`, to: `${key}/${l.to}` });
    v.totals.tokens_predicted += s.totals.tokens_predicted;
    v.totals.prompt_tokens += s.totals.prompt_tokens;
    v.totals.mem_held += s.totals.mem_held;
  }
  return v;
}

// Subscribes to one SSE stream; reconnects on drop. onState reports the connection for the page to grey out.
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

// The local stream plus one per source key; the source list is re-read every 30 s so a source added to
// the file appears after the collector restarts, and one whose stream is down keeps its last snapshot,
// shown stale, until it is gone from the list.
export function stream(onSnapshot: Listener, onState: StateListener): () => void {
  const snaps = new Map<string, Snapshot>();
  const subs = new Map<string, () => void>();
  const emit = () => onSnapshot(compose(snaps, "local"));
  subs.set("local", subscribe(API + "stream", (s) => { snaps.set("local", s); emit(); }, onState));
  const refresh = async () => {
    let keys: string[] = [];
    try { const r = await fetch(API + "sources"); if (r.ok) keys = await r.json(); } catch { /* the page's own collector is down; the local stream reports that */ }
    for (const k of keys) if (!subs.has(k)) subs.set(k, subscribe(`${API}sources/${encodeURIComponent(k)}/stream`, (s) => { snaps.set(k, s); emit(); }, () => {}));
    for (const k of [...subs.keys()]) if (k !== "local" && !keys.includes(k)) { subs.get(k)!(); subs.delete(k); snaps.delete(k); emit(); }
  };
  refresh();
  const timer = setInterval(refresh, 30000);
  return () => { clearInterval(timer); for (const stop of subs.values()) stop(); };
}

// Mock source for working on the page without a collector: ?mock=1.
// Cycles idle -> prefill burst -> generation with the rates measured on the split gpt-oss-20b test.
export function mock(onSnapshot: Listener, onState: StateListener): () => void {
  const GiB = 1073741824;
  const nodes = [
    { id: "local", kind: "llama-server" as const, device: "MTL0", label: "orion", mem_total: 59392 * 1048576, mem_model: 44 * GiB, mem_context: 3.6 * GiB, mem_compute: 0.4 * GiB },
    { id: "10.0.0.2:50052", kind: "rpc" as const, device: "RPC0", label: "vega", mem_total: 27264 * 1048576, mem_model: 22 * GiB, mem_context: 3 * GiB, mem_compute: 0.4 * GiB },
  ];
  let t = 0, predicted = 0, prompt = 0;
  onState(true);
  const id = setInterval(() => {
    t += 1;
    const phase = t % 40;
    const prefill = phase >= 5 && phase < 9;
    const gen = phase >= 9 && phase < 30;
    const tps = gen ? 22 + Math.sin(t / 3) * 2 : 0;
    const pps = prefill ? 650 : 0;
    predicted += tps; prompt += pps;
    const out = prefill ? 45e6 : gen ? 1.4e6 : 0;
    const inb = prefill ? 15e6 : gen ? 1.2e6 : 0;
    onSnapshot(compose(new Map([["local", {
      t: Date.now() / 1000,
      source: "mock",
      model: { path: "/x/gpt-oss-120b-F16.gguf", name: "gpt-oss-120b-F16", n_ctx: 131072, build: "b10566-bb4caa754", structure: { arch: "gpt-oss", n_layer: 36, n_embd: 2880, n_head: 64, n_expert: 128, n_expert_used: 4, params: "116.83 B" } },
      nodes: [
        { ...nodes[0], layers: [0, 23] as [number, number], tokens_per_s: tps, prompt_tokens_per_s: pps, requests_processing: prefill || gen ? 1 : 0,
          slot: { processing: prefill || gen, n_prompt: 34813 + t * 120, n_cached: 30723, n_processed: prefill ? Math.min(4090, (phase - 5) * 1100) : 4090 } },
        { ...nodes[1], layers: [24, 35] as [number, number] },
      ],
      links: [{ from: "local", to: "10.0.0.2:50052", iface: "bridge0", bytes_out_per_s: out, bytes_in_per_s: inb }],
      totals: { tokens_predicted: predicted, prompt_tokens: prompt, mem_held: nodes.reduce((a, n) => a + n.mem_model + n.mem_context + n.mem_compute, 0) },
    }]]), "local"));
  }, 1000);
  return () => clearInterval(id);
}

export function source(): (a: Listener, b: StateListener) => () => void {
  return new URLSearchParams(location.search).has("mock") ? mock : stream;
}
