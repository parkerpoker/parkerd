package tablecustody

import (
	"fmt"
	"sort"
)

func BuildState(binding StateBinding, balances []PlayerBalance, previous *CustodyState) (CustodyState, error) {
	if binding.TableID == "" {
		return CustodyState{}, fmt.Errorf("custody state is missing table id")
	}
	participants := make([]SliceParticipant, 0, len(balances))
	totalValue := 0
	for _, balance := range balances {
		if balance.PlayerID == "" {
			return CustodyState{}, fmt.Errorf("custody state is missing player id")
		}
		if balance.SeatIndex < 0 {
			return CustodyState{}, fmt.Errorf("custody state seat index for %s is invalid", balance.PlayerID)
		}
		if balance.StackSats < 0 || balance.ReservedFeeSats < 0 || balance.TotalContributionSats < 0 || balance.RoundContributionSats < 0 {
			return CustodyState{}, fmt.Errorf("custody state balance for %s cannot be negative", balance.PlayerID)
		}
		participants = append(participants, SliceParticipant{
			ContributionSats: balance.TotalContributionSats,
			Folded:           balance.Folded,
			PlayerID:         balance.PlayerID,
			SeatIndex:        balance.SeatIndex,
		})
		totalValue += balance.StackSats + balance.ReservedFeeSats + balance.TotalContributionSats
	}
	derivation, err := DerivePotStructure(participants)
	if err != nil {
		return CustodyState{}, err
	}
	stackClaims := make([]StackClaim, 0, len(balances))
	for _, balance := range balances {
		unmatchedContribution := derivation.UnmatchedContributionSats[balance.PlayerID]
		stackClaims = append(stackClaims, StackClaim{
			AllIn:                 balance.AllIn,
			AmountSats:            balance.StackSats + unmatchedContribution,
			Folded:                balance.Folded,
			PlayerID:              balance.PlayerID,
			ReservedFeeSats:       balance.ReservedFeeSats,
			RoundContributionSats: balance.RoundContributionSats,
			SeatIndex:             balance.SeatIndex,
			Status:                balance.Status,
			TotalContributionSats: balance.TotalContributionSats,
			VTXORefs:              append([]VTXORef(nil), balance.VTXORefs...),
		})
	}

	state := CustodyState{
		ActionDeadlineAt: binding.ActionDeadlineAt,
		ActingPlayerID:   binding.ActingPlayerID,
		ChallengeAnchor:  binding.ChallengeAnchor,
		CreatedAt:        binding.CreatedAt,
		CustodySeq:       nextCustodySeq(previous),
		DecisionIndex:    binding.DecisionIndex,
		Epoch:            binding.Epoch,
		HandID:           binding.HandID,
		HandNumber:       binding.HandNumber,
		LegalActionsHash: binding.LegalActionsHash,
		PotSlices:        derivation.Slices,
		PrevStateHash:    previousStateHash(previous),
		PublicStateHash:  binding.PublicStateHash,
		StackClaims:      stackClaims,
		TableID:          binding.TableID,
		TimeoutPolicy:    binding.TimeoutPolicy,
		TranscriptRoot:   binding.TranscriptRoot,
	}
	if err := ValidateState(state); err != nil {
		return CustodyState{}, err
	}
	state.StateHash = HashCustodyState(state)
	if err := validateConservedValue(state, totalValue); err != nil {
		return CustodyState{}, err
	}
	return state, nil
}

