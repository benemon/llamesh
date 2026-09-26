import { source } from "./data";
import { Scene } from "./scene";
import { UI } from "./ui";

window.addEventListener("error", (e) => console.error("llamesh:", e.message));
window.addEventListener("unhandledrejection", (e) => console.error("llamesh: unhandled", e.reason));

const scene = new Scene();
await scene.init(document.getElementById("stage")!);
const ui = new UI(scene);
source()((s) => { scene.apply(s); ui.apply(s); }, (ok) => ui.connected(ok));
