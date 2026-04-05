package meshruntime

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/btcsuite/btcd/wire"
	"github.com/parkerpoker/parkerd/internal/game"
	"github.com/parkerpoker/parkerd/internal/tablecustody"
)

const (
	turnMenuDeliveryLeadFloorMS = 1_500
	turnMenuDeliveryLeadSlackMS = 250
	turnMenuDeliveryMaxBuilds   = 3
)

func tableHasActionableTurn(table nativeTableState) bool {
	return table.ActiveHand != nil &&
		game.PhaseAllowsActions(table.ActiveHand.State.Phase) &&
		table.ActiveHand.State.ActingSeatIndex != nil &&
		table.LatestCustodyState != nil
}

func turnMenuSourceStateHash(table nativeTableState) string {
	if turnChallengeMatchesTable(table, table.PendingTurnChallenge) && table.PendingTurnChallenge != nil && strings.TrimSpace(table.PendingTurnChallenge.SourceStateHash) != "" {
		return table.PendingTurnChallenge.SourceStateHash
	}
	return latestCustodyStateHash(table)
}

func turnMenuValidationTable(table nativeTableState) nativeTableState {
	validation := tableWithEpoch(table, pendingTurnEpoch(table, table.PendingTurnMenu))
	if !turnChallengeMatchesTable(table, table.PendingTurnChallenge) || len(table.CustodyTransitions) == 0 {
		return validation
	}
	lastIndex := len(table.CustodyTransitions) - 1
	if table.CustodyTransitions[lastIndex].Kind != tablecustody.TransitionKindTurnChallengeOpen {
		return validation
	}
	previous := previousCustodyStateForTransition(table, lastIndex)
	if previous == nil {
		return validation
	}
	validation.LatestCustodyState = cloneCustodyState(previous)
	return validation
}

func turnMenuMatchesTable(table nativeTableState, menu *NativePendingTurnMenu) bool {
	if menu == nil || !tableHasActionableTurn(table) {
		return false
	}
	return menu.TurnAnchorHash == turnAnchorHashForEpoch(table, menu.Epoch) &&
		menu.PrevCustodyStateHash == turnMenuSourceStateHash(table) &&
		menu.HandID == table.ActiveHand.State.HandID &&
		menu.DecisionIndex == custodyDecisionIndex(&table.ActiveHand.State) &&
		menu.ActingPlayerID == seatPlayerID(table, *table.ActiveHand.State.ActingSeatIndex)
}

func turnBundleCacheKey(tableID string, epoch int, turnAnchorHash string) string {
	return fmt.Sprintf("%s|%d|%s", strings.TrimSpace(tableID), epoch, strings.TrimSpace(turnAnchorHash))
}

func turnBundleCacheEpoch(table nativeTableState) int {
	return pendingTurnEpoch(table, table.PendingTurnMenu)
}

func turnBundleCacheAnchorHash(table nativeTableState) string {
	return pendingTurnAnchorHash(table, table.PendingTurnMenu)
}

func localTurnBundleCacheMatchesTable(table nativeTableState, cache *LocalTurnBundleCache) bool {
	if cache == nil || !tableHasActionableTurn(table) {
		return false
	}
	expectedDeadline := ""
	if turnMenuMatchesTable(table, table.PendingTurnMenu) && table.PendingTurnMenu != nil {
		expectedDeadline = strings.TrimSpace(table.PendingTurnMenu.ActionDeadlineAt)
	}
	if expectedDeadline != "" && strings.TrimSpace(cache.ActionDeadlineAt) != expectedDeadline {
		return false
	}
	return strings.TrimSpace(cache.TableID) == table.Config.TableID &&
		cache.Epoch == turnBundleCacheEpoch(table) &&
		cache.HandID == table.ActiveHand.State.HandID &&
		cache.DecisionIndex == custodyDecisionIndex(&table.ActiveHand.State) &&
		cache.ActingPlayerID == seatPlayerID(table, *table.ActiveHand.State.ActingSeatIndex) &&
		cache.PrevCustodyStateHash == turnMenuSourceStateHash(table) &&
		cache.TurnAnchorHash == turnBundleCacheAnchorHash(table)
}

