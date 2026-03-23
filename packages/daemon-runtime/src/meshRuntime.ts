import { createHash } from "node:crypto";

import {
  applyHoldemAction,
  createDeterministicDeck,
  createHoldemHand,
  toCheckpointShape,
  type HoldemState,
} from "@parker/game-engine";
import {
  parseSignedTableEvent,
  publicTableStateSchema,
  type BuyInConfirm,
  type CooperativeTableSnapshot,
  type HandAuditBundle,
  type HostFailoverAcceptance,
  type HostLease,
  type HostLeaseSignature,
  type MeshPlayerActionPayload,
  type MeshRequestBody,
  type MeshResponseBody,
  type MeshSeatedPlayer,
  type MeshTableConfig,
  type MeshWireFrame,
  type PeerAddress,
  type PlayerActionIntent,
  type PlayerJoinIntent,
  type PrivateInvite,
  type PublicTableState,
  type PublicTableUpdate,
  type SignedTableAdvertisement,
  type SignedTableEvent,
  type TableEventBody,
  type TableFundsOperation,
  type TableSnapshotSignature,
} from "@parker/protocol";
import {
  buildIdentityBinding,
  createArkadeTableFundsProvider,
  createMockTableFundsProvider,
  createScopedIdentity,
  decryptScopedPayload,
  encryptScopedPayload,
  signStructuredData,
  stableStringify,
  verifyTableFundsOperationSignature,
  verifyIdentityBinding,
  verifyStructuredData,
  type LocalIdentity,
  type ScopedIdentity,
  type TableCheckpointRecord,
  type TableFundsProvider,
} from "@parker/settlement";

import type { CliRuntimeConfig } from "./config.js";
import { IndexerClient } from "./indexerClient.js";
import { CliLogger } from "./logger.js";
import { MeshStore } from "./meshStore.js";
import type {
  FundsWarning,
  MeshRuntimeMode,
  MeshRuntimeState,
  MeshTableContext,
  TableSummary,
} from "./meshTypes.js";
import { PeerTransport } from "./peerTransport.js";
import { ProfileStore, type KnownPeerState, type PlayerProfileState } from "./profileStore.js";
import { JsonTableFundsStateStore } from "./tableFundsStateStore.js";
import { CliWalletRuntime } from "./walletRuntime.js";

const HOST_HEARTBEAT_INTERVAL_MS = 1_000;
const HOST_LEASE_DURATION_MS = 4_000;
const HOST_FAILURE_TIMEOUT_MS = 3_500;
const AUTO_NEXT_HAND_DELAY_MS = 1_000;

interface CreateMeshTableParams {
  bigBlindSats?: number;
  buyInMaxSats?: number;
  buyInMinSats?: number;
  name?: string;
  public?: boolean;
  smallBlindSats?: number;
  spectatorsAllowed?: boolean;
  witnessPeerIds?: string[];
}

interface MeshTableView {
  config: MeshTableConfig;
  events: SignedTableEvent[];
  latestSnapshot: CooperativeTableSnapshot | null;
  latestFullySignedSnapshot: CooperativeTableSnapshot | null;
  publicState: PublicTableState | null;
}

type EventDisposition = "accept" | "ignore" | "queue";

export class MeshRuntime {
  private readonly activeFailovers = new Set<string>();
  private readonly autoHandTimers = new Map<string, NodeJS.Timeout>();
  private readonly contextByTableId = new Map<string, MeshTableContext>();
  private fundsProvider: TableFundsProvider | undefined;
  private isClosing = false;
  private indexer: IndexerClient | undefined;
  private interval: NodeJS.Timeout | undefined;
  private mode: MeshRuntimeMode = "player";
  private peerIdentity?: ScopedIdentity;
  private protocolIdentity?: ScopedIdentity;
  private store: MeshStore;
  private transport: PeerTransport | undefined;
  private walletIdentity?: LocalIdentity;

  constructor(
    private readonly profileName: string,
    private readonly config: CliRuntimeConfig,
    private readonly logger: CliLogger,
    private readonly profileStore = new ProfileStore(config.profileDir),
    private readonly walletRuntime = new CliWalletRuntime(config, profileStore),
    private readonly onStateChange?: (state: MeshRuntimeState) => void,
  ) {
    this.store = new MeshStore(
      `${config.daemonDir}/${profileName.replace(/[^a-zA-Z0-9_-]/g, "_")}.state`,
    );
  }

  async bootstrap(nickname?: string) {
    const bootstrap = await this.walletRuntime.bootstrap(this.profileName, nickname);
    const state = await this.ensureProfileState(bootstrap.state.nickname);
    this.walletIdentity = bootstrap.identity;
    this.protocolIdentity = createScopedIdentity("protocol", state.protocolPrivateKeyHex);
    this.peerIdentity = createScopedIdentity("peer", state.peerPrivateKeyHex);
    this.indexer = this.config.indexerUrl ? new IndexerClient(this.config.indexerUrl) : undefined;
    this.emitState();
    return {
      peerId: this.peerIdentity.id,
      peerPublicKeyHex: this.peerIdentity.publicKeyHex,
      protocolId: this.protocolIdentity.id,
      protocolPublicKeyHex: this.protocolIdentity.publicKeyHex,
      wallet: bootstrap.wallet,
      walletPlayerId: this.walletIdentity.playerId,
    };
  }

  async start(mode: MeshRuntimeMode) {
    this.isClosing = false;
    this.mode = mode;
    await this.bootstrap();
    if (!this.peerIdentity || !this.protocolIdentity) {
      throw new Error("mesh identities are unavailable");
    }
    if (!this.transport) {
      this.transport = new PeerTransport({
        alias: (await this.ensureProfileState()).nickname,
        onFrame: async (peerId, frame) => {
          await this.handlePeerFrame(peerId, frame);
        },
        onPeerSeen: () => {},
        peerIdentity: this.peerIdentity,
        protocolIdentity: this.protocolIdentity,
        roles: [this.mode],
      });
      const peerUrl = await this.transport.start(this.config.peerHost, this.config.peerPort);
      await this.persistPeerUrl(peerUrl);
    }
    await this.loadPersistedTables();
    await this.reconnectKnownPeers();
    if (!this.interval) {
      this.interval = setInterval(() => {
        void this.tick();
      }, 500);
    }
    this.emitState();
  }

  close() {
    this.isClosing = true;
    if (this.interval) {
      clearInterval(this.interval);
      this.interval = undefined;
    }
    for (const timer of this.autoHandTimers.values()) {
      clearTimeout(timer);
    }
    this.autoHandTimers.clear();
    this.transport?.close();
    this.transport = undefined;
  }

  currentState(): MeshRuntimeState {
    const peerState =
      this.peerIdentity && this.protocolIdentity && this.walletIdentity
        ? {
            peerId: this.peerIdentity.id,
            ...(this.transport?.getListenUrl()
              ? { peerUrl: this.transport.getListenUrl() }
              : {}),
            protocolId: this.protocolIdentity.id,
            walletPlayerId: this.walletIdentity.playerId,
          }
        : {};
    return {
      fundsWarnings: this.buildFundsWarnings(),
      mode: this.mode,
      peer: peerState,
      peers: this.transport?.listKnownPeers() ?? [],
      publicTables: [...new Map(this.contextPublicAds().map((ad) => [ad.tableId, ad])).values()],
      tables: [...this.contextByTableId.values()].map((context) => this.toTableSummary(context)),
    };
  }

  async networkPeers() {
    return this.transport?.listKnownPeers() ?? [];
  }

  async addBootstrapPeer(peerUrl: string, alias?: string, roles: MeshRuntimeMode[] = []) {
    await this.ensureStarted();
    const provisionalPeerId = `peer-${createHash("sha256").update(peerUrl).digest("hex").slice(0, 20)}`;
    const peer: PeerAddress = {
      ...(alias ? { alias } : {}),
      peerId: provisionalPeerId,
      peerUrl,
      roles,
    };
    this.transport?.registerKnownPeer(peer);
    await this.rememberPeer(peer);
    const confirmedPeerPromise = this.transport?.waitForConfirmedPeer(peerUrl, 1_000).catch(() => undefined);
    try {
      await this.transport?.request(peer.peerId, { type: "peer-cache-request" });
    } catch {
      // Peer may not know us yet; keep the hint anyway.
    }
    const resolvedPeer =
      (await confirmedPeerPromise) ??
      (await this.waitForKnownPeer(peerUrl, provisionalPeerId, 1_000)) ??
      peer;
    await this.rememberPeer(resolvedPeer);
    this.emitState();
    return resolvedPeer;
  }

  async listPublicTables() {
    if (this.indexer) {
      try {
        return await this.indexer.fetchPublicTables();
      } catch (error) {
        this.logger.info("public indexer unavailable, falling back to local cache", {
          error: (error as Error).message,
        });
      }
    }
    return this.contextPublicAds().map((advertisement) => ({
      advertisement,
      latestState: this.contextByTableId.get(advertisement.tableId)?.publicState ?? null,
      recentUpdates: [] as PublicTableUpdate[],
    }));
  }

  async createTable(input: CreateMeshTableParams = {}) {
    await this.ensureStarted();
    if (!this.peerIdentity || !this.protocolIdentity || !this.walletIdentity || !this.transport) {
      throw new Error("mesh runtime is not initialized");
    }
    const tableId = crypto.randomUUID();
    const createdAt = new Date().toISOString();
    const visibility = input.public ? "public" : "private";
    const config: MeshTableConfig = {
      bigBlindSats: input.bigBlindSats ?? 100,
      buyInMaxSats: input.buyInMaxSats ?? 10_000,
      buyInMinSats: input.buyInMinSats ?? 4_000,
      createdAt,
      dealerMode: "host-dealer-v1",
      hostPeerId: this.peerIdentity.id,
      hostPlaysAllowed: false,
      networkId: this.config.network,
      occupiedSeats: 0,
      publicSpectatorDelayHands: 1,
      seatCount: 2,
      smallBlindSats: input.smallBlindSats ?? 50,
      spectatorsAllowed: input.spectatorsAllowed ?? true,
      status: "announced",
      tableId,
      name: input.name ?? `${this.profileName} table`,
      visibility,
    };

    const witnessPeerIds =
      input.witnessPeerIds ??
      (this.transport.listKnownPeers().filter((peer) => peer.roles.includes("witness")).map((peer) => peer.peerId));
    const context = this.createContext({
      config,
      currentEpoch: 1,
      currentHostPeerId: this.peerIdentity.id,
      currentHostPeerUrl: this.transport.getListenUrl(),
      role: "host",
      witnessSet: witnessPeerIds,
    });
    for (const witnessPeerId of witnessPeerIds) {
      const knownWitness = this.transport.listKnownPeers().find((peer) => peer.peerId === witnessPeerId);
      if (knownWitness) {
        context.peerAddresses.set(witnessPeerId, knownWitness);
      }
    }
    this.contextByTableId.set(tableId, context);
    const advertisement = this.buildAdvertisement(context);
    context.advertisement = advertisement;
    await this.appendLocalEvent(context, {
      advertisement,
      table: config,
      type: "TableAnnounce",
    });
    const lease = await this.buildHostLease(tableId, 1, witnessPeerIds);
    await this.appendLocalEvent(context, {
      lease,
      type: "HostLeaseGranted",
    });
    await this.store.upsertPublicAd(advertisement);
    await this.saveMeshTableReference(tableId, {
      config,
      currentEpoch: 1,
      hostPeerId: this.peerIdentity.id,
      hostPeerUrl: this.transport.getListenUrl(),
      role: "host",
      tableId,
      visibility,
    });
    if (visibility === "public") {
      await this.announceTable(tableId);
    }
    this.emitState();
    return {
      inviteCode: this.encodeInvite({
        hostPeerId: this.peerIdentity.id,
        hostPeerUrl: this.transport.getListenUrl(),
        networkId: this.config.network,
        protocolVersion: "poker/v1",
        tableId,
      }),
      table: config,
    };
  }

  async announceTable(tableId?: string) {
    await this.ensureStarted();
    const context = this.requireContext(tableId);
    const advertisement = context.advertisement ?? this.buildAdvertisement(context);
    context.advertisement = advertisement;
    await this.store.upsertPublicAd(advertisement);
    if (this.indexer && context.config.visibility === "public") {
      await this.indexer.announceTable(advertisement);
      await this.indexer.publishUpdate({
        advertisement,
        publicState: context.publicState,
        publishedAt: new Date().toISOString(),
        tableId: context.config.tableId,
        type: "PublicTableSnapshot",
      });
    }
    await this.transport?.broadcast(
      this.transport.listKnownPeers().map((peer) => peer.peerId),
      {
        ad: advertisement,
        kind: "public-ad",
      },
    );
    this.emitState();
    return advertisement;
  }

