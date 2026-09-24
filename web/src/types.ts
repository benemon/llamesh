export interface Slot {
  processing: boolean;
  n_prompt: number;
  n_cached: number;
  n_processed: number;
}

export interface Node {
  id: string;
  kind: "llama-server" | "rpc";
  device: string;
  label: string;
  mem_total: number;
  mem_model: number;
  mem_context: number;
  mem_compute: number;
  layers?: [number, number]; // derived from bytes: contiguous, in device order
  tokens_per_s?: number;
  prompt_tokens_per_s?: number;
  requests_processing?: number;
  slot?: Slot;
  stale?: boolean;
}

export interface Link {
  from: string;
  to: string;
  iface: string;
  bytes_out_per_s: number;
  bytes_in_per_s: number;
  stale?: boolean;
}

export interface Snapshot {
  t: number;
  model: { path: string; name: string; n_ctx: number; build: string; structure: Structure };
  nodes: Node[];
  links: Link[];
  totals: { tokens_predicted: number; prompt_tokens: number; mem_held: number };
}

export interface Structure {
  arch?: string;
  name?: string;
  type?: string;
  params?: string;
  n_layer?: number;
  n_embd?: number;
  n_head?: number;
  n_head_kv?: number;
  n_expert?: number;
  n_expert_used?: number;
  n_vocab?: number;
  n_ctx_train?: number;
}

export interface Topology {
  nodes: Pick<Node, "id" | "kind" | "device" | "label">[];
  links: Pick<Link, "from" | "to" | "iface">[];
}
