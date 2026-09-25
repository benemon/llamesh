export interface Slot {
  processing: boolean;
  n_prompt: number;
  n_cached: number;
  n_processed: number;
  n_decoded?: number; // tokens generated so far in the request in flight
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

export interface Model { path: string; name: string; n_ctx: number; build: string; structure: Structure }

export interface Snapshot {
  t: number;
  source: string; // hostname of the collector's host
  model: Model;
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


// What the page draws: every source's nodes in one space. Ids are namespaced by source so two hosts'
// "local" nodes never collide; the address a node had in its own snapshot is kept for display.
export interface ViewNode extends Node {
  source: string;
  address: string;
  primary: boolean; // the llama-server of the source serving this page: the centre of the picture
  n_ctx: number;
  n_layer?: number;
  ctx_fill: number; // this node's source: prompt plus generated tokens over the context window
  server_slot?: Slot;
}

export interface View {
  t: number;
  sources: { id: string; model: Model; stale: boolean }[];
  nodes: ViewNode[];
  links: Link[];
  totals: { tokens_predicted: number; prompt_tokens: number; mem_held: number };
}
