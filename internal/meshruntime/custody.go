package meshruntime

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/parkerpoker/parkerd/internal/game"
	"github.com/parkerpoker/parkerd/internal/settlementcore"
	"github.com/parkerpoker/parkerd/internal/tablecustody"
)

const defaultCustodyTimeoutPolicy = tablecustody.TimeoutPolicyAutoCheckOrFold

func latestCustodyStateHash(table nativeTableState) string {
	if table.LatestCustodyState == nil {
		return ""
	}
	return table.LatestCustodyState.StateHash
}

func (runtime *meshRuntime) publicMoneyStateHash(table nativeTableState, hand *game.HoldemState) string {
	payload := map[string]any{
		"tableId": table.Config.TableID,
	}
	if hand != nil {
		checkpoint := game.ToCheckpointShape(*hand)
		payload["checkpoint"] = checkpoint
		payload["handId"] = hand.HandID
		payload["handNumber"] = hand.HandNumber
	} else if table.PublicState != nil {
		payload["chipBalances"] = table.PublicState.ChipBalances
		payload["handId"] = table.PublicState.HandID
		payload["handNumber"] = table.PublicState.HandNumber
		payload["potSats"] = table.PublicState.PotSats
		payload["roundContributions"] = table.PublicState.RoundContributions
		payload["totalContributions"] = table.PublicState.TotalContributions
	}
	return tablecustody.HashPublicState(payload)
}

func (runtime *meshRuntime) legalActionsHash(hand *game.HoldemState) string {
	if hand == nil || !game.PhaseAllowsActions(hand.Phase) || hand.ActingSeatIndex == nil {
		return tablecustody.HashLegalActions([]game.LegalAction{})
	}
	return tablecustody.HashLegalActions(game.GetLegalActions(*hand, hand.ActingSeatIndex))
}

func (runtime *meshRuntime) canonicalNextHandAt(table nativeTableState) string {
	if trimmed := strings.TrimSpace(table.NextHandAt); trimmed != "" {
		return trimmed
	}
	if trimmed := strings.TrimSpace(table.ActiveHandStartAt); trimmed != "" {
		return trimmed
	}
	if table.LatestCustodyState != nil && strings.TrimSpace(table.LatestCustodyState.CreatedAt) != "" {
		return addMillis(table.LatestCustodyState.CreatedAt, runtime.nextHandDelayMSForTable(table))
	}
	if len(table.CustodyTransitions) > 0 {
		lastCreatedAt := strings.TrimSpace(table.CustodyTransitions[len(table.CustodyTransitions)-1].NextState.CreatedAt)
		if lastCreatedAt != "" {
			return addMillis(lastCreatedAt, runtime.nextHandDelayMSForTable(table))
		}
	}
	return ""
}

func (runtime *meshRuntime) custodyActionDeadline(table nativeTableState, kind tablecustody.TransitionKind, hand *game.HoldemState) string {
	if hand == nil {
		return ""
	}
	baseDeadline := ""
	switch kind {
	case tablecustody.TransitionKindBlindPost:
		baseDeadline = runtime.canonicalNextHandAt(table)
	default:
		baseDeadline = runtime.currentCustodyActionDeadline(table)
		if baseDeadline == "" && table.LatestCustodyState != nil {
			baseDeadline = strings.TrimSpace(table.LatestCustodyState.ActionDeadlineAt)
		}
	}
	if kind == tablecustody.TransitionKindTurnChallengeOpen {
		return baseDeadline
	}
	if game.PhaseAllowsActions(hand.Phase) {
		if baseDeadline != "" {
			return addMillis(baseDeadline, runtime.actionTimeoutMSForTable(table))
		}
		return addMillis(nowISO(), runtime.actionTimeoutMSForTable(table))
	}
	if shouldTrackProtocolDeadline(hand.Phase) {
		if kind != tablecustody.TransitionKindBlindPost &&
			table.ActiveHand != nil &&
			table.ActiveHand.State.HandID == hand.HandID &&
			table.ActiveHand.State.Phase == hand.Phase &&
			strings.TrimSpace(table.ActiveHand.Cards.PhaseDeadlineAt) != "" {
			return table.ActiveHand.Cards.PhaseDeadlineAt
		}
		if baseDeadline != "" {
			return addMillis(baseDeadline, runtime.handProtocolTimeoutMSForTable(table))
		}
		return addMillis(nowISO(), runtime.handProtocolTimeoutMSForTable(table))
	}
	if kind == tablecustody.TransitionKindAction || kind == tablecustody.TransitionKindTimeout {
		return baseDeadline
	}
	if table.ActiveHand != nil {
		return table.ActiveHand.Cards.PhaseDeadlineAt
	}
	return ""
}

func (runtime *meshRuntime) currentCustodyActionDeadline(table nativeTableState) string {
	if turnMenuMatchesTable(table, table.PendingTurnMenu) && strings.TrimSpace(table.PendingTurnMenu.ActionDeadlineAt) != "" {
		return table.PendingTurnMenu.ActionDeadlineAt
	}
	if table.LatestCustodyState == nil {
		return ""
	}
	baseDeadline := strings.TrimSpace(table.LatestCustodyState.ActionDeadlineAt)
	if baseDeadline == "" || table.ActiveHand == nil || !game.PhaseAllowsActions(table.ActiveHand.State.Phase) || table.ActiveHand.State.ActingSeatIndex == nil {
		return baseDeadline
	}
	currentPublicStateHash := runtime.publicMoneyStateHash(table, &table.ActiveHand.State)
	if table.LatestCustodyState.PublicStateHash == currentPublicStateHash {
		return baseDeadline
	}
	return addMillis(baseDeadline, runtime.actionTimeoutMSForTable(table))
}

