package meshruntime

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/parkerpoker/parkerd/internal/game"
	"github.com/parkerpoker/parkerd/internal/tablecustody"
)

func createSyntheticRealStartedTableForRecoveryTest(t *testing.T) (*meshRuntime, *meshRuntime, string) {
	t.Helper()

	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")
	tableID, _ := createStartedTwoPlayerTableInSyntheticRealMode(t, host, guest)
	return host, guest, tableID
}

func manualBlindTransitionSourceForRecoveryTest(t *testing.T) (*meshRuntime, *meshRuntime, nativeTableState, game.HoldemState, tablecustody.CustodyTransition) {
	t.Helper()

	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")
	tableID, _ := createStartedTwoPlayerTableInSyntheticRealMode(t, host, guest)
	table := mustReadNativeTable(t, host, tableID)
	if table.ActiveHand == nil {
		t.Fatal("expected active hand")
	}
	if len(table.CustodyTransitions) == 0 {
		t.Fatal("expected custody history")
	}
	lastTransition := table.CustodyTransitions[len(table.CustodyTransitions)-1]
	if lastTransition.Kind != tablecustody.TransitionKindBlindPost {
		t.Fatalf("expected latest custody transition %s, got %s", tablecustody.TransitionKindBlindPost, lastTransition.Kind)
	}
	return host, guest, table, table.ActiveHand.State, cloneJSON(lastTransition)
}

func withLatestRecoveryBundlesForTest(t *testing.T, runtime *meshRuntime, table nativeTableState, postHand game.HoldemState) nativeTableState {
	t.Helper()

	if len(table.CustodyTransitions) == 0 {
		t.Fatal("expected custody history")
	}
	table = cloneJSON(table)
	lastIndex := len(table.CustodyTransitions) - 1
	transition := cloneJSON(table.CustodyTransitions[lastIndex])
	if len(transition.Proof.RecoveryBundles) == 0 {
		baseTable := cloneJSON(table)
		if lastIndex > 0 {
			previous := cloneJSON(baseTable.CustodyTransitions[lastIndex-1].NextState)
			baseTable.LatestCustodyState = &previous
		} else {
			baseTable.LatestCustodyState = nil
		}
		if err := runtime.attachDeterministicRecoveryBundles(baseTable, &transition, nil, &postHand); err != nil {
			t.Fatalf("attach recovery bundles: %v", err)
		}
		table.CustodyTransitions[lastIndex] = transition
		latest := cloneJSON(table.CustodyTransitions[lastIndex].NextState)
		table.LatestCustodyState = &latest
	}
	return table
}

func writeLocalRecoveryTableForTest(t *testing.T, runtime *meshRuntime, table nativeTableState) {
	t.Helper()

	cloned := cloneJSON(table)
	if err := runtime.store.writeTable(&cloned); err != nil {
		t.Fatalf("write recovery table: %v", err)
	}
}

func expireLocalActionDeadlineForRecoveryTest(t *testing.T, runtime *meshRuntime, tableID string) {
	t.Helper()

	table := mustReadNativeTable(t, runtime, tableID)
	expireCurrentTurnActionDeadlineForTest(t, &table)
	if err := runtime.store.writeTable(&table); err != nil {
		t.Fatalf("write expired action deadline: %v", err)
	}
}

func expireLocalProtocolDeadlineForRecoveryTest(t *testing.T, runtime *meshRuntime, tableID string) {
	t.Helper()

	table := mustReadNativeTable(t, runtime, tableID)
	if table.ActiveHand == nil {
		t.Fatal("expected active hand")
	}
	table.ActiveHand.Cards.PhaseDeadlineAt = addMillis(nowISO(), -1)
	if err := runtime.store.writeTable(&table); err != nil {
		t.Fatalf("write expired protocol deadline: %v", err)
	}
}

