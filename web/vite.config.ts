import { fileURLToPath } from "node:url";
import { defineConfig } from "vite";

// base "./" so the page works at / and behind HAProxy at /llamesh/.
export default defineConfig({
  base: "./",
  // Bundle Pixi from its prebuilt single-file ESM. Rollup re-bundling the package's source modules produced
  // either an Application.init that never resolved (chunked) or a "cannot access before initialization" error
  // (dynamic imports inlined): Pixi's internal module cycle survives only in its own build.
  resolve: { alias: { "pixi.js": fileURLToPath(new URL("./node_modules/pixi.js/dist/pixi.mjs", import.meta.url)) } },
  build: { outDir: "dist", emptyOutDir: true, target: "es2022" },
  server: { proxy: { "/api": "http://127.0.0.1:8899" } },
});
