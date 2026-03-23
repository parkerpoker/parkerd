import { mkdirSync } from "node:fs";
import { dirname } from "node:path";

import Database from "better-sqlite3";

import type {
  PublicTableUpdate,
  PublicTableView,
  SignedTableAdvertisement,
} from "@parker/protocol";

export class ParkerDatabase {
  private readonly db: Database.Database;

  constructor(filename = "apps/indexer/data/parker.sqlite") {
    if (filename !== ":memory:") {
      mkdirSync(dirname(filename), { recursive: true });
    }
    this.db = new Database(filename);
    this.db.pragma("journal_mode = WAL");
    this.migrate();
  }

  private migrate() {
    this.db.exec(`
      CREATE TABLE IF NOT EXISTS public_table_ads (
        table_id TEXT PRIMARY KEY,
        ad_json TEXT NOT NULL,
        updated_at TEXT NOT NULL
      );

      CREATE TABLE IF NOT EXISTS public_table_updates (
        update_id TEXT PRIMARY KEY,
        table_id TEXT NOT NULL,
        update_json TEXT NOT NULL,
        created_at TEXT NOT NULL
      );
    `);
  }

  getPublicTableView(tableId: string): PublicTableView | null {
    const ad = this.db
      .prepare<{ tableId: string }, { ad_json: string }>(
        `SELECT ad_json FROM public_table_ads WHERE table_id = @tableId`,
      )
      .get({ tableId });
    if (!ad) {
      return null;
    }
    return {
      advertisement: JSON.parse(ad.ad_json) as SignedTableAdvertisement,
      latestState: this.getLatestPublicState(tableId),
      recentUpdates: this.listPublicUpdates(tableId),
    };
  }

  listPublicTableViews(): PublicTableView[] {
    const rows = this.db
      .prepare<[], { table_id: string }>(`SELECT table_id FROM public_table_ads ORDER BY updated_at DESC`)
      .all();
    return rows
      .map((row) => this.getPublicTableView(row.table_id))
      .filter((value): value is PublicTableView => Boolean(value));
  }

  listPublicUpdates(tableId: string): PublicTableUpdate[] {
    const rows = this.db
      .prepare<{ tableId: string }, { update_json: string }>(
        `SELECT update_json FROM public_table_updates WHERE table_id = @tableId ORDER BY created_at DESC LIMIT 32`,
      )
      .all({ tableId });
    return rows.map((row) => JSON.parse(row.update_json) as PublicTableUpdate);
  }

  savePublicTableAd(ad: SignedTableAdvertisement) {
    this.db
      .prepare(
        `
          INSERT INTO public_table_ads (table_id, ad_json, updated_at)
          VALUES (@tableId, @adJson, @updatedAt)
          ON CONFLICT(table_id) DO UPDATE SET
            ad_json = excluded.ad_json,
            updated_at = excluded.updated_at
        `,
      )
      .run({
        tableId: ad.tableId,
        adJson: JSON.stringify(ad),
        updatedAt: new Date().toISOString(),
      });
  }

  savePublicUpdate(update: PublicTableUpdate) {
    this.db
      .prepare(
        `
          INSERT INTO public_table_updates (update_id, table_id, update_json, created_at)
          VALUES (@updateId, @tableId, @updateJson, @createdAt)
        `,
      )
      .run({
        updateId: crypto.randomUUID(),
        tableId: update.tableId,
        updateJson: JSON.stringify(update),
        createdAt: "publishedAt" in update ? update.publishedAt : new Date().toISOString(),
      });
  }

  private getLatestPublicState(tableId: string) {
    const updates = this.listPublicUpdates(tableId);
    for (const update of updates) {
      if (update.type === "PublicTableSnapshot" || update.type === "PublicHandUpdate") {
        return update.publicState;
      }
    }
    return null;
  }

  close() {
    this.db.close();
  }
}
