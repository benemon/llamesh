import * as THREE from "three";
import { OrbitControls } from "three/addons/controls/OrbitControls.js";
import { Kind, layerCount, type Layers, type Link, type View, type ViewNode } from "./types";

const PARTICLE_BYTES = 4 * 1048576; // one grain per 4 MiB held
// Sizes are volumetric so bodies compare honestly: a 64 GiB body has radius 170, and two 32 GiB blobs
// together have its volume. A host's envelope is its device's memory; the blobs inside are what is held.
const radiusOf = (bytes: number) => Math.max(24, 170 * Math.cbrt(Math.max(bytes, 0) / (64 * 1073741824)));
// The primary is blue; every other node, RPC or another host's server, takes the next distinct hue.
const PRIMARY = 0x7fb7ff;
const NODE_COLOURS = [0xffb36b, 0x8ce99a, 0xf78fb3, 0xc3a6ff, 0xffe27a, 0x7fe3e0];
// Additive light on a dark ground, or ink on paper: the same hues, saturated, at a fixed lightness.
// Scaling the colour instead greys the pastel hues out.
let dark = true;
const ink = (hex: number, l: number) => {
  const hsl = { h: 0, s: 0, l: 0 };
  new THREE.Color(hex).getHSL(hsl);
  return new THREE.Color().setHSL(hsl.h, Math.max(hsl.s, 0.95), l).getHex();
};
const shade = (hex: number) => dark ? hex : ink(hex, 0.36);
const coreColour = (hex: number) => dark ? hex : ink(hex, 0.3);
const WHITE = new THREE.Color(0xffffff);
const ENVELOPE = () => dark ? 0x9fbbe0 : 0xcfc6b4;
const PAPER = new THREE.Color(0xfbf8f1);
const GROUND = () => dark ? 0x0a0c11 : PAPER.getHex();

// On paper, grains are solid dots that cover what is behind them, so an overlap never reads darker than one
// grain; meshes multiply, darkening what is under them by their colour, and premultiplied output makes a
// transparent fragment the identity.
function style(m: THREE.Material, onPaper: "flat" | "normal" | "multiply") {
  const flat = !dark && onPaper === "flat";
  m.alphaTest = flat ? 0.35 : 0;
  m.depthWrite = flat;
  m.premultipliedAlpha = false;
  if (dark) m.blending = THREE.AdditiveBlending;
  else if (onPaper === "multiply") {
    m.blending = THREE.CustomBlending;
    m.blendEquation = THREE.AddEquation;
    m.blendSrc = THREE.DstColorFactor;
    m.blendDst = THREE.OneMinusSrcAlphaFactor;
    m.premultipliedAlpha = true;
  } else m.blending = THREE.NormalBlending;
  m.needsUpdate = true;
}

// A radial white falloff: the grain sprite (small, sharp) and the glow (large, soft).
function radialTexture(size: number, stops: [number, number][]): THREE.Texture {
  const c = document.createElement("canvas");
  c.width = c.height = size;
  const ctx = c.getContext("2d")!;
  const g = ctx.createRadialGradient(size / 2, size / 2, 0, size / 2, size / 2, size / 2);
  for (const [at, alpha] of stops) g.addColorStop(at, `rgba(255,255,255,${alpha})`);
  ctx.fillStyle = g;
  ctx.fillRect(0, 0, size, size);
  const t = new THREE.CanvasTexture(c);
  t.colorSpace = THREE.SRGBColorSpace;
  return t;
}

function points(tex: THREE.Texture, color: number, size: number, capacity: number): THREE.Points {
  const geo = new THREE.BufferGeometry();
  geo.setAttribute("position", new THREE.BufferAttribute(new Float32Array(capacity * 3), 3));
  const mat = new THREE.PointsMaterial({ map: tex, color, size, transparent: true, opacity: 0.5, depthWrite: false, sizeAttenuation: true });
  const p = new THREE.Points(geo, mat);
  p.frustumCulled = false;
  return p;
}