func (runtime *meshRuntime) loadLocalTurnBundleCache(table nativeTableState) (*LocalTurnBundleCache, error) {
	if table.LocalTurnBundleCache != nil && localTurnBundleCacheMatchesTable(table, table.LocalTurnBundleCache) {
		cache := cloneJSON(*table.LocalTurnBundleCache)
		return &cache, nil
	}
	privateState, err := runtime.readTablePrivateState(table.Config.TableID)
	if err != nil {
		return nil, err
	}
	cache, ok := privateState.TurnBundleCaches[turnBundleCacheKey(table.Config.TableID, turnBundleCacheEpoch(table), turnBundleCacheAnchorHash(table))]
	if !ok || !localTurnBundleCacheMatchesTable(table, &cache) {
		return nil, nil
	}
	cloned := cloneJSON(cache)
	return &cloned, nil
}

func (runtime *meshRuntime) storeLocalTurnBundleCache(tableID string, cache *LocalTurnBundleCache) error {
	privateState, err := runtime.readTablePrivateState(tableID)
	if err != nil {
		return err
	}
	if privateState.TurnBundleCaches == nil {
		privateState.TurnBundleCaches = map[string]LocalTurnBundleCache{}
	}
	for key, existing := range privateState.TurnBundleCaches {
		if existing.TableID != tableID {
			continue
		}
		if cache == nil || key != turnBundleCacheKey(tableID, cache.Epoch, cache.TurnAnchorHash) {
			delete(privateState.TurnBundleCaches, key)
		}
	}
	if cache != nil {
		privateState.TurnBundleCaches[turnBundleCacheKey(tableID, cache.Epoch, cache.TurnAnchorHash)] = cloneJSON(*cache)
	}
	return runtime.writeTablePrivateState(tableID, privateState)
}

func findTurnCandidateByHashFromCache(menu *NativePendingTurnMenu, cache *LocalTurnBundleCache, candidateHash string) (NativeTurnCandidateBundle, bool) {
	if cache != nil {
		for _, candidate := range cache.Candidates {
			if candidate.CandidateHash == candidateHash {
				return cloneJSON(candidate), true
			}
		}
		if cache.TimeoutCandidate != nil && cache.TimeoutCandidate.CandidateHash == candidateHash {
			return cloneJSON(*cache.TimeoutCandidate), true
		}
	}
	return findTurnCandidateByHash(menu, candidateHash)
}

func findTurnCandidateByOptionFromCache(menu *NativePendingTurnMenu, cache *LocalTurnBundleCache, optionID string) (NativeTurnCandidateBundle, bool) {
	if cache != nil {
		for _, candidate := range cache.Candidates {
			if candidate.OptionID == optionID {
				return cloneJSON(candidate), true
			}
		}
	}
	return findTurnCandidateByOption(menu, optionID)
}

func (runtime *meshRuntime) pendingTurnMenuWithLocalBundles(table nativeTableState) (*NativePendingTurnMenu, error) {
	if !turnMenuMatchesTable(table, table.PendingTurnMenu) || table.PendingTurnMenu == nil {
		return nil, nil
	}
	menu := cloneJSON(*table.PendingTurnMenu)
	cache, err := runtime.loadLocalTurnBundleCache(table)
	if err != nil {
		return nil, err
	}
	if cache == nil {
		return &menu, nil
	}
	menu.Candidates = cloneJSON(cache.Candidates)
	menu.ChallengeEnvelope = cloneJSON(cache.ChallengeEnvelope)
	menu.TimeoutCandidate = cloneJSON(cache.TimeoutCandidate)
	return &menu, nil
}

