import type { Scene } from "./scene";
import type { View, ViewNode } from "./types";

const fmtB = (b: number) => b >= 1073741824 ? `${(b / 1073741824).toFixed(1)} GiB` : b >= 1048576 ? `${(b / 1048576).toFixed(0)} MiB` : `${(b / 1024).toFixed(0)} KiB`;
const fmtRate = (b: number) => b >= 1e6 ? `${(b / 1e6).toFixed(1)} MB/s` : `${(b / 1e3).toFixed(0)} KB/s`;

// Every scalar of a node, flattened, as label -> display string. What the panel lists and pins draw from.
function fields(n: ViewNode): [string, string][] {
  const f: [string, string][] = [
    ["host", n.source],
    ["device", n.device],
    ["address", n.address],
    ["memory total", fmtB(n.mem_total)],
    ["model", fmtB(n.mem_model)],
    ["context", fmtB(n.mem_context)],
    ["compute", fmtB(n.mem_compute)],
    ["held", `${((n.mem_model + n.mem_context + n.mem_compute) / n.mem_total * 100).toFixed(0)} %`],
  ];
  // llama.cpp numbers layers from 0; people count from 1, so "1–24 of 36" and "25–36 of 36"
  if (n.layers && n.n_layer) f.push(["layers", `${n.layers[0] + 1}–${n.layers[1] + 1} of ${n.n_layer}`]);
  if (n.server_slot && n.n_ctx) f.push(["context", `${Math.round(n.ctx_fill * 100)} % of ${(n.n_ctx / 1024).toFixed(0)}k held`]);
  if (n.kind === "llama-server") {
    f.push(["tokens/s", (n.tokens_per_s ?? 0).toFixed(1)], ["prompt tokens/s", (n.prompt_tokens_per_s ?? 0).toFixed(0)], ["requests", String(n.requests_processing ?? 0)]);
    if (n.slot) f.push(["slot", n.slot.processing ? "processing" : "idle"], ["prompt", `${n.slot.n_processed} / ${n.slot.n_prompt} (${n.slot.n_cached} cached)`]);
  }
  if (n.stale) f.push(["stale", "yes"]);
  return f;
}

export class UI {
  private panel = document.getElementById("panel")!;
  private labels = document.getElementById("labels")!;
  private strip = document.getElementById("strip")!;
  private status = document.getElementById("status")!;
  private depth = document.getElementById("depth")!;
  private selected: string | null = null;
  private last: View | null = null;
  private cells = new Map<string, HTMLElement>(); // strip cells by key, updated in place so hover survives the poll
  private pinnedFocus: string | null = null;
  private pins = new Map<string, Set<string>>();

  constructor(private scene: Scene) {
    scene.onPick = (id) => { if (!scene.dragged) this.select(id === this.selected ? null : id); }; // tapping the open blob closes it
    scene.canvas.addEventListener("pointerup", (e) => { if (e.target === scene.canvas && !scene.dragged && !scene.hitTest(e.clientX, e.clientY)) this.select(null); });
    this.panel.addEventListener("click", (e) => { if ((e.target as HTMLElement).closest(".close")) this.select(null); });
    let wasZoomed = false, lastDepth = "";
    scene.onFrame(() => {
      const z = scene.zoom > 1.8;
      if (z !== wasZoomed && this.last) { wasZoomed = z; this.renderLabels(this.last); }
      this.placeLabels();
      const d = this.depthText();
      if (d !== lastDepth) { lastDepth = d; this.depth.innerHTML = d; }
    });
  }


  private pinned(id: string): Set<string> {
    if (!this.pins.has(id)) {
      let saved: string[] = [];
      try { saved = JSON.parse(localStorage.getItem(`llamesh.pins.${id}`) ?? "[]"); } catch { /* no storage */ }
      this.pins.set(id, new Set(saved));
    }
    return this.pins.get(id)!;
  }

  private save(id: string) {
    try { localStorage.setItem(`llamesh.pins.${id}`, JSON.stringify([...this.pinned(id)])); } catch { /* no storage */ }
  }

  select(id: string | null) {
    this.selected = id;
    this.panel.hidden = id === null;
    this.renderPanel();
  }

  connected(ok: boolean) {
    this.status.textContent = ok ? "" : "disconnected, showing the last snapshot";
    document.body.classList.toggle("stale", !ok);
  }

  apply(s: View) {
    this.last = s;
    this.renderPanel();
    this.renderStrip(s);
    this.renderLabels(s);
  }

  private renderPanel() {
    if (!this.selected || !this.last) return;
    const n = this.last.nodes.find((x) => x.id === this.selected);
    if (!n) { this.select(null); return; }
    const pins = this.pinned(n.id);
    this.panel.innerHTML = `<button class="close" aria-label="close">×</button><h2>${n.kind === "rpc" ? "rpc node" : "llama-server"} <span>${n.label}</span><small>${n.id}</small></h2>` +
      fields(n).map(([k, v]) => `<label><input type="checkbox" data-k="${k}" ${pins.has(k) ? "checked" : ""}/> <b>${k}</b><span>${v}</span></label>`).join("") +
      `<p class="hint">tick a field to pin it to the blob</p>`;
    this.panel.querySelectorAll<HTMLInputElement>("input").forEach((cb) => cb.onchange = () => {
      cb.checked ? pins.add(cb.dataset.k!) : pins.delete(cb.dataset.k!);
      this.save(n.id);
      if (this.last) this.renderLabels(this.last);
    });
  }

