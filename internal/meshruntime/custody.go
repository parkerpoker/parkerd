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

func (runtime *meshRuntime) custodyActionDeadline(table nativeTableState, hand *game.HoldemState) string {
	if hand == nil {
		return ""
	}
	if game.PhaseAllowsActions(hand.Phase) {
		return addMillis(nowISO(), runtime.actionTimeoutMS())
	}
	if table.ActiveHand != nil {
		return table.ActiveHand.Cards.PhaseDeadlineAt
	}
	return ""
}

func (runtime *meshRuntime) custodyBalancesFromHand(table nativeTableState, hand *game.HoldemState) []tablecustody.PlayerBalance {
	refByPlayerID := runtime.currentCustodyRefsByPlayer(table)
	balances := make([]tablecustody.PlayerBalance, 0, len(table.Seats))
	if hand == nil {
		for _, seat := range table.Seats {
			amount := seat.BuyInSats
			if table.LatestCustodyState != nil {
				for _, claim := range table.LatestCustodyState.StackClaims {
					if claim.PlayerID == seat.PlayerID {
						amount = claim.AmountSats
						break
					}
				}
			}
			balances = append(balances, tablecustody.PlayerBalance{
				PlayerID:  seat.PlayerID,
				SeatIndex: seat.SeatIndex,
				StackSats: amount,
				Status:    seat.Status,
				VTXORefs:  firstNonEmptyRefs(refByPlayerID[seat.PlayerID], seat.FundingRefs),
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

func (runtime *meshRuntime) custodyStateBinding(table nativeTableState, hand *game.HoldemState) tablecustody.StateBinding {
	handID := ""
	handNumber := 0
	if hand != nil {
		handID = hand.HandID
		handNumber = hand.HandNumber
	} else if table.PublicState != nil {
		handID = stringValue(table.PublicState.HandID)
		handNumber = table.PublicState.HandNumber
	}
	return tablecustody.StateBinding{
		ActionDeadlineAt: runtime.custodyActionDeadline(table, hand),
		ActingPlayerID:   runtime.custodyActingPlayerID(table, hand),
		ChallengeAnchor:  firstNonEmptyString(handTranscriptRoot(table), table.LastEventHash),
		CreatedAt:        nowISO(),
		DecisionIndex:    custodyDecisionIndex(hand),
		Epoch:            table.CurrentEpoch,
		HandID:           handID,
		HandNumber:       handNumber,
		LegalActionsHash: runtime.legalActionsHash(hand),
		PublicStateHash:  runtime.publicMoneyStateHash(table, hand),
		TableID:          table.Config.TableID,
		TimeoutPolicy:    defaultCustodyTimeoutPolicy,
		TranscriptRoot:   handTranscriptRoot(table),
	}
}

func custodyDecisionIndex(hand *game.HoldemState) int {
	if hand == nil {
		return 0
	}
	return len(hand.ActionLog)
}

func (runtime *meshRuntime) buildCustodyTransition(table nativeTableState, kind tablecustody.TransitionKind, hand *game.HoldemState, action *game.Action, timeout *tablecustody.TimeoutResolution) (tablecustody.CustodyTransition, error) {
	binding := runtime.custodyStateBinding(table, hand)
	var descriptor *tablecustody.ActionDescriptor
	if action != nil {
		descriptor = &tablecustody.ActionDescriptor{Type: string(action.Type), TotalSats: action.TotalSats}
	}
	transition, err := tablecustody.BuildTransition(kind, binding, runtime.custodyBalancesFromHand(table, hand), table.LatestCustodyState, descriptor, timeout)
	if err != nil {
		return tablecustody.CustodyTransition{}, err
	}
	carryForwardUnchangedCustodyRefs(table.LatestCustodyState, &transition)
	transition.NextState.StateHash = tablecustody.HashCustodyState(transition.NextState)
	transition.NextStateHash = transition.NextState.StateHash
	transition.Proof.StateHash = transition.NextStateHash
	transition.Proof.VTXORefs = stackProofRefs(transition.NextState)
	transition.Proof.TransitionHash = tablecustody.HashCustodyTransition(transition)
	return transition, nil
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
	return tablecustody.BuildTransition(tablecustody.TransitionKindBuyInLock, runtime.custodyStateBinding(table, nil), runtime.custodyBalancesFromHand(table, nil), table.LatestCustodyState, nil, nil)
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

func (runtime *meshRuntime) collectCustodyApprovals(table nativeTableState, transition tablecustody.CustodyTransition, requiredPlayerIDs []string) ([]tablecustody.CustodySignature, error) {
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

func (runtime *meshRuntime) requiredCustodySigners(table nativeTableState, transition tablecustody.CustodyTransition) []string {
	if transition.Kind == tablecustody.TransitionKindBuyInLock {
		return nil
	}
	if (transition.Kind == tablecustody.TransitionKindCashOut || transition.Kind == tablecustody.TransitionKindEmergencyExit) &&
		strings.TrimSpace(transition.ActingPlayerID) != "" {
		return []string{transition.ActingPlayerID}
	}
	excluded := excludedCustodySignerIDs(transition)
	playerIDs := make([]string, 0, len(table.Seats))
	for _, seat := range table.Seats {
		if seat.Status != "" && seat.Status != "active" {
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

func (runtime *meshRuntime) finalizeCustodyTransition(table *nativeTableState, transition *tablecustody.CustodyTransition) error {
	if table == nil || transition == nil {
		return nil
	}
	if !runtime.config.UseMockSettlement {
		return runtime.finalizeRealCustodyTransition(table, transition)
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
		spec, err := runtime.stackOutputSpec(*table, transition.NextState.StackClaims[index].PlayerID, transition.NextState.StackClaims[index].AmountSats)
		if err != nil {
			return err
		}
		ref := tablecustody.VTXORef{
			AmountSats:    transition.NextState.StackClaims[index].AmountSats,
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
	approvals, err := runtime.collectCustodyApprovals(*table, *transition, runtime.requiredCustodySigners(*table, *transition))
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