func (runtime *meshRuntime) ensurePendingTurnMenuLocked(table *nativeTableState) error {
	if table == nil {
		return nil
	}
	if !tableHasActionableTurn(*table) {
		table.PendingTurnMenu = nil
		return nil
	}
	if turnMenuMatchesTable(*table, table.PendingTurnMenu) {
		if err := runtime.validatePendingTurnMenu(*table, table.PendingTurnMenu); err == nil {
			if cache, cacheErr := runtime.loadLocalTurnBundleCache(*table); cacheErr == nil {
				table.LocalTurnBundleCache = cache
				needsHostRebuild := table.CurrentHost.Peer.PeerID == runtime.selfPeerID() &&
					strings.TrimSpace(table.PendingTurnMenu.SelectedCandidateHash) == "" &&
					!turnChallengeMatchesTable(*table, table.PendingTurnChallenge) &&
					cache == nil
				if !needsHostRebuild {
					return nil
				}
			}
		}
	}
	table.PendingTurnMenu = nil
	table.LocalTurnBundleCache = nil
	if table.CurrentHost.Peer.PeerID == runtime.selfPeerID() {
		if err := runtime.syncTableToCustodySigners(*table, playerIDsFromSeats(table.Seats)); err != nil {
			return err
		}
	}
	menu, err := runtime.buildPendingTurnMenu(*table)
	if err != nil {
		return err
	}
	cache := &LocalTurnBundleCache{
		ActionDeadlineAt:     menu.ActionDeadlineAt,
		ActingPlayerID:       menu.ActingPlayerID,
		Candidates:           cloneJSON(menu.Candidates),
		ChallengeEnvelope:    cloneJSON(menu.ChallengeEnvelope),
		DecisionIndex:        menu.DecisionIndex,
		Epoch:                menu.Epoch,
		HandID:               menu.HandID,
		PrevCustodyStateHash: menu.PrevCustodyStateHash,
		TableID:              table.Config.TableID,
		TurnAnchorHash:       menu.TurnAnchorHash,
	}
	if menu.TimeoutCandidate != nil {
		timeoutCandidate := cloneJSON(*menu.TimeoutCandidate)
		cache.TimeoutCandidate = &timeoutCandidate
	}
	if err := runtime.storeLocalTurnBundleCache(table.Config.TableID, cache); err != nil {
		return err
	}
	table.LocalTurnBundleCache = cache
	table.PendingTurnMenu = &menu
	return nil
}

func (runtime *meshRuntime) turnMenuActionDeadlineAt(table nativeTableState, deliveredAt string) string {
	deliveredAt = strings.TrimSpace(deliveredAt)
	if deliveredAt == "" {
		return ""
	}
	return addMillis(deliveredAt, runtime.actionTimeoutMSForTable(table))
}

func (runtime *meshRuntime) initialTurnMenuDeliveryLeadMS(table nativeTableState) int {
	return maxInt(turnMenuDeliveryLeadFloorMS, runtime.actionTimeoutMSForTable(table)/4)
}

func turnMenuBuildCompletedBeforeDelivery(deliveredAt string, completedAt time.Time) bool {
	deliveryTime, err := parseISOTimestamp(deliveredAt)
	if err != nil {
		return false
	}
	return !completedAt.After(deliveryTime)
}

func (runtime *meshRuntime) buildPendingTurnMenu(table nativeTableState) (NativePendingTurnMenu, error) {
	leadMS := runtime.initialTurnMenuDeliveryLeadMS(table)
	for attempt := 0; attempt < turnMenuDeliveryMaxBuilds; attempt++ {
		buildStartedAt := time.Now().UTC()
		deliveredAt := buildStartedAt.Add(time.Duration(leadMS) * time.Millisecond).Format(time.RFC3339Nano)
		menu, err := runtime.buildPendingTurnMenuForDelivery(table, deliveredAt)
		if err != nil {
			return NativePendingTurnMenu{}, err
		}
		completedAt := time.Now().UTC()
		if turnMenuBuildCompletedBeforeDelivery(menu.DeliveredAt, completedAt) {
			return menu, nil
		}
		observedBuildMS := int(completedAt.Sub(buildStartedAt) / time.Millisecond)
		leadMS = maxInt(leadMS+turnMenuDeliveryLeadSlackMS, observedBuildMS+turnMenuDeliveryLeadSlackMS)
	}
	return NativePendingTurnMenu{}, errors.New("turn menu build exceeded its delivery lead")
}

