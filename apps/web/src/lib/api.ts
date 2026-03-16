import type {
  CreateTableRequest,
  CreateTableResponse,
  DeckCommitment,
  JoinTableRequest,
  JoinTableResponse,
  TableSnapshot,
  TimeoutDelegation,
} from "@parker/protocol";

export const SERVER_URL = import.meta.env.VITE_SERVER_URL ?? "http://localhost:3020";
export const WEBSOCKET_URL =
  import.meta.env.VITE_WEBSOCKET_URL ?? SERVER_URL.replace(/^http/, "ws") + "/ws";

async function fetchJson<T>(path: string, init?: RequestInit): Promise<T> {
  const response = await fetch(`${SERVER_URL}${path}`, {
    headers: {
      "Content-Type": "application/json",
    },
    ...init,
  });

  if (!response.ok) {
    const message = await response.text();
    throw new Error(message || `request failed with ${response.status}`);
  }

  return (await response.json()) as T;
}

export function createTable(request: CreateTableRequest) {
  return fetchJson<CreateTableResponse>("/api/tables", {
    method: "POST",
    body: JSON.stringify(request),
  });
}

export function joinTable(request: JoinTableRequest) {
  return fetchJson<JoinTableResponse>("/api/tables/join", {
    method: "POST",
    body: JSON.stringify(request),
  });
}

export function getTable(tableId: string) {
  return fetchJson<TableSnapshot>(`/api/tables/${tableId}`);
}

export function getTableByInvite(inviteCode: string) {
  return fetchJson<TableSnapshot>(`/api/tables/by-invite/${inviteCode}`);
}

export function postCommitment(tableId: string, commitment: DeckCommitment) {
  return fetchJson<TableSnapshot>(`/api/tables/${tableId}/commitments`, {
    method: "POST",
    body: JSON.stringify(commitment),
  });
}

export function postDelegation(tableId: string, delegation: TimeoutDelegation) {
  return fetchJson<TableSnapshot>(`/api/tables/${tableId}/delegations`, {
    method: "POST",
    body: JSON.stringify(delegation),
  });
}

