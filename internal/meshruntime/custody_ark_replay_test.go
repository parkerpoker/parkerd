package meshruntime

import (
	"errors"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/parkerpoker/parkerd/internal/game"
	"github.com/parkerpoker/parkerd/internal/tablecustody"
)

func syntheticRealAcceptedTableForTest(t *testing.T) (*meshRuntime, *meshRuntime, nativeTableState, int) {
	t.Helper()

	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)
	enableSyntheticRealMode(host, guest)

	waitForLocalCanAct(t, []*meshRuntime{host, guest}, host, tableID)
	if _, err := host.SendAction(tableID, game.Action{Type: game.ActionCall}); err != nil {
		t.Fatalf("host send synthetic-real action: %v", err)
	}
	table := waitForActionLogLength(t, []*meshRuntime{host, guest}, guest, tableID, 1)
	return host, guest, table, len(table.CustodyTransitions) - 1
}

func tamperAcceptedCustodyTransitionForTest(table nativeTableState, transitionIndex int, mutate func(*tablecustody.CustodyTransition)) nativeTableState {
	tampered := cloneJSON(table)
	transition := cloneJSON(tampered.CustodyTransitions[transitionIndex])
	mutate(&transition)
	recomputeCustodyTransitionHashesForTest(&transition)
	tampered.CustodyTransitions[transitionIndex] = transition
	latest := cloneJSON(tampered.CustodyTransitions[len(tampered.CustodyTransitions)-1].NextState)
	tampered.LatestCustodyState = &latest
	return tampered
}

func mustDecodePSBTForReplayTest(t *testing.T, encoded string) *psbt.Packet {
	t.Helper()

	packet, err := psbt.NewFromRawBytes(strings.NewReader(encoded), true)
	if err != nil {
		t.Fatalf("decode psbt: %v", err)
	}
	return packet
}

func mustEncodePSBTForReplayTest(t *testing.T, packet *psbt.Packet) string {
	t.Helper()

	encoded, err := packet.B64Encode()
	if err != nil {
		t.Fatalf("encode psbt: %v", err)
	}
	return encoded
}

func TestAcceptedCustodyHistoryReplaysSettlementWitnessOffline(t *testing.T) {
	_, guest, table, _ := syntheticRealAcceptedTableForTest(t)

	arkVerifyCalls := 0
	guest.custodyArkVerify = func(refs []tablecustody.VTXORef, requireSpendable bool) error {
		arkVerifyCalls++
		return errors.New("ark unavailable")
	}
	if err := guest.validateAcceptedCustodyHistory(nil, table); err != nil {
		t.Fatalf("expected intact settlement witness to replay offline, got %v", err)
	}
	if arkVerifyCalls != 0 {
		t.Fatalf("expected accepted replay to avoid live Ark verification, got %d calls", arkVerifyCalls)
	}
}

func TestAcceptedCustodyHistoryRejectsMissingSettlementWitness(t *testing.T) {
	_, guest, table, transitionIndex := syntheticRealAcceptedTableForTest(t)

	tampered := tamperAcceptedCustodyTransitionForTest(table, transitionIndex, func(transition *tablecustody.CustodyTransition) {
		transition.Proof.SettlementWitness = nil
	})
	if err := guest.validateAcceptedCustodyHistory(nil, tampered); err == nil || !strings.Contains(err.Error(), "settlement witness is missing") {
		t.Fatalf("expected missing settlement witness to be rejected, got %v", err)
	}
}

func TestAcceptedCustodyHistoryRejectsTamperedSettlementWitnessProofPSBT(t *testing.T) {
	_, guest, table, transitionIndex := syntheticRealAcceptedTableForTest(t)

	testCases := []struct {
		name          string
		expectedError string
		mutate        func(t *testing.T, witness *tablecustody.CustodySettlementWitness)
	}{
		{
			name:          "inputs",
			expectedError: "input",
			mutate: func(t *testing.T, witness *tablecustody.CustodySettlementWitness) {
				packet := mustDecodePSBTForReplayTest(t, witness.ProofPSBT)
				packet.UnsignedTx.TxIn[1].PreviousOutPoint.Hash[0] ^= 0x01
				witness.ProofPSBT = mustEncodePSBTForReplayTest(t, packet)
			},
		},
		{
			name:          "outputs",
			expectedError: "output",
			mutate: func(t *testing.T, witness *tablecustody.CustodySettlementWitness) {
				packet := mustDecodePSBTForReplayTest(t, witness.ProofPSBT)
				packet.UnsignedTx.TxOut[0].Value++
				witness.ProofPSBT = mustEncodePSBTForReplayTest(t, packet)
			},
		},
		{
			name:          "locktime",
			expectedError: "locktime",
			mutate: func(t *testing.T, witness *tablecustody.CustodySettlementWitness) {
				packet := mustDecodePSBTForReplayTest(t, witness.ProofPSBT)
				packet.UnsignedTx.LockTime++
				witness.ProofPSBT = mustEncodePSBTForReplayTest(t, packet)
			},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			tampered := tamperAcceptedCustodyTransitionForTest(table, transitionIndex, func(transition *tablecustody.CustodyTransition) {
				testCase.mutate(t, transition.Proof.SettlementWitness)
			})
			if err := guest.validateAcceptedCustodyHistory(nil, tampered); err == nil || !strings.Contains(strings.ToLower(err.Error()), testCase.expectedError) {
				t.Fatalf("expected tampered proof psbt %s to be rejected, got %v", testCase.name, err)
			}
		})
	}
}