func (runtime *meshRuntime) buildPendingTurnMenuForDelivery(table nativeTableState, deliveredAt string) (NativePendingTurnMenu, error) {
	if !tableHasActionableTurn(table) {
		return NativePendingTurnMenu{}, errors.New("turn menu requires an actionable hand")
	}
	options, err := deriveFiniteMenuOptions(table.ActiveHand.State, table)
	if err != nil {
		return NativePendingTurnMenu{}, err
	}
	anchorHash := turnAnchorHash(table)
	menu := NativePendingTurnMenu{
		ActionDeadlineAt:     runtime.turnMenuActionDeadlineAt(table, deliveredAt),
		ActingPlayerID:       seatPlayerID(table, *table.ActiveHand.State.ActingSeatIndex),
		Candidates:           make([]NativeTurnCandidateBundle, 0, len(options)),
		DecisionIndex:        custodyDecisionIndex(&table.ActiveHand.State),
		DeliveredAt:          deliveredAt,
		Epoch:                table.CurrentEpoch,
		HandID:               table.ActiveHand.State.HandID,
		Options:              append([]NativeActionMenuOption(nil), options...),
		PrevCustodyStateHash: latestCustodyStateHash(table),
		TurnAnchorHash:       anchorHash,
	}
	buildTable := cloneJSON(table)
	buildTable.PendingTurnMenu = &menu
	for index := range options {
		request, err := runtime.signedActionRequestForOption(buildTable, options[index])
		if err != nil {
			return NativePendingTurnMenu{}, err
		}
		bundle, err := runtime.buildTurnCandidateBundle(buildTable, options[index], anchorHash, request)
		if err != nil {
			return NativePendingTurnMenu{}, err
		}
		menu.Options[index].CandidateHash = bundle.CandidateHash
		menu.Candidates = append(menu.Candidates, bundle)
		buildTable.PendingTurnMenu = &menu
	}
	timeoutCandidate, err := runtime.buildTimeoutCandidateBundle(buildTable, anchorHash)
	if err != nil {
		return NativePendingTurnMenu{}, err
	}
	menu.TimeoutCandidate = &timeoutCandidate
	if turnTimeoutModeForTable(table) == turnTimeoutModeChainChallenge {
		envelope, err := runtime.buildChallengeEnvelope(buildTable, &menu)
		if err != nil {
			return NativePendingTurnMenu{}, err
		}
		menu.ChallengeEnvelope = envelope
	}
	if err := runtime.validatePendingTurnMenu(buildTable, &menu); err != nil {
		return NativePendingTurnMenu{}, err
	}
	return menu, nil
}

func (runtime *meshRuntime) validateTurnCandidateBundle(table nativeTableState, menu *NativePendingTurnMenu, bundle NativeTurnCandidateBundle, expectedOption *NativeActionMenuOption) error {
	if menu == nil {
		return errors.New("pending turn menu is missing")
	}
	if bundle.TurnAnchorHash != menu.TurnAnchorHash {
		return errors.New("turn candidate anchor mismatch")
	}
	if bundle.Transition.PrevStateHash != latestCustodyStateHash(table) {
		return errors.New("turn candidate prev custody mismatch")
	}
	if bundle.Transition.Proof.TurnAnchorHash != menu.TurnAnchorHash {
		return errors.New("turn candidate proof anchor mismatch")
	}
	if bundle.Transition.Proof.TurnCandidateHash != bundle.CandidateHash {
		return errors.New("turn candidate proof hash mismatch")
	}
	switch {
	case expectedOption != nil:
		if bundle.OptionID != expectedOption.OptionID {
			return fmt.Errorf("turn candidate option mismatch: got %s want %s", bundle.OptionID, expectedOption.OptionID)
		}
		if bundle.ActionRequest == nil {
			return errors.New("turn candidate is missing its signed action request")
		}
		if bundle.TimeoutResolution != nil {
			return errors.New("action turn candidate unexpectedly carries a timeout resolution")
		}
		if !reflect.DeepEqual(bundle.ActionRequest.Action, expectedOption.Action) {
			return errors.New("turn candidate action request does not match its menu option")
		}
		seat, ok := seatRecordForPlayer(table, bundle.ActionRequest.PlayerID)
		if !ok {
			return fmt.Errorf("missing seat for action player %s", bundle.ActionRequest.PlayerID)
		}
		if err := runtime.validateActionRequest(table, seat, *bundle.ActionRequest); err != nil {
			return err
		}
	default:
		if bundle.OptionID != "timeout" {
			return fmt.Errorf("timeout turn candidate option mismatch: got %s", bundle.OptionID)
		}
		if bundle.ActionRequest != nil {
			return errors.New("timeout turn candidate unexpectedly carries an action request")
		}
		if bundle.TimeoutResolution == nil {
			return errors.New("timeout turn candidate is missing its timeout resolution")
		}
		if strings.TrimSpace(bundle.TurnDeadlineAt) == "" {
			return errors.New("timeout turn candidate is missing its turn deadline")
		}
		if bundle.TurnDeadlineAt != menu.ActionDeadlineAt {
			return errors.New("timeout turn candidate deadline mismatch")
		}
	}
	if err := runtime.validatePrebuiltCustodySigningTransition(table, bundle.Transition.PrevStateHash, bundle.Transition.Proof.RequestHash, bundle.Transition, authorizerForCandidate(bundle)); err != nil {
		return err
	}
	requiredApprovals := runtime.requiredCustodySigners(table, bundle.Transition)
	if err := runtime.validateCustodyApprovals(table, bundle.Transition, requiredApprovals); err != nil {
		return err
	}
	signingTransition, plan, err := runtime.normalizedCustodySigningTransition(table, bundle.Transition)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(bundle.AuthorizedOutputs, plan.AuthorizedOutputs) {
		return errors.New("turn candidate authorized outputs mismatch")
	}
	if !reflect.DeepEqual(bundle.ProofSignerIDs, plan.ProofSignerIDs) {
		return errors.New("turn candidate proof signers mismatch")
	}
	if !reflect.DeepEqual(bundle.TreeSignerIDs, plan.TreeSignerIDs) {
		return errors.New("turn candidate tree signers mismatch")
	}
	expectedCandidateHash := turnCandidateHash(menu.TurnAnchorHash, signingTransition, plan.AuthorizedOutputs)
	if bundle.CandidateHash != expectedCandidateHash {
		return errors.New("turn candidate hash mismatch")
	}
	if len(plan.Inputs) == 0 {
		if strings.TrimSpace(bundle.SignedProofPSBT) != "" || strings.TrimSpace(bundle.RegisterMessage) != "" {
			return errors.New("turn candidate unexpectedly carries Ark intent artifacts")
		}
		return nil
	}
	if strings.TrimSpace(bundle.SignedProofPSBT) == "" || strings.TrimSpace(bundle.RegisterMessage) == "" {
		return errors.New("turn candidate is missing its signed Ark intent artifacts")
	}
	if err := validateCustodyRegisterMessage(bundle.RegisterMessage, custodyOnchainOutputIndexes(plan.AuthorizedOutputs), sortedSignerPubkeys(bundle.SignerPubkeys)); err != nil {
		return fmt.Errorf("turn candidate register message mismatch: %w", err)
	}
	return nil
}

