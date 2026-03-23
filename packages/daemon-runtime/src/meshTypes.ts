import type {
  CardCode,
  HandAuditBundle,
  MeshRole,
  MeshTableConfig,
  PeerAddress,
  PublicTableState,
  SignedTableAdvertisement,
  SignedTableEvent,
  CooperativeTableSnapshot,
  TableFundsOperation,
  MeshSeatedPlayer,
} from "@parker/protocol";
import type { HoldemState, LegalAction } from "@parker/game-engine";

export type MeshRuntimeMode = MeshRole;

export interface TableSummary {
  currentEpoch: number;
  handNumber: number;
  hostPeerId: string;
  latestSnapshotId?: string;
  phase: PublicTableState["phase"];
  role: "host" | "player" | "witness";
  status: MeshTableConfig["status"];
  tableId: string;
  tableName: string;
  visibility: MeshTableConfig["visibility"];
}

export interface FundsWarning {
  expiresAt: string;
  playerId: string;
  severity: "warning" | "critical";
  tableId: string;
}

export interface MeshRuntimeState {
  fundsWarnings: FundsWarning[];
  mode: MeshRuntimeMode;
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

export interface LocalPrivateTableState {
  activeHand?: HoldemState;
  auditBundlesByHandId: Record<string, HandAuditBundle>;
  myHoleCardsByHandId: Record<string, [CardCode, CardCode]>;
}

export interface MeshTableLocalView {
  canAct: boolean;
  legalActions: LegalAction[];
  myHoleCards: [CardCode, CardCode] | null;
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

export interface MeshTableContext {
  advertisement?: SignedTableAdvertisement;
  config: MeshTableConfig;
  currentEpoch: number;
  currentHostPeerId: string;
  currentHostPeerUrl: string;
  events: SignedTableEvent[];
  handSetupInFlight: boolean;
  pendingEvents: Map<string, SignedTableEvent>;
  buyInReceipts: Map<string, TableFundsOperation>;
  latestFullySignedSnapshot: CooperativeTableSnapshot | null;
  latestSnapshot: CooperativeTableSnapshot | null;
  lastEventHash: string | null;
  lastHostHeartbeatAt: number;
  pendingPlayers: Map<string, MeshSeatedPlayer>;
  peerAddresses: Map<string, PeerAddress>;
  readyTransitionInFlight: boolean;
  privateState: LocalPrivateTableState;
  publicState: PublicTableState | null;
  role: "host" | "player" | "witness";
  settlementSnapshotEventHashInFlight: string | null;
  snapshots: CooperativeTableSnapshot[];
  witnessSet: string[];
}
