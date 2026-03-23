import { describe, expect, it } from "vitest";

import {
  meshPlayerActionPayloadSchema,
  networkSchema,
  publicTableUpdateSchema,
  signedTableAdvertisementSchema,
} from "../src/index.js";

describe("protocol schemas", () => {
  it("accepts signed public advertisements", () => {
    const parsed = signedTableAdvertisementSchema.parse({
      protocolVersion: "poker/v1",
      networkId: "regtest",
      tableId: "9f710fc0-58a8-4cb2-8d89-014d977ff8d5",
      hostPeerId: "peer-alpha-123",
      hostPeerUrl: "ws://127.0.0.1:7777/mesh",
      tableName: "Public Table",
      stakes: {
        bigBlindSats: 100,
        smallBlindSats: 50,
      },
      currency: "sats",
      seatCount: 2,
      occupiedSeats: 1,
      spectatorsAllowed: true,
      hostModeCapabilities: ["host-dealer-v1"],
      witnessCount: 1,
      buyInMinSats: 4_000,
      buyInMaxSats: 4_000,
      visibility: "public",
      adExpiresAt: "2026-03-23T00:00:10.000Z",
      hostProtocolPubkeyHex: "02".repeat(33),
      hostSignatureHex: "aa".repeat(64),
    });

    expect(parsed.tableName).toBe("Public Table");
  });

  it("accepts regtest networks", () => {
    expect(networkSchema.parse("regtest")).toBe("regtest");
  });

  it("parses public table updates", () => {
    const advertisement = signedTableAdvertisementSchema.parse({
      protocolVersion: "poker/v1",
      networkId: "regtest",
      tableId: "9f710fc0-58a8-4cb2-8d89-014d977ff8d5",
      hostPeerId: "peer-alpha-123",
      hostPeerUrl: "ws://127.0.0.1:7777/mesh",
      tableName: "Public Table",
      stakes: {
        bigBlindSats: 100,
        smallBlindSats: 50,
      },
      currency: "sats",
      seatCount: 2,
      occupiedSeats: 1,
      spectatorsAllowed: true,
      hostModeCapabilities: ["host-dealer-v1"],
      witnessCount: 1,
      buyInMinSats: 4_000,
      buyInMaxSats: 4_000,
      visibility: "public",
      adExpiresAt: "2026-03-23T00:00:10.000Z",
      hostProtocolPubkeyHex: "02".repeat(33),
      hostSignatureHex: "aa".repeat(64),
    });

    const parsed = publicTableUpdateSchema.parse({
      type: "PublicTableSnapshot",
      tableId: "9f710fc0-58a8-4cb2-8d89-014d977ff8d5",
      advertisement,
      publicState: null,
      publishedAt: "2026-03-23T00:00:11.000Z",
    });

    expect(parsed.type).toBe("PublicTableSnapshot");
  });

  it("accepts mesh betting actions", () => {
    const parsed = meshPlayerActionPayloadSchema.parse({
      type: "raise",
      totalSats: 800,
    });

    expect(parsed.totalSats).toBe(800);
  });
});
