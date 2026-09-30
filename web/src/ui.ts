import type { Scene } from "./scene";
import { Kind, type View, type ViewNode } from "./types";

const dash = "—";
const fmtB = (b?: number) => b === undefined ? dash : b >= 1073741824 ? `${(b / 1073741824).toFixed(1)} GiB` : b >= 1048576 ? `${(b / 1048576).toFixed(0)} MiB` : `${(b / 1024).toFixed(0)} KiB`;

// Every scalar of a node, flattened, as label -> display string. Labels are the pin keys, so no two
// rows share one.
function fields(n: ViewNode): [string, string][] {
  const f: [string, string][] = [
    ["host", n.source],
    ["device", n.device],
    ["address", n.address],
  ];
  f.push(
    // a server on the CPU alone holds the machine's memory, which is its total
    [n.device === "CPU" ? "machine memory" : "memory total", fmtB(n.mem_total)],
    ["weights", fmtB(n.mem_model)],
    ["cache", fmtB(n.mem_context)],
    ["compute", fmtB(n.mem_compute)],
    ["held", n.mem_total !== undefined && n.mem_total > 0 && n.mem_model !== undefined && n.mem_context !== undefined && n.mem_compute !== undefined ? `${((n.mem_model + n.mem_context + n.mem_compute) / n.mem_total * 100).toFixed(0)} %` : dash],
  );
  // llama.cpp numbers layers from 0; people count from 1, so "1–24 of 36" and "25–36 of 36"
  f.push(["layers", n.layers && n.n_layer ? `${n.layers.first + 1}–${n.layers.last + 1} of ${n.n_layer}` : dash]);
  f.push(["context", n.server_slot && n.n_ctx ? `${Math.round(n.ctx_fill * 100)} % of ${(n.n_ctx / 1024).toFixed(0)}k held` : dash]);
  if (n.kind === Kind.KIND_LLAMA_SERVER) {
    f.push(["model", n.model_name], ["engine", n.engine], ["build", n.build]);
    f.push(["tokens/s", n.tokens_per_s?.toFixed(1) ?? dash], ["prompt tokens/s", n.prompt_tokens_per_s?.toFixed(0) ?? dash], ["requests", n.requests_processing?.toString() ?? dash], ["queued", n.requests_queued?.toString() ?? dash]);
    if (n.slot) f.push(["slot", n.slot.processing ? "processing" : "idle"], ["prompt", `${n.slot.n_processed} / ${n.slot.n_prompt} (${n.slot.n_cached} cached)`]);
  }
  if (n.stale) f.push(["stale", "yes"]);
  return f;
}

export class UI {
  private panel = document.getElementById("panel")!;
  private labels = document.getElementById("labels")!;
  private models = document.querySelector("#models .rows")!;
  private totals = document.getElementById("totals")!;
  private status = document.getElementById("status")!;
  private depth = document.getElementById("depth")!;
  private selected: string | null = null;
  private last: View | null = null;
  private cells = new Map<string, HTMLElement>(); // panel entries by key
  private pinnedFocus: string | null = null;
  private pins = new Map<string, Set<string>>();

  constructor(private scene: Scene) {
    this.installTheme();
    scene.onPick = (id) => { if (!scene.dragged) this.select(id === this.selected ? null : id); }; // tapping the open blob closes it
    scene.canvas.addEventListener("pointerup", (e) => { if (e.target === scene.canvas && !scene.dragged && !scene.hitTest(e.clientX, e.clientY)) this.select(null); });
    this.panel.addEventListener("click", (e) => { if ((e.target as HTMLElement).closest(".close")) this.select(null); });
    let wasZoomed = false, lastDepth = "";
    scene.onFrame(() => {
      const z = scene.zoom > 1.8;
      if (z !== wasZoomed && this.last) { wasZoomed = z; this.renderLabels(this.last, z); }
      this.placeLabels();
      const d = this.depthText();
      if (d !== lastDepth) { lastDepth = d; this.depth.innerHTML = d; }
    });
  }


