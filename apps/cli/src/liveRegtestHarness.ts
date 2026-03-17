import { mkdtemp } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

import { resolveCliRuntimeConfig } from "./config.js";
import { runHarness, loadHarnessScenario } from "./harness.js";
import { CliLogger } from "./logger.js";

async function main() {
  const baseDir = await mkdtemp(join(tmpdir(), "parker-live-regtest-"));
  const config = resolveCliRuntimeConfig({
    "daemon-dir": join(baseDir, "daemons"),
    network: "regtest",
    "profile-dir": join(baseDir, "profiles"),
    "run-dir": join(baseDir, "runs"),
  });
  const logger = new CliLogger(true);
  const scenario = await loadHarnessScenario(
    fileURLToPath(new URL("../examples/regtest-nigiri-live.json", import.meta.url)),
  );

  try {
    await runHarness({
      config,
      logger,
      scenario,
    });
    logger.result({
      baseDir,
      status: "completed",
    });
  } catch (error) {
    logger.error("live regtest harness failed", {
      baseDir,
      error: (error as Error).message,
    });
    throw error;
  }
}

void main().catch((error) => {
  process.stderr.write(`${(error as Error).message}\n`);
  process.exitCode = 1;
});
