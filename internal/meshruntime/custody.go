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
	if table.LatestCustodyState != nil && strings.TrimSpace(table.LatestCustodyState.CreatedAt) != "" {
		return addMillis(table.LatestCustodyState.CreatedAt, runtime.nextHandDelayMSForTable(table))
	}
	if len(table.CustodyTransitions) > 0 {
		lastCreatedAt := strings.TrimSpace(table.CustodyTransitions[len(table.CustodyTransitions)-1].NextState.CreatedAt)
		if lastCreatedAt != "" {
			return addMillis(lastCreatedAt, runtime.nextHandDelayMSForTable(table))
		}
	}
	if trimmed := strings.TrimSpace(table.ActiveHandStartAt); trimmed != "" {
		return trimmed
	}
	if trimmed := strings.TrimSpace(table.NextHandAt); trimmed != "" {
		return trimmed
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
		if table.LatestCustodyState != nil {
			baseDeadline = strings.TrimSpace(table.LatestCustodyState.ActionDeadlineAt)
		}
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
	if table.ActiveHand != nil {
		return table.ActiveHand.Cards.PhaseDeadlineAt
	}
	return ""
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
			reserve := maxInt(0, sumVTXORefs(seat.FundingRefs)-amount)
			refs := append([]tablecustody.VTXORef(nil), seat.FundingRefs...)
			if table.LatestCustodyState != nil {
				for _, claim := range table.LatestCustodyState.StackClaims {
					if claim.PlayerID == seat.PlayerID {
						amount = claim.AmountSats
						reserve = claim.ReservedFeeSats
						refs = append([]tablecustody.VTXORef(nil), claim.VTXORefs...)
						break
					}
				}
			}
			balances = append(balances, tablecustody.PlayerBalance{
				PlayerID:        seat.PlayerID,
				ReservedFeeSats: reserve,
				SeatIndex:       seat.SeatIndex,
				StackSats:       amount,
				Status:          seat.Status,
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
	ChallengeAnchor string
	TranscriptRoot  string
}

func (runtime *meshRuntime) currentCustodyTranscriptBindings(table nativeTableState) (string, string) {
	transcriptRoot := handTranscriptRoot(table)
	return firstNonEmptyString(transcriptRoot, table.LastEventHash), transcriptRoot
}

func actionRequestBindingOverrides(request nativeActionRequest) *custodyBindingOverrides {
	return &custodyBindingOverrides{
		ChallengeAnchor: request.ChallengeAnchor,
		TranscriptRoot:  request.TranscriptRoot,
	}
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
	if overrides != nil {
		challengeAnchor = overrides.ChallengeAnchor
		transcriptRoot = overrides.TranscriptRoot
	}
	return tablecustody.StateBinding{
		ActionDeadlineAt: runtime.custodyActionDeadline(table, kind, hand),
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
	carryForwardUnchangedCustodyRefs(table.LatestCustodyState, &transition)
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

func carryForwardUnchangedCustodyRefs(previous *tablecustody.CustodyState, transition *tablecustody.CustodyTransition) {
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
		if ok && reflect.DeepEqual(comparableStackClaim(prevClaim), comparableStackClaim(nextClaim)) {
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
		if ok && reflect.DeepEqual(comparablePotSlice(prevSlice), comparablePotSlice(nextSlice)) {
			transition.NextState.PotSlices[index].VTXORefs = append([]tablecustody.VTXORef(nil), prevSlice.VTXORefs...)
		}
	}
}

func (runtime *meshRuntime) buildSeatLockTransition(table nativeTableState) (tablecustody.CustodyTransition, error) {
	return tablecustody.BuildTransition(tablecustody.TransitionKindBuyInLock, runtime.custodyStateBinding(table, tablecustody.TransitionKindBuyInLock, nil, nil), runtime.custodyBalancesFromHand(table, nil), table.LatestCustodyState, nil, nil)
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

func (runtime *meshRuntime) deriveTimeoutCustodyTransition(table nativeTableState) (tablecustody.CustodyTransition, error) {
	if table.ActiveHand == nil {
		return tablecustody.CustodyTransition{}, errors.New("timeout transition requires an active hand")
	}
	if table.ActiveHand.State.ActingSeatIndex == nil || !game.PhaseAllowsActions(table.ActiveHand.State.Phase) {
		return tablecustody.CustodyTransition{}, errors.New("timeout transition requires an actionable hand state")
	}
	if table.LatestCustodyState == nil {
		return tablecustody.CustodyTransition{}, errors.New("timeout transition requires a prior custody state")
	}
	if elapsedMillis(table.LatestCustodyState.ActionDeadlineAt) < 0 {
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

func (runtime *meshRuntime) deriveBlindPostCustodyTransition(table nativeTableState, handID string, handNumber int) (tablecustody.CustodyTransition, error) {
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
	return runtime.buildCustodyTransition(table, tablecustody.TransitionKindBlindPost, &hand, nil, nil)
}

func (runtime *meshRuntime) validateCustodyTransitionSemantics(table nativeTableState, transition tablecustody.CustodyTransition, authorizer *nativeTransitionAuthorizer) error {
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
		if err := runtime.validateActionRequest(table, seat, request); err != nil {
			return err
		}
		nextState, err := game.ApplyHoldemAction(table.ActiveHand.State, seat.SeatIndex, request.Action)
		if err != nil {
			return err
		}
		expected, err = runtime.buildCustodyTransitionWithOverrides(table, tablecustody.TransitionKindAction, &nextState, &request.Action, nil, actionRequestBindingOverrides(request))
	case tablecustody.TransitionKindTimeout:
		expected, err = runtime.deriveTimeoutCustodyTransition(table)
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
		expected, err = runtime.deriveBlindPostCustodyTransition(table, transition.NextState.HandID, transition.NextState.HandNumber)
	case tablecustody.TransitionKindShowdownPayout:
		if table.ActiveHand == nil || table.ActiveHand.State.Phase != game.StreetSettled {
			return errors.New("showdown-payout transition requires a settled hand")
		}
		hand := cloneJSON(table.ActiveHand.State)
		expected, err = runtime.buildCustodyTransition(table, tablecustody.TransitionKindShowdownPayout, &hand, nil, latestTimeoutResolutionForHand(table))
	case tablecustody.TransitionKindCarryForward:
		expected, err = runtime.buildCustodyTransition(table, tablecustody.TransitionKindCarryForward, nil, nil, nil)
	case tablecustody.TransitionKindBuyInLock:
		expected, err = runtime.buildSeatLockTransition(table)
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

func custodyApprovalPayload(tableID, playerID, prevStateHash, nextStateHash string, custodySeq int, signedAt string) map[string]any {
	return map[string]any{
		"custodySeq":    custodySeq,
		"nextStateHash": nextStateHash,
		"playerId":      playerID,
		"prevStateHash": prevStateHash,
		"signedAt":      signedAt,
		"tableId":       tableID,
		"type":          "custody-approval",
	}
}

func (runtime *meshRuntime) localCustodyApproval(transition tablecustody.CustodyTransition) (tablecustody.CustodySignature, error) {
	signedAt := nowISO()
	signatureHex, err := settlementcore.SignStructuredData(runtime.walletID.PrivateKeyHex, custodyApprovalPayload(transition.TableID, runtime.walletID.PlayerID, transition.PrevStateHash, transition.NextStateHash, transition.CustodySeq, signedAt))
	if err != nil {
		return tablecustody.CustodySignature{}, err
	}
	return tablecustody.CustodySignature{
		ApprovalHash:    transition.NextStateHash,
		PlayerID:        runtime.walletID.PlayerID,
		SignatureHex:    signatureHex,
		SignedAt:        signedAt,
		WalletPubkeyHex: runtime.walletID.PublicKeyHex,
	}, nil
}

func verifyCustodyApproval(seat nativeSeatRecord, transition tablecustody.CustodyTransition, approval tablecustody.CustodySignature) error {
	ok, err := settlementcore.VerifyStructuredData(seat.WalletPubkeyHex, custodyApprovalPayload(transition.TableID, approval.PlayerID, transition.PrevStateHash, transition.NextStateHash, transition.CustodySeq, approval.SignedAt), approval.SignatureHex)
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
	if request.PlayerID != runtime.walletID.PlayerID {
		return nativeCustodyApprovalResponse{}, errors.New("custody approval request is not addressed to this player")
	}
	if table.LatestCustodyState == nil || table.LatestCustodyState.StateHash != request.ExpectedPrevStateHash {
		return nativeCustodyApprovalResponse{}, errors.New("custody approval request references stale state")
	}
	if err := runtime.validateCustodyTransitionSemantics(*table, request.Transition, request.Authorizer); err != nil {
		return nativeCustodyApprovalResponse{}, err
	}
	if err := tablecustody.ValidateTransition(table.LatestCustodyState, request.Transition); err != nil {
		return nativeCustodyApprovalResponse{}, err
	}
	requireArkSettlement := !runtime.config.UseMockSettlement && custodyTransitionRequiresArkSettlement(table.LatestCustodyState, request.Transition)
	if err := validateAcceptedCustodyRefs(table.LatestCustodyState, request.Transition, requireArkSettlement); err != nil {
		return nativeCustodyApprovalResponse{}, err
	}
	if requireArkSettlement {
		if err := runtime.validateCustodyTransitionArkProof(table.LatestCustodyState, request.Transition, true); err != nil {
			return nativeCustodyApprovalResponse{}, err
		}
	}
	approval, err := runtime.localCustodyApproval(request.Transition)
	if err != nil {
		return nativeCustodyApprovalResponse{}, err
	}
	return nativeCustodyApprovalResponse{Approval: approval}, nil
}

func (runtime *meshRuntime) collectCustodyApprovals(table nativeTableState, transition tablecustody.CustodyTransition, authorizer *nativeTransitionAuthorizer, requiredPlayerIDs []string) ([]tablecustody.CustodySignature, error) {
	approvals := make([]tablecustody.CustodySignature, 0, len(requiredPlayerIDs))
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
			TableID:               table.Config.TableID,
			Transition:            transition,
		})
		if err != nil {
			return nil, err
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
		spec, err := runtime.stackOutputSpec(*table, transition.NextState.StackClaims[index].PlayerID, backedAmount)
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
		ReplayValidated: true,
		StateHash:       transition.NextStateHash,
		VTXORefs:        append([]tablecustody.VTXORef(nil), stackRefs...),
	}
	transition.Proof.TransitionHash = tablecustody.HashCustodyTransition(*transition)
	approvals, err := runtime.collectCustodyApprovals(*table, *transition, authorizer, runtime.requiredCustodySigners(*table, *transition))
	if err != nil {
		return err
	}
	transition.Approvals = approvals
	transition.Proof = tablecustody.CustodyProof{
		ArkIntentID:     intentID,
		ArkTxID:         txID,
		FinalizedAt:     nowISO(),
		ReplayValidated: true,
		Signatures:      approvals,
		StateHash:       transition.NextState.StateHash,
		VTXORefs:        stackRefs,
	}
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