  // dark, light, or the system's choice; the page and the scene follow together
  private installTheme() {
    const buttons = document.querySelectorAll<HTMLButtonElement>("#theme button");
    const system = matchMedia("(prefers-color-scheme: light)");
    const apply = (choice: string) => {
      if (choice === "system") delete document.documentElement.dataset.theme; else document.documentElement.dataset.theme = choice;
      buttons.forEach((b) => b.setAttribute("aria-pressed", String(b.dataset.theme === choice)));
      this.scene.theme(choice === "dark" || (choice === "system" && !system.matches));
      try { localStorage.setItem("llamesh.theme", choice); } catch { /* no storage */ }
    };
    let saved = "system";
    try { saved = localStorage.getItem("llamesh.theme") ?? "system"; } catch { /* no storage */ }
    apply(saved);
    buttons.forEach((b) => b.addEventListener("click", () => apply(b.dataset.theme!)));
    system.addEventListener("change", () => { if (!document.documentElement.dataset.theme) apply("system"); });
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
    this.renderLabels(s, this.scene.zoom > 1.8);
  }

  private renderPanel() {
    if (!this.selected || !this.last) return;
    const n = this.last.nodes.find((x) => x.id === this.selected);
    if (!n) { this.select(null); return; }
    const pins = this.pinned(n.id);
    this.panel.innerHTML = `<button class="close" aria-label="close">×</button><h2>${n.kind === Kind.KIND_RPC ? "rpc node" : n.kind === Kind.KIND_DEVICE ? "device" : n.engine || "model server"} <span>${n.label}</span><small>${n.id}</small></h2>` +
      fields(n).map(([k, v]) => `<label><input type="checkbox" data-k="${k}" ${pins.has(k) ? "checked" : ""}/> <b>${k}</b><span>${v}</span></label>`).join("") +
      `<p class="hint">tick a field to pin it to the blob</p>`;
    this.panel.querySelectorAll<HTMLInputElement>("input").forEach((cb) => cb.onchange = () => {
      if (cb.checked) pins.add(cb.dataset.k!); else pins.delete(cb.dataset.k!);
      this.save(n.id);
      if (this.last) this.renderLabels(this.last, this.scene.zoom > 1.8);
    });
  }