func (runtime *meshRuntime) custodyBalancesFromHand(table nativeTableState, hand *game.HoldemState) []tablecustody.PlayerBalance {
	refByPlayerID := runtime.currentCustodyRefsByPlayer(table)
	reserveByPlayerID := map[string]int{}
	if table.LatestCustodyState != nil {
		for _, claim := range table.LatestCustodyState.StackClaims {
			reserveByPlayerID[claim.PlayerID] = claim.ReservedFeeSats
		}
	}
	balances := make([]tablecustody.PlayerBalance, 0, len(table.Seats))
	if hand == nil {
		for _, seat := range table.Seats {
			amount := seat.BuyInSats
			allIn := false
			folded := false
			reserve := maxInt(0, sumVTXORefs(seat.FundingRefs)-amount)
			refs := append([]tablecustody.VTXORef(nil), seat.FundingRefs...)
			status := seat.Status
			if table.LatestCustodyState != nil {
				for _, claim := range table.LatestCustodyState.StackClaims {
					if claim.PlayerID == seat.PlayerID {
						amount = claim.AmountSats
						allIn = claim.AllIn
						folded = claim.Folded
						reserve = claim.ReservedFeeSats
						refs = append([]tablecustody.VTXORef(nil), claim.VTXORefs...)
						status = firstNonEmptyString(claim.Status, seat.Status)
						break
					}
				}
			}
			balances = append(balances, tablecustody.PlayerBalance{
				AllIn:           allIn,
				Folded:          folded,
				PlayerID:        seat.PlayerID,
				ReservedFeeSats: reserve,
				SeatIndex:       seat.SeatIndex,
				StackSats:       amount,
				Status:          status,
				VTXORefs:        refs,
			})
		}
		return balances
	}

	for _, seat := range table.Seats {
		playerState := hand.Players[seat.SeatIndex]
		balances = append(balances, tablecustody.PlayerBalance{
			AllIn:                 playerState.Status == game.PlayerStatusAllIn,
			Folded:                playerState.Status == game.PlayerStatusFolded,
			PlayerID:              seat.PlayerID,
			ReservedFeeSats:       reserveByPlayerID[seat.PlayerID],
			RoundContributionSats: playerState.RoundContributionSats,
			SeatIndex:             seat.SeatIndex,
			StackSats:             playerState.StackSats,
			Status:                string(playerState.Status),
			TotalContributionSats: playerState.TotalContributionSats,
			VTXORefs:              firstNonEmptyRefs(refByPlayerID[seat.PlayerID], seat.FundingRefs),
		})
	}
	return balances
}

func firstNonEmptyRefs(values ...[]tablecustody.VTXORef) []tablecustody.VTXORef {
	for _, refs := range values {
		if len(refs) > 0 {
			return append([]tablecustody.VTXORef(nil), refs...)
		}
	}
	return nil
}

func (runtime *meshRuntime) currentCustodyRefsByPlayer(table nativeTableState) map[string][]tablecustody.VTXORef {
	refs := map[string][]tablecustody.VTXORef{}
	if table.LatestCustodyState == nil {
		return refs
	}
	for _, claim := range table.LatestCustodyState.StackClaims {
		refs[claim.PlayerID] = append([]tablecustody.VTXORef(nil), claim.VTXORefs...)
	}
	return refs
}

func (runtime *meshRuntime) custodyActingPlayerID(table nativeTableState, hand *game.HoldemState) string {
	if hand == nil || hand.ActingSeatIndex == nil {
		return ""
	}
	seat, ok := seatRecordByIndex(table, *hand.ActingSeatIndex)
	if !ok {
		return ""
	}
	return seat.PlayerID
}

type custodyBindingOverrides struct {
	ActionDeadlineAt string
	ChallengeAnchor  string
	TranscriptRoot   string
}

func (runtime *meshRuntime) currentCustodyTranscriptBindings(table nativeTableState) (string, string) {
	transcriptRoot := handTranscriptRoot(table)
	return firstNonEmptyString(transcriptRoot, table.LastEventHash), transcriptRoot
}

func actionRequestBindingOverrides(request nativeActionRequest) *custodyBindingOverrides {
	return &custodyBindingOverrides{
		ActionDeadlineAt: request.ActionDeadlineAt,
		ChallengeAnchor:  request.ChallengeAnchor,
		TranscriptRoot:   request.TranscriptRoot,
	}
}

func tableWithTurnDeadline(table nativeTableState, turnDeadlineAt string) nativeTableState {
	if table.ActiveHand == nil || table.ActiveHand.State.ActingSeatIndex == nil || strings.TrimSpace(turnDeadlineAt) == "" {
		return table
	}
	if turnMenuMatchesTable(table, table.PendingTurnMenu) {
		if table.PendingTurnMenu != nil && table.PendingTurnMenu.ActionDeadlineAt == turnDeadlineAt {
			return table
		}
		validation := cloneJSON(table)
		if validation.PendingTurnMenu == nil {
			validation.PendingTurnMenu = &NativePendingTurnMenu{}
		}
		validation.PendingTurnMenu.ActionDeadlineAt = turnDeadlineAt
		return validation
	}
	validation := cloneJSON(table)
	validation.PendingTurnMenu = &NativePendingTurnMenu{
		ActionDeadlineAt:     turnDeadlineAt,
		ActingPlayerID:       seatPlayerID(table, *table.ActiveHand.State.ActingSeatIndex),
		DecisionIndex:        custodyDecisionIndex(&table.ActiveHand.State),
		Epoch:                pendingTurnEpoch(table, table.PendingTurnMenu),
		HandID:               table.ActiveHand.State.HandID,
		PrevCustodyStateHash: latestCustodyStateHash(table),
		TurnAnchorHash:       pendingTurnAnchorHash(table, table.PendingTurnMenu),
	}
	return validation
}

func actionRequestValidationTable(table nativeTableState, request nativeActionRequest) nativeTableState {
	return tableWithTurnDeadline(tableWithEpoch(table, request.Epoch), request.ActionDeadlineAt)
}

