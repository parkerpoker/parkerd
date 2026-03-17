import type {
  PublicTableUpdate,
  PublicTableView,
  SignedTableAdvertisement,
} from "@parker/protocol";

export class IndexerClient {
  constructor(private readonly baseUrl: string) {}

  async announceTable(ad: SignedTableAdvertisement) {
    await this.postJson("/api/indexer/table-ads", ad);
  }

  async fetchPublicTable(tableId: string) {
    return await this.fetchJson<PublicTableView>(`/api/public/tables/${tableId}`);
  }

  async fetchPublicTables() {
    return await this.fetchJson<PublicTableView[]>("/api/public/tables");
  }

  async publishUpdate(update: PublicTableUpdate) {
    await this.postJson("/api/indexer/table-updates", update);
  }

  private async fetchJson<T>(path: string): Promise<T> {
    const response = await fetch(`${this.baseUrl}${path}`);
    if (!response.ok) {
      throw new Error((await response.text()) || `indexer request failed with ${response.status}`);
    }
    return (await response.json()) as T;
  }

  private async postJson(path: string, body: unknown) {
    const response = await fetch(`${this.baseUrl}${path}`, {
      body: JSON.stringify(body),
      headers: {
        "Content-Type": "application/json",
      },
      method: "POST",
    });
    if (!response.ok) {
      throw new Error((await response.text()) || `indexer request failed with ${response.status}`);
    }
  }
}
