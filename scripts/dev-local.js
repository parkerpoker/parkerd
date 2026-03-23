import { spawn, spawnSync } from "node:child_process";
import { homedir } from "node:os";
import { join } from "node:path";

const network = process.env.PARKER_NETWORK ?? "regtest";
const startNigiri = network === "regtest";
const daemonLauncher = "./scripts/bin/parker-daemon";
const controllerLauncher = "./scripts/bin/parker-controller";
const indexerLauncher = "./scripts/bin/parker-indexer";
const nigiriLauncher = "./scripts/bin/nigiri";
const childProcesses = new Set();
const nigiriDatadir =
  process.env.PARKER_NIGIRI_DATADIR ??
  join(homedir(), "Library", "Application Support", "Nigiri", "parker-dev-local");
const indexerURL = process.env.PARKER_INDEXER_URL ?? process.env.VITE_INDEXER_URL ?? "http://127.0.0.1:3020";
const sharedEnv = {
  ...process.env,
  PARKER_INDEXER_URL: indexerURL,
  VITE_INDEXER_URL: process.env.VITE_INDEXER_URL ?? indexerURL,
  ...(startNigiri ? { PARKER_NIGIRI_DATADIR: nigiriDatadir } : {}),
};

async function main() {
  if (startNigiri) {
    stopNigiri();
    await runBlockingProcess("Nigiri", nigiriLauncher, ["--datadir", nigiriDatadir, "start", "--ark", "--ln", "--ci"]);
  }

  startLongRunningProcess("Indexer", indexerLauncher, []);
  startLongRunningProcess("Controller", controllerLauncher, []);
  startLongRunningProcess("Web", "npm", ["run", "dev:web"]);
  startLongRunningProcess("Host", daemonLauncher, ["--profile", "host", "--mode", "host"]);
  startLongRunningProcess("Witness", daemonLauncher, ["--profile", "witness", "--mode", "witness"]);
  startLongRunningProcess("Alice", daemonLauncher, ["--profile", "alice", "--mode", "player"]);
  startLongRunningProcess("Bob", daemonLauncher, ["--profile", "bob", "--mode", "player"]);

  process.stdout.write(
    `parker local dev stack running (network=${network}, nigiri=${startNigiri ? "started" : "skipped"})\n`,
  );

  const shutdown = () => {
    for (const child of childProcesses) {
      child.kill("SIGTERM");
    }
    childProcesses.clear();
    if (startNigiri) {
      stopNigiri();
    }
    process.exit(0);
  };

  process.on("SIGINT", shutdown);
  process.on("SIGTERM", shutdown);
}

function startLongRunningProcess(label, command, args) {
  const child = spawn(command, args, {
    cwd: process.cwd(),
    env: sharedEnv,
    stdio: ["ignore", "pipe", "pipe"],
  });
  childProcesses.add(child);

  child.stdout?.on("data", (chunk) => {
    writePrefixed(label, chunk.toString());
  });
  child.stderr?.on("data", (chunk) => {
    writePrefixed(label, chunk.toString());
  });
  child.on("exit", (code, signal) => {
    childProcesses.delete(child);
    process.stdout.write(`[${label}] exited (${signal ?? code ?? "unknown"})\n`);
  });
}

function runBlockingProcess(label, command, args) {
  return new Promise((resolve, reject) => {
    const child = spawn(command, args, {
      cwd: process.cwd(),
      env: sharedEnv,
      stdio: ["ignore", "pipe", "pipe"],
    });
    childProcesses.add(child);

    child.stdout?.on("data", (chunk) => {
      writePrefixed(label, chunk.toString());
    });
    child.stderr?.on("data", (chunk) => {
      writePrefixed(label, chunk.toString());
    });
    child.on("exit", (code, signal) => {
      childProcesses.delete(child);
      if (code === 0) {
        resolve();
        return;
      }
      reject(new Error(`${label} exited (${signal ?? code ?? "unknown"})`));
    });
  });
}

function writePrefixed(label, text) {
  for (const line of text.split(/\r?\n/).filter(Boolean)) {
    process.stdout.write(`[${label}] ${line}\n`);
  }
}

function stopNigiri() {
  spawnSync(nigiriLauncher, ["--datadir", nigiriDatadir, "stop"], {
    cwd: process.cwd(),
    env: sharedEnv,
    stdio: "ignore",
  });
}

void main().catch((error) => {
  process.stderr.write(`${error.message}\n`);
  process.exitCode = 1;
});
