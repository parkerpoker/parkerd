import { loadEnvFile } from "node:process";
import { fileURLToPath } from "node:url";

import { createApp } from "./app.js";

try {
  loadEnvFile(fileURLToPath(new URL("../../../.env", import.meta.url)));
} catch {
  // Ignore missing local env files; explicit process env still wins.
}

const port = Number(process.env.PORT ?? 3020);
const host = process.env.HOST ?? "0.0.0.0";

createApp()
  .then(({ app }) => app.listen({ port, host }))
  .catch((error) => {
    console.error(error);
    process.exitCode = 1;
  });