  async joinTable(inviteCode: string, buyInSats?: number) {
    await this.ensureStarted();
    if (!this.peerIdentity || !this.protocolIdentity || !this.walletIdentity || !this.transport) {
      throw new Error("mesh runtime is not initialized");
    }
    const invite = this.decodeInvite(inviteCode);
    const hostPeer: PeerAddress = {
      peerId: invite.hostPeerId,
      peerUrl: invite.hostPeerUrl,
      roles: ["host"],
    };
    this.transport.registerKnownPeer(hostPeer);
    await this.rememberPeer(hostPeer);
    if (!this.contextByTableId.has(invite.tableId)) {
      this.contextByTableId.set(
        invite.tableId,
        this.createContext({
          config: {
            bigBlindSats: 100,
            buyInMaxSats: 10_000,
            buyInMinSats: buyInSats ?? 4_000,
            createdAt: new Date().toISOString(),
            dealerMode: "host-dealer-v1",
            hostPeerId: invite.hostPeerId,
            hostPlaysAllowed: false,
            name: `Table ${invite.tableId.slice(0, 8)}`,
            networkId: invite.networkId,
            occupiedSeats: 0,
            publicSpectatorDelayHands: 1,
            seatCount: 2,
            smallBlindSats: 50,
            spectatorsAllowed: false,
            status: "seating",
            tableId: invite.tableId,
            visibility: "private",
          },
          currentEpoch: 1,
          currentHostPeerId: invite.hostPeerId,
          currentHostPeerUrl: invite.hostPeerUrl,
          role: "player",
          witnessSet: [],
        }),
      );
    }
    await this.saveMeshTableReference(invite.tableId, {
      config: this.cloneValue(this.requireContext(invite.tableId).config),
      currentEpoch: 1,
      hostPeerId: invite.hostPeerId,
      hostPeerUrl: invite.hostPeerUrl,
      role: "player",
      tableId: invite.tableId,
      visibility: "private",
    });

    const fundsProvider = this.createFundsProvider();
    const preparedBuyIn = await fundsProvider.prepareBuyIn(
      invite.tableId,
      this.walletIdentity.playerId,
      buyInSats ?? 4_000,
    );
    const profile = await this.ensureProfileState();
    const joinIntent = await this.buildJoinIntent(invite, profile.nickname, preparedBuyIn.amountSats);
    const joinResponse = await this.transport.request(invite.hostPeerId, {
      intent: joinIntent,
      preparedBuyIn,
      type: "join-request",
    });
    if (!joinResponse || joinResponse.type !== "join-response" || !joinResponse.accepted) {
      throw new Error(joinResponse?.type === "join-response" ? joinResponse.reason ?? "join failed" : "join failed");
    }

    const confirmedReceipt = await fundsProvider.confirmBuyIn(
      invite.tableId,
      this.walletIdentity.playerId,
      preparedBuyIn,
    );
    const buyInConfirmation = this.buildBuyInConfirm(invite.tableId, confirmedReceipt);
    const buyInResponse = await this.transport.request(invite.hostPeerId, {
      confirmation: buyInConfirmation,
      type: "buy-in-confirm",
    });
    if (!buyInResponse || buyInResponse.type !== "buy-in-response" || !buyInResponse.accepted) {
      throw new Error(buyInResponse?.type === "buy-in-response" ? buyInResponse.reason ?? "buy-in failed" : "buy-in failed");
    }
    this.emitState();
    return await this.waitForTableContext(invite.tableId);
  }

  async getTableState(tableId?: string): Promise<MeshTableView> {
    const targetTableId = tableId ?? this.contextByTableId.keys().next().value;
    if (targetTableId && !this.contextByTableId.has(targetTableId)) {
      await this.loadPersistedTable(targetTableId);
    }
    const context = this.requireContext(targetTableId);
    return {
      config: context.config,
      events: [...context.events],
      latestFullySignedSnapshot: context.latestFullySignedSnapshot,
      latestSnapshot: context.latestSnapshot,
      publicState: context.publicState,
    };
  }

  async sendAction(payload: MeshPlayerActionPayload, tableId?: string) {
    await this.ensureStarted();
    const context = this.requireContext(tableId);
    if (!this.protocolIdentity || !this.walletIdentity || !this.transport || !context.publicState?.handId) {
      throw new Error("cannot send an action without an active hand");
    }
    const seat = context.publicState.seatedPlayers.find(
      (candidate) => candidate.playerId === this.walletIdentity?.playerId,
    );
    if (!seat) {
      throw new Error("player is not seated at this table");
    }
    const intent = this.buildActionIntent(context, seat.seatIndex, payload);
    const response = await this.transport.request(context.currentHostPeerId, {
      intent,
      type: "action-request",
    });
    if (!response || response.type !== "action-response" || !response.accepted) {
      throw new Error(response?.type === "action-response" ? response.reason ?? "action rejected" : "action rejected");
    }
    return this.currentState();
  }

  async rotateHost(tableId?: string) {
    await this.ensureStarted();
    const context = this.requireContext(tableId);
    if (context.role === "witness") {
      await this.triggerFailover(context, "manual witness rotation");
      return this.currentState();
    }
    if (context.role === "host") {
      throw new Error("manual host rotation must be initiated by a witness so the failover quorum can be enforced");
    }
    throw new Error("this daemon cannot rotate the host for the requested table");
  }

  async cashOut(tableId?: string) {
    const context = this.requireContext(tableId);
    if (!this.walletIdentity) {
      throw new Error("wallet identity is unavailable");
    }
    const latestSnapshot = context.latestFullySignedSnapshot;
    if (!latestSnapshot) {
      throw new Error("no fully signed cooperative checkpoint is available yet");
    }
    const balance = latestSnapshot.chipBalances[this.walletIdentity.playerId] ?? 0;
    const checkpointHash = this.snapshotHash(latestSnapshot);
    const receipt = await this.createFundsProvider().cooperativeCashOut(
      context.config.tableId,
      this.walletIdentity.playerId,
      balance,
      checkpointHash,
    );
    context.buyInReceipts.set(this.walletIdentity.playerId, receipt);
    this.emitState();
    return {
      balance,
      checkpointHash,
      receipt,
    };
  }

  async renewFunds(tableId?: string) {
    const context = this.requireContext(tableId);
    const receipts = await this.createFundsProvider().renewTablePositions(context.config.tableId);
    for (const receipt of receipts) {
      context.buyInReceipts.set(receipt.playerId, receipt);
    }
    this.emitState();
    return receipts;
  }

  async emergencyExit(tableId?: string) {
    const context = this.requireContext(tableId);
    if (!this.walletIdentity) {
      throw new Error("wallet identity is unavailable");
    }
    const latestSnapshot = context.latestFullySignedSnapshot;
    if (!latestSnapshot) {
      throw new Error("cannot emergency exit without a cooperative checkpoint");
    }
    const balance = latestSnapshot.chipBalances[this.walletIdentity.playerId] ?? 0;
    const checkpointHash = this.snapshotHash(latestSnapshot);
    const receipt = await this.createFundsProvider().emergencyExit(
      context.config.tableId,
      this.walletIdentity.playerId,
      checkpointHash,
      balance,
    );
    context.buyInReceipts.set(this.walletIdentity.playerId, receipt);
    this.emitState();
    return {
      balance,
      checkpointHash,
      receipt,
    };
  }

  private async appendLocalEvent(context: MeshTableContext, body: TableEventBody, senderRole: "host" | "witness" = "host") {
    const event = this.buildSignedEvent(context, body, senderRole);
    await this.acceptEvent(context, event, true);
    const peerIds = new Set<string>();
    for (const player of context.pendingPlayers.values()) {
      if (player.peerId !== this.peerIdentity?.id) {
        peerIds.add(player.peerId);
      }
    }
    for (const witnessPeerId of context.witnessSet) {
      if (witnessPeerId !== this.peerIdentity?.id) {
        peerIds.add(witnessPeerId);
      }
    }
    if (peerIds.size > 0) {
      await this.transport?.broadcast([...peerIds], {
        event,
        kind: "event",
      });
    }
    if (context.role === "host" && context.config.visibility === "public") {
      await this.publishPublicState(context, body);
    }
    return event;
  }

  private async acceptEvent(context: MeshTableContext, event: SignedTableEvent, persist: boolean) {
    const parsed = this.cloneValue(parseSignedTableEvent(event));
    const unsignedEvent = this.unsignedEvent(parsed);
    if (!verifyStructuredData(parsed.senderProtocolPubkeyHex, unsignedEvent, parsed.signature)) {
      throw new Error(`invalid table event signature for ${parsed.messageType}`);
    }

    const nextHash = this.eventHash(parsed);
    if (
      context.events.some((candidate) => this.eventHash(candidate) === nextHash) ||
      context.pendingEvents.has(nextHash)
    ) {
      return;
    }

    const disposition = this.classifyEvent(context, parsed);
    if (disposition === "ignore") {
      return;
    }
    if (disposition === "queue") {
      context.pendingEvents.set(nextHash, parsed);
      return;
    }

    this.assertEventSemantics(context, parsed);
    await this.commitAcceptedEvent(context, parsed, nextHash, persist);
    await this.flushPendingEvents(context);
  }

  private async commitAcceptedEvent(
    context: MeshTableContext,
    event: SignedTableEvent,
    nextHash: string,
    persist: boolean,
  ) {
    const canonicalEvent = this.cloneValue(event);
    context.events.push(canonicalEvent);
    context.lastEventHash = nextHash;
    this.applyEventToContext(context, canonicalEvent);
    if (persist) {
      await this.store.appendEvent(context.config.tableId, canonicalEvent);
      if (canonicalEvent.body.type === "WitnessSnapshot") {
        await this.store.appendSnapshot(context.config.tableId, canonicalEvent.body.snapshot);
      }
    }
    if (
      canonicalEvent.body.type === "PrivateCardDelivery" &&
      this.walletIdentity &&
      canonicalEvent.body.recipientPlayerId === this.walletIdentity.playerId &&
      this.protocolIdentity
    ) {
      const cards = decryptScopedPayload<[string, string]>({
        envelope: canonicalEvent.body.encryptedPayload,
        recipientPrivateKeyHex: this.protocolIdentity.privateKeyHex,
        scope: `${canonicalEvent.tableId}:${canonicalEvent.handId ?? ""}:cards`,
        senderPublicKeyHex: canonicalEvent.senderProtocolPubkeyHex,
      });
      context.privateState.myHoleCardsByHandId[canonicalEvent.body.handId] = cards;
      await this.store.savePrivateState(context.config.tableId, context.privateState);
    }
    if (
      canonicalEvent.body.type === "WitnessSnapshot" &&
      this.isSettlementBoundarySnapshot(canonicalEvent.body.snapshot)
    ) {
      await this.recordSettlementCheckpoint(context, canonicalEvent.body.snapshot);
    }
    context.lastHostHeartbeatAt = Date.now();
    this.emitState();
  }

  private applyEventToContext(context: MeshTableContext, event: SignedTableEvent) {
    const { body } = event;
    switch (body.type) {
      case "TableAnnounce":
        context.config = this.cloneValue(body.table);
        if (body.advertisement) {
          context.advertisement = this.cloneValue(body.advertisement);
        }
        break;
      case "JoinAccepted":
        this.transport?.registerKnownPeer({
          alias: body.intent.player.nickname,
          peerId: body.intent.peerId,
          peerUrl: body.intent.peerUrl,
          protocolPubkeyHex: body.intent.protocolPubkeyHex,
          roles: ["player"],
        });
        context.pendingPlayers.set(body.intent.player.playerId, {
          arkAddress: body.intent.player.arkAddress,
          buyInSats: 0,
          nickname: body.intent.player.nickname,
          peerId: body.intent.peerId,
          playerId: body.intent.player.playerId,
          protocolPubkeyHex: body.intent.protocolPubkeyHex,
          seatIndex: body.seatIndex,
          status: "waiting",
          walletPubkeyHex: body.intent.identityBinding.walletPubkeyHex,
        });
        context.peerAddresses.set(body.intent.peerId, {
          alias: body.intent.player.nickname,
          peerId: body.intent.peerId,
          peerUrl: body.intent.peerUrl,
          protocolPubkeyHex: body.intent.protocolPubkeyHex,
          roles: ["player"],
        });
        break;
      case "SeatLocked": {
        const player = context.pendingPlayers.get(body.playerId);
        if (player) {
          player.buyInSats = body.buyInSats;
          player.status = "active";
          context.pendingPlayers.set(body.playerId, player);
        }
        context.config.occupiedSeats = context.pendingPlayers.size;
        context.config.status = "seating";
        break;
      }
      case "BuyInLocked":
        context.buyInReceipts.set(body.receipt.playerId, body.receipt);
        break;
      case "TableReady":
        context.config.status = "ready";
        if (body.publicState) {
          context.publicState = this.cloneValue(body.publicState);
        }
        break;
      case "HandStart":
      case "StreetStart":
      case "ActionAccepted":
      case "StreetClosed":
      case "ShowdownReveal":
      case "HandResult":
        context.publicState = this.cloneValue(body.publicState);
        context.config.status = body.publicState.status;
        break;
      case "HandAbort":
        this.ensureAutoTimerCleared(context.config.tableId);
        context.config.status = "aborted";
        delete context.privateState.activeHand;
        if (context.latestFullySignedSnapshot) {
          context.publicState = this.publicStateFromSnapshot(context, context.latestFullySignedSnapshot);
          context.config.status = "ready";
        }
        break;
      case "TableClosed":
        context.config.status = "closed";
        break;
      case "WitnessSnapshot": {
        const snapshot = this.cloneValue(body.snapshot);
        const existingIndex = context.snapshots.findIndex(
          (candidate) => candidate.snapshotId === snapshot.snapshotId,
        );
        if (existingIndex === -1) {
          context.snapshots.push(snapshot);
        } else {
          context.snapshots[existingIndex] = snapshot;
        }
        context.latestSnapshot = snapshot;
        if (this.isSettlementBoundarySnapshot(snapshot)) {
          context.latestFullySignedSnapshot = snapshot;
        }
        break;
      }
      case "HostLeaseGranted":
        context.currentEpoch = body.lease.epoch;
        context.currentHostPeerId = body.lease.hostPeerId;
        {
          const hostSignature = body.lease.signatures.find(
            (signature) => signature.signerPeerId === body.lease.hostPeerId,
          );
          if (hostSignature) {
            const existing = context.peerAddresses.get(body.lease.hostPeerId);
            context.peerAddresses.set(body.lease.hostPeerId, {
              peerId: body.lease.hostPeerId,
              peerUrl: existing?.peerUrl ?? context.currentHostPeerUrl,
              protocolPubkeyHex: hostSignature.signerPubkeyHex,
              roles: ["host"],
              ...(existing?.alias ? { alias: existing.alias } : {}),
            });
          }
        }
        context.witnessSet = this.resolveWitnessSet(context, body.lease);
        break;
      case "HostRotated":
        context.currentEpoch = body.newEpoch;
        context.currentHostPeerId = body.newHostPeerId;
        {
          const hostSignature = body.lease.signatures.find(
            (signature) => signature.signerPeerId === body.lease.hostPeerId,
          );
          const existing = context.peerAddresses.get(body.newHostPeerId);
          context.currentHostPeerUrl = existing?.peerUrl ?? context.currentHostPeerUrl;
          if (hostSignature) {
            context.peerAddresses.set(body.newHostPeerId, {
              peerId: body.newHostPeerId,
              peerUrl: existing?.peerUrl ?? context.currentHostPeerUrl,
              protocolPubkeyHex: hostSignature.signerPubkeyHex,
              roles: ["host"],
              ...(existing?.alias ? { alias: existing.alias } : {}),
            });
          }
        }
        context.witnessSet = this.resolveWitnessSet(context, body.lease);
        if (body.newHostPeerId === this.peerIdentity?.id) {
          context.currentHostPeerUrl = this.transport?.getListenUrl() ?? context.currentHostPeerUrl;
          context.role = "host";
        } else if (context.role === "host") {
          context.role = "player";
        }
        break;
      default:
        break;
    }
  }

