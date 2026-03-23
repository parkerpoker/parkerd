import { resolve } from "node:path";

import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

export default defineConfig({
  envDir: resolve(__dirname, "../.."),
  plugins: [react()],
  server: {
    host: "0.0.0.0",
    port: 3010,
  },
  build: {
    rollupOptions: {
      input: {
        main: resolve(__dirname, "index.html"),
      },
    },
  },
});
