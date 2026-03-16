import type { CardCode, Street } from "@parker/protocol";

import { createDeterministicDeck } from "./rng.js";
import { compareScoredHands, scoreSevenCardHand, type HandScore } from "./evaluator.js";

export interface HoldemSeatConfig {
  playerId: string;
  stackSats: number;
}

export interface HoldemPlayerState extends HoldemSeatConfig {
  seatIndex: number;
  status: "active" | "folded" | "all-in";
  holeCards: [CardCode, CardCode];
  roundContributionSats: number;
  totalContributionSats: number;
  actedThisRound: boolean;
}

export type HoldemAction =
  | { type: "fold" }
  | { type: "check" }
  | { type: "call" }
  | { type: "bet"; totalSats: number }
  | { type: "raise"; totalSats: number };

export interface HoldemWinner {
  playerId: string;
  seatIndex: number;
  amountSats: number;
  handScore?: HandScore;
}

export interface HoldemState {
  handId: string;
  handNumber: number;
  phase: Street;
  dealerSeatIndex: number;
  actingSeatIndex: number | null;
  smallBlindSats: number;
  bigBlindSats: number;
  currentBetSats: number;
  minRaiseToSats: number;
  lastFullRaiseSats: number;
  raiseLockedSeatIndex: number | null;
  potSats: number;
  board: CardCode[];
  runout: {
    flop: [CardCode, CardCode, CardCode];
    turn: CardCode;
    river: CardCode;
  };
  deckSeedHex: string;
  players: [HoldemPlayerState, HoldemPlayerState];
  winners: HoldemWinner[];
  showdownScores: Record<string, HandScore>;
  actionLog: Array<{
    actorPlayerId: string;
    action: HoldemAction;
    phase: Street;
  }>;
}

export interface HoldemHandConfig {
  handId: string;
  handNumber: number;
  deckSeedHex: string;
  dealerSeatIndex: 0 | 1;
  smallBlindSats: number;
  bigBlindSats: number;
  seats: [HoldemSeatConfig, HoldemSeatConfig];
}

export interface LegalAction {
  type: HoldemAction["type"];
  minTotalSats?: number;
  maxTotalSats?: number;
}

function buildHoleCards(deck: CardCode[], dealerSeatIndex: number): [CardCode, CardCode][] {
  const seats: [CardCode[], CardCode[]] = [[], []];
  let dealSeat = dealerSeatIndex;
  let deckIndex = 0;

  for (let round = 0; round < 2; round += 1) {
    for (let seatOffset = 0; seatOffset < 2; seatOffset += 1) {
      seats[dealSeat]!.push(deck[deckIndex]!);
      deckIndex += 1;
      dealSeat = dealSeat === 0 ? 1 : 0;
    }
  }

  return seats as [[CardCode, CardCode], [CardCode, CardCode]];
}

function buildRunout(deck: CardCode[]) {
  return {
    flop: [deck[5]!, deck[6]!, deck[7]!] as [CardCode, CardCode, CardCode],
    turn: deck[9]!,
    river: deck[11]!,
  };
}

function cloneState(state: HoldemState): HoldemState {
  return {
    ...state,
    board: [...state.board],
    runout: {
      flop: [...state.runout.flop] as [CardCode, CardCode, CardCode],
      turn: state.runout.turn,
      river: state.runout.river,
    },
    players: state.players.map((player) => ({ ...player })) as [HoldemPlayerState, HoldemPlayerState],
    winners: state.winners.map((winner) => ({ ...winner })),
    showdownScores: { ...state.showdownScores },
    actionLog: state.actionLog.map((entry) => ({ ...entry, action: { ...entry.action } as HoldemAction })),
  };
}

function nextSeat(seatIndex: number): 0 | 1 {
  return seatIndex === 0 ? 1 : 0;
}

function getActivePlayers(state: HoldemState): HoldemPlayerState[] {
  return state.players.filter((player) => player.status !== "folded");
}

function getPlayersAbleToAct(state: HoldemState): HoldemPlayerState[] {
  return state.players.filter((player) => player.status === "active");
}

function firstToActForStreet(state: HoldemState): 0 | 1 {
  return nextSeat(state.dealerSeatIndex);
}