func tableWithEpoch(table nativeTableState, epoch int) nativeTableState {
	if epoch <= 0 || table.CurrentEpoch == epoch {
		return table
	}
	validation := cloneJSON(table)
	validation.CurrentEpoch = epoch
	return validation
}

func (runtime *meshRuntime) custodyStateBinding(table nativeTableState, kind tablecustody.TransitionKind, hand *game.HoldemState, overrides *custodyBindingOverrides) tablecustody.StateBinding {
	handID := ""
	handNumber := 0
	if hand != nil {
		handID = hand.HandID
		handNumber = hand.HandNumber
	} else if table.PublicState != nil {
		handID = stringValue(table.PublicState.HandID)
		handNumber = table.PublicState.HandNumber
	}
	challengeAnchor, transcriptRoot := runtime.currentCustodyTranscriptBindings(table)
	actionDeadlineAt := runtime.custodyActionDeadline(table, kind, hand)
	if overrides != nil {
		if strings.TrimSpace(overrides.ActionDeadlineAt) != "" {
			actionDeadlineAt = strings.TrimSpace(overrides.ActionDeadlineAt)
		}
		if strings.TrimSpace(overrides.ChallengeAnchor) != "" {
			challengeAnchor = overrides.ChallengeAnchor
		}
		if strings.TrimSpace(overrides.TranscriptRoot) != "" {
			transcriptRoot = overrides.TranscriptRoot
		}
	}
	return tablecustody.StateBinding{
		ActionDeadlineAt: actionDeadlineAt,
		ActingPlayerID:   runtime.custodyActingPlayerID(table, hand),
		ChallengeAnchor:  challengeAnchor,
		CreatedAt:        nowISO(),
		DecisionIndex:    custodyDecisionIndex(hand),
		Epoch:            table.CurrentEpoch,
		HandID:           handID,
		HandNumber:       handNumber,
		LegalActionsHash: runtime.legalActionsHash(hand),
		PublicStateHash:  runtime.publicMoneyStateHash(table, hand),
		TableID:          table.Config.TableID,
		TimeoutPolicy:    defaultCustodyTimeoutPolicy,
		TranscriptRoot:   transcriptRoot,
	}
}

func custodyDecisionIndex(hand *game.HoldemState) int {
	if hand == nil {
		return 0
	}
	return len(hand.ActionLog)
}

func (runtime *meshRuntime) buildCustodyTransitionWithOverrides(table nativeTableState, kind tablecustody.TransitionKind, hand *game.HoldemState, action *game.Action, timeout *tablecustody.TimeoutResolution, overrides *custodyBindingOverrides) (tablecustody.CustodyTransition, error) {
	binding := runtime.custodyStateBinding(table, kind, hand, overrides)
	var descriptor *tablecustody.ActionDescriptor
	if action != nil {
		descriptor = &tablecustody.ActionDescriptor{Type: string(action.Type), TotalSats: action.TotalSats}
	}
	transition, err := tablecustody.BuildTransition(kind, binding, runtime.custodyBalancesFromHand(table, hand), table.LatestCustodyState, descriptor, timeout)
	if err != nil {
		return tablecustody.CustodyTransition{}, err
	}
	if !runtime.config.UseMockSettlement {
		if err := runtime.applyRealCustodyFeeReserve(table, &transition); err != nil {
			return tablecustody.CustodyTransition{}, err
		}
	}
	runtime.carryForwardUnchangedCustodyRefs(table, &transition)
	transition.NextState.StateHash = tablecustody.HashCustodyState(transition.NextState)
	transition.NextStateHash = transition.NextState.StateHash
	transition.Proof.StateHash = transition.NextStateHash
	transition.Proof.VTXORefs = stackProofRefs(transition.NextState)
	transition.Proof.TransitionHash = tablecustody.HashCustodyTransition(transition)
	return transition, nil
}

func (runtime *meshRuntime) buildCustodyTransition(table nativeTableState, kind tablecustody.TransitionKind, hand *game.HoldemState, action *game.Action, timeout *tablecustody.TimeoutResolution) (tablecustody.CustodyTransition, error) {
	return runtime.buildCustodyTransitionWithOverrides(table, kind, hand, action, timeout, nil)
}

func (runtime *meshRuntime) carryForwardUnchangedCustodyRefs(table nativeTableState, transition *tablecustody.CustodyTransition) {
	previous := table.LatestCustodyState
	if previous == nil || transition == nil {
		return
	}
	prevStacks := map[string]tablecustody.StackClaim{}
	for _, claim := range previous.StackClaims {
		prevStacks[claim.PlayerID] = claim
	}
	for index := range transition.NextState.StackClaims {
		nextClaim := transition.NextState.StackClaims[index]
		prevClaim, ok := prevStacks[nextClaim.PlayerID]
		if ok && runtime.stackClaimRefsReusableForTransition(table, *transition, prevClaim, nextClaim) {
			transition.NextState.StackClaims[index].VTXORefs = append([]tablecustody.VTXORef(nil), prevClaim.VTXORefs...)
		}
	}
	prevPots := map[string]tablecustody.PotSlice{}
	for _, slice := range previous.PotSlices {
		prevPots[slice.PotID] = slice
	}
	for index := range transition.NextState.PotSlices {
		nextSlice := transition.NextState.PotSlices[index]
		prevSlice, ok := prevPots[nextSlice.PotID]
		if ok && runtime.potSliceRefsReusableForTransition(table, *transition, prevSlice, nextSlice) {
			transition.NextState.PotSlices[index].VTXORefs = append([]tablecustody.VTXORef(nil), prevSlice.VTXORefs...)
		}
	}
}

func (runtime *meshRuntime) buildSeatLockTransition(table nativeTableState) (tablecustody.CustodyTransition, error) {
	return tablecustody.BuildTransition(tablecustody.TransitionKindBuyInLock, runtime.custodyStateBinding(table, tablecustody.TransitionKindBuyInLock, nil, nil), runtime.custodyBalancesFromHand(table, nil), table.LatestCustodyState, nil, nil)
}