  private async beginNextHand(context: MeshTableContext) {
    if (!this.walletIdentity || !this.transport || context.role !== "host") {
      return;
    }
    if (context.pendingPlayers.size !== 2 || context.config.status === "closed") {
      return;
    }
    if (context.publicState && context.publicState.phase !== "settled" && context.publicState.handId) {
      return;
    }
    const handNumber = (context.publicState?.handNumber ?? 0) + 1;
    const seats = [...context.pendingPlayers.values()].sort((left, right) => left.seatIndex - right.seatIndex);
    const balances =
      context.latestFullySignedSnapshot?.chipBalances ??
      Object.fromEntries(seats.map((seat) => [seat.playerId, seat.buyInSats]));
    const deckSeedHex = createHash("sha256")
      .update(`${context.config.tableId}:${handNumber}:${Date.now()}`)
      .digest("hex");
    const hand = createHoldemHand({
      bigBlindSats: context.config.bigBlindSats,
      dealerSeatIndex: ((handNumber - 1) % 2) as 0 | 1,
      deckSeedHex,
      handId: crypto.randomUUID(),
      handNumber,
      seats: seats.map((seat) => ({
        playerId: seat.playerId,
        stackSats: balances[seat.playerId] ?? seat.buyInSats,
      })) as [{ playerId: string; stackSats: number }, { playerId: string; stackSats: number }],
      smallBlindSats: context.config.smallBlindSats,
    });

    const deckOrder = createDeterministicDeck(deckSeedHex).map((card) => card.code) as HandAuditBundle["deckOrder"];
    const commitmentRoot = createHash("sha256")
      .update(`${deckSeedHex}:${deckOrder.join(",")}`)
      .digest("hex");
    const auditBundle: HandAuditBundle = {
      commitmentRoot,
      deckOrder,
      deckSeedHex,
      revealedAt: new Date().toISOString(),
    };
    context.handSetupInFlight = true;
    try {
      context.privateState.activeHand = hand;
      context.privateState.auditBundlesByHandId[hand.handId] = auditBundle;
      await this.store.savePrivateState(context.config.tableId, context.privateState);

      const publicState = this.publicStateFromHand(context, hand, commitmentRoot);
      await this.appendLocalEvent(context, {
        dealerSeatIndex: hand.dealerSeatIndex,
        handId: hand.handId,
        handNumber,
        publicState,
        type: "HandStart",
      });
      await this.appendLocalEvent(context, {
        commitment: {
          committedAt: new Date().toISOString(),
          mode: "host-dealer-v1",
          rootHash: commitmentRoot,
        },
        handId: hand.handId,
        type: "DealerCommit",
      });

      for (const player of seats) {
        const envelope = encryptScopedPayload({
          payload: hand.players[player.seatIndex]!.holeCards,
          recipientPublicKeyHex: player.protocolPubkeyHex,
          scope: `${context.config.tableId}:${hand.handId}:cards`,
          senderPrivateKeyHex: this.protocolIdentity!.privateKeyHex,
        });
        await this.appendLocalEvent(context, {
          encryptedPayload: envelope,
          handId: hand.handId,
          proofHash: createHash("sha256")
            .update(`${commitmentRoot}:${player.playerId}:${hand.players[player.seatIndex]!.holeCards.join(",")}`)
            .digest("hex"),
          recipientPeerId: player.peerId,
          recipientPlayerId: player.playerId,
          type: "PrivateCardDelivery",
        });
      }

      await this.appendLocalEvent(context, {
        handId: hand.handId,
        publicState,
        street: publicState.phase ?? "preflop",
        type: "StreetStart",
      });
    } finally {
      context.handSetupInFlight = false;
    }
  }

  private async captureSnapshot(context: MeshTableContext, expectedEventHash = context.lastEventHash) {
    if (!this.protocolIdentity || !this.peerIdentity || !context.publicState) {
      return;
    }
    if (!expectedEventHash || context.lastEventHash !== expectedEventHash || this.hasSnapshotForEventHash(context, expectedEventHash)) {
      return;
    }
    const snapshotBase = this.buildSnapshotBase(context);
    if (snapshotBase.latestEventHash !== expectedEventHash) {
      return;
    }

    const selfRole = context.role === "witness" ? "witness" : "host";
    const selfSignature: TableSnapshotSignature = {
      signatureHex: signStructuredData(this.protocolIdentity, this.unsignedSnapshot(snapshotBase)),
      signedAt: new Date().toISOString(),
      signerPeerId: this.peerIdentity.id,
      signerPubkeyHex: this.protocolIdentity.publicKeyHex,
      signerRole: selfRole,
    };
    const signatures: TableSnapshotSignature[] = [
      {
        ...selfSignature,
      },
    ];
    const snapshotCandidate: CooperativeTableSnapshot = {
      ...snapshotBase,
      signatures: [selfSignature],
    };

    const remotePeerIds = new Set<string>();
    for (const player of context.publicState.seatedPlayers) {
      if (player.peerId !== this.peerIdentity.id) {
        remotePeerIds.add(player.peerId);
      }
    }
    for (const witnessPeerId of context.witnessSet) {
      if (witnessPeerId !== this.peerIdentity.id) {
        remotePeerIds.add(witnessPeerId);
      }
    }

    for (const peerId of remotePeerIds) {
      for (let attempt = 0; attempt < 8; attempt += 1) {
        try {
          const response = await this.transport?.request(peerId, {
            snapshot: snapshotCandidate,
            type: "snapshot-sign-request",
          });
          if (response?.type === "snapshot-sign-response") {
            if (this.verifySnapshotSignature(context, snapshotCandidate, response.signature)) {
              signatures.push(response.signature);
            }
            break;
          }
        } catch (error) {
          if (attempt === 7) {
            this.logger.info("snapshot signature request failed", {
              error: (error as Error).message,
              peerId,
              tableId: context.config.tableId,
            });
            break;
          }
          await new Promise<void>((resolve) => {
            setTimeout(resolve, 100);
          });
        }
      }
    }

    const snapshot: CooperativeTableSnapshot = {
      ...snapshotBase,
      signatures,
    };
    if (context.lastEventHash !== expectedEventHash || this.hasSnapshotForEventHash(context, expectedEventHash)) {
      return;
    }
    if (!this.hasRequiredSignatures(context, snapshot)) {
      const requiredPeerIds = this.requiredSnapshotPeerIds(context);
      const actualPeerIds = snapshot.signatures.map((signature) => signature.signerPeerId);
      this.logger.info("snapshot quorum is incomplete", {
        actualPeerIds,
        requiredPeerIds,
        tableId: context.config.tableId,
      });
    }
    this.assertSnapshot(context, snapshot, true);
    const senderRole = context.role === "witness" ? "witness" : "host";
    await this.appendLocalEvent(
      context,
      {
        snapshot,
        type: "WitnessSnapshot",
      },
      senderRole,
    );
  }

  private async captureSnapshotAfterDelay(context: MeshTableContext, delayMs = 150) {
    const expectedEventHash = context.lastEventHash;
    if (
      !expectedEventHash ||
      context.settlementSnapshotEventHashInFlight === expectedEventHash ||
      this.hasSnapshotForEventHash(context, expectedEventHash)
    ) {
      return;
    }
    context.settlementSnapshotEventHashInFlight = expectedEventHash;
    await new Promise<void>((resolve) => {
      setTimeout(resolve, delayMs);
    });
    try {
      if (context.lastEventHash !== expectedEventHash || this.hasSnapshotForEventHash(context, expectedEventHash)) {
        return;
      }
      await this.captureSnapshot(context, expectedEventHash);
    } finally {
      if (context.settlementSnapshotEventHashInFlight === expectedEventHash) {
        context.settlementSnapshotEventHashInFlight = null;
      }
    }
  }

  private async handlePeerFrame(peerId: string, frame: MeshWireFrame) {
    if (this.isClosing) {
      return;
    }
    switch (frame.kind) {
      case "request":
        await this.handlePeerRequest(peerId, frame.requestId, frame.body);
        return;
      case "event": {
        let context = this.contextByTableId.get(frame.event.tableId);
        if (!context && frame.event.body.type === "TableAnnounce") {
          const announce = frame.event.body;
          context = this.createContext({
            ...(announce.advertisement ? { advertisement: announce.advertisement } : {}),
            config: announce.table,
            currentEpoch: frame.event.epoch,
            currentHostPeerId: announce.table.hostPeerId,
            currentHostPeerUrl:
              this.transport?.listKnownPeers().find((candidate) => candidate.peerId === announce.table.hostPeerId)
                ?.peerUrl ?? "",
            role: "witness",
            witnessSet: [],
          });
          context.peerAddresses.set(frame.event.senderPeerId, {
            peerId: frame.event.senderPeerId,
            peerUrl: context.currentHostPeerUrl,
            protocolPubkeyHex: frame.event.senderProtocolPubkeyHex,
            roles: ["host"],
          });
          this.contextByTableId.set(frame.event.tableId, context);
        }
        if (!context) {
          throw new Error(`received event for unknown table ${frame.event.tableId}`);
        }
        try {
          await this.acceptEvent(context, frame.event, true);
        } catch (error) {
          this.logger.info("ignoring invalid remote event", {
            error: (error as Error).message,
            peerId,
            tableId: frame.event.tableId,
            type: frame.event.body.type,
          });
          return;
        }
        if (context.currentHostPeerId === this.peerIdentity?.id && frame.event.body.type === "TableReady") {
          this.scheduleNextHand(context);
        }
        if (frame.event.body.type === "HandResult" && context.currentHostPeerId === this.peerIdentity?.id) {
          this.scheduleNextHand(context);
        }
        return;
      }
      case "heartbeat": {
        const context = this.contextByTableId.get(frame.tableId);
        if (!context) {
          return;
        }
        if (frame.hostPeerId !== context.currentHostPeerId || frame.epoch !== context.currentEpoch) {
          return;
        }
        const hostPubkey = this.resolvePeerProtocolPubkey(context, frame.hostPeerId);
        if (!hostPubkey) {
          return;
        }
        const unsignedHeartbeat = {
          epoch: frame.epoch,
          hostPeerId: frame.hostPeerId,
          leaseExpiry: frame.leaseExpiry,
          sentAt: frame.sentAt,
          tableId: frame.tableId,
        };
        if (!verifyStructuredData(hostPubkey, unsignedHeartbeat, frame.signatureHex)) {
          return;
        }
        context.lastHostHeartbeatAt = Date.now();
        return;
      }
      case "public-ad":
        {
          const { hostSignatureHex, ...unsignedAd } = frame.ad;
          if (!verifyStructuredData(frame.ad.hostProtocolPubkeyHex, unsignedAd, hostSignatureHex)) {
            return;
          }
        }
        await this.store.upsertPublicAd(frame.ad);
        this.emitState();
        return;
      case "public-update":
        return;
      default:
        return;
    }
  }

  private async handlePeerRequest(peerId: string, requestId: string, body: MeshRequestBody) {
    if (!this.transport || !this.protocolIdentity || !this.peerIdentity) {
      return;
    }

    try {
      const response = await this.handleRequestBody(peerId, body);
      await this.transport?.send(peerId, {
        body: response,
        kind: "response",
        ok: true,
        requestId,
      });
    } catch (error) {
      await this.transport?.send(peerId, {
        error: (error as Error).message,
        kind: "response",
        ok: false,
        requestId,
      });
    }
  }

  private async handleRequestBody(peerId: string, body: MeshRequestBody): Promise<MeshResponseBody | undefined> {
    switch (body.type) {
      case "join-request":
        return await this.handleJoinRequest(peerId, body.intent, body.preparedBuyIn);
      case "buy-in-confirm":
        return await this.handleBuyInConfirm(peerId, body.confirmation);
      case "action-request":
        return await this.handleActionRequest(peerId, body.intent);
      case "snapshot-sign-request":
        return await this.handleSnapshotSignRequest(peerId, body.snapshot);
      case "lease-sign-request":
        return await this.handleLeaseSignRequest(peerId, body.lease);
      case "failover-accept-request":
        return await this.handleFailoverAcceptanceRequest(peerId, body.tableId, body.currentEpoch, body.proposedEpoch, body.proposedHostPeerId);
      case "peer-cache-request":
        return {
          peers: this.transport?.listKnownPeers() ?? [],
          type: "peer-cache-response",
        };
      case "public-table-list-request":
        return {
          ads: this.contextPublicAds(),
          type: "public-table-list-response",
        };
      default:
        return undefined;
    }
  }

