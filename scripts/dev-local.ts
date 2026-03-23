import { spawn, spawnSync, type ChildProcess } from "node:child_process";
import { homedir } from "node:os";
import { join } from "node:path";

const network = process.env.PARKER_NETWORK ?? "regtest";
const startNigiri = network === "regtest";
const daemonLauncher = "./scripts/bin/parker-daemon";
const nigiriLauncher = "./scripts/bin/nigiri";
const childProcesses = new Set<ChildProcess>();
const nigiriDatadir =
  process.env.PARKER_NIGIRI_DATADIR ??
  join(homedir(), "Library", "Application Support", "Nigiri", "parker-dev-local");
const indexerUrl = process.env.PARKER_INDEXER_URL ?? process.env.VITE_INDEXER_URL ?? "http://127.0.0.1:3020";
const sharedEnv = {
  ...process.env,
  PARKER_INDEXER_URL: indexerUrl,
  VITE_INDEXER_URL: process.env.VITE_INDEXER_URL ?? indexerUrl,
  ...(startNigiri ? { PARKER_NIGIRI_DATADIR: nigiriDatadir } : {}),
};

async function main() {
  if (startNigiri) {
    stopNigiri();
    await runBlockingProcess("Nigiri", nigiriLauncher, ["--datadir", nigiriDatadir, "start", "--ark", "--ln", "--ci"]);
  }

  startLongRunningProcess("Indexer", "npm", ["run", "dev:indexer"]);
  startLongRunningProcess("Controller", "npm", ["run", "dev:controller"]);
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

function startLongRunningProcess(label: string, command: string, args: string[]) {
  const child = spawn(command, args, {
    cwd: process.cwd(),
    env: sharedEnv,
    stdio: ["ignore", "pipe", "pipe"],
  });
  childProcesses.add(child);

  child.stdout?.on("data", (chunk: Buffer | string) => {
    writePrefixed(label, chunk.toString());
  });
  child.stderr?.on("data", (chunk: Buffer | string) => {
    writePrefixed(label, chunk.toString());
  });
  child.on("exit", (code, signal) => {
    childProcesses.delete(child);
    process.stdout.write(`[${label}] exited (${signal ?? code ?? "unknown"})\n`);
  });
}

function runBlockingProcess(label: string, command: string, args: string[]) {
  return new Promise<void>((resolve, reject) => {
    const child = spawn(command, args, {
      cwd: process.cwd(),
      env: sharedEnv,
      stdio: ["ignore", "pipe", "pipe"],
    });
    childProcesses.add(child);

    child.stdout?.on("data", (chunk: Buffer | string) => {
      writePrefixed(label, chunk.toString());
    });
    child.stderr?.on("data", (chunk: Buffer | string) => {
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

function writePrefixed(label: string, text: string) {
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
  process.stderr.write(`${(error as Error).message}\n`);
  process.exitCode = 1;
});