func (runtime *meshRuntime) validatePendingTurnMenu(table nativeTableState, menu *NativePendingTurnMenu) error {
	if menu == nil {
		if tableHasActionableTurn(table) {
			return errors.New("actionable turn is missing its pending turn menu")
		}
		return nil
	}
	if !tableHasActionableTurn(table) {
		return errors.New("pending turn menu exists without an actionable turn")
	}
	if !turnMenuMatchesTable(table, menu) {
		return errors.New("pending turn menu does not match the current turn")
	}
	validationTable := turnMenuValidationTable(table)
	expectedOptions, err := deriveFiniteMenuOptions(validationTable.ActiveHand.State, validationTable)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(canonicalActionMenuOptions(menu.Options), canonicalActionMenuOptions(expectedOptions)) {
		return errors.New("pending turn menu options do not match the deterministic finite menu")
	}
	if strings.TrimSpace(menu.DeliveredAt) == "" {
		return errors.New("pending turn menu is missing its delivery timestamp")
	}
	if menu.ActionDeadlineAt != runtime.turnMenuActionDeadlineAt(table, menu.DeliveredAt) {
		return errors.New("pending turn menu action deadline does not start at menu delivery")
	}
	for index := range expectedOptions {
		if menu.Options[index].CandidateHash == "" {
			return fmt.Errorf("pending turn menu candidate hash is missing for %s", expectedOptions[index].OptionID)
		}
	}
	cache, err := runtime.loadLocalTurnBundleCache(validationTable)
	if err != nil {
		return err
	}
	if cache != nil {
		if len(cache.Candidates) != len(expectedOptions) {
			return errors.New("local turn bundle cache is missing candidate bundles")
		}
		for index := range expectedOptions {
			candidate, ok := findTurnCandidateByOptionFromCache(menu, cache, expectedOptions[index].OptionID)
			if !ok {
				return fmt.Errorf("local turn bundle cache is missing candidate %s", expectedOptions[index].OptionID)
			}
			if menu.Options[index].CandidateHash != candidate.CandidateHash {
				return fmt.Errorf("pending turn menu candidate hash mismatch for %s", expectedOptions[index].OptionID)
			}
			if err := runtime.validateTurnCandidateBundle(validationTable, menu, candidate, &expectedOptions[index]); err != nil {
				return fmt.Errorf("pending turn candidate %s is invalid: %w", expectedOptions[index].OptionID, err)
			}
		}
		if cache.TimeoutCandidate != nil {
			if err := runtime.validateTurnCandidateBundle(validationTable, menu, *cache.TimeoutCandidate, nil); err != nil {
				return fmt.Errorf("pending timeout candidate is invalid: %w", err)
			}
		}
	}
	if menu.TimeoutCandidate != nil && cache != nil && cache.TimeoutCandidate == nil {
		return errors.New("pending timeout candidate is unavailable in the local turn bundle cache")
	}
	challengeValidationMenu := menu
	if cache != nil {
		cloned := cloneJSON(*menu)
		cloned.Candidates = cloneJSON(cache.Candidates)
		cloned.ChallengeEnvelope = cloneJSON(cache.ChallengeEnvelope)
		cloned.TimeoutCandidate = cloneJSON(cache.TimeoutCandidate)
		challengeValidationMenu = &cloned
	}
	if err := runtime.validateChallengeEnvelope(validationTable, challengeValidationMenu); err != nil {
		return err
	}
	if strings.TrimSpace(menu.SelectedCandidateHash) == "" {
		return nil
	}
	selectedOption, ok := findTurnMenuOptionByCandidateHash(menu, menu.SelectedCandidateHash)
	if !ok || selectedOption.OptionID == "" {
		return errors.New("pending turn menu selected candidate is invalid")
	}
	if menu.SelectionAuth == nil {
		return errors.New("pending turn menu is missing selection auth for its locked candidate")
	}
	if err := runtime.verifySelectionAuth(validationTable, *menu.SelectionAuth); err != nil {
		return err
	}
	if strings.TrimSpace(menu.LockedAt) == "" {
		return errors.New("pending turn menu is missing its lock timestamp")
	}
	if strings.TrimSpace(menu.SettlementDeadlineAt) == "" {
		return errors.New("pending turn menu is missing its settlement deadline")
	}
	if menu.SelectedBundle == nil {
		return errors.New("pending turn menu is missing its selected bundle")
	}
	if err := runtime.validateTurnCandidateBundle(validationTable, menu, *menu.SelectedBundle, &selectedOption); err != nil {
		return fmt.Errorf("pending turn menu selected bundle is invalid: %w", err)
	}
	return nil
}

