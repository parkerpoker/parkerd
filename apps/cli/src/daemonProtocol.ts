import type { CreateTableRequest, MeshPlayerActionPayload, SignedActionPayload } from "@parker/protocol";

import type { LogEnvelope } from "./logger.js";
import type { MeshRuntimeState, MeshRuntimeMode } from "./meshTypes.js";
import type { PlayerRuntimeState } from "./playerClient.js";

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

export interface DaemonRuntimeState extends PlayerRuntimeState {
  mesh?: MeshRuntimeState;
}

export type DaemonMethod =
  | "bootstrap"
  | "connectCurrentTable"
  | "createTable"
  | "getSnapshot"
  | "getTranscript"
  | "joinTable"
  | "meshBootstrapPeer"
  | "meshCashOut"
  | "meshCreateTable"
  | "meshExit"
  | "meshGetTable"
  | "meshNetworkPeers"
  | "meshPublicTables"
  | "meshRenew"
  | "meshRotateHost"
  | "meshSendAction"
  | "meshTableAnnounce"
  | "meshTableJoin"
  | "ping"
  | "sendAction"
  | "sendPeerMessage"
  | "status"
  | "stop"
  | "watch"
  | "walletDeposit"
  | "walletFaucet"
  | "walletOffboard"
  | "walletOnboard"
  | "walletSummary"
  | "walletWithdraw"
  | "commitSeed";

export interface DaemonRequestEnvelope {
  id: string;
  method: DaemonMethod;
  params?: Record<string, unknown>;
  type: "request";
}

export interface DaemonResponseEnvelope {
  error?: string;
  id: string;
  ok: boolean;
  result?: unknown;
  type: "response";
}

export interface DaemonEventEnvelope {
  event: "log" | "state";
  payload: DaemonRuntimeState | LogEnvelope;
  type: "event";
}

export interface CreateTableParams {
  table?: Partial<CreateTableRequest>;
}

export interface JoinTableParams {
  buyInSats?: number;
  inviteCode: string;
}

export interface WalletDepositParams {
  amountSats: number;
}

export interface WalletWithdrawParams {
  amountSats: number;
  invoice: string;
}

export interface WalletFaucetParams {
  amountSats: number;
}

export interface WalletOffboardParams {
  address: string;
  amountSats?: number;
}

export interface BootstrapParams {
  nickname?: string;
}

export interface CommitSeedParams {
  reveal?: boolean;
}

export interface SendActionParams {
  payload: SignedActionPayload;
}

export interface SendPeerMessageParams {
  message: string;
}

export interface MeshBootstrapPeerParams {
  alias?: string;
  peerUrl: string;
  roles?: MeshRuntimeMode[];
}

export interface MeshCreateTableParams {
  table?: {
    bigBlindSats?: number;
    buyInMaxSats?: number;
    buyInMinSats?: number;
    name?: string;
    public?: boolean;
    smallBlindSats?: number;
    spectatorsAllowed?: boolean;
    witnessPeerIds?: string[];
  };
}

export interface MeshJoinTableParams {
  buyInSats?: number;
  inviteCode: string;
}

export interface MeshActionParams {
  payload: MeshPlayerActionPayload;
  tableId?: string;
}

export interface MeshTableParams {
  tableId?: string;
}

export function parseDaemonEnvelope(input: string): DaemonRequestEnvelope {
  return JSON.parse(input) as DaemonRequestEnvelope;
}