// A node is a sphere of concentric shells, one per transformer layer it holds, the first layer innermost:
// one body from afar, separate shells once the camera is among them. The context cache is the core.
class Blob {
  readonly group = new THREE.Group();
  readonly id: string;
  colour: number;
  private grains: THREE.Points;
  private core: THREE.Points;     // the context in use: a dense ball of the same grains, volume proportional to tokens
  private coreBase: { r: number; theta: number; phi: number }[] = [];
  private capacity: THREE.Mesh;   // faint shell at the size the context would be when full
  private coreGlow: THREE.Sprite;
  private halo: THREE.Sprite;
  private pick: THREE.Mesh;
  private base: { theta: number; phi: number; w: number; jitter: number; layer: number }[] = [];
  radius = 60;
  sourceKey = "";
  dim = 1;        // 1 lit, towards 0 faded: another source is being explored
  private dimShown = 1;
  private coreCap = 20;   // radius of the context core when the window is full: volume ~ the node's context bytes
  layers: Layers | null = null;
  activity = 0;
  ctxFill = 0;
  coreR = 0;     // the context core's radius this frame
  camDist = 1e9; // camera distance to this node's centre, set by the scene each frame
  private shown = 0;
  private spin = 0;

  constructor(node: ViewNode, index: number, tex: THREE.Texture, glow: THREE.Texture) {
    this.id = node.id;
    const tint = node.primary ? PRIMARY : NODE_COLOURS[index % NODE_COLOURS.length];
    this.colour = tint;
    this.grains = points(tex, shade(tint), 3.2, 30000);
    // The same grains as the shells, packed densely enough to read as a body.
    this.core = points(tex, coreColour(tint), 2.6, 8000);
    (this.core.material as THREE.PointsMaterial).opacity = 0.75;
    for (let i = 0; i < 8000; i++) this.coreBase.push({ r: Math.cbrt(Math.random()), theta: Math.random() * Math.PI * 2, phi: Math.acos(2 * Math.random() - 1) });
    this.capacity = new THREE.Mesh(new THREE.SphereGeometry(1, 48, 32), new THREE.MeshBasicMaterial({ color: shade(tint), transparent: true, opacity: 0.04, depthWrite: false, side: THREE.DoubleSide }));
    this.coreGlow = new THREE.Sprite(new THREE.SpriteMaterial({ map: glow, color: 0xffd9a8, transparent: true, opacity: 0.18, blending: THREE.AdditiveBlending, depthWrite: false }));
    this.halo = new THREE.Sprite(new THREE.SpriteMaterial({ map: glow, color: tint, transparent: true, opacity: 0.35, blending: THREE.AdditiveBlending, depthWrite: false }));
    this.pick = new THREE.Mesh(new THREE.SphereGeometry(1, 12, 12), new THREE.MeshBasicMaterial({ visible: false }));
    this.pick.userData.id = node.id;
    this.group.add(this.halo, this.grains, this.capacity, this.coreGlow, this.core, this.pick);
    this.resize(node);
    this.retheme();
  }

  // A node's role can change after its blob exists, so the tint follows the snapshot.
  tint(colour: number) {
    if (colour === this.colour) return;
    this.colour = colour;
    this.retheme();
  }

  retheme() {
    (this.grains.material as THREE.PointsMaterial).color.setHex(shade(this.colour));
    (this.core.material as THREE.PointsMaterial).color.setHex(coreColour(this.colour));
    (this.capacity.material as THREE.MeshBasicMaterial).color.setHex(shade(this.colour));
    style(this.grains.material as THREE.Material, "flat");
    style(this.core.material as THREE.Material, "normal");
    style(this.capacity.material as THREE.Material, "multiply");
    (this.halo.material as THREE.SpriteMaterial).color.setHex(this.colour);
    this.halo.visible = this.coreGlow.visible = dark; // a glow on a light ground washes out what is under it
  }

  resize(node: ViewNode) {
    const held = node.mem_model + node.mem_context + node.mem_compute;
    const want = Math.max(400, Math.min(30000, Math.round(held / PARTICLE_BYTES)));
    this.radius = radiusOf(held);
    const count = layerCount(this.layers);
    while (this.base.length < want) {
      // uniform on a sphere: theta around the axis, phi from the pole
      this.base.push({ theta: Math.random() * Math.PI * 2, phi: Math.acos(2 * Math.random() - 1), w: 0.6 + Math.random() * 0.8, jitter: (Math.random() - 0.5) * 0.35, layer: 0 });
    }
    this.base.length = want;
    this.base.forEach((g, i) => { g.layer = Math.min(count - 1, Math.floor(i * count / want)); });
    this.grains.geometry.setDrawRange(0, want);
    // capacity core radius from the node's context bytes: 1 GiB -> 22 units, volume proportional to bytes
    this.coreCap = Math.min(this.radius * 0.22, 22 * Math.cbrt(Math.max(node.mem_context, 1) / 1073741824));
    this.capacity.scale.setScalar(this.coreCap);
    this.halo.scale.set(this.radius * 3.4, this.radius * 3.4, 1);
    this.pick.scale.setScalar(this.radius * 1.1);
  }

