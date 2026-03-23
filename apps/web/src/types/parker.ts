// Web-local Parker types kept in sync with the Go controller, daemon, and indexer contracts.

export type MeshRuntimeMode = "player" | "host" | "witness" | "indexer";

export interface WalletSummary {
  availableSats: number;
  totalSats: number;
  arkAddress: string;
  boardingAddress: string;
}

export interface LocalProfileSummary {
  currentMeshTableId?: string;
  currentTableId?: string;
  hasPeerIdentity: boolean;
  hasProtocolIdentity: boolean;
  hasWalletIdentity: boolean;
  knownPeerCount: number;
  meshTableCount: number;
  nickname: string;
  profileName: string;
}

export interface ProfileDaemonMetadata {
  lastHeartbeat: string;
  logPath: string;
  mode?: MeshRuntimeMode;
  peerId?: string;
  peerUrl?: string;
  pid: number;
  profile: string;
  protocolId?: string;
  socketPath: string;
  startedAt: string;
  status: "running" | "starting" | "stopping";
}

export interface PeerAddress {
  alias?: string;
  lastSeenAt?: string;
  peerId: string;
  peerUrl: string;
  protocolPubkeyHex?: string;
  relayPeerId?: string;
  roles: string[];
}

export interface SignedTableAdvertisement {
  adExpiresAt: string;
  buyInMaxSats: number;
  buyInMinSats: number;
  currency: string;
  geographicHint?: string;
  hostModeCapabilities: string[];
  hostPeerId: string;
  hostPeerUrl?: string;
  hostProtocolPubkeyHex: string;
  hostSignatureHex: string;
  latencyHintMs?: number;
  networkId: string;
  occupiedSeats: number;
  protocolVersion: string;
  seatCount: number;
  spectatorsAllowed: boolean;
  stakes: {
    bigBlindSats: number;
    smallBlindSats: number;
  };
  tableId: string;
  tableName: string;
  visibility: string;
  witnessCount: number;
}

export interface TableSummary {
  currentEpoch: number;
  handNumber: number;
  hostPeerId: string;
  latestSnapshotId?: string;
  phase: string | null;
  role: string;
  status: string;
  tableId: string;
  tableName: string;
  visibility: string;
}

export interface MeshRuntimeState {
  fundsWarnings: Array<Record<string, unknown>>;
  mode: string;
  peer: {
    peerId?: string;
    peerUrl?: string;
    protocolId?: string;
    walletPlayerId?: string;
  };
  peers: PeerAddress[];
  publicTables: SignedTableAdvertisement[];
  tables: TableSummary[];
}

export interface LegalAction {
  type: "fold" | "check" | "call" | "bet" | "raise";
  minTotalSats?: number | null;
  maxTotalSats?: number | null;
}

export interface MeshTableConfig {
  bigBlindSats: number;
  buyInMaxSats: number;
  buyInMinSats: number;
  createdAt: string;
  dealerMode: string;
  hostPeerId: string;
  hostPlaysAllowed: boolean;
  name: string;
  networkId: string;
  occupiedSeats: number;
  publicSpectatorDelayHands: number;
  seatCount: number;
  smallBlindSats: number;
  spectatorsAllowed: boolean;
  status: string;
  tableId: string;
  visibility: string;
}

export interface SeatedPlayer {
  arkAddress: string;
  buyInSats: number;
  nickname: string;
  peerId: string;
  playerId: string;
  protocolPubkeyHex: string;
  seatIndex: number;
  status: string;
  walletPubkeyHex: string;
}

export interface DealerCommitment {
  committedAt: string;
  mode: string;
  rootHash: string;
}

export interface PublicTableState {
  actingSeatIndex: number | null;
  board: string[];
  chipBalances: Record<string, number>;
  currentBetSats: number;
  dealerCommitment: DealerCommitment | null;
  dealerSeatIndex: number | null;
  epoch: number;
  foldedPlayerIds: string[];
  handId: string | null;
  handNumber: number;
  latestEventHash: string | null;
  livePlayerIds: string[];
  minRaiseToSats: number;
  phase: string | null;
  potSats: number;
  previousSnapshotHash: string | null;
  roundContributions: Record<string, number>;
  seatedPlayers: SeatedPlayer[];
  snapshotId: string;
  status: string;
  tableId: string;
  totalContributions: Record<string, number>;
  updatedAt: string;
}

export interface TableSnapshotSignature {
  signatureHex: string;
  signedAt: string;
  signerPeerId: string;
  signerPubkeyHex: string;
  signerRole: string;
}

export interface CooperativeTableSnapshot {
  chipBalances: Record<string, number>;
  createdAt: string;
  dealerCommitmentRoot: string | null;
  epoch: number;
  foldedPlayerIds: string[];
  handId: string | null;
  handNumber: number;
  latestEventHash: string | null;
  livePlayerIds: string[];
  phase: string | null;
  potSats: number;
  previousSnapshotHash: string | null;
  seatedPlayers: SeatedPlayer[];
  sidePots: number[];
  signatures: TableSnapshotSignature[];
  snapshotId: string;
  tableId: string;
  turnIndex: number | null;
}

export interface SignedTableEvent {
  body: Record<string, unknown>;
  epoch: number;
  handId: string | null;
  messageType: string;
  networkId: string;
  prevEventHash: string | null;
  protocolVersion: string;
  seq: number;
  senderPeerId: string;
  senderProtocolPubkeyHex: string;
  senderRole: string;
  signature: string;
  tableId: string;
  timestamp: string;
}

export interface MeshTableLocalView {
  canAct: boolean;
  legalActions: LegalAction[];
  myHoleCards: [string, string] | null;
  myPlayerId: string | null;
  mySeatIndex: number | null;
}

export interface MeshTableView {
  config: MeshTableConfig;
  events: SignedTableEvent[];
  latestFullySignedSnapshot: CooperativeTableSnapshot | null;
  latestSnapshot: CooperativeTableSnapshot | null;
  local: MeshTableLocalView;
  publicState: PublicTableState | null;
}

export interface DaemonRuntimeState {
  mesh?: MeshRuntimeState;
  wallet?: WalletSummary;
}

export interface ProfileDaemonStatus {
  metadata: ProfileDaemonMetadata | null;
  reachable: boolean;
  state: DaemonRuntimeState | null;
}

export interface PublicTableUpdate {
  advertisement?: SignedTableAdvertisement;
  publishedAt?: string;
  publicState?: PublicTableState;
  tableId: string;
  type: string;
}

export interface PublicTableView {
  advertisement: SignedTableAdvertisement;
  latestState: PublicTableState | null;
  recentUpdates: PublicTableUpdate[];
}