  private async handleJoinRequest(
    peerId: string,
    intent: PlayerJoinIntent,
    preparedBuyIn: TableFundsOperation,
  ): Promise<MeshResponseBody> {
    const context = this.requireContext(intent.tableId);
    if (context.currentHostPeerId !== this.peerIdentity?.id || context.role !== "host") {
      return {
        accepted: false,
        reason: "this daemon is not the current host",
        type: "join-response",
      };
    }
    this.assertJoinIntentSignature(intent);
    if (intent.peerId !== peerId) {
      throw new Error("join request peer identity does not match the transport peer");
    }
    this.assertFundsReceipt({
      context,
      expectedKind: "buy-in-prepared",
      expectedPlayerId: intent.player.playerId,
      expectedStatus: "prepared",
      receipt: preparedBuyIn,
      walletPubkeyHex: intent.identityBinding.walletPubkeyHex,
    });
    if (
      preparedBuyIn.amountSats < context.config.buyInMinSats ||
      preparedBuyIn.amountSats > context.config.buyInMaxSats
    ) {
      throw new Error("prepared buy-in amount is outside the table limits");
    }

    await this.appendLocalEvent(context, {
      intent,
      type: "JoinRequest",
    });

    const occupied = new Set([...context.pendingPlayers.values()].map((player) => player.seatIndex));
    const seatIndex = intent.requestedSeatIndex ?? ([0, 1].find((candidate) => !occupied.has(candidate)) as 0 | 1 | undefined);
    if (seatIndex === undefined) {
      await this.appendLocalEvent(context, {
        intent,
        reason: "table is full",
        type: "JoinRejected",
      });
      return {
        accepted: false,
        reason: "table is full",
        type: "join-response",
      };
    }

    this.transport?.registerKnownPeer({
      alias: intent.player.nickname,
      peerId: intent.peerId,
      peerUrl: intent.peerUrl,
      protocolPubkeyHex: intent.protocolPubkeyHex,
      roles: ["player"],
    });
    for (const existingEvent of context.events) {
      await this.transport?.send(intent.peerId, {
        event: existingEvent,
        kind: "event",
      });
    }

    await this.appendLocalEvent(context, {
      intent,
      seatIndex,
      type: "JoinAccepted",
    });
    return {
      accepted: true,
      seatIndex,
      type: "join-response",
    };
  }

  private async handleBuyInConfirm(
    peerId: string,
    confirmation: BuyInConfirm,
  ): Promise<MeshResponseBody> {
    const context = this.requireContext(confirmation.tableId);
    if (context.currentHostPeerId !== this.peerIdentity?.id || context.role !== "host") {
      return {
        accepted: false,
        reason: "this daemon is not the current host",
        type: "buy-in-response",
      };
    }
    const pending = [...context.pendingPlayers.values()].find(
      (candidate) => candidate.playerId === confirmation.playerId,
    );
    if (!pending) {
      return {
        accepted: false,
        reason: "player is not seated",
        type: "buy-in-response",
      };
    }
    if (pending.peerId !== peerId) {
      throw new Error("buy-in confirmation peer identity does not match the seated player");
    }
    const unsignedConfirmation = {
      confirmedAt: confirmation.confirmedAt,
      playerId: confirmation.playerId,
      receipt: this.unsignedFundsOperation(confirmation.receipt),
      tableId: confirmation.tableId,
    };
    if (!verifyStructuredData(pending.protocolPubkeyHex, unsignedConfirmation, confirmation.signatureHex)) {
      throw new Error("invalid buy-in confirmation signature");
    }
    this.assertFundsReceipt({
      context,
      expectedKind: "buy-in-locked",
      expectedPlayerId: pending.playerId,
      expectedStatus: "locked",
      receipt: confirmation.receipt,
      walletPubkeyHex: pending.walletPubkeyHex,
    });
    await this.appendLocalEvent(context, {
      buyInSats: confirmation.receipt.amountSats,
      peerId: pending.peerId,
      playerId: pending.playerId,
      seatIndex: pending.seatIndex,
      type: "SeatLocked",
    });
    await this.appendLocalEvent(context, {
      receipt: confirmation.receipt,
      type: "BuyInLocked",
    });
    if (
      context.buyInReceipts.size === context.config.seatCount &&
      !context.readyTransitionInFlight &&
      context.config.status !== "ready"
    ) {
      context.readyTransitionInFlight = true;
      const readyState = this.buildReadyPublicState(context);
      try {
        await this.appendLocalEvent(context, {
          balances: readyState.chipBalances,
          publicState: readyState,
          type: "TableReady",
        });
        await this.captureSnapshotAfterDelay(context);
        this.scheduleNextHand(context);
      } finally {
        context.readyTransitionInFlight = false;
      }
    }
    return {
      accepted: true,
      type: "buy-in-response",
    };
  }

  private async handleActionRequest(
    peerId: string,
    intent: PlayerActionIntent,
  ): Promise<MeshResponseBody> {
    const context = this.requireContext(intent.tableId);
    if (context.currentHostPeerId !== this.peerIdentity?.id || context.role !== "host") {
      return {
        accepted: false,
        reason: "this daemon is not the current host",
        type: "action-response",
      };
    }
    this.assertActionIntentSignature(intent);

    const hand = context.privateState.activeHand;
    if (!hand || hand.handId !== intent.handId) {
      return {
        accepted: false,
        reason: "hand is not active",
        type: "action-response",
      };
    }
    if (context.handSetupInFlight) {
      return {
        accepted: false,
        reason: "hand is still starting",
        type: "action-response",
      };
    }
    const seat = context.publicState?.seatedPlayers.find((candidate) => candidate.playerId === intent.playerId);
    if (!seat || seat.peerId !== peerId) {
      throw new Error("action request peer identity does not match the acting seat");
    }

    await this.appendLocalEvent(context, {
      intent,
      type: "PlayerAction",
    });

    try {
      const previous = hand;
      const next = applyHoldemAction(previous, intent.seatIndex, intent.action);
      context.privateState.activeHand = next;
      await this.store.savePrivateState(context.config.tableId, context.privateState);
      const publicState = this.publicStateFromHand(
        context,
        next,
        context.publicState?.dealerCommitment?.rootHash ??
          context.privateState.auditBundlesByHandId[intent.handId]?.commitmentRoot ??
          "",
      );
      await this.appendLocalEvent(context, {
        intent,
        publicState,
        type: "ActionAccepted",
      });

      if (next.phase === "settled") {
        if (next.players.every((player) => player.status !== "folded")) {
          await this.appendLocalEvent(context, {
            auditBundle: context.privateState.auditBundlesByHandId[intent.handId]!,
            handId: intent.handId,
            holeCardsByPlayerId: Object.fromEntries(
              next.players.map((player) => [player.playerId, player.holeCards]),
            ),
            publicState,
            type: "ShowdownReveal",
          });
        }
        await this.appendLocalEvent(context, {
          balances: publicState.chipBalances,
          checkpointHash: context.latestFullySignedSnapshot
            ? this.snapshotHash(context.latestFullySignedSnapshot)
            : undefined,
          handId: intent.handId,
          publicState,
          type: "HandResult",
          winners: next.winners.map((winner) => ({
            amountSats: winner.amountSats,
            playerId: winner.playerId,
            seatIndex: winner.seatIndex,
          })),
        });
        await this.captureSnapshotAfterDelay(context);
        this.scheduleNextHand(context);
      } else if (previous.phase !== next.phase) {
        await this.appendLocalEvent(context, {
          handId: intent.handId,
          publicState,
          street: previous.phase,
          type: "StreetClosed",
        });
        await this.appendLocalEvent(context, {
          handId: intent.handId,
          publicState,
          street: next.phase,
          type: "StreetStart",
        });
      }

      return {
        accepted: true,
        type: "action-response",
      };
    } catch (error) {
      await this.appendLocalEvent(context, {
        intent,
        reason: (error as Error).message,
        type: "ActionRejected",
      });
      return {
        accepted: false,
        reason: (error as Error).message,
        type: "action-response",
      };
    }
  }

  private async handleSnapshotSignRequest(
    peerId: string,
    snapshot: CooperativeTableSnapshot,
  ): Promise<MeshResponseBody> {
    if (!this.protocolIdentity || !this.peerIdentity) {
      throw new Error("protocol identity is unavailable");
    }
    const context = this.requireContext(snapshot.tableId);
    await this.waitForSnapshotState(context, snapshot);
    this.assertSnapshot(context, snapshot, false);
    if (!snapshot.signatures.some((signature) => signature.signerPeerId === peerId)) {
      throw new Error("snapshot sign request is missing the collector signature");
    }
    return {
      signature: {
        signatureHex: signStructuredData(this.protocolIdentity, this.unsignedSnapshot(snapshot)),
        signedAt: new Date().toISOString(),
        signerPeerId: this.peerIdentity.id,
        signerPubkeyHex: this.protocolIdentity.publicKeyHex,
        signerRole: this.mode === "witness" ? "witness" : "player",
      },
      type: "snapshot-sign-response",
    };
  }

  private async handleLeaseSignRequest(peerId: string, lease: HostLease): Promise<MeshResponseBody> {
    if (!this.protocolIdentity || !this.peerIdentity) {
      throw new Error("protocol identity is unavailable");
    }
    const context = this.contextByTableId.get(lease.tableId);
    if (context) {
      this.assertLease(context, lease, false);
    } else {
      if (lease.epoch !== 1) {
        throw new Error("cannot sign a lease for an unknown non-genesis table");
      }
      if (!lease.witnessSet.includes(this.peerIdentity.id)) {
        throw new Error("this witness is not part of the requested lease quorum");
      }
      const hostSignature = lease.signatures.find(
        (signature) => signature.signerPeerId === peerId && signature.signerRole === "host",
      );
      const knownHost = this.transport?.listKnownPeers().find((candidate) => candidate.peerId === peerId);
      if (!hostSignature || !knownHost?.protocolPubkeyHex) {
        throw new Error("cannot verify the initial host lease signature");
      }
      if (
        hostSignature.signerPubkeyHex !== knownHost.protocolPubkeyHex ||
        !verifyStructuredData(
          hostSignature.signerPubkeyHex,
          this.unsignedLease(lease),
          hostSignature.signatureHex,
        )
      ) {
        throw new Error("initial host lease signature is invalid");
      }
    }
    if (!lease.signatures.some((signature) => signature.signerPeerId === peerId)) {
      throw new Error("lease sign request is missing the host signature");
    }
    const signature: HostLeaseSignature = {
      signatureHex: signStructuredData(this.protocolIdentity, this.unsignedLease(lease)),
      signedAt: new Date().toISOString(),
      signerPeerId: this.peerIdentity.id,
      signerPubkeyHex: this.protocolIdentity.publicKeyHex,
      signerRole: this.mode === "witness" ? "witness" : "host",
    };
    return {
      signature,
      type: "lease-sign-response",
    };
  }

  private async handleFailoverAcceptanceRequest(
    peerId: string,
    tableId: string,
    currentEpoch: number,
    proposedEpoch: number,
    proposedHostPeerId: string,
  ): Promise<MeshResponseBody> {
    if (!this.protocolIdentity || !this.peerIdentity) {
      throw new Error("protocol identity is unavailable");
    }
    const context = this.requireContext(tableId);
    if (currentEpoch !== context.currentEpoch || proposedEpoch !== context.currentEpoch + 1) {
      throw new Error("failover acceptance request does not match the local epoch");
    }
    if (!this.requiredFailoverPeerIds(context).includes(this.peerIdentity.id)) {
      throw new Error("this daemon is not part of the failover quorum");
    }
    if (!context.witnessSet.includes(peerId) && peerId !== context.currentHostPeerId) {
      throw new Error("only the current host or a configured witness may request failover acceptance");
    }
    const acceptanceBase = {
      currentEpoch,
      proposedEpoch,
      proposedHostPeerId,
      signedAt: new Date().toISOString(),
      signerPeerId: this.peerIdentity.id,
      signerPubkeyHex: this.protocolIdentity.publicKeyHex,
      tableId,
    };
    const acceptance: HostFailoverAcceptance = {
      ...acceptanceBase,
      signatureHex: signStructuredData(this.protocolIdentity, acceptanceBase),
    };
    return {
      acceptance,
      type: "failover-accept-response",
    };
  }

  private async tick() {
    for (const context of this.contextByTableId.values()) {
      if (context.currentHostPeerId === this.peerIdentity?.id && context.role === "host") {
        await this.sendHeartbeat(context);
        continue;
      }
      if (
        context.role === "witness" &&
        Date.now() - context.lastHostHeartbeatAt > HOST_FAILURE_TIMEOUT_MS &&
        !this.activeFailovers.has(context.config.tableId)
      ) {
        await this.triggerFailover(context, "missed host heartbeats");
      }
    }
    this.emitState();
  }

