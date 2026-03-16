import type { CardCode } from "@parker/protocol";

export const SUITS = ["c", "d", "h", "s"] as const;
export const RANKS = ["2", "3", "4", "5", "6", "7", "8", "9", "T", "J", "Q", "K", "A"] as const;

export type Suit = (typeof SUITS)[number];
export type Rank = (typeof RANKS)[number];

export interface Card {
  code: CardCode;
  rank: Rank;
  suit: Suit;
  rankValue: number;
}

const rankValueMap = new Map<Rank, number>(RANKS.map((rank, index) => [rank, index + 2]));

export function parseCard(code: CardCode): Card {
  const [rank, suit] = code.split("") as [Rank, Suit];
  return {
    code,
    rank,
    suit,
    rankValue: rankValueMap.get(rank) ?? 0,
  };
}

export function createDeck(): Card[] {
  return SUITS.flatMap((suit) =>
    RANKS.map((rank) => parseCard(`${rank}${suit}` as CardCode)),
  );
}

