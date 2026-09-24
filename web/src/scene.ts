import { Application, Container, Graphics, Particle, ParticleContainer, Sprite, Texture } from "pixi.js";
import type { Link, Node, Snapshot } from "./types";

const PARTICLE_BYTES = 4 * 1048576; // one particle per 4 MiB held
const PALETTE = { local: 0x7fb7ff, rpc: 0xffb36b, out: 0x7fb7ff, in: 0xffb36b };

// Particle textures are generated at the renderer's resolution so they stay crisp on high-density screens.
function dot(app: Application, radius: number): Texture {
  const g = new Graphics().circle(radius, radius, radius).fill({ color: 0xffffff });
  return app.renderer.generateTexture({ target: g, resolution: app.renderer.resolution });
}

// A soft radial glow drawn with Canvas 2D; Graphics has no gradient fill that fades to transparent.
function glow(size: number, resolution: number): Texture {
  const c = document.createElement("canvas");
  c.width = c.height = size * resolution;
  size *= resolution;
  const ctx = c.getContext("2d")!;
  const grad = ctx.createRadialGradient(size / 2, size / 2, 0, size / 2, size / 2, size / 2);
  grad.addColorStop(0, "rgba(255,255,255,0.9)");
  grad.addColorStop(0.35, "rgba(255,255,255,0.25)");
  grad.addColorStop(1, "rgba(255,255,255,0)");
  ctx.fillStyle = grad;
  ctx.fillRect(0, 0, size, size);
  return Texture.from({ resource: c, resolution });
}

// A swarm of particles orbiting a centre. Count follows the memory the node holds; the swarm tightens and
// brightens while the node is working and drifts loosely when idle.
export class Blob {
  readonly container = new Container();
  readonly id: string;
  // A ParticleContainer draws tens of thousands of points in one call; only position and alpha change per frame.
  private swarm = new ParticleContainer({ dynamicProperties: { position: true, alpha: true, scale: false, rotation: false, color: false } });
  private core: Sprite;
  private parts: { p: Particle; r: number; a: number; w: number; n: number; layer: number }[] = [];
  private core2 = new ParticleContainer({ dynamicProperties: { position: true, alpha: true, scale: false, rotation: false, color: false } });
  private coreParts: { p: Particle; r: number; a: number; w: number }[] = [];
  private radius = 60;
  layers: [number, number] | null = null; // this node's layer range
  nLayer = 0;                              // the model's layer count
  ctxFill = 0;                             // 0..1, context in use
  zoom = 1;                                // stage scale, set by the scene each frame
  get screenRadius() { return this.radius; }
  activity = 0;          // target from the latest snapshot
  private shown = 0;     // smoothed value the drawing uses
  x = 0;
  y = 0;

  constructor(node: Node, tex: Texture, coreTex: Texture) {
    this.id = node.id;
    this.core = new Sprite(coreTex);
    this.core.anchor.set(0.5);
    this.core.tint = node.kind === "rpc" ? PALETTE.rpc : PALETTE.local;
    this.core.alpha = 0.12;
    this.core.blendMode = "add";
    this.swarm.blendMode = "add";
    this.core2.blendMode = "add";
    this.container.addChild(this.core, this.swarm, this.core2);
    this.container.eventMode = "static";
    this.container.cursor = "pointer";
    this.resize(node, tex);
  }

