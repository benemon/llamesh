window.addEventListener("error", (e) => console.error("llamesh:", e.message));
window.addEventListener("unhandledrejection", (e) => console.error("llamesh: unhandled", e.reason));
import { source } from "./data";
import { Scene } from "./scene";
import { UI } from "./ui";

const scene = new Scene();
console.log("llamesh: init");
await scene.init(document.getElementById("stage")!);
console.log("llamesh: renderer three");
const ui = new UI(scene);
(window as unknown as { llamesh: unknown }).llamesh = { scene, ui };  // for poking at it from the console
source()((s) => { scene.apply(s); ui.apply(s); }, (ok) => ui.connected(ok));