func (runtime *meshRuntime) validateAcceptedPendingTurnMenu(table nativeTableState, menu *NativePendingTurnMenu) error {
	if menu == nil {
		return nil
	}
	if !tableHasActionableTurn(table) {
		return nil
	}
	if !turnMenuMatchesTable(table, menu) {
		return errors.New("pending turn menu does not match the current turn")
	}
	validationTable := turnMenuValidationTable(table)
	expectedOptions, err := deriveFiniteMenuOptions(validationTable.ActiveHand.State, validationTable)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(canonicalActionMenuOptions(menu.Options), canonicalActionMenuOptions(expectedOptions)) {
		return errors.New("pending turn menu options do not match the deterministic finite menu")
	}
	if strings.TrimSpace(menu.DeliveredAt) == "" {
		return errors.New("pending turn menu is missing its delivery timestamp")
	}
	if menu.ActionDeadlineAt != runtime.turnMenuActionDeadlineAt(table, menu.DeliveredAt) {
		return errors.New("pending turn menu action deadline does not start at menu delivery")
	}
	for index := range expectedOptions {
		if strings.TrimSpace(menu.Options[index].CandidateHash) == "" {
			return fmt.Errorf("pending turn menu candidate hash is missing for %s", expectedOptions[index].OptionID)
		}
	}
	if strings.TrimSpace(menu.SelectedCandidateHash) == "" {
		return nil
	}
	selectedOption, ok := findTurnMenuOptionByCandidateHash(menu, menu.SelectedCandidateHash)
	if !ok {
		return errors.New("pending turn menu selected candidate is invalid")
	}
	if menu.SelectionAuth == nil {
		return errors.New("pending turn menu is missing selection auth for its locked candidate")
	}
	if err := runtime.verifySelectionAuth(validationTable, *menu.SelectionAuth); err != nil {
		return err
	}
	if strings.TrimSpace(menu.LockedAt) == "" || strings.TrimSpace(menu.SettlementDeadlineAt) == "" {
		return errors.New("pending turn menu is missing its lock timing")
	}
	if menu.SelectedBundle == nil {
		return errors.New("pending turn menu is missing its selected bundle")
	}
	return runtime.validateTurnCandidateBundle(validationTable, menu, *menu.SelectedBundle, &selectedOption)
}

