import { z } from "zod";

export const networkSchema = z.enum(["mutinynet", "regtest"]);
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

export const clientSocketEventSchema = z.discriminatedUnion("type", [
  z.object({
    type: z.literal("identify"),
    tableId: z.string().uuid(),
    playerId: z.string().min(8).max(96),
  }),
  z.object({
    type: z.literal("peer-message"),
    tableId: z.string().uuid(),
    fromPlayerId: z.string().min(8).max(96),
    targetPlayerId: z.string().min(8).max(96),
    message: z.string().min(1),
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
    type: z.literal("peer-message"),
    tableId: z.string().uuid(),
    fromPlayerId: z.string().min(8).max(96),
    message: z.string().min(1),
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

const hashHexSchema = z.string().regex(/^[0-9a-f]{64}$/i);
const hexSignatureSchema = z.string().regex(/^[0-9a-f]+$/i);
const meshSeatIndexSchema = z.number().int().min(0).max(5);

export const meshRoleSchema = z.enum(["player", "host", "witness", "indexer"]);
export type MeshRole = z.infer<typeof meshRoleSchema>;

export const meshTableVisibilitySchema = z.enum(["private", "public"]);
export type MeshTableVisibility = z.infer<typeof meshTableVisibilitySchema>;

export const hostDealerModeSchema = z.enum([
  "host-dealer-v1",
  "bias-resistant-host-dealer",
  "mental-poker",
]);
export type HostDealerMode = z.infer<typeof hostDealerModeSchema>;

export const tableLifecycleStatusSchema = z.enum([
  "announced",
  "seating",
  "ready",
  "active",
  "aborted",
  "closed",
]);
export type TableLifecycleStatus = z.infer<typeof tableLifecycleStatusSchema>;

export const meshPlayerActionPayloadSchema = z.discriminatedUnion("type", [
  z.object({ type: z.literal("fold") }),
  z.object({ type: z.literal("check") }),
  z.object({ type: z.literal("call") }),
  z.object({
    type: z.literal("bet"),
    totalSats: z.number().int().positive(),
  }),
  z.object({
    type: z.literal("raise"),
    totalSats: z.number().int().positive(),
  }),
]);
export type MeshPlayerActionPayload = z.infer<typeof meshPlayerActionPayloadSchema>;

export const peerAddressSchema = z.object({
  peerId: z.string().min(8).max(128),
  peerUrl: z.string().url(),
  alias: z.string().min(1).max(64).optional(),
  protocolPubkeyHex: z.string().regex(/^[0-9a-f]+$/i).optional(),
  relayPeerId: z.string().min(8).max(128).optional(),
  roles: z.array(meshRoleSchema).default([]),
  lastSeenAt: z.string().datetime().optional(),
});
export type PeerAddress = z.infer<typeof peerAddressSchema>;

export const identityBindingSchema = z.object({
  tableId: z.string().uuid(),
  peerId: z.string().min(8).max(128),
  protocolId: z.string().min(8).max(128),
  protocolPubkeyHex: z.string().regex(/^[0-9a-f]+$/i),
  walletPlayerId: z.string().min(8).max(128),
  walletPubkeyHex: z.string().regex(/^[0-9a-f]+$/i),
  signedAt: z.string().datetime(),
  signatureHex: hexSignatureSchema,
});
export type IdentityBinding = z.infer<typeof identityBindingSchema>;

export const hostLeaseSignatureSchema = z.object({
  signerPeerId: z.string().min(8).max(128),
  signerRole: meshRoleSchema,
  signerPubkeyHex: z.string().regex(/^[0-9a-f]+$/i),
  signatureHex: hexSignatureSchema,
  signedAt: z.string().datetime(),
});
export type HostLeaseSignature = z.infer<typeof hostLeaseSignatureSchema>;

export const hostLeaseSchema = z.object({
  tableId: z.string().uuid(),
  epoch: z.number().int().nonnegative(),
  hostPeerId: z.string().min(8).max(128),
  witnessSet: z.array(z.string().min(8).max(128)),
  leaseStart: z.string().datetime(),
  leaseExpiry: z.string().datetime(),
  signatures: z.array(hostLeaseSignatureSchema),
});
export type HostLease = z.infer<typeof hostLeaseSchema>;

export const meshTableConfigSchema = z.object({
  tableId: z.string().uuid(),
  name: z.string().min(1).max(96),
  networkId: z.string().min(1).max(64),
  visibility: meshTableVisibilitySchema,
  status: tableLifecycleStatusSchema,
  dealerMode: hostDealerModeSchema,
  hostPeerId: z.string().min(8).max(128),
  smallBlindSats: z.number().int().positive(),
  bigBlindSats: z.number().int().positive(),
  buyInMinSats: z.number().int().positive(),
  buyInMaxSats: z.number().int().positive(),
  seatCount: z.number().int().min(2).max(6),
  occupiedSeats: z.number().int().min(0).max(6),
  spectatorsAllowed: z.boolean(),
  hostPlaysAllowed: z.boolean(),
  publicSpectatorDelayHands: z.number().int().nonnegative().default(1),
  createdAt: z.string().datetime(),
});
export type MeshTableConfig = z.infer<typeof meshTableConfigSchema>;

export const meshSeatedPlayerSchema = z.object({
  playerId: z.string().min(8).max(128),
  peerId: z.string().min(8).max(128),
  nickname: z.string().min(1).max(64),
  seatIndex: meshSeatIndexSchema,
  status: z.enum(["waiting", "active", "folded", "all-in", "left"]),
  protocolPubkeyHex: z.string().regex(/^[0-9a-f]+$/i),
  walletPubkeyHex: z.string().regex(/^[0-9a-f]+$/i),
  arkAddress: z.string().min(8),
  buyInSats: z.number().int().nonnegative(),
});
export type MeshSeatedPlayer = z.infer<typeof meshSeatedPlayerSchema>;

export const dealerCommitmentSchema = z.object({
  mode: hostDealerModeSchema,
  rootHash: hashHexSchema,
  committedAt: z.string().datetime(),
});
export type DealerCommitment = z.infer<typeof dealerCommitmentSchema>;

export const publicTableStateSchema = z.object({
  snapshotId: z.string().uuid(),
  tableId: z.string().uuid(),
  epoch: z.number().int().nonnegative(),
  status: tableLifecycleStatusSchema,
  handId: z.string().uuid().nullable(),
  handNumber: z.number().int().nonnegative(),
  phase: streetSchema.nullable(),
  actingSeatIndex: meshSeatIndexSchema.nullable(),
  dealerSeatIndex: meshSeatIndexSchema.nullable(),
  board: z.array(cardCodeSchema).max(5),
  seatedPlayers: z.array(meshSeatedPlayerSchema),
  chipBalances: z.record(z.string(), z.number().int().nonnegative()),
  roundContributions: z.record(z.string(), z.number().int().nonnegative()),
  totalContributions: z.record(z.string(), z.number().int().nonnegative()),
  potSats: z.number().int().nonnegative(),
  currentBetSats: z.number().int().nonnegative(),
  minRaiseToSats: z.number().int().nonnegative(),
  livePlayerIds: z.array(z.string().min(8).max(128)),
  foldedPlayerIds: z.array(z.string().min(8).max(128)),
  dealerCommitment: dealerCommitmentSchema.nullable(),
  previousSnapshotHash: hashHexSchema.nullable(),
  latestEventHash: hashHexSchema.nullable(),
  updatedAt: z.string().datetime(),
});
export type PublicTableState = z.infer<typeof publicTableStateSchema>;

export const tableSnapshotSignatureSchema = z.object({
  signerPeerId: z.string().min(8).max(128),
  signerRole: meshRoleSchema,
  signerPubkeyHex: z.string().regex(/^[0-9a-f]+$/i),
  signatureHex: hexSignatureSchema,
  signedAt: z.string().datetime(),
});
export type TableSnapshotSignature = z.infer<typeof tableSnapshotSignatureSchema>;

export const cooperativeTableSnapshotSchema = z.object({
  snapshotId: z.string().uuid(),
  tableId: z.string().uuid(),
  epoch: z.number().int().nonnegative(),
  handId: z.string().uuid().nullable(),
  handNumber: z.number().int().nonnegative(),
  phase: streetSchema.nullable(),
  seatedPlayers: z.array(meshSeatedPlayerSchema),
  chipBalances: z.record(z.string(), z.number().int().nonnegative()),
  potSats: z.number().int().nonnegative(),
  sidePots: z.array(z.number().int().nonnegative()).default([]),
  turnIndex: meshSeatIndexSchema.nullable(),
  livePlayerIds: z.array(z.string().min(8).max(128)),
  foldedPlayerIds: z.array(z.string().min(8).max(128)),
  dealerCommitmentRoot: hashHexSchema.nullable(),
  previousSnapshotHash: hashHexSchema.nullable(),
  latestEventHash: hashHexSchema.nullable(),
  createdAt: z.string().datetime(),
  signatures: z.array(tableSnapshotSignatureSchema),
});
export type CooperativeTableSnapshot = z.infer<typeof cooperativeTableSnapshotSchema>;

export const tableFundsOperationSchema = z.object({
  operationId: z.string().uuid(),
  kind: z.enum([
    "buy-in-prepared",
    "buy-in-locked",
    "checkpoint-recorded",
    "cashout",
    "close-table",
    "renewal",
    "emergency-exit",
  ]),
  provider: z.string().min(1).max(64),
  tableId: z.string().uuid(),
  playerId: z.string().min(8).max(128),
  networkId: z.string().min(1).max(64),
  amountSats: z.number().int().nonnegative(),
  checkpointHash: hashHexSchema.optional(),
  vtxoExpiry: z.string().datetime().optional(),
  createdAt: z.string().datetime(),
  status: z.enum(["prepared", "locked", "recorded", "completed", "renewed", "exited"]),
  signerPubkeyHex: z.string().regex(/^[0-9a-f]+$/i),
  signatureHex: hexSignatureSchema,
});
export type TableFundsOperation = z.infer<typeof tableFundsOperationSchema>;

export const privateInviteSchema = z.object({
  protocolVersion: z.string().min(1).max(32),
  networkId: z.string().min(1).max(64),
  tableId: z.string().uuid(),
  hostPeerId: z.string().min(8).max(128),
  hostPeerUrl: z.string().url(),
  relayPeerId: z.string().min(8).max(128).optional(),
});
export type PrivateInvite = z.infer<typeof privateInviteSchema>;

export const signedTableAdvertisementSchema = z.object({
  protocolVersion: z.string().min(1).max(32),
  networkId: z.string().min(1).max(64),
  tableId: z.string().uuid(),
  hostPeerId: z.string().min(8).max(128),
  hostPeerUrl: z.string().url().optional(),
  tableName: z.string().min(1).max(96),
  stakes: z.object({
    smallBlindSats: z.number().int().positive(),
    bigBlindSats: z.number().int().positive(),
  }),
  currency: z.string().min(1).max(32),
  seatCount: z.number().int().min(2).max(6),
  occupiedSeats: z.number().int().min(0).max(6),
  spectatorsAllowed: z.boolean(),
  hostModeCapabilities: z.array(hostDealerModeSchema),
  witnessCount: z.number().int().nonnegative(),
  buyInMinSats: z.number().int().positive(),
  buyInMaxSats: z.number().int().positive(),
  visibility: meshTableVisibilitySchema,
  geographicHint: z.string().max(64).optional(),
  latencyHintMs: z.number().int().nonnegative().optional(),
  adExpiresAt: z.string().datetime(),
  hostProtocolPubkeyHex: z.string().regex(/^[0-9a-f]+$/i),
  hostSignatureHex: hexSignatureSchema,
});
export type SignedTableAdvertisement = z.infer<typeof signedTableAdvertisementSchema>;

export const playerJoinIntentSchema = z.object({
  tableId: z.string().uuid(),
  player: playerProfileSchema,
  peerId: z.string().min(8).max(128),
  peerUrl: z.string().url(),
  protocolId: z.string().min(8).max(128),
  protocolPubkeyHex: z.string().regex(/^[0-9a-f]+$/i),
  requestedSeatIndex: meshSeatIndexSchema.optional(),
  requestedAt: z.string().datetime(),
  identityBinding: identityBindingSchema,
  signatureHex: hexSignatureSchema,
});
export type PlayerJoinIntent = z.infer<typeof playerJoinIntentSchema>;

export const buyInConfirmSchema = z.object({
  tableId: z.string().uuid(),
  playerId: z.string().min(8).max(128),
  confirmedAt: z.string().datetime(),
  receipt: tableFundsOperationSchema,
  signatureHex: hexSignatureSchema,
});
export type BuyInConfirm = z.infer<typeof buyInConfirmSchema>;

export const playerActionIntentSchema = z.object({
  tableId: z.string().uuid(),
  handId: z.string().uuid(),
  epoch: z.number().int().nonnegative(),
  playerId: z.string().min(8).max(128),
  seatIndex: meshSeatIndexSchema,
  protocolPubkeyHex: z.string().regex(/^[0-9a-f]+$/i),
  requestedAt: z.string().datetime(),
  action: meshPlayerActionPayloadSchema,
  signatureHex: hexSignatureSchema,
});
export type PlayerActionIntent = z.infer<typeof playerActionIntentSchema>;

export const privateCardEnvelopeSchema = z.object({
  nonceHex: z.string().regex(/^[0-9a-f]+$/i),
  ciphertextHex: z.string().regex(/^[0-9a-f]+$/i),
  authTagHex: z.string().regex(/^[0-9a-f]+$/i),
});
export type PrivateCardEnvelope = z.infer<typeof privateCardEnvelopeSchema>;

export const handAuditBundleSchema = z.object({
  commitmentRoot: hashHexSchema,
  deckSeedHex: z.string().regex(/^[0-9a-f]{64}$/i),
  deckOrder: z.array(cardCodeSchema).length(52),
  revealedAt: z.string().datetime(),
});
export type HandAuditBundle = z.infer<typeof handAuditBundleSchema>;

export const handResultSchema = z.object({
  playerId: z.string().min(8).max(128),
  seatIndex: meshSeatIndexSchema,
  amountSats: z.number().int().nonnegative(),
});
export type HandResult = z.infer<typeof handResultSchema>;

export const hostFailoverAcceptanceSchema = z.object({
  tableId: z.string().uuid(),
  currentEpoch: z.number().int().nonnegative(),
  proposedEpoch: z.number().int().nonnegative(),
  proposedHostPeerId: z.string().min(8).max(128),
  signerPeerId: z.string().min(8).max(128),
  signerPubkeyHex: z.string().regex(/^[0-9a-f]+$/i),
  signedAt: z.string().datetime(),
  signatureHex: hexSignatureSchema,
});
export type HostFailoverAcceptance = z.infer<typeof hostFailoverAcceptanceSchema>;

export const publicTableUpdateSchema = z.discriminatedUnion("type", [
  z.object({
    type: z.literal("PublicTableSnapshot"),
    tableId: z.string().uuid(),
    advertisement: signedTableAdvertisementSchema,
    publicState: publicTableStateSchema.nullable(),
    publishedAt: z.string().datetime(),
  }),
  z.object({
    type: z.literal("PublicHandUpdate"),
    tableId: z.string().uuid(),
    handId: z.string().uuid(),
    handNumber: z.number().int().positive(),
    phase: streetSchema,
    publicState: publicTableStateSchema,
    publishedAt: z.string().datetime(),
  }),
  z.object({
    type: z.literal("PublicShowdownReveal"),
    tableId: z.string().uuid(),
    handId: z.string().uuid(),
    handNumber: z.number().int().positive(),
    holeCardsByPlayerId: z.record(z.string(), z.array(cardCodeSchema).length(2)),
    board: z.array(cardCodeSchema).max(5),
    publishedAt: z.string().datetime(),
  }),
]);
export type PublicTableUpdate = z.infer<typeof publicTableUpdateSchema>;

export const tableEventBodySchema = z.discriminatedUnion("type", [
  z.object({
    type: z.literal("TableAnnounce"),
    table: meshTableConfigSchema,
    advertisement: signedTableAdvertisementSchema.optional(),
  }),
  z.object({
    type: z.literal("TableWithdraw"),
    reason: z.string().min(1).max(256),
  }),
  z.object({
    type: z.literal("JoinRequest"),
    intent: playerJoinIntentSchema,
  }),
  z.object({
    type: z.literal("JoinAccepted"),
    intent: playerJoinIntentSchema,
    seatIndex: meshSeatIndexSchema,
  }),
  z.object({
    type: z.literal("JoinRejected"),
    intent: playerJoinIntentSchema,
    reason: z.string().min(1).max(256),
  }),
  z.object({
    type: z.literal("SeatProposal"),
    playerId: z.string().min(8).max(128),
    peerId: z.string().min(8).max(128),
    seatIndex: meshSeatIndexSchema,
  }),
  z.object({
    type: z.literal("SeatLocked"),
    playerId: z.string().min(8).max(128),
    peerId: z.string().min(8).max(128),
    seatIndex: meshSeatIndexSchema,
    buyInSats: z.number().int().positive(),
  }),
  z.object({
    type: z.literal("BuyInRequested"),
    intent: playerJoinIntentSchema,
    amountSats: z.number().int().positive(),
  }),
  z.object({
    type: z.literal("BuyInLocked"),
    receipt: tableFundsOperationSchema,
  }),
  z.object({
    type: z.literal("TableReady"),
    balances: z.record(z.string(), z.number().int().nonnegative()),
    publicState: publicTableStateSchema.nullable(),
  }),
  z.object({
    type: z.literal("TableClosed"),
    reason: z.string().min(1).max(256),
    balances: z.record(z.string(), z.number().int().nonnegative()),
  }),
  z.object({
    type: z.literal("HandStart"),
    handId: z.string().uuid(),
    handNumber: z.number().int().positive(),
    dealerSeatIndex: meshSeatIndexSchema,
    publicState: publicTableStateSchema,
  }),
  z.object({
    type: z.literal("DealerCommit"),
    handId: z.string().uuid(),
    commitment: dealerCommitmentSchema,
  }),
  z.object({
    type: z.literal("PrivateCardDelivery"),
    handId: z.string().uuid(),
    recipientPlayerId: z.string().min(8).max(128),
    recipientPeerId: z.string().min(8).max(128),
    encryptedPayload: privateCardEnvelopeSchema,
    proofHash: hashHexSchema,
  }),
  z.object({
    type: z.literal("StreetStart"),
    handId: z.string().uuid(),
    street: streetSchema,
    publicState: publicTableStateSchema,
  }),
  z.object({
    type: z.literal("PlayerAction"),
    intent: playerActionIntentSchema,
  }),
  z.object({
    type: z.literal("ActionAccepted"),
    intent: playerActionIntentSchema,
    publicState: publicTableStateSchema,
  }),
  z.object({
    type: z.literal("ActionRejected"),
    intent: playerActionIntentSchema,
    reason: z.string().min(1).max(256),
  }),
  z.object({
    type: z.literal("StreetClosed"),
    handId: z.string().uuid(),
    street: streetSchema,
    publicState: publicTableStateSchema,
  }),
  z.object({
    type: z.literal("ShowdownReveal"),
    handId: z.string().uuid(),
    holeCardsByPlayerId: z.record(z.string(), z.array(cardCodeSchema).length(2)),
    auditBundle: handAuditBundleSchema,
    publicState: publicTableStateSchema,
  }),
  z.object({
    type: z.literal("HandResult"),
    handId: z.string().uuid(),
    winners: z.array(handResultSchema),
    balances: z.record(z.string(), z.number().int().nonnegative()),
    publicState: publicTableStateSchema,
    checkpointHash: hashHexSchema.optional(),
  }),
  z.object({
    type: z.literal("HandAbort"),
    handId: z.string().uuid(),
    reason: z.string().min(1).max(256),
    rollbackSnapshotHash: hashHexSchema,
  }),
  z.object({
    type: z.literal("HostLeaseGranted"),
    lease: hostLeaseSchema,
  }),
  z.object({
    type: z.literal("HostHeartbeat"),
    observedAt: z.string().datetime(),
    lease: hostLeaseSchema,
  }),
  z.object({
    type: z.literal("WitnessSnapshot"),
    snapshot: cooperativeTableSnapshotSchema,
  }),
  z.object({
    type: z.literal("HostFailoverProposed"),
    previousHostPeerId: z.string().min(8).max(128),
    proposedHostPeerId: z.string().min(8).max(128),
    latestSnapshotHash: hashHexSchema,
    reason: z.string().min(1).max(256),
  }),
  z.object({
    type: z.literal("HostFailoverAccepted"),
    acceptance: hostFailoverAcceptanceSchema,
  }),
  z.object({
    type: z.literal("HostRotated"),
    previousHostPeerId: z.string().min(8).max(128),
    newHostPeerId: z.string().min(8).max(128),
    newEpoch: z.number().int().nonnegative(),
    lease: hostLeaseSchema,
  }),
  z.object({
    type: z.literal("PublicTableSnapshot"),
    update: publicTableUpdateSchema,
  }),
  z.object({
    type: z.literal("PublicHandUpdate"),
    update: publicTableUpdateSchema,
  }),
  z.object({
    type: z.literal("PublicShowdownReveal"),
    update: publicTableUpdateSchema,
  }),
]);
export type TableEventBody = z.infer<typeof tableEventBodySchema>;

export const signedTableEventSchema = z.object({
  protocolVersion: z.string().min(1).max(32),
  networkId: z.string().min(1).max(64),
  tableId: z.string().uuid(),
  handId: z.string().uuid().nullable(),
  epoch: z.number().int().nonnegative(),
  seq: z.number().int().nonnegative(),
  prevEventHash: hashHexSchema.nullable(),
  messageType: z.string().min(1).max(64),
  senderPeerId: z.string().min(8).max(128),
  senderRole: meshRoleSchema,
  senderProtocolPubkeyHex: z.string().regex(/^[0-9a-f]+$/i),
  timestamp: z.string().datetime(),
  body: tableEventBodySchema,
  signature: hexSignatureSchema,
});
export type SignedTableEvent = z.infer<typeof signedTableEventSchema>;

export const meshRequestBodySchema = z.discriminatedUnion("type", [
  z.object({
    type: z.literal("join-request"),
    intent: playerJoinIntentSchema,
    preparedBuyIn: tableFundsOperationSchema,
  }),
  z.object({
    type: z.literal("buy-in-confirm"),
    confirmation: buyInConfirmSchema,
  }),
  z.object({
    type: z.literal("action-request"),
    intent: playerActionIntentSchema,
  }),
  z.object({
    type: z.literal("snapshot-sign-request"),
    snapshot: cooperativeTableSnapshotSchema,
  }),
  z.object({
    type: z.literal("lease-sign-request"),
    lease: hostLeaseSchema,
  }),
  z.object({
    type: z.literal("failover-accept-request"),
    tableId: z.string().uuid(),
    currentEpoch: z.number().int().nonnegative(),
    proposedEpoch: z.number().int().nonnegative(),
    proposedHostPeerId: z.string().min(8).max(128),
  }),
  z.object({
    type: z.literal("peer-cache-request"),
  }),
  z.object({
    type: z.literal("public-table-list-request"),
  }),
]);
export type MeshRequestBody = z.infer<typeof meshRequestBodySchema>;

export const meshResponseBodySchema = z.discriminatedUnion("type", [
  z.object({
    type: z.literal("join-response"),
    accepted: z.boolean(),
    seatIndex: meshSeatIndexSchema.optional(),
    reason: z.string().min(1).max(256).optional(),
  }),
  z.object({
    type: z.literal("buy-in-response"),
    accepted: z.boolean(),
    reason: z.string().min(1).max(256).optional(),
  }),
  z.object({
    type: z.literal("action-response"),
    accepted: z.boolean(),
    reason: z.string().min(1).max(256).optional(),
  }),
  z.object({
    type: z.literal("snapshot-sign-response"),
    signature: tableSnapshotSignatureSchema,
  }),
  z.object({
    type: z.literal("lease-sign-response"),
    signature: hostLeaseSignatureSchema,
  }),
  z.object({
    type: z.literal("failover-accept-response"),
    acceptance: hostFailoverAcceptanceSchema,
  }),
  z.object({
    type: z.literal("peer-cache-response"),
    peers: z.array(peerAddressSchema),
  }),
  z.object({
    type: z.literal("public-table-list-response"),
    ads: z.array(signedTableAdvertisementSchema),
  }),
]);
export type MeshResponseBody = z.infer<typeof meshResponseBodySchema>;

export const meshWireFrameSchema = z.discriminatedUnion("kind", [
  z.object({
    kind: z.literal("hello"),
    peerId: z.string().min(8).max(128),
    peerPubkeyHex: z.string().regex(/^[0-9a-f]+$/i),
    protocolId: z.string().min(8).max(128),
    protocolPubkeyHex: z.string().regex(/^[0-9a-f]+$/i),
    alias: z.string().min(1).max(64),
    peerUrl: z.string().url(),
    roles: z.array(meshRoleSchema),
    sentAt: z.string().datetime(),
    signatureHex: hexSignatureSchema,
  }),
  z.object({
    kind: z.literal("request"),
    requestId: z.string().uuid(),
    body: meshRequestBodySchema,
  }),
  z.object({
    kind: z.literal("response"),
    requestId: z.string().uuid(),
    ok: z.boolean(),
    body: meshResponseBodySchema.optional(),
    error: z.string().optional(),
  }),
  z.object({
    kind: z.literal("event"),
    event: signedTableEventSchema,
  }),
  z.object({
    kind: z.literal("heartbeat"),
    tableId: z.string().uuid(),
    epoch: z.number().int().nonnegative(),
    hostPeerId: z.string().min(8).max(128),
    leaseExpiry: z.string().datetime(),
    sentAt: z.string().datetime(),
    signatureHex: hexSignatureSchema,
  }),
  z.object({
    kind: z.literal("public-ad"),
    ad: signedTableAdvertisementSchema,
  }),
  z.object({
    kind: z.literal("public-update"),
    update: publicTableUpdateSchema,
  }),
  z.object({
    kind: z.literal("relay-forward"),
    targetPeerId: z.string().min(8).max(128),
    frameJson: z.string().min(1),
  }),
]);
export type MeshWireFrame = z.infer<typeof meshWireFrameSchema>;

export const publicTableViewSchema = z.object({
  advertisement: signedTableAdvertisementSchema,
  latestState: publicTableStateSchema.nullable(),
  recentUpdates: z.array(publicTableUpdateSchema),
});
export type PublicTableView = z.infer<typeof publicTableViewSchema>;

export function parseMeshWireFrame(input: unknown): MeshWireFrame {
  return meshWireFrameSchema.parse(input);
}

export function parseSignedTableEvent(input: unknown): SignedTableEvent {
  return signedTableEventSchema.parse(input);
}
