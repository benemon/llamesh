import type { Layers, Link, Model, Node, Slot } from "./pb/llamesh/v1/llamesh";

export { Kind } from "./pb/llamesh/v1/llamesh";
export type { Layers, Link, Model, Node, Slot, Snapshot, Structure } from "./pb/llamesh/v1/llamesh";

// Layers a node holds, from its contiguous range; a node without a range is drawn as one layer.
export const layerCount = (layers?: Layers | null) => layers ? layers.last - layers.first + 1 : 1;

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
  engine: string;
  build: string;
  ctx_fill: number; // this node's source: prompt plus generated tokens over the context window
  server_slot?: Slot;
}

export interface View {
  sources: { id: string; host: string; model: Model; stale: boolean }[];
  hosts: { id: string; mem_total: number }[]; // one per machine holding nodes; mem_total is its device's memory
  nodes: ViewNode[];
  links: Link[];
  totals: { tokens_predicted: number; mem_held?: number; link_bytes_per_s: number };
}
