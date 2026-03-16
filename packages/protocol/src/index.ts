import { z } from "zod";

export const networkSchema = z.enum(["mutinynet"]);
export type Network = z.infer<typeof networkSchema>;

export const gameVariantSchema = z.enum(["holdem-heads-up"]);
export type GameVariant = z.infer<typeof gameVariantSchema>;

export const tableStatusSchema = z.enum([
  "waiting",
  "buy-in",
  "seeding",
  "active",
  "settling",
  "closed",
]);
export type TableStatus = z.infer<typeof tableStatusSchema>;

export const streetSchema = z.enum([
  "preflop",
  "flop",
  "turn",
  "river",
  "showdown",
  "settled",
]);
export type Street = z.infer<typeof streetSchema>;

export const cardCodeSchema = z
  .string()
  .regex(/^(?:[2-9TJQKA][cdhs])$/, "card codes must match rank+suit form like Ah");
export type CardCode = z.infer<typeof cardCodeSchema>;
export const concealedCardCodeSchema = z.union([cardCodeSchema, z.literal("XX")]);

export const playerProfileSchema = z.object({
  playerId: z.string().min(8).max(96),
  nickname: z.string().min(1).max(32),
  joinedAt: z.string().datetime(),
  pubkeyHex: z.string().regex(/^[0-9a-f]+$/i),
  arkAddress: z.string().min(8),
  boardingAddress: z.string().min(8).optional(),
});
export type PlayerProfile = z.infer<typeof playerProfileSchema>;

export const seatReservationSchema = z.object({
  reservationId: z.string().uuid(),
  tableId: z.string().uuid(),
  seatIndex: z.number().int().min(0).max(1),
  buyInSats: z.number().int().positive(),
  status: z.enum(["reserved", "locked", "seated", "refunded"]),
  inviteCode: z.string().min(6).max(32),
  createdAt: z.string().datetime(),
  expiresAt: z.string().datetime(),
  player: playerProfileSchema,
});
export type SeatReservation = z.infer<typeof seatReservationSchema>;

export const deckCommitmentSchema = z.object({
  tableId: z.string().uuid(),
  handNumber: z.number().int().nonnegative(),
  seatIndex: z.number().int().min(0).max(1),
  playerId: z.string().min(8).max(96),
  commitmentHash: z.string().regex(/^[0-9a-f]{64}$/i),
  revealSeed: z.string().regex(/^[0-9a-f]{64}$/i).optional(),
  revealedAt: z.string().datetime().optional(),
});
export type DeckCommitment = z.infer<typeof deckCommitmentSchema>;

export const signedActionPayloadSchema = z.discriminatedUnion("type", [
  z.object({
    type: z.literal("reveal-seed"),
    seedHex: z.string().regex(/^[0-9a-f]{64}$/i),
  }),
  z.object({
    type: z.literal("fold"),
  }),
  z.object({
    type: z.literal("check"),
  }),
  z.object({
    type: z.literal("call"),
  }),
  z.object({
    type: z.literal("bet"),
    totalSats: z.number().int().positive(),
  }),
  z.object({
    type: z.literal("raise"),
    totalSats: z.number().int().positive(),
  }),
  z.object({
    type: z.literal("timeout-fold"),
  }),
  z.object({
    type: z.literal("ack-checkpoint"),
    checkpointId: z.string(),
  }),
]);
export type SignedActionPayload = z.infer<typeof signedActionPayloadSchema>;

export const signedGameActionSchema = z.object({
  tableId: z.string().uuid(),
  handId: z.string().uuid(),
  checkpointId: z.string(),
  clientSeq: z.number().int().nonnegative(),
  actorPlayerId: z.string().min(8).max(96),
  actorSeatIndex: z.number().int().min(0).max(1),
  sentAt: z.string().datetime(),
  signerPubkeyHex: z.string().regex(/^[0-9a-f]+$/i),
  signatureHex: z.string().regex(/^[0-9a-f]+$/i),
  payload: signedActionPayloadSchema,
});
export type SignedGameAction = z.infer<typeof signedGameActionSchema>;

export const checkpointSignatureSchema = z.object({
  playerId: z.string().min(8).max(96),
  pubkeyHex: z.string().regex(/^[0-9a-f]+$/i),
  signatureHex: z.string().regex(/^[0-9a-f]+$/i),
  signedAt: z.string().datetime(),
});
export type CheckpointSignature = z.infer<typeof checkpointSignatureSchema>;

