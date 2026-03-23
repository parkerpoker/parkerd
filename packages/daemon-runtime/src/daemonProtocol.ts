import type { MeshPlayerActionPayload, SwapJobStatus, SwapQuote } from "@parker/protocol";
import type { WalletSummary } from "@parker/settlement";

import type { LogEnvelope } from "./logger.js";
import type { MeshRuntimeState, MeshRuntimeMode } from "./meshTypes.js";

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

export interface MeshBootstrapResult {
  peerId: string;
  peerPublicKeyHex: string;
  protocolId: string;
  protocolPublicKeyHex: string;
  wallet: WalletSummary;
  walletPlayerId: string;
}

export interface DaemonRuntimeState {
  mesh?: MeshRuntimeState;
  wallet?: WalletSummary;
}

export const DAEMON_HEARTBEAT_INTERVAL_MS = 5_000;

export const DAEMON_METHODS = [
  "bootstrap",
  "meshBootstrapPeer",
  "meshCashOut",
  "meshCreateTable",
  "meshExit",
  "meshGetTable",
  "meshNetworkPeers",
  "meshPublicTables",
  "meshRenew",
  "meshRotateHost",
  "meshSendAction",
  "meshTableAnnounce",
  "meshTableJoin",
  "ping",
  "status",
  "stop",
  "watch",
  "walletDeposit",
  "walletFaucet",
  "walletOffboard",
  "walletOnboard",
  "walletSummary",
  "walletWithdraw",
] as const;

export const DAEMON_WATCH_EVENTS = ["log", "state"] as const;

export type DaemonMethod = (typeof DAEMON_METHODS)[number];

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
  event: (typeof DAEMON_WATCH_EVENTS)[number];
  payload: DaemonRuntimeState | LogEnvelope;
  type: "event";
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

export interface WalletDepositResult extends SwapQuote {}
export interface WalletWithdrawResult extends SwapJobStatus {}

export function parseDaemonEnvelope(input: string): DaemonRequestEnvelope {
  return JSON.parse(input) as DaemonRequestEnvelope;
}