  resize(node: Node, tex: Texture) {
    const held = node.mem_model + node.mem_context + node.mem_compute;
    const want = Math.max(400, Math.min(24000, Math.round(held / PARTICLE_BYTES)));
    // Spread grows with the count so the grains keep their spacing: radius ~ sqrt(count).
    this.radius = 60 + 110 * Math.sqrt(Math.max(held, 1) / (64 * 1073741824));
    this.core.width = this.core.height = this.radius * 3.2;
    const tint = node.kind === "rpc" ? PALETTE.rpc : PALETTE.local;
    while (this.parts.length < want) {
      const sc = 0.45 + Math.random() * 0.45;
      const p = new Particle({ texture: tex, anchorX: 0.5, anchorY: 0.5, tint, scaleX: sc, scaleY: sc });
      this.swarm.addParticle(p);
      // flatter than gaussian: dense middle, but the grains stay spaced out to the edge
      const r = Math.pow(Math.random(), 0.6);
      this.parts.push({ p, r, a: Math.random() * Math.PI * 2, w: (0.03 + Math.random() * 0.12) * (Math.random() < 0.5 ? 1 : -1), n: Math.random() * 1000, layer: 0 });
    }
    while (this.parts.length > want) this.swarm.removeParticle(this.parts.pop()!.p);
    // grains belong to layers in order: the first slice to the node's first layer, and so on
    const count = this.layers ? this.layers[1] - this.layers[0] + 1 : 1;
    this.parts.forEach((q, i) => { q.layer = Math.min(count - 1, Math.floor(i * count / this.parts.length)); });
    // the context core: one grain per 4 MiB of context memory, white, shown in proportion to use
    const wantCore = Math.max(60, Math.min(3000, Math.round(node.mem_context / PARTICLE_BYTES)));
    while (this.coreParts.length < wantCore) {
      const p = new Particle({ texture: tex, anchorX: 0.5, anchorY: 0.5, tint: 0xffffff, scaleX: 0.6, scaleY: 0.6 });
      this.core2.addParticle(p);
      this.coreParts.push({ p, r: Math.sqrt(Math.random()), a: Math.random() * Math.PI * 2, w: (0.05 + Math.random() * 0.1) * (Math.random() < 0.5 ? 1 : -1) });
    }
    while (this.coreParts.length > wantCore) this.core2.removeParticle(this.coreParts.pop()!.p);
    this.container.hitArea = { contains: (x: number, y: number) => x * x + y * y <= this.radius * this.radius * 1.2 };
  }

  tick(dt: number, time: number) {
    this.shown += (this.activity - this.shown) * Math.min(1, dt / 3); // eases over ~3 s: a breath, never a snap
    const a = this.shown;
    const R = this.radius * (1 - 0.25 * a);
    // Zooming past 1.6x resolves the swarm into rings, one per layer this node holds, innermost first.
    const ring = Math.min(1, Math.max(0, (this.zoom - 1.6) / 0.8));
    const count = this.layers ? this.layers[1] - this.layers[0] + 1 : 1;
    for (const q of this.parts) {
      q.a += q.w * dt * (1 + a * 0.6);
      const wob = Math.sin(time * 0.25 + q.n) * 0.06 + Math.sin(time * 0.6 + q.n * 1.7) * 0.03;
      const rCloud = R * (q.r + wob);
      const rRing = R * (0.42 + 0.58 * (q.layer + 0.5) / count + wob * 0.15);
      const r = rCloud + (rRing - rCloud) * ring;
      q.p.x = Math.cos(q.a) * r;
      q.p.y = Math.sin(q.a) * r * 0.85;
      // low per-grain alpha: with thousands overlapping, brightness comes from density and must not saturate
      q.p.alpha = 0.12 + 0.2 * a + 0.05 * Math.sin(time * 0.8 + q.n);
    }
    this.swarm.update();
    // the context core fills from the centre as the request grows
    const rc = R * 0.36 * Math.sqrt(Math.max(0.02, this.ctxFill));
    const shown = Math.round(this.coreParts.length * Math.max(0.02, this.ctxFill));
    this.coreParts.forEach((q, i) => {
      q.a += q.w * dt;
      const r = rc * q.r;
      q.p.x = Math.cos(q.a) * r;
      q.p.y = Math.sin(q.a) * r * 0.85;
      q.p.alpha = i < shown ? 0.35 + 0.3 * a : 0;
    });
    this.core2.update();
    this.core.alpha = 0.1 + 0.18 * a;
    this.container.position.set(this.x, this.y);
  }
}

// Particles travelling along a curve between two blobs, spawned at a rate set by bytes per second.
export class Stream {
  readonly container = new Container();
  private pool: { s: Sprite; t: number; up: boolean }[] = [];
  private acc = { out: 0, in: 0 };
  rateOut = 0;
  rateIn = 0;
  constructor(private tex: Texture, readonly from: Blob, readonly to: Blob) {}

