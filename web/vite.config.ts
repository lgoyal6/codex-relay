import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// The build output is embedded into the Go binary, so everything must be local:
// no CDN, no external fonts, no remote analytics.
export default defineConfig({
  plugins: [react()],
  build: {
    outDir: "../internal/httpapi/dist",
    emptyOutDir: true,
    assetsInlineLimit: 0,
    rollupOptions: { output: { manualChunks: undefined } },
  },
});