  private async triggerFailover(context: MeshTableContext, reason: string) {
    if (!this.transport || !this.protocolIdentity || !this.peerIdentity) {
      return;
    }
    this.activeFailovers.add(context.config.tableId);
    try {
      await this.appendLocalEvent(
        context,
        {
          latestSnapshotHash: context.latestSnapshot ? this.snapshotHash(context.latestSnapshot) : this.hash("genesis"),
          previousHostPeerId: context.currentHostPeerId,
          proposedHostPeerId: this.peerIdentity.id,
          reason,
          type: "HostFailoverProposed",
        },
        "witness",
      );

      const selfAcceptanceBase = {
        currentEpoch: context.currentEpoch,
        proposedEpoch: context.currentEpoch + 1,
        proposedHostPeerId: this.peerIdentity.id,
        signedAt: new Date().toISOString(),
        signerPeerId: this.peerIdentity.id,
        signerPubkeyHex: this.protocolIdentity.publicKeyHex,
        tableId: context.config.tableId,
      };
      const acceptances: HostFailoverAcceptance[] = [
        {
          ...selfAcceptanceBase,
          signatureHex: signStructuredData(this.protocolIdentity, selfAcceptanceBase),
        },
      ];
      await this.appendLocalEvent(
        context,
        {
          acceptance: acceptances[0]!,
          type: "HostFailoverAccepted",
        },
        "witness",
      );
      const peerIds = new Set<string>();
      for (const player of context.publicState?.seatedPlayers ?? []) {
        if (player.peerId !== this.peerIdentity.id) {
          peerIds.add(player.peerId);
        }
      }
      for (const witnessPeerId of context.witnessSet) {
        if (witnessPeerId !== this.peerIdentity.id) {
          peerIds.add(witnessPeerId);
        }
      }
      for (const peerId of peerIds) {
        try {
          const response = await this.transport.request(peerId, {
            currentEpoch: context.currentEpoch,
            proposedEpoch: context.currentEpoch + 1,
            proposedHostPeerId: this.peerIdentity.id,
            tableId: context.config.tableId,
            type: "failover-accept-request",
          });
          if (
            response?.type === "failover-accept-response" &&
            this.verifyFailoverAcceptance(context, response.acceptance)
          ) {
            acceptances.push(response.acceptance);
            await this.appendLocalEvent(
              context,
              {
                acceptance: response.acceptance,
                type: "HostFailoverAccepted",
              },
              "witness",
            );
          }
        } catch (error) {
          this.logger.info("failover acceptance request failed", {
            error: (error as Error).message,
            peerId,
            tableId: context.config.tableId,
          });
        }
      }
      const quorumPeerIds = new Set(this.requiredFailoverPeerIds(context));
      const acceptedPeerIds = new Set(acceptances.map((acceptance) => acceptance.signerPeerId));
      for (const peerId of quorumPeerIds) {
        if (!acceptedPeerIds.has(peerId)) {
          throw new Error(`failover quorum is incomplete; missing acceptance from ${peerId}`);
        }
      }

      const lease = await this.buildHostLease(
        context.config.tableId,
        context.currentEpoch + 1,
        context.witnessSet,
      );
      await this.appendLocalEvent(context, {
        lease,
        newEpoch: context.currentEpoch + 1,
        newHostPeerId: this.peerIdentity.id,
        previousHostPeerId: context.currentHostPeerId,
        type: "HostRotated",
      }, "witness");

      const activePhase = context.publicState?.phase ?? null;
      const activeHandId = context.publicState?.handId ?? null;
      const abortedMidHand =
        activePhase !== null &&
        activePhase !== "settled" &&
        Boolean(context.latestFullySignedSnapshot);
      if (abortedMidHand && context.latestFullySignedSnapshot) {
        await this.appendLocalEvent(
          context,
          {
            handId: activeHandId ?? crypto.randomUUID(),
            reason: "host disappeared mid-hand",
            rollbackSnapshotHash: this.snapshotHash(context.latestFullySignedSnapshot),
            type: "HandAbort",
          },
          "host",
        );
        await this.captureSnapshotAfterDelay(context);
      }
      if (!abortedMidHand) {
        this.scheduleNextHand(context);
      }
    } finally {
      this.activeFailovers.delete(context.config.tableId);
    }
  }

  private async performHostRotation(
    context: MeshTableContext,
    targetPeerId: string,
    reason: string,
  ) {
    if (!this.transport || !this.peerIdentity) {
      return;
    }
    const lease = await this.buildHostLease(context.config.tableId, context.currentEpoch + 1, context.witnessSet);
    await this.appendLocalEvent(context, {
      latestSnapshotHash: context.latestSnapshot ? this.snapshotHash(context.latestSnapshot) : this.hash("genesis"),
      previousHostPeerId: context.currentHostPeerId,
      proposedHostPeerId: targetPeerId,
      reason,
      type: "HostFailoverProposed",
    });
    await this.appendLocalEvent(context, {
      lease,
      newEpoch: context.currentEpoch + 1,
      newHostPeerId: targetPeerId,
      previousHostPeerId: context.currentHostPeerId,
      type: "HostRotated",
    });
  }

  private async sendHeartbeat(context: MeshTableContext) {
    if (!this.protocolIdentity || !this.peerIdentity || !this.transport) {
      return;
    }
    const unsignedHeartbeat = {
      epoch: context.currentEpoch,
      hostPeerId: this.peerIdentity.id,
      leaseExpiry: new Date(Date.now() + HOST_LEASE_DURATION_MS).toISOString(),
      sentAt: new Date().toISOString(),
      tableId: context.config.tableId,
    };
    const peerIds = new Set<string>();
    for (const player of context.publicState?.seatedPlayers ?? []) {
      if (player.peerId !== this.peerIdentity.id) {
        peerIds.add(player.peerId);
      }
    }
    for (const witnessPeerId of context.witnessSet) {
      if (witnessPeerId !== this.peerIdentity.id) {
        peerIds.add(witnessPeerId);
      }
    }
    if (peerIds.size === 0) {
      return;
    }
    await this.transport.broadcast([...peerIds], {
      ...unsignedHeartbeat,
      kind: "heartbeat",
      signatureHex: signStructuredData(this.protocolIdentity, unsignedHeartbeat),
    });
  }

  private async buildHostLease(
    tableId: string,
    epoch: number,
    witnessSet: string[],
  ): Promise<HostLease> {
    if (!this.protocolIdentity || !this.peerIdentity || !this.transport) {
      throw new Error("protocol identity is unavailable");
    }
    const normalizedWitnessSet = [...new Set(witnessSet)].filter((peerId) => peerId !== this.peerIdentity?.id);
    let leaseBase: HostLease = {
      epoch,
      hostPeerId: this.peerIdentity.id,
      leaseExpiry: new Date(Date.now() + HOST_LEASE_DURATION_MS).toISOString(),
      leaseStart: new Date().toISOString(),
      signatures: [],
      tableId,
      witnessSet: normalizedWitnessSet,
    };
    let hostSignature: HostLeaseSignature = {
      signatureHex: signStructuredData(this.protocolIdentity, this.unsignedLease(leaseBase)),
      signedAt: new Date().toISOString(),
      signerPeerId: this.peerIdentity.id,
      signerPubkeyHex: this.protocolIdentity.publicKeyHex,
      signerRole: "host",
    };
    let leaseCandidate: HostLease = {
      ...leaseBase,
      signatures: [hostSignature],
    };
    const context = this.contextByTableId.get(tableId);
    const firstPass = await this.requestLeaseWitnessSignatures(
      context,
      tableId,
      normalizedWitnessSet,
      leaseCandidate,
    );
    const resolvedWitnessSet = [...new Set(firstPass.resolvedWitnessSet)].filter(
      (peerId) => peerId !== this.peerIdentity?.id,
    );

    let signatures: HostLeaseSignature[];
    if (stableStringify(resolvedWitnessSet) !== stableStringify(normalizedWitnessSet)) {
      leaseBase = {
        ...leaseBase,
        witnessSet: resolvedWitnessSet,
      };
      hostSignature = {
        signatureHex: signStructuredData(this.protocolIdentity, this.unsignedLease(leaseBase)),
        signedAt: new Date().toISOString(),
        signerPeerId: this.peerIdentity.id,
        signerPubkeyHex: this.protocolIdentity.publicKeyHex,
        signerRole: "host",
      };
      leaseCandidate = {
        ...leaseBase,
        signatures: [hostSignature],
      };
      const secondPass = await this.requestLeaseWitnessSignatures(
        context,
        tableId,
        normalizedWitnessSet,
        leaseCandidate,
      );
      signatures = [{ ...hostSignature }, ...secondPass.signatures];
    } else {
      signatures = [{ ...hostSignature }, ...firstPass.signatures];
    }
    const lease = {
      ...leaseBase,
      signatures,
    };
    if (context) {
      this.assertLease(context, lease, true);
    }
    return lease;
  }

  private async requestLeaseWitnessSignatures(
    context: MeshTableContext | undefined,
    tableId: string,
    witnessPeerIds: string[],
    leaseCandidate: HostLease,
  ) {
    const signatures: HostLeaseSignature[] = [];
    const resolvedWitnessSet = [...witnessPeerIds];
    for (const witnessPeerId of witnessPeerIds) {
      for (let attempt = 0; attempt < 5; attempt += 1) {
        try {
          const response = await this.transport?.request(witnessPeerId, {
            lease: leaseCandidate,
            type: "lease-sign-request",
          });
          if (response?.type === "lease-sign-response") {
            const expectedPubkey =
              (context ? this.resolvePeerProtocolPubkey(context, response.signature.signerPeerId) : null) ??
              response.signature.signerPubkeyHex;
            if (
              expectedPubkey === response.signature.signerPubkeyHex &&
              verifyStructuredData(
                response.signature.signerPubkeyHex,
                this.unsignedLease(leaseCandidate),
                response.signature.signatureHex,
              )
            ) {
              signatures.push(response.signature);
              if (response.signature.signerPeerId !== witnessPeerId) {
                const knownPeer = this.transport
                  ?.listKnownPeers()
                  .find((candidate) => candidate.peerId === witnessPeerId);
                if (knownPeer) {
                  this.transport?.registerKnownPeer({
                    ...knownPeer,
                    peerId: response.signature.signerPeerId,
                    protocolPubkeyHex: response.signature.signerPubkeyHex,
                  });
                }
              }
              const resolvedIndex = resolvedWitnessSet.indexOf(witnessPeerId);
              if (resolvedIndex !== -1) {
                resolvedWitnessSet[resolvedIndex] = response.signature.signerPeerId;
              }
            }
            break;
          }
        } catch (error) {
          if (attempt === 4) {
            this.logger.info("lease signature request failed", {
              error: (error as Error).message,
              tableId,
              witnessPeerId,
            });
            break;
          }
          await new Promise<void>((resolve) => {
            setTimeout(resolve, 100);
          });
        }
      }
    }
    return {
      resolvedWitnessSet,
      signatures,
    };
  }

  private buildAdvertisement(context: MeshTableContext): SignedTableAdvertisement {
    if (!this.protocolIdentity || !this.transport) {
      throw new Error("protocol identity is unavailable");
    }
    const hostModeCapabilities: SignedTableAdvertisement["hostModeCapabilities"] = ["host-dealer-v1"];
    const base = {
      adExpiresAt: new Date(Date.now() + HOST_LEASE_DURATION_MS).toISOString(),
      buyInMaxSats: context.config.buyInMaxSats,
      buyInMinSats: context.config.buyInMinSats,
      currency: "sats",
      hostModeCapabilities,
      hostPeerId: context.currentHostPeerId,
      hostPeerUrl: this.transport.getListenUrl(),
      hostProtocolPubkeyHex: this.protocolIdentity.publicKeyHex,
      latencyHintMs: 25,
      networkId: context.config.networkId,
      occupiedSeats: context.config.occupiedSeats,
      protocolVersion: "poker/v1",
      seatCount: context.config.seatCount,
      spectatorsAllowed: context.config.spectatorsAllowed,
      stakes: {
        bigBlindSats: context.config.bigBlindSats,
        smallBlindSats: context.config.smallBlindSats,
      },
      tableId: context.config.tableId,
      tableName: context.config.name,
      visibility: context.config.visibility,
      witnessCount: context.witnessSet.length,
    };
    return {
      ...base,
      hostSignatureHex: signStructuredData(this.protocolIdentity, base),
    };
  }

  private buildActionIntent(
    context: MeshTableContext,
    seatIndex: number,
    payload: MeshPlayerActionPayload,
  ): PlayerActionIntent {
    if (!this.protocolIdentity || !context.publicState?.handId || !this.walletIdentity) {
      throw new Error("cannot build an action intent without an active hand");
    }
    const base = {
      action: payload,
      epoch: context.currentEpoch,
      handId: context.publicState.handId,
      playerId: this.walletIdentity.playerId,
      protocolPubkeyHex: this.protocolIdentity.publicKeyHex,
      requestedAt: new Date().toISOString(),
      seatIndex,
      tableId: context.config.tableId,
    };
    return {
      ...base,
      signatureHex: signStructuredData(this.protocolIdentity, base),
    };
  }

  private buildBuyInConfirm(tableId: string, receipt: TableFundsOperation): BuyInConfirm {
    if (!this.protocolIdentity || !this.walletIdentity) {
      throw new Error("protocol identity is unavailable");
    }
    const base = {
      confirmedAt: new Date().toISOString(),
      playerId: this.walletIdentity.playerId,
      receipt,
      tableId,
    };
    return {
      ...base,
      signatureHex: signStructuredData(this.protocolIdentity, {
        ...base,
        receipt: this.unsignedFundsOperation(receipt),
      }),
    };
  }

  private async buildJoinIntent(invite: PrivateInvite, nickname: string, buyInSats: number): Promise<PlayerJoinIntent> {
    if (!this.peerIdentity || !this.protocolIdentity || !this.walletIdentity || !this.transport) {
      throw new Error("mesh identities are unavailable");
    }
    const wallet = await this.walletRuntime.getWallet(this.profileName);
    const player = {
      arkAddress: wallet.arkAddress,
      joinedAt: new Date().toISOString(),
      nickname,
      playerId: this.walletIdentity.playerId,
      pubkeyHex: this.walletIdentity.publicKeyHex,
    };
    const identityBinding = buildIdentityBinding({
      peerId: this.peerIdentity.id,
      protocolIdentity: this.protocolIdentity,
      tableId: invite.tableId,
      walletIdentity: this.walletIdentity,
    });
    const base = {
      identityBinding,
      peerId: this.peerIdentity.id,
      peerUrl: this.transport.getListenUrl(),
      player: {
        ...player,
        boardingAddress: wallet.boardingAddress,
      },
      protocolId: this.protocolIdentity.id,
      protocolPubkeyHex: this.protocolIdentity.publicKeyHex,
      requestedAt: new Date().toISOString(),
      tableId: invite.tableId,
    };
    return {
      ...base,
      signatureHex: signStructuredData(this.protocolIdentity, base),
    };
  }

  private buildReadyPublicState(context: MeshTableContext): PublicTableState {
    const seatedPlayers = [...context.pendingPlayers.values()].sort(
      (left, right) => left.seatIndex - right.seatIndex,
    );
    const chipBalances = Object.fromEntries(
      seatedPlayers.map((player) => [player.playerId, context.buyInReceipts.get(player.playerId)?.amountSats ?? player.buyInSats]),
    );
    return publicTableStateSchema.parse({
      actingSeatIndex: null,
      board: [],
      chipBalances,
      currentBetSats: 0,
      dealerCommitment: null,
      dealerSeatIndex: null,
      epoch: context.currentEpoch,
      foldedPlayerIds: [],
      handId: null,
      handNumber: context.publicState?.handNumber ?? 0,
      latestEventHash: context.lastEventHash,
      livePlayerIds: seatedPlayers.map((player) => player.playerId),
      minRaiseToSats: context.config.bigBlindSats,
      phase: null,
      potSats: 0,
      previousSnapshotHash: context.latestSnapshot ? this.snapshotHash(context.latestSnapshot) : null,
      roundContributions: Object.fromEntries(seatedPlayers.map((player) => [player.playerId, 0])),
      seatedPlayers,
      snapshotId: crypto.randomUUID(),
      status: "ready",
      tableId: context.config.tableId,
      totalContributions: Object.fromEntries(seatedPlayers.map((player) => [player.playerId, 0])),
      updatedAt: new Date().toISOString(),
    });
  }

