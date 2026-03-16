import {
  buildCommitmentHash,
  createHoldemHand,
  deriveDeckSeed,
} from "@parker/game-engine";
import type {
  CreateTableRequest,
  ServerSocketEvent,
  SignedActionPayload,
  TableSnapshot,
} from "@parker/protocol";
import { randomHex, type LocalIdentity, type WalletSummary } from "@parker/settlement";

import type { CliRuntimeConfig } from "./config.js";
import { CliLogger } from "./logger.js";
import { ProfileStore, type PlayerProfileState } from "./profileStore.js";
import { ParkerApiClient } from "./api.js";
import { TableSocketClient } from "./tableSocketClient.js";
import { CliWalletRuntime } from "./walletRuntime.js";

export interface PlayerRuntimeState {
  identity: LocalIdentity | undefined;
  peerMessages: string[];
  peerStatus: string;
  profile: PlayerProfileState | undefined;
  snapshot: TableSnapshot | null;
  wallet: WalletSummary | undefined;
}

export class ParkerPlayerClient {
  private checkpointWaiters: Array<{
    check: () => void;
    description: string;
    interval: NodeJS.Timeout;
    predicate: (snapshot: TableSnapshot | null) => boolean;
    reject: (error: Error) => void;
    resolve: (snapshot: TableSnapshot) => void;
    timeout: NodeJS.Timeout;
  }> = [];
  private closed = false;
  private identity: LocalIdentity | undefined;
  private readonly peerMessages: string[] = [];
  private peerStatus = "offline";
  private profileState: PlayerProfileState | undefined;
  private snapshot: TableSnapshot | null = null;
  private socket: TableSocketClient | undefined;
  private wallet: WalletSummary | undefined;

  constructor(
    private readonly profileName: string,
    private readonly config: CliRuntimeConfig,
    private readonly logger: CliLogger,
    private readonly api = new ParkerApiClient(config.serverUrl),
    private readonly store = new ProfileStore(config.profileDir),
    private readonly walletRuntime = new CliWalletRuntime(config, store),
    private readonly onStateChange?: (state: PlayerRuntimeState) => void,
  ) {}

  async bootstrap(nickname?: string) {
    const bootstrap = await this.walletRuntime.bootstrap(this.profileName, nickname);
    this.identity = bootstrap.identity;
    this.profileState = bootstrap.state;
    this.wallet = bootstrap.wallet;
    this.notifyStateChange();
    return bootstrap;
  }

  async connectCurrentTable() {
    this.closed = false;
    const identity = await this.ensureIdentity();
    const state = await this.ensureProfileState();
    if (!state.currentTable) {
      throw new Error("profile does not have a current table");
    }
    if (this.socket) {
      return;
    }

    this.socket = await TableSocketClient.connect({
      onEvent: async (event) => {
        try {
          await this.handleServerEvent(event);
        } catch (error) {
          if (!this.closed) {
            this.logger.error((error as Error).message);
          }
        }
      },
      playerId: identity.playerId,
      tableId: state.currentTable.tableId,
      wsUrl: this.config.websocketUrl,
    });
    this.notifyStateChange();
  }

  async createTable(input: Partial<CreateTableRequest> = {}) {
    const identity = await this.ensureIdentity();
    const wallet = await this.ensureWallet();
    const state = await this.ensureProfileState();

    const response = await this.api.createTable({
      actionTimeoutSeconds: input.actionTimeoutSeconds ?? 25,
      bigBlindSats: input.bigBlindSats ?? 100,
      buyInMaxSats: input.buyInMaxSats ?? 10_000,
      buyInMinSats: input.buyInMinSats ?? 2_000,
      commitmentDeadlineSeconds: input.commitmentDeadlineSeconds ?? 20,
      host: {
        playerId: identity.playerId,
        nickname: state.nickname,
        joinedAt: new Date().toISOString(),
        pubkeyHex: identity.publicKeyHex,
        arkAddress: wallet.arkAddress,
        boardingAddress: wallet.boardingAddress,
      },
      smallBlindSats: input.smallBlindSats ?? 50,
    });
    state.currentTable = {
      inviteCode: response.table.inviteCode,
      seatIndex: 0,
      tableId: response.table.tableId,
    };
    await this.walletRuntime.saveState(state);
    this.profileState = state;
    this.snapshot = await this.api.getTable(response.table.tableId);
    this.peerStatus = "offline";
    this.notifyStateChange();
    return response;
  }