  private renderLabels(s: View) {
    this.labels.innerHTML = "";
    for (const n of s.nodes) {
      const pins = this.pinned(n.id);
      // zoomed in, the layer range and context fill show without being pinned: that is what the rings are
      const zoomed = this.scene.zoom > 1.8;
      const rows = fields(n).filter(([k]) => pins.has(k) || (zoomed && (k === "layers" || k === "context")));
      const el = document.createElement("div");
      el.className = "label";
      el.dataset.id = n.id;
      const colour = "#" + (this.scene.blobs.get(n.id)?.colour ?? 0x7fb7ff).toString(16).padStart(6, "0");
      el.innerHTML = `<div class="name"><i style="background:${colour}"></i>${n.label} <em>${n.device}</em></div>` + rows.map(([k, v]) => `<div><span>${k}</span>${v}</div>`).join("");
      this.labels.appendChild(el);
    }
    this.placeLabels();
  }

  private placeLabels() {
    this.labels.querySelectorAll<HTMLElement>(".label").forEach((el) => {
      const p = this.scene.screenPos(el.dataset.id!);
      if (!p) return;
      const strip = this.strip.getBoundingClientRect().top;
      el.style.left = `${Math.min(Math.max(p.x, el.offsetWidth / 2 + 8), window.innerWidth - el.offsetWidth / 2 - 8)}px`;
      el.style.top = `${Math.min(p.y + p.r * 1.25, strip - el.offsetHeight - 12)}px`;
    });
  }

  // What the camera is inside: the layer being passed with its share of the node's weights and context,
  // or the core. Per-layer figures are the node's totals divided by its layer count; llama.cpp reports
  // nothing finer.
  private depthText(): string {
    const d = this.scene.depth();
    if (!d || !this.last) return "";
    const n = this.last.nodes.find((x) => x.id === d.id);
    if (!n) return "";
    const count = n.layers ? n.layers[1] - n.layers[0] + 1 : 1;
    const fill = n.ctx_fill;
    if (d.core) return `<b>${n.label}</b> · context core · ${fmtB(n.mem_context * fill)} of ${fmtB(n.mem_context)} in use (${Math.round(fill * 100)} % of the window)`;
    return `<b>${n.label}</b> · layer ${d.layer! + 1} of ${n.n_layer ?? "?"} · ${fmtB(n.mem_model / count)} weights · ${fmtB(n.mem_context / count * fill)} context in use`;
  }

  // The strip's cells persist and are updated in place. A model cell is one server's picture: rolling
  // over it focuses that picture, clicking pins the focus, clicking again releases it.
  private cell(key: string, label: string, value: string, focusKey?: string) {
    let el = this.cells.get(key);
    if (!el) {
      el = document.createElement("div");
      el.innerHTML = "<span></span><b></b>";
      if (focusKey) {
        el.classList.add("model");
        el.addEventListener("pointerenter", () => { if (!this.pinnedFocus) this.scene.focus(focusKey); });
        el.addEventListener("pointerleave", () => { if (!this.pinnedFocus) this.scene.focus(null); });
        el.addEventListener("click", () => {
          this.pinnedFocus = this.pinnedFocus === focusKey ? null : focusKey;
          this.scene.focus(this.pinnedFocus ?? focusKey);
          for (const c of this.cells.values()) c.classList.toggle("pinned", c === el && this.pinnedFocus !== null);
        });
      }
      this.cells.set(key, el);
      this.strip.appendChild(el);
    }
    const [k, v] = [el.firstElementChild!, el.lastElementChild!];
    if (k.textContent !== label) k.textContent = label;
    if (v.textContent !== value) v.textContent = value;
    return el;
  }

  private renderStrip(s: View) {
    const want = new Set<string>();
    const keep = (key: string, label: string, value: string, focusKey?: string) => { want.add(key); return this.cell(key, label, value, focusKey); };
    if (s.nodes.length === 0) {
      keep("empty", "model", "no llama-server running");
    } else {
      const tps = s.nodes.reduce((a, n) => a + (n.tokens_per_s ?? 0), 0);
      const flow = s.links.reduce((a, l) => a + l.bytes_out_per_s + l.bytes_in_per_s, 0);
      const held = s.nodes.reduce((a, n) => a + n.mem_model + n.mem_context + n.mem_compute, 0);
      s.sources.forEach((src, i) => keep(`src:${src.id}`, i === 0 ? "model" : `model · ${src.host}:${src.id.slice(src.id.lastIndexOf("/") + 1)}`,
        `${src.model.name} · ${(src.model.n_ctx / 1024).toFixed(0)}k ctx · ${src.model.build}` + (src.model.structure?.n_layer ? ` · ${src.model.structure.n_layer} layers${src.model.structure.n_expert ? ` · ${src.model.structure.n_expert_used}/${src.model.structure.n_expert} experts` : ""}` : ""), src.id));
      keep("held", "held", `${fmtB(held)} across ${s.nodes.length} node${s.nodes.length === 1 ? "" : "s"}`);
      keep("tps", "tokens/s", tps.toFixed(1));
      keep("link", "link", fmtRate(flow));
      const slot = s.nodes.find((n) => n.primary)?.slot;
      if (slot) {
        const prog = slot.n_prompt > 0 ? `${Math.min(100, Math.round(slot.n_processed / slot.n_prompt * 100))} %` : "";
        keep("request", "request", slot.processing ? `prompt ${prog}${slot.n_cached ? `, ${Math.round(slot.n_cached / slot.n_prompt * 100)} % cached` : ""}` : "idle");
      }
      keep("generated", "generated", s.totals.tokens_predicted.toLocaleString());
    }
    for (const [key, el] of [...this.cells]) if (!want.has(key)) { el.remove(); this.cells.delete(key); if (this.pinnedFocus && key === `src:${this.pinnedFocus}`) { this.pinnedFocus = null; this.scene.focus(null); } }
  }
}
