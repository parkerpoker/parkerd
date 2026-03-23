package game

import (
	"strings"
	"testing"
)

func TestCreateDeterministicDeck(t *testing.T) {
	seed := strings.Repeat("ab", 32)
	deck := CreateDeterministicDeck(seed)
	if len(deck) != 52 {
		t.Fatalf("expected 52 cards, got %d", len(deck))
	}

	got := make([]CardCode, 0, 16)
	for _, card := range deck[:16] {
		got = append(got, card.Code)
	}
	want := []CardCode{"Jh", "9h", "Qh", "2h", "8h", "7c", "4d", "8d", "As", "6s", "5h", "Ah", "Kh", "3s", "6h", "9c"}
	if !slicesEqual(got, want) {
		t.Fatalf("unexpected deterministic deck prefix: got %v want %v", got, want)
	}
}

func TestCommitmentAndDeckSeed(t *testing.T) {
	tableID := "4187c6da-b781-48cc-a5b6-40df6d44c96f"
	alphaSeed := strings.Repeat("ab", 32)
	betaSeed := strings.Repeat("22", 32)

	alphaCommitment := BuildCommitmentHash(tableID, 0, "alpha-player", alphaSeed)
	betaCommitment := BuildCommitmentHash(tableID, 1, "beta-player", betaSeed)

	if alphaCommitment != "ce2e13dd2a2c72eee98c109604a18fc6b4230f21517dd9576e7337ac3773bb4a" {
		t.Fatalf("unexpected alpha commitment: %s", alphaCommitment)
	}

	seed, err := DeriveDeckSeed(
		tableID,
		1,
		[]DeckCommitment{
			{SeatIndex: 0, PlayerID: "alpha-player", CommitmentHash: alphaCommitment},
			{SeatIndex: 1, PlayerID: "beta-player", CommitmentHash: betaCommitment},
		},
		[]DeckCommitment{
			{SeatIndex: 0, PlayerID: "alpha-player", CommitmentHash: alphaCommitment, RevealSeed: alphaSeed},
			{SeatIndex: 1, PlayerID: "beta-player", CommitmentHash: betaCommitment, RevealSeed: betaSeed},
		},
	)
	if err != nil {
		t.Fatalf("derive deck seed: %v", err)
	}
	if seed != "be79df0d640cf833aa3c4ba56b1705ef311c74691e69d65aaa9f89c0865450af" {
		t.Fatalf("unexpected deck seed: %s", seed)
	}
}

func TestHoldemHandTransitions(t *testing.T) {
	state, err := CreateHoldemHand(HoldemHandConfig{
		HandID:          "770e8400-e29b-41d4-a716-446655440000",
		HandNumber:      1,
		DeckSeedHex:     strings.Repeat("ab", 32),
		DealerSeatIndex: 0,
		SmallBlindSats:  50,
		BigBlindSats:    100,
		Seats: [2]HoldemSeatConfig{
			{PlayerID: "alpha", StackSats: 2000},
			{PlayerID: "beta", StackSats: 2000},
		},
	})
	if err != nil {
		t.Fatalf("create hand: %v", err)
	}

	legal := GetLegalActions(state, intPtr(0))
	if len(legal) != 3 || legal[0].Type != ActionFold || legal[1].Type != ActionCall || legal[2].Type != ActionRaise {
		t.Fatalf("unexpected legal actions: %#v", legal)
	}
	if legal[2].MinTotalSats == nil || *legal[2].MinTotalSats != 200 {
		t.Fatalf("unexpected raise minimum: %#v", legal[2])
	}
	if state.Players[0].HoleCards != [2]CardCode{"Jh", "Qh"} || state.Players[1].HoleCards != [2]CardCode{"9h", "2h"} {
		t.Fatalf("unexpected hole cards: %#v", state.Players)
	}

	state, err = ApplyHoldemAction(state, 0, Action{Type: ActionRaise, TotalSats: 250})
	if err != nil {
		t.Fatalf("raise: %v", err)
	}
	state, err = ApplyHoldemAction(state, 1, Action{Type: ActionCall})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if state.Phase != StreetFlop || state.CurrentBetSats != 0 || state.MinRaiseToSats != 100 {
		t.Fatalf("unexpected post-flop state: %#v", state)
	}
	if len(state.Board) != 3 || state.Board[0] != "7c" || state.Board[1] != "4d" || state.Board[2] != "8d" {
		t.Fatalf("unexpected flop board: %#v", state.Board)
	}
	if state.Players[0].StackSats != 1750 || state.Players[1].StackSats != 1750 {
		t.Fatalf("unexpected stacks after action sequence: %#v", state.Players)
	}
}