  tick(dt: number, time: number) {
    this.shown += (this.activity - this.shown) * Math.min(1, dt / 3);
    this.dimShown += (this.dim - this.dimShown) * Math.min(1, dt / 0.35);
    const dim = this.dimShown;
    const a = this.shown;
    const R = this.radius * (1 - 0.25 * a);
    const count = layerCount(this.layers);
    this.spin += dt * 0.05 * (1 + a * 0.6);
    const pos = this.grains.geometry.getAttribute("position") as THREE.BufferAttribute;
    const arr = pos.array as Float32Array;
    const n = this.base.length;
    const shell = (R * 0.76) / count; // shells from 0.24R (outside the core) to R, evenly spaced
    for (let i = 0; i < n; i++) {
      const g = this.base[i];
      const th = g.theta + this.spin * g.w;
      const wob = Math.sin(time * 0.25 + i) * 0.04;
      const r = R * 0.24 + shell * (g.layer + 0.5 + g.jitter) + R * wob;
      const sp = Math.sin(g.phi);
      arr[i * 3] = r * sp * Math.cos(th);
      arr[i * 3 + 1] = r * Math.cos(g.phi);
      arr[i * 3 + 2] = r * sp * Math.sin(th);
    }
    pos.needsUpdate = true;
    // Solid ink is discarded below the alpha test, so on paper a dimmed grain pales towards the paper instead.
    const gm = this.grains.material as THREE.PointsMaterial;
    if (dark) gm.opacity = (0.32 + 0.3 * a) * dim;
    else { gm.opacity = 1; gm.color.setHex(shade(this.colour)).lerp(PAPER, 1 - dim); }
    // the core's volume is the context in use; it can only grow to the capacity shell
    const rc = this.coreCap * Math.cbrt(Math.max(0.01, this.ctxFill));
    this.coreR = rc;
    const cpos = this.core.geometry.getAttribute("position") as THREE.BufferAttribute;
    const carr = cpos.array as Float32Array;
    for (let i = 0; i < this.coreBase.length; i++) {
      const c = this.coreBase[i];
      const r = rc * c.r;
      const th = c.theta + this.spin * 0.4;
      const sp = Math.sin(c.phi);
      carr[i * 3] = r * sp * Math.cos(th);
      carr[i * 3 + 1] = r * Math.cos(c.phi);
      carr[i * 3 + 2] = r * sp * Math.sin(th);
    }
    cpos.needsUpdate = true;
    const f = Math.min(1, Math.max(0, this.ctxFill));
    if (dark) {
      (this.core.material as THREE.PointsMaterial).color.setHex(this.colour).multiplyScalar(0.3 + 0.7 * f).lerp(WHITE, 0.35 * f * f);
    }
    // Point sprites grow with proximity; up close they would merge into a wall. Shrink them as the camera
    // approaches and fade the glows away once it is inside, so what is left is grains around the viewer.
    const near = Math.min(1, Math.max(0.12, this.camDist / (R * 1.5)));
    const nearCore = Math.min(1, Math.max(0.1, this.camDist / (rc * 4)));
    (this.grains.material as THREE.PointsMaterial).size = 3.2 * near * (dark ? 1 : 1.3);
    (this.core.material as THREE.PointsMaterial).size = 2.6 * nearCore;
    // Packed, the core fades harder than the shells. Additive grains this dense saturate to white; the fill
    // sets how far they build.
    (this.core.material as THREE.PointsMaterial).opacity = (0.6 + 0.3 * a + 0.03 * Math.sin(time * 1.5)) * (0.4 + 0.6 * nearCore) * dim * dim * (dark ? 0.06 + 0.3 * f : 0.8);
    this.coreGlow.scale.set(rc * 3, rc * 3, 1);
    (this.coreGlow.material as THREE.SpriteMaterial).opacity = (0.1 + 0.15 * a) * Math.min(1, Math.max(0, (this.camDist - rc * 2) / (rc * 4))) * dim;
    (this.halo.material as THREE.SpriteMaterial).opacity = (0.22 + 0.25 * a) * Math.min(1, Math.max(0, (this.camDist - R * 0.6) / (R * 1.4))) * dim;
    (this.capacity.material as THREE.MeshBasicMaterial).opacity = 0.04 * dim;
  }
}

