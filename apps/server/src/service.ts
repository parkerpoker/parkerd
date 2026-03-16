import { createHash } from "node:crypto";

import {
  applyHoldemAction,
  buildCommitmentHash,
  createHoldemHand,
  deriveDeckSeed,
  toCheckpointShape,
  type HoldemAction,
  type HoldemState,
} from "@parker/game-engine";
import {
  createTableRequestSchema,
  joinTableRequestSchema,
  type DeckCommitment,
  type Network,
  type SignedGameAction,
  type TableCheckpoint,
  type TableConfig,
  type TableSnapshot,
  type TimeoutDelegation,
} from "@parker/protocol";
import {
  buildEscrowDescriptor,
  createMockSettlementProvider,
  type SettlementProvider,
} from "@parker/settlement";

import { ParkerDatabase } from "./db.js";

export interface ParkerTableServiceConfig {
  websocketUrl: string;
  network?: Network;
  refundDelayBlocks?: number;
  settlementProvider?: SettlementProvider;
}

function generateInviteCode(): string {
  return createHash("sha256")
    .update(crypto.randomUUID())
    .digest("hex")
    .slice(0, 8)
    .toUpperCase();
}

function actionToEngineAction(action: SignedGameAction): HoldemAction {
  switch (action.payload.type) {
    case "fold":
      return { type: "fold" };
    case "check":
      return { type: "check" };
    case "call":
      return { type: "call" };
    case "bet":
      return { type: "bet", totalSats: action.payload.totalSats };
    case "raise":
      return { type: "raise", totalSats: action.payload.totalSats };
    case "timeout-fold":
      return { type: "fold" };
    default:
      throw new Error(`unsupported action payload ${action.payload.type}`);
  }
}

export class ParkerTableService {
  readonly db: ParkerDatabase;
  private readonly websocketUrl: string;
  private readonly network: Network;
  private readonly refundDelayBlocks: number;
  private readonly settlementProvider: SettlementProvider;

  constructor(db: ParkerDatabase, config: ParkerTableServiceConfig) {
    this.db = db;
    this.websocketUrl = config.websocketUrl;
    this.network = config.network ?? "mutinynet";
    this.refundDelayBlocks = config.refundDelayBlocks ?? 12;
    this.settlementProvider = config.settlementProvider ?? createMockSettlementProvider(this.network);
  }

  createTable(input: unknown) {
    const request = createTableRequestSchema.parse(input);
    const tableId = crypto.randomUUID();
    const inviteCode = generateInviteCode();
    const now = new Date().toISOString();

    const table: TableConfig = {
      tableId,
      inviteCode,
      hostPlayerId: request.host.playerId,
      network: this.network,
      variant: "holdem-heads-up",
      smallBlindSats: request.smallBlindSats,
      bigBlindSats: request.bigBlindSats,
      buyInMinSats: request.buyInMinSats,
      buyInMaxSats: request.buyInMaxSats,
      commitmentDeadlineSeconds: request.commitmentDeadlineSeconds,
      actionTimeoutSeconds: request.actionTimeoutSeconds,
      createdAt: now,
      status: "waiting",
    };

    const seatReservation = {
      reservationId: crypto.randomUUID(),
      tableId,
      seatIndex: 0 as const,
      buyInSats: request.buyInMinSats,
      status: "reserved" as const,
      inviteCode,
      createdAt: now,
      expiresAt: new Date(Date.now() + 10 * 60_000).toISOString(),
      player: request.host,
    };

    const emptySeat = {
      reservationId: crypto.randomUUID(),
      tableId,
      seatIndex: 1 as const,
      buyInSats: request.buyInMinSats,
      status: "reserved" as const,
      inviteCode,
      createdAt: now,
      expiresAt: new Date(Date.now() + 10 * 60_000).toISOString(),
      player: {
        playerId: "open-seat",
        nickname: "Open Seat",
        joinedAt: now,
        pubkeyHex: "00",
        arkAddress: "tark1openseat",
      },
    };

    const snapshot: TableSnapshot = {
      table,
      seats: [seatReservation, emptySeat],
      commitments: [],
      pendingDelegations: [],
    };

    this.db.saveSnapshot(snapshot);
    return {
      table,
      seatReservation,
      websocketUrl: this.websocketUrl,
    };
  }

