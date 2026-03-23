import { spawn, type ChildProcess } from "node:child_process";

const network = process.env.PARKER_NETWORK ?? "regtest";
const startNigiri = network === "regtest";
const childProcesses = new Set<ChildProcess>();

async function main() {
  if (startNigiri) {
    await runSetupCommand("Nigiri", "nigiri", ["start"]);
  }

  startLongRunningProcess("Indexer", "npm", ["run", "dev:indexer"]);
  startLongRunningProcess("Controller", "npm", ["run", "dev:controller"]);
  startLongRunningProcess("Web", "npm", ["run", "dev:web"]);
  startLongRunningProcess("Host", "node", ["--import", "tsx", "apps/daemon/src/index.ts", "--profile", "host", "--mode", "host"]);
  startLongRunningProcess("Witness", "node", ["--import", "tsx", "apps/daemon/src/index.ts", "--profile", "witness", "--mode", "witness"]);
  startLongRunningProcess("Alice", "node", ["--import", "tsx", "apps/daemon/src/index.ts", "--profile", "alice", "--mode", "player"]);
  startLongRunningProcess("Bob", "node", ["--import", "tsx", "apps/daemon/src/index.ts", "--profile", "bob", "--mode", "player"]);

  process.stdout.write(
    `parker local dev stack running (network=${network}, nigiri=${startNigiri ? "started" : "skipped"})\n`,
  );

  const shutdown = () => {
    for (const child of childProcesses) {
      child.kill("SIGTERM");
    }
    childProcesses.clear();
    process.exit(0);
  };

  process.on("SIGINT", shutdown);
  process.on("SIGTERM", shutdown);
}

function startLongRunningProcess(label: string, command: string, args: string[]) {
  const child = spawn(command, args, {
    cwd: process.cwd(),
    env: process.env,
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

async function runSetupCommand(label: string, command: string, args: string[]) {
  await new Promise<void>((resolve, reject) => {
    const child = spawn(command, args, {
      cwd: process.cwd(),
      env: process.env,
      stdio: ["ignore", "pipe", "pipe"],
    });
    child.stdout?.on("data", (chunk: Buffer | string) => {
      writePrefixed(label, chunk.toString());
    });
    child.stderr?.on("data", (chunk: Buffer | string) => {
      writePrefixed(label, chunk.toString());
    });
    child.once("exit", (code) => {
      if (code === 0) {
        resolve();
        return;
      }
      reject(new Error(`${label} failed with exit code ${code ?? "unknown"}`));
    });
    child.once("error", reject);
  });
}

function writePrefixed(label: string, text: string) {
  for (const line of text.split(/\r?\n/).filter(Boolean)) {
    process.stdout.write(`[${label}] ${line}\n`);
  }
}

void main().catch((error) => {
  process.stderr.write(`${(error as Error).message}\n`);
  process.exitCode = 1;
});