  async joinTable(inviteCode: string, buyInSats = 4_000) {
    const identity = await this.ensureIdentity();
    const wallet = await this.ensureWallet();
    const state = await this.ensureProfileState();
    await this.api.joinTable({
      inviteCode,
      player: {
        playerId: identity.playerId,
        nickname: state.nickname,
        joinedAt: new Date().toISOString(),
        pubkeyHex: identity.publicKeyHex,
        arkAddress: wallet.arkAddress,
        boardingAddress: wallet.boardingAddress,
      },
      buyInSats,
    });
    const snapshot = await this.api.getTableByInvite(inviteCode);
    state.currentTable = {
      inviteCode,
      seatIndex: 1,
      tableId: snapshot.table.tableId,
    };
    await this.walletRuntime.saveState(state);
    this.profileState = state;
    this.snapshot = snapshot;
    this.peerStatus = "offline";
    this.notifyStateChange();
    return snapshot;
  }

  async getSnapshot() {
    const state = await this.ensureProfileState();
    if (!state.currentTable) {
      throw new Error("profile does not have a current table");
    }
    this.snapshot = await this.api.getTable(state.currentTable.tableId);
    this.notifyStateChange();
    return this.snapshot;
  }

  async getTranscript() {
    const state = await this.ensureProfileState();
    if (!state.currentTable) {
      throw new Error("profile does not have a current table");
    }
    return await this.api.getTranscript(state.currentTable.tableId);
  }

  async commitSeed(reveal = false) {
    const state = await this.ensureProfileState();
    const identity = await this.ensureIdentity();
    const snapshot = await this.getSnapshot();
    const seat = this.findMySeat(snapshot);
    const handNumber = snapshot.checkpoint?.handNumber ?? 1;
    const key = `${snapshot.table.tableId}:${handNumber}`;
    const seedHex = state.handSeeds[key] ?? randomHex(32);
    state.handSeeds[key] = seedHex;
    await this.walletRuntime.saveState(state);
    this.profileState = state;

    this.snapshot = await this.api.postCommitment(snapshot.table.tableId, {
      tableId: snapshot.table.tableId,
      handNumber,
      seatIndex: seat.seatIndex,
      playerId: identity.playerId,
      commitmentHash: buildCommitmentHash({
        tableId: snapshot.table.tableId,
        seatIndex: seat.seatIndex,
        playerId: identity.playerId,
        seedHex,
      }),
      revealSeed: reveal ? seedHex : undefined,
      revealedAt: reveal ? new Date().toISOString() : undefined,
    });
    this.notifyStateChange();
    return this.snapshot;
  }

  async sendAction(payload: SignedActionPayload) {
    const identity = await this.ensureIdentity();
    const snapshot = await this.getSnapshot();
    const seat = this.findMySeat(snapshot);
    if (!snapshot.checkpoint) {
      throw new Error("no active checkpoint");
    }
    await this.connectCurrentTable();
    const unsignedAction = {
      tableId: snapshot.table.tableId,
      handId: snapshot.checkpoint.handId,
      checkpointId: snapshot.checkpoint.checkpointId,
      clientSeq: Date.now(),
      actorPlayerId: identity.playerId,
      actorSeatIndex: seat.seatIndex,
      sentAt: new Date().toISOString(),
      signerPubkeyHex: identity.publicKeyHex,
      payload,
    };
    const signatureHex = await this.walletRuntime.signMessage(this.profileName, JSON.stringify(unsignedAction));
    const action = {
      ...unsignedAction,
      signatureHex,
    };
    this.socket?.send({
      type: "signed-action",
      action,
    });
    try {
      await this.relayPeerMessage(JSON.stringify({ type: "mirror-action", action }));
    } catch (error) {
      this.logger.info("failed to relay mirrored action", { error: (error as Error).message });
    }
    this.notifyStateChange();
  }

  async sendPeerMessage(message: string) {
    await this.relayPeerMessage(message);
    this.notifyStateChange();
  }

  async waitForCondition(
    description: string,
    predicate: (snapshot: TableSnapshot | null, peerStatus: string) => boolean,
    timeoutMs = 30_000,
  ) {
    if (predicate(this.snapshot, this.peerStatus)) {
      return this.snapshot;
    }

    return await new Promise<TableSnapshot>((resolve, reject) => {
      const check = () => {
        void (async () => {
          const tableId = this.profileState?.currentTable?.tableId;
          if (tableId) {
            try {
              this.snapshot = await this.api.getTable(tableId);
            } catch {
              // Best-effort refresh; websocket state may still be enough.
            }
          }

          if (predicate(this.snapshot, this.peerStatus) && this.snapshot) {
            this.checkpointWaiters = this.checkpointWaiters.filter((waiter) => waiter.check !== check);
            clearInterval(interval);
            clearTimeout(timeout);
            resolve(this.snapshot);
          }
        })();
      };
      const timeout = setTimeout(() => {
        this.checkpointWaiters = this.checkpointWaiters.filter((waiter) => waiter.check !== check);
        clearInterval(interval);
        reject(new Error(`timed out waiting for ${description}`));
      }, timeoutMs);
      const interval = setInterval(check, 100);

      this.checkpointWaiters.push({
        check,
        description,
        interval,
        predicate: (snapshot) => predicate(snapshot, this.peerStatus),
        reject,
        resolve,
        timeout,
      });
      check();
    });
  }