function recomputePot(players: HoldemPlayerState[]): number {
  return players.reduce((total, player) => total + player.totalContributionSats, 0);
}

function awardPot(state: HoldemState, winners: HoldemWinner[]) {
  const baseShare = Math.floor(state.potSats / winners.length);
  let remainder = state.potSats % winners.length;

  for (const winner of winners) {
    const seat = state.players[winner.seatIndex]!;
    const share = baseShare + (remainder > 0 ? 1 : 0);
    remainder = Math.max(0, remainder - 1);
    seat.stackSats += share;
    winner.amountSats = share;
  }

  state.winners = winners;
  state.phase = "settled";
  state.actingSeatIndex = null;
}

function settleByFold(state: HoldemState) {
  const remaining = getActivePlayers(state);
  if (remaining.length !== 1) {
    throw new Error("fold settlement requires exactly one remaining player");
  }

  const folded = state.players.find((player) => player.status === "folded");
  if (folded && remaining[0]!.totalContributionSats > folded.totalContributionSats) {
    const unmatched = remaining[0]!.totalContributionSats - folded.totalContributionSats;
    remaining[0]!.stackSats += unmatched;
    remaining[0]!.totalContributionSats -= unmatched;
    remaining[0]!.roundContributionSats = Math.min(
      remaining[0]!.roundContributionSats,
      folded.totalContributionSats,
    );
    state.potSats = recomputePot(state.players);
  }

  awardPot(state, [
    {
      playerId: remaining[0]!.playerId,
      seatIndex: remaining[0]!.seatIndex,
      amountSats: 0,
    },
  ]);
}

function settleShowdown(state: HoldemState) {
  const contenders = getActivePlayers(state);
  const scores = contenders.map((player) => {
    const score = scoreSevenCardHand([...player.holeCards, ...state.board]);
    state.showdownScores[player.playerId] = score;
    return {
      player,
      score,
    };
  });

  const best = scores.reduce((current, contender) => {
    if (!current) {
      return contender;
    }
    return compareScoredHands(contender.score, current.score) > 0 ? contender : current;
  }, null as (typeof scores)[number] | null);

  if (!best) {
    throw new Error("showdown requires at least one contender");
  }

  const winners = scores
    .filter((entry) => compareScoredHands(entry.score, best.score) === 0)
    .map((entry) => ({
      playerId: entry.player.playerId,
      seatIndex: entry.player.seatIndex,
      amountSats: 0,
      handScore: entry.score,
    }));

  awardPot(state, winners);
}

function advanceStreet(state: HoldemState) {
  for (const player of state.players) {
    player.roundContributionSats = 0;
    player.actedThisRound = player.status !== "active";
  }

  state.currentBetSats = 0;
  state.lastFullRaiseSats = state.bigBlindSats;
  state.minRaiseToSats = state.bigBlindSats;
  state.raiseLockedSeatIndex = null;

  switch (state.phase) {
    case "preflop":
      state.phase = "flop";
      state.board = [...state.runout.flop];
      break;
    case "flop":
      state.phase = "turn";
      state.board = [...state.board, state.runout.turn];
      break;
    case "turn":
      state.phase = "river";
      state.board = [...state.board, state.runout.river];
      break;
    case "river":
      state.phase = "showdown";
      settleShowdown(state);
      return;
    default:
      return;
  }

  const nextActor = state.players[firstToActForStreet(state)]!;
  state.actingSeatIndex = nextActor.status === "active" ? nextActor.seatIndex : null;
}

function closeActionRoundIfNeeded(state: HoldemState) {
  if (getActivePlayers(state).length === 1) {
    settleByFold(state);
    return;
  }

  const ableToAct = getPlayersAbleToAct(state);
  if (ableToAct.length === 0) {
    if (state.phase === "preflop") {
      state.board = [...state.runout.flop, state.runout.turn, state.runout.river];
    } else if (state.phase === "flop") {
      state.board = [...state.board, state.runout.turn, state.runout.river];
    } else if (state.phase === "turn") {
      state.board = [...state.board, state.runout.river];
    }
    state.phase = "showdown";
    settleShowdown(state);
    return;
  }

  const roundComplete = ableToAct.every(
    (player) => player.actedThisRound && player.roundContributionSats === state.currentBetSats,
  );

  if (roundComplete) {
    advanceStreet(state);
    return;
  }

  const nextActor = ableToAct.find((player) => !player.actedThisRound || player.roundContributionSats !== state.currentBetSats);
  state.actingSeatIndex = nextActor?.seatIndex ?? null;
}

