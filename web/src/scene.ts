import * as THREE from "three";
import { OrbitControls } from "three/addons/controls/OrbitControls.js";
import type { Link, Node, Snapshot } from "./types";

const PARTICLE_BYTES = 4 * 1048576; // one grain per 4 MiB held
const PALETTE = { local: 0x7fb7ff, rpc: 0xffb36b, out: 0x7fb7ff, in: 0xffb36b };

// A soft round sprite for every grain, drawn with Canvas 2D at the device resolution.
function grainTexture(): THREE.Texture {
  const size = 64;
  const c = document.createElement("canvas");
  c.width = c.height = size;
  const ctx = c.getContext("2d")!;
  const g = ctx.createRadialGradient(size / 2, size / 2, 0, size / 2, size / 2, size / 2);
  g.addColorStop(0, "rgba(255,255,255,1)");
  g.addColorStop(0.5, "rgba(255,255,255,0.8)");
  g.addColorStop(1, "rgba(255,255,255,0)");
  ctx.fillStyle = g;
  ctx.fillRect(0, 0, size, size);
  const t = new THREE.CanvasTexture(c);
  t.colorSpace = THREE.SRGBColorSpace;
  return t;
}

function glowTexture(): THREE.Texture {
  const size = 256;
  const c = document.createElement("canvas");
  c.width = c.height = size;
  const ctx = c.getContext("2d")!;
  const g = ctx.createRadialGradient(size / 2, size / 2, 0, size / 2, size / 2, size / 2);
  g.addColorStop(0, "rgba(255,255,255,0.7)");
  g.addColorStop(0.4, "rgba(255,255,255,0.18)");
  g.addColorStop(1, "rgba(255,255,255,0)");
  ctx.fillStyle = g;
  ctx.fillRect(0, 0, size, size);
  return new THREE.CanvasTexture(c);
}

function points(tex: THREE.Texture, color: number, size: number, capacity: number): THREE.Points {
  const geo = new THREE.BufferGeometry();
  geo.setAttribute("position", new THREE.BufferAttribute(new Float32Array(capacity * 3), 3));
  const mat = new THREE.PointsMaterial({ map: tex, color, size, transparent: true, opacity: 0.5, blending: THREE.AdditiveBlending, depthWrite: false, sizeAttenuation: true });
  const p = new THREE.Points(geo, mat);
  p.frustumCulled = false;
  return p;
}

// A node is a sphere of concentric shells, one per transformer layer it holds, the first layer innermost:
// one body from afar, separate shells once the camera is among them. The context cache is the core.
export class Blob {
  readonly group = new THREE.Group();
  readonly id: string;
  private grains: THREE.Points;
  private core: THREE.Mesh;       // the context in use: one body, volume proportional to tokens
  private capacity: THREE.Mesh;   // faint shell at the size the context would be when full
  private coreGlow: THREE.Sprite;
  private halo: THREE.Sprite;
  private pick: THREE.Mesh;
  private base: { theta: number; phi: number; w: number; jitter: number; layer: number }[] = [];
  radius = 60;
  layers: [number, number] | null = null;
  activity = 0;
  ctxFill = 0;
  private shown = 0;
  private spin = 0;

  constructor(node: Node, tex: THREE.Texture, glow: THREE.Texture) {
    this.id = node.id;
    const tint = node.kind === "rpc" ? PALETTE.rpc : PALETTE.local;
    this.grains = points(tex, tint, 3.2, 30000);
    this.core = new THREE.Mesh(new THREE.SphereGeometry(1, 48, 32), new THREE.MeshBasicMaterial({ color: 0xffffff, transparent: true, opacity: 0.85, blending: THREE.AdditiveBlending, depthWrite: false }));
    this.capacity = new THREE.Mesh(new THREE.SphereGeometry(1, 48, 32), new THREE.MeshBasicMaterial({ color: tint, transparent: true, opacity: 0.06, blending: THREE.AdditiveBlending, depthWrite: false, side: THREE.DoubleSide }));
    this.coreGlow = new THREE.Sprite(new THREE.SpriteMaterial({ map: glow, color: 0xffffff, transparent: true, opacity: 0.5, blending: THREE.AdditiveBlending, depthWrite: false }));
    this.halo = new THREE.Sprite(new THREE.SpriteMaterial({ map: glow, color: tint, transparent: true, opacity: 0.35, blending: THREE.AdditiveBlending, depthWrite: false }));
    this.pick = new THREE.Mesh(new THREE.SphereGeometry(1, 12, 12), new THREE.MeshBasicMaterial({ visible: false }));
    this.pick.userData.id = node.id;
    this.group.add(this.halo, this.grains, this.capacity, this.coreGlow, this.core, this.pick);
    this.resize(node);
  }