  async walletSummary() {
    this.wallet = await this.walletRuntime.getWallet(this.profileName);
    this.notifyStateChange();
    return this.wallet;
  }

  async walletDeposit(amountSats: number) {
    const quote = await this.walletRuntime.createDepositQuote(this.profileName, amountSats);
    this.wallet = await this.walletRuntime.getWallet(this.profileName);
    this.notifyStateChange();
    return quote;
  }

  async walletFaucet(amountSats: number) {
    await this.walletRuntime.nigiriFaucet(this.profileName, amountSats);
    this.notifyStateChange();
  }

  async walletOffboard(address: string, amountSats?: number) {
    const result = await this.walletRuntime.offboard(this.profileName, address, amountSats);
    this.notifyStateChange();
    return result;
  }

  async walletOnboard() {
    const result = await this.walletRuntime.onboard(this.profileName);
    this.notifyStateChange();
    return result;
  }

  async walletWithdraw(amountSats: number, invoice: string) {
    const status = await this.walletRuntime.submitWithdrawal(this.profileName, amountSats, invoice);
    this.wallet = await this.walletRuntime.getWallet(this.profileName);
    this.notifyStateChange();
    return status;
  }

  close() {
    this.closed = true;
    this.socket?.close();
    this.socket = undefined;
    this.peerStatus = "offline";
    for (const waiter of this.checkpointWaiters) {
      clearInterval(waiter.interval);
      clearTimeout(waiter.timeout);
      waiter.reject(new Error("client closed"));
    }
    this.checkpointWaiters = [];
    this.notifyStateChange();
  }

  currentState(): PlayerRuntimeState {
    return {
      identity: this.identity,
      peerMessages: [...this.peerMessages],
      peerStatus: this.peerStatus,
      profile: this.profileState,
      snapshot: this.snapshot,
      wallet: this.wallet,
    };
  }

  private async ensureIdentity() {
    if (!this.identity) {
      await this.bootstrap();
    }
    if (!this.identity) {
      throw new Error("identity is not initialized");
    }
    return this.identity;
  }

  private async ensureProfileState() {
    if (!this.profileState) {
      await this.bootstrap();
    }
    if (!this.profileState) {
      throw new Error("profile state is not initialized");
    }
    return this.profileState;
  }

  private async ensureWallet() {
    this.wallet = this.wallet ?? (await this.walletRuntime.getWallet(this.profileName));
    return this.wallet;
  }

  private findMySeat(snapshot: TableSnapshot) {
    const identity = this.identity;
    const seat = snapshot.seats.find((candidate) => candidate.player.playerId === identity?.playerId);
    if (!seat) {
      throw new Error("player is not seated at the table");
    }
    return seat;
  }

  private async handleServerEvent(event: ServerSocketEvent) {
    if (this.closed) {
      return;
    }
    if (event.type === "table-snapshot") {
      this.snapshot = event.snapshot;
      this.syncPeerStatusFromSnapshot();
      await this.maybePostTimeoutDelegation();
      this.notifyStateChange();
      this.resolveWaiters();
      return;
    }

    if (event.type === "checkpoint" && this.snapshot) {
      this.snapshot = {
        ...this.snapshot,
        checkpoint: event.checkpoint,
      };
      await this.maybePostTimeoutDelegation();
      this.notifyStateChange();
      this.resolveWaiters();
      return;
    }

    if (event.type === "peer-message") {
      this.recordPeerMessage(event.message);
      this.notifyStateChange();
      this.resolveWaiters();
      return;
    }

    if (event.type === "presence") {
      this.logger.info(`${event.playerId} is ${event.status}`);
      if (event.playerId !== this.identity?.playerId) {
        this.peerStatus = event.status === "online" ? "relay" : "offline";
      }
      this.notifyStateChange();
      this.resolveWaiters();
      return;
    }

    if (event.type === "error") {
      this.logger.error(event.message);
      this.notifyStateChange();
    }
  }

