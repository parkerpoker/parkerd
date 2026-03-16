import { describe, expect, it } from "vitest";

import {
  buildEscrowDescriptor,
  createLocalIdentity,
  createMockSettlementProvider,
  createTimeoutDelegation,
  signCheckpoint,
  verifyCheckpointSignature,
} from "../src/index.js";

describe("settlement helpers", () => {
  it("signs and verifies checkpoint payloads", () => {
    const identity = createLocalIdentity("11".repeat(32));
    const checkpoint = {
      checkpointId: "1c33f18d-4ff2-4b6f-9f46-0ef6c338efb6",
      tableId: "9f710fc0-58a8-4cb2-8d89-014d977ff8d5",
      handId: "8b628dfa-271d-4a27-aed7-fdf9d5f747d4",
      handNumber: 1,
      phase: "preflop" as const,
      actingSeatIndex: 0,
      dealerSeatIndex: 0,
      board: [],
      holeCardsByPlayerId: {
        alpha: ["Ah", "Kd"],
        beta: ["Qs", "Jc"],
      },
      playerStacks: {
        alpha: 1900,
        beta: 1900,
      },
      roundContributions: {
        alpha: 100,
        beta: 100,
      },
      totalContributions: {
        alpha: 100,
        beta: 100,
      },
      potSats: 200,
      currentBetSats: 100,
      minRaiseToSats: 200,
      deckSeedHash: "aa".repeat(32),
      commitmentHashes: ["bb".repeat(32), "cc".repeat(32)],
      nextActionDeadline: "2026-03-15T12:05:00.000Z",
      transcriptHash: "dd".repeat(32),
    };

    const signature = signCheckpoint(identity, checkpoint);
    expect(
      verifyCheckpointSignature({
        publicKeyHex: identity.publicKeyHex,
        checkpoint,
        signatureHex: signature,
      }),
    ).toBe(true);
  });

  it("creates timeout delegations", () => {
    const identity = createLocalIdentity("22".repeat(32));
    const delegation = createTimeoutDelegation({
      tableId: "9f710fc0-58a8-4cb2-8d89-014d977ff8d5",
      checkpointId: "f280f1ee-c2c7-4cb3-a78c-db46eaab4947",
      actingSeatIndex: 1,
      delegatedPlayerId: "player-alpha",
      settlementAddress: "tark1timeoutfold",
      validAfter: "2026-03-15T12:05:00.000Z",
      expiresAt: "2026-03-15T12:06:00.000Z",
      signer: identity,
    });

    expect(delegation.delegatedAction).toBe("timeout-fold");
    expect(delegation.signatureHex).toHaveLength(128);
  });

  it("builds deterministic escrow descriptors", () => {
    const descriptor = buildEscrowDescriptor({
      tableId: "9f710fc0-58a8-4cb2-8d89-014d977ff8d5",
      network: "regtest",
      participantPubkeys: ["02".repeat(33), "03".repeat(33)],
      watchtowerPubkey: "04".repeat(33),
      totalLockedSats: 8_000,
      refundDelayBlocks: 12,
      currentCheckpointId: "f280f1ee-c2c7-4cb3-a78c-db46eaab4947",
    });

    expect(descriptor.contractAddress.startsWith("descriptor:")).toBe(true);
    expect(descriptor.status).toBe("funded");
    expect(descriptor.network).toBe("regtest");
  });

  it("supports mock deposit and withdrawal flows", async () => {
    const provider = createMockSettlementProvider();
    const identity = provider.createLocalIdentity("33".repeat(32));
    const quote = await provider.createDepositQuote(identity, 10_000);
    const withdrawal = await provider.submitWithdrawal(identity, "lnbc1invoice");

    expect(quote.direction).toBe("deposit");
    expect(withdrawal.status).toBe("completed");
  });
});