// Grains travelling along an arc between two nodes: out above, in below, at a rate set by bytes per second.
class Stream {
  readonly points: THREE.Points;
  private pool: { t: number; up: boolean }[] = [];
  private acc = { out: 0, in: 0 };
  rateOut = 0;
  rateIn = 0;
  constructor(tex: THREE.Texture, readonly from: Blob, readonly to: Blob) {
    this.points = points(tex, 0xffffff, 4.5, 2000);
    this.retheme();
    (this.points.material as THREE.PointsMaterial).opacity = 0.9;
    this.points.geometry.setAttribute("color", new THREE.BufferAttribute(new Float32Array(2000 * 3), 3));
    (this.points.material as THREE.PointsMaterial).vertexColors = true;
  }

  retheme() {
    style(this.points.material as THREE.Material, "flat");
  }

  tick(dt: number) {
    const mat = this.points.material as THREE.PointsMaterial;
    const dim = Math.min(this.from.dim, this.to.dim);
    mat.opacity = dark ? 0.9 * dim : 0.9;
    mat.size = 0.025 * (this.from.radius + this.to.radius) * (dark ? 1 : 1.4); // grains sized with the bodies they join, so the arc reads at any layout scale
    // 1 MB/s ~ 60 grains/s over a 3 s flight: the ~1 MB/s a three-node link carries in generation reads as
    // a steady thread; a prefill burst saturates at the cap
    this.acc.out += dt * Math.min(300, 60 * this.rateOut / 1e6);
    this.acc.in += dt * Math.min(300, 60 * this.rateIn / 1e6);
    while (this.acc.out >= 1 && this.pool.length < 2000) { this.pool.push({ t: 0, up: true }); this.acc.out -= 1; }
    while (this.acc.in >= 1 && this.pool.length < 2000) { this.pool.push({ t: 0, up: false }); this.acc.in -= 1; }
    const A = this.from.group.position, B = this.to.group.position;
    const mid = A.clone().add(B).multiplyScalar(0.5);
    const lift = Math.max(this.from.radius, this.to.radius) * 0.9;
    const pos = this.points.geometry.getAttribute("position") as THREE.BufferAttribute;
    const col = this.points.geometry.getAttribute("color") as THREE.BufferAttribute;
    const parr = pos.array as Float32Array, carr = col.array as Float32Array;
    const cOut = new THREE.Color(shade(this.from.colour)), cIn = new THREE.Color(shade(this.to.colour));
    if (!dark) { cOut.lerp(PAPER, 1 - dim); cIn.lerp(PAPER, 1 - dim); }
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
      // Solid ink cannot fade out, so on paper a thread ends where it enters a core instead.
      if (!dark && (Math.hypot(x - A.x, y - A.y, z - A.z) < this.from.coreR || Math.hypot(x - B.x, y - B.y, z - B.z) < this.to.coreR)) continue;
      parr[k * 3] = x; parr[k * 3 + 1] = y; parr[k * 3 + 2] = z;
      const c = p.up ? cOut : cIn;
      const f = dark ? Math.sin(p.t * Math.PI) : 1;
      carr[k * 3] = c.r * f; carr[k * 3 + 1] = c.g * f; carr[k * 3 + 2] = c.b * f;
      k++;
    }
    pos.needsUpdate = true; col.needsUpdate = true;
    this.points.geometry.setDrawRange(0, k);
  }
}

// A machine: a faint shell at the size of its device's memory, with the blobs it holds arranged inside.
class Host {
  readonly mesh: THREE.Mesh;
  radius = 24;
  dim = 1;
  private dimShown = 1;
  constructor(readonly id: string) {
    this.mesh = new THREE.Mesh(new THREE.SphereGeometry(1, 64, 40), new THREE.MeshBasicMaterial({ color: ENVELOPE(), transparent: true, opacity: 0.035, depthWrite: false, side: THREE.DoubleSide }));
    this.retheme();
  }
  retheme() {
    const m = this.mesh.material as THREE.MeshBasicMaterial;
    m.color.setHex(ENVELOPE()); style(m, "multiply");
  }
  resize(memTotal: number) {
    this.radius = radiusOf(memTotal);
    this.mesh.scale.setScalar(this.radius);
  }
  tick(dt: number) {
    this.dimShown += (this.dim - this.dimShown) * Math.min(1, dt / 0.35);
    (this.mesh.material as THREE.MeshBasicMaterial).opacity = (dark ? 0.035 : 0.07) * (0.25 + 0.75 * this.dimShown);
  }
}