  private async maybePostTimeoutDelegation() {
    if (this.closed) {
      return;
    }
    const snapshot = this.snapshot;
    if (!snapshot?.checkpoint || !this.identity) {
      return;
    }
    const seat = snapshot.seats.find((candidate) => candidate.player.playerId === this.identity?.playerId);
    if (
      !seat ||
      snapshot.checkpoint.phase === "settled" ||
      snapshot.checkpoint.actingSeatIndex !== seat.seatIndex ||
      snapshot.pendingDelegations.some((delegation) => delegation.checkpointId === snapshot.checkpoint?.checkpointId)
    ) {
      return;
    }
    const wallet = await this.ensureWallet();
    try {
      this.snapshot = await this.api.postDelegation(
        snapshot.table.tableId,
        await this.walletRuntime.createTimeoutDelegation(this.profileName, {
          tableId: snapshot.table.tableId,
          checkpointId: snapshot.checkpoint.checkpointId,
          actingSeatIndex: seat.seatIndex,
          delegatedPlayerId: this.identity.playerId,
          settlementAddress: wallet.arkAddress,
          validAfter: snapshot.checkpoint.nextActionDeadline ?? new Date().toISOString(),
          expiresAt: new Date(
            Date.parse(snapshot.checkpoint.nextActionDeadline ?? new Date().toISOString()) + 60_000,
          ).toISOString(),
        }),
      );
      this.notifyStateChange();
    } catch (error) {
      if (!this.closed) {
        this.logger.error(`failed to post timeout delegation: ${(error as Error).message}`);
      }
    }
  }

  private async relayPeerMessage(message: string) {
    await this.connectCurrentTable();
    const snapshot = this.snapshot ?? (await this.getSnapshot());
    const identity = await this.ensureIdentity();
    const opponentSeat = this.findOpponentSeat(snapshot);
    if (!opponentSeat) {
      throw new Error("opponent is not connected to the table");
    }
    this.socket?.send({
      type: "peer-message",
      tableId: snapshot.table.tableId,
      fromPlayerId: identity.playerId,
      targetPlayerId: opponentSeat.player.playerId,
      message,
    });
  }

  private findOpponentSeat(snapshot: TableSnapshot) {
    return snapshot.seats.find(
      (candidate) => candidate.player.playerId !== this.identity?.playerId && candidate.player.playerId !== "open-seat",
    );
  }

  private recordPeerMessage(message: string) {
    this.peerMessages.unshift(message);
    if (this.peerMessages.length > 12) {
      this.peerMessages.length = 12;
    }
  }

  private resolveWaiters() {
    if (!this.snapshot) {
      return;
    }
    const resolved = this.checkpointWaiters.filter((waiter) => waiter.predicate(this.snapshot));
    this.checkpointWaiters = this.checkpointWaiters.filter((waiter) => !resolved.includes(waiter));
    for (const waiter of resolved) {
      clearInterval(waiter.interval);
      clearTimeout(waiter.timeout);
      waiter.resolve(this.snapshot);
    }
  }

  private notifyStateChange() {
    this.onStateChange?.(this.currentState());
  }

  private syncPeerStatusFromSnapshot() {
    if (!this.socket || !this.snapshot || !this.findOpponentSeat(this.snapshot)) {
      this.peerStatus = "offline";
    }
  }
}

export function deriveLocalHoleCards(snapshot: TableSnapshot, playerId: string, seatIndex: number) {
  const checkpoint = snapshot.checkpoint;
  if (!checkpoint) {
    return ["XX", "XX"] as const;
  }

  if (checkpoint.phase === "settled" && checkpoint.holeCardsByPlayerId[playerId]) {
    return checkpoint.holeCardsByPlayerId[playerId]!;
  }

  const reveals = snapshot.commitments.filter((commitment) => commitment.revealSeed);
  if (reveals.length !== 2) {
    return ["XX", "XX"] as const;
  }

  const deckSeedHex = deriveDeckSeed({
    tableId: snapshot.table.tableId,
    handNumber: checkpoint.handNumber,
    commitments: snapshot.commitments,
    reveals,
  });
  const hand = createHoldemHand({
    handId: checkpoint.handId,
    handNumber: checkpoint.handNumber,
    deckSeedHex,
    dealerSeatIndex: checkpoint.dealerSeatIndex as 0 | 1,
    smallBlindSats: snapshot.table.smallBlindSats,
    bigBlindSats: snapshot.table.bigBlindSats,
    seats: [
      {
        playerId: snapshot.seats[0]!.player.playerId,
        stackSats:
          checkpoint.playerStacks[snapshot.seats[0]!.player.playerId] ?? snapshot.seats[0]!.buyInSats,
      },
      {
        playerId: snapshot.seats[1]!.player.playerId,
        stackSats:
          checkpoint.playerStacks[snapshot.seats[1]!.player.playerId] ?? snapshot.seats[1]!.buyInSats,
      },
    ],
  });
  return hand.players[seatIndex]!.holeCards;
}