  private spawn(up: boolean) {
    const s = new Sprite(this.tex);
    s.anchor.set(0.5);
    s.blendMode = "add";
    s.scale.set(1.2 + Math.random() * 0.8);
    s.tint = up ? PALETTE.out : PALETTE.in;
    this.container.addChild(s);
    this.pool.push({ s, t: 0, up });
  }

  tick(dt: number) {
    // 1 MB/s ~ 30 particles/s over a 1.5 s flight: generation's 2 to 4 MB/s reads as a steady thread of ~100
    // particles, a 45 MB/s prefill burst saturates at the cap and reads as a torrent
    this.acc.out += dt * Math.min(300, 15 * this.rateOut / 1e6);
    this.acc.in += dt * Math.min(300, 15 * this.rateIn / 1e6);
    while (this.acc.out >= 1) { this.spawn(true); this.acc.out -= 1; }
    while (this.acc.in >= 1) { this.spawn(false); this.acc.in -= 1; }
    const ax = this.from.x, ay = this.from.y, bx = this.to.x, by = this.to.y;
    const mx = (ax + bx) / 2, my = (ay + by) / 2;
    for (let i = this.pool.length - 1; i >= 0; i--) {
      const p = this.pool[i];
      p.t += dt / 3;
      if (p.t >= 1) { this.container.removeChild(p.s); p.s.destroy(); this.pool.splice(i, 1); continue; }
      const u = p.up ? p.t : 1 - p.t;
      const bend = p.up ? -70 : 70;
      const cx = mx, cy = my + bend;
      const x = (1 - u) * (1 - u) * ax + 2 * (1 - u) * u * cx + u * u * bx;
      const y = (1 - u) * (1 - u) * ay + 2 * (1 - u) * u * cy + u * u * by;
      p.s.position.set(x + (Math.random() - 0.5) * 2, y + (Math.random() - 0.5) * 2);
      p.s.alpha = Math.sin(p.t * Math.PI) * 0.9;
    }
  }
}

export class Scene {
  readonly app = new Application();
  blobs = new Map<string, Blob>();
  streams: Stream[] = [];
  private tex!: Texture;
  private coreTex!: Texture;
  private time = 0;
  onPick: (id: string) => void = () => {};

  async init(el: HTMLElement) {
    // WebGL, not WebGPU: the auto-detected WebGPU path stalled in Chrome 141 on macOS with no error surfaced.
    // Full device resolution (phones are 3x); the particle count is small enough that fill rate is not a concern.
    await this.app.init({ preference: "webgl", resizeTo: window, autoDensity: true, background: 0x0a0c11, antialias: true, resolution: Math.min(3, window.devicePixelRatio || 1) });
    el.appendChild(this.app.canvas);
    this.tex = dot(this.app, 1.6);
    this.coreTex = glow(256, this.app.renderer.resolution);
    this.installZoom();
    this.app.ticker.add((t) => {
      const dt = t.deltaMS / 1000;
      this.time += dt;
      this.layout();
      for (const s of this.streams) s.tick(dt);
      for (const b of this.blobs.values()) { b.zoom = this.app.stage.scale.x; b.tick(dt, this.time); }
    });
  }

  private layout() {
    const w = this.app.screen.width, h = this.app.screen.height;
    const list = [...this.blobs.values()];
    const gap = w / (list.length + 1);
    list.forEach((b, i) => { b.x = gap * (i + 1); b.y = h * 0.45; });
  }

  // Zoom the stage about a screen point, 1x to 5x; positions elsewhere go through toGlobal so labels and
  // hit tests follow.
  private zoomAt(factor: number, sx: number, sy: number) {
    const st = this.app.stage;
    const next = Math.min(5, Math.max(1, st.scale.x * factor));
    const k = next / st.scale.x;
    st.position.set(sx - (sx - st.position.x) * k, sy - (sy - st.position.y) * k);
    st.scale.set(next);
    if (next === 1) st.position.set(0, 0);
  }

  // True while a drag is in progress or just ended, so the page does not treat the release as a tap.
  dragged = false;

