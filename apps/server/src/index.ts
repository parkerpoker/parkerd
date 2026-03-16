import { loadEnvFile } from "node:process";

import type { Network } from "@parker/protocol";

import { createApp } from "./app.js";

loadEnvFile("../../.env");

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