  private buildSignedEvent(
    context: MeshTableContext,
    body: TableEventBody,
    senderRole: "host" | "witness",
  ): SignedTableEvent {
    if (!this.protocolIdentity || !this.peerIdentity) {
      throw new Error("protocol identity is unavailable");
    }
    const nextEpoch =
      body.type === "HostRotated" ? body.newEpoch : context.currentEpoch;
    const previousInEpoch = [...context.events]
      .reverse()
      .find((candidate) => candidate.epoch === nextEpoch);
    const seq = previousInEpoch ? previousInEpoch.seq + 1 : 1;
    const canonicalBody = this.cloneValue(body);
    const unsigned = {
      body: canonicalBody,
      epoch: nextEpoch,
      handId: this.eventHandId(canonicalBody),
      messageType: canonicalBody.type,
      networkId: this.config.network,
      prevEventHash: context.lastEventHash,
      protocolVersion: "poker/v1",
      senderPeerId: this.peerIdentity.id,
      senderProtocolPubkeyHex: this.protocolIdentity.publicKeyHex,
      senderRole,
      seq,
      tableId: context.config.tableId,
      timestamp: new Date().toISOString(),
    };
    return {
      ...unsigned,
      signature: signStructuredData(this.protocolIdentity, unsigned),
    };
  }

  private contextPublicAds() {
    return [...this.contextByTableId.values()]
      .map((context) => context.advertisement)
      .filter((candidate): candidate is SignedTableAdvertisement => Boolean(candidate));
  }

  private createContext(args: {
    advertisement?: SignedTableAdvertisement;
    config: MeshTableConfig;
    currentEpoch: number;
    currentHostPeerId: string;
    currentHostPeerUrl: string;
    role: "host" | "player" | "witness";
    witnessSet: string[];
  }): MeshTableContext {
    return {
      ...(args.advertisement ? { advertisement: args.advertisement } : {}),
      buyInReceipts: new Map(),
      config: args.config,
      currentEpoch: args.currentEpoch,
      currentHostPeerId: args.currentHostPeerId,
      currentHostPeerUrl: args.currentHostPeerUrl,
      events: [],
      handSetupInFlight: false,
      latestFullySignedSnapshot: null,
      latestSnapshot: null,
      lastEventHash: null,
      lastHostHeartbeatAt: Date.now(),
      peerAddresses: new Map(),
      pendingPlayers: new Map(),
      pendingEvents: new Map(),
      readyTransitionInFlight: false,
      privateState: {
        auditBundlesByHandId: {},
        myHoleCardsByHandId: {},
      },
      publicState: null,
      role: args.role,
      settlementSnapshotEventHashInFlight: null,
      snapshots: [],
      witnessSet: [...args.witnessSet],
    };
  }

  private createFundsProvider(): TableFundsProvider {
    if (!this.walletIdentity) {
      throw new Error("wallet identity is unavailable");
    }
    if (!this.fundsProvider) {
      this.fundsProvider = this.config.useMockSettlement
        ? createMockTableFundsProvider({
            networkId: this.config.network,
            signer: this.walletIdentity,
          })
        : createArkadeTableFundsProvider({
            arkServerUrl: this.config.arkServerUrl,
            arkadeNetworkName: this.config.arkadeNetworkName,
            boltzApiUrl: this.config.boltzApiUrl,
            networkId: this.config.network,
            signer: this.walletIdentity,
            stateStore: new JsonTableFundsStateStore(
              `${this.config.daemonDir}/${this.profileName.replace(/[^a-zA-Z0-9_-]/g, "_")}.table-funds.json`,
            ),
          });
    }
    return this.fundsProvider;
  }

  private decodeInvite(inviteCode: string): PrivateInvite {
    return JSON.parse(Buffer.from(inviteCode, "base64url").toString("utf8")) as PrivateInvite;
  }

  private emitState() {
    this.onStateChange?.(this.currentState());
  }

  private encodeInvite(invite: PrivateInvite) {
    return Buffer.from(JSON.stringify(invite), "utf8").toString("base64url");
  }

  private ensureAutoTimerCleared(tableId: string) {
    const existing = this.autoHandTimers.get(tableId);
    if (existing) {
      clearTimeout(existing);
      this.autoHandTimers.delete(tableId);
    }
  }

  private async ensureProfileState(nickname = this.profileName) {
    const existing = (await this.profileStore.load(this.profileName)) ?? {
      handSeeds: {},
      nickname,
      privateKeyHex: this.walletIdentity?.privateKeyHex ?? crypto.randomUUID().replaceAll("-", ""),
      profileName: this.profileName,
    };
    const state: PlayerProfileState = {
      ...existing,
      handSeeds: existing.handSeeds ?? {},
      knownPeers: existing.knownPeers ?? [],
      meshTables: existing.meshTables ?? {},
      nickname: existing.nickname ?? nickname,
      peerPrivateKeyHex: existing.peerPrivateKeyHex ?? createScopedIdentity("peer").privateKeyHex,
      profileName: this.profileName,
      protocolPrivateKeyHex:
        existing.protocolPrivateKeyHex ?? createScopedIdentity("protocol").privateKeyHex,
      walletPrivateKeyHex: existing.walletPrivateKeyHex ?? existing.privateKeyHex,
    };
    state.privateKeyHex = state.walletPrivateKeyHex ?? state.privateKeyHex;
    await this.profileStore.save(state);
    return state;
  }

  private async ensureStarted() {
    if (!this.transport) {
      await this.start(this.mode);
    }
  }

  private eventHandId(body: TableEventBody) {
    switch (body.type) {
      case "HandStart":
      case "DealerCommit":
      case "PrivateCardDelivery":
      case "StreetStart":
      case "StreetClosed":
      case "ShowdownReveal":
      case "HandResult":
      case "HandAbort":
        return body.handId;
      case "ActionAccepted":
      case "ActionRejected":
      case "PlayerAction":
        return body.intent.handId;
      default:
        return null;
    }
  }

  private eventHash(event: SignedTableEvent) {
    return this.hash(stableStringify(this.unsignedEvent(event)));
  }

  private hash(input: string) {
    return createHash("sha256").update(input).digest("hex");
  }

  private requiredSnapshotPeerIds(context: MeshTableContext) {
    return [...new Set<string>([
      context.currentHostPeerId,
      ...(context.publicState?.seatedPlayers.map((player) => player.peerId) ?? []),
      ...context.witnessSet,
    ])].filter(Boolean);
  }

  private resolveWitnessSet(context: MeshTableContext, lease: HostLease) {
    const signerWitnessPeerIds = lease.signatures
      .filter((signature) => signature.signerRole === "witness")
      .map((signature) => signature.signerPeerId);
    if (signerWitnessPeerIds.length > 0) {
      signerWitnessPeerIds.forEach((peerId, index) => {
        const provisionalPeerId = lease.witnessSet[index];
        if (!provisionalPeerId || provisionalPeerId === peerId) {
          return;
        }
        const knownPeer =
          context.peerAddresses.get(provisionalPeerId) ??
          this.transport?.listKnownPeers().find((candidate) => candidate.peerId === provisionalPeerId);
        if (!knownPeer) {
          return;
        }
        const resolvedPeer: PeerAddress = {
          ...knownPeer,
          peerId,
        };
        context.peerAddresses.delete(provisionalPeerId);
        context.peerAddresses.set(peerId, resolvedPeer);
        this.transport?.registerKnownPeer(resolvedPeer);
      });
      return [...new Set(signerWitnessPeerIds)];
    }
    return [...lease.witnessSet];
  }

  private hasRequiredSignatures(context: MeshTableContext, snapshot: CooperativeTableSnapshot) {
    const actualPeerIds = new Set(snapshot.signatures.map((signature) => signature.signerPeerId));
    for (const peerId of this.requiredSnapshotPeerIds(context)) {
      if (!actualPeerIds.has(peerId)) {
        return false;
      }
    }
    return true;
  }

  private hasSnapshotForEventHash(context: MeshTableContext, eventHash: string) {
    return context.snapshots.some(
      (snapshot) =>
        snapshot.latestEventHash === eventHash &&
        this.isSettlementBoundarySnapshot(snapshot) &&
        this.hasRequiredSignatures(context, snapshot),
    );
  }

  private async loadPersistedTables() {
    const profile = await this.ensureProfileState();
    for (const tableId of await this.store.listTableIds()) {
      await this.loadPersistedTable(tableId, profile);
    }
  }

  private async persistPeerUrl(_peerUrl: string) {
    await this.ensureProfileState();
  }

  private async loadPersistedTable(
    tableId: string,
    profile?: PlayerProfileState,
  ) {
    if (this.contextByTableId.has(tableId)) {
      return;
    }
    const resolvedProfile = profile ?? (await this.ensureProfileState());
    const tableRef = resolvedProfile.meshTables?.[tableId];
    const events = await this.store.loadEvents(tableId);
    const first = events[0];
    if (events.length === 0 && !tableRef?.config) {
      return;
    }
    const bootstrapConfig = first?.body.type === "TableAnnounce" ? first.body.table : tableRef?.config;
    if (!bootstrapConfig) {
      return;
    }
    const context = this.createContext({
      ...(first?.body.type === "TableAnnounce" && first.body.advertisement
        ? { advertisement: first.body.advertisement }
        : {}),
      config: this.cloneValue(bootstrapConfig),
      currentEpoch: first?.epoch ?? tableRef?.currentEpoch ?? 1,
      currentHostPeerId: bootstrapConfig.hostPeerId,
      currentHostPeerUrl: tableRef?.hostPeerUrl ?? "",
      role: tableRef?.role ?? "player",
      witnessSet: [],
    });
    context.privateState = await this.store.loadPrivateState(tableId);
    context.snapshots = await this.store.loadSnapshots(tableId);
    context.latestSnapshot = context.snapshots.at(-1) ?? null;
    for (const event of events) {
      context.events.push(event);
      context.lastEventHash = this.eventHash(event);
      this.applyEventToContext(context, event);
    }
    context.latestFullySignedSnapshot =
      [...context.snapshots]
        .reverse()
        .find((snapshot) => this.isSettlementBoundarySnapshot(snapshot) && this.hasRequiredSignatures(context, snapshot)) ?? null;
    this.contextByTableId.set(tableId, context);
  }

  private async publishPublicState(context: MeshTableContext, body: TableEventBody) {
    if (!this.indexer || context.config.visibility !== "public" || !context.advertisement) {
      return;
    }
    if (body.type === "TableAnnounce") {
      await this.indexer.announceTable(context.advertisement);
      return;
    }
    if (!context.publicState) {
      return;
    }
    if (body.type === "TableReady" || !context.publicState.handId || context.publicState.handNumber === 0) {
      await this.indexer.publishUpdate({
        advertisement: context.advertisement,
        publicState: context.publicState,
        publishedAt: new Date().toISOString(),
        tableId: context.config.tableId,
        type: "PublicTableSnapshot",
      });
      return;
    }
    if (
      body.type === "HandStart" ||
      body.type === "StreetStart" ||
      body.type === "ActionAccepted" ||
      body.type === "StreetClosed" ||
      body.type === "HandResult" ||
      body.type === "TableClosed"
    ) {
      await this.indexer.publishUpdate({
        handId: context.publicState.handId ?? crypto.randomUUID(),
        handNumber: context.publicState.handNumber,
        phase: context.publicState.phase ?? "preflop",
        publicState: context.publicState,
        publishedAt: new Date().toISOString(),
        tableId: context.config.tableId,
        type: "PublicHandUpdate",
      });
    }
    if (body.type === "ShowdownReveal") {
      await this.indexer.publishUpdate({
        board: body.publicState.board,
        handId: body.handId,
        handNumber: body.publicState.handNumber,
        holeCardsByPlayerId: body.holeCardsByPlayerId,
        publishedAt: new Date().toISOString(),
        tableId: context.config.tableId,
        type: "PublicShowdownReveal",
      });
    }
  }

  private publicStateFromHand(
    context: MeshTableContext,
    hand: HoldemState,
    commitmentRoot: string,
  ): PublicTableState {
    const checkpoint = toCheckpointShape(hand);
    const seatedPlayers = [...context.pendingPlayers.values()].sort(
      (left, right) => left.seatIndex - right.seatIndex,
    );
    return publicTableStateSchema.parse({
      actingSeatIndex: checkpoint.actingSeatIndex,
      board: checkpoint.board,
      chipBalances: checkpoint.playerStacks,
      currentBetSats: checkpoint.currentBetSats,
      dealerCommitment: {
        committedAt: new Date().toISOString(),
        mode: "host-dealer-v1",
        rootHash: commitmentRoot,
      },
      dealerSeatIndex: checkpoint.dealerSeatIndex,
      epoch: context.currentEpoch,
      foldedPlayerIds: hand.players
        .filter((player) => player.status === "folded")
        .map((player) => player.playerId),
      handId: hand.handId,
      handNumber: hand.handNumber,
      latestEventHash: context.lastEventHash,
      livePlayerIds: hand.players
        .filter((player) => player.status !== "folded")
        .map((player) => player.playerId),
      minRaiseToSats: checkpoint.minRaiseToSats,
      phase: checkpoint.phase,
      potSats: checkpoint.potSats,
      previousSnapshotHash: context.latestSnapshot ? this.snapshotHash(context.latestSnapshot) : null,
      roundContributions: checkpoint.roundContributions,
      seatedPlayers: seatedPlayers.map((player) => ({
        ...player,
        status: hand.players[player.seatIndex]!.status,
      })),
      snapshotId: crypto.randomUUID(),
      status: hand.phase === "settled" ? "ready" : "active",
      tableId: context.config.tableId,
      totalContributions: checkpoint.totalContributions,
      updatedAt: new Date().toISOString(),
    });
  }