  resize(node: Node) {
    const held = node.mem_model + node.mem_context + node.mem_compute;
    const want = Math.max(400, Math.min(30000, Math.round(held / PARTICLE_BYTES)));
    this.radius = 60 + 110 * Math.sqrt(Math.max(held, 1) / (64 * 1073741824));
    const count = this.layers ? this.layers[1] - this.layers[0] + 1 : 1;
    while (this.base.length < want) {
      // uniform on a sphere: theta around the axis, phi from the pole
      this.base.push({ theta: Math.random() * Math.PI * 2, phi: Math.acos(2 * Math.random() - 1), w: 0.6 + Math.random() * 0.8, jitter: (Math.random() - 0.5) * 0.35, layer: 0 });
    }
    this.base.length = want;
    this.base.forEach((g, i) => { g.layer = Math.min(count - 1, Math.floor(i * count / want)); });
    this.grains.geometry.setDrawRange(0, want);
    this.capacity.scale.setScalar(this.radius * 0.3);
    this.halo.scale.set(this.radius * 3.4, this.radius * 3.4, 1);
    this.pick.scale.setScalar(this.radius * 1.1);
  }

  tick(dt: number, time: number) {
    this.shown += (this.activity - this.shown) * Math.min(1, dt / 3);
    const a = this.shown;
    const R = this.radius * (1 - 0.25 * a);
    const count = this.layers ? this.layers[1] - this.layers[0] + 1 : 1;
    this.spin += dt * 0.05 * (1 + a * 0.6);
    const pos = this.grains.geometry.getAttribute("position") as THREE.BufferAttribute;
    const arr = pos.array as Float32Array;
    const n = this.base.length;
    const shell = (R * 0.66) / count; // shells from 0.34R (outside the core) to R, evenly spaced
    for (let i = 0; i < n; i++) {
      const g = this.base[i];
      const th = g.theta + this.spin * g.w;
      const wob = Math.sin(time * 0.25 + i) * 0.04;
      const r = R * 0.34 + shell * (g.layer + 0.5 + g.jitter) + R * wob;
      const sp = Math.sin(g.phi);
      arr[i * 3] = r * sp * Math.cos(th);
      arr[i * 3 + 1] = r * Math.cos(g.phi);
      arr[i * 3 + 2] = r * sp * Math.sin(th);
    }
    pos.needsUpdate = true;
    (this.grains.material as THREE.PointsMaterial).opacity = 0.32 + 0.3 * a;
    // the core's volume is the context in use; it can only grow to the capacity shell
    const rc = this.radius * 0.3 * Math.cbrt(Math.max(0.01, this.ctxFill));
    this.core.scale.setScalar(rc);
    this.coreGlow.scale.set(rc * 4, rc * 4, 1);
    (this.core.material as THREE.MeshBasicMaterial).opacity = 0.7 + 0.25 * a + 0.05 * Math.sin(time * 1.5);
    (this.coreGlow.material as THREE.SpriteMaterial).opacity = 0.35 + 0.35 * a;
    (this.halo.material as THREE.SpriteMaterial).opacity = 0.22 + 0.25 * a;
  }
}

// Grains travelling along an arc between two nodes: out above, in below, at a rate set by bytes per second.
export class Stream {
  readonly points: THREE.Points;
  private pool: { t: number; up: boolean }[] = [];
  private acc = { out: 0, in: 0 };
  rateOut = 0;
  rateIn = 0;
  constructor(tex: THREE.Texture, readonly from: Blob, readonly to: Blob) {
    this.points = points(tex, 0xffffff, 4.5, 2000);
    (this.points.material as THREE.PointsMaterial).opacity = 0.9;
    this.points.geometry.setAttribute("color", new THREE.BufferAttribute(new Float32Array(2000 * 3), 3));
    (this.points.material as THREE.PointsMaterial).vertexColors = true;
  }