func semanticSeatLockTable(table nativeTableState, transition tablecustody.CustodyTransition) nativeTableState {
	normalized := cloneJSON(table)
	activePlayerIDs := make(map[string]struct{}, len(transition.NextState.StackClaims))
	for _, claim := range transition.NextState.StackClaims {
		activePlayerIDs[claim.PlayerID] = struct{}{}
	}
	for index := range normalized.Seats {
		if normalized.Seats[index].Status != "pending-join" {
			continue
		}
		if _, ok := activePlayerIDs[normalized.Seats[index].PlayerID]; !ok {
			continue
		}
		normalized.Seats[index].Status = "active"
		normalized.Seats[index].NativeSeatedPlayer.Status = "active"
	}
	return normalized
}

func cloneTransitionAuthorizer(authorizer *nativeTransitionAuthorizer) *nativeTransitionAuthorizer {
	if authorizer == nil {
		return nil
	}
	cloned := cloneJSON(*authorizer)
	return &cloned
}

func authorizerForActionRequest(request nativeActionRequest) *nativeTransitionAuthorizer {
	authorizer := nativeTransitionAuthorizer{
		ActionRequest: &request,
	}
	return &authorizer
}

func authorizerForFundsRequest(request nativeFundsRequest) *nativeTransitionAuthorizer {
	authorizer := nativeTransitionAuthorizer{
		FundsRequest: &request,
	}
	return &authorizer
}

func timeoutPolicyFromState(state *tablecustody.CustodyState) tablecustody.TimeoutPolicy {
	if state != nil && state.TimeoutPolicy != "" {
		return state.TimeoutPolicy
	}
	return defaultCustodyTimeoutPolicy
}

func sortTimeoutResolution(resolution *tablecustody.TimeoutResolution) {
	if resolution == nil {
		return
	}
	sort.Strings(resolution.DeadPlayerIDs)
	sort.Strings(resolution.LostEligibilityPlayerIDs)
}

func semanticComparableCustodyStacks(state *tablecustody.CustodyState) []tablecustody.StackClaim {
	stacks := canonicalCustodyMoneyStacks(state)
	for index := range stacks {
		stacks[index].VTXORefs = nil
	}
	return stacks
}

func semanticComparableCustodyPots(state *tablecustody.CustodyState) []tablecustody.PotSlice {
	pots := canonicalCustodyMoneyPots(state)
	for index := range pots {
		pots[index].VTXORefs = nil
	}
	return pots
}

func comparableSemanticCustodyTransition(transition tablecustody.CustodyTransition) tablecustody.CustodyTransition {
	comparable := cloneJSON(transition)
	comparable.Approvals = nil
	comparable.ArkIntentID = ""
	comparable.ArkTxID = ""
	comparable.NextState.ActionDeadlineAt = ""
	comparable.NextState.ChallengeAnchor = ""
	comparable.NextState.CreatedAt = ""
	comparable.NextState.PotSlices = semanticComparableCustodyPots(&comparable.NextState)
	comparable.NextState.StateHash = ""
	comparable.NextState.StackClaims = semanticComparableCustodyStacks(&comparable.NextState)
	comparable.NextState.TranscriptRoot = ""
	comparable.NextStateHash = ""
	comparable.Proof = tablecustody.CustodyProof{}
	comparable.ProposedAt = ""
	comparable.ProposedBy = ""
	comparable.TimeoutResolution = cloneTimeoutResolution(comparable.TimeoutResolution)
	sortTimeoutResolution(comparable.TimeoutResolution)
	comparable.TransitionID = ""
	return comparable
}

func (runtime *meshRuntime) semanticComparableCustodyTransition(table nativeTableState, transition tablecustody.CustodyTransition) (tablecustody.CustodyTransition, error) {
	normalized, _, err := runtime.normalizedCustodySigningTransition(table, transition)
	if err != nil {
		return tablecustody.CustodyTransition{}, err
	}
	return comparableSemanticCustodyTransition(normalized), nil
}

func validateStableSemanticCustodyBindings(expected, supplied tablecustody.CustodyTransition) error {
	if supplied.NextState.ActionDeadlineAt != expected.NextState.ActionDeadlineAt {
		return errors.New("action deadline mismatch")
	}
	if supplied.NextState.ChallengeAnchor != expected.NextState.ChallengeAnchor {
		return errors.New("challenge anchor mismatch")
	}
	if supplied.NextState.TranscriptRoot != expected.NextState.TranscriptRoot {
		return errors.New("transcript root mismatch")
	}
	return nil
}

func (runtime *meshRuntime) deriveTimeoutCustodyTransitionWithOptions(table nativeTableState, requireElapsed bool, requireCompleteMenu bool) (tablecustody.CustodyTransition, error) {
	if table.ActiveHand == nil {
		return tablecustody.CustodyTransition{}, errors.New("timeout transition requires an active hand")
	}
	if table.ActiveHand.State.ActingSeatIndex == nil || !game.PhaseAllowsActions(table.ActiveHand.State.Phase) {
		return tablecustody.CustodyTransition{}, errors.New("timeout transition requires an actionable hand state")
	}
	if table.LatestCustodyState == nil {
		return tablecustody.CustodyTransition{}, errors.New("timeout transition requires a prior custody state")
	}
	if pendingTurnHasLockedCandidate(table) {
		return tablecustody.CustodyTransition{}, errors.New("timeout transition is stale after the turn locked a candidate")
	}
	if requireCompleteMenu && runtime.validatePendingTurnMenu(table, table.PendingTurnMenu) != nil {
		return tablecustody.CustodyTransition{}, errors.New("timeout transition requires a complete pending turn menu")
	}
	if requireElapsed && elapsedMillis(runtime.currentCustodyActionDeadline(table)) < 0 {
		return tablecustody.CustodyTransition{}, errors.New("timeout transition is before the custody deadline")
	}
	actingSeatIndex := *table.ActiveHand.State.ActingSeatIndex
	actingPlayerID := seatPlayerID(table, actingSeatIndex)
	legalActions := game.GetLegalActions(table.ActiveHand.State, table.ActiveHand.State.ActingSeatIndex)
	actionTypes := make([]string, 0, len(legalActions))
	for _, legalAction := range legalActions {
		actionTypes = append(actionTypes, string(legalAction.Type))
	}
	resolution := tablecustody.BuildTimeoutResolution(timeoutPolicyFromState(table.LatestCustodyState), actingPlayerID, actionTypes, []string{actingPlayerID})
	var action game.Action
	switch resolution.ActionType {
	case string(game.ActionCheck):
		action = game.Action{Type: game.ActionCheck}
	default:
		action = game.Action{Type: game.ActionFold}
	}
	nextState, err := game.ApplyHoldemAction(table.ActiveHand.State, actingSeatIndex, action)
	if err != nil {
		return tablecustody.CustodyTransition{}, err
	}
	return runtime.buildCustodyTransition(table, tablecustody.TransitionKindTimeout, &nextState, &action, &resolution)
}

