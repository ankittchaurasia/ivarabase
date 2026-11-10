import { defineConfig, type ProxyOptions } from "vite";
import react from "@vitejs/plugin-react";

// `npm run dev` serves the UI on :5173 and proxies API calls to a running
// `ivarabase serve` (default 127.0.0.1:8787, override with IVARA_DEV_API).
const api = process.env.IVARA_DEV_API || "http://127.0.0.1:8787";

const proxy: ProxyOptions = {
  target: api,
  configure: (p) => {
    // When the Go server is not running, answer with a normal IvaraBase error
    // so the UI can say so instead of showing a blank page.
    p.on("error", (_err, _req, res) => {
      const r = res as import("http").ServerResponse;
      if (!r.headersSent && typeof r.writeHead === "function") {
        r.writeHead(502, { "Content-Type": "application/json" });
        r.end(
          JSON.stringify({
            error: {
              code: "server_unreachable",
              message: `The IvaraBase server is not running at ${api}`,
              hint: "Start it in another terminal with `./bin/ivarabase serve`, or use `make dev` to run server and UI together.",
            },
          }),
        );
      }
    });
  },
};

export default defineConfig({
  plugins: [react()],
  base: "/",
  build: {
    outDir: "../static",
    emptyOutDir: true,
    chunkSizeWarningLimit: 1200,
    rollupOptions: {
      output: {
        manualChunks: { editor: ["codemirror", "@codemirror/lang-sql", "@codemirror/lang-javascript", "@codemirror/lang-json"] },
      },
    },
  },
  server: {
    // trailing slashes: only API paths, never UI pages that happen to start with the same letters
    proxy: { "/api/": proxy, "/mcp": proxy, "/fn/": proxy },
  },
});