  // Zoomed in, the layer range and context fill show without being pinned: that is what the rings are.
  private renderLabels(s: View, zoomed: boolean) {
    this.labels.innerHTML = "";
    for (const n of s.nodes) {
      const pins = this.pinned(n.id);
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

  // Labels hang below their blobs; two blobs of one host sit close, so a label that would overlap one
  // already placed is pushed down beneath it.
  private placeLabels() {
    const placed: { x: number; y: number; w: number; h: number }[] = [];
    this.labels.querySelectorAll<HTMLElement>(".label").forEach((el) => {
      const p = this.scene.screenPos(el.dataset.id!);
      if (!p) return;
      const floor = window.innerHeight - 12;
      const w = el.offsetWidth, h = el.offsetHeight;
      const x = Math.min(Math.max(p.x, w / 2 + 8), window.innerWidth - w / 2 - 8);
      let y = Math.min(p.y + p.r * 1.25, floor - h - 12);
      for (const o of placed) if (Math.abs(o.x - x) < (o.w + w) / 2 && y < o.y + o.h + 6 && y + h + 6 > o.y) y = o.y + o.h + 6;
      placed.push({ x, y, w, h });
      el.style.left = `${x}px`;
      el.style.top = `${y}px`;
    });
  }

  private depthText(): string {
    const d = this.scene.depth();
    if (!d || !this.last) return "";
    const n = this.last.nodes.find((x) => x.id === d.id);
    if (!n) return "";
    if (d.core) return `<b>${n.label}</b> · context core · ${dash} in use (${Math.round(n.ctx_fill * 100)} % of the window)`;
    const layer = n.layers && d.layer !== null ? String(d.layer + 1) : dash;
    return `<b>${n.label}</b> · layer ${layer} of ${n.n_layer ?? dash} · ${dash} weights · ${dash} context in use`;
  }

  // Entries persist and are updated in place so the hover survives the poll.
  private row(src: View["sources"][number], s: View) {
    let el = this.cells.get(`src:${src.id}`);
    if (!el) {
      el = document.createElement("div");
      el.innerHTML = '<span><i></i></span><span class="name"></span><span class="rate"></span><span class="meta"><span class="host"></span> · <span class="shape"></span><b class="req"></b></span>';
      const key = src.id;
      el.addEventListener("pointerenter", () => { if (!this.pinnedFocus) this.scene.focus(key); });
      el.addEventListener("pointerleave", () => { if (!this.pinnedFocus) this.scene.focus(null); });
      el.addEventListener("click", () => {
        this.pinnedFocus = this.pinnedFocus === key ? null : key;
        this.scene.focus(this.pinnedFocus ?? key);
        if (this.pinnedFocus) this.scene.centre(key);
        for (const [k, c] of this.cells) c.classList.toggle("pinned", k === `src:${this.pinnedFocus}`);
      });
      this.cells.set(`src:${src.id}`, el);
      this.models.appendChild(el);
    }
    const nodes = s.nodes.filter((n) => n.sourceKey === src.id);
    const server = nodes.find((n) => n.kind === Kind.KIND_LLAMA_SERVER);
    const colour = "#" + (this.scene.blobs.get(server?.id ?? "")?.colour ?? 0x7fb7ff).toString(16).padStart(6, "0");
    const tps = nodes.reduce((a, n) => a + (n.tokens_per_s ?? 0), 0);
    const slot = server?.slot;
    const prefill = slot?.processing && slot.n_decoded === 0;
    const req = !slot ? "" : !slot.processing ? "idle" : prefill ? (slot.n_processed < slot.n_prompt ? `prompt ${Math.round(slot.n_processed / slot.n_prompt * 100)} %` : "prompt") : "generating";
    const st = src.model.structure;
    const set = (sel: string, text: string) => { const c = el!.querySelector(sel)!; if (c.textContent !== text) c.textContent = text; };
    (el.querySelector("i") as HTMLElement).style.background = colour;
    set(".name", src.model.name || "…");
    set(".host", `${src.host}:${src.id.slice(src.id.lastIndexOf("/") + 1)}${nodes.length > 1 ? ` +${nodes.length - 1} rpc` : ""}`);
    set(".shape", `${src.model.engine || dash} · ${(src.model.n_ctx / 1024).toFixed(0)}k ctx · ${st?.n_layer || dash} layers${st?.n_expert ? ` · ${st.n_expert_used}/${st.n_expert} experts` : ""}`);
    const promptTps = server?.prompt_tokens_per_s;
    set(".rate", prefill ? (promptTps === undefined ? dash : `${promptTps.toFixed(0)} prompt tok/s`) : server?.tokens_per_s === undefined ? dash : `${tps.toFixed(1)} tok/s`);
    const state = src.stale ? "stale" : req;
    set(".req", state ? ` · ${state}` : "");
    return el;
  }

  private cell(key: string, label: string, value: string) {
    let el = this.cells.get(key);
    if (!el) {
      el = document.createElement("div");
      el.innerHTML = "<span></span><b></b>";
      this.cells.set(key, el);
      this.totals.appendChild(el);
    }
    const [k, v] = [el.firstElementChild!, el.lastElementChild!];
    if (k.textContent !== label) k.textContent = label;
    if (v.textContent !== value) v.textContent = value;
    return el;
  }

  private renderStrip(s: View) {
    const want = new Set<string>();
    if (s.nodes.length === 0) {
      want.add("empty"); this.cell("empty", "model", "no llama-server running");
    } else {
      for (const src of s.sources) { want.add(`src:${src.id}`); this.row(src, s); }
      const tps = s.nodes.reduce((a, n) => a + (n.tokens_per_s ?? 0), 0);
      const keep = (key: string, label: string, value: string) => { want.add(key); this.cell(key, label, value); };
      keep("held", "held", s.totals.mem_held === undefined ? dash : `${fmtB(s.totals.mem_held)} across ${s.nodes.length} node${s.nodes.length === 1 ? "" : "s"}`);
      keep("tps", "tokens/s", s.nodes.some((n) => n.tokens_per_s !== undefined) ? tps.toFixed(1) : dash);
      const flow = s.totals.link_bytes_per_s;
      keep("link", "link", flow >= 1e6 ? `${(flow / 1e6).toFixed(1)} MB/s` : `${(flow / 1e3).toFixed(0)} KB/s`);
      keep("generated", "generated", s.totals.tokens_predicted.toLocaleString());
    }
    for (const [key, el] of [...this.cells]) if (!want.has(key)) { el.remove(); this.cells.delete(key); if (this.pinnedFocus && key === `src:${this.pinnedFocus}`) { this.pinnedFocus = null; this.scene.focus(null); } }
  }
}
