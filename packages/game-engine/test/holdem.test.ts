import { describe, expect, it } from "vitest";

import {
  applyHoldemAction,
  buildCommitmentHash,
  createDeterministicDeck,
  createHoldemHand,
  deriveDeckSeed,
  getLegalActions,
  scoreSevenCardHand,
} from "../src/index.js";

describe("deterministic deck helpers", () => {
  it("produces the same shuffle for the same seed", () => {
    const first = createDeterministicDeck("abcd".repeat(16)).map((card) => card.code);
    const second = createDeterministicDeck("abcd".repeat(16)).map((card) => card.code);

    expect(first).toEqual(second);
  });

  it("derives a deck seed from verified commitments", () => {
    const reveal0 = "11".repeat(32);
    const reveal1 = "22".repeat(32);
    const tableId = "4187c6da-b781-48cc-a5b6-40df6d44c96f";
    const commitment0 = buildCommitmentHash({
      tableId,
      seatIndex: 0,
      playerId: "alpha-player",
      seedHex: reveal0,
    });
    const commitment1 = buildCommitmentHash({
      tableId,
      seatIndex: 1,
      playerId: "beta-player",
      seedHex: reveal1,
    });

    expect(
      deriveDeckSeed({
        tableId,
        handNumber: 1,
        commitments: [
          { seatIndex: 0, playerId: "alpha-player", commitmentHash: commitment0 },
          { seatIndex: 1, playerId: "beta-player", commitmentHash: commitment1 },
        ],
        reveals: [
          {
            seatIndex: 0,
            playerId: "alpha-player",
            commitmentHash: commitment0,
            revealSeed: reveal0,
          },
          {
            seatIndex: 1,
            playerId: "beta-player",
            commitmentHash: commitment1,
            revealSeed: reveal1,
          },
        ],
      }),
    ).toHaveLength(64);
  });
});

describe("holdem engine", () => {
  function startHand(deckSeedHex = "ab".repeat(32)) {
    return createHoldemHand({
      handId: "770e8400-e29b-41d4-a716-446655440000",
      handNumber: 1,
      deckSeedHex,
      dealerSeatIndex: 0,
      smallBlindSats: 50,
      bigBlindSats: 100,
      seats: [
        { playerId: "alpha", stackSats: 2000 },
        { playerId: "beta", stackSats: 2000 },
      ],
    });
  }

  it("rejects illegal preflop checks from the small blind", () => {
    const state = startHand();
    expect(() => applyHoldemAction(state, 0, { type: "check" })).toThrow(/illegal check/);
  });

  it("advances streets and preserves raise rules", () => {
    let state = startHand();
    const legalBeforeAction = getLegalActions(state, 0);
    const raise = legalBeforeAction.find((action) => action.type === "raise");
    expect(raise?.minTotalSats).toBe(200);

    state = applyHoldemAction(state, 0, { type: "raise", totalSats: 250 });
    expect(state.currentBetSats).toBe(250);
    expect(state.minRaiseToSats).toBe(400);

    state = applyHoldemAction(state, 1, { type: "call" });
    expect(state.phase).toBe("flop");
    expect(state.board).toHaveLength(3);
  });

  it("settles folded pots immediately", () => {
    let state = startHand();
    state = applyHoldemAction(state, 0, { type: "fold" });

    expect(state.phase).toBe("settled");
    expect(state.winners[0]?.playerId).toBe("beta");
    expect(state.players[1]?.stackSats).toBe(2050);
  });

  it("splits tied pots at showdown", () => {
    let state = startHand("cd".repeat(32));
    state.players[0]!.holeCards = ["Ah", "Kd"];
    state.players[1]!.holeCards = ["As", "Kc"];
    state.runout = {
      flop: ["Qh", "Jh", "Th"],
      turn: "2c",
      river: "3d",
    };

    state = applyHoldemAction(state, 0, { type: "call" });
    state = applyHoldemAction(state, 1, { type: "check" });
    state = applyHoldemAction(state, 1, { type: "check" });
    state = applyHoldemAction(state, 0, { type: "check" });
    state = applyHoldemAction(state, 1, { type: "check" });
    state = applyHoldemAction(state, 0, { type: "check" });
    state = applyHoldemAction(state, 1, { type: "check" });
    state = applyHoldemAction(state, 0, { type: "check" });

    expect(state.phase).toBe("settled");
    expect(state.winners).toHaveLength(2);
    expect(state.winners[0]?.amountSats).toBe(100);
    expect(state.winners[1]?.amountSats).toBe(100);
  });
});

describe("hand evaluator", () => {
  it("scores a straight flush above two pair", () => {
    const straightFlush = scoreSevenCardHand(["Ah", "Kh", "Qh", "Jh", "Th", "2c", "3d"]);
    const twoPair = scoreSevenCardHand(["Ah", "Ad", "Kh", "Kd", "2c", "3d", "4s"]);

    expect(straightFlush.category).toBeGreaterThan(twoPair.category);
  });
});