export function createHoldemHand(config: HoldemHandConfig): HoldemState {
  if (config.seats.length !== 2) {
    throw new Error("Parker MVP supports exactly two seats");
  }

  const deck = createDeterministicDeck(config.deckSeedHex).map((card) => card.code);
  const holeCards = buildHoleCards(deck, config.dealerSeatIndex);
  const runout = buildRunout(deck);
  const smallBlindSeat = config.dealerSeatIndex;
  const bigBlindSeat = nextSeat(config.dealerSeatIndex);

  const players = config.seats.map((seat, seatIndex) => ({
    ...seat,
    seatIndex,
    status: "active" as const,
    holeCards: holeCards[seatIndex]!,
    roundContributionSats: 0,
    totalContributionSats: 0,
    actedThisRound: false,
  })) as [HoldemPlayerState, HoldemPlayerState];

  const smallBlindPlayer = players[smallBlindSeat]!;
  const bigBlindPlayer = players[bigBlindSeat]!;
  const committedSmallBlind = Math.min(smallBlindPlayer.stackSats, config.smallBlindSats);
  const committedBigBlind = Math.min(bigBlindPlayer.stackSats, config.bigBlindSats);

  smallBlindPlayer.stackSats -= committedSmallBlind;
  smallBlindPlayer.roundContributionSats = committedSmallBlind;
  smallBlindPlayer.totalContributionSats = committedSmallBlind;
  smallBlindPlayer.status = smallBlindPlayer.stackSats === 0 ? "all-in" : "active";

  bigBlindPlayer.stackSats -= committedBigBlind;
  bigBlindPlayer.roundContributionSats = committedBigBlind;
  bigBlindPlayer.totalContributionSats = committedBigBlind;
  bigBlindPlayer.status = bigBlindPlayer.stackSats === 0 ? "all-in" : "active";

  return {
    handId: config.handId,
    handNumber: config.handNumber,
    phase: "preflop",
    dealerSeatIndex: config.dealerSeatIndex,
    actingSeatIndex: smallBlindSeat,
    smallBlindSats: config.smallBlindSats,
    bigBlindSats: config.bigBlindSats,
    currentBetSats: committedBigBlind,
    minRaiseToSats: committedBigBlind + config.bigBlindSats,
    lastFullRaiseSats: config.bigBlindSats,
    raiseLockedSeatIndex: null,
    potSats: committedSmallBlind + committedBigBlind,
    board: [],
    runout,
    deckSeedHex: config.deckSeedHex,
    players,
    winners: [],
    showdownScores: {},
    actionLog: [],
  };
}

export function getLegalActions(state: HoldemState, seatIndex = state.actingSeatIndex): LegalAction[] {
  if (seatIndex === null) {
    return [];
  }

  const player = state.players[seatIndex];
  if (!player || player.status !== "active") {
    return [];
  }

  const toCall = Math.max(0, state.currentBetSats - player.roundContributionSats);
  const maxTotalSats = player.roundContributionSats + player.stackSats;

  if (toCall === 0) {
    const actions: LegalAction[] = [{ type: "check" }];
    if (player.stackSats > 0) {
      actions.push({
        type: "bet",
        minTotalSats: Math.max(state.bigBlindSats, 1),
        maxTotalSats,
      });
    }
    return actions;
  }

  const actions: LegalAction[] = [{ type: "fold" }, { type: "call" }];
  if (player.stackSats > toCall && state.raiseLockedSeatIndex !== seatIndex) {
    actions.push({
      type: "raise",
      minTotalSats: Math.min(state.minRaiseToSats, maxTotalSats),
      maxTotalSats,
    });
  }

  return actions;
}