func (runtime *meshRuntime) signedActionRequestForOption(table nativeTableState, option NativeActionMenuOption) (nativeActionRequest, error) {
	if !tableHasActionableTurn(table) || table.ActiveHand.State.ActingSeatIndex == nil {
		return nativeActionRequest{}, errors.New("action signing requires an actionable turn")
	}
	actingPlayerID := seatPlayerID(table, *table.ActiveHand.State.ActingSeatIndex)
	if actingPlayerID == runtime.walletID.PlayerID {
		return runtime.buildSignedActionRequest(table, option.Action)
	}
	seat, ok := seatRecordForPlayer(table, actingPlayerID)
	if !ok {
		return nativeActionRequest{}, fmt.Errorf("missing acting seat for player %s", actingPlayerID)
	}
	peerURL := firstNonEmptyString(seat.PeerURL, runtime.knownPeerURL(seat.PeerID))
	if peerURL == "" {
		return nativeActionRequest{}, fmt.Errorf("missing peer url for acting player %s", actingPlayerID)
	}
	unsigned, err := runtime.unsignedActionRequest(table, option.Action)
	if err != nil {
		return nativeActionRequest{}, err
	}
	return runtime.remoteActionSignature(peerURL, nativeActionSignRequest{Request: unsigned})
}

func (runtime *meshRuntime) buildTurnCandidateBundle(table nativeTableState, option NativeActionMenuOption, anchorHash string, request nativeActionRequest) (NativeTurnCandidateBundle, error) {
	if request.OptionID != option.OptionID {
		return NativeTurnCandidateBundle{}, fmt.Errorf("signed action request option mismatch: got %s want %s", request.OptionID, option.OptionID)
	}
	if request.TurnAnchorHash != anchorHash {
		return NativeTurnCandidateBundle{}, fmt.Errorf("signed action request turn anchor mismatch: got %s want %s", request.TurnAnchorHash, anchorHash)
	}
	nextState, err := game.ApplyHoldemAction(table.ActiveHand.State, *table.ActiveHand.State.ActingSeatIndex, request.Action)
	if err != nil {
		return NativeTurnCandidateBundle{}, err
	}
	transition, err := runtime.buildCustodyTransitionWithOverrides(table, tablecustody.TransitionKindAction, &nextState, &request.Action, nil, actionRequestBindingOverrides(request))
	if err != nil {
		return NativeTurnCandidateBundle{}, err
	}
	return runtime.prebuildTurnCandidateBundle(table, transition, authorizerForActionRequest(request), option.OptionID, anchorHash, &request)
}

func (runtime *meshRuntime) buildTimeoutCandidateBundle(table nativeTableState, anchorHash string) (NativeTurnCandidateBundle, error) {
	if !tableHasActionableTurn(table) {
		return NativeTurnCandidateBundle{}, errors.New("timeout candidate requires an actionable hand")
	}
	actingSeatIndex := *table.ActiveHand.State.ActingSeatIndex
	actingPlayerID := seatPlayerID(table, actingSeatIndex)
	legalActions := game.GetLegalActions(table.ActiveHand.State, table.ActiveHand.State.ActingSeatIndex)
	actionTypes := make([]string, 0, len(legalActions))
	for _, legal := range legalActions {
		actionTypes = append(actionTypes, string(legal.Type))
	}
	resolution := tablecustody.BuildTimeoutResolution(timeoutPolicyFromState(table.LatestCustodyState), actingPlayerID, actionTypes, []string{actingPlayerID})
	action := game.Action{Type: game.ActionFold}
	if resolution.ActionType == string(game.ActionCheck) {
		action = game.Action{Type: game.ActionCheck}
	}
	nextState, err := game.ApplyHoldemAction(table.ActiveHand.State, actingSeatIndex, action)
	if err != nil {
		return NativeTurnCandidateBundle{}, err
	}
	transition, err := runtime.buildCustodyTransition(table, tablecustody.TransitionKindTimeout, &nextState, &action, &resolution)
	if err != nil {
		return NativeTurnCandidateBundle{}, err
	}
	return runtime.prebuildTurnCandidateBundle(table, transition, &nativeTransitionAuthorizer{TurnDeadlineAt: runtime.currentCustodyActionDeadline(table)}, "timeout", anchorHash, nil)
}

