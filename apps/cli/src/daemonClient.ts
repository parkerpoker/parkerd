import net from "node:net";
import { spawn } from "node:child_process";
import { fileURLToPath } from "node:url";

import type {
  CreateTableRequest,
  CreateTableResponse,
  MeshPlayerActionPayload,
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
  DaemonRuntimeState,
  DaemonEventEnvelope,
  DaemonRequestEnvelope,
  DaemonResponseEnvelope,
  ProfileDaemonMetadata,
} from "./daemonProtocol.js";
import type { LogEnvelope } from "./logger.js";
import type { BootstrapResult } from "./walletRuntime.js";

const DAEMON_REQUEST_TIMEOUT_MS = 60_000;

export interface ProfileDaemonStatus {
  metadata: ProfileDaemonMetadata | null;
  reachable: boolean;
  state: DaemonRuntimeState | null;
}

export class DaemonRpcClient {
  private cachedState: DaemonRuntimeState = {
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
    let state: DaemonRuntimeState | null = null;
    if (reachable) {
      state = (await this.request("status", undefined, autoStart)) as DaemonRuntimeState;
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
      const state = (await this.request("status")) as DaemonRuntimeState;
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
            this.cachedState = parsed.payload as DaemonRuntimeState;
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

  async meshBootstrapPeer(peerUrl: string, alias?: string, roles?: string[]) {
    return await this.request("meshBootstrapPeer", {
      ...(alias ? { alias } : {}),
      peerUrl,
      ...(roles ? { roles } : {}),
    });
  }

  async meshCashOut(tableId?: string) {
    return await this.request("meshCashOut", tableId ? { tableId } : undefined);
  }

  async meshCreateTable(table?: Record<string, unknown>) {
    return await this.request("meshCreateTable", table ? { table } : undefined);
  }

  async meshExit(tableId?: string) {
    return await this.request("meshExit", tableId ? { tableId } : undefined);
  }

  async meshGetTable(tableId?: string) {
    return await this.request("meshGetTable", tableId ? { tableId } : undefined);
  }

  async meshNetworkPeers() {
    return await this.request("meshNetworkPeers");
  }

  async meshPublicTables() {
    return await this.request("meshPublicTables");
  }

  async meshRenew(tableId?: string) {
    return await this.request("meshRenew", tableId ? { tableId } : undefined);
  }

  async meshRotateHost(tableId?: string) {
    return await this.request("meshRotateHost", tableId ? { tableId } : undefined);
  }

  async meshSendAction(payload: MeshPlayerActionPayload, tableId?: string) {
    return await this.request("meshSendAction", {
      payload,
      ...(tableId ? { tableId } : {}),
    });
  }

  async meshTableAnnounce(tableId?: string) {
    return await this.request("meshTableAnnounce", tableId ? { tableId } : undefined);
  }

  async meshTableJoin(inviteCode: string, buyInSats?: number) {
    return await this.request("meshTableJoin", {
      inviteCode,
      ...(buyInSats === undefined ? {} : { buyInSats }),
    });
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
      let settled = false;
      let buffer = "";
      const timeout = setTimeout(() => {
        if (settled) {
          return;
        }
        settled = true;
        socket.destroy();
        reject(new Error(`daemon request ${method} timed out after ${DAEMON_REQUEST_TIMEOUT_MS}ms`));
      }, DAEMON_REQUEST_TIMEOUT_MS);
      socket.on("data", (chunk: Buffer | string) => {
        if (settled) {
          return;
        }
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
              this.cachedState = parsed.payload as DaemonRuntimeState;
            }
            continue;
          }
          if (parsed.id !== request.id) {
            continue;
          }
          settled = true;
          clearTimeout(timeout);
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
      socket.once("error", (error) => {
        if (settled) {
          return;
        }
        settled = true;
        clearTimeout(timeout);
        reject(error);
      });
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
        PARKER_INDEXER_URL: this.config.indexerUrl,
        PARKER_WEBSOCKET_URL: this.config.websocketUrl,
        PARKER_ARK_SERVER_URL: this.config.arkServerUrl,
        PARKER_BOLTZ_URL: this.config.boltzApiUrl,
        PARKER_USE_MOCK_SETTLEMENT: String(this.config.useMockSettlement),
        PARKER_PROFILE_DIR: this.config.profileDir,
        PARKER_DAEMON_DIR: this.config.daemonDir,
        PARKER_PEER_HOST: this.config.peerHost,
        PARKER_PEER_PORT: String(this.config.peerPort),
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
    if (
      "peerStatus" in (result as Record<string, unknown>) &&
      "peerMessages" in (result as Record<string, unknown>)
    ) {
      this.cachedState = result as DaemonRuntimeState;
      return;
    }
    if ("mesh" in (result as Record<string, unknown>)) {
      this.cachedState = {
        ...this.cachedState,
        ...(result as Partial<DaemonRuntimeState>),
      };
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