func (runtime *meshRuntime) deriveTimeoutCustodyTransitionWithDeadlineCheck(table nativeTableState, requireElapsed bool) (tablecustody.CustodyTransition, error) {
	return runtime.deriveTimeoutCustodyTransitionWithOptions(table, requireElapsed, true)
}

func (runtime *meshRuntime) deriveTimeoutCustodyTransition(table nativeTableState) (tablecustody.CustodyTransition, error) {
	return runtime.deriveTimeoutCustodyTransitionWithDeadlineCheck(table, true)
}

func (runtime *meshRuntime) deriveBlindPostCustodyTransitionWithOverrides(table nativeTableState, handID string, handNumber int, overrides *custodyBindingOverrides) (tablecustody.CustodyTransition, error) {
	table = cloneJSON(table)
	runtime.releaseCompletedActiveHandForNextStart(&table)
	if len(table.Seats) < 2 {
		return tablecustody.CustodyTransition{}, errors.New("blind-post transition requires two seated players")
	}
	startingBalances := startingBalancesForHand(table, handNumber)
	hand, err := game.CreateHoldemHand(game.HoldemHandConfig{
		BigBlindSats:    table.Config.BigBlindSats,
		DealerSeatIndex: (handNumber - 1) % len(table.Seats),
		HandID:          handID,
		HandNumber:      handNumber,
		Seats: []game.HoldemSeatConfig{
			{PlayerID: table.Seats[0].PlayerID, StackSats: startingBalances[table.Seats[0].PlayerID]},
			{PlayerID: table.Seats[1].PlayerID, StackSats: startingBalances[table.Seats[1].PlayerID]},
		},
		SmallBlindSats: table.Config.SmallBlindSats,
	})
	if err != nil {
		return tablecustody.CustodyTransition{}, err
	}
	return runtime.buildCustodyTransitionWithOverrides(table, tablecustody.TransitionKindBlindPost, &hand, nil, nil, overrides)
}

func (runtime *meshRuntime) deriveBlindPostCustodyTransition(table nativeTableState, handID string, handNumber int) (tablecustody.CustodyTransition, error) {
	return runtime.deriveBlindPostCustodyTransitionWithOverrides(table, handID, handNumber, nil)
}