  joinTable(input: unknown) {
    const request = joinTableRequestSchema.parse(input);
    const snapshot = this.db.getSnapshotByInviteCode(request.inviteCode);
    if (!snapshot) {
      throw new Error("table not found");
    }

    const now = new Date().toISOString();
    const seatReservation = {
      reservationId: crypto.randomUUID(),
      tableId: snapshot.table.tableId,
      seatIndex: 1 as const,
      buyInSats: request.buyInSats,
      status: "locked" as const,
      inviteCode: snapshot.table.inviteCode,
      createdAt: now,
      expiresAt: new Date(Date.now() + 10 * 60_000).toISOString(),
      player: request.player,
    };

    snapshot.seats = [snapshot.seats[0]!, seatReservation];
    snapshot.table.status = "buy-in";
    const totalLocked = snapshot.seats.reduce((total, seat) => total + seat.buyInSats, 0);
    snapshot.escrow = buildEscrowDescriptor({
      tableId: snapshot.table.tableId,
      network: this.network,
      participantPubkeys: [
        snapshot.seats[0]!.player.pubkeyHex,
        snapshot.seats[1]!.player.pubkeyHex,
      ],
      watchtowerPubkey: snapshot.seats[0]!.player.pubkeyHex,
      totalLockedSats: totalLocked,
      refundDelayBlocks: this.refundDelayBlocks,
    });
    snapshot.table.status = "seeding";
    this.db.saveSnapshot(snapshot);
    return {
      table: snapshot.table,
      seatReservation,
    };
  }

  getSnapshot(tableId: string) {
    const snapshot = this.db.getSnapshot(tableId);
    if (!snapshot) {
      throw new Error("table not found");
    }
    return snapshot;
  }

  saveDelegation(tableId: string, delegation: TimeoutDelegation) {
    const snapshot = this.getSnapshot(tableId);
    snapshot.pendingDelegations = snapshot.pendingDelegations.filter(
      (candidate) => candidate.delegationId !== delegation.delegationId,
    );
    snapshot.pendingDelegations.push(delegation);
    this.db.saveDelegation(delegation);
    this.db.saveSnapshot(snapshot);
    return snapshot;
  }

  saveCommitment(tableId: string, input: DeckCommitment) {
    const snapshot = this.getSnapshot(tableId);
    snapshot.commitments = snapshot.commitments.filter(
      (candidate) => candidate.seatIndex !== input.seatIndex,
    );
    snapshot.commitments.push(input);

    const completeReveals = snapshot.commitments.filter((commitment) => commitment.revealSeed);
    if (snapshot.escrow && snapshot.commitments.length === 2 && completeReveals.length === 2) {
      this.startHand(snapshot);
    } else {
      this.db.saveSnapshot(snapshot);
    }

    return this.getSnapshot(tableId);
  }

  private startHand(snapshot: TableSnapshot) {
    const existing = this.db.getHandState(snapshot.table.tableId);
    const nextHandNumber = existing ? existing.handNumber + 1 : 1;
    const deckSeedHex = deriveDeckSeed({
      tableId: snapshot.table.tableId,
      handNumber: nextHandNumber,
      commitments: snapshot.commitments,
      reveals: snapshot.commitments.filter((commitment) => commitment.revealSeed),
    });

    const currentStacks =
      snapshot.checkpoint?.playerStacks ??
      Object.fromEntries(snapshot.seats.map((seat) => [seat.player.playerId, seat.buyInSats]));

    const hand = createHoldemHand({
      handId: crypto.randomUUID(),
      handNumber: nextHandNumber,
      deckSeedHex,
      dealerSeatIndex: ((nextHandNumber - 1) % 2) as 0 | 1,
      smallBlindSats: snapshot.table.smallBlindSats,
      bigBlindSats: snapshot.table.bigBlindSats,
      seats: [
        {
          playerId: snapshot.seats[0]!.player.playerId,
          stackSats: currentStacks[snapshot.seats[0]!.player.playerId]!,
        },
        {
          playerId: snapshot.seats[1]!.player.playerId,
          stackSats: currentStacks[snapshot.seats[1]!.player.playerId]!,
        },
      ],
    });

    snapshot.table.status = "active";
    snapshot.checkpoint = this.buildCheckpoint(snapshot, hand);
    snapshot.pendingDelegations = [];
    if (snapshot.escrow) {
      snapshot.escrow.currentCheckpointId = snapshot.checkpoint.checkpointId;
    }
    this.db.saveHandState(snapshot.table.tableId, hand);
    this.db.saveCheckpoint(snapshot.checkpoint);
    this.db.saveSnapshot(snapshot);
  }

