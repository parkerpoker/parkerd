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

func TestTranscriptHashingAndMentalReplay(t *testing.T) {
	aliceKey, err := GenerateMentalKeyPair()
	if err != nil {
		t.Fatalf("generate alice key: %v", err)
	}
	bobKey, err := GenerateMentalKeyPair()
	if err != nil {
		t.Fatalf("generate bob key: %v", err)
	}

	transcript := HandTranscript{
		HandID:     "770e8400-e29b-41d4-a716-446655440000",
		HandNumber: 7,
		TableID:    "4187c6da-b781-48cc-a5b6-40df6d44c96f",
	}
	aliceSeat := 0
	bobSeat := 1
	aliceSeed := strings.Repeat("11", 32)
	bobSeed := strings.Repeat("22", 32)
	aliceCommitment, err := BuildFairnessCommitment(transcript.TableID, transcript.HandNumber, aliceSeat, "alice", string(StreetCommitment), aliceSeed, aliceKey.PublicExponentHex)
	if err != nil {
		t.Fatalf("build alice commitment: %v", err)
	}
	bobCommitment, err := BuildFairnessCommitment(transcript.TableID, transcript.HandNumber, bobSeat, "bob", string(StreetCommitment), bobSeed, bobKey.PublicExponentHex)
	if err != nil {
		t.Fatalf("build bob commitment: %v", err)
	}

	transcript, _, err = AppendTranscriptRecord(transcript, HandTranscriptRecord{
		CommitmentHash: aliceCommitment,
		Kind:           "fairness-commit",
		Phase:          string(StreetCommitment),
		PlayerID:       "alice",
		SeatIndex:      &aliceSeat,
	})
	if err != nil {
		t.Fatalf("append alice commit: %v", err)
	}
	transcript, _, err = AppendTranscriptRecord(transcript, HandTranscriptRecord{
		CommitmentHash: bobCommitment,
		Kind:           "fairness-commit",
		Phase:          string(StreetCommitment),
		PlayerID:       "bob",
		SeatIndex:      &bobSeat,
	})
	if err != nil {
		t.Fatalf("append bob commit: %v", err)
	}

	replay, err := ReplayMentalDeck([]MentalDeckReveal{
		{PlayerID: "alice", SeatIndex: aliceSeat, ShuffleSeedHex: aliceSeed, LockPublicExponentHex: aliceKey.PublicExponentHex},
		{PlayerID: "bob", SeatIndex: bobSeat, ShuffleSeedHex: bobSeed, LockPublicExponentHex: bobKey.PublicExponentHex},
	})
	if err != nil {
		t.Fatalf("replay mental deck: %v", err)
	}
	transcript, _, err = AppendTranscriptRecord(transcript, HandTranscriptRecord{
		DeckStage:             replay.RevealStagesBySeat[aliceSeat],
		DeckStageRoot:         replay.RevealStageRoots[0],
		Kind:                  "fairness-reveal",
		LockPublicExponentHex: aliceKey.PublicExponentHex,
		Phase:                 string(StreetReveal),
		PlayerID:              "alice",
		SeatIndex:             &aliceSeat,
		ShuffleSeedHex:        aliceSeed,
	})
	if err != nil {
		t.Fatalf("append alice reveal: %v", err)
	}
	transcript, _, err = AppendTranscriptRecord(transcript, HandTranscriptRecord{
		DeckStage:             replay.RevealStagesBySeat[bobSeat],
		DeckStageRoot:         replay.RevealStageRoots[1],
		Kind:                  "fairness-reveal",
		LockPublicExponentHex: bobKey.PublicExponentHex,
		Phase:                 string(StreetReveal),
		PlayerID:              "bob",
		SeatIndex:             &bobSeat,
		ShuffleSeedHex:        bobSeed,
	})
	if err != nil {
		t.Fatalf("append bob reveal: %v", err)
	}

	root, err := ReplayTranscriptRoot(transcript)
	if err != nil {
		t.Fatalf("replay transcript root: %v", err)
	}
	if root == "" || root != transcript.RootHash {
		t.Fatalf("unexpected transcript root: replay=%q transcript=%q", root, transcript.RootHash)
	}

	plan, err := HoldemDealPositions(0)
	if err != nil {
		t.Fatalf("deal positions: %v", err)
	}
	alicePositions := plan.HoleCardPositionsBySeat[aliceSeat]
	if len(alicePositions) != 2 {
		t.Fatalf("expected two alice hole-card positions, got %v", alicePositions)
	}
	bobPartial, err := DecryptMentalValueHex(replay.FinalDeck[alicePositions[0]], bobKey.PrivateExponentHex)
	if err != nil {
		t.Fatalf("bob partial decrypt: %v", err)
	}
	alicePlain, err := DecryptMentalValueHex(bobPartial, aliceKey.PrivateExponentHex)
	if err != nil {
		t.Fatalf("alice full decrypt: %v", err)
	}
	card, err := DecodeMentalCardHex(alicePlain)
	if err != nil {
		t.Fatalf("decode alice card: %v", err)
	}
	if _, ok := mentalCardValueByCode[card]; !ok {
		t.Fatalf("expected a valid hole card, got %q", card)
	}
}

