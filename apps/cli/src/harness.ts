import { mkdir, readFile, writeFile } from "node:fs/promises";
import { spawn, type ChildProcess } from "node:child_process";
import { createRequire } from "node:module";
import { fileURLToPath } from "node:url";
import { join, resolve } from "node:path";

import type { Network } from "@parker/protocol";

import { ParkerApiClient } from "./api.js";
import { type CliRuntimeConfig } from "./config.js";
import { DaemonRpcClient } from "./daemonClient.js";
import { CliLogger } from "./logger.js";
import type { PlayerScenario } from "./scenario.js";

export interface HarnessScenario {
  mock?: boolean;
  network?: Network;
  players: PlayerScenario[];
  serverPort?: number;
  startNigiri?: boolean;
  startServer?: boolean;
}

export async function loadHarnessScenario(path: string): Promise<HarnessScenario> {
  return JSON.parse(await readFile(path, "utf8")) as HarnessScenario;
}

export async function runHarness(args: {
  config: CliRuntimeConfig;
  logger: CliLogger;
  scenario: HarnessScenario;
}) {
  const runDir = join(args.config.runDir, `run-${new Date().toISOString().replaceAll(":", "-")}`);
  await mkdir(runDir, { recursive: true });

  const network = args.scenario.network ?? args.config.network;
  const useMockSettlement = args.scenario.mock ?? args.config.useMockSettlement;
  const serverUrl = `http://127.0.0.1:${args.scenario.serverPort ?? 3020}`;
  const websocketUrl = `${serverUrl.replace(/^http/, "ws")}/ws`;
  const processes: ChildProcess[] = [];
  const runtimePaths = resolveHarnessRuntimePaths();

  try {
    if (args.scenario.startNigiri) {
      await runCommand("nigiri", ["start", "--ark", "--ln", "--ci"], runtimePaths.workspaceRoot);
    }

    if (args.scenario.startServer) {
      processes.push(
        spawn(process.execPath, [runtimePaths.tsxCliPath, runtimePaths.serverEntryPath], {
          cwd: runtimePaths.workspaceRoot,
          env: {
            ...process.env,
            PARKER_NETWORK: network,
            PORT: String(args.scenario.serverPort ?? 3020),
            WEBSOCKET_URL: websocketUrl,
          },
          stdio: "inherit",
        }),
      );
    }

    await new ParkerApiClient(serverUrl).waitForHealth();

    const playerProcesses = await Promise.all(
      args.scenario.players.map(async (player) => {
        const scenarioPath = join(runDir, `${player.profile}.scenario.json`);
        await writeFile(scenarioPath, `${JSON.stringify(player, null, 2)}\n`, "utf8");
        const logPath = join(runDir, `${player.profile}.log`);
        return await spawnPlayerProcess({
          config: args.config,
          logPath,
          network,
          runDir,
          runtimePaths,
          scenarioPath,
          serverUrl,
          useMockSettlement,
          websocketUrl,
        });
      }),
    );
    processes.push(...playerProcesses.map((player) => player.process));

    const results = await Promise.all(playerProcesses.map((player) => player.exit));
    const failed = results.find((code) => code !== 0);
    if (failed !== undefined) {
      throw new Error(`one or more player processes exited with code ${failed}`);
    }

    args.logger.result({
      network,
      runDir,
      serverUrl,
      useMockSettlement,
      websocketUrl,
    });
  } finally {
    for (const child of processes.reverse()) {
      if (!child.killed) {
        child.kill("SIGTERM");
      }
    }
    await stopHarnessDaemons(args.scenario.players, {
      ...args.config,
      network,
      serverUrl,
      useMockSettlement,
      websocketUrl,
    });
    if (args.scenario.startNigiri) {
      await runCommand("nigiri", ["stop"], runtimePaths.workspaceRoot);
    }
  }
}

async function spawnPlayerProcess(args: {
  config: CliRuntimeConfig;
  logPath: string;
  network: Network;
  runDir: string;
  runtimePaths: HarnessRuntimePaths;
  scenarioPath: string;
  serverUrl: string;
  useMockSettlement: boolean;
  websocketUrl: string;
}) {
  const { open } = await import("node:fs/promises");
  const logHandle = await open(args.logPath, "w");
  const processRef = spawn(
    process.execPath,
    [
      args.runtimePaths.tsxCliPath,
      args.runtimePaths.cliEntryPath,
      "play-scenario",
      "--scenario-file",
      args.scenarioPath,
      "--run-dir",
      args.runDir,
      "--json",
    ],
    {
      cwd: args.runtimePaths.workspaceRoot,
      env: {
        ...process.env,
        PARKER_ARK_SERVER_URL: args.config.arkServerUrl,
        PARKER_BOLTZ_URL: args.config.boltzApiUrl,
        PARKER_DAEMON_DIR: args.config.daemonDir,
        PARKER_NETWORK: args.network,
        PARKER_PROFILE_DIR: args.config.profileDir,
        PARKER_RUN_DIR: args.config.runDir,
        PARKER_SERVER_URL: args.serverUrl,
        PARKER_WEBSOCKET_URL: args.websocketUrl,
        PARKER_USE_MOCK_SETTLEMENT: String(args.useMockSettlement),
      },
      stdio: ["ignore", "pipe", "pipe"],
    },
  );
  processRef.stdout?.pipe(logHandle.createWriteStream());
  processRef.stderr?.pipe(logHandle.createWriteStream());

  const exit = new Promise<number>((resolve, reject) => {
    processRef.once("error", reject);
    processRef.once("exit", (code) => {
      void logHandle.close();
      resolve(code ?? 0);
    });
  });

  return {
    exit,
    process: processRef,
  };
}

interface HarnessRuntimePaths {
  cliEntryPath: string;
  serverEntryPath: string;
  tsxCliPath: string;
  workspaceRoot: string;
}

function resolveHarnessRuntimePaths(): HarnessRuntimePaths {
  const require = createRequire(import.meta.url);
  return {
    cliEntryPath: fileURLToPath(new URL("../src/index.ts", import.meta.url)),
    serverEntryPath: fileURLToPath(new URL("../../server/src/index.ts", import.meta.url)),
    tsxCliPath: require.resolve("tsx/dist/cli.mjs"),
    workspaceRoot: fileURLToPath(new URL("../../../", import.meta.url)),
  };
}

async function runCommand(command: string, args: string[], cwd: string) {
  const child = spawn(command, args, {
    cwd,
    stdio: "inherit",
  });
  await new Promise<void>((resolve, reject) => {
    child.once("error", reject);
    child.once("exit", (code) => {
      if (code === 0) {
        resolve();
        return;
      }
      reject(new Error(`${command} ${args.join(" ")} exited with code ${code ?? "unknown"}`));
    });
  });
}

async function stopHarnessDaemons(players: PlayerScenario[], config: CliRuntimeConfig) {
  await Promise.all(
    players.map(async (player) => {
      const client = new DaemonRpcClient(player.profile, config);
      try {
        await client.stopDaemon();
      } catch {
        // Ignore missing/stale daemons during cleanup.
      } finally {
        client.close();
      }
    }),
  );
}