  private installZoom() {
    const c = this.app.canvas;
    c.addEventListener("wheel", (e) => { e.preventDefault(); this.zoomAt(Math.exp(-e.deltaY * 0.002), e.clientX, e.clientY); }, { passive: false });
    const pts = new Map<number, { x: number; y: number }>();
    let lastDist = 0, lastTap = 0, moved = 0;
    c.addEventListener("pointerdown", (e) => {
      pts.set(e.pointerId, { x: e.clientX, y: e.clientY });
      if (pts.size === 1) {
        const now = performance.now();
        if (now - lastTap < 300) { this.app.stage.scale.set(1); this.app.stage.position.set(0, 0); }
        lastTap = now;
        moved = 0;
        this.dragged = false;
      }
      lastDist = 0;
    });
    c.addEventListener("pointermove", (e) => {
      const prev = pts.get(e.pointerId);
      if (!prev) return;
      pts.set(e.pointerId, { x: e.clientX, y: e.clientY });
      if (pts.size === 1) {
        // one pointer: pan when zoomed in, after a few pixels so a tap stays a tap
        moved += Math.hypot(e.clientX - prev.x, e.clientY - prev.y);
        if (moved > 6 && this.app.stage.scale.x > 1) {
          this.dragged = true;
          const st = this.app.stage;
          st.position.set(st.position.x + e.clientX - prev.x, st.position.y + e.clientY - prev.y);
        }
        return;
      }
      if (pts.size !== 2) return;
      const [a, b] = [...pts.values()];
      const d = Math.hypot(a.x - b.x, a.y - b.y);
      if (lastDist > 0) this.zoomAt(d / lastDist, (a.x + b.x) / 2, (a.y + b.y) / 2);
      lastDist = d;
      this.dragged = true;
    });
    const up = (e: PointerEvent) => { pts.delete(e.pointerId); lastDist = 0; if (pts.size === 0) setTimeout(() => { this.dragged = false; }, 0); };
    c.addEventListener("pointerup", up);
    c.addEventListener("pointercancel", up);
  }

  apply(s: Snapshot) {
    for (const n of s.nodes) {
      let b = this.blobs.get(n.id);
      if (!b) {
        b = new Blob(n, this.tex, this.coreTex);
        b.container.on("pointertap", () => this.onPick(n.id));
        this.blobs.set(n.id, b);
        this.app.stage.addChild(b.container);
      }
      b.layers = n.layers ?? null;
      b.nLayer = s.model.structure?.n_layer ?? 0;
      b.resize(n, this.tex);
      b.activity = this.activityOf(n, s.links);
      const server = s.nodes.find((x) => x.kind === "llama-server");
      b.ctxFill = server?.slot && s.model.n_ctx ? Math.min(1, server.slot.n_prompt / s.model.n_ctx) : 0;
    }
    for (const id of [...this.blobs.keys()]) if (!s.nodes.some((n) => n.id === id)) { this.app.stage.removeChild(this.blobs.get(id)!.container); this.blobs.delete(id); }
    for (const l of s.links) {
      let st = this.streams.find((x) => x.from.id === l.from && x.to.id === l.to);
      if (!st) {
        const a = this.blobs.get(l.from), b = this.blobs.get(l.to);
        if (!a || !b) continue;
        st = new Stream(this.tex, a, b);
        this.streams.push(st);
        this.app.stage.addChildAt(st.container, 0);
      }
      st.rateOut = l.bytes_out_per_s;
      st.rateIn = l.bytes_in_per_s;
    }
  }

  // The server's activity is measured; a remote node's is the bytes reaching it, which is all the server knows too.
  private activityOf(n: Node, links: Link[]): number {
    if (n.kind === "llama-server") return (n.requests_processing ?? 0) > 0 || (n.tokens_per_s ?? 0) > 0 ? 1 : 0;
    const flow = links.filter((l) => l.to === n.id).reduce((a, l) => a + l.bytes_out_per_s + l.bytes_in_per_s, 0);
    return Math.min(1, flow / 5e5);
  }

  get zoom() { return this.app.stage.scale.x; }

  screenPos(id: string): { x: number; y: number; r: number } | null {
    const b = this.blobs.get(id);
    if (!b) return null;
    const rect = this.app.canvas.getBoundingClientRect();
    const k = rect.width / this.app.screen.width;
    const g = this.app.stage.toGlobal({ x: b.x, y: b.y });
    return { x: rect.left + g.x * k, y: rect.top + g.y * k, r: b.screenRadius * k * this.app.stage.scale.x };
  }
}