func TestHoldemHandTransitions(t *testing.T) {
	state, err := CreateHoldemHand(HoldemHandConfig{
		HandID:          "770e8400-e29b-41d4-a716-446655440000",
		HandNumber:      1,
		DealerSeatIndex: 0,
		SmallBlindSats:  50,
		BigBlindSats:    100,
		Seats: []HoldemSeatConfig{
			{PlayerID: "alpha", StackSats: 2000},
			{PlayerID: "beta", StackSats: 2000},
		},
	})
	if err != nil {
		t.Fatalf("create hand: %v", err)
	}
	if state.Phase != StreetCommitment {
		t.Fatalf("expected commitment phase, got %s", state.Phase)
	}

	state, err = ActivateHoldemHand(state)
	if err != nil {
		t.Fatalf("activate hand: %v", err)
	}
	legal := GetLegalActions(state, intPtr(0))
	if len(legal) != 3 || legal[0].Type != ActionFold || legal[1].Type != ActionCall || legal[2].Type != ActionRaise {
		t.Fatalf("unexpected legal actions: %#v", legal)
	}
	if legal[2].MinTotalSats == nil || *legal[2].MinTotalSats != 200 {
		t.Fatalf("unexpected raise minimum: %#v", legal[2])
	}

	state, err = ApplyHoldemAction(state, 0, Action{Type: ActionRaise, TotalSats: 250})
	if err != nil {
		t.Fatalf("raise: %v", err)
	}
	state, err = ApplyHoldemAction(state, 1, Action{Type: ActionCall})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if state.Phase != StreetFlopReveal {
		t.Fatalf("expected flop reveal, got %s", state.Phase)
	}

	state, err = ApplyBoardCards(state, []CardCode{"7c", "4d", "8d"})
	if err != nil {
		t.Fatalf("apply flop: %v", err)
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
		DealerSeatIndex: 0,
		SmallBlindSats:  50,
		BigBlindSats:    100,
		Seats: []HoldemSeatConfig{
			{PlayerID: "alpha", StackSats: 2000},
			{PlayerID: "beta", StackSats: 2000},
		},
	})
	if err != nil {
		t.Fatalf("create hand: %v", err)
	}
	state, err = ForceFoldSeat(state, 0)
	if err != nil {
		t.Fatalf("fold: %v", err)
	}
	if state.Phase != StreetSettled || len(state.Winners) != 1 || state.Winners[0].PlayerID != "beta" || state.Players[1].StackSats != 2050 {
		t.Fatalf("unexpected fold settlement: %#v", state)
	}
	if state.PotSats != 0 || state.Players[0].TotalContributionSats != 0 || state.Players[1].TotalContributionSats != 0 {
		t.Fatalf("expected fold settlement to clear contributions: %#v", state)
	}

	state, err = CreateHoldemHand(HoldemHandConfig{
		HandID:          "770e8400-e29b-41d4-a716-446655440000",
		HandNumber:      1,
		DealerSeatIndex: 0,
		SmallBlindSats:  50,
		BigBlindSats:    100,
		Seats: []HoldemSeatConfig{
			{PlayerID: "alpha", StackSats: 2000},
			{PlayerID: "beta", StackSats: 2000},
		},
	})
	if err != nil {
		t.Fatalf("create hand: %v", err)
	}
	state, err = ActivateHoldemHand(state)
	if err != nil {
		t.Fatalf("activate hand: %v", err)
	}

	state, err = ApplyHoldemAction(state, 0, Action{Type: ActionCall})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	state, err = ApplyHoldemAction(state, 1, Action{Type: ActionCheck})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	state, err = ApplyBoardCards(state, []CardCode{"Qh", "Jh", "Th"})
	if err != nil {
		t.Fatalf("flop reveal: %v", err)
	}
	state, err = ApplyHoldemAction(state, 1, Action{Type: ActionCheck})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	state, err = ApplyHoldemAction(state, 0, Action{Type: ActionCheck})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	state, err = ApplyBoardCards(state, []CardCode{"2c"})
	if err != nil {
		t.Fatalf("turn reveal: %v", err)
	}
	state, err = ApplyHoldemAction(state, 1, Action{Type: ActionCheck})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	state, err = ApplyHoldemAction(state, 0, Action{Type: ActionCheck})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	state, err = ApplyBoardCards(state, []CardCode{"3d"})
	if err != nil {
		t.Fatalf("river reveal: %v", err)
	}
	state, err = ApplyHoldemAction(state, 1, Action{Type: ActionCheck})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	state, err = ApplyHoldemAction(state, 0, Action{Type: ActionCheck})
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if state.Phase != StreetShowdownReveal {
		t.Fatalf("expected showdown reveal, got %s", state.Phase)
	}

	state, err = SettleHoldemShowdown(state, map[string][2]CardCode{
		"alpha": {"Ah", "Kd"},
		"beta":  {"As", "Kc"},
	})
	if err != nil {
		t.Fatalf("settle showdown: %v", err)
	}
	if state.Phase != StreetSettled || len(state.Winners) != 2 || state.Winners[0].AmountSats != 100 || state.Winners[1].AmountSats != 100 {
		t.Fatalf("unexpected showdown outcome: %#v", state)
	}
	if state.PotSats != 0 || state.Players[0].TotalContributionSats != 0 || state.Players[1].TotalContributionSats != 0 {
		t.Fatalf("expected showdown settlement to clear contributions: %#v", state)
	}
}

func TestApplyHoldemActionIncludesActingSeatNumberInTurnErrors(t *testing.T) {
	state, err := CreateHoldemHand(HoldemHandConfig{
		HandID:          "770e8400-e29b-41d4-a716-446655440001",
		HandNumber:      2,
		DealerSeatIndex: 0,
		SmallBlindSats:  50,
		BigBlindSats:    100,
		Seats: []HoldemSeatConfig{
			{PlayerID: "alpha", StackSats: 2000},
			{PlayerID: "beta", StackSats: 2000},
		},
	})
	if err != nil {
		t.Fatalf("create hand: %v", err)
	}
	state, err = ActivateHoldemHand(state)
	if err != nil {
		t.Fatalf("activate hand: %v", err)
	}

	_, err = ApplyHoldemAction(state, 1, Action{Type: ActionCheck})
	if err == nil {
		t.Fatal("expected out-of-turn action to fail")
	}
	if got := err.Error(); got != "seat 1 cannot act while seat 0 is up" {
		t.Fatalf("unexpected out-of-turn error %q", got)
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
