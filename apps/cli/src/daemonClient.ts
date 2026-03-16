import net from "node:net";
import { spawn } from "node:child_process";
import { fileURLToPath } from "node:url";

import type {
  CreateTableRequest,
  CreateTableResponse,
  SignedActionPayload,
  SwapJobStatus,
  SwapQuote,
  TableSnapshot,
} from "@parker/protocol";
import type { WalletSummary } from "@parker/settlement";

import type { TableTranscript } from "./api.js";
import type { CliRuntimeConfig } from "./config.js";
import { cleanupProfileDaemonArtifacts, isPidAlive, readProfileDaemonMetadata } from "./daemonFiles.js";
import { buildProfileDaemonPaths } from "./daemonPaths.js";
import type {
  DaemonEventEnvelope,
  DaemonRequestEnvelope,
  DaemonResponseEnvelope,
  ProfileDaemonMetadata,
} from "./daemonProtocol.js";
import type { LogEnvelope } from "./logger.js";
import type { PlayerRuntimeState } from "./playerClient.js";
import type { BootstrapResult } from "./walletRuntime.js";

export interface ProfileDaemonStatus {
  metadata: ProfileDaemonMetadata | null;
  reachable: boolean;
  state: PlayerRuntimeState | null;
}

export class DaemonRpcClient {
  private cachedState: PlayerRuntimeState = {
    identity: undefined,
    peerMessages: [],
    peerStatus: "offline",
    profile: undefined,
    snapshot: null,
    wallet: undefined,
  };

  constructor(
    private readonly profileName: string,
    private readonly config: CliRuntimeConfig,
  ) {}

  async bootstrap(nickname?: string): Promise<BootstrapResult> {
    return (await this.request("bootstrap", nickname ? { nickname } : undefined)) as BootstrapResult;
  }

  close() {}

  async connectCurrentTable() {
    await this.request("connectCurrentTable");
  }

  async createTable(table?: Partial<CreateTableRequest>): Promise<CreateTableResponse> {
    return (await this.request("createTable", table ? { table } : undefined)) as CreateTableResponse;
  }

  currentState() {
    return this.cachedState;
  }

  async ensureRunning() {
    if (await this.isReachable()) {
      return;
    }
    await this.startDaemonProcess();
    await this.waitForReachable();
  }

  async getSnapshot() {
    return (await this.request("getSnapshot")) as TableSnapshot;
  }

  async getTranscript() {
    return (await this.request("getTranscript")) as TableTranscript;
  }

  async inspect(autoStart = false): Promise<ProfileDaemonStatus> {
    const metadata = await readProfileDaemonMetadata(this.paths);
    const reachable = autoStart ? (await this.isReachable(true)) : await this.isReachable();
    let state: PlayerRuntimeState | null = null;
    if (reachable) {
      state = (await this.request("status", undefined, autoStart)) as PlayerRuntimeState;
    }
    return { metadata, reachable, state };
  }

  async joinTable(inviteCode: string, buyInSats = 4_000): Promise<TableSnapshot> {
    return (await this.request("joinTable", { buyInSats, inviteCode })) as TableSnapshot;
  }

  async sendAction(payload: SignedActionPayload) {
    await this.request("sendAction", { payload });
  }

  async sendPeerMessage(message: string) {
    await this.request("sendPeerMessage", { message });
  }

  async stopDaemon() {
    await this.request("stop", undefined, false);
  }

  async waitForCondition(
    description: string,
    predicate: (snapshot: TableSnapshot | null, peerStatus: string) => boolean,
    timeoutMs = 30_000,
  ) {
    const start = Date.now();
    while (Date.now() - start < timeoutMs) {
      const state = (await this.request("status")) as PlayerRuntimeState;
      this.cachedState = state;
      if (predicate(state.snapshot, state.peerStatus)) {
        return state.snapshot;
      }
      await sleep(100);
    }
    throw new Error(`timed out waiting for ${description}`);
  }

  async walletDeposit(amountSats: number): Promise<SwapQuote> {
    return (await this.request("walletDeposit", { amountSats })) as SwapQuote;
  }

  async walletFaucet(amountSats: number) {
    await this.request("walletFaucet", { amountSats });
  }

  async walletOffboard(address: string, amountSats?: number) {
    return await this.request("walletOffboard", amountSats === undefined ? { address } : { address, amountSats });
  }

  async walletOnboard() {
    return await this.request("walletOnboard");
  }

  async walletSummary(): Promise<WalletSummary> {
    return (await this.request("walletSummary")) as WalletSummary;
  }

  async walletWithdraw(amountSats: number, invoice: string): Promise<SwapJobStatus> {
    return (await this.request("walletWithdraw", { amountSats, invoice })) as SwapJobStatus;
  }