func BuildTransition(kind TransitionKind, binding StateBinding, balances []PlayerBalance, previous *CustodyState, action *ActionDescriptor, timeout *TimeoutResolution) (CustodyTransition, error) {
	nextState, err := BuildState(binding, balances, previous)
	if err != nil {
		return CustodyTransition{}, err
	}
	transition := CustodyTransition{
		Action:            action,
		ActingPlayerID:    binding.ActingPlayerID,
		CustodySeq:        nextState.CustodySeq,
		DecisionIndex:     binding.DecisionIndex,
		Kind:              kind,
		NextState:         nextState,
		NextStateHash:     nextState.StateHash,
		PrevStateHash:     nextState.PrevStateHash,
		TableID:           binding.TableID,
		TimeoutResolution: timeout,
	}
	transition.Proof = CustodyProof{
		ReplayValidated: true,
		StateHash:       nextState.StateHash,
	}
	transition.Proof.TransitionHash = HashCustodyTransition(transition)
	return transition, ValidateTransition(previous, transition)
}

func ValidateState(state CustodyState) error {
	if state.TableID == "" {
		return fmt.Errorf("custody state table id is required")
	}
	if len(state.StackClaims) == 0 {
		return fmt.Errorf("custody state must contain stack claims")
	}
	seenPlayers := map[string]struct{}{}
	matchedContributionByPlayer := map[string]int{}
	for _, stack := range state.StackClaims {
		if stack.PlayerID == "" {
			return fmt.Errorf("custody stack claim is missing player id")
		}
		if _, ok := seenPlayers[stack.PlayerID]; ok {
			return fmt.Errorf("duplicate custody stack claim for %s", stack.PlayerID)
		}
		seenPlayers[stack.PlayerID] = struct{}{}
		if stack.AmountSats < 0 || stack.ReservedFeeSats < 0 || stack.TotalContributionSats < 0 || stack.RoundContributionSats < 0 {
			return fmt.Errorf("custody stack claim for %s cannot be negative", stack.PlayerID)
		}
		if stack.RoundContributionSats > stack.TotalContributionSats {
			return fmt.Errorf("custody stack claim round contribution exceeds total for %s", stack.PlayerID)
		}
	}
	for _, slice := range state.PotSlices {
		if slice.TotalSats < 0 || slice.CapSats < 0 {
			return fmt.Errorf("custody pot slice %s cannot be negative", slice.PotID)
		}
		if len(slice.EligiblePlayerIDs) == 0 && slice.TotalSats > 0 {
			return fmt.Errorf("custody pot slice %s is missing eligible players", slice.PotID)
		}
		sum := 0
		for playerID, amount := range slice.Contributions {
			if amount < 0 {
				return fmt.Errorf("custody pot slice %s contribution for %s cannot be negative", slice.PotID, playerID)
			}
			if _, ok := seenPlayers[playerID]; !ok {
				return fmt.Errorf("custody pot slice %s references unknown player %s", slice.PotID, playerID)
			}
			sum += amount
			matchedContributionByPlayer[playerID] += amount
		}
		if sum != slice.TotalSats {
			return fmt.Errorf("custody pot slice %s total mismatch", slice.PotID)
		}
	}
	if expected := HashCustodyState(state); state.StateHash != "" && state.StateHash != expected {
		return fmt.Errorf("custody state hash mismatch")
	}
	for _, stack := range state.StackClaims {
		matchedContribution := matchedContributionByPlayer[stack.PlayerID]
		if matchedContribution > stack.TotalContributionSats {
			return fmt.Errorf("custody pot slices overclaim contributions for %s", stack.PlayerID)
		}
		if impliedUnmatched := stack.TotalContributionSats - matchedContribution; stack.AmountSats < impliedUnmatched {
			return fmt.Errorf("custody stack claim for %s is below unmatched contribution floor", stack.PlayerID)
		}
	}
	return nil
}

