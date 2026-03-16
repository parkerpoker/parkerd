import { describe, expect, it } from "vitest";

import {
  clientSocketEventSchema,
  createTableRequestSchema,
  networkSchema,
  parseServerSocketEvent,
  serverSocketEventSchema,
} from "../src/index.js";

describe("protocol schemas", () => {
  it("fills create table defaults", () => {
    const parsed = createTableRequestSchema.parse({
      host: {
        playerId: "player-1234",
        nickname: "Parker",
        joinedAt: "2026-03-15T12:00:00.000Z",
        pubkeyHex: "deadbeef",
        arkAddress: "tark1testaddress",
      },
    });

    expect(parsed.bigBlindSats).toBe(100);
    expect(parsed.smallBlindSats).toBe(50);
  });

  it("accepts peer relay events", () => {
    const parsed = clientSocketEventSchema.parse({
      type: "peer-message",
      tableId: "9f710fc0-58a8-4cb2-8d89-014d977ff8d5",
      fromPlayerId: "player-alpha",
      targetPlayerId: "player-beta",
      message: "hello-beta",
    });

    expect(parsed.type).toBe("peer-message");
  });

  it("accepts regtest networks", () => {
    expect(networkSchema.parse("regtest")).toBe("regtest");
  });

  it("parses server snapshot events", () => {
    const parsed = parseServerSocketEvent({
      type: "table-snapshot",
      snapshot: {
        table: {
          tableId: "9f710fc0-58a8-4cb2-8d89-014d977ff8d5",
          inviteCode: "PARKER1",
          hostPlayerId: "player-alpha",
          network: "mutinynet",
          variant: "holdem-heads-up",
          smallBlindSats: 50,
          bigBlindSats: 100,
          buyInMinSats: 2000,
          buyInMaxSats: 10000,
          commitmentDeadlineSeconds: 20,
          actionTimeoutSeconds: 25,
          createdAt: "2026-03-15T12:00:00.000Z",
          status: "waiting",
        },
        seats: [
          {
            reservationId: "770e8400-e29b-41d4-a716-446655440000",
            tableId: "9f710fc0-58a8-4cb2-8d89-014d977ff8d5",
            seatIndex: 0,
            buyInSats: 4000,
            status: "reserved",
            inviteCode: "PARKER1",
            createdAt: "2026-03-15T12:00:00.000Z",
            expiresAt: "2026-03-15T12:05:00.000Z",
            player: {
              playerId: "player-alpha",
              nickname: "Alpha",
              joinedAt: "2026-03-15T12:00:00.000Z",
              pubkeyHex: "deadbeef",
              arkAddress: "tark1alpha",
            },
          },
          {
            reservationId: "770e8400-e29b-41d4-a716-446655440001",
            tableId: "9f710fc0-58a8-4cb2-8d89-014d977ff8d5",
            seatIndex: 1,
            buyInSats: 4000,
            status: "reserved",
            inviteCode: "PARKER1",
            createdAt: "2026-03-15T12:00:00.000Z",
            expiresAt: "2026-03-15T12:05:00.000Z",
            player: {
              playerId: "player-beta",
              nickname: "Beta",
              joinedAt: "2026-03-15T12:00:00.000Z",
              pubkeyHex: "cafebabe",
              arkAddress: "tark1beta",
            },
          },
        ],
        commitments: [],
        pendingDelegations: [],
      },
    });

    expect(serverSocketEventSchema.parse(parsed).type).toBe("table-snapshot");
  });
});