  private publicStateFromSnapshot(
    context: MeshTableContext,
    snapshot: CooperativeTableSnapshot,
  ): PublicTableState {
    return publicTableStateSchema.parse({
      actingSeatIndex: snapshot.turnIndex,
      board: context.publicState?.board ?? [],
      chipBalances: snapshot.chipBalances,
      currentBetSats: 0,
      dealerCommitment: snapshot.dealerCommitmentRoot
        ? {
            committedAt: snapshot.createdAt,
            mode: "host-dealer-v1",
            rootHash: snapshot.dealerCommitmentRoot,
          }
        : null,
      dealerSeatIndex: context.publicState?.dealerSeatIndex ?? null,
      epoch: snapshot.epoch,
      foldedPlayerIds: snapshot.foldedPlayerIds,
      handId: snapshot.handId,
      handNumber: snapshot.handNumber,
      latestEventHash: snapshot.latestEventHash,
      livePlayerIds: snapshot.livePlayerIds,
      minRaiseToSats: context.config.bigBlindSats,
      phase: snapshot.phase,
      potSats: snapshot.potSats,
      previousSnapshotHash: snapshot.previousSnapshotHash,
      roundContributions: Object.fromEntries(snapshot.seatedPlayers.map((player) => [player.playerId, 0])),
      seatedPlayers: snapshot.seatedPlayers,
      snapshotId: crypto.randomUUID(),
      status: "ready",
      tableId: snapshot.tableId,
      totalContributions: Object.fromEntries(snapshot.seatedPlayers.map((player) => [player.playerId, 0])),
      updatedAt: new Date().toISOString(),
    });
  }

  private requireContext(tableId?: string) {
    const targetTableId = tableId ?? this.contextByTableId.keys().next().value;
    if (!targetTableId) {
      throw new Error("no mesh table is selected");
    }
    const context = this.contextByTableId.get(targetTableId);
    if (!context) {
      throw new Error(`unknown mesh table ${targetTableId}`);
    }
    return context;
  }

  private async reconnectKnownPeers() {
    if (!this.transport) {
      return;
    }
    const profile = await this.ensureProfileState();
    for (const peer of profile.knownPeers ?? []) {
      this.transport.registerKnownPeer({
        peerId: peer.peerId,
        peerUrl: peer.peerUrl,
        ...(peer.protocolPubkeyHex ? { protocolPubkeyHex: peer.protocolPubkeyHex } : {}),
        roles: peer.roles ?? [],
        ...(peer.alias ? { alias: peer.alias } : {}),
        ...(peer.lastSeenAt ? { lastSeenAt: peer.lastSeenAt } : {}),
        ...(peer.relayPeerId ? { relayPeerId: peer.relayPeerId } : {}),
      });
    }
  }

  private async rememberPeer(peer: PeerAddress) {
    if (this.isClosing) {
      return;
    }
    const profile = await this.ensureProfileState();
    if (this.isClosing) {
      return;
    }
    const known = new Map((profile.knownPeers ?? []).map((entry) => [entry.peerId, entry] as const));
    const next: KnownPeerState = {
      lastSeenAt: peer.lastSeenAt ?? new Date().toISOString(),
      peerId: peer.peerId,
      peerUrl: peer.peerUrl,
      ...(peer.protocolPubkeyHex ? { protocolPubkeyHex: peer.protocolPubkeyHex } : {}),
      roles: peer.roles,
      ...(peer.alias ? { alias: peer.alias } : {}),
      ...(peer.relayPeerId ? { relayPeerId: peer.relayPeerId } : {}),
    };
    known.set(peer.peerId, next);
    profile.knownPeers = [...known.values()];
    if (this.isClosing) {
      return;
    }
    await this.profileStore.save(profile);
    if (this.isClosing) {
      return;
    }
    this.emitState();
  }

  private scheduleNextHand(context: MeshTableContext) {
    this.ensureAutoTimerCleared(context.config.tableId);
    const timer = setTimeout(() => {
      void this.beginNextHand(context);
    }, AUTO_NEXT_HAND_DELAY_MS);
    this.autoHandTimers.set(context.config.tableId, timer);
  }

  private snapshotHash(snapshot: CooperativeTableSnapshot) {
    return this.hash(stableStringify(this.unsignedSnapshot(snapshot)));
  }

  private classifyEvent(context: MeshTableContext, event: SignedTableEvent): EventDisposition {
    if (event.epoch < context.currentEpoch) {
      return "ignore";
    }

    const previousInEpoch = [...context.events]
      .reverse()
      .find((candidate) => candidate.epoch === event.epoch);
    const expectedSeq = previousInEpoch ? previousInEpoch.seq + 1 : 1;

    if (event.epoch === context.currentEpoch) {
      if (event.seq < expectedSeq) {
        return "ignore";
      }
      if (event.seq > expectedSeq) {
        return "queue";
      }
    } else if (event.seq !== 1) {
      return "queue";
    }

    if (context.lastEventHash !== event.prevEventHash) {
      return "queue";
    }

    return "accept";
  }

  private cloneValue<T>(input: T): T {
    return JSON.parse(JSON.stringify(input)) as T;
  }

  private async flushPendingEvents(context: MeshTableContext) {
    let progressed = true;
    while (progressed) {
      progressed = false;
      for (const [eventHash, event] of [...context.pendingEvents.entries()]) {
        const disposition = this.classifyEvent(context, event);
        if (disposition === "ignore") {
          context.pendingEvents.delete(eventHash);
          continue;
        }
        if (disposition !== "accept") {
          continue;
        }
        context.pendingEvents.delete(eventHash);
        try {
          this.assertEventSemantics(context, event);
          await this.commitAcceptedEvent(context, event, eventHash, true);
        } catch (error) {
          this.logger.info("discarding invalid queued event", {
            error: (error as Error).message,
            eventHash,
            tableId: context.config.tableId,
            type: event.body.type,
          });
        }
        progressed = true;
        break;
      }
    }
  }

  private toTableSummary(context: MeshTableContext): TableSummary {
    return {
      currentEpoch: context.currentEpoch,
      handNumber: context.publicState?.handNumber ?? 0,
      hostPeerId: context.currentHostPeerId,
      ...(context.latestSnapshot ? { latestSnapshotId: context.latestSnapshot.snapshotId } : {}),
      phase: context.publicState?.phase ?? null,
      role: context.role,
      status: context.config.status,
      tableId: context.config.tableId,
      tableName: context.config.name,
      visibility: context.config.visibility,
    };
  }

  private unsignedEvent(event: SignedTableEvent) {
    const { signature, ...unsigned } = event;
    return unsigned;
  }

  private unsignedFundsOperation(receipt: TableFundsOperation) {
    const { signatureHex, ...unsigned } = receipt;
    return unsigned;
  }

  private unsignedLease(lease: HostLease) {
    const { signatures, ...unsigned } = lease;
    return unsigned;
  }

  private unsignedSnapshot(snapshot: CooperativeTableSnapshot) {
    const { signatures, ...unsigned } = snapshot;
    return unsigned;
  }

  private assertEventSemantics(context: MeshTableContext, event: SignedTableEvent) {
    if (event.protocolVersion !== "poker/v1") {
      throw new Error(`unexpected protocol version ${event.protocolVersion}`);
    }
    if (event.networkId !== context.config.networkId) {
      throw new Error(`unexpected network id ${event.networkId}`);
    }
    if (event.tableId !== context.config.tableId) {
      throw new Error(`event table id ${event.tableId} does not match context`);
    }
    if (event.messageType !== event.body.type) {
      throw new Error("event messageType does not match the event body type");
    }
    if ((event.handId ?? null) !== (this.eventHandId(event.body) ?? null)) {
      throw new Error("event handId does not match the embedded body hand id");
    }

    const assertHostEmitter = () => {
      if (event.senderRole !== "host" || event.senderPeerId !== context.currentHostPeerId) {
        throw new Error(`${event.body.type} must be emitted by the current host`);
      }
    };

    switch (event.body.type) {
      case "TableAnnounce":
        if (event.senderRole !== "host" || event.senderPeerId !== event.body.table.hostPeerId) {
          throw new Error("TableAnnounce must be emitted by the hosting peer");
        }
        return;
      case "JoinRequest":
        assertHostEmitter();
        this.assertJoinIntentSignature(event.body.intent);
        return;
      case "JoinAccepted":
      case "JoinRejected":
        assertHostEmitter();
        this.assertJoinIntentSignature(event.body.intent);
        return;
      case "SeatLocked": {
        assertHostEmitter();
        const player = context.pendingPlayers.get(event.body.playerId);
        if (!player || player.peerId !== event.body.peerId || player.seatIndex !== event.body.seatIndex) {
          throw new Error("SeatLocked does not match the pending player reservation");
        }
        return;
      }
      case "BuyInLocked": {
        assertHostEmitter();
        const player = context.pendingPlayers.get(event.body.receipt.playerId);
        if (!player) {
          throw new Error("BuyInLocked references a player who is not pending at the table");
        }
        this.assertFundsReceipt({
          context,
          expectedKind: "buy-in-locked",
          expectedPlayerId: player.playerId,
          expectedStatus: "locked",
          receipt: event.body.receipt,
          walletPubkeyHex: player.walletPubkeyHex,
        });
        return;
      }
      case "TableReady":
      case "HandStart":
      case "DealerCommit":
      case "PrivateCardDelivery":
      case "StreetStart":
      case "PlayerAction":
      case "ActionAccepted":
      case "ActionRejected":
      case "StreetClosed":
      case "ShowdownReveal":
      case "HandResult":
      case "HandAbort":
      case "TableClosed":
        assertHostEmitter();
        if ("intent" in event.body) {
          this.assertActionIntentSignature(event.body.intent);
        }
        return;
      case "HostLeaseGranted":
        if (event.senderRole !== "host" || event.senderPeerId !== event.body.lease.hostPeerId) {
          throw new Error("HostLeaseGranted must be emitted by the lease holder");
        }
        this.assertLease(context, event.body.lease, true);
        return;
      case "WitnessSnapshot":
        if (
          (event.senderRole === "host" && event.senderPeerId !== context.currentHostPeerId) ||
          (event.senderRole === "witness" && !context.witnessSet.includes(event.senderPeerId))
        ) {
          throw new Error("WitnessSnapshot must be emitted by the current host or a configured witness");
        }
        this.assertSnapshot(context, event.body.snapshot, true);
        return;
      case "HostFailoverProposed":
        if (
          !(event.senderRole === "witness" && context.witnessSet.includes(event.senderPeerId)) &&
          !(event.senderRole === "host" && event.senderPeerId === context.currentHostPeerId)
        ) {
          throw new Error("HostFailoverProposed must be emitted by the current host or a configured witness");
        }
        if (event.body.previousHostPeerId !== context.currentHostPeerId) {
          throw new Error("HostFailoverProposed references the wrong previous host");
        }
        return;
      case "HostFailoverAccepted":
        if (event.senderRole !== "witness" || !context.witnessSet.includes(event.senderPeerId)) {
          throw new Error("HostFailoverAccepted must be emitted by a witness collector");
        }
        if (!this.verifyFailoverAcceptance(context, event.body.acceptance)) {
          throw new Error("HostFailoverAccepted carries an invalid acceptance signature");
        }
        return;
      case "HostRotated":
        if (
          !(event.senderRole === "host" && event.senderPeerId === context.currentHostPeerId) &&
          !(event.senderRole === "witness" && context.witnessSet.includes(event.senderPeerId))
        ) {
          throw new Error("HostRotated must be emitted by the current host or a configured witness");
        }
        if (event.body.newEpoch !== context.currentEpoch + 1 || event.body.lease.epoch !== event.body.newEpoch) {
          throw new Error("HostRotated must advance the epoch by exactly one");
        }
        if (event.body.newHostPeerId !== event.body.lease.hostPeerId) {
          throw new Error("HostRotated lease host does not match the announced new host");
        }
        this.assertLease(context, event.body.lease, true);
        return;
      default:
        return;
    }
  }

  private expectedFundsProviderName() {
    return this.config.useMockSettlement ? "mock-table-funds/v1" : "arkade-table-funds/v1";
  }

  private buildSnapshotBase(
    context: MeshTableContext,
    snapshotId: CooperativeTableSnapshot["snapshotId"] = crypto.randomUUID() as CooperativeTableSnapshot["snapshotId"],
    createdAt = new Date().toISOString(),
  ): CooperativeTableSnapshot {
    if (!context.publicState) {
      throw new Error("cannot build a snapshot without a public table state");
    }
    return {
      createdAt,
      dealerCommitmentRoot: context.publicState.dealerCommitment?.rootHash ?? null,
      epoch: context.currentEpoch,
      foldedPlayerIds: context.publicState.foldedPlayerIds,
      handId: context.publicState.handId,
      handNumber: context.publicState.handNumber,
      latestEventHash: context.lastEventHash,
      livePlayerIds: context.publicState.livePlayerIds,
      phase: context.publicState.phase,
      potSats: context.publicState.potSats,
      previousSnapshotHash: context.latestSnapshot ? this.snapshotHash(context.latestSnapshot) : null,
      seatedPlayers: context.publicState.seatedPlayers,
      sidePots: [],
      snapshotId,
      tableId: context.config.tableId,
      turnIndex: context.publicState.actingSeatIndex,
      chipBalances: context.publicState.chipBalances,
      signatures: [],
    };
  }

  private isSettlementBoundarySnapshot(snapshot: CooperativeTableSnapshot) {
    return snapshot.phase === null || snapshot.phase === "settled";
  }

  private resolvePeerProtocolPubkey(context: MeshTableContext, peerId: string) {
    if (peerId === this.peerIdentity?.id) {
      return this.protocolIdentity?.publicKeyHex ?? null;
    }
    const seatedPlayer = [...context.pendingPlayers.values()].find((candidate) => candidate.peerId === peerId);
    if (seatedPlayer) {
      return seatedPlayer.protocolPubkeyHex;
    }
    const knownPeer =
      context.peerAddresses.get(peerId) ??
      this.transport?.listKnownPeers().find((candidate) => candidate.peerId === peerId);
    if (knownPeer?.protocolPubkeyHex) {
      return knownPeer.protocolPubkeyHex;
    }
    if (peerId === context.currentHostPeerId) {
      return context.advertisement?.hostProtocolPubkeyHex ?? null;
    }
    return null;
  }

