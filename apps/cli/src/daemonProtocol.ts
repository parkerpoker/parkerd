import type { CreateTableRequest, SignedActionPayload } from "@parker/protocol";

import type { LogEnvelope } from "./logger.js";
import type { PlayerRuntimeState } from "./playerClient.js";

export interface ProfileDaemonMetadata {
  lastHeartbeat: string;
  logPath: string;
  pid: number;
  profile: string;
  socketPath: string;
  startedAt: string;
  status: "running" | "starting" | "stopping";
}

export type DaemonMethod =
  | "bootstrap"
  | "connectCurrentTable"
  | "createTable"
  | "getSnapshot"
  | "getTranscript"
  | "joinTable"
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
  payload: LogEnvelope | PlayerRuntimeState;
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

export function parseDaemonEnvelope(input: string): DaemonRequestEnvelope {
  return JSON.parse(input) as DaemonRequestEnvelope;
}
