import { defineConfig } from "vite";

// base "./" so the page works at / and behind HAProxy at /llamesh/.
export default defineConfig({
  base: "./",
  build: { outDir: "dist", emptyOutDir: true, target: "es2022" },
  server: { proxy: { "/api": "http://127.0.0.1:8899" } },
});
