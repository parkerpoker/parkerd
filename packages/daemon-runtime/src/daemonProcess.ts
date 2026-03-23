import { appendFile } from "node:fs/promises";
import net from "node:net";

import type { WalletSummary } from "@parker/settlement";

import type { CliRuntimeConfig } from "./config.js";
import { cleanupProfileDaemonArtifacts, readProfileDaemonMetadata, writeProfileDaemonMetadata } from "./daemonFiles.js";
import { buildProfileDaemonPaths } from "./daemonPaths.js";
import type {
  BootstrapParams,
  DaemonEventEnvelope,
  DaemonResponseEnvelope,
  DaemonRequestEnvelope,
  DaemonRuntimeState,
  MeshActionParams,
  MeshBootstrapPeerParams,
  MeshCreateTableParams,
  MeshJoinTableParams,
  MeshTableParams,
  ProfileDaemonMetadata,
  WalletDepositParams,
  WalletFaucetParams,
  WalletOffboardParams,
  WalletWithdrawParams,
} from "./daemonProtocol.js";
import { CliLogger, type LogEnvelope } from "./logger.js";
import { MeshRuntime } from "./meshRuntime.js";
import type { MeshRuntimeMode } from "./meshTypes.js";
import { ProfileStore } from "./profileStore.js";
import { CliWalletRuntime } from "./walletRuntime.js";

export class ProfileDaemon {
  private readonly paths: ReturnType<typeof buildProfileDaemonPaths>;
  private heartbeatTimer: NodeJS.Timeout | undefined;
  private metadata: ProfileDaemonMetadata | null = null;
  private readonly server = net.createServer((socket) => {
    this.handleSocket(socket);
  });
  private readonly watchers = new Set<net.Socket>();
  private readonly profileStore: ProfileStore;
  private readonly walletRuntime: CliWalletRuntime;
  private readonly mesh: MeshRuntime;
  private readyPromise = Promise.resolve();
  private wallet: WalletSummary | undefined;

  constructor(
    private readonly profileName: string,
    private readonly config: CliRuntimeConfig,
    private readonly mode: MeshRuntimeMode = "player",
  ) {
    this.paths = buildProfileDaemonPaths(this.config.daemonDir, this.profileName);
    const logger = new CliLogger(config.outputJson, profileName, {
      muteOutput: true,
      sink: (payload) => {
        void this.handleLogEvent(payload);
      },
    });
    this.profileStore = new ProfileStore(config.profileDir);
    this.walletRuntime = new CliWalletRuntime(config, this.profileStore);
    this.mesh = new MeshRuntime(
      profileName,
      config,
      logger,
      this.profileStore,
      this.walletRuntime,
      () => {
        void this.broadcastState();
      },
    );
  }

  async start() {
    process.on("SIGINT", () => {
      void this.stop();
    });
    process.on("SIGTERM", () => {
      void this.stop();
    });

    const existing = await readProfileDaemonMetadata(this.paths);
    if (existing && existing.pid !== process.pid) {
      await cleanupProfileDaemonArtifacts(this.paths);
    }

    await new Promise<void>((resolve, reject) => {
      this.server.once("error", reject);
      this.server.listen(this.paths.socketPath, () => resolve());
    });

    this.metadata = {
      lastHeartbeat: new Date().toISOString(),
      logPath: this.paths.logPath,
      pid: process.pid,
      profile: this.profileName,
      socketPath: this.paths.socketPath,
      startedAt: new Date().toISOString(),
      status: "starting",
    };
    await writeProfileDaemonMetadata(this.paths, this.metadata);

    this.readyPromise = (async () => {
      if (!this.metadata) {
        return;
      }
      await this.mesh.start(this.mode);
      this.wallet = await this.walletRuntime.getWallet(this.profileName);
      const state = this.currentState();
      this.metadata.mode = this.mode;
      if (state.mesh?.peer.peerId) {
        this.metadata.peerId = state.mesh.peer.peerId;
      }
      if (state.mesh?.peer.peerUrl) {
        this.metadata.peerUrl = state.mesh.peer.peerUrl;
      }
      if (state.mesh?.peer.protocolId) {
        this.metadata.protocolId = state.mesh.peer.protocolId;
      }
      this.metadata.status = "running";
      this.metadata.lastHeartbeat = new Date().toISOString();
      await writeProfileDaemonMetadata(this.paths, this.metadata);
    })();
    await this.readyPromise;

    this.heartbeatTimer = setInterval(() => {
      void this.writeHeartbeat();
    }, 5_000);
  }

