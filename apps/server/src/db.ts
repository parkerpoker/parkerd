import { mkdirSync } from "node:fs";
import { dirname } from "node:path";

import Database from "better-sqlite3";

import type {
  PublicTableUpdate,
  PublicTableView,
  SignedTableAdvertisement,
  TableCheckpoint,
  TableSnapshot,
  TimeoutDelegation,
} from "@parker/protocol";
import type { HoldemState } from "@parker/game-engine";

export interface EventRecord {
  eventId: string;
  tableId: string;
  eventType: string;
  payload: unknown;
  createdAt: string;
}

export class ParkerDatabase {
  private readonly db: Database.Database;

  constructor(filename = "apps/server/data/parker.sqlite") {
    if (filename !== ":memory:") {
      mkdirSync(dirname(filename), { recursive: true });
    }
    this.db = new Database(filename);
    this.db.pragma("journal_mode = WAL");
    this.migrate();
  }

  private migrate() {
    this.db.exec(`
      CREATE TABLE IF NOT EXISTS tables (
        table_id TEXT PRIMARY KEY,
        invite_code TEXT NOT NULL UNIQUE,
        snapshot_json TEXT NOT NULL,
        created_at TEXT NOT NULL
      );

      CREATE TABLE IF NOT EXISTS hands (
        table_id TEXT PRIMARY KEY,
        state_json TEXT NOT NULL,
        updated_at TEXT NOT NULL
      );

      CREATE TABLE IF NOT EXISTS checkpoints (
        checkpoint_id TEXT PRIMARY KEY,
        table_id TEXT NOT NULL,
        checkpoint_json TEXT NOT NULL,
        created_at TEXT NOT NULL
      );

      CREATE TABLE IF NOT EXISTS delegations (
        delegation_id TEXT PRIMARY KEY,
        table_id TEXT NOT NULL,
        delegation_json TEXT NOT NULL,
        created_at TEXT NOT NULL
      );

      CREATE TABLE IF NOT EXISTS events (
        event_id TEXT PRIMARY KEY,
        table_id TEXT NOT NULL,
        event_type TEXT NOT NULL,
        payload_json TEXT NOT NULL,
        created_at TEXT NOT NULL
      );

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

  saveSnapshot(snapshot: TableSnapshot) {
    this.db
      .prepare(
        `
          INSERT INTO tables (table_id, invite_code, snapshot_json, created_at)
          VALUES (@tableId, @inviteCode, @snapshotJson, @createdAt)
          ON CONFLICT(table_id) DO UPDATE SET
            invite_code = excluded.invite_code,
            snapshot_json = excluded.snapshot_json
        `,
      )
      .run({
        tableId: snapshot.table.tableId,
        inviteCode: snapshot.table.inviteCode,
        snapshotJson: JSON.stringify(snapshot),
        createdAt: snapshot.table.createdAt,
      });
  }

  getSnapshot(tableId: string): TableSnapshot | null {
    const row = this.db
      .prepare<{ tableId: string }, { snapshot_json: string }>(
        `SELECT snapshot_json FROM tables WHERE table_id = @tableId`,
      )
      .get({ tableId });
    return row ? (JSON.parse(row.snapshot_json) as TableSnapshot) : null;
  }

  getSnapshotByInviteCode(inviteCode: string): TableSnapshot | null {
    const row = this.db
      .prepare<{ inviteCode: string }, { snapshot_json: string }>(
        `SELECT snapshot_json FROM tables WHERE invite_code = @inviteCode`,
      )
      .get({ inviteCode });
    return row ? (JSON.parse(row.snapshot_json) as TableSnapshot) : null;
  }

  listSnapshots(): TableSnapshot[] {
    const rows = this.db
      .prepare<[], { snapshot_json: string }>(`SELECT snapshot_json FROM tables`)
      .all();
    return rows.map((row) => JSON.parse(row.snapshot_json) as TableSnapshot);
  }

  saveHandState(tableId: string, state: HoldemState) {
    const updatedAt = new Date().toISOString();
    this.db
      .prepare(
        `
          INSERT INTO hands (table_id, state_json, updated_at)
          VALUES (@tableId, @stateJson, @updatedAt)
          ON CONFLICT(table_id) DO UPDATE SET
            state_json = excluded.state_json,
            updated_at = excluded.updated_at
        `,
      )
      .run({
        tableId,
        stateJson: JSON.stringify(state),
        updatedAt,
      });
  }

  getHandState(tableId: string): HoldemState | null {
    const row = this.db
      .prepare<{ tableId: string }, { state_json: string }>(
        `SELECT state_json FROM hands WHERE table_id = @tableId`,
      )
      .get({ tableId });
    return row ? (JSON.parse(row.state_json) as HoldemState) : null;
  }

  saveCheckpoint(checkpoint: TableCheckpoint) {
    this.db
      .prepare(
        `
          INSERT OR REPLACE INTO checkpoints (checkpoint_id, table_id, checkpoint_json, created_at)
          VALUES (@checkpointId, @tableId, @checkpointJson, @createdAt)
        `,
      )
      .run({
        checkpointId: checkpoint.checkpointId,
        tableId: checkpoint.tableId,
        checkpointJson: JSON.stringify(checkpoint),
        createdAt: new Date().toISOString(),
      });
  }

  listCheckpoints(tableId: string): TableCheckpoint[] {
    const rows = this.db
      .prepare<{ tableId: string }, { checkpoint_json: string }>(
        `SELECT checkpoint_json FROM checkpoints WHERE table_id = @tableId ORDER BY created_at ASC`,
      )
      .all({ tableId });
    return rows.map((row) => JSON.parse(row.checkpoint_json) as TableCheckpoint);
  }

  saveDelegation(delegation: TimeoutDelegation) {
    this.db
      .prepare(
        `
          INSERT OR REPLACE INTO delegations (delegation_id, table_id, delegation_json, created_at)
          VALUES (@delegationId, @tableId, @delegationJson, @createdAt)
        `,
      )
      .run({
        delegationId: delegation.delegationId,
        tableId: delegation.tableId,
        delegationJson: JSON.stringify(delegation),
        createdAt: new Date().toISOString(),
      });
  }

  appendEvent(record: EventRecord) {
    this.db
      .prepare(
        `
          INSERT INTO events (event_id, table_id, event_type, payload_json, created_at)
          VALUES (@eventId, @tableId, @eventType, @payloadJson, @createdAt)
        `,
      )
      .run({
        eventId: record.eventId,
        tableId: record.tableId,
        eventType: record.eventType,
        payloadJson: JSON.stringify(record.payload),
        createdAt: record.createdAt,
      });
  }

  listEvents(tableId: string): EventRecord[] {
    const rows = this.db
      .prepare<{ tableId: string }, { event_id: string; table_id: string; event_type: string; payload_json: string; created_at: string }>(
        `SELECT event_id, table_id, event_type, payload_json, created_at FROM events WHERE table_id = @tableId ORDER BY created_at ASC`,
      )
      .all({ tableId });

    return rows.map((row) => ({
      eventId: row.event_id,
      tableId: row.table_id,
      eventType: row.event_type,
      payload: JSON.parse(row.payload_json),
      createdAt: row.created_at,
    }));
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
      if (update.type === "PublicTableSnapshot") {
        return update.publicState;
      }
      if (update.type === "PublicHandUpdate") {
        return update.publicState;
      }
    }
    return null;
  }

  close() {
    this.db.close();
  }
}
