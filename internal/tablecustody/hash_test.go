package tablecustody

import (
	"testing"

	arktree "github.com/arkade-os/arkd/pkg/ark-lib/tree"
)

func TestCustodySettlementWitnessHashing(t *testing.T) {
	initial, err := BuildState(StateBinding{
		CreatedAt:     "2026-04-01T00:00:00Z",
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
	transition, err := BuildTransition(TransitionKindAction, StateBinding{
		CreatedAt:        "2026-04-01T00:00:01Z",
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

	transition.Proof.SettlementWitness = &CustodySettlementWitness{
		ArkIntentID:      "intent-1",
		ArkTxID:          "tx-1",
		FinalizedAt:      "2026-04-01T00:00:02Z",
		ProofPSBT:        "proof-1",
		CommitmentTx:     "commitment-1",
		BatchExpiryType:  "blocks",
		BatchExpiryValue: 144,
		VtxoTree: arktree.FlatTxTree{{
			Txid: "node-1",
			Tx:   "tx-1",
		}},
	}

	requestHash := HashCustodyRequest(transition)
	finalHash := HashCustodyTransition(transition)

	transition.Proof.SettlementWitness = &CustodySettlementWitness{
		ArkIntentID:      "intent-1",
		ArkTxID:          "tx-1",
		FinalizedAt:      "2026-04-01T00:00:02Z",
		ProofPSBT:        "proof-2",
		CommitmentTx:     "commitment-2",
		BatchExpiryType:  "blocks",
		BatchExpiryValue: 288,
		VtxoTree: arktree.FlatTxTree{{
			Txid: "node-2",
			Tx:   "tx-2",
		}},
	}

	if nextRequestHash := HashCustodyRequest(transition); nextRequestHash != requestHash {
		t.Fatalf("expected request hash to ignore settlement witness, got %s want %s", nextRequestHash, requestHash)
	}
	if nextFinalHash := HashCustodyTransition(transition); nextFinalHash == finalHash {
		t.Fatal("expected transition hash to change when settlement witness changes")
	}
}