func waitForRecoveredTransitionForTest(t *testing.T, runtime *meshRuntime, tableID string, kind tablecustody.TransitionKind) nativeTableState {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		runtime.Tick()
		table := mustReadNativeTable(t, runtime, tableID)
		if len(table.CustodyTransitions) == 0 {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		lastTransition := table.CustodyTransitions[len(table.CustodyTransitions)-1]
		if lastTransition.Kind == kind && lastTransition.Proof.RecoveryWitness != nil {
			return table
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for recovered %s transition", kind)
	return nativeTableState{}
}

func TestRecoveryBundlesOnlyAttachOnDeterministicMoneyResolvingStates(t *testing.T) {
	host, guest, tableID := createSyntheticRealStartedTableForRecoveryTest(t)

	started := mustReadNativeTable(t, host, tableID)
	withBundles := withLatestRecoveryBundlesForTest(t, host, started, started.ActiveHand.State)
	blindTransition := withBundles.CustodyTransitions[len(withBundles.CustodyTransitions)-1]
	if blindTransition.Kind != tablecustody.TransitionKindBlindPost {
		t.Fatalf("expected blind-post transition, got %s", blindTransition.Kind)
	}
	if len(blindTransition.Proof.RecoveryBundles) == 0 {
		t.Fatal("expected blind-post transition to store deterministic timeout recovery bundles")
	}

	waitForLocalCanAct(t, []*meshRuntime{host, guest}, host, tableID)
	if _, err := host.SendAction(tableID, game.Action{Type: game.ActionCall}); err != nil {
		t.Fatalf("host send call: %v", err)
	}
	afterCall := waitForActionLogLength(t, []*meshRuntime{host, guest}, guest, tableID, 1)
	withActionBundles := withLatestRecoveryBundlesForTest(t, host, afterCall, afterCall.ActiveHand.State)
	lastTransition := withActionBundles.CustodyTransitions[len(withActionBundles.CustodyTransitions)-1]
	if lastTransition.Kind != tablecustody.TransitionKindAction {
		t.Fatalf("expected action transition, got %s", lastTransition.Kind)
	}
	if len(lastTransition.Proof.RecoveryBundles) != 0 {
		t.Fatalf("expected auto-check state to skip recovery bundles, got %d", len(lastTransition.Proof.RecoveryBundles))
	}
}

func TestDeterministicRecoveryFastPathSkipsSourcesWithoutPotRefs(t *testing.T) {
	host, _, table, hand, blindTransition := manualBlindTransitionSourceForRecoveryTest(t)

	if deterministicRecoveryImpossibleForSource(table, &blindTransition, &hand) {
		t.Fatal("expected blind-post source to allow deterministic recovery")
	}

	noPotTransition := cloneJSON(blindTransition)
	noPotTransition.NextState.PotSlices = nil
	noPotTransition.Proof.RecoveryBundles = nil
	if !deterministicRecoveryImpossibleForSource(table, &noPotTransition, &hand) {
		t.Fatal("expected missing source pot refs to make deterministic recovery impossible")
	}
	if err := host.attachDeterministicRecoveryBundles(table, &noPotTransition, nil, &hand); err != nil {
		t.Fatalf("attach recovery bundles on no-pot source: %v", err)
	}
	if len(noPotTransition.Proof.RecoveryBundles) != 0 {
		t.Fatalf("expected no recovery bundles for no-pot source, got %d", len(noPotTransition.Proof.RecoveryBundles))
	}
}

func TestRecoveryBundleSourceContextReusePreservesBundleContents(t *testing.T) {
	host, _, table, hand, blindTransition := manualBlindTransitionSourceForRecoveryTest(t)

	targets, err := host.deterministicRecoveryTargetsForTransition(table, blindTransition, &hand)
	if err != nil {
		t.Fatalf("derive deterministic recovery targets: %v", err)
	}
	if len(targets) == 0 {
		t.Fatal("expected deterministic recovery targets")
	}

	var (
		target  tablecustody.CustodyTransition
		outputs []custodyBatchOutput
		found   bool
	)
	for _, candidate := range targets {
		outputs, err = host.recoveryAuthorizedOutputsForTransition(table, &blindTransition.NextState, candidate)
		if err != nil {
			t.Fatalf("derive recovery outputs: %v", err)
		}
		if len(outputs) == 0 {
			continue
		}
		target = candidate
		found = true
		break
	}
	if !found {
		t.Fatal("expected at least one recovery target with authorized outputs")
	}

	sourceCtx, err := host.prepareRecoverySourceContext(table, blindTransition)
	if err != nil {
		t.Fatalf("prepare recovery source context: %v", err)
	}
	if sourceCtx == nil {
		t.Fatal("expected recovery source context")
	}

	directBundle, err := host.buildRecoveryBundle(table, blindTransition, target, nil, outputs)
	if err != nil {
		t.Fatalf("build direct recovery bundle: %v", err)
	}
	reusedBundle, err := host.buildRecoveryBundleWithSourceContext(table, blindTransition, target, nil, outputs, sourceCtx)
	if err != nil {
		t.Fatalf("build reused recovery bundle: %v", err)
	}
	if !reflect.DeepEqual(directBundle, reusedBundle) {
		t.Fatalf("expected reused recovery bundle to match direct build\n direct=%+v\n reused=%+v", directBundle, reusedBundle)
	}
}

func TestSelectPotCSVExitSpendPathChoosesSharedCSVLeaf(t *testing.T) {
	host, _, tableID := createSyntheticRealStartedTableForRecoveryTest(t)

	table := mustReadNativeTable(t, host, tableID)
	sourceTransition := table.CustodyTransitions[len(table.CustodyTransitions)-1]
	sourceRefs := sourcePotRecoveryRefs(&sourceTransition.NextState)
	if len(sourceRefs) == 0 {
		t.Fatal("expected contested pot refs on blind-post transition")
	}
	spendPath, err := host.selectPotCSVExitSpendPath(table, sourceRefs[0])
	if err != nil {
		t.Fatalf("select csv spend path: %v", err)
	}
	if !spendPath.UsesCSVLocktime {
		t.Fatal("expected recovery spend path to use csv locktime")
	}
	if spendPath.UsesCLTVLocktime {
		t.Fatal("expected recovery spend path to avoid cltv locktime")
	}
	if len(spendPath.PlayerIDs) != 2 {
		t.Fatalf("expected both players on shared csv leaf, got %+v", spendPath.PlayerIDs)
	}
}

func TestRecoveryPSBTValidationAndRemoteSigningAuthorization(t *testing.T) {
	host, guest, table, hand, blindTransition := manualBlindTransitionSourceForRecoveryTest(t)
	tableID := table.Config.TableID

	targets, err := host.deterministicRecoveryTargetsForTransition(table, blindTransition, &hand)
	if err != nil {
		t.Fatalf("derive deterministic recovery targets: %v", err)
	}
	if len(targets) == 0 {
		t.Fatal("expected at least one deterministic blind-post recovery target")
	}
	outputs, err := host.recoveryAuthorizedOutputsForTransition(table, &blindTransition.NextState, targets[0])
	if err != nil {
		t.Fatalf("derive recovery outputs: %v", err)
	}
	bundle, err := host.buildRecoveryBundle(table, blindTransition, targets[0], nil, outputs)
	if err != nil {
		t.Fatalf("build recovery bundle: %v", err)
	}
	if bundle == nil {
		t.Fatal("expected recovery bundle")
	}
	if err := host.validateStoredRecoveryBundle(table, *bundle); err != nil {
		t.Fatalf("validate signed recovery bundle: %v", err)
	}
	sourceRefs := sourcePotRecoveryRefs(&blindTransition.NextState)
	spendPaths := make([]custodySpendPath, 0, len(sourceRefs))
	for _, ref := range sourceRefs {
		spendPath, err := host.selectPotCSVExitSpendPath(table, ref)
		if err != nil {
			t.Fatalf("select spend path: %v", err)
		}
		spendPaths = append(spendPaths, spendPath)
	}
	unsignedPSBT, err := recoveryUnsignedPSBT(sourceRefs, spendPaths, recoveryOutputsFromBundle(*bundle))
	if err != nil {
		t.Fatalf("build unsigned recovery psbt: %v", err)
	}
	if err := validateCustodyRecoveryPSBT(mustDecodePSBTForReplayTest(t, unsignedPSBT), sourceRefs, spendPaths, recoveryOutputsFromBundle(*bundle)); err != nil {
		t.Fatalf("validate unsigned recovery psbt: %v", err)
	}

	response, err := guest.handleCustodyTxSignFromPeer(nativeCustodyTxSignRequest{
		ExpectedPrevStateHash: blindTransition.PrevStateHash,
		ExpectedOutputs:       recoveryOutputsFromBundle(*bundle),
		PlayerID:              guest.walletID.PlayerID,
		ProtocolVersion:       nativeProtocolVersion,
		PSBT:                  unsignedPSBT,
		Purpose:               "recovery",
		TableID:               tableID,
		Transition:            blindTransition,
		TransitionHash:        blindTransition.Proof.RequestHash,
	})
	if err != nil {
		t.Fatalf("guest recovery signing request: %v", err)
	}
	signedPacket := mustDecodePSBTForReplayTest(t, response.SignedPSBT)
	if len(signedPacket.Inputs[0].TaprootScriptSpendSig) == 0 {
		t.Fatal("expected recovery signing response to append a script-spend signature")
	}

	_, err = guest.handleCustodyTxSignFromPeer(nativeCustodyTxSignRequest{
		ExpectedPrevStateHash: blindTransition.PrevStateHash,
		PlayerID:              guest.walletID.PlayerID,
		ProtocolVersion:       nativeProtocolVersion,
		PSBT:                  unsignedPSBT,
		Purpose:               "recovery",
		TableID:               tableID,
		Transition:            blindTransition,
		TransitionHash:        blindTransition.Proof.RequestHash,
	})
	if err == nil || !strings.Contains(err.Error(), "authorized outputs") {
		t.Fatalf("expected missing recovery outputs to be rejected, got %v", err)
	}
}

func TestActionTimeoutRecoveryMatchesCooperativeSuccessor(t *testing.T) {
	host, guest, before, _, _ := manualBlindTransitionSourceForRecoveryTest(t)

	before = withLatestRecoveryBundlesForTest(t, host, before, before.ActiveHand.State)
	if len(before.CustodyTransitions[len(before.CustodyTransitions)-1].Proof.RecoveryBundles) == 0 {
		t.Fatal("expected stored recovery bundles before timeout")
	}
	before.LatestCustodyState.ActionDeadlineAt = addMillis(nowISO(), -1)
	before.LatestCustodyState.PublicStateHash = host.publicMoneyStateHash(before, &before.ActiveHand.State)
	writeLocalRecoveryTableForTest(t, host, before)
	writeLocalRecoveryTableForTest(t, guest, before)
	actingSeatIndex := *before.ActiveHand.State.ActingSeatIndex
	actingPlayerID := seatPlayerID(before, actingSeatIndex)
	legalActions := game.GetLegalActions(before.ActiveHand.State, before.ActiveHand.State.ActingSeatIndex)
	actionTypes := make([]string, 0, len(legalActions))
	for _, legalAction := range legalActions {
		actionTypes = append(actionTypes, string(legalAction.Type))
	}
	resolution := tablecustody.BuildTimeoutResolution(defaultCustodyTimeoutPolicy, actingPlayerID, actionTypes, []string{actingPlayerID})
	action := game.Action{Type: game.ActionFold}
	nextState, err := game.ApplyHoldemAction(before.ActiveHand.State, actingSeatIndex, action)
	if err != nil {
		t.Fatalf("apply timeout fold: %v", err)
	}
	expectedTransition, err := host.buildCustodyTransition(before, tablecustody.TransitionKindTimeout, &nextState, &action, &resolution)
	if err != nil {
		t.Fatalf("build cooperative timeout transition: %v", err)
	}
	recoveredTransition := cloneJSON(expectedTransition)
	recoveredTable := cloneJSON(before)
	if handled, err := host.finalizeCustodyRecoveryTransition(&recoveredTable, &recoveredTransition, nil); err != nil {
		t.Fatalf("finalize timeout recovery: %v", err)
	} else if !handled {
		t.Fatal("expected timeout recovery bundle to finalize")
	}
	lastTransition := recoveredTransition
	if lastTransition.Proof.SettlementWitness != nil {
		t.Fatal("expected timeout recovery to avoid settlement witness")
	}
	if lastTransition.Proof.RecoveryWitness == nil {
		t.Fatal("expected timeout recovery witness")
	}
	if !reflect.DeepEqual(comparableSemanticCustodyTransition(lastTransition), comparableSemanticCustodyTransition(expectedTransition)) {
		t.Fatalf("expected recovered timeout to match cooperative semantics")
	}
}

func TestActionTimeoutRecoveryDoesNotRequirePersistedPendingTurnMenu(t *testing.T) {
	host, _, before, _, _ := manualBlindTransitionSourceForRecoveryTest(t)

	before = withLatestRecoveryBundlesForTest(t, host, before, before.ActiveHand.State)
	before.Config.TurnTimeoutMode = turnTimeoutModeDirect
	lastIndex := len(before.CustodyTransitions) - 1
	for index := range before.CustodyTransitions[lastIndex].Proof.RecoveryBundles {
		before.CustodyTransitions[lastIndex].Proof.RecoveryBundles[index].EarliestExecuteAt = addMillis(nowISO(), -1)
	}
	expireCurrentTurnActionDeadlineForTest(t, &before)
	before.LatestCustodyState.PublicStateHash = host.publicMoneyStateHash(before, &before.ActiveHand.State)
	before.PendingTurnMenu = nil

	handled, err := host.handleActionTimeoutLocked(&before)
	if err != nil {
		t.Fatalf("handle action timeout without pending menu: %v", err)
	}
	if !handled {
		t.Fatal("expected timeout recovery to finalize without a persisted pending turn menu")
	}
	lastTransition := before.CustodyTransitions[len(before.CustodyTransitions)-1]
	if lastTransition.Kind != tablecustody.TransitionKindTimeout {
		t.Fatalf("expected latest custody transition %q, got %q", tablecustody.TransitionKindTimeout, lastTransition.Kind)
	}
	if lastTransition.Proof.RecoveryWitness == nil {
		t.Fatal("expected timeout recovery witness on the finalized timeout transition")
	}
}

func TestRecoveryFailsClosedWithoutStoredSourceBundle(t *testing.T) {
	host, _, before, _, _ := manualBlindTransitionSourceForRecoveryTest(t)

	lastIndex := len(before.CustodyTransitions) - 1
	before.CustodyTransitions[lastIndex].Proof.RecoveryBundles = nil
	latest := cloneJSON(before.CustodyTransitions[lastIndex].NextState)
	before.LatestCustodyState = &latest
	before.LatestCustodyState.ActionDeadlineAt = addMillis(nowISO(), -1)
	before.LatestCustodyState.PublicStateHash = host.publicMoneyStateHash(before, &before.ActiveHand.State)

	actingSeatIndex := *before.ActiveHand.State.ActingSeatIndex
	actingPlayerID := seatPlayerID(before, actingSeatIndex)
	legalActions := game.GetLegalActions(before.ActiveHand.State, before.ActiveHand.State.ActingSeatIndex)
	actionTypes := make([]string, 0, len(legalActions))
	for _, legalAction := range legalActions {
		actionTypes = append(actionTypes, string(legalAction.Type))
	}
	resolution := tablecustody.BuildTimeoutResolution(defaultCustodyTimeoutPolicy, actingPlayerID, actionTypes, []string{actingPlayerID})
	action := game.Action{Type: game.ActionFold}
	nextState, err := game.ApplyHoldemAction(before.ActiveHand.State, actingSeatIndex, action)
	if err != nil {
		t.Fatalf("apply timeout fold: %v", err)
	}
	recoveredTransition, err := host.buildCustodyTransition(before, tablecustody.TransitionKindTimeout, &nextState, &action, &resolution)
	if err != nil {
		t.Fatalf("build timeout transition: %v", err)
	}

	recoveredTable := cloneJSON(before)
	handled, err := host.finalizeCustodyRecoveryTransition(&recoveredTable, &recoveredTransition, nil)
	if err != nil {
		t.Fatalf("finalize recovery without stored source bundle: %v", err)
	}
	if handled {
		t.Fatal("expected recovery to fail closed without a stored source bundle")
	}
	if recoveredTransition.Proof.RecoveryWitness != nil {
		t.Fatal("expected no recovery witness when no stored source bundle exists")
	}
	if len(recoveredTransition.Proof.RecoveryBundles) != 0 {
		t.Fatal("expected no inline recovery bundle fallback on the recovered transition")
	}
}

func TestShowdownRevealRecoveryMatchesCooperativePayoutSuccessor(t *testing.T) {
	t.Parallel()

	host, guest, tableID := createSyntheticRealStartedTableForRecoveryTest(t)

	waitForLocalCanAct(t, []*meshRuntime{host, guest}, host, tableID)
	if _, err := host.SendAction(tableID, game.Action{Type: game.ActionCall}); err != nil {
		t.Fatalf("host send preflop call: %v", err)
	}
	waitForLocalCanAct(t, []*meshRuntime{host, guest}, guest, tableID)
	if _, err := guest.SendAction(tableID, aggressiveActionForTable(t, mustReadNativeTable(t, guest, tableID))); err != nil {
		t.Fatalf("guest send preflop bet: %v", err)
	}
	waitForLocalCanAct(t, []*meshRuntime{host, guest}, host, tableID)
	if _, err := host.SendAction(tableID, passiveActionForTable(t, mustReadNativeTable(t, host, tableID))); err != nil {
		t.Fatalf("host send preflop call to showdown line: %v", err)
	}
	for index, actor := range []*meshRuntime{guest, host, guest, host, guest, host} {
		waitForLocalCanAct(t, []*meshRuntime{host, guest}, actor, tableID)
		if _, err := actor.SendAction(tableID, game.Action{Type: game.ActionCheck}); err != nil {
			t.Fatalf("send river-line check %d: %v", index, err)
		}
	}

	before := waitForHandPhase(t, []*meshRuntime{host, guest}, host, tableID, game.StreetShowdownReveal)
	before = withLatestRecoveryBundlesForTest(t, host, before, before.ActiveHand.State)
	if len(before.CustodyTransitions[len(before.CustodyTransitions)-1].Proof.RecoveryBundles) == 0 {
		t.Fatal("expected showdown-reveal source transition to store recovery bundles")
	}

	resolution := &tablecustody.TimeoutResolution{
		ActionType:               string(game.ActionFold),
		ActingPlayerID:           guest.walletID.PlayerID,
		DeadPlayerIDs:            []string{guest.walletID.PlayerID},
		LostEligibilityPlayerIDs: []string{guest.walletID.PlayerID},
		Policy:                   defaultCustodyTimeoutPolicy,
		Reason:                   "protocol timeout during showdown-reveal",
	}
	nextState, err := game.ForceFoldSeat(before.ActiveHand.State, 1)
	if err != nil {
		t.Fatalf("force fold missing showdown player: %v", err)
	}
	expectedBase := cloneJSON(before)
	expectedBase.ActiveHand.State = nextState
	publicState := host.publicStateFromHand(expectedBase, nextState)
	expectedBase.PublicState = &publicState
	expectedBase.ActiveHand.Cards.PhaseDeadlineAt = ""
	expectedTransition, err := host.buildCustodyTransition(expectedBase, tablecustody.TransitionKindShowdownPayout, &nextState, nil, resolution)
	if err != nil {
		t.Fatalf("build cooperative showdown-timeout transition: %v", err)
	}

	recoveredTransition := cloneJSON(expectedTransition)
	recoveredTable := cloneJSON(before)
	if handled, err := host.finalizeCustodyRecoveryTransition(&recoveredTable, &recoveredTransition, nil); err != nil {
		t.Fatalf("finalize showdown recovery: %v", err)
	} else if !handled {
		t.Fatal("expected showdown recovery bundle to finalize")
	}
	lastTransition := recoveredTransition
	if lastTransition.Proof.SettlementWitness != nil {
		t.Fatal("expected showdown recovery to avoid settlement witness")
	}
	if lastTransition.Proof.RecoveryWitness == nil {
		t.Fatal("expected showdown recovery witness")
	}
	if !reflect.DeepEqual(comparableSemanticCustodyTransition(lastTransition), comparableSemanticCustodyTransition(expectedTransition)) {
		t.Fatalf("expected recovered showdown payout to match cooperative semantics")
	}
}

func TestAcceptedCustodyHistoryReplaysRecoveryWitnessOfflineAndRejectsTampering(t *testing.T) {
	host, guest, before, _, _ := manualBlindTransitionSourceForRecoveryTest(t)
	before = withLatestRecoveryBundlesForTest(t, host, before, before.ActiveHand.State)
	before.LatestCustodyState.ActionDeadlineAt = addMillis(nowISO(), -1)
	before.LatestCustodyState.PublicStateHash = host.publicMoneyStateHash(before, &before.ActiveHand.State)
	writeLocalRecoveryTableForTest(t, host, before)
	writeLocalRecoveryTableForTest(t, guest, before)
	actingSeatIndex := *before.ActiveHand.State.ActingSeatIndex
	actingPlayerID := seatPlayerID(before, actingSeatIndex)
	legalActions := game.GetLegalActions(before.ActiveHand.State, before.ActiveHand.State.ActingSeatIndex)
	actionTypes := make([]string, 0, len(legalActions))
	for _, legalAction := range legalActions {
		actionTypes = append(actionTypes, string(legalAction.Type))
	}
	resolution := tablecustody.BuildTimeoutResolution(defaultCustodyTimeoutPolicy, actingPlayerID, actionTypes, []string{actingPlayerID})
	action := game.Action{Type: game.ActionFold}
	nextState, err := game.ApplyHoldemAction(before.ActiveHand.State, actingSeatIndex, action)
	if err != nil {
		t.Fatalf("apply timeout fold: %v", err)
	}
	recoveredTransition, err := host.buildCustodyTransition(before, tablecustody.TransitionKindTimeout, &nextState, &action, &resolution)
	if err != nil {
		t.Fatalf("build timeout transition: %v", err)
	}
	recovered := cloneJSON(before)
	if handled, err := host.finalizeCustodyRecoveryTransition(&recovered, &recoveredTransition, nil); err != nil {
		t.Fatalf("finalize recovery transition: %v", err)
	} else if !handled {
		t.Fatal("expected recovery bundle to finalize")
	}
	recovered.CustodyTransitions = append(recovered.CustodyTransitions, recoveredTransition)
	latest := cloneJSON(recoveredTransition.NextState)
	recovered.LatestCustodyState = &latest

	arkVerifyCalls := 0
	host.custodyArkVerify = func(refs []tablecustody.VTXORef, requireSpendable bool) error {
		arkVerifyCalls++
		return errors.New("ark unavailable")
	}
	if err := host.validateAcceptedCustodyHistory(nil, recovered); err != nil {
		t.Fatalf("expected recovery witness to replay offline, got %v", err)
	}
	if arkVerifyCalls != 0 {
		t.Fatalf("expected recovery replay to avoid live Ark verification, got %d calls", arkVerifyCalls)
	}

	sourceIndex := len(recovered.CustodyTransitions) - 2
	tamperedBundle := tamperAcceptedCustodyTransitionForTest(recovered, sourceIndex, func(transition *tablecustody.CustodyTransition) {
		packet := mustDecodePSBTForReplayTest(t, transition.Proof.RecoveryBundles[0].SignedPSBT)
		packet.UnsignedTx.TxOut[0].Value++
		transition.Proof.RecoveryBundles[0].SignedPSBT = mustEncodePSBTForReplayTest(t, packet)
	})
	if err := host.validateAcceptedCustodyHistory(nil, tamperedBundle); err == nil {
		t.Fatal("expected tampered recovery bundle to be rejected")
	}
	tamperedSignatureBundle := tamperAcceptedCustodyTransitionForTest(recovered, sourceIndex, func(transition *tablecustody.CustodyTransition) {
		packet := mustDecodePSBTForReplayTest(t, transition.Proof.RecoveryBundles[0].SignedPSBT)
		if len(packet.Inputs[0].TaprootScriptSpendSig) == 0 {
			t.Fatal("expected recovery bundle signatures")
		}
		packet.Inputs[0].TaprootScriptSpendSig[0].Signature[0] ^= 0x01
		transition.Proof.RecoveryBundles[0].SignedPSBT = mustEncodePSBTForReplayTest(t, packet)
		transition.Proof.RecoveryBundles[0].BundleHash = tablecustody.HashCustodyRecoveryBundle(transition.Proof.RecoveryBundles[0])
	})
	if err := host.validateAcceptedCustodyHistory(nil, tamperedSignatureBundle); err == nil || !strings.Contains(strings.ToLower(err.Error()), "witness") {
		t.Fatalf("expected tampered recovery signature to be rejected, got %v", err)
	}

	witnessIndex := len(recovered.CustodyTransitions) - 1
	tamperedWitness := tamperAcceptedCustodyTransitionForTest(recovered, witnessIndex, func(transition *tablecustody.CustodyTransition) {
		transition.Proof.RecoveryWitness.RecoveryTxID = strings.Repeat("1", 64)
	})
	if err := host.validateAcceptedCustodyHistory(nil, tamperedWitness); err == nil || !strings.Contains(strings.ToLower(err.Error()), "txid") {
		t.Fatalf("expected tampered recovery witness to be rejected, got %v", err)
	}
}
