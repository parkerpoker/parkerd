import { appendFile } from "node:fs/promises";
import net from "node:net";

import type { CreateTableRequest, SignedActionPayload } from "@parker/protocol";

import type { CliRuntimeConfig } from "./config.js";
import { cleanupProfileDaemonArtifacts, readProfileDaemonMetadata, writeProfileDaemonMetadata } from "./daemonFiles.js";
import { buildProfileDaemonPaths } from "./daemonPaths.js";
import type {
  BootstrapParams,
  CommitSeedParams,
  CreateTableParams,
  DaemonEventEnvelope,
  DaemonRuntimeState,
  DaemonRequestEnvelope,
  DaemonResponseEnvelope,
  JoinTableParams,
  MeshActionParams,
  MeshBootstrapPeerParams,
  MeshCreateTableParams,
  MeshJoinTableParams,
  MeshTableParams,
  ProfileDaemonMetadata,
  SendActionParams,
  SendPeerMessageParams,
  WalletDepositParams,
  WalletFaucetParams,
  WalletOffboardParams,
  WalletWithdrawParams,
} from "./daemonProtocol.js";
import { CliLogger, type LogEnvelope } from "./logger.js";
import { MeshRuntime } from "./meshRuntime.js";
import type { MeshRuntimeMode } from "./meshTypes.js";
import { ParkerPlayerClient } from "./playerClient.js";
import { ProfileStore } from "./profileStore.js";

export class ProfileDaemon {
  private readonly paths: ReturnType<typeof buildProfileDaemonPaths>;
  private heartbeatTimer: NodeJS.Timeout | undefined;
  private readonly mesh: MeshRuntime;
  private metadata: ProfileDaemonMetadata | null = null;
  private readonly player: ParkerPlayerClient;
  private readonly server = net.createServer((socket) => {
    this.handleSocket(socket);
  });
  private readonly watchers = new Set<net.Socket>();
  private readyPromise: Promise<void>;

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
    this.player = new ParkerPlayerClient(
      profileName,
      config,
      logger,
      undefined,
      undefined,
      undefined,
      () => {
        void this.broadcastState();
      },
    );
    this.mesh = new MeshRuntime(profileName, config, logger, undefined, undefined, () => {
      void this.broadcastState();
    });
    this.readyPromise = this.restoreExistingSession();
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

    this.readyPromise = this.readyPromise.then(async () => {
      if (!this.metadata) {
        return;
      }
      await this.mesh.start(this.mode);
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
    });
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
    this.player.close();
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
        case "bootstrap":
          return okResponse(request.id, {
            legacy: await this.player.bootstrap(((request.params ?? {}) as unknown as BootstrapParams).nickname),
            mesh: await this.mesh.bootstrap(((request.params ?? {}) as unknown as BootstrapParams).nickname),
          });
        case "walletSummary":
          return okResponse(request.id, await this.player.walletSummary());
        case "walletDeposit": {
          const params = (request.params ?? {}) as unknown as WalletDepositParams;
          return okResponse(request.id, await this.player.walletDeposit(params.amountSats));
        }
        case "walletWithdraw": {
          const params = (request.params ?? {}) as unknown as WalletWithdrawParams;
          return okResponse(request.id, await this.player.walletWithdraw(params.amountSats, params.invoice));
        }
        case "walletFaucet": {
          const params = (request.params ?? {}) as unknown as WalletFaucetParams;
          await this.player.walletFaucet(params.amountSats);
          return okResponse(request.id, this.player.currentState());
        }
        case "walletOnboard":
          return okResponse(request.id, await this.player.walletOnboard());
        case "walletOffboard": {
          const params = (request.params ?? {}) as unknown as WalletOffboardParams;
          return okResponse(request.id, await this.player.walletOffboard(params.address, params.amountSats));
        }
        case "createTable":
          return okResponse(
            request.id,
            await this.player.createTable(
              ((request.params ?? {}) as unknown as CreateTableParams).table as Partial<CreateTableRequest> | undefined,
            ),
          );
        case "joinTable": {
          const params = (request.params ?? {}) as unknown as JoinTableParams;
          return okResponse(request.id, await this.player.joinTable(params.inviteCode, params.buyInSats));
        }
        case "connectCurrentTable":
          await this.player.connectCurrentTable();
          return okResponse(request.id, this.currentState());
        case "getSnapshot":
          return okResponse(request.id, await this.player.getSnapshot());
        case "getTranscript":
          return okResponse(request.id, await this.player.getTranscript());
        case "commitSeed":
          return okResponse(
            request.id,
            await this.player.commitSeed(((request.params ?? {}) as unknown as CommitSeedParams).reveal ?? false),
          );
        case "sendAction":
          await this.player.sendAction(((request.params ?? {}) as unknown as SendActionParams).payload as SignedActionPayload);
          return okResponse(request.id, this.currentState());
        case "sendPeerMessage":
          await this.player.sendPeerMessage(((request.params ?? {}) as unknown as SendPeerMessageParams).message);
          return okResponse(request.id, this.currentState());
        case "meshNetworkPeers":
          return okResponse(request.id, await this.mesh.networkPeers());
        case "meshBootstrapPeer": {
          const params = (request.params ?? {}) as unknown as MeshBootstrapPeerParams;
          return okResponse(
            request.id,
            await this.mesh.addBootstrapPeer(params.peerUrl, params.alias, params.roles),
          );
        }
        case "meshCreateTable":
          return okResponse(
            request.id,
            await this.mesh.createTable(((request.params ?? {}) as unknown as MeshCreateTableParams).table),
          );
        case "meshTableAnnounce":
          return okResponse(
            request.id,
            await this.mesh.announceTable(((request.params ?? {}) as unknown as MeshTableParams).tableId),
          );
        case "meshTableJoin": {
          const params = (request.params ?? {}) as unknown as MeshJoinTableParams;
          return okResponse(request.id, await this.mesh.joinTable(params.inviteCode, params.buyInSats));
        }
        case "meshGetTable":
          return okResponse(
            request.id,
            await this.mesh.getTableState(((request.params ?? {}) as unknown as MeshTableParams).tableId),
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
            await this.mesh.rotateHost(((request.params ?? {}) as unknown as MeshTableParams).tableId),
          );
        case "meshCashOut":
          return okResponse(
            request.id,
            await this.mesh.cashOut(((request.params ?? {}) as unknown as MeshTableParams).tableId),
          );
        case "meshRenew":
          return okResponse(
            request.id,
            await this.mesh.renewFunds(((request.params ?? {}) as unknown as MeshTableParams).tableId),
          );
        case "meshExit":
          return okResponse(
            request.id,
            await this.mesh.emergencyExit(((request.params ?? {}) as unknown as MeshTableParams).tableId),
          );
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

  private async restoreExistingSession() {
    const store = new ProfileStore(this.config.profileDir);
    const existing = await store.load(this.profileName);
    if (!existing) {
      return;
    }
    await this.player.bootstrap(existing.nickname);
    if (existing.currentTable) {
      try {
        await this.player.connectCurrentTable();
      } catch (error) {
        await this.handleLogEvent({
          data: { error: (error as Error).message },
          level: "error",
          message: "failed to reconnect current table on daemon startup",
          scope: this.profileName,
        });
      }
    }
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
      ...this.player.currentState(),
      mesh: this.mesh.currentState(),
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