function expectLegal(state: HoldemState, seatIndex: number, action: HoldemAction) {
  const legalActions = getLegalActions(state, seatIndex);
  const legal = legalActions.find((candidate) => candidate.type === action.type);
  if (!legal) {
    throw new Error(`illegal ${action.type} action for seat ${seatIndex}`);
  }

  if ("totalSats" in action) {
    if (action.totalSats < (legal.minTotalSats ?? action.totalSats)) {
      throw new Error(`action total ${action.totalSats} is below minimum ${legal.minTotalSats}`);
    }
    if (action.totalSats > (legal.maxTotalSats ?? action.totalSats)) {
      throw new Error(`action total ${action.totalSats} exceeds maximum ${legal.maxTotalSats}`);
    }
  }
}

export function applyHoldemAction(
  state: HoldemState,
  seatIndex: number,
  action: HoldemAction,
): HoldemState {
  if (state.phase === "settled") {
    throw new Error("hand already settled");
  }
  if (state.actingSeatIndex !== seatIndex) {
    throw new Error(`seat ${seatIndex} cannot act while seat ${state.actingSeatIndex} is up`);
  }

  expectLegal(state, seatIndex, action);

  const next = cloneState(state);
  const player = next.players[seatIndex]!;
  const opponent = next.players[nextSeat(seatIndex)]!;
  next.actionLog.push({
    actorPlayerId: player.playerId,
    action,
    phase: next.phase,
  });

  switch (action.type) {
    case "fold":
      player.status = "folded";
      player.actedThisRound = true;
      break;
    case "check":
      player.actedThisRound = true;
      break;
    case "call": {
      const toCall = Math.max(0, next.currentBetSats - player.roundContributionSats);
      const paid = Math.min(toCall, player.stackSats);
      player.stackSats -= paid;
      player.roundContributionSats += paid;
      player.totalContributionSats += paid;
      player.actedThisRound = true;
      if (player.stackSats === 0) {
        player.status = "all-in";
      }
      break;
    }
    case "bet": {
      const paid = action.totalSats - player.roundContributionSats;
      player.stackSats -= paid;
      player.roundContributionSats = action.totalSats;
      player.totalContributionSats += paid;
      player.actedThisRound = true;
      next.currentBetSats = action.totalSats;
      next.lastFullRaiseSats = action.totalSats;
      next.minRaiseToSats = action.totalSats + next.lastFullRaiseSats;
      next.raiseLockedSeatIndex = null;
      opponent.actedThisRound = false;
      if (player.stackSats === 0) {
        player.status = "all-in";
      }
      break;
    }
    case "raise": {
      const paid = action.totalSats - player.roundContributionSats;
      const raiseSize = action.totalSats - next.currentBetSats;
      player.stackSats -= paid;
      player.roundContributionSats = action.totalSats;
      player.totalContributionSats += paid;
      player.actedThisRound = true;
      next.currentBetSats = action.totalSats;
      if (raiseSize >= next.lastFullRaiseSats) {
        next.lastFullRaiseSats = raiseSize;
        next.raiseLockedSeatIndex = null;
      } else if (player.stackSats === 0) {
        next.raiseLockedSeatIndex = opponent.seatIndex;
      }
      next.minRaiseToSats = next.currentBetSats + next.lastFullRaiseSats;
      opponent.actedThisRound = false;
      if (player.stackSats === 0) {
        player.status = "all-in";
      }
      break;
    }
  }

  next.potSats = recomputePot(next.players);
  closeActionRoundIfNeeded(next);
  return next;
}

export function toCheckpointShape(state: HoldemState) {
  return {
    phase: state.phase,
    actingSeatIndex: state.actingSeatIndex,
    dealerSeatIndex: state.dealerSeatIndex,
    board: state.board,
    playerStacks: Object.fromEntries(state.players.map((player) => [player.playerId, player.stackSats])),
    roundContributions: Object.fromEntries(
      state.players.map((player) => [player.playerId, player.roundContributionSats]),
    ),
    totalContributions: Object.fromEntries(
      state.players.map((player) => [player.playerId, player.totalContributionSats]),
    ),
    potSats: state.potSats,
    currentBetSats: state.currentBetSats,
    minRaiseToSats: state.minRaiseToSats,
    holeCardsByPlayerId: Object.fromEntries(
      state.players.map((player) => [player.playerId, [...player.holeCards]]),
    ),
  };
}