func TestHoldemFoldAndShowdown(t *testing.T) {
	state, err := CreateHoldemHand(HoldemHandConfig{
		HandID:          "770e8400-e29b-41d4-a716-446655440000",
		HandNumber:      1,
		DeckSeedHex:     strings.Repeat("cd", 32),
		DealerSeatIndex: 0,
		SmallBlindSats:  50,
		BigBlindSats:    100,
		Seats: [2]HoldemSeatConfig{
			{PlayerID: "alpha", StackSats: 2000},
			{PlayerID: "beta", StackSats: 2000},
		},
	})
	if err != nil {
		t.Fatalf("create hand: %v", err)
	}
	state, err = ApplyHoldemAction(state, 0, Action{Type: ActionFold})
	if err != nil {
		t.Fatalf("fold: %v", err)
	}
	if state.Phase != StreetSettled || len(state.Winners) != 1 || state.Winners[0].PlayerID != "beta" || state.Players[1].StackSats != 2050 {
		t.Fatalf("unexpected fold settlement: %#v", state)
	}

	state, err = CreateHoldemHand(HoldemHandConfig{
		HandID:          "770e8400-e29b-41d4-a716-446655440000",
		HandNumber:      1,
		DeckSeedHex:     strings.Repeat("ab", 32),
		DealerSeatIndex: 0,
		SmallBlindSats:  50,
		BigBlindSats:    100,
		Seats: [2]HoldemSeatConfig{
			{PlayerID: "alpha", StackSats: 2000},
			{PlayerID: "beta", StackSats: 2000},
		},
	})
	if err != nil {
		t.Fatalf("create hand: %v", err)
	}
	state.Players[0].HoleCards = [2]CardCode{"Ah", "Kd"}
	state.Players[1].HoleCards = [2]CardCode{"As", "Kc"}
	state.Runout = HoldemRunout{
		Flop:  [3]CardCode{"Qh", "Jh", "Th"},
		Turn:  "2c",
		River: "3d",
	}

	state, err = ApplyHoldemAction(state, 0, Action{Type: ActionCall})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	state, err = ApplyHoldemAction(state, 1, Action{Type: ActionCheck})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	state, err = ApplyHoldemAction(state, 1, Action{Type: ActionCheck})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	state, err = ApplyHoldemAction(state, 0, Action{Type: ActionCheck})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	state, err = ApplyHoldemAction(state, 1, Action{Type: ActionCheck})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	state, err = ApplyHoldemAction(state, 0, Action{Type: ActionCheck})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	state, err = ApplyHoldemAction(state, 1, Action{Type: ActionCheck})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	state, err = ApplyHoldemAction(state, 0, Action{Type: ActionCheck})
	if err != nil {
		t.Fatalf("check: %v", err)
	}

	if state.Phase != StreetSettled || len(state.Winners) != 2 || state.Winners[0].AmountSats != 100 || state.Winners[1].AmountSats != 100 {
		t.Fatalf("unexpected showdown outcome: %#v", state)
	}
}

func TestScoreSevenCardHand(t *testing.T) {
	straightFlush, err := ScoreSevenCardHand([]CardCode{"Ah", "Kh", "Qh", "Jh", "Th", "2c", "3d"})
	if err != nil {
		t.Fatalf("score straight flush: %v", err)
	}
	twoPair, err := ScoreSevenCardHand([]CardCode{"Ah", "Ad", "Kh", "Kd", "2c", "3d", "4s"})
	if err != nil {
		t.Fatalf("score two pair: %v", err)
	}
	if straightFlush.Category <= twoPair.Category {
		t.Fatalf("expected straight flush to outrank two pair: %#v vs %#v", straightFlush, twoPair)
	}
}

func intPtr(value int) *int {
	return &value
}

func slicesEqual(left, right []CardCode) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