  async stop() {
    if (this.metadata?.status === "stopping") {
      return;
    }
    if (this.metadata) {
      this.metadata.status = "stopping";
      await writeProfileDaemonMetadata(this.paths, this.metadata);
    }
    if (this.heartbeatTimer) {
      clearInterval(this.heartbeatTimer);
      this.heartbeatTimer = undefined;
    }
    this.mesh.close();
    for (const watcher of this.watchers) {
      watcher.end();
    }
    this.watchers.clear();

    await new Promise<void>((resolve) => {
      this.server.close(() => resolve());
    });
    await cleanupProfileDaemonArtifacts(this.paths);
  }

  private async refreshWallet() {
    this.wallet = await this.walletRuntime.getWallet(this.profileName);
  }

  private async broadcast(event: DaemonEventEnvelope) {
    const serialized = `${JSON.stringify(event)}\n`;
    for (const watcher of [...this.watchers]) {
      if (watcher.destroyed) {
        this.watchers.delete(watcher);
        continue;
      }
      watcher.write(serialized);
    }
  }

  private async dispatch(request: DaemonRequestEnvelope, socket: net.Socket): Promise<DaemonResponseEnvelope> {
    if (request.method !== "ping" && request.method !== "status" && request.method !== "stop") {
      await this.readyPromise;
    }

    try {
      switch (request.method) {
        case "ping":
          return okResponse(request.id, { ok: true });
        case "watch": {
          this.watchers.add(socket);
          socket.once("close", () => {
            this.watchers.delete(socket);
          });
          return okResponse(request.id, this.currentState());
        }
        case "status":
          return okResponse(request.id, this.currentState());
        case "bootstrap": {
          const result = await this.mesh.bootstrap(((request.params ?? {}) as BootstrapParams).nickname);
          this.wallet = result.wallet;
          await this.broadcastState();
          return okResponse(request.id, { mesh: result });
        }
        case "walletSummary":
          await this.refreshWallet();
          return okResponse(request.id, this.wallet);
        case "walletDeposit": {
          const params = (request.params ?? {}) as unknown as WalletDepositParams;
          const quote = await this.walletRuntime.createDepositQuote(this.profileName, params.amountSats);
          await this.refreshWallet();
          await this.broadcastState();
          return okResponse(request.id, quote);
        }
        case "walletWithdraw": {
          const params = (request.params ?? {}) as unknown as WalletWithdrawParams;
          const withdrawal = await this.walletRuntime.submitWithdrawal(this.profileName, params.amountSats, params.invoice);
          await this.refreshWallet();
          await this.broadcastState();
          return okResponse(request.id, withdrawal);
        }
        case "walletFaucet": {
          const params = (request.params ?? {}) as unknown as WalletFaucetParams;
          await this.walletRuntime.nigiriFaucet(this.profileName, params.amountSats);
          await this.refreshWallet();
          await this.broadcastState();
          return okResponse(request.id, this.currentState());
        }
        case "walletOnboard": {
          const result = await this.walletRuntime.onboard(this.profileName);
          await this.refreshWallet();
          await this.broadcastState();
          return okResponse(request.id, result);
        }
        case "walletOffboard": {
          const params = (request.params ?? {}) as unknown as WalletOffboardParams;
          const result = await this.walletRuntime.offboard(this.profileName, params.address, params.amountSats);
          await this.refreshWallet();
          await this.broadcastState();
          return okResponse(request.id, result);
        }
        case "meshNetworkPeers":
          return okResponse(request.id, await this.mesh.networkPeers());
        case "meshBootstrapPeer": {
          const params = (request.params ?? {}) as unknown as MeshBootstrapPeerParams;
          return okResponse(request.id, await this.mesh.addBootstrapPeer(params.peerUrl, params.alias, params.roles));
        }
        case "meshCreateTable":
          return okResponse(
            request.id,
            await this.mesh.createTable(((request.params ?? {}) as MeshCreateTableParams).table),
          );
        case "meshTableAnnounce":
          return okResponse(
            request.id,
            await this.mesh.announceTable(((request.params ?? {}) as MeshTableParams).tableId),
          );
        case "meshTableJoin": {
          const params = (request.params ?? {}) as unknown as MeshJoinTableParams;
          const result = await this.mesh.joinTable(params.inviteCode, params.buyInSats);
          await this.refreshWallet();
          await this.broadcastState();
          return okResponse(request.id, result);
        }
        case "meshGetTable":
          return okResponse(
            request.id,
            await this.mesh.getTableState(((request.params ?? {}) as MeshTableParams).tableId),
          );
        case "meshSendAction":
          return okResponse(
            request.id,
            await this.mesh.sendAction(
              ((request.params ?? {}) as unknown as MeshActionParams).payload,
              ((request.params ?? {}) as unknown as MeshActionParams).tableId,
            ),
          );
        case "meshRotateHost":
          return okResponse(
            request.id,
            await this.mesh.rotateHost(((request.params ?? {}) as MeshTableParams).tableId),
          );
        case "meshCashOut": {
          const result = await this.mesh.cashOut(((request.params ?? {}) as MeshTableParams).tableId);
          await this.refreshWallet();
          await this.broadcastState();
          return okResponse(request.id, result);
        }
        case "meshRenew": {
          const result = await this.mesh.renewFunds(((request.params ?? {}) as MeshTableParams).tableId);
          await this.refreshWallet();
          await this.broadcastState();
          return okResponse(request.id, result);
        }
        case "meshExit": {
          const result = await this.mesh.emergencyExit(((request.params ?? {}) as MeshTableParams).tableId);
          await this.refreshWallet();
          await this.broadcastState();
          return okResponse(request.id, result);
        }
        case "meshPublicTables":
          return okResponse(request.id, await this.mesh.listPublicTables());
        case "stop":
          setTimeout(() => {
            void this.stop();
          }, 0);
          return okResponse(request.id, { stopping: true });
      }
    } catch (error) {
      return {
        error: (error as Error).message,
        id: request.id,
        ok: false,
        type: "response",
      };
    }
  }

  private async handleLogEvent(payload: LogEnvelope) {
    await appendFile(this.paths.logPath, `${JSON.stringify(payload)}\n`, "utf8");
    await this.broadcast({
      event: "log",
      payload,
      type: "event",
    });
  }

  private handleSocket(socket: net.Socket) {
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
        const request = JSON.parse(line) as DaemonRequestEnvelope;
        void this.dispatch(request, socket).then((response) => {
          socket.write(`${JSON.stringify(response)}\n`);
        });
      }
    });
  }

  private async broadcastState() {
    await this.broadcast({
      event: "state",
      payload: this.currentState(),
      type: "event",
    });
  }

  private currentState(): DaemonRuntimeState {
    return {
      mesh: this.mesh.currentState(),
      ...(this.wallet ? { wallet: this.wallet } : {}),
    };
  }

  private async writeHeartbeat() {
    if (!this.metadata) {
      return;
    }
    this.metadata.lastHeartbeat = new Date().toISOString();
    await writeProfileDaemonMetadata(this.paths, this.metadata);
  }
}

function okResponse(id: string, result: unknown): DaemonResponseEnvelope {
  return {
    id,
    ok: true,
    result,
    type: "response",
  };
}