func ValidateTransition(previous *CustodyState, transition CustodyTransition) error {
	if transition.TableID == "" {
		return fmt.Errorf("custody transition table id is required")
	}
	if err := ValidateState(transition.NextState); err != nil {
		return err
	}
	if previous != nil {
		if transition.PrevStateHash != previous.StateHash {
			return fmt.Errorf("custody transition prev state hash mismatch")
		}
		if transition.CustodySeq <= previous.CustodySeq {
			return fmt.Errorf("custody transition seq must advance")
		}
		if transition.NextState.TableID != previous.TableID {
			return fmt.Errorf("custody transition table mismatch")
		}
	}
	if transition.NextStateHash != transition.NextState.StateHash {
		return fmt.Errorf("custody transition next state hash mismatch")
	}
	if transition.NextState.TableID != transition.TableID {
		return fmt.Errorf("custody transition next state table mismatch")
	}
	if transition.NextState.PrevStateHash != transition.PrevStateHash {
		return fmt.Errorf("custody transition next state prev hash mismatch")
	}
	if transition.NextState.CustodySeq != transition.CustodySeq {
		return fmt.Errorf("custody transition seq mismatch")
	}
	if transition.NextState.DecisionIndex != transition.DecisionIndex {
		return fmt.Errorf("custody transition decision mismatch")
	}
	if transition.ActingPlayerID != transition.NextState.ActingPlayerID {
		return fmt.Errorf("custody transition acting player mismatch")
	}
	if transition.Proof.StateHash != "" && transition.Proof.StateHash != transition.NextStateHash {
		return fmt.Errorf("custody proof state hash mismatch")
	}
	if transition.Proof.ArkIntentID != "" && transition.ArkIntentID != "" && transition.Proof.ArkIntentID != transition.ArkIntentID {
		return fmt.Errorf("custody proof intent mismatch")
	}
	if transition.Proof.ArkTxID != "" && transition.ArkTxID != "" && transition.Proof.ArkTxID != transition.ArkTxID {
		return fmt.Errorf("custody proof txid mismatch")
	}
	witnessKinds := 0
	if transition.Proof.SettlementWitness != nil {
		witnessKinds++
	}
	if transition.Proof.RecoveryWitness != nil {
		witnessKinds++
	}
	if transition.Proof.ChallengeWitness != nil {
		witnessKinds++
	}
	if transition.Proof.ExitWitness != nil {
		witnessKinds++
	}
	if witnessKinds > 1 {
		return fmt.Errorf("custody proof cannot carry multiple witness types")
	}
	if transition.Proof.SettlementWitness != nil {
		witness := transition.Proof.SettlementWitness
		if transition.ArkIntentID != "" && witness.ArkIntentID != transition.ArkIntentID {
			return fmt.Errorf("custody settlement witness intent mismatch")
		}
		if transition.Proof.ArkIntentID != "" && witness.ArkIntentID != transition.Proof.ArkIntentID {
			return fmt.Errorf("custody proof witness intent mismatch")
		}
		if transition.ArkTxID != "" && witness.ArkTxID != transition.ArkTxID {
			return fmt.Errorf("custody settlement witness txid mismatch")
		}
		if transition.Proof.ArkTxID != "" && witness.ArkTxID != transition.Proof.ArkTxID {
			return fmt.Errorf("custody proof witness txid mismatch")
		}
		if transition.Proof.FinalizedAt != "" && witness.FinalizedAt != transition.Proof.FinalizedAt {
			return fmt.Errorf("custody proof witness finalized timestamp mismatch")
		}
	}
	for index, bundle := range transition.Proof.RecoveryBundles {
		if bundle.Kind == "" {
			return fmt.Errorf("custody recovery bundle %d is missing a kind", index)
		}
		if len(bundle.SourcePotRefs) == 0 {
			return fmt.Errorf("custody recovery bundle %d is missing source pot refs", index)
		}
		if len(bundle.AuthorizedOutputs) == 0 {
			return fmt.Errorf("custody recovery bundle %d is missing authorized outputs", index)
		}
		if bundle.SignedPSBT == "" {
			return fmt.Errorf("custody recovery bundle %d is missing signed psbt", index)
		}
		if expected := HashCustodyRecoveryBundle(bundle); bundle.BundleHash != "" && bundle.BundleHash != expected {
			return fmt.Errorf("custody recovery bundle %d hash mismatch", index)
		}
	}
	if witness := transition.Proof.RecoveryWitness; witness != nil {
		if transition.Kind != TransitionKindTimeout && transition.Kind != TransitionKindShowdownPayout {
			return fmt.Errorf("custody recovery witness is only valid for timeout or showdown-payout transitions")
		}
		if witness.BundleHash == "" {
			return fmt.Errorf("custody recovery witness is missing bundle hash")
		}
		if witness.RecoveryTxID == "" {
			return fmt.Errorf("custody recovery witness is missing recovery txid")
		}
		if len(witness.BroadcastTxIDs) == 0 {
			return fmt.Errorf("custody recovery witness is missing broadcast txids")
		}
		if transition.Proof.FinalizedAt != "" && witness.ExecutedAt != "" && witness.ExecutedAt != transition.Proof.FinalizedAt {
			return fmt.Errorf("custody recovery witness finalized timestamp mismatch")
		}
		if transition.ArkIntentID != "" || transition.ArkTxID != "" || transition.Proof.ArkIntentID != "" || transition.Proof.ArkTxID != "" {
			return fmt.Errorf("custody recovery witness cannot coexist with Ark settlement ids")
		}
	}
	if bundle := transition.Proof.ChallengeBundle; bundle != nil {
		if bundle.Kind == "" {
			return fmt.Errorf("custody challenge bundle is missing a kind")
		}
		if len(bundle.SourceRefs) == 0 {
			return fmt.Errorf("custody challenge bundle is missing source refs")
		}
		if len(bundle.AuthorizedOutputs) == 0 {
			return fmt.Errorf("custody challenge bundle is missing authorized outputs")
		}
		if bundle.SignedPSBT == "" {
			return fmt.Errorf("custody challenge bundle is missing signed psbt")
		}
		if expected := HashCustodyChallengeBundle(*bundle); bundle.BundleHash != "" && bundle.BundleHash != expected {
			return fmt.Errorf("custody challenge bundle hash mismatch")
		}
	}
	if witness := transition.Proof.ChallengeWitness; witness != nil {
		if transition.Kind != TransitionKindAction &&
			transition.Kind != TransitionKindTimeout &&
			transition.Kind != TransitionKindTurnChallengeOpen &&
			transition.Kind != TransitionKindTurnChallengeEscape {
			return fmt.Errorf("custody challenge witness is only valid for action, timeout, turn-challenge-open, or turn-challenge-escape transitions")
		}
		if witness.BundleHash == "" {
			return fmt.Errorf("custody challenge witness is missing bundle hash")
		}
		if witness.TransactionID == "" {
			return fmt.Errorf("custody challenge witness is missing transaction id")
		}
		if len(witness.BroadcastTxIDs) == 0 {
			return fmt.Errorf("custody challenge witness is missing broadcast txids")
		}
		if transition.Proof.FinalizedAt != "" && witness.ExecutedAt != "" && witness.ExecutedAt != transition.Proof.FinalizedAt {
			return fmt.Errorf("custody challenge witness finalized timestamp mismatch")
		}
		if transition.ArkIntentID != "" || transition.ArkTxID != "" || transition.Proof.ArkIntentID != "" || transition.Proof.ArkTxID != "" {
			return fmt.Errorf("custody challenge witness cannot coexist with Ark settlement ids")
		}
		if transition.Proof.ChallengeBundle == nil {
			return fmt.Errorf("custody challenge witness is missing its bundle")
		}
		if transition.Proof.ChallengeBundle.BundleHash != "" && transition.Proof.ChallengeBundle.BundleHash != witness.BundleHash {
			return fmt.Errorf("custody challenge witness bundle hash mismatch")
		}
	}
	if witness := transition.Proof.ExitWitness; witness != nil {
		if transition.Kind != TransitionKindEmergencyExit {
			return fmt.Errorf("custody exit witness is only valid for emergency-exit transitions")
		}
		if len(witness.BroadcastTransactions) == 0 {
			return fmt.Errorf("custody exit witness is missing broadcast transactions")
		}
		seenBroadcast := map[string]struct{}{}
		for index, tx := range witness.BroadcastTransactions {
			if tx.TransactionID == "" {
				return fmt.Errorf("custody exit witness broadcast transaction %d is missing a txid", index)
			}
			if tx.TransactionHex == "" {
				return fmt.Errorf("custody exit witness broadcast transaction %d is missing tx hex", index)
			}
			if _, ok := seenBroadcast[tx.TransactionID]; ok {
				return fmt.Errorf("custody exit witness broadcast transaction %d is duplicated", index)
			}
			seenBroadcast[tx.TransactionID] = struct{}{}
		}
		if witness.SweepTransaction != nil {
			if witness.SweepTransaction.TransactionID == "" {
				return fmt.Errorf("custody exit witness sweep transaction is missing a txid")
			}
			if witness.SweepTransaction.TransactionHex == "" {
				return fmt.Errorf("custody exit witness sweep transaction is missing tx hex")
			}
			if _, ok := seenBroadcast[witness.SweepTransaction.TransactionID]; ok {
				return fmt.Errorf("custody exit witness sweep transaction duplicates a broadcast txid")
			}
		}
	}
	expectedHash := HashCustodyTransition(transition)
	if transition.Proof.TransitionHash != "" && transition.Proof.TransitionHash != expectedHash {
		return fmt.Errorf("custody transition hash mismatch")
	}
	return nil
}

