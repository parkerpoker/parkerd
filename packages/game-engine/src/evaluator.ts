import type { CardCode } from "@parker/protocol";

import { parseCard, type Card } from "./cards.js";

export interface HandScore {
  category: number;
  rankValues: number[];
  bestFive: CardCode[];
  label: string;
}

const HAND_LABELS = [
  "high-card",
  "pair",
  "two-pair",
  "three-of-a-kind",
  "straight",
  "flush",
  "full-house",
  "four-of-a-kind",
  "straight-flush",
] as const;

function compareRankArrays(left: number[], right: number[]): number {
  const maxLength = Math.max(left.length, right.length);
  for (let index = 0; index < maxLength; index += 1) {
    const leftValue = left[index] ?? 0;
    const rightValue = right[index] ?? 0;
    if (leftValue > rightValue) {
      return 1;
    }
    if (leftValue < rightValue) {
      return -1;
    }
  }
  return 0;
}

function sortCardsDesc(cards: Card[]): Card[] {
  return [...cards].sort((left, right) => right.rankValue - left.rankValue);
}

function getStraightHigh(sortedDistinctRanksDesc: number[]): number | null {
  const ranks = [...sortedDistinctRanksDesc];
  if (ranks[0] === 14) {
    ranks.push(1);
  }

  let run = 1;
  let bestHigh: number | null = null;

  for (let index = 0; index < ranks.length - 1; index += 1) {
    if (ranks[index] === ranks[index + 1]! + 1) {
      run += 1;
      if (run >= 5) {
        bestHigh = ranks[index - 3]!;
      }
    } else {
      run = 1;
    }
  }

  return bestHigh;
}

function compareFiveCardHands(left: HandScore, right: HandScore): number {
  if (left.category > right.category) {
    return 1;
  }
  if (left.category < right.category) {
    return -1;
  }
  return compareRankArrays(left.rankValues, right.rankValues);
}

function evaluateFiveCardHand(cards: Card[]): HandScore {
  const sorted = sortCardsDesc(cards);
  const suits = new Set(sorted.map((card) => card.suit));
  const isFlush = suits.size === 1;
  const counts = new Map<number, number>();

  for (const card of sorted) {
    counts.set(card.rankValue, (counts.get(card.rankValue) ?? 0) + 1);
  }

  const entries = [...counts.entries()].sort((left, right) => {
    if (right[1] !== left[1]) {
      return right[1] - left[1];
    }
    return right[0] - left[0];
  });

  const distinctRanksDesc = [...counts.keys()].sort((left, right) => right - left);
  const straightHigh = getStraightHigh(distinctRanksDesc);

  if (isFlush && straightHigh) {
    return {
      category: 8,
      rankValues: [straightHigh],
      bestFive: sorted.map((card) => card.code),
      label: HAND_LABELS[8],
    };
  }

  if (entries[0]?.[1] === 4) {
    const quad = entries[0][0];
    const kicker = entries[1]![0];
    return {
      category: 7,
      rankValues: [quad, kicker],
      bestFive: sorted.map((card) => card.code),
      label: HAND_LABELS[7],
    };
  }

  if (entries[0]?.[1] === 3 && entries[1]?.[1] === 2) {
    return {
      category: 6,
      rankValues: [entries[0][0], entries[1][0]],
      bestFive: sorted.map((card) => card.code),
      label: HAND_LABELS[6],
    };
  }

  if (isFlush) {
    return {
      category: 5,
      rankValues: sorted.map((card) => card.rankValue),
      bestFive: sorted.map((card) => card.code),
      label: HAND_LABELS[5],
    };
  }

  if (straightHigh) {
    return {
      category: 4,
      rankValues: [straightHigh],
      bestFive: sorted.map((card) => card.code),
      label: HAND_LABELS[4],
    };
  }

  if (entries[0]?.[1] === 3) {
    const kickers = entries.slice(1).map(([rank]) => rank).sort((left, right) => right - left);
    return {
      category: 3,
      rankValues: [entries[0][0], ...kickers],
      bestFive: sorted.map((card) => card.code),
      label: HAND_LABELS[3],
    };
  }

  if (entries[0]?.[1] === 2 && entries[1]?.[1] === 2) {
    const pairs = [entries[0][0], entries[1][0]].sort((left, right) => right - left);
    const kicker = entries[2]![0];
    return {
      category: 2,
      rankValues: [...pairs, kicker],
      bestFive: sorted.map((card) => card.code),
      label: HAND_LABELS[2],
    };
  }

  if (entries[0]?.[1] === 2) {
    const kickers = entries.slice(1).map(([rank]) => rank).sort((left, right) => right - left);
    return {
      category: 1,
      rankValues: [entries[0][0], ...kickers],
      bestFive: sorted.map((card) => card.code),
      label: HAND_LABELS[1],
    };
  }

  return {
    category: 0,
    rankValues: sorted.map((card) => card.rankValue),
    bestFive: sorted.map((card) => card.code),
    label: HAND_LABELS[0],
  };
}

function combinations<T>(items: T[], size: number): T[][] {
  if (size === 0) {
    return [[]];
  }
  if (items.length < size || items.length === 0) {
    return [];
  }

  const head = items[0]!;
  const tail = items.slice(1);
  return [
    ...combinations(tail, size - 1).map((combination) => [head, ...combination]),
    ...combinations(tail, size),
  ];
}

export function scoreSevenCardHand(cardCodes: CardCode[]): HandScore {
  if (cardCodes.length !== 7) {
    throw new Error(`expected 7 cards, received ${cardCodes.length}`);
  }

  let best: HandScore | null = null;

  for (const candidate of combinations(cardCodes.map(parseCard), 5)) {
    const score = evaluateFiveCardHand(candidate);
    if (!best || compareFiveCardHands(score, best) > 0) {
      best = score;
    }
  }

  if (!best) {
    throw new Error("could not score seven card hand");
  }

  return best;
}

export function compareScoredHands(left: HandScore, right: HandScore): number {
  return compareFiveCardHands(left, right);
}
