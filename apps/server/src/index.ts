import { loadEnvFile } from "node:process";
import { fileURLToPath } from "node:url";

import type { Network } from "@parker/protocol";

import { createApp } from "./app.js";

try {
  loadEnvFile(fileURLToPath(new URL("../../../.env", import.meta.url)));
} catch {
  // Ignore missing local env files; explicit process env still wins.
}

const port = Number(process.env.PORT ?? 3020);
const host = process.env.HOST ?? "0.0.0.0";
const network = (process.env.PARKER_NETWORK ?? "mutinynet") as Network;

createApp({
  network,
  websocketUrl: process.env.WEBSOCKET_URL ?? `ws://localhost:${port}/ws`,
})
  .then(({ app }) => app.listen({ port, host }))
  .catch((error) => {
    console.error(error);
    process.exitCode = 1;
  });