export const tableCheckpointSchema = z.object({
  checkpointId: z.string().uuid(),
  tableId: z.string().uuid(),
  handId: z.string().uuid(),
  handNumber: z.number().int().positive(),
  phase: streetSchema,
  actingSeatIndex: z.number().int().min(0).max(1).nullable(),
  dealerSeatIndex: z.number().int().min(0).max(1),
  board: z.array(cardCodeSchema).max(5),
  holeCardsByPlayerId: z.record(z.string(), z.array(concealedCardCodeSchema).length(2)),
  playerStacks: z.record(z.string(), z.number().int().nonnegative()),
  roundContributions: z.record(z.string(), z.number().int().nonnegative()),
  totalContributions: z.record(z.string(), z.number().int().nonnegative()),
  potSats: z.number().int().nonnegative(),
  currentBetSats: z.number().int().nonnegative(),
  minRaiseToSats: z.number().int().nonnegative(),
  deckSeedHash: z.string().regex(/^[0-9a-f]{64}$/i),
  commitmentHashes: z.array(z.string().regex(/^[0-9a-f]{64}$/i)).length(2),
  nextActionDeadline: z.string().datetime().nullable(),
  transcriptHash: z.string().regex(/^[0-9a-f]{64}$/i),
  signatures: z.array(checkpointSignatureSchema),
});
export type TableCheckpoint = z.infer<typeof tableCheckpointSchema>;

export const timeoutDelegationSchema = z.object({
  delegationId: z.string().uuid(),
  tableId: z.string().uuid(),
  checkpointId: z.string().uuid(),
  actingSeatIndex: z.number().int().min(0).max(1),
  delegatedPlayerId: z.string().min(8).max(96),
  delegatedAction: z.enum(["timeout-fold", "checkpoint-renewal"]),
  validAfter: z.string().datetime(),
  expiresAt: z.string().datetime(),
  settlementAddress: z.string().min(8),
  signatureHex: z.string().regex(/^[0-9a-f]+$/i),
});
export type TimeoutDelegation = z.infer<typeof timeoutDelegationSchema>;

export const escrowStateSchema = z.object({
  escrowId: z.string().uuid(),
  tableId: z.string().uuid(),
  network: networkSchema,
  contractAddress: z.string().min(8),
  totalLockedSats: z.number().int().nonnegative(),
  participantPubkeys: z.array(z.string().regex(/^[0-9a-f]+$/i)).min(2).max(3),
  watchtowerPubkey: z.string().regex(/^[0-9a-f]+$/i),
  refundDelayBlocks: z.number().int().positive(),
  cooperativePath: z.string(),
  renewalPath: z.string(),
  refundPath: z.string(),
  currentCheckpointId: z.string().uuid().optional(),
  status: z.enum(["pending", "funded", "renewing", "settling", "refunded", "closed"]),
});
export type EscrowState = z.infer<typeof escrowStateSchema>;

export const settlementInstructionSchema = z.object({
  instructionId: z.string().uuid(),
  tableId: z.string().uuid(),
  checkpointId: z.string().uuid().optional(),
  kind: z.enum([
    "open-table-escrow",
    "sync-checkpoint",
    "settle-winner",
    "split-pot",
    "refund-table",
  ]),
  outputs: z.array(
    z.object({
      playerId: z.string().min(8).max(96),
      address: z.string().min(8),
      amountSats: z.number().int().nonnegative(),
    }),
  ),
  createdAt: z.string().datetime(),
});
export type SettlementInstruction = z.infer<typeof settlementInstructionSchema>;

export const swapQuoteSchema = z.object({
  quoteId: z.string().uuid(),
  direction: z.enum(["deposit", "withdrawal"]),
  amountSats: z.number().int().positive(),
  feeSats: z.number().int().nonnegative(),
  invoice: z.string().optional(),
  paymentHash: z.string().optional(),
  lightningInvoice: z.string().optional(),
  expiresAt: z.string().datetime(),
});
export type SwapQuote = z.infer<typeof swapQuoteSchema>;

export const swapJobStatusSchema = z.object({
  swapId: z.string(),
  direction: z.enum(["deposit", "withdrawal"]),
  status: z.enum(["pending", "funded", "completed", "refunded", "failed"]),
  createdAt: z.string().datetime(),
  updatedAt: z.string().datetime(),
  details: z.string().optional(),
});
export type SwapJobStatus = z.infer<typeof swapJobStatusSchema>;

