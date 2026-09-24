import type { Snapshot, Topology } from "./types";

export type Listener = (s: Snapshot) => void;
export type StateListener = (connected: boolean) => void;

const API = new URL("./api/", document.baseURI).toString();

export async function topology(): Promise<Topology> {
  const r = await fetch(API + "topology");
  if (!r.ok) throw new Error(`topology ${r.status}`);
  return r.json();
}

// One SSE subscription; reconnects on drop and reports the state so the page can grey out.
export function stream(onSnapshot: Listener, onState: StateListener): () => void {
  let es: EventSource | null = null;
  let stopped = false;
  const open = () => {
    es = new EventSource(API + "stream");
    es.onopen = () => onState(true);
    es.onmessage = (e) => onSnapshot(JSON.parse(e.data));
    es.onerror = () => {
      onState(false);
      es?.close();
      if (!stopped) setTimeout(open, 2000);
    };
  };
  open();
  return () => {
    stopped = true;
    es?.close();
  };
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
    onSnapshot({
      t: Date.now() / 1000,
      model: { path: "/x/gpt-oss-120b-F16.gguf", name: "gpt-oss-120b-F16", n_ctx: 131072, build: "b10566-bb4caa754", structure: { arch: "gpt-oss", n_layer: 36, n_embd: 2880, n_head: 64, n_expert: 128, n_expert_used: 4, params: "116.83 B" } },
      nodes: [
        { ...nodes[0], layers: [0, 23] as [number, number], tokens_per_s: tps, prompt_tokens_per_s: pps, requests_processing: prefill || gen ? 1 : 0,
          slot: { processing: prefill || gen, n_prompt: 34813 + t * 120, n_cached: 30723, n_processed: prefill ? Math.min(4090, (phase - 5) * 1100) : 4090 } },
        { ...nodes[1], layers: [24, 35] as [number, number] },
      ],
      links: [{ from: "local", to: "10.0.0.2:50052", iface: "bridge0", bytes_out_per_s: out, bytes_in_per_s: inb }],
      totals: { tokens_predicted: predicted, prompt_tokens: prompt, mem_held: nodes.reduce((a, n) => a + n.mem_model + n.mem_context + n.mem_compute, 0) },
    });
  }, 1000);
  return () => clearInterval(id);
}

export function source(): (a: Listener, b: StateListener) => () => void {
  return new URLSearchParams(location.search).has("mock") ? mock : stream;
}
