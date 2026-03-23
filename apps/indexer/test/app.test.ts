import { afterEach, describe, expect, it } from "vitest";

import { createApp } from "../src/app.js";
import { ParkerDatabase } from "../src/db.js";

describe("indexer app", () => {
  let db: ParkerDatabase | null = null;
  let app: Awaited<ReturnType<typeof createApp>>["app"] | null = null;

  afterEach(async () => {
    await app?.close();
    db?.close();
    app = null;
    db = null;
  });

  it("stores and serves public table ads and updates", async () => {
    db = new ParkerDatabase(":memory:");
    ({ app } = await createApp({ database: db }));
    const tableId = "11111111-1111-4111-8111-111111111111";

    const advertisement = {
      protocolVersion: "poker/v1",
      networkId: "regtest",
      tableId,
      hostPeerId: "peer-host-1",
      hostPeerUrl: "ws://127.0.0.1:7777/mesh",
      tableName: "Public Test Table",
      stakes: { smallBlindSats: 50, bigBlindSats: 100 },
      currency: "sats",
      seatCount: 2,
      occupiedSeats: 1,
      spectatorsAllowed: true,
      hostModeCapabilities: ["host-dealer-v1"],
      witnessCount: 1,
      buyInMinSats: 4000,
      buyInMaxSats: 4000,
      visibility: "public",
      latencyHintMs: 25,
      adExpiresAt: "2026-03-23T00:00:10.000Z",
      hostProtocolPubkeyHex: "02".repeat(33),
      hostSignatureHex: "aa".repeat(64),
    };

    const publicState = {
      snapshotId: "22222222-2222-4222-8222-222222222222",
      tableId,
      epoch: 1,
      status: "ready",
      handId: null,
      handNumber: 0,
      phase: null,
      actingSeatIndex: null,
      dealerSeatIndex: 0,
      board: [],
      seatedPlayers: [],
      chipBalances: {},
      roundContributions: {},
      totalContributions: {},
      potSats: 0,
      currentBetSats: 0,
      minRaiseToSats: 100,
      livePlayerIds: [],
      foldedPlayerIds: [],
      dealerCommitment: null,
      previousSnapshotHash: null,
      latestEventHash: null,
      updatedAt: "2026-03-23T00:00:00.000Z",
    };

    const adResponse = await app.inject({
      method: "POST",
      url: "/api/indexer/table-ads",
      payload: advertisement,
    });
    expect(adResponse.statusCode).toBe(200);

    const updateResponse = await app.inject({
      method: "POST",
      url: "/api/indexer/table-updates",
      payload: {
        type: "PublicTableSnapshot",
        tableId,
        advertisement,
        publicState,
        publishedAt: "2026-03-23T00:00:01.000Z",
      },
    });
    expect(updateResponse.statusCode).toBe(200);

    const listResponse = await app.inject({
      method: "GET",
      url: "/api/public/tables",
    });
    expect(listResponse.statusCode).toBe(200);
    const list = listResponse.json();
    expect(list).toHaveLength(1);
    expect(list[0].advertisement.tableId).toBe(tableId);
    expect(list[0].latestState.snapshotId).toBe("22222222-2222-4222-8222-222222222222");

    const tableResponse = await app.inject({
      method: "GET",
      url: `/api/public/tables/${tableId}`,
    });
    expect(tableResponse.statusCode).toBe(200);
    expect(tableResponse.json().recentUpdates).toHaveLength(1);
  });
});