func (runtime *meshRuntime) prebuildTurnCandidateBundle(table nativeTableState, transition tablecustody.CustodyTransition, authorizer *nativeTransitionAuthorizer, optionID, anchorHash string, request *nativeActionRequest) (NativeTurnCandidateBundle, error) {
	approvalTransition, _, err := runtime.normalizedCustodyApprovalTransition(table, transition)
	if err != nil {
		return NativeTurnCandidateBundle{}, err
	}
	approvals, err := runtime.collectCustodyApprovals(table, approvalTransition, authorizer, runtime.requiredCustodySigners(table, approvalTransition))
	if err != nil {
		return NativeTurnCandidateBundle{}, err
	}
	signingTransition, plan, err := runtime.normalizedCustodySigningTransition(table, transition)
	if err != nil {
		return NativeTurnCandidateBundle{}, err
	}
	bundle := NativeTurnCandidateBundle{
		ActionRequest:     request,
		AuthorizedOutputs: append([]custodyBatchOutput(nil), plan.AuthorizedOutputs...),
		OptionID:          optionID,
		ProofSignerIDs:    append([]string(nil), plan.ProofSignerIDs...),
		TimeoutResolution: cloneTimeoutResolution(transition.TimeoutResolution),
		Transition:        cloneJSON(transition),
		TreeSignerIDs:     append([]string(nil), plan.TreeSignerIDs...),
		TurnAnchorHash:    anchorHash,
	}
	if authorizer != nil {
		bundle.TurnDeadlineAt = strings.TrimSpace(authorizer.TurnDeadlineAt)
	}
	requestHash := approvalTransition.Proof.RequestHash
	bundle.Transition.Approvals = append([]tablecustody.CustodySignature(nil), approvals...)
	bundle.Transition.Proof.RequestHash = requestHash
	bundle.Transition.Proof.ReplayValidated = true
	bundle.Transition.Proof.Signatures = append([]tablecustody.CustodySignature(nil), approvals...)
	bundle.CandidateHash = turnCandidateHash(anchorHash, signingTransition, plan.AuthorizedOutputs)
	bundle.Transition.Proof.TurnAnchorHash = anchorHash
	bundle.Transition.Proof.TurnCandidateHash = bundle.CandidateHash
	if len(plan.Inputs) == 0 {
		return bundle, nil
	}
	signerSessions, signerPubkeys, derivationPath, err := runtime.prepareCustodyBatchSigners(table, transition.PrevStateHash, requestHash, signingTransition, authorizer, plan.TreeSignerIDs, plan.AuthorizedOutputs)
	if err != nil {
		return NativeTurnCandidateBundle{}, err
	}
	_ = signerSessions
	bundle.DerivationPath = derivationPath
	bundle.SignerPubkeys = signerPubkeys
	registerMessage, err := custodyRegisterMessage(custodyOnchainOutputIndexes(plan.AuthorizedOutputs), sortedSignerPubkeys(signerPubkeys))
	if err != nil {
		return NativeTurnCandidateBundle{}, err
	}
	intentInputs, leafProofs, arkFields, locktime, err := custodyIntentInputs(plan.Inputs)
	if err != nil {
		return NativeTurnCandidateBundle{}, err
	}
	txOutputs := make([]*wire.TxOut, 0, len(plan.AuthorizedOutputs))
	for _, output := range plan.AuthorizedOutputs {
		txOut, err := decodeBatchOutputTxOut(output)
		if err != nil {
			return NativeTurnCandidateBundle{}, err
		}
		txOutputs = append(txOutputs, txOut)
	}
	unsignedProof, err := custodyBuildProofPSBT(registerMessage, intentInputs, txOutputs, leafProofs, arkFields, locktime)
	if err != nil {
		return NativeTurnCandidateBundle{}, err
	}
	signedProof, err := runtime.fullySignCustodyPSBT(table, transition.PrevStateHash, requestHash, "proof", plan.ProofSignerIDs, unsignedProof, signingTransition, authorizer, plan.AuthorizedOutputs)
	if err != nil {
		return NativeTurnCandidateBundle{}, err
	}
	bundle.RegisterMessage = registerMessage
	bundle.SignedProofPSBT = signedProof
	return bundle, nil
}