func (runtime *meshRuntime) validateCustodyTransitionSemanticsWithOptions(table nativeTableState, transition tablecustody.CustodyTransition, authorizer *nativeTransitionAuthorizer, allowFutureTimeout bool) error {
	var (
		expected tablecustody.CustodyTransition
		err      error
	)

	switch transition.Kind {
	case tablecustody.TransitionKindAction:
		if table.ActiveHand == nil {
			return errors.New("action transition requires an active hand")
		}
		if authorizer == nil || authorizer.ActionRequest == nil {
			return errors.New("action transition is missing its signed action request")
		}
		request := *authorizer.ActionRequest
		seat, ok := seatRecordForPlayer(table, request.PlayerID)
		if !ok {
			return fmt.Errorf("missing seat for action player %s", request.PlayerID)
		}
		validationTable := actionRequestValidationTable(table, request)
		if err := runtime.validateActionRequest(validationTable, seat, request); err != nil {
			return err
		}
		nextState, err := game.ApplyHoldemAction(validationTable.ActiveHand.State, seat.SeatIndex, request.Action)
		if err != nil {
			return err
		}
		expected, err = runtime.buildCustodyTransitionWithOverrides(validationTable, tablecustody.TransitionKindAction, &nextState, &request.Action, nil, actionRequestBindingOverrides(request))
	case tablecustody.TransitionKindTimeout:
		validationTable := table
		requireCompleteMenu := false
		if authorizer != nil && strings.TrimSpace(authorizer.TurnDeadlineAt) != "" {
			validationTable = tableWithTurnDeadline(table, authorizer.TurnDeadlineAt)
		} else if turnMenuMatchesTable(table, table.PendingTurnMenu) && strings.TrimSpace(table.PendingTurnMenu.ActionDeadlineAt) != "" {
			validationTable = tableWithTurnDeadline(table, table.PendingTurnMenu.ActionDeadlineAt)
		}
		expected, err = runtime.deriveTimeoutCustodyTransitionWithOptions(validationTable, !allowFutureTimeout, requireCompleteMenu)
	case tablecustody.TransitionKindTurnChallengeOpen:
		if authorizer == nil || strings.TrimSpace(authorizer.TurnDeadlineAt) == "" {
			return errors.New("turn-challenge-open transition is missing its turn deadline authorizer")
		}
		validationTable := tableWithTurnDeadline(table, authorizer.TurnDeadlineAt)
		expected, err = runtime.buildTurnChallengeOpenTransition(validationTable, validationTable.PendingTurnMenu)
	case tablecustody.TransitionKindTurnChallengeEscape:
		turnDeadlineAt := transition.NextState.ActionDeadlineAt
		if authorizer != nil && strings.TrimSpace(authorizer.TurnDeadlineAt) != "" {
			turnDeadlineAt = authorizer.TurnDeadlineAt
		}
		expected, err = runtime.buildTurnChallengeEscapeTransition(tableWithTurnDeadline(table, turnDeadlineAt), turnDeadlineAt)
	case tablecustody.TransitionKindCashOut, tablecustody.TransitionKindEmergencyExit:
		if authorizer == nil || authorizer.FundsRequest == nil {
			return errors.New("funds transition is missing its signed funds request")
		}
		request := *authorizer.FundsRequest
		seat, ok := seatRecordForPlayer(table, request.PlayerID)
		if !ok {
			return fmt.Errorf("missing seat for funds player %s", request.PlayerID)
		}
		if err := runtime.validateFundsRequest(table, seat, request); err != nil {
			return err
		}
		transitionKind, finalStatus, err := fundsTransitionKindAndStatus(request.Kind)
		if err != nil {
			return err
		}
		expected, err = runtime.buildFundsCustodyTransitionForPlayer(table, request.PlayerID, transitionKind, finalStatus)
	case tablecustody.TransitionKindBlindPost:
		expected, err = runtime.deriveBlindPostCustodyTransitionWithOverrides(
			table,
			transition.NextState.HandID,
			transition.NextState.HandNumber,
			&custodyBindingOverrides{ActionDeadlineAt: transition.NextState.ActionDeadlineAt},
		)
	case tablecustody.TransitionKindShowdownPayout:
		if table.ActiveHand == nil || table.ActiveHand.State.Phase != game.StreetSettled {
			return errors.New("showdown-payout transition requires a settled hand")
		}
		hand := cloneJSON(table.ActiveHand.State)
		timeoutResolution := latestTimeoutResolutionForHand(table)
		overrides := (*custodyBindingOverrides)(nil)
		if derived := runtime.showdownPayoutTimeoutResolution(table, timeoutResolution); derived != nil {
			timeoutResolution = derived
			overrides = &custodyBindingOverrides{
				ActionDeadlineAt: table.LatestCustodyState.ActionDeadlineAt,
			}
		}
		expected, err = runtime.buildCustodyTransitionWithOverrides(table, tablecustody.TransitionKindShowdownPayout, &hand, nil, timeoutResolution, overrides)
	case tablecustody.TransitionKindCarryForward:
		expected, err = runtime.buildCustodyTransition(table, tablecustody.TransitionKindCarryForward, nil, nil, nil)
	case tablecustody.TransitionKindBuyInLock:
		expected, err = runtime.buildSeatLockTransition(semanticSeatLockTable(table, transition))
	default:
		return nil
	}
	if err != nil {
		return err
	}
	comparableExpected, err := runtime.semanticComparableCustodyTransition(table, expected)
	if err != nil {
		return err
	}
	comparableSupplied, err := runtime.semanticComparableCustodyTransition(table, transition)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(comparableSupplied, comparableExpected) {
		debugMeshf("custody semantic mismatch kind=%s supplied=%s expected=%s", transition.Kind, string(MustMarshalJSON(comparableSupplied)), string(MustMarshalJSON(comparableExpected)))
		return fmt.Errorf("custody %s transition does not match the locally derived successor", transition.Kind)
	}
	if err := validateStableSemanticCustodyBindings(expected, transition); err != nil {
		debugMeshf("custody semantic binding mismatch kind=%s err=%v supplied=%s expected=%s", transition.Kind, err, string(MustMarshalJSON(transition.NextState)), string(MustMarshalJSON(expected.NextState)))
		return fmt.Errorf("custody %s transition does not match the locally derived successor", transition.Kind)
	}
	return nil
}

func (runtime *meshRuntime) validateCustodyTransitionSemantics(table nativeTableState, transition tablecustody.CustodyTransition, authorizer *nativeTransitionAuthorizer) error {
	return runtime.validateCustodyTransitionSemanticsWithOptions(table, transition, authorizer, false)
}

func custodyApprovalPayload(tableID, playerID, prevStateHash, approvalHash string, custodySeq int, signedAt string) map[string]any {
	return map[string]any{
		"approvalHash":  approvalHash,
		"custodySeq":    custodySeq,
		"playerId":      playerID,
		"prevStateHash": prevStateHash,
		"signedAt":      signedAt,
		"tableId":       tableID,
		"type":          "custody-approval",
	}
}

func custodyApprovalTargetHash(transition tablecustody.CustodyTransition) string {
	return firstNonEmptyString(strings.TrimSpace(transition.Proof.RequestHash), custodyTransitionRequestHash(transition))
}

func (runtime *meshRuntime) localCustodyApproval(transition tablecustody.CustodyTransition) (tablecustody.CustodySignature, error) {
	signedAt := nowISO()
	approvalHash := custodyApprovalTargetHash(transition)
	signatureHex, err := settlementcore.SignStructuredData(runtime.walletID.PrivateKeyHex, custodyApprovalPayload(transition.TableID, runtime.walletID.PlayerID, transition.PrevStateHash, approvalHash, transition.CustodySeq, signedAt))
	if err != nil {
		return tablecustody.CustodySignature{}, err
	}
	return tablecustody.CustodySignature{
		ApprovalHash:    approvalHash,
		PlayerID:        runtime.walletID.PlayerID,
		SignatureHex:    signatureHex,
		SignedAt:        signedAt,
		WalletPubkeyHex: runtime.walletID.PublicKeyHex,
	}, nil
}

