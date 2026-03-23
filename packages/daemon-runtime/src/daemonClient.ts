import net from "node:net";
import { spawn } from "node:child_process";
import { fileURLToPath } from "node:url";

import type {
  MeshPlayerActionPayload,
  SwapJobStatus,
  SwapQuote,
} from "@parker/protocol";
import type { WalletSummary } from "@parker/settlement";

import type { CliRuntimeConfig } from "./config.js";
import { cleanupProfileDaemonArtifacts, isPidAlive, readProfileDaemonMetadata } from "./daemonFiles.js";
import { buildProfileDaemonPaths } from "./daemonPaths.js";
import type {
  DaemonEventEnvelope,
  DaemonRequestEnvelope,
  DaemonResponseEnvelope,
  DaemonRuntimeState,
  MeshBootstrapResult,
  ProfileDaemonMetadata,
} from "./daemonProtocol.js";
import type { MeshRuntimeMode } from "./meshTypes.js";

const DAEMON_REQUEST_TIMEOUT_MS = 60_000;

export interface ProfileDaemonStatus {
  metadata: ProfileDaemonMetadata | null;
  reachable: boolean;
  state: DaemonRuntimeState | null;
}

export class DaemonRpcClient {
  private cachedState: DaemonRuntimeState = {};

  constructor(
    private readonly profileName: string,
    private readonly config: CliRuntimeConfig,
  ) {}

  async bootstrap(nickname?: string): Promise<{ mesh: MeshBootstrapResult }> {
    return (await this.request("bootstrap", nickname ? { nickname } : undefined)) as {
      mesh: MeshBootstrapResult;
    };
  }

  close() {}

  currentState() {
    return this.cachedState;
  }

  async ensureRunning(mode?: MeshRuntimeMode) {
    if (await this.isReachable()) {
      return;
    }
    await this.startDaemonProcess(mode);
    await this.waitForReachable();
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

  async stopDaemon() {
    await this.request("stop", undefined, false);
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

  private async startDaemonProcess(mode?: MeshRuntimeMode) {
    const metadata = await readProfileDaemonMetadata(this.paths);
    if (metadata && !isPidAlive(metadata.pid)) {
      await cleanupProfileDaemonArtifacts(this.paths);
    }

    const daemonLauncherPath = resolveDaemonLauncherPath();
    const args = ["--profile", this.profileName];
    if (mode) {
      args.push("--mode", mode);
    }
    const child = spawn(daemonLauncherPath, args, {
      cwd: process.cwd(),
      detached: true,
      env: {
        ...process.env,
        PARKER_NETWORK: this.config.network,
        ...(this.config.indexerUrl ? { PARKER_INDEXER_URL: this.config.indexerUrl } : {}),
        PARKER_ARK_SERVER_URL: this.config.arkServerUrl,
        PARKER_BOLTZ_URL: this.config.boltzApiUrl,
        PARKER_USE_MOCK_SETTLEMENT: String(this.config.useMockSettlement),
        PARKER_PROFILE_DIR: this.config.profileDir,
        PARKER_DAEMON_DIR: this.config.daemonDir,
        PARKER_PEER_HOST: this.config.peerHost,
        PARKER_PEER_PORT: String(this.config.peerPort),
        PARKER_RUN_DIR: this.config.runDir,
        ...(this.config.nigiriDatadir ? { PARKER_NIGIRI_DATADIR: this.config.nigiriDatadir } : {}),
      },
      stdio: "ignore",
    });
    child.unref();
  }

  private updateCachedState(result: unknown) {
    if (!result || typeof result !== "object") {
      return;
    }
    const candidate = result as Record<string, unknown>;
    if ("mesh" in candidate || "wallet" in candidate) {
      this.cachedState = {
        ...this.cachedState,
        ...(result as DaemonRuntimeState),
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

function resolveDaemonLauncherPath() {
  return fileURLToPath(new URL("../../../scripts/bin/parker-daemon", import.meta.url));
}

function sleep(ms: number) {
  return new Promise<void>((resolve) => {
    setTimeout(resolve, ms);
  });
}
