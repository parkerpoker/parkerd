package tablecustody

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"

	arktree "github.com/arkade-os/arkd/pkg/ark-lib/tree"
)

func HashValue(value any) string {
	sum := sha256.Sum256(mustJSON(value))
	return hex.EncodeToString(sum[:])
}

func HashLegalActions(actions any) string {
	return HashValue(actions)
}

func HashPublicState(value any) string {
	return HashValue(value)
}

func HashCustodyState(state CustodyState) string {
	unsigned := canonicalState(state)
	unsigned.StateHash = ""
	return HashValue(unsigned)
}

func HashCustodyTransition(transition CustodyTransition) string {
	unsigned := canonicalTransition(transition)
	unsigned.Proof.TransitionHash = ""
	return HashValue(unsigned)
}

func HashCustodyRecoveryBundle(bundle CustodyRecoveryBundle) string {
	canonical := canonicalRecoveryBundle(bundle)
	canonical.BundleHash = ""
	return HashValue(canonical)
}

func HashCustodyChallengeBundle(bundle CustodyChallengeBundle) string {
	canonical := canonicalChallengeBundle(bundle)
	canonical.BundleHash = ""
	return HashValue(canonical)
}

func HashCustodyRequest(transition CustodyTransition) string {
	unsigned := canonicalTransition(transition)
	unsigned.Approvals = nil
	unsigned.ArkIntentID = ""
	unsigned.ArkTxID = ""
	unsigned.Proof.CandidateIntentAck = nil
	unsigned.Proof.ChallengeBundle = nil
	unsigned.Proof.ChallengeWitness = nil
	unsigned.Proof.ArkIntentID = ""
	unsigned.Proof.ArkTxID = ""
	unsigned.Proof.ExitProofRef = ""
	unsigned.Proof.FinalizedAt = ""
	unsigned.Proof.RequestHash = ""
	unsigned.Proof.RecoveryBundles = nil
	unsigned.Proof.RecoveryWitness = nil
	unsigned.Proof.ReplayValidated = false
	unsigned.Proof.SettlementWitness = nil
	unsigned.Proof.StateHash = ""
	unsigned.Proof.Signatures = nil
	unsigned.Proof.TurnAnchorHash = ""
	unsigned.Proof.TurnCandidateHash = ""
	unsigned.Proof.TransitionHash = ""
	unsigned.Proof.VTXORefs = nil
	return HashValue(unsigned)
}

func canonicalState(state CustodyState) CustodyState {
	state.StackClaims = append([]StackClaim(nil), state.StackClaims...)
	sort.SliceStable(state.StackClaims, func(left, right int) bool {
		if state.StackClaims[left].SeatIndex != state.StackClaims[right].SeatIndex {
			return state.StackClaims[left].SeatIndex < state.StackClaims[right].SeatIndex
		}
		return state.StackClaims[left].PlayerID < state.StackClaims[right].PlayerID
	})
	for index := range state.StackClaims {
		state.StackClaims[index].VTXORefs = append([]VTXORef(nil), state.StackClaims[index].VTXORefs...)
		sort.SliceStable(state.StackClaims[index].VTXORefs, func(left, right int) bool {
			return compareVTXORefs(state.StackClaims[index].VTXORefs[left], state.StackClaims[index].VTXORefs[right]) < 0
		})
	}

	state.PotSlices = append([]PotSlice(nil), state.PotSlices...)
	sort.SliceStable(state.PotSlices, func(left, right int) bool {
		if state.PotSlices[left].Sequence != state.PotSlices[right].Sequence {
			return state.PotSlices[left].Sequence < state.PotSlices[right].Sequence
		}
		return state.PotSlices[left].PotID < state.PotSlices[right].PotID
	})
	for index := range state.PotSlices {
		state.PotSlices[index].EligiblePlayerIDs = sortedStrings(state.PotSlices[index].EligiblePlayerIDs)
		state.PotSlices[index].ContributedPlayerIDs = sortedStrings(state.PotSlices[index].ContributedPlayerIDs)
		state.PotSlices[index].WinnerPlayerIDs = sortedStrings(state.PotSlices[index].WinnerPlayerIDs)
		state.PotSlices[index].OddChipPlayerIDs = append([]string(nil), state.PotSlices[index].OddChipPlayerIDs...)
		state.PotSlices[index].VTXORefs = append([]VTXORef(nil), state.PotSlices[index].VTXORefs...)
		sort.SliceStable(state.PotSlices[index].VTXORefs, func(left, right int) bool {
			return compareVTXORefs(state.PotSlices[index].VTXORefs[left], state.PotSlices[index].VTXORefs[right]) < 0
		})
	}
	return state
}