  private assertJoinIntentSignature(intent: PlayerJoinIntent) {
    if (!verifyIdentityBinding(intent.identityBinding)) {
      throw new Error("invalid join identity binding");
    }
    const unsignedIntent = {
      identityBinding: intent.identityBinding,
      peerId: intent.peerId,
      peerUrl: intent.peerUrl,
      player: intent.player,
      protocolId: intent.protocolId,
      protocolPubkeyHex: intent.protocolPubkeyHex,
      requestedAt: intent.requestedAt,
      tableId: intent.tableId,
      ...(intent.requestedSeatIndex !== undefined
        ? { requestedSeatIndex: intent.requestedSeatIndex }
        : {}),
    };
    if (!verifyStructuredData(intent.protocolPubkeyHex, unsignedIntent, intent.signatureHex)) {
      throw new Error("invalid join request signature");
    }
  }

  private assertActionIntentSignature(intent: PlayerActionIntent) {
    const unsignedIntent = {
      action: intent.action,
      epoch: intent.epoch,
      handId: intent.handId,
      playerId: intent.playerId,
      protocolPubkeyHex: intent.protocolPubkeyHex,
      requestedAt: intent.requestedAt,
      seatIndex: intent.seatIndex,
      tableId: intent.tableId,
    };
    if (!verifyStructuredData(intent.protocolPubkeyHex, unsignedIntent, intent.signatureHex)) {
      throw new Error("invalid action signature");
    }
  }

  private assertFundsReceipt(args: {
    context: MeshTableContext;
    expectedKind: TableFundsOperation["kind"];
    expectedPlayerId: string;
    expectedStatus: TableFundsOperation["status"];
    receipt: TableFundsOperation;
    walletPubkeyHex: string;
  }) {
    if (!verifyTableFundsOperationSignature(args.receipt)) {
      throw new Error("table funds receipt signature is invalid");
    }
    if (args.receipt.provider !== this.expectedFundsProviderName()) {
      throw new Error(`unexpected funds provider ${args.receipt.provider}`);
    }
    if (args.receipt.kind !== args.expectedKind || args.receipt.status !== args.expectedStatus) {
      throw new Error(`unexpected funds receipt state ${args.receipt.kind}/${args.receipt.status}`);
    }
    if (args.receipt.playerId !== args.expectedPlayerId) {
      throw new Error("table funds receipt player identity does not match the joiner");
    }
    if (args.receipt.tableId !== args.context.config.tableId) {
      throw new Error("table funds receipt table id does not match the current table");
    }
    if (args.receipt.networkId !== args.context.config.networkId) {
      throw new Error("table funds receipt network does not match the current table");
    }
    if (args.receipt.signerPubkeyHex !== args.walletPubkeyHex) {
      throw new Error("table funds receipt signer does not match the bound wallet identity");
    }
  }

  private verifySnapshotSignature(
    context: MeshTableContext,
    snapshot: CooperativeTableSnapshot,
    signature: TableSnapshotSignature,
  ) {
    const expectedPubkey =
      this.resolvePeerProtocolPubkey(context, signature.signerPeerId) ?? signature.signerPubkeyHex;
    if (expectedPubkey !== signature.signerPubkeyHex) {
      return false;
    }
    if (
      signature.signerRole === "player" &&
      !snapshot.seatedPlayers.some((player) => player.peerId === signature.signerPeerId)
    ) {
      return false;
    }
    if (signature.signerRole === "host" && signature.signerPeerId !== context.currentHostPeerId) {
      return false;
    }
    if (signature.signerRole === "witness" && !context.witnessSet.includes(signature.signerPeerId)) {
      return false;
    }
    if (signature.signerRole !== "player" && signature.signerRole !== "witness" && signature.signerRole !== "host") {
      return false;
    }
    return verifyStructuredData(
      signature.signerPubkeyHex,
      this.unsignedSnapshot(snapshot),
      signature.signatureHex,
    );
  }

  private assertSnapshot(context: MeshTableContext, snapshot: CooperativeTableSnapshot, requireQuorum = true) {
    if (snapshot.tableId !== context.config.tableId) {
      throw new Error("snapshot table id does not match the active context");
    }
    const expected = this.buildSnapshotBase(context, snapshot.snapshotId, snapshot.createdAt);
    if (
      stableStringify(this.unsignedSnapshot(snapshot)) !== stableStringify(this.unsignedSnapshot(expected))
    ) {
      throw new Error("snapshot body does not match the current canonical public state");
    }
    const seenSigners = new Set<string>();
    for (const signature of snapshot.signatures) {
      if (seenSigners.has(signature.signerPeerId)) {
        throw new Error("snapshot contains duplicate signer entries");
      }
      if (!this.verifySnapshotSignature(context, snapshot, signature)) {
        throw new Error(`snapshot signature from ${signature.signerPeerId} is invalid`);
      }
      seenSigners.add(signature.signerPeerId);
    }
    if (requireQuorum && !this.hasRequiredSignatures(context, snapshot)) {
      throw new Error("snapshot is missing one or more required signatures");
    }
  }

  private async waitForSnapshotState(
    context: MeshTableContext,
    snapshot: CooperativeTableSnapshot,
    timeoutMs = 1_000,
  ) {
    const start = Date.now();
    while (Date.now() - start < timeoutMs) {
      if (context.publicState && context.lastEventHash === snapshot.latestEventHash) {
        const expected = this.buildSnapshotBase(context, snapshot.snapshotId, snapshot.createdAt);
        if (
          stableStringify(this.unsignedSnapshot(snapshot)) ===
          stableStringify(this.unsignedSnapshot(expected))
        ) {
          return;
        }
      }
      await new Promise<void>((resolve) => {
        setTimeout(resolve, 25);
      });
    }
  }

  private requiredLeaseSignerPeerIds(lease: HostLease) {
    return [...new Set([lease.hostPeerId, ...lease.witnessSet])];
  }

  private leaseWitnessMatches(
    context: MeshTableContext,
    lease: HostLease,
    signature: Pick<HostLeaseSignature, "signerPeerId" | "signerPubkeyHex">,
  ) {
    if (lease.witnessSet.includes(signature.signerPeerId)) {
      return true;
    }
    return lease.witnessSet.some((peerId) => {
      const knownPeer =
        context.peerAddresses.get(peerId) ??
        this.transport?.listKnownPeers().find((candidate) => candidate.peerId === peerId);
      return knownPeer?.protocolPubkeyHex === signature.signerPubkeyHex;
    });
  }

  private leaseHasRequiredSigner(
    context: MeshTableContext,
    lease: HostLease,
    requiredPeerId: string,
  ) {
    if (requiredPeerId === lease.hostPeerId) {
      return lease.signatures.some(
        (signature) =>
          signature.signerPeerId === lease.hostPeerId && signature.signerRole === "host",
      );
    }
    return lease.signatures.some(
      (signature) =>
        signature.signerRole === "witness" &&
        this.leaseWitnessMatches(context, lease, signature) &&
        (signature.signerPeerId === requiredPeerId ||
          this.resolvePeerProtocolPubkey(context, requiredPeerId) === signature.signerPubkeyHex),
    );
  }

  private verifyLeaseSignature(
    context: MeshTableContext,
    lease: HostLease,
    signature: HostLeaseSignature,
  ) {
    const expectedPubkey =
      this.resolvePeerProtocolPubkey(context, signature.signerPeerId) ?? signature.signerPubkeyHex;
    if (expectedPubkey !== signature.signerPubkeyHex) {
      return false;
    }
    if (signature.signerPeerId === lease.hostPeerId && signature.signerRole !== "host") {
      return false;
    }
    if (signature.signerRole === "witness" && !this.leaseWitnessMatches(context, lease, signature)) {
      return false;
    }
    if (signature.signerRole !== "host" && signature.signerRole !== "witness") {
      return false;
    }
    return verifyStructuredData(
      signature.signerPubkeyHex,
      this.unsignedLease(lease),
      signature.signatureHex,
    );
  }

  private assertLease(context: MeshTableContext, lease: HostLease, requireQuorum = true) {
    if (lease.tableId !== context.config.tableId) {
      throw new Error("lease table id does not match the active table");
    }
    if (Date.parse(lease.leaseExpiry) <= Date.parse(lease.leaseStart)) {
      throw new Error("lease expiry must be after the lease start");
    }
    const seenSigners = new Set<string>();
    for (const signature of lease.signatures) {
      if (seenSigners.has(signature.signerPeerId)) {
        throw new Error("lease contains duplicate signer entries");
      }
      if (!this.verifyLeaseSignature(context, lease, signature)) {
        throw new Error(`lease signature from ${signature.signerPeerId} is invalid`);
      }
      seenSigners.add(signature.signerPeerId);
    }
    if (requireQuorum) {
      for (const peerId of this.requiredLeaseSignerPeerIds(lease)) {
        if (!this.leaseHasRequiredSigner(context, lease, peerId)) {
          throw new Error("lease quorum is incomplete");
        }
      }
    }
  }

  private requiredFailoverPeerIds(context: MeshTableContext) {
    return [...new Set([
      ...context.witnessSet,
      ...(context.publicState?.seatedPlayers.map((player) => player.peerId) ?? []),
    ])];
  }

  private verifyFailoverAcceptance(context: MeshTableContext, acceptance: HostFailoverAcceptance) {
    const expectedPubkey = this.resolvePeerProtocolPubkey(context, acceptance.signerPeerId);
    if (!expectedPubkey || expectedPubkey !== acceptance.signerPubkeyHex) {
      return false;
    }
    if (acceptance.tableId !== context.config.tableId) {
      return false;
    }
    if (acceptance.currentEpoch !== context.currentEpoch) {
      return false;
    }
    if (acceptance.proposedEpoch !== context.currentEpoch + 1) {
      return false;
    }
    if (!this.requiredFailoverPeerIds(context).includes(acceptance.signerPeerId)) {
      return false;
    }
    const { signatureHex, ...acceptanceBase } = acceptance;
    return verifyStructuredData(acceptance.signerPubkeyHex, acceptanceBase, signatureHex);
  }

  private async recordSettlementCheckpoint(context: MeshTableContext, snapshot: CooperativeTableSnapshot) {
    if (!this.walletIdentity) {
      return;
    }
    if (!snapshot.seatedPlayers.some((player) => player.playerId === this.walletIdentity?.playerId)) {
      return;
    }
    const checkpointHash = this.snapshotHash(snapshot);
    const currentReceipt = context.buyInReceipts.get(this.walletIdentity.playerId);
    if (
      currentReceipt?.kind === "checkpoint-recorded" &&
      currentReceipt.checkpointHash === checkpointHash
    ) {
      return;
    }
    const record: TableCheckpointRecord = {
      balances: snapshot.chipBalances,
      checkpointHash,
      participants: snapshot.seatedPlayers.map((player) => ({
        arkAddress: player.arkAddress,
        buyInSats: player.buyInSats,
        peerId: player.peerId,
        playerId: player.playerId,
      })),
      tableId: context.config.tableId,
    };
    const receipt = await this.createFundsProvider().recordCheckpoint(record);
    context.buyInReceipts.set(receipt.playerId, receipt);
  }

  private async waitForKnownPeer(peerUrl: string, provisionalPeerId: string, timeoutMs: number) {
    const start = Date.now();
    while (Date.now() - start < timeoutMs) {
      const resolvedPeer = this.transport
        ?.listKnownPeers()
        .find((candidate) => candidate.peerUrl === peerUrl && candidate.peerId !== provisionalPeerId);
      if (resolvedPeer) {
        return resolvedPeer;
      }
      await new Promise<void>((resolve) => {
        setTimeout(resolve, 50);
      });
    }
    return this.transport?.listKnownPeers().find((candidate) => candidate.peerUrl === peerUrl);
  }

  private async saveMeshTableReference(
    tableId: string,
    reference: NonNullable<PlayerProfileState["meshTables"]>[string],
  ) {
    const profile = await this.ensureProfileState();
    profile.currentMeshTableId = tableId;
    profile.meshTables = {
      ...(profile.meshTables ?? {}),
      [tableId]: reference,
    };
    await this.profileStore.save(profile);
  }

  private buildFundsWarnings(): FundsWarning[] {
    const warnings: FundsWarning[] = [];
    const now = Date.now();
    for (const context of this.contextByTableId.values()) {
      for (const receipt of context.buyInReceipts.values()) {
        if (!receipt.vtxoExpiry) {
          continue;
        }
        const expiresAt = Date.parse(receipt.vtxoExpiry);
        if (Number.isNaN(expiresAt)) {
          continue;
        }
        const delta = expiresAt - now;
        if (delta <= 0) {
          warnings.push({
            expiresAt: receipt.vtxoExpiry,
            playerId: receipt.playerId,
            severity: "critical",
            tableId: context.config.tableId,
          });
        } else if (delta <= 5 * 60_000) {
          warnings.push({
            expiresAt: receipt.vtxoExpiry,
            playerId: receipt.playerId,
            severity: "warning",
            tableId: context.config.tableId,
          });
        }
      }
    }
    return warnings;
  }

  private async waitForTableContext(tableId: string, timeoutMs = 5_000) {
    const start = Date.now();
    while (Date.now() - start < timeoutMs) {
      const context = this.contextByTableId.get(tableId);
      if (context) {
        return {
          config: context.config,
          events: [...context.events],
          latestFullySignedSnapshot: context.latestFullySignedSnapshot,
          latestSnapshot: context.latestSnapshot,
          publicState: context.publicState,
        };
      }
      await new Promise<void>((resolve) => {
        setTimeout(resolve, 50);
      });
    }
    throw new Error(`timed out waiting for table ${tableId}`);
  }
}