  private buildCheckpoint(snapshot: TableSnapshot, hand: HoldemState): TableCheckpoint {
    const checkpointShape = toCheckpointShape(hand);
    const checkpointBase = {
      checkpointId: crypto.randomUUID(),
      tableId: snapshot.table.tableId,
      handId: hand.handId,
      handNumber: hand.handNumber,
      ...checkpointShape,
      holeCardsByPlayerId:
        hand.phase === "settled"
          ? checkpointShape.holeCardsByPlayerId
          : Object.fromEntries(
              Object.keys(checkpointShape.holeCardsByPlayerId).map((playerId) => [playerId, ["XX", "XX"]]),
            ),
      deckSeedHash: createHash("sha256").update(hand.deckSeedHex).digest("hex"),
      commitmentHashes: [...snapshot.commitments]
        .sort((left, right) => left.seatIndex - right.seatIndex)
        .map((commitment) => commitment.commitmentHash) as [string, string],
      nextActionDeadline:
        hand.phase === "settled"
          ? null
          : new Date(Date.now() + snapshot.table.actionTimeoutSeconds * 1_000).toISOString(),
      transcriptHash: createHash("sha256")
        .update(
          JSON.stringify({
            tableId: snapshot.table.tableId,
            handId: hand.handId,
            handNumber: hand.handNumber,
            actionCount: hand.actionLog.length,
            board: hand.board,
          }),
        )
        .digest("hex"),
      signatures: [],
    } satisfies Omit<TableCheckpoint, "signatures"> & { signatures: [] };

    return checkpointBase;
  }

  processSignedAction(event: SignedGameAction) {
    const snapshot = this.getSnapshot(event.tableId);
    const hand = this.db.getHandState(event.tableId);
    if (!hand) {
      throw new Error("hand not started");
    }

    const nextHand = applyHoldemAction(hand, event.actorSeatIndex, actionToEngineAction(event));
    snapshot.checkpoint = this.buildCheckpoint(snapshot, nextHand);
    snapshot.pendingDelegations = snapshot.pendingDelegations.filter(
      (delegation) => delegation.checkpointId !== event.checkpointId,
    );
    if (snapshot.escrow) {
      snapshot.escrow.currentCheckpointId = snapshot.checkpoint.checkpointId;
      snapshot.escrow.status = nextHand.phase === "settled" ? "settling" : "funded";
    }
    this.db.saveHandState(snapshot.table.tableId, nextHand);
    this.db.saveCheckpoint(snapshot.checkpoint);
    this.db.appendEvent({
      eventId: crypto.randomUUID(),
      tableId: snapshot.table.tableId,
      eventType: "signed-action",
      payload: event,
      createdAt: new Date().toISOString(),
    });
    this.db.saveSnapshot(snapshot);
    return snapshot;
  }

  runTimeoutSweep() {
    const now = Date.now();
    for (const snapshot of this.db.listSnapshots()) {
      const checkpoint = snapshot.checkpoint;
      if (!checkpoint?.nextActionDeadline || checkpoint.phase === "settled") {
        continue;
      }

      const expiresAt = Date.parse(checkpoint.nextActionDeadline);
      if (Number.isNaN(expiresAt) || expiresAt > now || checkpoint.actingSeatIndex === null) {
        continue;
      }

      const delegation = snapshot.pendingDelegations.find(
        (candidate) =>
          candidate.checkpointId === checkpoint.checkpointId &&
          candidate.actingSeatIndex === checkpoint.actingSeatIndex,
      );

      if (!delegation) {
        this.db.appendEvent({
          eventId: crypto.randomUUID(),
          tableId: snapshot.table.tableId,
          eventType: "timeout-missed",
          payload: { checkpointId: checkpoint.checkpointId, actingSeatIndex: checkpoint.actingSeatIndex },
          createdAt: new Date().toISOString(),
        });
        continue;
      }

      this.processSignedAction({
        tableId: snapshot.table.tableId,
        handId: checkpoint.handId,
        checkpointId: checkpoint.checkpointId,
        clientSeq: Date.now(),
        actorPlayerId: delegation.delegatedPlayerId,
        actorSeatIndex: delegation.actingSeatIndex,
        sentAt: new Date().toISOString(),
        signerPubkeyHex: delegation.signatureHex.slice(0, 66),
        signatureHex: delegation.signatureHex,
        payload: { type: "timeout-fold" },
      });
    }
  }

  listTranscript(tableId: string) {
    return {
      checkpoints: this.db.listCheckpoints(tableId),
      events: this.db.listEvents(tableId),
    };
  }

  buildClientCommitment(args: {
    tableId: string;
    seatIndex: number;
    playerId: string;
    seedHex: string;
  }): DeckCommitment {
    return {
      tableId: args.tableId,
      handNumber: this.db.getHandState(args.tableId)?.handNumber ?? 1,
      seatIndex: args.seatIndex,
      playerId: args.playerId,
      commitmentHash: buildCommitmentHash({
        tableId: args.tableId,
        seatIndex: args.seatIndex,
        playerId: args.playerId,
        seedHex: args.seedHex,
      }),
    };
  }
}