func canonicalTransition(transition CustodyTransition) CustodyTransition {
	transition.NextState = canonicalState(transition.NextState)
	transition.Approvals = append([]CustodySignature(nil), transition.Approvals...)
	sort.SliceStable(transition.Approvals, func(left, right int) bool {
		return transition.Approvals[left].PlayerID < transition.Approvals[right].PlayerID
	})

	transition.Proof.Signatures = append([]CustodySignature(nil), transition.Proof.Signatures...)
	sort.SliceStable(transition.Proof.Signatures, func(left, right int) bool {
		return transition.Proof.Signatures[left].PlayerID < transition.Proof.Signatures[right].PlayerID
	})
	if transition.Proof.SettlementWitness != nil {
		canonicalWitness := canonicalSettlementWitness(*transition.Proof.SettlementWitness)
		transition.Proof.SettlementWitness = &canonicalWitness
	}
	transition.Proof.RecoveryBundles = append([]CustodyRecoveryBundle(nil), transition.Proof.RecoveryBundles...)
	for index := range transition.Proof.RecoveryBundles {
		transition.Proof.RecoveryBundles[index] = canonicalRecoveryBundle(transition.Proof.RecoveryBundles[index])
	}
	sort.SliceStable(transition.Proof.RecoveryBundles, func(left, right int) bool {
		leftBundle := transition.Proof.RecoveryBundles[left]
		rightBundle := transition.Proof.RecoveryBundles[right]
		if leftBundle.Kind != rightBundle.Kind {
			return leftBundle.Kind < rightBundle.Kind
		}
		if leftBundle.BundleHash != rightBundle.BundleHash {
			return leftBundle.BundleHash < rightBundle.BundleHash
		}
		return compareVTXORefSlices(leftBundle.SourcePotRefs, rightBundle.SourcePotRefs) < 0
	})
	if transition.Proof.RecoveryWitness != nil {
		canonicalWitness := canonicalRecoveryWitness(*transition.Proof.RecoveryWitness)
		transition.Proof.RecoveryWitness = &canonicalWitness
	}
	if transition.Proof.CandidateIntentAck != nil {
		canonicalAck := canonicalCandidateIntentAck(*transition.Proof.CandidateIntentAck)
		transition.Proof.CandidateIntentAck = &canonicalAck
	}
	if transition.Proof.ChallengeBundle != nil {
		canonicalBundle := canonicalChallengeBundle(*transition.Proof.ChallengeBundle)
		transition.Proof.ChallengeBundle = &canonicalBundle
	}
	if transition.Proof.ChallengeWitness != nil {
		canonicalWitness := canonicalChallengeWitness(*transition.Proof.ChallengeWitness)
		transition.Proof.ChallengeWitness = &canonicalWitness
	}
	transition.Proof.VTXORefs = append([]VTXORef(nil), transition.Proof.VTXORefs...)
	sort.SliceStable(transition.Proof.VTXORefs, func(left, right int) bool {
		return compareVTXORefs(transition.Proof.VTXORefs[left], transition.Proof.VTXORefs[right]) < 0
	})
	return transition
}

func canonicalSettlementWitness(witness CustodySettlementWitness) CustodySettlementWitness {
	witness.VtxoTree = canonicalFlatTxTree(witness.VtxoTree)
	witness.ConnectorTree = canonicalFlatTxTree(witness.ConnectorTree)
	return witness
}

func canonicalRecoveryBundle(bundle CustodyRecoveryBundle) CustodyRecoveryBundle {
	bundle.AuthorizedOutputs = append([]CustodyRecoveryOutput(nil), bundle.AuthorizedOutputs...)
	for index := range bundle.AuthorizedOutputs {
		bundle.AuthorizedOutputs[index].Tapscripts = append([]string(nil), bundle.AuthorizedOutputs[index].Tapscripts...)
		sort.Strings(bundle.AuthorizedOutputs[index].Tapscripts)
	}
	sort.SliceStable(bundle.AuthorizedOutputs, func(left, right int) bool {
		leftOutput := bundle.AuthorizedOutputs[left]
		rightOutput := bundle.AuthorizedOutputs[right]
		switch {
		case leftOutput.OwnerPlayerID != rightOutput.OwnerPlayerID:
			return leftOutput.OwnerPlayerID < rightOutput.OwnerPlayerID
		case leftOutput.AmountSats != rightOutput.AmountSats:
			return leftOutput.AmountSats < rightOutput.AmountSats
		default:
			return leftOutput.Script < rightOutput.Script
		}
	})
	bundle.SourcePotRefs = append([]VTXORef(nil), bundle.SourcePotRefs...)
	sort.SliceStable(bundle.SourcePotRefs, func(left, right int) bool {
		return compareVTXORefs(bundle.SourcePotRefs[left], bundle.SourcePotRefs[right]) < 0
	})
	if bundle.TimeoutResolution != nil {
		canonicalResolution := canonicalTimeoutResolution(*bundle.TimeoutResolution)
		bundle.TimeoutResolution = &canonicalResolution
	}
	return bundle
}

