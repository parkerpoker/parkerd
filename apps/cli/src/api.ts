import type {
  CreateTableRequest,
  CreateTableResponse,
  DeckCommitment,
  JoinTableRequest,
  JoinTableResponse,
  TableCheckpoint,
  TableSnapshot,
  TimeoutDelegation,
} from "@parker/protocol";

export interface TranscriptEventRecord {
  createdAt: string;
  eventId: string;
  eventType: string;
  payload: unknown;
  tableId: string;
}

export interface TableTranscript {
  checkpoints: TableCheckpoint[];
  events: TranscriptEventRecord[];
}

export class ParkerApiClient {
  constructor(private readonly serverUrl: string) {}

  createTable(request: CreateTableRequest) {
    return this.fetchJson<CreateTableResponse>("/api/tables", {
      method: "POST",
      body: JSON.stringify(request),
    });
  }

  joinTable(request: JoinTableRequest) {
    return this.fetchJson<JoinTableResponse>("/api/tables/join", {
      method: "POST",
      body: JSON.stringify(request),
    });
  }

  getTable(tableId: string) {
    return this.fetchJson<TableSnapshot>(`/api/tables/${tableId}`);
  }

  getTableByInvite(inviteCode: string) {
    return this.fetchJson<TableSnapshot>(`/api/tables/by-invite/${inviteCode}`);
  }

  getTranscript(tableId: string) {
    return this.fetchJson<TableTranscript>(`/api/tables/${tableId}/transcript`);
  }

  postCommitment(tableId: string, commitment: DeckCommitment) {
    return this.fetchJson<TableSnapshot>(`/api/tables/${tableId}/commitments`, {
      method: "POST",
      body: JSON.stringify(commitment),
    });
  }

  postDelegation(tableId: string, delegation: TimeoutDelegation) {
    return this.fetchJson<TableSnapshot>(`/api/tables/${tableId}/delegations`, {
      method: "POST",
      body: JSON.stringify(delegation),
    });
  }

  async waitForHealth(timeoutMs = 30_000) {
    const start = Date.now();
    while (Date.now() - start < timeoutMs) {
      try {
        const response = await fetch(`${this.serverUrl}/health`);
        if (response.ok) {
          return;
        }
      } catch {
        // Retry until the timeout.
      }
      await sleep(250);
    }
    throw new Error(`server at ${this.serverUrl} did not become healthy within ${timeoutMs}ms`);
  }

  private async fetchJson<T>(path: string, init?: RequestInit): Promise<T> {
    const response = await fetch(`${this.serverUrl}${path}`, {
      headers: {
        "Content-Type": "application/json",
      },
      ...init,
    });
    if (!response.ok) {
      throw new Error((await response.text()) || `request failed with ${response.status}`);
    }
    return (await response.json()) as T;
  }
}

function sleep(ms: number) {
  return new Promise<void>((resolve) => {
    setTimeout(resolve, ms);
  });
}