  tick(dt: number) {
    this.acc.out += dt * Math.min(300, 15 * this.rateOut / 1e6);
    this.acc.in += dt * Math.min(300, 15 * this.rateIn / 1e6);
    while (this.acc.out >= 1 && this.pool.length < 2000) { this.pool.push({ t: 0, up: true }); this.acc.out -= 1; }
    while (this.acc.in >= 1 && this.pool.length < 2000) { this.pool.push({ t: 0, up: false }); this.acc.in -= 1; }
    const A = this.from.group.position, B = this.to.group.position;
    const mid = A.clone().add(B).multiplyScalar(0.5);
    const lift = Math.max(this.from.radius, this.to.radius) * 0.9;
    const pos = this.points.geometry.getAttribute("position") as THREE.BufferAttribute;
    const col = this.points.geometry.getAttribute("color") as THREE.BufferAttribute;
    const parr = pos.array as Float32Array, carr = col.array as Float32Array;
    const cOut = new THREE.Color(PALETTE.out), cIn = new THREE.Color(PALETTE.in);
    let k = 0;
    for (let i = this.pool.length - 1; i >= 0; i--) {
      const p = this.pool[i];
      p.t += dt / 3;
      if (p.t >= 1) { this.pool.splice(i, 1); continue; }
      const u = p.up ? p.t : 1 - p.t;
      const cy = mid.y + (p.up ? lift : -lift);
      const x = (1 - u) * (1 - u) * A.x + 2 * (1 - u) * u * mid.x + u * u * B.x;
      const y = (1 - u) * (1 - u) * A.y + 2 * (1 - u) * u * cy + u * u * B.y;
      const z = (1 - u) * (1 - u) * A.z + 2 * (1 - u) * u * mid.z + u * u * B.z + (p.up ? 1 : -1) * lift * 0.25 * Math.sin(u * Math.PI);
      parr[k * 3] = x; parr[k * 3 + 1] = y; parr[k * 3 + 2] = z;
      const c = p.up ? cOut : cIn;
      const f = Math.sin(p.t * Math.PI);
      carr[k * 3] = c.r * f; carr[k * 3 + 1] = c.g * f; carr[k * 3 + 2] = c.b * f;
      k++;
    }
    pos.needsUpdate = true; col.needsUpdate = true;
    this.points.geometry.setDrawRange(0, k);
  }
}

export class Scene {
  readonly canvas = document.createElement("canvas");
  private renderer!: THREE.WebGLRenderer;
  private scene = new THREE.Scene();
  private camera!: THREE.PerspectiveCamera;
  private controls!: OrbitControls;
  private tex = grainTexture();
  private glow = glowTexture();
  private frameCbs: (() => void)[] = [];
  private time = 0;
  private baseDist = 800;
  private placed = false;
  blobs = new Map<string, Blob>();
  streams: Stream[] = [];
  onPick: (id: string) => void = () => {};
  dragged = false;

  async init(el: HTMLElement) {
    this.renderer = new THREE.WebGLRenderer({ canvas: this.canvas, antialias: true, alpha: false });
    this.renderer.setPixelRatio(Math.min(3, window.devicePixelRatio || 1));
    this.renderer.setClearColor(0x0a0c11);
    el.appendChild(this.canvas);
    this.camera = new THREE.PerspectiveCamera(50, 1, 1, 20000);
    this.camera.position.set(0, 120, this.baseDist);
    this.controls = new OrbitControls(this.camera, this.canvas);
    this.controls.enableDamping = true;
    this.controls.dampingFactor = 0.08;
    this.controls.minDistance = 8;   // close enough to pass between layers
    this.controls.maxDistance = 4000;
    this.controls.rotateSpeed = 0.6;
    this.controls.zoomSpeed = 0.8;
    const resize = () => {
      this.renderer.setSize(window.innerWidth, window.innerHeight, false);
      this.camera.aspect = window.innerWidth / window.innerHeight;
      this.camera.updateProjectionMatrix();
    };
    window.addEventListener("resize", resize);
    resize();
    this.installPicking();
    let last = performance.now();
    const loop = () => {
      const now = performance.now();
      const dt = Math.min(0.1, (now - last) / 1000);
      last = now;
      this.time += dt;
      this.controls.update();
      for (const s of this.streams) s.tick(dt);
      for (const b of this.blobs.values()) b.tick(dt, this.time);
      this.renderer.render(this.scene, this.camera);
      for (const cb of this.frameCbs) cb();
      requestAnimationFrame(loop);
    };
    requestAnimationFrame(loop);
  }

  onFrame(cb: () => void) { this.frameCbs.push(cb); }

  // How far in the viewer has come, relative to the starting distance; the page reveals detail past ~1.8.
  get zoom() { return this.baseDist / Math.max(1, this.camera.position.distanceTo(this.controls.target)); }

  private installPicking() {
    let down: { x: number; y: number; t: number } | null = null;
    this.canvas.addEventListener("pointerdown", (e) => { down = { x: e.clientX, y: e.clientY, t: performance.now() }; this.dragged = false; });
    this.canvas.addEventListener("pointermove", (e) => { if (down && Math.hypot(e.clientX - down.x, e.clientY - down.y) > 6) this.dragged = true; });
    this.canvas.addEventListener("pointerup", (e) => {
      if (!down) return;
      const tap = !this.dragged && performance.now() - down.t < 400;
      down = null;
      if (!tap) return;
      const id = this.hitTest(e.clientX, e.clientY);
      if (id) this.onPick(id);
    });
    this.canvas.addEventListener("dblclick", () => { this.controls.reset(); });
  }