func TestAcceptedCustodyHistoryRejectsTamperedSettlementWitnessArtifacts(t *testing.T) {
	_, guest, table, transitionIndex := syntheticRealAcceptedTableForTest(t)

	testCases := []struct {
		name   string
		mutate func(t *testing.T, witness *tablecustody.CustodySettlementWitness)
	}{
		{
			name: "commitment-tx",
			mutate: func(t *testing.T, witness *tablecustody.CustodySettlementWitness) {
				packet := mustDecodePSBTForReplayTest(t, witness.CommitmentTx)
				packet.UnsignedTx.TxOut[0].Value++
				witness.CommitmentTx = mustEncodePSBTForReplayTest(t, packet)
			},
		},
		{
			name: "batch-expiry",
			mutate: func(t *testing.T, witness *tablecustody.CustodySettlementWitness) {
				witness.BatchExpiryValue++
			},
		},
		{
			name: "vtxo-tree",
			mutate: func(t *testing.T, witness *tablecustody.CustodySettlementWitness) {
				packet := mustDecodePSBTForReplayTest(t, witness.VtxoTree[0].Tx)
				packet.UnsignedTx.TxOut[0].Value++
				witness.VtxoTree[0].Tx = mustEncodePSBTForReplayTest(t, packet)
			},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			tampered := tamperAcceptedCustodyTransitionForTest(table, transitionIndex, func(transition *tablecustody.CustodyTransition) {
				testCase.mutate(t, transition.Proof.SettlementWitness)
			})
			if err := guest.validateAcceptedCustodyHistory(nil, tampered); err == nil {
				t.Fatalf("expected tampered settlement witness %s to be rejected", testCase.name)
			}
		})
	}
}

func TestAcceptedCustodyHistoryRejectsWitnessDerivedRefMismatches(t *testing.T) {
	_, guest, table, transitionIndex := syntheticRealAcceptedTableForTest(t)

	t.Run("next-state", func(t *testing.T) {
		tampered := tamperAcceptedCustodyTransitionForTest(table, transitionIndex, func(transition *tablecustody.CustodyTransition) {
			transition.NextState.StackClaims[0].VTXORefs[0].TxID = strings.Repeat("f", 64)
		})
		if err := guest.validateAcceptedCustodyHistory(nil, tampered); err == nil || !strings.Contains(err.Error(), "stack ref") {
			t.Fatalf("expected witness/next-state ref mismatch to be rejected, got %v", err)
		}
	})

	t.Run("proof-refs", func(t *testing.T) {
		tampered := tamperAcceptedCustodyTransitionForTest(table, transitionIndex, func(transition *tablecustody.CustodyTransition) {
			transition.Proof.VTXORefs[0].TxID = strings.Repeat("e", 64)
		})
		if err := guest.validateAcceptedCustodyHistory(nil, tampered); err == nil || !strings.Contains(err.Error(), "stack ref") {
			t.Fatalf("expected witness/proof ref mismatch to be rejected, got %v", err)
		}
	})
}

func TestAcceptedCustodyHistoryRejectsSettlementWitnessSummaryMismatches(t *testing.T) {
	_, guest, table, transitionIndex := syntheticRealAcceptedTableForTest(t)

	testCases := []struct {
		name   string
		mutate func(*tablecustody.CustodyTransition)
	}{
		{
			name: "ark-intent-id",
			mutate: func(transition *tablecustody.CustodyTransition) {
				transition.Proof.ArkIntentID = "wrong-intent"
			},
		},
		{
			name: "ark-txid",
			mutate: func(transition *tablecustody.CustodyTransition) {
				transition.Proof.ArkTxID = strings.Repeat("1", 64)
			},
		},
		{
			name: "finalized-at",
			mutate: func(transition *tablecustody.CustodyTransition) {
				transition.Proof.FinalizedAt = "2026-04-01T00:00:00Z"
			},
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			tampered := tamperAcceptedCustodyTransitionForTest(table, transitionIndex, testCase.mutate)
			if err := guest.validateAcceptedCustodyHistory(nil, tampered); err == nil || (!strings.Contains(strings.ToLower(err.Error()), "intent") && !strings.Contains(strings.ToLower(err.Error()), "txid") && !strings.Contains(strings.ToLower(err.Error()), "finalized")) {
				t.Fatalf("expected witness summary mismatch %s to be rejected, got %v", testCase.name, err)
			}
		})
	}
}

func TestEmergencyExitAcceptedHistoryStillSucceedsWithoutSettlementWitness(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createJoinedTwoPlayerTable(t, host, guest)
	enableSyntheticRealMode(host, guest)

	if _, err := guest.Exit(tableID); err != nil {
		t.Fatalf("guest emergency exit: %v", err)
	}

	table := mustReadNativeTable(t, host, tableID)
	transition := table.CustodyTransitions[len(table.CustodyTransitions)-1]
	if transition.Kind != tablecustody.TransitionKindEmergencyExit {
		t.Fatalf("expected emergency exit transition, got %s", transition.Kind)
	}
	if transition.Proof.SettlementWitness != nil {
		t.Fatal("expected emergency exit to keep the execution-proof path without a settlement witness")
	}
	if err := host.validateAcceptedCustodyHistory(nil, table); err != nil {
		t.Fatalf("expected emergency-exit history to replay without a settlement witness, got %v", err)
	}
}
