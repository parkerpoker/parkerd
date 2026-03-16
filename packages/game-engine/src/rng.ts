import { sha256 } from "@noble/hashes/sha2";
import { bytesToHex, utf8ToBytes } from "@noble/hashes/utils";

import type { DeckCommitment } from "@parker/protocol";

import { createDeck, type Card } from "./cards.js";

export function sha256Hex(input: string): string {
  return bytesToHex(sha256(utf8ToBytes(input)));
}

function nextRandomFraction(seedHex: string, counter: number): number {
  const hash = sha256(`${seedHex}:${counter}`);
  const view = new DataView(hash.buffer, hash.byteOffset, hash.byteLength);
  return view.getUint32(0) / 0x1_0000_0000;
}

export function createDeterministicDeck(seedHex: string): Card[] {
  const deck = createDeck();
  for (let index = deck.length - 1; index > 0; index -= 1) {
    const randomIndex = Math.floor(nextRandomFraction(seedHex, index) * (index + 1));
    const current = deck[index]!;
    deck[index] = deck[randomIndex]!;
    deck[randomIndex] = current;
  }
  return deck;
}

export function buildCommitmentHash(args: {
  tableId: string;
  seatIndex: number;
  playerId: string;
  seedHex: string;
}): string {
  return sha256Hex(`${args.tableId}:${args.seatIndex}:${args.playerId}:${args.seedHex}`);
}

export function deriveDeckSeed(args: {
  tableId: string;
  handNumber: number;
  commitments: Pick<DeckCommitment, "seatIndex" | "playerId" | "commitmentHash">[];
  reveals: Pick<DeckCommitment, "seatIndex" | "playerId" | "commitmentHash" | "revealSeed">[];
}): string {
  const verifiedReveals = args.reveals.map((reveal) => {
    if (!reveal.revealSeed) {
      throw new Error(`missing reveal seed for seat ${reveal.seatIndex}`);
    }

    const derived = buildCommitmentHash({
      tableId: args.tableId,
      seatIndex: reveal.seatIndex,
      playerId: reveal.playerId,
      seedHex: reveal.revealSeed,
    });

    if (derived !== reveal.commitmentHash) {
      throw new Error(`reveal does not match commitment for seat ${reveal.seatIndex}`);
    }

    return `${reveal.seatIndex}:${reveal.playerId}:${reveal.commitmentHash}:${reveal.revealSeed}`;
  });

  const commitments = [...args.commitments]
    .sort((left, right) => left.seatIndex - right.seatIndex)
    .map((commitment) => `${commitment.seatIndex}:${commitment.playerId}:${commitment.commitmentHash}`);

  return sha256Hex(
    [args.tableId, args.handNumber, ...commitments, ...verifiedReveals.sort()].join("|"),
  );
}
