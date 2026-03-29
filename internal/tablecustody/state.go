package tablecustody

import (
	"fmt"
	"sort"
)

func BuildState(binding StateBinding, balances []PlayerBalance, previous *CustodyState) (CustodyState, error) {
	if binding.TableID == "" {
		return CustodyState{}, fmt.Errorf("custody state is missing table id")
	}
	stackClaims := make([]StackClaim, 0, len(balances))
	participants := make([]SliceParticipant, 0, len(balances))
	totalStacks := 0
	totalContributions := 0
	for _, balance := range balances {
		if balance.PlayerID == "" {
			return CustodyState{}, fmt.Errorf("custody state is missing player id")
		}
		if balance.SeatIndex < 0 {
			return CustodyState{}, fmt.Errorf("custody state seat index for %s is invalid", balance.PlayerID)
		}
		if balance.StackSats < 0 || balance.TotalContributionSats < 0 || balance.RoundContributionSats < 0 {
			return CustodyState{}, fmt.Errorf("custody state balance for %s cannot be negative", balance.PlayerID)
		}
		stackClaims = append(stackClaims, StackClaim{
			AllIn:                 balance.AllIn,
			AmountSats:            balance.StackSats,
			Folded:                balance.Folded,
			PlayerID:              balance.PlayerID,
			RoundContributionSats: balance.RoundContributionSats,
			SeatIndex:             balance.SeatIndex,
			Status:                balance.Status,
			TotalContributionSats: balance.TotalContributionSats,
			VTXORefs:              append([]VTXORef(nil), balance.VTXORefs...),
		})
		participants = append(participants, SliceParticipant{
			ContributionSats: balance.TotalContributionSats,
			Folded:           balance.Folded,
			PlayerID:         balance.PlayerID,
			SeatIndex:        balance.SeatIndex,
		})
		totalStacks += balance.StackSats
		totalContributions += balance.TotalContributionSats
	}
	potSlices, err := DerivePotSlices(participants)
	if err != nil {
		return CustodyState{}, err
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
		PotSlices:        potSlices,
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
	if err := validateConservedValue(state, totalStacks+totalContributions); err != nil {
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
	totalPot := 0
	for _, stack := range state.StackClaims {
		if stack.PlayerID == "" {
			return fmt.Errorf("custody stack claim is missing player id")
		}
		if _, ok := seenPlayers[stack.PlayerID]; ok {
			return fmt.Errorf("duplicate custody stack claim for %s", stack.PlayerID)
		}
		seenPlayers[stack.PlayerID] = struct{}{}
		if stack.AmountSats < 0 || stack.TotalContributionSats < 0 || stack.RoundContributionSats < 0 {
			return fmt.Errorf("custody stack claim for %s cannot be negative", stack.PlayerID)
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
			sum += amount
		}
		if sum != slice.TotalSats {
			return fmt.Errorf("custody pot slice %s total mismatch", slice.PotID)
		}
		totalPot += slice.TotalSats
	}
	if expected := HashCustodyState(state); state.StateHash != "" && state.StateHash != expected {
		return fmt.Errorf("custody state hash mismatch")
	}
	if totalPot != totalPotFromClaims(state.StackClaims) {
		return fmt.Errorf("custody pot slices do not match contribution totals")
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

func totalPotFromClaims(claims []StackClaim) int {
	total := 0
	for _, claim := range claims {
		total += claim.TotalContributionSats
	}
	return total
}

func validateConservedValue(state CustodyState, total int) error {
	current := totalPotFromClaims(state.StackClaims)
	for _, claim := range state.StackClaims {
		current += claim.AmountSats
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
