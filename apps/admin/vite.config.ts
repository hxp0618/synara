// FILE: vite.config.ts
// Purpose: Build and locally proxy the independent Platform Admin application.
// Layer: Admin build configuration

import react from "@vitejs/plugin-react";
import { defineConfig } from "vite";

const port = Number(process.env.PORT ?? process.env.SYNARA_ADMIN_PORT ?? 5743);
const controlPlaneTarget =
  process.env.VITE_CONTROL_PLANE_PROXY_TARGET ??
  process.env.SYNARA_CONTROL_PLANE_URL ??
  "http://127.0.0.1:3780";

export default defineConfig({
  plugins: [react()],
  server: {
    host: "127.0.0.1",
    port,
    strictPort: true,
    proxy: {
      "/v1": {
        target: controlPlaneTarget,
        changeOrigin: false,
      },
    },
  },
  preview: {
    host: "127.0.0.1",
    port,
    strictPort: true,
  },
  build: {
    outDir: "dist",
    emptyOutDir: true,
    sourcemap: false,
  },
});
