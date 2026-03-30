package tablecustody

import "testing"

func TestDerivePotSlices(t *testing.T) {
	t.Parallel()

	derivation, err := DerivePotStructure([]SliceParticipant{
		{PlayerID: "a", SeatIndex: 0, ContributionSats: 1000},
		{PlayerID: "b", SeatIndex: 1, ContributionSats: 4000},
		{PlayerID: "c", SeatIndex: 2, ContributionSats: 4000, Folded: true},
		{PlayerID: "d", SeatIndex: 3, ContributionSats: 7000},
	})
	if err != nil {
		t.Fatalf("derive pot slices: %v", err)
	}
	slices := derivation.Slices
	if len(slices) != 2 {
		t.Fatalf("expected 2 matched pot slices, got %d", len(slices))
	}
	if slices[0].TotalSats != 4000 || len(slices[0].EligiblePlayerIDs) != 3 {
		t.Fatalf("unexpected main pot: %+v", slices[0])
	}
	if slices[1].TotalSats != 9000 || len(slices[1].EligiblePlayerIDs) != 2 {
		t.Fatalf("unexpected side pot 1: %+v", slices[1])
	}
	if derivation.UnmatchedContributionSats["d"] != 3000 {
		t.Fatalf("expected unmatched contribution for d, got %+v", derivation.UnmatchedContributionSats)
	}
}

func TestSplitAmountOddChipsFollowDealerOrder(t *testing.T) {
	t.Parallel()

	payouts, oddChips := SplitAmount(5, []PayoutCandidate{
		{PlayerID: "a", SeatIndex: 0},
		{PlayerID: "b", SeatIndex: 1},
	}, 0)
	if payouts["a"] != 3 || payouts["b"] != 2 {
		t.Fatalf("unexpected payouts: %+v", payouts)
	}
	if len(oddChips) != 1 || oddChips[0] != "a" {
		t.Fatalf("unexpected odd chip allocation: %v", oddChips)
	}
}

func TestBuildTransitionRejectsStaleState(t *testing.T) {
	t.Parallel()

	initial, err := BuildState(StateBinding{
		CreatedAt:     "2026-03-29T00:00:00Z",
		DecisionIndex: 0,
		Epoch:         1,
		HandID:        "hand-1",
		HandNumber:    1,
		TableID:       "table-1",
		TimeoutPolicy: TimeoutPolicyAutoCheckOrFold,
	}, []PlayerBalance{
		{PlayerID: "a", SeatIndex: 0, StackSats: 3900, TotalContributionSats: 100},
		{PlayerID: "b", SeatIndex: 1, StackSats: 3800, TotalContributionSats: 200},
	}, nil)
	if err != nil {
		t.Fatalf("build initial state: %v", err)
	}
	if initial.StateHash == "" {
		t.Fatal("expected initial state hash")
	}

	next, err := BuildTransition(TransitionKindAction, StateBinding{
		CreatedAt:        "2026-03-29T00:00:01Z",
		DecisionIndex:    1,
		Epoch:            1,
		HandID:           "hand-1",
		HandNumber:       1,
		TableID:          "table-1",
		ActingPlayerID:   "a",
		LegalActionsHash: "legal",
		PublicStateHash:  "public",
		TimeoutPolicy:    TimeoutPolicyAutoCheckOrFold,
		TranscriptRoot:   "root-1",
	}, []PlayerBalance{
		{PlayerID: "a", SeatIndex: 0, StackSats: 3600, TotalContributionSats: 400, RoundContributionSats: 400},
		{PlayerID: "b", SeatIndex: 1, StackSats: 3800, TotalContributionSats: 200, RoundContributionSats: 200},
	}, &initial, &ActionDescriptor{Type: "raise", TotalSats: 400}, nil)
	if err != nil {
		t.Fatalf("build transition: %v", err)
	}

	stale := initial
	stale.StateHash = "stale"
	if err := ValidateTransition(&stale, next); err == nil {
		t.Fatal("expected stale previous state to be rejected")
	}
}

func TestBuildStateProjectsUnmatchedContributionIntoStackClaim(t *testing.T) {
	t.Parallel()

	state, err := BuildState(StateBinding{
		CreatedAt:     "2026-03-29T00:00:00Z",
		DecisionIndex: 0,
		Epoch:         1,
		HandID:        "hand-1",
		HandNumber:    1,
		TableID:       "table-1",
		TimeoutPolicy: TimeoutPolicyAutoCheckOrFold,
	}, []PlayerBalance{
		{PlayerID: "small", SeatIndex: 0, StackSats: 3835, RoundContributionSats: 165, TotalContributionSats: 165},
		{PlayerID: "big", SeatIndex: 1, StackSats: 3670, RoundContributionSats: 330, TotalContributionSats: 330},
	}, nil)
	if err != nil {
		t.Fatalf("build state: %v", err)
	}
	if len(state.PotSlices) != 1 || state.PotSlices[0].TotalSats != 330 {
		t.Fatalf("expected one matched blind pot slice, got %+v", state.PotSlices)
	}
	if got := SortedStackClaims(state.StackClaims)[0].AmountSats; got != 3835 {
		t.Fatalf("unexpected small blind stack claim amount %d", got)
	}
	if got := SortedStackClaims(state.StackClaims)[1].AmountSats; got != 3835 {
		t.Fatalf("unexpected big blind stack claim amount %d", got)
	}
}
