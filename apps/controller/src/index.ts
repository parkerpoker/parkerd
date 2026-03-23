import { fileURLToPath } from "node:url";

import { createControllerApp } from "./app.js";

const DEFAULT_CONTROLLER_PORT = 3030;

async function main() {
  const port = Number(process.env.PARKER_CONTROLLER_PORT ?? DEFAULT_CONTROLLER_PORT);
  const webDistDir = fileURLToPath(new URL("../../web/dist", import.meta.url));
  const { app, allowedOrigins } = await createControllerApp({
    controllerPort: port,
    webDistDir,
  });

  await app.listen({
    host: "127.0.0.1",
    port,
  });

  process.stdout.write(
    `parker controller listening on http://127.0.0.1:${port} (allowed origins: ${allowedOrigins.join(", ")})\n`,
  );
}

void main().catch((error) => {
  process.stderr.write(`${(error as Error).message}\n`);
  process.exitCode = 1;
});