  hitTest(clientX: number, clientY: number): string | null {
    const rect = this.canvas.getBoundingClientRect();
    const ndc = new THREE.Vector2(((clientX - rect.left) / rect.width) * 2 - 1, -((clientY - rect.top) / rect.height) * 2 + 1);
    const ray = new THREE.Raycaster();
    ray.setFromCamera(ndc, this.camera);
    const hits = ray.intersectObjects([...this.blobs.values()].map((b) => b.group.children.find((c) => c.userData.id)!), false);
    return hits.length ? (hits[0].object.userData.id as string) : null;
  }

  private layout() {
    const list = [...this.blobs.values()];
    const gap = Math.max(...list.map((b) => b.radius)) * 2.8;
    const total = gap * (list.length - 1);
    list.forEach((b, i) => b.group.position.set(-total / 2 + gap * i, 0, 0));
    this.baseDist = Math.max(600, total * 0.9 + gap);
    // The starting view is set once, on the first layout; snapshots arrive every second and must not move it.
    if (!this.placed) { this.placed = true; this.camera.position.set(0, this.baseDist * 0.42, this.baseDist * 0.9); this.controls.saveState(); }
  }

  apply(s: Snapshot) {
    for (const n of s.nodes) {
      let b = this.blobs.get(n.id);
      if (!b) {
        b = new Blob(n, this.tex, this.glow);
        this.blobs.set(n.id, b);
        this.scene.add(b.group);
      }
      b.layers = n.layers ?? null;
      b.resize(n);
      b.activity = this.activityOf(n, s.links);
      const server = s.nodes.find((x) => x.kind === "llama-server");
      b.ctxFill = server?.slot && s.model.n_ctx ? Math.min(1, server.slot.n_prompt / s.model.n_ctx) : 0;
    }
    for (const id of [...this.blobs.keys()]) if (!s.nodes.some((n) => n.id === id)) { this.scene.remove(this.blobs.get(id)!.group); this.blobs.delete(id); }
    this.layout();
    for (const l of s.links) {
      let st = this.streams.find((x) => x.from.id === l.from && x.to.id === l.to);
      if (!st) {
        const a = this.blobs.get(l.from), b = this.blobs.get(l.to);
        if (!a || !b) continue;
        st = new Stream(this.tex, a, b);
        this.streams.push(st);
        this.scene.add(st.points);
      }
      st.rateOut = l.bytes_out_per_s;
      st.rateIn = l.bytes_in_per_s;
    }
  }

  private activityOf(n: Node, links: Link[]): number {
    if (n.kind === "llama-server") return (n.requests_processing ?? 0) > 0 || (n.tokens_per_s ?? 0) > 0 ? 1 : 0;
    const flow = links.filter((l) => l.to === n.id).reduce((a, l) => a + l.bytes_out_per_s + l.bytes_in_per_s, 0);
    return Math.min(1, flow / 5e5);
  }

  // Where the camera is inside a node, if anywhere: the layer whose shell it is passing, or the core.
  depth(): { id: string; layer: number | null; core: boolean } | null {
    for (const b of this.blobs.values()) {
      const d = this.camera.position.distanceTo(b.group.position);
      const R = b.radius;
      if (d >= R) continue;
      if (d < R * 0.34) return { id: b.id, layer: null, core: true };
      const count = b.layers ? b.layers[1] - b.layers[0] + 1 : 1;
      const idx = Math.min(count - 1, Math.floor((d - R * 0.34) / ((R * 0.66) / count)));
      return { id: b.id, layer: (b.layers ? b.layers[0] : 0) + idx, core: false };
    }
    return null;
  }

  screenPos(id: string): { x: number; y: number; r: number } | null {
    const b = this.blobs.get(id);
    if (!b) return null;
    const rect = this.canvas.getBoundingClientRect();
    const v = b.group.position.clone().project(this.camera);
    if (v.z > 1) return null;
    const dist = this.camera.position.distanceTo(b.group.position);
    const r = b.radius * (rect.height / 2) / (dist * Math.tan((this.camera.fov * Math.PI) / 360));
    return { x: rect.left + (v.x + 1) / 2 * rect.width, y: rect.top + (1 - v.y) / 2 * rect.height, r };
  }
}