func verifyCustodyApproval(seat nativeSeatRecord, transition tablecustody.CustodyTransition, approval tablecustody.CustodySignature) error {
	approvalHash := custodyApprovalTargetHash(transition)
	ok, err := settlementcore.VerifyStructuredData(seat.WalletPubkeyHex, custodyApprovalPayload(transition.TableID, approval.PlayerID, transition.PrevStateHash, approvalHash, transition.CustodySeq, approval.SignedAt), approval.SignatureHex)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("custody approval signature is invalid")
	}
	return nil
}

func (runtime *meshRuntime) handleCustodyApprovalFromPeer(request nativeCustodyApprovalRequest) (nativeCustodyApprovalResponse, error) {
	table, err := runtime.requireLocalTable(request.TableID)
	if err != nil {
		return nativeCustodyApprovalResponse{}, err
	}
	if err := validateSettlementRequestProtocolVersion(request.ProtocolVersion); err != nil {
		return nativeCustodyApprovalResponse{}, err
	}
	if request.PlayerID != runtime.walletID.PlayerID {
		return nativeCustodyApprovalResponse{}, errors.New("custody approval request is not addressed to this player")
	}
	if err := runtime.validatePrebuiltCustodySigningTransition(*table, request.ExpectedPrevStateHash, custodyApprovalTargetHash(request.Transition), request.Transition, request.Authorizer); err != nil {
		return nativeCustodyApprovalResponse{}, err
	}
	if runtime.custodyApprovalHook != nil {
		if err := runtime.custodyApprovalHook(request); err != nil {
			return nativeCustodyApprovalResponse{}, err
		}
	}
	approval, err := runtime.localCustodyApproval(request.Transition)
	if err != nil {
		return nativeCustodyApprovalResponse{}, err
	}
	return nativeCustodyApprovalResponse{Approval: approval}, nil
}

func (runtime *meshRuntime) collectCustodyApprovals(table nativeTableState, transition tablecustody.CustodyTransition, authorizer *nativeTransitionAuthorizer, requiredPlayerIDs []string) (approvals []tablecustody.CustodySignature, err error) {
	timingFields := custodyTimingFields(table, transition, "custody_approval_collect")
	timingFields.Purpose = "approval"
	timing := startMeshTiming(timingFields)
	defer func() {
		timing.EndWith(timingFields, err)
	}()
	approvals = make([]tablecustody.CustodySignature, 0, len(requiredPlayerIDs))
	for _, playerID := range requiredPlayerIDs {
		seat, ok := seatRecordForPlayer(table, playerID)
		if !ok {
			return nil, fmt.Errorf("missing seat for custody signer %s", playerID)
		}
		if playerID == runtime.walletID.PlayerID {
			approval, err := runtime.localCustodyApproval(transition)
			if err != nil {
				return nil, err
			}
			approvals = append(approvals, approval)
			continue
		}
		if seat.PeerURL == "" {
			return nil, fmt.Errorf("missing peer url for custody signer %s", playerID)
		}
		approval, err := runtime.remoteApproveCustody(seat.PeerURL, nativeCustodyApprovalRequest{
			ExpectedPrevStateHash: transition.PrevStateHash,
			Authorizer:            cloneTransitionAuthorizer(authorizer),
			PlayerID:              playerID,
			ProtocolVersion:       nativeProtocolVersion,
			TableID:               table.Config.TableID,
			Transition:            transition,
		})
		if err != nil {
			return nil, fmt.Errorf("remote custody approval for %s: %w", playerID, err)
		}
		if err := verifyCustodyApproval(seat, transition, approval); err != nil {
			return nil, err
		}
		approvals = append(approvals, approval)
	}
	sort.SliceStable(approvals, func(left, right int) bool {
		return approvals[left].PlayerID < approvals[right].PlayerID
	})
	return approvals, nil
}

func cloneTimeoutResolution(resolution *tablecustody.TimeoutResolution) *tablecustody.TimeoutResolution {
	if resolution == nil {
		return nil
	}
	cloned := cloneJSON(*resolution)
	return &cloned
}

func excludedCustodySignerIDs(transition tablecustody.CustodyTransition) map[string]struct{} {
	excluded := map[string]struct{}{}
	if transition.TimeoutResolution == nil {
		return excluded
	}
	excluded[transition.TimeoutResolution.ActingPlayerID] = struct{}{}
	for _, playerID := range transition.TimeoutResolution.DeadPlayerIDs {
		excluded[playerID] = struct{}{}
	}
	for _, playerID := range transition.TimeoutResolution.LostEligibilityPlayerIDs {
		excluded[playerID] = struct{}{}
	}
	return excluded
}

func terminalCustodySeatStatus(status string) bool {
	switch strings.TrimSpace(status) {
	case "completed", "exited", "pending-exit":
		return true
	default:
		return false
	}
}

func (runtime *meshRuntime) requiredCustodySigners(table nativeTableState, transition tablecustody.CustodyTransition) []string {
	if transition.Kind == tablecustody.TransitionKindBuyInLock {
		return nil
	}
	if (transition.Kind == tablecustody.TransitionKindCashOut || transition.Kind == tablecustody.TransitionKindEmergencyExit) &&
		strings.TrimSpace(transition.ActingPlayerID) != "" {
		return []string{transition.ActingPlayerID}
	}
	excluded := excludedCustodySignerIDs(transition)
	claimStatusByPlayer := map[string]string{}
	for _, claim := range transition.NextState.StackClaims {
		claimStatusByPlayer[claim.PlayerID] = claim.Status
	}
	playerIDs := make([]string, 0, len(table.Seats))
	for _, seat := range table.Seats {
		if status, ok := claimStatusByPlayer[seat.PlayerID]; ok {
			if terminalCustodySeatStatus(status) {
				continue
			}
		} else if seat.Status != "" && seat.Status != "active" {
			continue
		}
		if _, skip := excluded[seat.PlayerID]; skip {
			continue
		}
		playerIDs = append(playerIDs, seat.PlayerID)
	}
	sort.Strings(playerIDs)
	return playerIDs
}

