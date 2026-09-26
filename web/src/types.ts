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
  bytes_out_per_s: number;
  bytes_in_per_s: number;
  stale?: boolean;
}

export interface Model { name: string; n_ctx: number; build: string; structure: Structure }

export interface Snapshot {
  t: number;
  source: string; // hostname of the collector's host
  target: string; // the llama-server's port on that host
  model: Model;
  nodes: Node[];
  links: Link[];
  totals: { tokens_predicted: number; mem_held: number };
}

export interface Structure {
  n_layer?: number;
  n_expert?: number;
  n_expert_used?: number;
}

// Layers a node holds, from its contiguous range; a node without a range is drawn as one layer.
export const layerCount = (layers?: [number, number] | null) => layers ? layers[1] - layers[0] + 1 : 1;

// What the page draws: every source's nodes in one space. Ids are namespaced by source so two hosts'
// "local" nodes never collide; the address a node had in its own snapshot is kept for display.
export interface ViewNode extends Node {
  source: string;
  sourceKey: string; // the picture this node belongs to (collector and port), the key of a strip cell
  host: string;     // the machine holding this node: the source's hostname for a server, the discovered name for an RPC node
  address: string;
  primary: boolean; // the llama-server of the source serving this page: the centre of the picture
  n_ctx: number;
  n_layer?: number;
  model_name: string;
  build: string;
  ctx_fill: number; // this node's source: prompt plus generated tokens over the context window
  server_slot?: Slot;
}

export interface View {
  sources: { id: string; host: string; model: Model; stale: boolean }[];
  hosts: { id: string; mem_total: number }[]; // one per machine holding nodes; mem_total is its device's memory
  nodes: ViewNode[];
  links: Link[];
  totals: { tokens_predicted: number; mem_held: number };
}