  async watch(onEvent: (event: DaemonEventEnvelope) => void) {
    await this.ensureRunning();
    const socket = await this.connectSocket();
    let buffer = "";
    let acknowledged = false;
    socket.on("data", (chunk: Buffer | string) => {
      buffer += chunk.toString();
      for (;;) {
        const newlineIndex = buffer.indexOf("\n");
        if (newlineIndex === -1) {
          break;
        }
        const line = buffer.slice(0, newlineIndex).trim();
        buffer = buffer.slice(newlineIndex + 1);
        if (!line) {
          continue;
        }
        const parsed = JSON.parse(line) as DaemonResponseEnvelope | DaemonEventEnvelope;
        if (parsed.type === "response") {
          if (parsed.ok && parsed.result) {
            this.updateCachedState(parsed.result);
          }
          acknowledged = true;
          continue;
        }
        if (parsed.type === "event") {
          if (parsed.event === "state") {
            this.cachedState = parsed.payload as PlayerRuntimeState;
          }
          onEvent(parsed);
        }
      }
    });
    socket.write(`${JSON.stringify(this.buildRequest("watch"))}\n`);
    const start = Date.now();
    while (!acknowledged) {
      if (Date.now() - start > 5_000) {
        socket.destroy();
        throw new Error("timed out waiting for daemon watch acknowledgement");
      }
      await sleep(50);
    }

    return () => {
      socket.end();
    };
  }

  async commitSeed(reveal = false): Promise<TableSnapshot> {
    return (await this.request("commitSeed", { reveal })) as TableSnapshot;
  }

  private get paths() {
    return buildProfileDaemonPaths(this.config.daemonDir, this.profileName);
  }

  private buildRequest(method: DaemonRequestEnvelope["method"], params?: Record<string, unknown>): DaemonRequestEnvelope {
    return params === undefined
      ? {
          id: crypto.randomUUID(),
          method,
          type: "request",
        }
      : {
          id: crypto.randomUUID(),
          method,
          params,
          type: "request",
        };
  }

  private async connectSocket() {
    return await new Promise<net.Socket>((resolve, reject) => {
      const socket = net.createConnection(this.paths.socketPath);
      socket.once("connect", () => resolve(socket));
      socket.once("error", reject);
    });
  }

  private async isReachable(autoStart = false) {
    try {
      await this.request("ping", undefined, autoStart);
      return true;
    } catch {
      return false;
    }
  }

  private async request(method: DaemonRequestEnvelope["method"], params?: Record<string, unknown>, autoStart = true) {
    if (autoStart) {
      await this.ensureRunning();
    }
    const socket = await this.connectSocket();
    const request = this.buildRequest(method, params);
    return await new Promise<unknown>((resolve, reject) => {
      let buffer = "";
      socket.on("data", (chunk: Buffer | string) => {
        buffer += chunk.toString();
        for (;;) {
          const newlineIndex = buffer.indexOf("\n");
          if (newlineIndex === -1) {
            break;
          }
          const line = buffer.slice(0, newlineIndex).trim();
          buffer = buffer.slice(newlineIndex + 1);
          if (!line) {
            continue;
          }
          const parsed = JSON.parse(line) as DaemonResponseEnvelope | DaemonEventEnvelope;
          if (parsed.type === "event") {
            if (parsed.event === "state") {
              this.cachedState = parsed.payload as PlayerRuntimeState;
            }
            continue;
          }
          if (parsed.id !== request.id) {
            continue;
          }
          socket.end();
          if (!parsed.ok) {
            reject(new Error(parsed.error ?? `daemon request ${method} failed`));
            return;
          }
          this.updateCachedState(parsed.result);
          resolve(parsed.result);
          return;
        }
      });
      socket.once("error", reject);
      socket.write(`${JSON.stringify(request)}\n`);
    });
  }

  private async startDaemonProcess() {
    const metadata = await readProfileDaemonMetadata(this.paths);
    if (metadata && !isPidAlive(metadata.pid)) {
      await cleanupProfileDaemonArtifacts(this.paths);
    }

    const daemonEntryPath = resolveDaemonEntryPath();
    const child = spawn(process.execPath, [...process.execArgv, daemonEntryPath, "--profile", this.profileName], {
      cwd: process.cwd(),
      detached: true,
      env: {
        ...process.env,
        PARKER_NETWORK: this.config.network,
        PARKER_SERVER_URL: this.config.serverUrl,
        PARKER_WEBSOCKET_URL: this.config.websocketUrl,
        PARKER_ARK_SERVER_URL: this.config.arkServerUrl,
        PARKER_BOLTZ_URL: this.config.boltzApiUrl,
        PARKER_USE_MOCK_SETTLEMENT: String(this.config.useMockSettlement),
        PARKER_PROFILE_DIR: this.config.profileDir,
        PARKER_DAEMON_DIR: this.config.daemonDir,
        PARKER_RUN_DIR: this.config.runDir,
      },
      stdio: "ignore",
    });
    child.unref();
  }

  private updateCachedState(result: unknown) {
    if (!result || typeof result !== "object") {
      return;
    }
    if ("peerStatus" in (result as Record<string, unknown>) && "peerMessages" in (result as Record<string, unknown>)) {
      this.cachedState = result as PlayerRuntimeState;
    }
  }

  private async waitForReachable(timeoutMs = 10_000) {
    const start = Date.now();
    while (Date.now() - start < timeoutMs) {
      try {
        await this.request("ping", undefined, false);
        return;
      } catch {
        await sleep(100);
      }
    }
    throw new Error(`timed out waiting for daemon for profile ${this.profileName}`);
  }
}

function resolveDaemonEntryPath() {
  const currentPath = fileURLToPath(import.meta.url);
  return currentPath.endsWith(".ts")
    ? currentPath.replace(/daemonClient\.ts$/, "daemonEntry.ts")
    : currentPath.replace(/daemonClient\.js$/, "daemonEntry.js");
}

function sleep(ms: number) {
  return new Promise<void>((resolve) => {
    setTimeout(resolve, ms);
  });
}