export const tableConfigSchema = z.object({
  tableId: z.string().uuid(),
  inviteCode: z.string().min(6).max(32),
  hostPlayerId: z.string().min(8).max(96),
  network: networkSchema.default("mutinynet"),
  variant: gameVariantSchema.default("holdem-heads-up"),
  smallBlindSats: z.number().int().positive(),
  bigBlindSats: z.number().int().positive(),
  buyInMinSats: z.number().int().positive(),
  buyInMaxSats: z.number().int().positive(),
  commitmentDeadlineSeconds: z.number().int().positive(),
  actionTimeoutSeconds: z.number().int().positive(),
  createdAt: z.string().datetime(),
  status: tableStatusSchema,
});
export type TableConfig = z.infer<typeof tableConfigSchema>;

export const tableSnapshotSchema = z.object({
  table: tableConfigSchema,
  seats: z.array(seatReservationSchema).length(2),
  commitments: z.array(deckCommitmentSchema),
  escrow: escrowStateSchema.optional(),
  checkpoint: tableCheckpointSchema.optional(),
  pendingDelegations: z.array(timeoutDelegationSchema),
});
export type TableSnapshot = z.infer<typeof tableSnapshotSchema>;

export const createTableRequestSchema = z.object({
  host: playerProfileSchema,
  smallBlindSats: z.number().int().positive().default(50),
  bigBlindSats: z.number().int().positive().default(100),
  buyInMinSats: z.number().int().positive().default(2000),
  buyInMaxSats: z.number().int().positive().default(10000),
  actionTimeoutSeconds: z.number().int().positive().default(25),
  commitmentDeadlineSeconds: z.number().int().positive().default(20),
});
export type CreateTableRequest = z.infer<typeof createTableRequestSchema>;

export const createTableResponseSchema = z.object({
  table: tableConfigSchema,
  seatReservation: seatReservationSchema,
  websocketUrl: z.string().url(),
});
export type CreateTableResponse = z.infer<typeof createTableResponseSchema>;

export const joinTableRequestSchema = z.object({
  inviteCode: z.string().min(6).max(32),
  player: playerProfileSchema,
  buyInSats: z.number().int().positive(),
});
export type JoinTableRequest = z.infer<typeof joinTableRequestSchema>;

export const joinTableResponseSchema = z.object({
  table: tableConfigSchema,
  seatReservation: seatReservationSchema,
});
export type JoinTableResponse = z.infer<typeof joinTableResponseSchema>;

export const signalPayloadSchema = z.object({
  description: z.string().optional(),
  candidate: z.string().optional(),
  sdpMid: z.string().optional(),
  sdpMLineIndex: z.number().int().optional(),
});
export type SignalPayload = z.infer<typeof signalPayloadSchema>;

export const clientSocketEventSchema = z.discriminatedUnion("type", [
  z.object({
    type: z.literal("identify"),
    tableId: z.string().uuid(),
    playerId: z.string().min(8).max(96),
  }),
  z.object({
    type: z.literal("signal"),
    tableId: z.string().uuid(),
    fromPlayerId: z.string().min(8).max(96),
    targetPlayerId: z.string().min(8).max(96),
    payload: signalPayloadSchema,
  }),
  z.object({
    type: z.literal("signed-action"),
    action: signedGameActionSchema,
  }),
  z.object({
    type: z.literal("checkpoint"),
    checkpoint: tableCheckpointSchema,
  }),
  z.object({
    type: z.literal("heartbeat"),
    tableId: z.string().uuid(),
    playerId: z.string().min(8).max(96),
    sentAt: z.string().datetime(),
  }),
]);
export type ClientSocketEvent = z.infer<typeof clientSocketEventSchema>;

export const serverSocketEventSchema = z.discriminatedUnion("type", [
  z.object({
    type: z.literal("table-snapshot"),
    snapshot: tableSnapshotSchema,
  }),
  z.object({
    type: z.literal("signal"),
    tableId: z.string().uuid(),
    fromPlayerId: z.string().min(8).max(96),
    payload: signalPayloadSchema,
  }),
  z.object({
    type: z.literal("presence"),
    tableId: z.string().uuid(),
    playerId: z.string().min(8).max(96),
    status: z.enum(["online", "offline"]),
  }),
  z.object({
    type: z.literal("checkpoint"),
    checkpoint: tableCheckpointSchema,
  }),
  z.object({
    type: z.literal("error"),
    code: z.string(),
    message: z.string(),
  }),
]);
export type ServerSocketEvent = z.infer<typeof serverSocketEventSchema>;

export function parseClientSocketEvent(input: unknown): ClientSocketEvent {
  return clientSocketEventSchema.parse(input);
}

export function parseServerSocketEvent(input: unknown): ServerSocketEvent {
  return serverSocketEventSchema.parse(input);
}