func (runtime *meshRuntime) finalizeCustodyTransition(table *nativeTableState, transition *tablecustody.CustodyTransition, authorizer *nativeTransitionAuthorizer) error {
	if table == nil || transition == nil {
		return nil
	}
	if !runtime.config.UseMockSettlement {
		return runtime.finalizeRealCustodyTransition(table, transition, authorizer)
	}
	approvalTransition, _, err := runtime.normalizedCustodyApprovalTransition(*table, *transition)
	if err != nil {
		return err
	}
	approvals, err := runtime.collectCustodyApprovals(*table, approvalTransition, authorizer, runtime.requiredCustodySigners(*table, approvalTransition))
	if err != nil {
		return err
	}
	intentID := "mock-intent-" + randomUUID()
	txID := tablecustody.HashValue(map[string]any{
		"tableId":    table.Config.TableID,
		"custodySeq": transition.CustodySeq,
		"kind":       transition.Kind,
		"nonce":      randomUUID(),
	})
	stackRefs := make([]tablecustody.VTXORef, 0, len(transition.NextState.StackClaims))
	for index := range transition.NextState.StackClaims {
		backedAmount := stackClaimBackedAmount(transition.NextState.StackClaims[index])
		if backedAmount <= 0 {
			transition.NextState.StackClaims[index].VTXORefs = nil
			continue
		}
		spec, err := runtime.stackOutputSpecForTransition(*table, transition, transition.NextState.StackClaims[index].PlayerID, backedAmount)
		if err != nil {
			return err
		}
		ref := tablecustody.VTXORef{
			AmountSats:    backedAmount,
			ArkIntentID:   intentID,
			ArkTxID:       txID,
			ExpiresAt:     addMillis(nowISO(), 86_400_000),
			OwnerPlayerID: transition.NextState.StackClaims[index].PlayerID,
			Script:        spec.Script,
			Tapscripts:    append([]string(nil), spec.Tapscripts...),
			TxID:          txID,
			VOut:          uint32(index),
		}
		transition.NextState.StackClaims[index].VTXORefs = []tablecustody.VTXORef{ref}
		stackRefs = append(stackRefs, ref)
	}
	for index := range transition.NextState.PotSlices {
		spec, err := runtime.potOutputSpec(*table, *transition, transition.NextState.PotSlices[index], runtime.requiredCustodySigners(*table, *transition))
		if err != nil {
			return err
		}
		transition.NextState.PotSlices[index].VTXORefs = []tablecustody.VTXORef{{
			AmountSats:  transition.NextState.PotSlices[index].TotalSats,
			ArkIntentID: intentID,
			ArkTxID:     txID,
			ExpiresAt:   addMillis(nowISO(), 86_400_000),
			Script:      spec.Script,
			Tapscripts:  append([]string(nil), spec.Tapscripts...),
			TxID:        txID,
			VOut:        uint32(len(transition.NextState.StackClaims) + index),
		}}
	}
	transition.ArkIntentID = intentID
	transition.ArkTxID = txID
	transition.NextState.StateHash = tablecustody.HashCustodyState(transition.NextState)
	transition.NextStateHash = transition.NextState.StateHash
	transition.Proof = tablecustody.CustodyProof{
		ArkIntentID:     intentID,
		ArkTxID:         txID,
		RequestHash:     approvalTransition.Proof.RequestHash,
		ReplayValidated: true,
		FinalizedAt:     nowISO(),
		Signatures:      append([]tablecustody.CustodySignature(nil), approvals...),
		StateHash:       transition.NextState.StateHash,
		VTXORefs:        append([]tablecustody.VTXORef(nil), stackRefs...),
	}
	transition.Approvals = approvals
	transition.Proof.TransitionHash = tablecustody.HashCustodyTransition(*transition)
	return tablecustody.ValidateTransition(table.LatestCustodyState, *transition)
}

func (runtime *meshRuntime) applyCustodyTransition(table *nativeTableState, transition tablecustody.CustodyTransition) {
	if table == nil {
		return
	}
	table.CustodyTransitions = append(table.CustodyTransitions, transition)
	state := transition.NextState
	table.LatestCustodyState = &state
	if transition.Kind == tablecustody.TransitionKindTurnChallengeOpen {
		return
	}
	table.PendingTurnChallenge = nil
	table.PendingTurnMenu = nil
}

func latestStackAmount(state *tablecustody.CustodyState, playerID string) int {
	if state == nil {
		return 0
	}
	for _, claim := range state.StackClaims {
		if claim.PlayerID == playerID {
			return claim.AmountSats
		}
	}
	return 0
}

func latestTimeoutResolutionForHand(table nativeTableState) *tablecustody.TimeoutResolution {
	if table.ActiveHand == nil {
		return nil
	}
	handID := table.ActiveHand.State.HandID
	for index := len(table.CustodyTransitions) - 1; index >= 0; index-- {
		transition := table.CustodyTransitions[index]
		if transition.NextState.HandID != handID {
			continue
		}
		if transition.TimeoutResolution != nil {
			return cloneTimeoutResolution(transition.TimeoutResolution)
		}
	}
	return nil
}

func (runtime *meshRuntime) syncTableToCustodySigners(table nativeTableState, requiredPlayerIDs []string) error {
	for _, playerID := range requiredPlayerIDs {
		if playerID == runtime.walletID.PlayerID {
			continue
		}
		seat, ok := seatRecordForPlayer(table, playerID)
		if !ok {
			return fmt.Errorf("missing seat for custody signer %s", playerID)
		}
		peerURL := firstNonEmptyString(seat.PeerURL, runtime.knownPeerURL(seat.PeerID))
		if peerURL == "" {
			return fmt.Errorf("missing peer url for custody signer %s", playerID)
		}
		syncRequest, err := runtime.buildTableSyncRequest(runtime.networkTableView(table, playerID))
		if err != nil {
			return err
		}
		if err := runtime.sendTableSync(peerURL, syncRequest); err != nil {
			return err
		}
	}
	return nil
}
