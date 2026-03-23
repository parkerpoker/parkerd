import type {
  DaemonRuntimeState,
  LocalProfileSummary,
  MeshRuntimeMode,
  MeshTableView,
  ProfileDaemonStatus,
} from "../types/parker.js";

const LOCAL_CONTROLLER_HEADER = "X-Parker-Local-Controller";
const LOCAL_CONTROLLER_BASE = import.meta.env.VITE_LOCAL_CONTROLLER_URL ?? "";

export interface LocalProfileStatusResponse {
  daemon: ProfileDaemonStatus;
  profile: LocalProfileSummary;
}

export interface LocalControllerHealth {
  allowedOrigins: string[];
  bind: string;
  ok: boolean;
  profilesDir: string;
  publicIndexerConfigured: boolean;
  webBundleAvailable: boolean;
}

export interface LocalControllerLogEvent {
  data?: unknown;
  level: "error" | "info" | "result";
  message?: string;
  scope?: string;
}

function controllerUrl(path: string) {
  return `${LOCAL_CONTROLLER_BASE}${path}`;
}

export function localControllerHeaders(init?: HeadersInit) {
  return {
    "Content-Type": "application/json",
    [LOCAL_CONTROLLER_HEADER]: "1",
    ...(init ?? {}),
  };
}

async function fetchControllerJson<T>(path: string, init?: RequestInit): Promise<T> {
  const response = await fetch(controllerUrl(path), {
    ...init,
    headers: localControllerHeaders(init?.headers),
  });

  if (!response.ok) {
    throw new Error(await readErrorMessage(response));
  }

  return (await response.json()) as T;
}

async function postControllerJson<T>(path: string, body?: unknown): Promise<T> {
  return await fetchControllerJson<T>(path, {
    ...(body === undefined ? {} : { body: JSON.stringify(body) }),
    method: "POST",
  });
}

async function readErrorMessage(response: Response) {
  try {
    const payload = (await response.json()) as { error?: string; message?: string };
    return payload.error ?? payload.message ?? `controller request failed with ${response.status}`;
  } catch {
    return (await response.text()) || `controller request failed with ${response.status}`;
  }
}

export function getLocalControllerHealth() {
  return fetch(controllerUrl("/health")).then(async (response) => {
    if (!response.ok) {
      throw new Error(await readErrorMessage(response));
    }
    return (await response.json()) as LocalControllerHealth;
  });
}

export function listLocalProfiles() {
  return fetchControllerJson<LocalProfileSummary[]>("/api/local/profiles");
}

export function getLocalProfileStatus(profile: string) {
  return fetchControllerJson<LocalProfileStatusResponse>(`/api/local/profiles/${profile}/status`);
}

export function startLocalDaemon(profile: string, body?: { mode?: MeshRuntimeMode }) {
  return postControllerJson<LocalProfileStatusResponse>(`/api/local/profiles/${profile}/daemon/start`, body);
}

export function stopLocalDaemon(profile: string) {
  return postControllerJson<LocalProfileStatusResponse>(`/api/local/profiles/${profile}/daemon/stop`);
}

export function bootstrapLocalProfile(profile: string, body?: { nickname?: string }) {
  return postControllerJson<{ mesh: DaemonRuntimeState["mesh"] }>(`/api/local/profiles/${profile}/bootstrap`, body);
}

export function getLocalWallet(profile: string) {
  return fetchControllerJson<DaemonRuntimeState["wallet"]>(`/api/local/profiles/${profile}/wallet`);
}

export function requestLocalDeposit(profile: string, amountSats: number) {
  return postControllerJson(`/api/local/profiles/${profile}/wallet/deposit`, { amountSats });
}

export function requestLocalWithdraw(profile: string, amountSats: number, invoice: string) {
  return postControllerJson(`/api/local/profiles/${profile}/wallet/withdraw`, { amountSats, invoice });
}

export function requestLocalFaucet(profile: string, amountSats: number) {
  return postControllerJson<LocalProfileStatusResponse["daemon"]>(`/api/local/profiles/${profile}/wallet/faucet`, {
    amountSats,
  });
}

export function requestLocalOnboard(profile: string) {
  return postControllerJson<string>(`/api/local/profiles/${profile}/wallet/onboard`);
}

export function requestLocalOffboard(profile: string, address: string, amountSats?: number) {
  return postControllerJson<string>(`/api/local/profiles/${profile}/wallet/offboard`, {
    address,
    ...(amountSats === undefined ? {} : { amountSats }),
  });
}

export function listLocalPeers(profile: string) {
  return fetchControllerJson<NonNullable<DaemonRuntimeState["mesh"]>["peers"]>(`/api/local/profiles/${profile}/network/peers`);
}

export function bootstrapLocalPeer(
  profile: string,
  body: {
    alias?: string;
    peerUrl: string;
    roles?: MeshRuntimeMode[];
  },
) {
  return postControllerJson(`/api/local/profiles/${profile}/network/bootstrap`, body);
}

export function listLocalPublicTables(profile: string) {
  return fetchControllerJson(`/api/local/profiles/${profile}/tables/public`);
}

export function createLocalTable(profile: string, table?: Record<string, unknown>) {
  return postControllerJson(`/api/local/profiles/${profile}/tables`, table ? { table } : undefined);
}

export function joinLocalTable(profile: string, inviteCode: string, buyInSats?: number) {
  return postControllerJson<MeshTableView>(`/api/local/profiles/${profile}/tables/join`, {
    inviteCode,
    ...(buyInSats === undefined ? {} : { buyInSats }),
  });
}

export function getLocalTable(profile: string, tableId: string) {
  return fetchControllerJson<MeshTableView>(`/api/local/profiles/${profile}/tables/${tableId}`);
}

export function announceLocalTable(profile: string, tableId: string) {
  return postControllerJson(`/api/local/profiles/${profile}/tables/${tableId}/announce`);
}

export function submitLocalTableAction(
  profile: string,
  tableId: string,
  payload: {
    type: string;
    totalSats?: number;
  },
) {
  return postControllerJson(`/api/local/profiles/${profile}/tables/${tableId}/action`, { payload });
}

export function rotateLocalHost(profile: string, tableId: string) {
  return postControllerJson(`/api/local/profiles/${profile}/tables/${tableId}/rotate-host`);
}

export function cashOutLocalTable(profile: string, tableId: string) {
  return postControllerJson(`/api/local/profiles/${profile}/tables/${tableId}/cashout`);
}

export function renewLocalTable(profile: string, tableId: string) {
  return postControllerJson(`/api/local/profiles/${profile}/tables/${tableId}/renew`);
}

export function exitLocalTable(profile: string, tableId: string) {
  return postControllerJson(`/api/local/profiles/${profile}/tables/${tableId}/exit`);
}