func BuildTimeoutResolution(policy TimeoutPolicy, actingPlayerID string, legalActionTypes []string, contestedPlayerIDs []string) TimeoutResolution {
	resolution := TimeoutResolution{
		ActingPlayerID: actingPlayerID,
		Policy:         policy,
		Reason:         "action deadline expired",
	}
	autoCheck := false
	for _, actionType := range legalActionTypes {
		if actionType == "check" {
			autoCheck = true
			break
		}
	}
	if policy == TimeoutPolicyAutoCheckOrFold && autoCheck {
		resolution.ActionType = "check"
		return resolution
	}
	resolution.ActionType = "fold"
	resolution.LostEligibilityPlayerIDs = append([]string(nil), contestedPlayerIDs...)
	resolution.DeadPlayerIDs = append([]string(nil), contestedPlayerIDs...)
	return resolution
}

func BuildExitProofRef(state CustodyState, playerID string, refs []VTXORef, txids []string) string {
	return HashValue(map[string]any{
		"exitTxids": txids,
		"playerId":  playerID,
		"refs":      refs,
		"stateHash": state.StateHash,
		"tableId":   state.TableID,
	})
}

func totalPotFromSlices(slices []PotSlice) int {
	total := 0
	for _, slice := range slices {
		total += slice.TotalSats
	}
	return total
}

func validateConservedValue(state CustodyState, total int) error {
	current := totalPotFromSlices(state.PotSlices)
	for _, claim := range state.StackClaims {
		current += claim.AmountSats + claim.ReservedFeeSats
	}
	if current != total {
		return fmt.Errorf("custody value is not conserved")
	}
	return nil
}

func nextCustodySeq(previous *CustodyState) int {
	if previous == nil {
		return 1
	}
	return previous.CustodySeq + 1
}

func previousStateHash(previous *CustodyState) string {
	if previous == nil {
		return ""
	}
	return previous.StateHash
}

func SortedStackClaims(claims []StackClaim) []StackClaim {
	cloned := append([]StackClaim(nil), claims...)
	sort.SliceStable(cloned, func(left, right int) bool {
		if cloned[left].SeatIndex != cloned[right].SeatIndex {
			return cloned[left].SeatIndex < cloned[right].SeatIndex
		}
		return cloned[left].PlayerID < cloned[right].PlayerID
	})
	return cloned
}
