import { resolve } from "node:path";

import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

export default defineConfig({
  envDir: resolve(__dirname, "../.."),
  plugins: [react()],
  server: {
    host: "0.0.0.0",
    port: 3010,
    headers: {
      "Service-Worker-Allowed": "/",
    },
  },
  build: {
    rollupOptions: {
      input: {
        main: resolve(__dirname, "index.html"),
        "wallet-service-worker": resolve(__dirname, "src/wallet-service-worker.ts"),
      },
      output: {
        entryFileNames(chunkInfo) {
          if (chunkInfo.name === "wallet-service-worker") {
            return "[name].js";
          }
          return "assets/[name]-[hash].js";
        },
        chunkFileNames: "assets/[name]-[hash].js",
        assetFileNames: "assets/[name]-[hash][extname]",
      },
    },
  },
});
