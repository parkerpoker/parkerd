import type { PublicTableView } from "@parker/protocol";

export const INDEXER_URL = import.meta.env.VITE_INDEXER_URL ?? "";

async function fetchJson<T>(path: string, init?: RequestInit): Promise<T> {
  const response = await fetch(`${INDEXER_URL}${path}`, {
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

export function getPublicTable(tableId: string) {
  return fetchJson<PublicTableView>(`/api/public/tables/${tableId}`);
}

export function listPublicTables() {
  return fetchJson<PublicTableView[]>("/api/public/tables");
}
