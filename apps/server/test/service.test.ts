import { afterEach, describe, expect, it } from "vitest";

import { ParkerDatabase } from "../src/db.js";
import { ParkerTableService } from "../src/service.js";

describe("ParkerTableService", () => {
  let db: ParkerDatabase | null = null;

  afterEach(() => {
    db?.close();
    db = null;
  });

  function createService() {
    db = new ParkerDatabase(":memory:");
    return new ParkerTableService(db, {
      websocketUrl: "ws://localhost:3020/ws",
    });
  }

  it("creates, joins, and starts a private table", () => {
    const service = createService();
    const created = service.createTable({
      host: {
        playerId: "player-alpha",
        nickname: "Alpha",
        joinedAt: "2026-03-15T12:00:00.000Z",
        pubkeyHex: "02".repeat(33),
        arkAddress: "tark1alpha",
      },
    });

    expect(created.table.inviteCode).toHaveLength(8);

    const joined = service.joinTable({
      inviteCode: created.table.inviteCode,
      player: {
        playerId: "player-beta",
        nickname: "Beta",
        joinedAt: "2026-03-15T12:01:00.000Z",
        pubkeyHex: "03".repeat(33),
        arkAddress: "tark1beta",
      },
      buyInSats: 4000,
    });

    expect(joined.table.status).toBe("seeding");

    const commitA = service.buildClientCommitment({
      tableId: created.table.tableId,
      seatIndex: 0,
      playerId: "player-alpha",
      seedHex: "11".repeat(32),
    });
    const commitB = service.buildClientCommitment({
      tableId: created.table.tableId,
      seatIndex: 1,
      playerId: "player-beta",
      seedHex: "22".repeat(32),
    });

    service.saveCommitment(created.table.tableId, {
      ...commitA,
      revealSeed: "11".repeat(32),
      revealedAt: "2026-03-15T12:01:05.000Z",
    });
    const snapshot = service.saveCommitment(created.table.tableId, {
      ...commitB,
      revealSeed: "22".repeat(32),
      revealedAt: "2026-03-15T12:01:06.000Z",
    });

    expect(snapshot.table.status).toBe("active");
    expect(snapshot.checkpoint?.phase).toBe("preflop");
  });

  it("records actions and checkpoints", () => {
    const service = createService();
    const created = service.createTable({
      host: {
        playerId: "player-alpha",
        nickname: "Alpha",
        joinedAt: "2026-03-15T12:00:00.000Z",
        pubkeyHex: "02".repeat(33),
        arkAddress: "tark1alpha",
      },
    });

    service.joinTable({
      inviteCode: created.table.inviteCode,
      player: {
        playerId: "player-beta",
        nickname: "Beta",
        joinedAt: "2026-03-15T12:01:00.000Z",
        pubkeyHex: "03".repeat(33),
        arkAddress: "tark1beta",
      },
      buyInSats: 4000,
    });

    const commitA = service.buildClientCommitment({
      tableId: created.table.tableId,
      seatIndex: 0,
      playerId: "player-alpha",
      seedHex: "11".repeat(32),
    });
    const commitB = service.buildClientCommitment({
      tableId: created.table.tableId,
      seatIndex: 1,
      playerId: "player-beta",
      seedHex: "22".repeat(32),
    });

    service.saveCommitment(created.table.tableId, {
      ...commitA,
      revealSeed: "11".repeat(32),
      revealedAt: "2026-03-15T12:01:05.000Z",
    });
    const snapshot = service.saveCommitment(created.table.tableId, {
      ...commitB,
      revealSeed: "22".repeat(32),
      revealedAt: "2026-03-15T12:01:06.000Z",
    });

    const nextSnapshot = service.processSignedAction({
      tableId: created.table.tableId,
      handId: snapshot.checkpoint!.handId,
      checkpointId: snapshot.checkpoint!.checkpointId,
      clientSeq: 1,
      actorPlayerId: "player-alpha",
      actorSeatIndex: 0,
      sentAt: "2026-03-15T12:01:10.000Z",
      signerPubkeyHex: "02".repeat(33),
      signatureHex: "aa".repeat(64),
      payload: {
        type: "call",
      },
    });

    expect(nextSnapshot.checkpoint?.handNumber).toBe(1);
    expect(service.listTranscript(created.table.tableId).events).toHaveLength(1);
  });
});
