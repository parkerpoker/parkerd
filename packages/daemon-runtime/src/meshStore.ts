import { appendFile, mkdir, readFile, readdir, writeFile } from "node:fs/promises";
import { dirname, join } from "node:path";

import type {
  CooperativeTableSnapshot,
  SignedTableAdvertisement,
  SignedTableEvent,
} from "@parker/protocol";

import type { LocalPrivateTableState } from "./meshTypes.js";

const DEFAULT_PRIVATE_STATE: LocalPrivateTableState = {
  auditBundlesByHandId: {},
  myHoleCardsByHandId: {},
};

export class MeshStore {
  constructor(private readonly rootDir: string) {}

  async appendEvent(tableId: string, event: SignedTableEvent) {
    await this.appendNdjson(this.eventsPath(tableId), event);
  }

  async appendSnapshot(tableId: string, snapshot: CooperativeTableSnapshot) {
    await this.appendNdjson(this.snapshotsPath(tableId), snapshot);
  }

  async loadEvents(tableId: string): Promise<SignedTableEvent[]> {
    return await this.readNdjson<SignedTableEvent>(this.eventsPath(tableId));
  }

  async loadPrivateState(tableId: string): Promise<LocalPrivateTableState> {
    return await this.readJson(this.privateStatePath(tableId), DEFAULT_PRIVATE_STATE);
  }

  async loadPublicAds(): Promise<SignedTableAdvertisement[]> {
    const value = await this.readJson<Record<string, SignedTableAdvertisement>>(
      join(this.rootDir, "public-ads.json"),
      {},
    );
    return Object.values(value);
  }

  async loadSnapshots(tableId: string): Promise<CooperativeTableSnapshot[]> {
    return await this.readNdjson<CooperativeTableSnapshot>(this.snapshotsPath(tableId));
  }

  async savePrivateState(tableId: string, state: LocalPrivateTableState) {
    await this.writeJson(this.privateStatePath(tableId), state);
  }

  async upsertPublicAd(ad: SignedTableAdvertisement) {
    const current = await this.readJson<Record<string, SignedTableAdvertisement>>(
      join(this.rootDir, "public-ads.json"),
      {},
    );
    current[ad.tableId] = ad;
    await this.writeJson(join(this.rootDir, "public-ads.json"), current);
  }

  async listTableIds(): Promise<string[]> {
    try {
      const entries = await readdir(join(this.rootDir, "tables"), { withFileTypes: true });
      return entries.filter((entry) => entry.isDirectory()).map((entry) => entry.name);
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code === "ENOENT") {
        return [];
      }
      throw error;
    }
  }

  private async appendNdjson(path: string, input: unknown) {
    await mkdir(dirname(path), { recursive: true });
    await appendFile(path, `${JSON.stringify(input)}\n`, "utf8");
  }

  private async readJson<T>(path: string, fallback: T): Promise<T> {
    try {
      return JSON.parse(await readFile(path, "utf8")) as T;
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code === "ENOENT") {
        return fallback;
      }
      throw error;
    }
  }

  private async readNdjson<T>(path: string): Promise<T[]> {
    try {
      const raw = await readFile(path, "utf8");
      return raw
        .split("\n")
        .map((line) => line.trim())
        .filter(Boolean)
        .map((line) => JSON.parse(line) as T);
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code === "ENOENT") {
        return [];
      }
      throw error;
    }
  }

  private async writeJson(path: string, input: unknown) {
    await mkdir(dirname(path), { recursive: true });
    await writeFile(path, `${JSON.stringify(input, null, 2)}\n`, "utf8");
  }

  private eventsPath(tableId: string) {
    return join(this.rootDir, "tables", tableId, "events.ndjson");
  }

  private privateStatePath(tableId: string) {
    return join(this.rootDir, "tables", tableId, "private-state.json");
  }

  private snapshotsPath(tableId: string) {
    return join(this.rootDir, "tables", tableId, "snapshots.ndjson");
  }
}