func canonicalRecoveryWitness(witness CustodyRecoveryWitness) CustodyRecoveryWitness {
	witness.BroadcastTxIDs = append([]string(nil), witness.BroadcastTxIDs...)
	sort.Strings(witness.BroadcastTxIDs)
	return witness
}

func canonicalChallengeBundle(bundle CustodyChallengeBundle) CustodyChallengeBundle {
	bundle.AuthorizedOutputs = append([]CustodyChallengeOutput(nil), bundle.AuthorizedOutputs...)
	for index := range bundle.AuthorizedOutputs {
		bundle.AuthorizedOutputs[index].Tapscripts = append([]string(nil), bundle.AuthorizedOutputs[index].Tapscripts...)
		sort.Strings(bundle.AuthorizedOutputs[index].Tapscripts)
	}
	sort.SliceStable(bundle.AuthorizedOutputs, func(left, right int) bool {
		leftOutput := bundle.AuthorizedOutputs[left]
		rightOutput := bundle.AuthorizedOutputs[right]
		switch {
		case leftOutput.ClaimKey != rightOutput.ClaimKey:
			return leftOutput.ClaimKey < rightOutput.ClaimKey
		case leftOutput.OwnerPlayerID != rightOutput.OwnerPlayerID:
			return leftOutput.OwnerPlayerID < rightOutput.OwnerPlayerID
		case leftOutput.AmountSats != rightOutput.AmountSats:
			return leftOutput.AmountSats < rightOutput.AmountSats
		default:
			return leftOutput.Script < rightOutput.Script
		}
	})
	bundle.SourceRefs = append([]VTXORef(nil), bundle.SourceRefs...)
	sort.SliceStable(bundle.SourceRefs, func(left, right int) bool {
		return compareVTXORefs(bundle.SourceRefs[left], bundle.SourceRefs[right]) < 0
	})
	if bundle.TimeoutResolution != nil {
		canonicalResolution := canonicalTimeoutResolution(*bundle.TimeoutResolution)
		bundle.TimeoutResolution = &canonicalResolution
	}
	return bundle
}

func canonicalChallengeWitness(witness CustodyChallengeWitness) CustodyChallengeWitness {
	witness.BroadcastTxIDs = append([]string(nil), witness.BroadcastTxIDs...)
	sort.Strings(witness.BroadcastTxIDs)
	return witness
}

func canonicalCandidateIntentAck(ack CandidateIntentAck) CandidateIntentAck {
	return ack
}

func canonicalTimeoutResolution(resolution TimeoutResolution) TimeoutResolution {
	resolution.DeadPlayerIDs = sortedStrings(resolution.DeadPlayerIDs)
	resolution.LostEligibilityPlayerIDs = sortedStrings(resolution.LostEligibilityPlayerIDs)
	return resolution
}

func canonicalFlatTxTree(tree arktree.FlatTxTree) arktree.FlatTxTree {
	if len(tree) == 0 {
		return nil
	}
	cloned := append(arktree.FlatTxTree(nil), tree...)
	for index := range cloned {
		if cloned[index].Children == nil {
			continue
		}
		children := make(map[uint32]string, len(cloned[index].Children))
		for outputIndex, childTxID := range cloned[index].Children {
			children[outputIndex] = childTxID
		}
		cloned[index].Children = children
	}
	sort.SliceStable(cloned, func(left, right int) bool {
		return cloned[left].Txid < cloned[right].Txid
	})
	return cloned
}

func sortedStrings(values []string) []string {
	cloned := append([]string(nil), values...)
	sort.Strings(cloned)
	return cloned
}

func compareVTXORefs(left, right VTXORef) int {
	if left.TxID < right.TxID {
		return -1
	}
	if left.TxID > right.TxID {
		return 1
	}
	if left.VOut < right.VOut {
		return -1
	}
	if left.VOut > right.VOut {
		return 1
	}
	return 0
}

func compareVTXORefSlices(left, right []VTXORef) int {
	leftRefs := append([]VTXORef(nil), left...)
	rightRefs := append([]VTXORef(nil), right...)
	sort.SliceStable(leftRefs, func(i, j int) bool {
		return compareVTXORefs(leftRefs[i], leftRefs[j]) < 0
	})
	sort.SliceStable(rightRefs, func(i, j int) bool {
		return compareVTXORefs(rightRefs[i], rightRefs[j]) < 0
	})
	limit := len(leftRefs)
	if len(rightRefs) < limit {
		limit = len(rightRefs)
	}
	for index := 0; index < limit; index++ {
		comparison := compareVTXORefs(leftRefs[index], rightRefs[index])
		if comparison != 0 {
			return comparison
		}
	}
	switch {
	case len(leftRefs) < len(rightRefs):
		return -1
	case len(leftRefs) > len(rightRefs):
		return 1
	default:
		return 0
	}
}

func mustJSON(value any) []byte {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return data
}
