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
  verifyIdentityBinding,
  verifyStructuredData,
  type LocalIdentity,
  type ScopedIdentity,
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
        onPeerSeen: (peer) => {
          void this.rememberPeer(peer);
        },
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
    const lease = await this.buildHostLease(tableId, 1, witnessPeerIds);
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
    await this.appendLocalEvent(context, {
      lease,
      type: "HostLeaseGranted",
    });
    await this.store.upsertPublicAd(advertisement);
    await this.saveMeshTableReference(tableId, {
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
    const joinIntent = this.buildJoinIntent(invite, profile.nickname, preparedBuyIn.amountSats);
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
    if (context.role === "host" && context.witnessSet.length > 0) {
      const targetPeerId = context.witnessSet[0]!;
      await this.performHostRotation(context, targetPeerId, "manual rotation");
      return this.currentState();
    }
    if (context.role === "witness") {
      await this.triggerFailover(context, "manual witness rotation");
      return this.currentState();
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
    return await this.createFundsProvider().emergencyExit(
      context.config.tableId,
      this.walletIdentity.playerId,
      this.snapshotHash(latestSnapshot),
      balance,
    );
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
          roles: ["player"],
        });
        void this.rememberPeer({
          alias: body.intent.player.nickname,
          peerId: body.intent.peerId,
          peerUrl: body.intent.peerUrl,
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
        context.config.status = "aborted";
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
        const requiredPeerIds = this.requiredSnapshotPeerIds(context);
        const signerPeerIds = snapshot.signatures.map((signature) => signature.signerPeerId);
        const missingPeerIds = requiredPeerIds.filter((peerId) => !signerPeerIds.includes(peerId));
        const hasRequiredSignatures = missingPeerIds.length === 0;
        if (hasRequiredSignatures) {
          context.latestFullySignedSnapshot = snapshot;
        }
        break;
      }
      case "HostLeaseGranted":
        context.currentEpoch = body.lease.epoch;
        context.currentHostPeerId = body.lease.hostPeerId;
        context.witnessSet = this.resolveWitnessSet(context, body.lease);
        break;
      case "HostRotated":
        context.currentEpoch = body.newEpoch;
        context.currentHostPeerId = body.newHostPeerId;
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
    await this.captureSnapshot(context);
  }

  private async captureSnapshot(context: MeshTableContext) {
    if (!this.protocolIdentity || !this.peerIdentity || !context.publicState) {
      return;
    }
    const snapshotBase = {
      createdAt: new Date().toISOString(),
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
      snapshotId: crypto.randomUUID(),
      tableId: context.config.tableId,
      turnIndex: context.publicState.actingSeatIndex,
      chipBalances: context.publicState.chipBalances,
      signatures: [] as TableSnapshotSignature[],
    } satisfies CooperativeTableSnapshot;

    const selfRole = context.role === "witness" ? "witness" : "host";
    const signatures: TableSnapshotSignature[] = [
      {
        signatureHex: signStructuredData(this.protocolIdentity, this.unsignedSnapshot(snapshotBase)),
        signedAt: new Date().toISOString(),
        signerPeerId: this.peerIdentity.id,
        signerPubkeyHex: this.protocolIdentity.publicKeyHex,
        signerRole: selfRole,
      },
    ];

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
      try {
        const response = await this.transport?.request(peerId, {
          snapshot: snapshotBase,
          type: "snapshot-sign-request",
        });
        if (response?.type === "snapshot-sign-response") {
          signatures.push(response.signature);
        }
      } catch (error) {
        this.logger.info("snapshot signature request failed", {
          error: (error as Error).message,
          peerId,
          tableId: context.config.tableId,
        });
      }
    }

    const snapshot: CooperativeTableSnapshot = {
      ...snapshotBase,
      signatures,
    };
    const senderRole = context.role === "witness" ? "witness" : "host";
    context.snapshots.push(snapshot);
    context.latestSnapshot = snapshot;
    if (this.hasRequiredSignatures(context, snapshot)) {
      context.latestFullySignedSnapshot = snapshot;
    }
    await this.store.appendSnapshot(context.config.tableId, snapshot);
    await this.appendLocalEvent(
      context,
      {
        snapshot,
        type: "WitnessSnapshot",
      },
      senderRole,
    );
  }

  private async handlePeerFrame(peerId: string, frame: MeshWireFrame) {
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
          this.contextByTableId.set(frame.event.tableId, context);
        }
        if (!context) {
          throw new Error(`received event for unknown table ${frame.event.tableId}`);
        }
        await this.acceptEvent(context, frame.event, true);
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
        if (frame.epoch >= context.currentEpoch) {
          context.lastHostHeartbeatAt = Date.now();
        }
        return;
      }
      case "public-ad":
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
      await this.transport.send(peerId, {
        body: response,
        kind: "response",
        ok: true,
        requestId,
      });
    } catch (error) {
      await this.transport.send(peerId, {
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
        return await this.handleSnapshotSignRequest(body.snapshot);
      case "lease-sign-request":
        return await this.handleLeaseSignRequest(body.lease);
      case "failover-accept-request":
        return await this.handleFailoverAcceptanceRequest(body.tableId, body.currentEpoch, body.proposedEpoch, body.proposedHostPeerId);
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
    _peerId: string,
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
    _peerId: string,
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
    const unsignedConfirmation = {
      confirmedAt: confirmation.confirmedAt,
      playerId: confirmation.playerId,
      receipt: this.unsignedFundsOperation(confirmation.receipt),
      tableId: confirmation.tableId,
    };
    if (
      !verifyStructuredData(
        pending.protocolPubkeyHex,
        unsignedConfirmation,
        confirmation.signatureHex,
      )
    ) {
      throw new Error("invalid buy-in confirmation signature");
    }
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
    if (context.buyInReceipts.size === context.config.seatCount) {
      const readyState = this.buildReadyPublicState(context);
      await this.appendLocalEvent(context, {
        balances: readyState.chipBalances,
        publicState: readyState,
        type: "TableReady",
      });
      await this.captureSnapshot(context);
      this.scheduleNextHand(context);
    }
    return {
      accepted: true,
      type: "buy-in-response",
    };
  }

  private async handleActionRequest(
    _peerId: string,
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

    const hand = context.privateState.activeHand;
    if (!hand || hand.handId !== intent.handId) {
      return {
        accepted: false,
        reason: "hand is not active",
        type: "action-response",
      };
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
        await this.captureSnapshot(context);
        this.scheduleNextHand(context);
      } else if (previous.phase !== next.phase) {
        await this.appendLocalEvent(context, {
          handId: intent.handId,
          publicState,
          street: previous.phase,
          type: "StreetClosed",
        });
        await this.captureSnapshot(context);
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

  private async handleSnapshotSignRequest(snapshot: CooperativeTableSnapshot): Promise<MeshResponseBody> {
    if (!this.protocolIdentity || !this.peerIdentity) {
      throw new Error("protocol identity is unavailable");
    }
    await this.store.appendSnapshot(snapshot.tableId, snapshot);
    const context = this.contextByTableId.get(snapshot.tableId);
    if (context) {
      context.snapshots.push(snapshot);
      context.latestSnapshot = snapshot;
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

  private async handleLeaseSignRequest(lease: HostLease): Promise<MeshResponseBody> {
    if (!this.protocolIdentity || !this.peerIdentity) {
      throw new Error("protocol identity is unavailable");
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
    tableId: string,
    currentEpoch: number,
    proposedEpoch: number,
    proposedHostPeerId: string,
  ): Promise<MeshResponseBody> {
    if (!this.protocolIdentity || !this.peerIdentity) {
      throw new Error("protocol identity is unavailable");
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

      const acceptances: HostFailoverAcceptance[] = [];
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
          if (response?.type === "failover-accept-response") {
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
        } catch {
          // best-effort; failover still proceeds for surviving peers
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

      if (context.publicState?.phase && context.publicState.phase !== "settled" && context.latestFullySignedSnapshot) {
        await this.appendLocalEvent(
          context,
          {
            handId: context.publicState.handId ?? crypto.randomUUID(),
            reason: "host disappeared mid-hand",
            rollbackSnapshotHash: this.snapshotHash(context.latestFullySignedSnapshot),
            type: "HandAbort",
          },
          "host",
        );
        await this.captureSnapshot(context);
      }
      this.scheduleNextHand(context);
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
    const leaseBase: HostLease = {
      epoch,
      hostPeerId: this.peerIdentity.id,
      leaseExpiry: new Date(Date.now() + HOST_LEASE_DURATION_MS).toISOString(),
      leaseStart: new Date().toISOString(),
      signatures: [],
      tableId,
      witnessSet,
    };
    const signatures: HostLeaseSignature[] = [
      {
        signatureHex: signStructuredData(this.protocolIdentity, this.unsignedLease(leaseBase)),
        signedAt: new Date().toISOString(),
        signerPeerId: this.peerIdentity.id,
        signerPubkeyHex: this.protocolIdentity.publicKeyHex,
        signerRole: "host",
      },
    ];
    for (const witnessPeerId of witnessSet) {
      try {
        const response = await this.transport.request(witnessPeerId, {
          lease: leaseBase,
          type: "lease-sign-request",
        });
        if (response?.type === "lease-sign-response") {
          signatures.push(response.signature);
        }
      } catch {
        // Witness signatures are best-effort in dev/test mode.
      }
    }
    return {
      ...leaseBase,
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

  private buildJoinIntent(invite: PrivateInvite, nickname: string, buyInSats: number): PlayerJoinIntent {
    if (!this.peerIdentity || !this.protocolIdentity || !this.walletIdentity || !this.transport) {
      throw new Error("mesh identities are unavailable");
    }
    const player = {
      arkAddress: `tark1${this.walletIdentity.playerId.slice(-16)}`,
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
        boardingAddress: `bcrt1q${this.walletIdentity.playerId.slice(-20).padEnd(20, "0")}`,
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
      latestFullySignedSnapshot: null,
      latestSnapshot: null,
      lastEventHash: null,
      lastHostHeartbeatAt: Date.now(),
      peerAddresses: new Map(),
      pendingPlayers: new Map(),
      pendingEvents: new Map(),
      privateState: {
        auditBundlesByHandId: {},
        myHoleCardsByHandId: {},
      },
      publicState: null,
      role: args.role,
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
            networkId: this.config.network,
            signer: this.walletIdentity,
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
      ...(context.publicState?.seatedPlayers.map((player) => player.peerId) ?? []),
      ...context.witnessSet,
      this.peerIdentity?.id ?? "",
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
        void this.rememberPeer(resolvedPeer);
      });
      return [...new Set(signerWitnessPeerIds)];
    }
    return [...lease.witnessSet];
  }

  private hasRequiredSignatures(context: MeshTableContext, snapshot: CooperativeTableSnapshot) {
    const requiredPeerIds = new Set(this.requiredSnapshotPeerIds(context));
    const actual = new Set(snapshot.signatures.map((signature) => signature.signerPeerId));
    for (const peerId of requiredPeerIds) {
      if (!actual.has(peerId)) {
        return false;
      }
    }
    return true;
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
    if (events.length === 0) {
      return;
    }
    const first = events[0]!;
    if (first.body.type !== "TableAnnounce") {
      return;
    }
    const context = this.createContext({
      ...(first.body.advertisement ? { advertisement: first.body.advertisement } : {}),
      config: first.body.table,
      currentEpoch: first.epoch,
      currentHostPeerId: first.body.table.hostPeerId,
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
        .find((snapshot) => this.hasRequiredSignatures(context, snapshot)) ?? null;
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
        roles: peer.roles ?? [],
        ...(peer.alias ? { alias: peer.alias } : {}),
        ...(peer.lastSeenAt ? { lastSeenAt: peer.lastSeenAt } : {}),
        ...(peer.relayPeerId ? { relayPeerId: peer.relayPeerId } : {}),
      });
    }
  }

  private async rememberPeer(peer: PeerAddress) {
    const profile = await this.ensureProfileState();
    const known = new Map((profile.knownPeers ?? []).map((entry) => [entry.peerId, entry] as const));
    const next: KnownPeerState = {
      lastSeenAt: peer.lastSeenAt ?? new Date().toISOString(),
      peerId: peer.peerId,
      peerUrl: peer.peerUrl,
      roles: peer.roles,
      ...(peer.alias ? { alias: peer.alias } : {}),
      ...(peer.relayPeerId ? { relayPeerId: peer.relayPeerId } : {}),
    };
    known.set(peer.peerId, next);
    profile.knownPeers = [...known.values()];
    await this.profileStore.save(profile);
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
        await this.commitAcceptedEvent(context, event, eventHash, true);
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