export class Scene {
  readonly canvas = document.createElement("canvas");
  private renderer!: THREE.WebGLRenderer;
  private scene = new THREE.Scene();
  private camera!: THREE.PerspectiveCamera;
  private controls!: OrbitControls;
  private tex = radialTexture(64, [[0, 1], [0.5, 0.8], [1, 0]]);
  private glow = radialTexture(256, [[0, 0.7], [0.4, 0.18], [1, 0]]);
  private frameCbs: (() => void)[] = [];
  private time = 0;
  private baseDist = 800;
  private placed = false;
  blobs = new Map<string, Blob>();
  private hosts = new Map<string, Host>();
  private hostOf = new Map<string, string>(); // blob id -> host id
  private focused: string | null = null;
  private glideTo: THREE.Vector3 | null = null;
  private streams: Stream[] = [];
  onPick: (id: string) => void = () => {};
  dragged = false;

  async init(el: HTMLElement) {
    this.renderer = new THREE.WebGLRenderer({ canvas: this.canvas, antialias: true, alpha: false });
    this.renderer.setPixelRatio(Math.min(3, window.devicePixelRatio || 1));
    this.renderer.setClearColor(GROUND());
    el.appendChild(this.canvas);
    this.scene.add(new THREE.HemisphereLight(0xffffff, 0x223044, 0.9));
    const key = new THREE.DirectionalLight(0xffffff, 1.4);
    key.position.set(-0.6, 1, 0.8);
    this.scene.add(key);
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
      if (this.glideTo) {
        const step = this.glideTo.clone().sub(this.controls.target).multiplyScalar(Math.min(1, dt * 6));
        this.controls.target.add(step);
        this.camera.position.add(step);
        if (this.controls.target.distanceTo(this.glideTo) < 0.5) this.glideTo = null;
      }
      this.controls.update();
      for (const s of this.streams) s.tick(dt);
      for (const b of this.blobs.values()) { b.camDist = this.camera.position.distanceTo(b.group.position); b.tick(dt, this.time); }
      for (const h of this.hosts.values()) h.tick(dt);
      this.renderer.render(this.scene, this.camera);
      for (const cb of this.frameCbs) cb();
      requestAnimationFrame(loop);
    };
    requestAnimationFrame(loop);
  }

  onFrame(cb: () => void) { this.frameCbs.push(cb); }

  theme(isDark: boolean) {
    dark = isDark;
    this.renderer.setClearColor(GROUND());
    for (const b of this.blobs.values()) b.retheme();
    for (const h of this.hosts.values()) h.retheme();
    for (const s of this.streams) s.retheme();
  }

  // Explore one server's picture: everything from other sources fades, and a host stays lit only while
  // it holds something of the focused source.
  focus(sourceKey: string | null) {
    this.focused = sourceKey;
    for (const b of this.blobs.values()) b.dim = sourceKey === null || b.sourceKey === sourceKey ? 1 : 0.12;
    for (const h of this.hosts.values()) h.dim = sourceKey === null || [...this.blobs.values()].some((b) => this.hostOf.get(b.id) === h.id && b.sourceKey === sourceKey) ? 1 : 0.12;
  }

  // Pans to the middle of one server's bodies, keeping the viewing angle and distance.
  centre(sourceKey: string) {
    const members = [...this.blobs.values()].filter((b) => b.sourceKey === sourceKey);
    if (members.length === 0) return;
    this.glideTo = members.reduce((c, b) => c.add(b.group.position), new THREE.Vector3()).divideScalar(members.length);
  }

  // How far in the viewer has come, relative to the starting distance; the page reveals detail past ~1.8.
  get zoom() { return this.baseDist / Math.max(1, this.camera.position.distanceTo(this.controls.target)); }

  private installPicking() {
    let down: { x: number; y: number; t: number } | null = null;
    this.canvas.addEventListener("pointerdown", (e) => { down = { x: e.clientX, y: e.clientY, t: performance.now() }; this.dragged = false; this.glideTo = null; });
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

  // Hosts on a sphere around the primary's, azimuth by the golden angle and elevation staggered, so two
  // are never collinear with the centre.
  private layout(primaryId: string | undefined) {
    const hostList = [...this.hosts.values()];
    const primaryHost = primaryId ? this.hosts.get(this.hostOf.get(primaryId)!) : undefined;
    const centre = primaryHost ?? hostList[0];
    const others = hostList.filter((h) => h !== centre);
    const gap = Math.max(...hostList.map((h) => h.radius), 1) * 2.6;
    centre?.mesh.position.set(0, 0, 0);
    const n = others.length;
    others.forEach((h, i) => {
      const az = i * 2.39996; // golden angle in radians
      const el = n > 1 ? (35 * Math.PI / 180) * (1 - (2 * i) / (n - 1)) : 0;
      h.mesh.position.set(Math.cos(el) * Math.cos(az) * gap, Math.sin(el) * gap, Math.cos(el) * Math.sin(az) * gap);
    });
    for (const h of hostList) {
      const members = [...this.blobs.values()].filter((b) => this.hostOf.get(b.id) === h.id);
      if (members.length === 1) { members[0].group.position.copy(h.mesh.position); continue; }
      members.forEach((b, i) => {
        const ring = Math.max(0, h.radius - b.radius); // tangent to the envelope from within: a small model sits out at the edge, a big one near the middle
        const az = (i / members.length) * Math.PI * 2;
        const el = (i % 2 ? -1 : 1) * 0.35;
        b.group.position.set(h.mesh.position.x + Math.cos(el) * Math.cos(az) * ring, h.mesh.position.y + Math.sin(el) * ring, h.mesh.position.z + Math.cos(el) * Math.sin(az) * ring);
      });
    }
    this.baseDist = Math.max(600, (n > 0 ? gap * 1.9 : 0) + (centre?.radius ?? 0) * 3.2);
    // The starting view is set once, on the first layout; snapshots arrive every second and must not move it.
    if (!this.placed) { this.placed = true; this.camera.position.set(0, this.baseDist * 0.42, this.baseDist * 0.9); this.controls.saveState(); }
  }

  apply(s: View) {
    for (const h of s.hosts) {
      let host = this.hosts.get(h.id);
      if (!host) { host = new Host(h.id); this.hosts.set(h.id, host); this.scene.add(host.mesh); }
      host.resize(h.mem_total);
    }
    for (const id of [...this.hosts.keys()]) if (!s.hosts.some((h) => h.id === id)) { this.scene.remove(this.hosts.get(id)!.mesh); this.hosts.delete(id); }
    for (const n of s.nodes) {
      this.hostOf.set(n.id, n.host);
      const index = Math.max(0, s.nodes.filter((x) => !x.primary).findIndex((x) => x.id === n.id));
      let b = this.blobs.get(n.id);
      if (!b) {
        b = new Blob(n, index, this.tex, this.glow);
        this.blobs.set(n.id, b);
        this.scene.add(b.group);
      }
      b.tint(n.primary ? PRIMARY : NODE_COLOURS[index % NODE_COLOURS.length]);
      b.sourceKey = n.sourceKey;
      b.layers = n.layers ?? null;
      b.resize(n);
      b.activity = this.activityOf(n, s.links);
      b.ctxFill = n.ctx_fill;
    }
    for (const id of [...this.blobs.keys()]) if (!s.nodes.some((n) => n.id === id)) { this.scene.remove(this.blobs.get(id)!.group); this.blobs.delete(id); this.hostOf.delete(id); }
    for (let i = this.streams.length - 1; i >= 0; i--) if (!this.blobs.has(this.streams[i].from.id) || !this.blobs.has(this.streams[i].to.id)) { this.scene.remove(this.streams[i].points); this.streams.splice(i, 1); }
    this.layout(s.nodes.find((n) => n.primary)?.id);
    this.focus(this.focused); // nodes may have come or gone
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

  private activityOf(n: ViewNode, links: Link[]): number {
    if (n.kind === Kind.KIND_LLAMA_SERVER) return (n.requests_processing ?? 0) > 0 || (n.tokens_per_s ?? 0) > 0 ? 1 : 0;
    if (n.kind === Kind.KIND_DEVICE) return n.server_slot?.processing ? 1 : 0; // busy when its server is
    const flow = links.filter((l) => l.to === n.id).reduce((a, l) => a + l.bytes_out_per_s + l.bytes_in_per_s, 0);
    return Math.min(1, flow / 5e5);
  }

  // Where the camera is inside a node, if anywhere: the layer whose shell it is passing, or the core.
  depth(): { id: string; layer: number | null; core: boolean } | null {
    for (const b of this.blobs.values()) {
      const d = this.camera.position.distanceTo(b.group.position);
      const R = b.radius;
      if (d >= R) continue;
      if (d < R * 0.24) return { id: b.id, layer: null, core: true };
      const count = layerCount(b.layers);
      const idx = Math.min(count - 1, Math.floor((d - R * 0.24) / ((R * 0.76) / count)));
      return { id: b.id, layer: (b.layers ? b.layers.first : 0) + idx, core: false };
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
