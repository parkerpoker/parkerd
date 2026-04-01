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

func HashCustodyRequest(transition CustodyTransition) string {
	unsigned := canonicalTransition(transition)
	unsigned.Approvals = nil
	unsigned.ArkIntentID = ""
	unsigned.ArkTxID = ""
	unsigned.Proof.ArkIntentID = ""
	unsigned.Proof.ArkTxID = ""
	unsigned.Proof.ExitProofRef = ""
	unsigned.Proof.FinalizedAt = ""
	unsigned.Proof.RequestHash = ""
	unsigned.Proof.ReplayValidated = false
	unsigned.Proof.SettlementWitness = nil
	unsigned.Proof.StateHash = ""
	unsigned.Proof.Signatures = nil
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

func mustJSON(value any) []byte {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return data
}
