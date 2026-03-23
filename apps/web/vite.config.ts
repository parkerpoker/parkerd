import { resolve } from "node:path";

import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

const CONTROLLER_TARGET = process.env.VITE_LOCAL_CONTROLLER_URL ?? "http://127.0.0.1:3030";
const INDEXER_TARGET = process.env.VITE_INDEXER_URL ?? "http://127.0.0.1:3020";

export default defineConfig({
  envDir: resolve(__dirname, "../.."),
  plugins: [react()],
  server: {
    host: "0.0.0.0",
    port: 3010,
    proxy: {
      "/api/local": {
        target: CONTROLLER_TARGET,
      },
      "/api/public": {
        target: INDEXER_TARGET,
      },
      "/health": {
        target: CONTROLLER_TARGET,
      },
    },
  },
  build: {
    rollupOptions: {
      input: {
        main: resolve(__dirname, "index.html"),
      },
    },
  },
});
