package meshruntime

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	sdktypes "github.com/arkade-os/go-sdk/types"
	"github.com/btcsuite/btcd/wire"
	cfg "github.com/parkerpoker/parkerd/internal/config"
	"github.com/parkerpoker/parkerd/internal/game"
	"github.com/parkerpoker/parkerd/internal/settlementcore"
	"github.com/parkerpoker/parkerd/internal/tablecustody"
)

func TestTableTrafficKeepsHoleCardsOwnerLocalAndPushesTranscriptUpdates(t *testing.T) {
	t.Parallel()

	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)

	joinedTable := mustReadNativeTable(t, guest, tableID)
	assertTranscriptProtectedCards(t, joinedTable)
	assertOwnerLocalCards(t, guest, joinedTable)

	anonymous := mustFetchNativeTableWithoutAuth(t, host, tableID)
	assertTranscriptProtectedCards(t, anonymous)

	fetched, err := guest.fetchRemoteTable(host.selfPeerURL(), tableID)
	if err != nil {
		t.Fatalf("fetch remote table: %v", err)
	}
	assertTranscriptProtectedCards(t, *fetched)

	waitForLocalCanAct(t, []*meshRuntime{host, guest}, host, tableID)
	if _, err := host.SendAction(tableID, game.Action{Type: game.ActionCall}); err != nil {
		t.Fatalf("host send action: %v", err)
	}
	afterHostAction := waitForActionLogLength(t, []*meshRuntime{host, guest}, guest, tableID, 1)
	assertTranscriptProtectedCards(t, afterHostAction)
	assertOwnerLocalCards(t, guest, afterHostAction)

	waitForLocalCanAct(t, []*meshRuntime{host, guest}, guest, tableID)
	if _, err := guest.SendAction(tableID, game.Action{Type: game.ActionCheck}); err != nil {
		t.Fatalf("guest send action: %v", err)
	}
	afterGuestAction := waitForActionLogLength(t, []*meshRuntime{host, guest}, guest, tableID, 2)
	assertTranscriptProtectedCards(t, afterGuestAction)
	assertOwnerLocalCards(t, guest, afterGuestAction)
}

func TestGuestSendActionWaitsForSlowReplicationTargetsInParallel(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")
	witness := newMeshTestRuntime(t, "witness")

	if _, err := host.BootstrapPeer(witness.selfPeerURL(), "", nil); err != nil {
		t.Fatalf("bootstrap witness peer: %v", err)
	}
	tableID, _ := createStartedTwoPlayerTable(t, host, guest, witness.selfPeerID())

	waitForLocalCanAct(t, []*meshRuntime{host, guest, witness}, host, tableID)
	if _, err := host.SendAction(tableID, game.Action{Type: game.ActionCall}); err != nil {
		t.Fatalf("host send action: %v", err)
	}
	waitForActionLogLength(t, []*meshRuntime{host, guest, witness}, host, tableID, 1)

	var syncMu sync.Mutex
	callCount := 0
	inFlight := 0
	maxInFlight := 0
	host.tableSyncSender = func(peerURL string, input nativeTableSyncRequest) error {
		syncMu.Lock()
		callCount++
		inFlight++
		if inFlight > maxInFlight {
			maxInFlight = inFlight
		}
		syncMu.Unlock()
		time.Sleep(700 * time.Millisecond)
		syncMu.Lock()
		inFlight--
		syncMu.Unlock()
		return nil
	}

	if _, err := guest.SendAction(tableID, game.Action{Type: game.ActionCheck}); err != nil {
		t.Fatalf("guest send action: %v", err)
	}
	syncMu.Lock()
	defer syncMu.Unlock()
	if callCount < 2 {
		t.Fatalf("expected at least two replication targets, got %d", callCount)
	}
	if maxInFlight < 2 {
		t.Fatalf("expected replication fanout to overlap in parallel, got max in-flight %d across %d calls", maxInFlight, callCount)
	}
}

func TestLocalTableViewHidesLegalActionsWhenItIsNotYourTurn(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)
	table := waitForLocalCanAct(t, []*meshRuntime{host, guest}, host, tableID)

	hostLocal := host.localTableView(table).Local
	if !hostLocal.CanAct {
		t.Fatal("expected host to be able to act")
	}
	if len(hostLocal.LegalActions) == 0 {
		t.Fatal("expected acting seat to receive legal actions")
	}

	guestTable := mustReadNativeTable(t, guest, tableID)
	guestLocal := guest.localTableView(guestTable).Local
	if guestLocal.CanAct {
		t.Fatal("expected guest to be waiting")
	}
	if len(guestLocal.LegalActions) != 0 {
		t.Fatalf("expected waiting seat legal actions to be hidden, got %#v", guestLocal.LegalActions)
	}
}

func TestHandleActionRejectsForgedSeatOwnerSignature(t *testing.T) {
	t.Parallel()

	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)
	table := mustReadNativeTable(t, host, tableID)
	coordinator := runtimeForPeerID(t, table.CurrentHost.Peer.PeerID, host, guest)

	signedAt := nowISO()
	forged := nativeActionRequest{
		Action:               game.Action{Type: game.ActionCall},
		ChallengeAnchor:      firstNonEmptyString(handTranscriptRoot(table), table.LastEventHash),
		DecisionIndex:        len(table.ActiveHand.State.ActionLog),
		Epoch:                table.CurrentEpoch,
		HandID:               table.ActiveHand.State.HandID,
		PlayerID:             host.walletID.PlayerID,
		PrevCustodyStateHash: latestCustodyStateHash(table),
		ProfileName:          guest.profileName,
		SignedAt:             signedAt,
		TableID:              tableID,
		TranscriptRoot:       handTranscriptRoot(table),
	}
	signatureHex, err := settlementcore.SignStructuredData(guest.walletID.PrivateKeyHex, nativeActionAuthPayload(forged.TableID, forged.PlayerID, forged.HandID, forged.PrevCustodyStateHash, forged.ChallengeAnchor, forged.TranscriptRoot, forged.Epoch, forged.DecisionIndex, forged.Action, forged.SignedAt))
	if err != nil {
		t.Fatalf("sign forged action: %v", err)
	}
	forged.SignatureHex = signatureHex

	if _, err := coordinator.handleActionFromPeer(forged); err == nil || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("expected forged action to be rejected with signature error, got %v", err)
	}
}

func TestHandleActionRejectsReplayedDecisionSignature(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)
	table := mustReadNativeTable(t, host, tableID)
	coordinator := runtimeForPeerID(t, table.CurrentHost.Peer.PeerID, host, guest)
	waitForLocalCanAct(t, []*meshRuntime{host, guest}, host, tableID)

	staleCall, err := host.buildSignedActionRequest(table, game.Action{Type: game.ActionCall})
	if err != nil {
		t.Fatalf("build stale call request: %v", err)
	}

	if _, err := host.SendAction(tableID, game.Action{Type: game.ActionRaise, TotalSats: 200}); err != nil {
		t.Fatalf("host send raise: %v", err)
	}
	waitForLocalCanAct(t, []*meshRuntime{host, guest}, guest, tableID)
	if _, err := guest.SendAction(tableID, game.Action{Type: game.ActionCall}); err != nil {
		t.Fatalf("guest send preflop call: %v", err)
	}
	waitForHandPhase(t, []*meshRuntime{host, guest}, host, tableID, game.StreetFlop)
	waitForLocalCanAct(t, []*meshRuntime{host, guest}, guest, tableID)
	if _, err := guest.SendAction(tableID, game.Action{Type: game.ActionBet, TotalSats: 100}); err != nil {
		t.Fatalf("guest send flop bet: %v", err)
	}

	if _, err := coordinator.handleActionFromPeer(staleCall); err == nil || !strings.Contains(err.Error(), "decision mismatch") {
		t.Fatalf("expected stale call to be rejected with decision mismatch, got %v", err)
	}

	current := mustReadNativeTable(t, host, tableID)
	freshCall, err := host.buildSignedActionRequest(current, game.Action{Type: game.ActionCall})
	if err != nil {
		t.Fatalf("build fresh call request: %v", err)
	}
	if freshCall.DecisionIndex != len(current.ActiveHand.State.ActionLog) {
		t.Fatalf("expected fresh decision index %d, got %d", len(current.ActiveHand.State.ActionLog), freshCall.DecisionIndex)
	}
	currentCoordinator := runtimeForPeerID(t, current.CurrentHost.Peer.PeerID, host, guest)
	if _, err := currentCoordinator.handleActionFromPeer(freshCall); err != nil {
		t.Fatalf("expected fresh call request to succeed, got %v", err)
	}
}

func TestHandleHandMessageRejectsReplayedCommit(t *testing.T) {
	t.Parallel()

	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)
	table := mustReadNativeTable(t, host, tableID)
	guestSeat := seatIndexForPlayer(t, table, guest.walletID.PlayerID)
	commitRecord, ok := findTranscriptRecord(table.ActiveHand.Cards.Transcript, nativeHandMessageFairnessCommit, &guestSeat, string(game.StreetCommitment), nil)
	if !ok {
		t.Fatal("expected guest fairness commit in transcript")
	}

	replayed := nativeHandMessageRequest{
		CommitmentHash: commitRecord.CommitmentHash,
		Epoch:          table.CurrentEpoch,
		HandID:         table.ActiveHand.State.HandID,
		HandNumber:     table.ActiveHand.State.HandNumber,
		Kind:           nativeHandMessageFairnessCommit,
		Phase:          string(game.StreetCommitment),
		PlayerID:       guest.walletID.PlayerID,
		ProfileName:    guest.profileName,
		SeatIndex:      guestSeat,
		SignedAt:       nowISO(),
		TableID:        tableID,
	}
	signatureHex, err := settlementcore.SignStructuredData(guest.walletID.PrivateKeyHex, nativeHandMessageAuthPayload(replayed))
	if err != nil {
		t.Fatalf("sign replayed commit: %v", err)
	}
	replayed.SignatureHex = signatureHex

	if _, err := host.handleHandMessageFromPeer(replayed); err == nil || (!strings.Contains(err.Error(), "commit") && !strings.Contains(err.Error(), "accepting")) {
		t.Fatalf("expected replayed commit to be rejected, got %v", err)
	}
}

func TestHandleHandMessageRejectsPlaintextCardsInCommit(t *testing.T) {
	t.Parallel()

	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createJoinedTwoPlayerTable(t, host, guest)
	startNextHandForTest(t, host, tableID)

	table := mustReadNativeTable(t, host, tableID)
	if table.ActiveHand == nil || table.ActiveHand.State.Phase != game.StreetCommitment {
		t.Fatalf("expected commitment phase, got %+v", table.ActiveHand)
	}

	record, err := guest.buildLocalContributionRecord(table)
	if err != nil {
		t.Fatalf("build guest local contribution record: %v", err)
	}
	if record == nil || record.Kind != nativeHandMessageFairnessCommit {
		t.Fatalf("expected guest fairness commit record, got %#v", record)
	}

	request, err := guest.buildSignedHandMessageRequest(table, *record)
	if err != nil {
		t.Fatalf("build guest hand message request: %v", err)
	}
	request.Cards = []string{"As", "Kd"}
	signatureHex, err := settlementcore.SignStructuredData(guest.walletID.PrivateKeyHex, nativeHandMessageAuthPayload(request))
	if err != nil {
		t.Fatalf("sign tampered hand message: %v", err)
	}
	request.SignatureHex = signatureHex

	if _, err := host.handleHandMessageFromPeer(request); err == nil || !strings.Contains(err.Error(), "plaintext cards") {
		t.Fatalf("expected plaintext-card hand message to be rejected, got %v", err)
	}
}

func TestFailoverKeepsActiveTranscriptDrivenHandRunning(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	player := newMeshTestRuntime(t, "player")
	witness := newMeshTestRuntime(t, "witness")

	if _, err := host.BootstrapPeer(witness.selfPeerURL(), "", nil); err != nil {
		t.Fatalf("bootstrap witness peer: %v", err)
	}
	tableID, _ := createStartedTwoPlayerTable(t, host, player, witness.selfPeerID())
	hostTable := waitForHandPhase(t, []*meshRuntime{host, player, witness}, host, tableID, game.StreetPreflop)
	var table nativeTableState
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		table = mustReadNativeTable(t, witness, tableID)
		if table.ActiveHand != nil &&
			table.ActiveHand.State.HandID == hostTable.ActiveHand.State.HandID &&
			table.ActiveHand.Cards.Transcript.RootHash == hostTable.ActiveHand.Cards.Transcript.RootHash {
			break
		}
		host.Tick()
		player.Tick()
		witness.Tick()
		time.Sleep(25 * time.Millisecond)
	}
	if table.ActiveHand == nil || table.ActiveHand.Cards.Transcript.RootHash != hostTable.ActiveHand.Cards.Transcript.RootHash {
		t.Fatalf("expected witness transcript root %q before failover, got %+v", hostTable.ActiveHand.Cards.Transcript.RootHash, table.ActiveHand)
	}
	originalHandID := table.ActiveHand.State.HandID
	originalRoot := table.ActiveHand.Cards.Transcript.RootHash
	table.LastHostHeartbeatAt = addMillis(nowISO(), -(nativeHostFailureMS + 100))
	if err := witness.store.writeTable(&table); err != nil {
		t.Fatalf("write stale witness table: %v", err)
	}

	if err := witness.failoverTable(tableID, "missed host heartbeats"); err != nil {
		t.Fatalf("failover table: %v", err)
	}

	failedOver := mustReadNativeTable(t, witness, tableID)
	if failedOver.CurrentHost.Peer.PeerID != witness.selfPeerID() {
		t.Fatalf("expected witness to become host, got %q", failedOver.CurrentHost.Peer.PeerID)
	}
	if failedOver.ActiveHand == nil {
		t.Fatal("expected active hand to remain available after failover")
	}
	if failedOver.ActiveHand.State.HandID != originalHandID {
		t.Fatalf("expected hand %q after failover, got %q", originalHandID, failedOver.ActiveHand.State.HandID)
	}
	if failedOver.ActiveHand.Cards.Transcript.RootHash != originalRoot {
		t.Fatalf("expected transcript root %q after failover, got %q", originalRoot, failedOver.ActiveHand.Cards.Transcript.RootHash)
	}
	if tableHasEventType(failedOver, "HandAbort") {
		t.Fatal("did not expect failover to abort the active hand")
	}

	waitForLocalCanAct(t, []*meshRuntime{host, player, witness}, host, tableID)
	if _, err := host.SendAction(tableID, game.Action{Type: game.ActionCall}); err != nil {
		t.Fatalf("host send action after failover: %v", err)
	}
	afterAction := waitForActionLogLength(t, []*meshRuntime{host, player, witness}, witness, tableID, 1)
	if got := len(afterAction.ActiveHand.State.ActionLog); got != 1 {
		t.Fatalf("expected resumed hand action log length 1, got %d", got)
	}
}

func TestMissingRevealTimeoutAwardsPotAndAppendsAbort(t *testing.T) {
	t.Parallel()

	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createJoinedTwoPlayerTable(t, host, guest)
	startNextHandForTest(t, host, tableID)
	if err := guest.Close(); err != nil {
		t.Fatalf("close guest runtime: %v", err)
	}
	settleDeadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(settleDeadline) {
		host.Tick()
		table := mustReadNativeTable(t, host, tableID)
		if table.ActiveHand != nil && table.ActiveHand.State.Phase == game.StreetSettled {
			if !tableHasEventType(table, "HandAbort") {
				t.Fatal("expected HandAbort event after timeout")
			}
			if len(table.ActiveHand.State.Winners) != 1 || table.ActiveHand.State.Winners[0].PlayerID != host.walletID.PlayerID {
				t.Fatalf("expected host to win timeout-forfeited hand, got %+v", table.ActiveHand.State.Winners)
			}
			if _, err := host.CashOut(tableID); err != nil {
				t.Fatalf("cash out after timeout-settled hand: %v", err)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("timed out waiting for missing reveal timeout to settle the hand")
}

func TestSyntheticRealModeMissingShowdownRevealTimeoutSettles(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)
	enableSyntheticRealMode(host, guest)

	waitForLocalCanAct(t, []*meshRuntime{host, guest}, host, tableID)
	if _, err := host.SendAction(tableID, game.Action{Type: game.ActionCall}); err != nil {
		t.Fatalf("host send preflop call: %v", err)
	}
	waitForLocalCanAct(t, []*meshRuntime{host, guest}, guest, tableID)
	if _, err := guest.SendAction(tableID, game.Action{Type: game.ActionBet, TotalSats: 800}); err != nil {
		t.Fatalf("guest send preflop bet: %v", err)
	}
	waitForLocalCanAct(t, []*meshRuntime{host, guest}, host, tableID)
	if _, err := host.SendAction(tableID, game.Action{Type: game.ActionCall}); err != nil {
		t.Fatalf("host send preflop call to showdown line: %v", err)
	}
	for index, actor := range []*meshRuntime{guest, host, guest, host, guest, host} {
		waitForLocalCanAct(t, []*meshRuntime{host, guest}, actor, tableID)
		if _, err := actor.SendAction(tableID, game.Action{Type: game.ActionCheck}); err != nil {
			t.Fatalf("send river-line check %d: %v", index, err)
		}
	}

	waitForHandPhase(t, []*meshRuntime{host, guest}, host, tableID, game.StreetShowdownReveal)
	if err := guest.Close(); err != nil {
		t.Fatalf("close guest runtime: %v", err)
	}
	settled := waitForSettledHand(t, []*meshRuntime{host}, host, tableID)
	if len(settled.ActiveHand.State.Winners) != 1 || settled.ActiveHand.State.Winners[0].PlayerID != host.walletID.PlayerID {
		t.Fatalf("expected host to win timeout-forfeited showdown, got %+v", settled.ActiveHand.State.Winners)
	}
	lastTransition := settled.CustodyTransitions[len(settled.CustodyTransitions)-1]
	if lastTransition.Kind != tablecustody.TransitionKindShowdownPayout {
		t.Fatalf("expected showdown payout transition after timeout, got %s", lastTransition.Kind)
	}
	if lastTransition.TimeoutResolution == nil {
		t.Fatal("expected showdown payout timeout resolution")
	}
	if !slices.Contains(lastTransition.TimeoutResolution.LostEligibilityPlayerIDs, guest.walletID.PlayerID) {
		t.Fatalf("expected guest to lose pot eligibility on timeout, got %+v", lastTransition.TimeoutResolution)
	}
	if len(lastTransition.Approvals) != 1 || lastTransition.Approvals[0].PlayerID != host.walletID.PlayerID {
		t.Fatalf("expected timeout payout approvals to exclude guest, got %+v", lastTransition.Approvals)
	}
}

func TestShowdownPayoutPlanConsumesRemovedPotRefs(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)
	enableSyntheticRealMode(host, guest)

	waitForLocalCanAct(t, []*meshRuntime{host, guest}, host, tableID)
	if _, err := host.SendAction(tableID, game.Action{Type: game.ActionCall}); err != nil {
		t.Fatalf("host send preflop call: %v", err)
	}
	waitForLocalCanAct(t, []*meshRuntime{host, guest}, guest, tableID)
	if _, err := guest.SendAction(tableID, game.Action{Type: game.ActionBet, TotalSats: 800}); err != nil {
		t.Fatalf("guest send preflop bet: %v", err)
	}
	waitForLocalCanAct(t, []*meshRuntime{host, guest}, host, tableID)
	if _, err := host.SendAction(tableID, game.Action{Type: game.ActionCall}); err != nil {
		t.Fatalf("host send preflop showdown call: %v", err)
	}
	for index, actor := range []*meshRuntime{guest, host, guest, host, guest, host} {
		waitForLocalCanAct(t, []*meshRuntime{host, guest}, actor, tableID)
		if _, err := actor.SendAction(tableID, game.Action{Type: game.ActionCheck}); err != nil {
			t.Fatalf("send showdown-line check %d: %v", index, err)
		}
	}

	table := waitForHandPhase(t, []*meshRuntime{host, guest}, host, tableID, game.StreetShowdownReveal)
	if len(table.LatestCustodyState.PotSlices) == 0 {
		t.Fatal("expected live custody pot before showdown timeout")
	}
	previousPotInputSats := 0
	for _, slice := range table.LatestCustodyState.PotSlices {
		previousPotInputSats += sumVTXORefs(slice.VTXORefs)
	}
	if previousPotInputSats == 0 {
		t.Fatal("expected prior pot refs to carry spendable value")
	}

	resolution := &tablecustody.TimeoutResolution{
		ActionType:               string(game.ActionFold),
		ActingPlayerID:           guest.walletID.PlayerID,
		DeadPlayerIDs:            []string{guest.walletID.PlayerID},
		LostEligibilityPlayerIDs: []string{guest.walletID.PlayerID},
		Policy:                   defaultCustodyTimeoutPolicy,
		Reason:                   "protocol timeout during showdown-reveal",
	}
	nextState, err := game.ForceFoldSeat(table.ActiveHand.State, 1)
	if err != nil {
		t.Fatalf("force fold missing showdown player: %v", err)
	}
	transition, err := host.buildCustodyTransition(table, tablecustody.TransitionKindShowdownPayout, &nextState, nil, resolution)
	if err != nil {
		t.Fatalf("build showdown payout transition: %v", err)
	}
	plan, err := host.buildCustodySettlementPlan(table, transition)
	if err != nil {
		t.Fatalf("build showdown payout settlement plan: %v", err)
	}

	potInputSats := 0
	inputSum := 0
	for _, input := range plan.Inputs {
		inputSum += input.Ref.AmountSats
		if strings.HasPrefix(input.ClaimKey, "pot:") {
			potInputSats += input.Ref.AmountSats
		}
	}
	outputSum := 0
	for _, output := range plan.Outputs {
		outputSum += output.AmountSats
	}
	if potInputSats != previousPotInputSats {
		t.Fatalf("expected settlement plan to consume %d sats of removed pot refs, got %d", previousPotInputSats, potInputSats)
	}
	if inputSum < outputSum {
		t.Fatalf("expected showdown payout inputs to cover outputs, got inputs=%d outputs=%d", inputSum, outputSum)
	}
}

func TestHandleJoinRejectsPeerEndpointMismatch(t *testing.T) {
	t.Parallel()

	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	createResult, err := host.CreateTable(map[string]any{"name": "Join Validation Table"})
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	tableID := stringValue(rawJSONMap(createResult["table"])["tableId"])
	if tableID == "" {
		t.Fatal("expected table id from create result")
	}

	request := mustBuildJoinRequest(t, guest, tableID, host.selfPeerURL())
	if _, err := host.handleJoinFromPeer(request); err == nil || !strings.Contains(err.Error(), "peer endpoint") {
		t.Fatalf("expected peer endpoint verification failure, got %v", err)
	}
}

func TestCreatedTablesListsOwnedTablesWithPaginationAndInvites(t *testing.T) {
	t.Parallel()

	host := newMeshTestRuntime(t, "host")

	createTable := func(name, visibility string) string {
		t.Helper()

		result, err := host.CreateTable(map[string]any{
			"name":       name,
			"visibility": visibility,
		})
		if err != nil {
			t.Fatalf("create table %q: %v", name, err)
		}
		inviteCode := stringValue(result["inviteCode"])
		if inviteCode == "" {
			t.Fatalf("expected invite code for %q", name)
		}
		return inviteCode
	}

	firstInvite := createTable("First Private Table", "private")
	time.Sleep(1100 * time.Millisecond)
	secondInvite := createTable("Second Public Table", "public")
	time.Sleep(1100 * time.Millisecond)
	thirdInvite := createTable("Third Private Table", "private")

	pageOne, err := host.CreatedTables("", 2)
	if err != nil {
		t.Fatalf("created tables page one: %v", err)
	}
	if len(pageOne.Items) != 2 {
		t.Fatalf("expected 2 created tables on first page, got %d", len(pageOne.Items))
	}
	if pageOne.Items[0].Config.Name != "Third Private Table" || pageOne.Items[1].Config.Name != "Second Public Table" {
		t.Fatalf("unexpected first page order: %+v", pageOne.Items)
	}
	if pageOne.Items[0].Config.Visibility != "private" || pageOne.Items[1].Config.Visibility != "public" {
		t.Fatalf("unexpected first page visibilities: %+v", pageOne.Items)
	}
	if pageOne.Items[0].InviteCode != thirdInvite || pageOne.Items[1].InviteCode != secondInvite {
		t.Fatalf("unexpected first page invite codes: %+v", pageOne.Items)
	}
	if pageOne.NextCursor == "" {
		t.Fatal("expected first page next cursor")
	}

	pageTwo, err := host.CreatedTables(pageOne.NextCursor, 2)
	if err != nil {
		t.Fatalf("created tables page two: %v", err)
	}
	if len(pageTwo.Items) != 1 {
		t.Fatalf("expected 1 created table on second page, got %d", len(pageTwo.Items))
	}
	if pageTwo.Items[0].Config.Name != "First Private Table" {
		t.Fatalf("unexpected second page item: %+v", pageTwo.Items[0])
	}
	if pageTwo.Items[0].InviteCode != firstInvite {
		t.Fatalf("expected second page invite code %q, got %q", firstInvite, pageTwo.Items[0].InviteCode)
	}
	if pageTwo.NextCursor != "" {
		t.Fatalf("expected second page next cursor to be empty, got %q", pageTwo.NextCursor)
	}
}

func TestSyncRouteRejectsForgedEnvelope(t *testing.T) {
	t.Parallel()

	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)

	before := mustReadNativeTable(t, guest, tableID)
	syncRequest, err := host.buildTableSyncRequest(host.networkTableView(mustReadNativeTable(t, host, tableID), guest.walletID.PlayerID))
	if err != nil {
		t.Fatalf("build sync request: %v", err)
	}
	syncRequest.Table.CurrentEpoch++
	guestInfo, err := host.fetchPeerInfo(guest.selfPeerURL())
	if err != nil {
		t.Fatalf("fetch guest peer info: %v", err)
	}
	request, requestKey, err := host.newOutboundEnvelope(
		nativeTransportMessageTablePush,
		nativeTransportChannelSync,
		tableID,
		guestInfo.Peer.PeerID,
		syncRequest,
		guestInfo.TransportPubkeyHex,
	)
	if err != nil {
		t.Fatalf("build forged transport envelope: %v", err)
	}
	response, err := host.exchangePeerTransport(guest.selfPeerURL(), request)
	if err != nil {
		t.Fatalf("send forged sync envelope: %v", err)
	}
	if _, err := host.decodeResponseEnvelope(response, requestKey); err == nil || (!strings.Contains(err.Error(), "signature") && !strings.Contains(err.Error(), "host")) {
		t.Fatalf("expected forged sync to be rejected with signature error, got %v", err)
	}
	after := mustReadNativeTable(t, guest, tableID)
	if after.CurrentEpoch != before.CurrentEpoch {
		t.Fatalf("expected guest table epoch to remain %d, got %d", before.CurrentEpoch, after.CurrentEpoch)
	}
}

func TestAcceptRemoteTableRejectsTamperedTranscript(t *testing.T) {
	t.Parallel()

	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)

	tampered := mustReadNativeTable(t, host, tableID)
	if tampered.ActiveHand == nil {
		t.Fatal("expected active hand to tamper")
	}
	tampered.ActiveHand.Cards.Transcript.RootHash = strings.Repeat("0", 64)
	if tampered.PublicState != nil && tampered.PublicState.DealerCommitment != nil {
		tampered.PublicState.DealerCommitment.RootHash = tampered.ActiveHand.Cards.Transcript.RootHash
	}

	if err := guest.acceptRemoteTable(tampered); err == nil || !strings.Contains(err.Error(), "transcript") {
		t.Fatalf("expected tampered transcript to be rejected, got %v", err)
	}
}

func TestAcceptRemoteTablePreservesLocalProtocolDeadline(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createJoinedTwoPlayerTable(t, host, guest)
	startNextHandForTest(t, host, tableID)

	if err := guest.acceptRemoteTable(mustReadNativeTable(t, host, tableID)); err != nil {
		t.Fatalf("initial accept remote table: %v", err)
	}
	initial := mustReadNativeTable(t, guest, tableID)
	if initial.ActiveHand == nil {
		t.Fatal("expected active hand after initial sync")
	}
	originalDeadline := initial.ActiveHand.Cards.PhaseDeadlineAt
	if originalDeadline == "" {
		t.Fatal("expected locally derived protocol deadline")
	}

	tampered := mustReadNativeTable(t, host, tableID)
	tampered.ActiveHand.Cards.PhaseDeadlineAt = addMillis(nowISO(), 60_000)
	if err := guest.acceptRemoteTable(tampered); err != nil {
		t.Fatalf("accept remote table with tampered deadline: %v", err)
	}

	accepted := mustReadNativeTable(t, guest, tableID)
	if accepted.ActiveHand == nil {
		t.Fatal("expected active hand after accepting tampered deadline")
	}
	if accepted.ActiveHand.Cards.PhaseDeadlineAt != originalDeadline {
		t.Fatalf("expected local protocol deadline %q, got %q", originalDeadline, accepted.ActiveHand.Cards.PhaseDeadlineAt)
	}
}

func TestAcceptRemoteTableReconstructsFinalDeckFromTranscript(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)

	tampered := mustReadNativeTable(t, host, tableID)
	if tampered.ActiveHand == nil {
		t.Fatal("expected active hand to tamper")
	}
	tampered.ActiveHand.Cards.FinalDeck = nil

	if err := guest.acceptRemoteTable(tampered); err != nil {
		t.Fatalf("accept remote table with missing final deck: %v", err)
	}

	accepted := mustReadNativeTable(t, guest, tableID)
	if accepted.ActiveHand == nil {
		t.Fatal("expected active hand after accepting missing final deck")
	}
	if got := len(accepted.ActiveHand.Cards.FinalDeck); got != 52 {
		t.Fatalf("expected reconstructed 52-card final deck, got %d", got)
	}
}

func TestAcceptRemoteTableRejectsRewrittenHistoricalLedger(t *testing.T) {
	t.Parallel()

	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)

	t.Run("event", func(t *testing.T) {
		tampered := mustReadNativeTable(t, host, tableID)
		if len(tampered.Events) == 0 {
			t.Fatal("expected event history to tamper")
		}
		tableBody, ok := tampered.Events[0].Body["table"].(map[string]any)
		if !ok {
			t.Fatalf("expected table announce payload, got %#v", tampered.Events[0].Body["table"])
		}
		tableBody["name"] = "forged historical name"
		tampered.Events, tampered.LastEventHash = resignHistoricalEventsForTest(t, host, tampered.Events)
		if tampered.PublicState != nil {
			tampered.PublicState.LatestEventHash = tampered.LastEventHash
		}
		if len(tampered.Snapshots) > 0 {
			for _, event := range tampered.Events {
				if stringValue(event.Body["type"]) != "TableReady" {
					continue
				}
				tampered.Snapshots[0].LatestEventHash = event.PrevEventHash
				resignHistoricalSnapshotForTest(t, host, &tampered.Snapshots[0])
				if tampered.LatestSnapshot != nil && tampered.LatestSnapshot.SnapshotID == tampered.Snapshots[0].SnapshotID {
					tampered.LatestSnapshot = cloneSnapshot(&tampered.Snapshots[0])
				}
				if tampered.LatestFullySignedSnapshot != nil && tampered.LatestFullySignedSnapshot.SnapshotID == tampered.Snapshots[0].SnapshotID {
					tampered.LatestFullySignedSnapshot = cloneSnapshot(&tampered.Snapshots[0])
				}
				break
			}
		}

		if err := guest.acceptRemoteTable(tampered); err == nil || (!strings.Contains(err.Error(), "historical event") && !strings.Contains(err.Error(), "locally derived successor")) {
			t.Fatalf("expected rewritten historical event to be rejected, got %v", err)
		}
	})

	t.Run("snapshot", func(t *testing.T) {
		tampered := mustReadNativeTable(t, host, tableID)
		if len(tampered.Snapshots) == 0 {
			t.Fatal("expected snapshot history to tamper")
		}
		tampered.Snapshots[0].DealerCommitmentRoot = "forged-historical-root"
		resignHistoricalSnapshotForTest(t, host, &tampered.Snapshots[0])
		if tampered.LatestSnapshot != nil && tampered.LatestSnapshot.SnapshotID == tampered.Snapshots[0].SnapshotID {
			*tampered.LatestSnapshot = cloneJSON(tampered.Snapshots[0])
		}
		if tampered.LatestFullySignedSnapshot != nil && tampered.LatestFullySignedSnapshot.SnapshotID == tampered.Snapshots[0].SnapshotID {
			*tampered.LatestFullySignedSnapshot = cloneJSON(tampered.Snapshots[0])
		}

		if err := guest.acceptRemoteTable(tampered); err == nil || !strings.Contains(err.Error(), "historical snapshot") {
			t.Fatalf("expected rewritten historical snapshot to be rejected, got %v", err)
		}
	})
}

func TestAcceptRemoteTableRejectsTamperedAcceptedActionRequests(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)
	current := waitForLocalCanAct(t, []*meshRuntime{host, guest}, host, tableID)
	if _, err := host.SendAction(tableID, game.Action{Type: game.ActionCall}); err != nil {
		t.Fatalf("host send call: %v", err)
	}

	base := mustReadNativeTable(t, host, tableID)
	eventIndex := findEventIndexByType(base, "PlayerAction")
	if eventIndex < 0 {
		t.Fatal("expected PlayerAction event")
	}
	request, hasRequest, err := actionRequestFromEvent(base.Events[eventIndex])
	if err != nil {
		t.Fatalf("decode action request from event: %v", err)
	}
	if !hasRequest || request == nil {
		t.Fatal("expected canonical action request in PlayerAction event")
	}

	t.Run("missing request", func(t *testing.T) {
		tampered := cloneJSON(base)
		delete(tampered.Events[eventIndex].Body, "actionRequest")
		resignAcceptedTableEventsForTest(t, host, &tampered)

		if err := guest.acceptRemoteTable(tampered); err == nil || !strings.Contains(err.Error(), "missing its signed action request") {
			t.Fatalf("expected missing action request to be rejected, got %v", err)
		}
	})

	t.Run("wrong signer", func(t *testing.T) {
		tampered := cloneJSON(base)
		forged := cloneJSON(*request)
		resignActionRequestForTest(t, guest, &forged)
		tampered.Events[eventIndex].Body["actionRequest"] = rawJSONMap(forged)
		resignAcceptedTableEventsForTest(t, host, &tampered)

		if err := guest.acceptRemoteTable(tampered); err == nil || !strings.Contains(err.Error(), "signature") {
			t.Fatalf("expected wrong action signer to be rejected, got %v", err)
		}
	})

	t.Run("wrong prev custody hash", func(t *testing.T) {
		tampered := cloneJSON(base)
		forged := cloneJSON(*request)
		forged.PrevCustodyStateHash = strings.Repeat("0", 64)
		resignActionRequestForTest(t, host, &forged)
		tampered.Events[eventIndex].Body["actionRequest"] = rawJSONMap(forged)
		resignAcceptedTableEventsForTest(t, host, &tampered)

		if err := guest.acceptRemoteTable(tampered); err == nil || !strings.Contains(err.Error(), "action request custody mismatch") {
			t.Fatalf("expected wrong action prev custody hash to be rejected, got %v", err)
		}
	})

	t.Run("wrong decision index", func(t *testing.T) {
		tampered := cloneJSON(base)
		forged := cloneJSON(*request)
		forged.DecisionIndex++
		resignActionRequestForTest(t, host, &forged)
		tampered.Events[eventIndex].Body["actionRequest"] = rawJSONMap(forged)
		resignAcceptedTableEventsForTest(t, host, &tampered)

		if err := guest.acceptRemoteTable(tampered); err == nil || !strings.Contains(err.Error(), "action request decision mismatch") {
			t.Fatalf("expected wrong action decision index to be rejected, got %v", err)
		}
	})

	t.Run("wrong transcript root", func(t *testing.T) {
		tampered := cloneJSON(base)
		forged := cloneJSON(*request)
		forged.TranscriptRoot = strings.Repeat("f", 64)
		resignActionRequestForTest(t, host, &forged)
		tampered.Events[eventIndex].Body["actionRequest"] = rawJSONMap(forged)
		resignAcceptedTableEventsForTest(t, host, &tampered)

		if err := guest.acceptRemoteTable(tampered); err == nil || !strings.Contains(err.Error(), "mismatch") {
			t.Fatalf("expected wrong action transcript bindings to be rejected, got %v", err)
		}
	})

	t.Run("wrong semantic successor", func(t *testing.T) {
		replica := newMeshTestRuntime(t, "guest-replica")
		replica.walletID = guest.walletID
		tampered := cloneJSON(base)
		_, transitionIndex, err := linkedCustodyTransitionForEvent(tampered, tampered.Events[eventIndex])
		if err != nil {
			t.Fatalf("link action transition: %v", err)
		}
		forgedTransition := cloneJSON(tampered.CustodyTransitions[transitionIndex])
		forgedTransition.NextState.ActionDeadlineAt = addMillis(forgedTransition.NextState.ActionDeadlineAt, 1_000)
		forgedTransition.NextState.StateHash = tablecustody.HashCustodyState(forgedTransition.NextState)
		forgedTransition.NextStateHash = forgedTransition.NextState.StateHash
		forgedTransition.Approvals = nil
		for _, originalApproval := range tampered.CustodyTransitions[transitionIndex].Approvals {
			var approval tablecustody.CustodySignature
			switch originalApproval.PlayerID {
			case host.walletID.PlayerID:
				approval, err = host.localCustodyApproval(forgedTransition)
			case guest.walletID.PlayerID:
				approval, err = guest.localCustodyApproval(forgedTransition)
			default:
				t.Fatalf("unexpected original approval signer %q", originalApproval.PlayerID)
			}
			if err != nil {
				t.Fatalf("sign forged approval for %s: %v", originalApproval.PlayerID, err)
			}
			forgedTransition.Approvals = append(forgedTransition.Approvals, approval)
		}
		forgedTransition.Proof.Signatures = append([]tablecustody.CustodySignature(nil), forgedTransition.Approvals...)
		forgedTransition.Proof.StateHash = forgedTransition.NextStateHash
		forgedTransition.Proof.TransitionHash = tablecustody.HashCustodyTransition(forgedTransition)
		tampered.CustodyTransitions[transitionIndex] = forgedTransition
		tampered.LatestCustodyState = &tampered.CustodyTransitions[transitionIndex].NextState
		tampered.Events[eventIndex].Body["transitionHash"] = forgedTransition.Proof.TransitionHash
		resignAcceptedTableEventsForTest(t, host, &tampered)

		if err := replica.acceptRemoteTable(tampered); err == nil || !strings.Contains(err.Error(), "locally derived successor") {
			t.Fatalf("expected semantically wrong action transition to be rejected, got %v", err)
		}
	})

	if current.ActiveHand == nil {
		t.Fatal("expected action-capable hand")
	}
}

func TestAcceptRemoteTableRejectsTamperedAcceptedFundsRequests(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createJoinedTwoPlayerTable(t, host, guest)
	if _, err := host.CashOut(tableID); err != nil {
		t.Fatalf("host cash out: %v", err)
	}

	base := mustReadNativeTable(t, host, tableID)
	eventIndex := findEventIndexByType(base, "CashOut")
	if eventIndex < 0 {
		t.Fatal("expected CashOut event")
	}
	request, hasRequest, err := fundsRequestFromEvent(base.Events[eventIndex])
	if err != nil {
		t.Fatalf("decode funds request from event: %v", err)
	}
	if !hasRequest || request == nil {
		t.Fatal("expected canonical funds request in CashOut event")
	}

	t.Run("missing request", func(t *testing.T) {
		tampered := cloneJSON(base)
		delete(tampered.Events[eventIndex].Body, "fundsRequest")
		resignAcceptedTableEventsForTest(t, host, &tampered)

		if err := guest.acceptRemoteTable(tampered); err == nil || !strings.Contains(err.Error(), "missing its signed funds request") {
			t.Fatalf("expected missing funds request to be rejected, got %v", err)
		}
	})

	t.Run("wrong signer", func(t *testing.T) {
		tampered := cloneJSON(base)
		forged := cloneJSON(*request)
		resignFundsRequestForTest(t, guest, &forged)
		tampered.Events[eventIndex].Body["fundsRequest"] = rawJSONMap(forged)
		resignAcceptedTableEventsForTest(t, host, &tampered)

		if err := guest.acceptRemoteTable(tampered); err == nil || !strings.Contains(err.Error(), "signature") {
			t.Fatalf("expected wrong funds signer to be rejected, got %v", err)
		}
	})

	t.Run("wrong prev custody hash", func(t *testing.T) {
		tampered := cloneJSON(base)
		forged := cloneJSON(*request)
		forged.PrevCustodyStateHash = strings.Repeat("0", 64)
		resignFundsRequestForTest(t, host, &forged)
		tampered.Events[eventIndex].Body["fundsRequest"] = rawJSONMap(forged)
		resignAcceptedTableEventsForTest(t, host, &tampered)

		if err := guest.acceptRemoteTable(tampered); err == nil || !strings.Contains(err.Error(), "prev custody hash mismatch") {
			t.Fatalf("expected wrong funds prev custody hash to be rejected, got %v", err)
		}
	})

	t.Run("wrong semantic successor", func(t *testing.T) {
		replica := newMeshTestRuntime(t, "guest-replica")
		replica.walletID = guest.walletID
		tampered := cloneJSON(base)
		_, transitionIndex, err := linkedCustodyTransitionForEvent(tampered, tampered.Events[eventIndex])
		if err != nil {
			t.Fatalf("link funds transition: %v", err)
		}
		forgedTransition := cloneJSON(tampered.CustodyTransitions[transitionIndex])
		forgedTransition.NextState.ChallengeAnchor = strings.Repeat("a", 64)
		forgedTransition.NextState.StateHash = tablecustody.HashCustodyState(forgedTransition.NextState)
		forgedTransition.NextStateHash = forgedTransition.NextState.StateHash
		forgedTransition.Approvals = nil
		for _, originalApproval := range tampered.CustodyTransitions[transitionIndex].Approvals {
			var approval tablecustody.CustodySignature
			switch originalApproval.PlayerID {
			case host.walletID.PlayerID:
				approval, err = host.localCustodyApproval(forgedTransition)
			case guest.walletID.PlayerID:
				approval, err = guest.localCustodyApproval(forgedTransition)
			default:
				t.Fatalf("unexpected original approval signer %q", originalApproval.PlayerID)
			}
			if err != nil {
				t.Fatalf("sign forged approval for %s: %v", originalApproval.PlayerID, err)
			}
			forgedTransition.Approvals = append(forgedTransition.Approvals, approval)
		}
		forgedTransition.Proof.Signatures = append([]tablecustody.CustodySignature(nil), forgedTransition.Approvals...)
		forgedTransition.Proof.StateHash = forgedTransition.NextStateHash
		forgedTransition.Proof.TransitionHash = tablecustody.HashCustodyTransition(forgedTransition)
		tampered.CustodyTransitions[transitionIndex] = forgedTransition
		tampered.LatestCustodyState = &tampered.CustodyTransitions[transitionIndex].NextState
		tampered.Events[eventIndex].Body["latestCustodyStateHash"] = forgedTransition.NextStateHash
		tampered.Events[eventIndex].Body["transitionHash"] = forgedTransition.Proof.TransitionHash
		resignAcceptedTableEventsForTest(t, host, &tampered)

		if err := replica.acceptRemoteTable(tampered); err == nil || !strings.Contains(err.Error(), "locally derived successor") {
			t.Fatalf("expected semantically wrong funds transition to be rejected, got %v", err)
		}
	})
}

func TestAcceptRemoteTableRejectsTamperedActionLogDespiteCanonicalSignedRequests(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)
	waitForLocalCanAct(t, []*meshRuntime{host, guest}, host, tableID)
	if _, err := host.SendAction(tableID, game.Action{Type: game.ActionCall}); err != nil {
		t.Fatalf("host send call: %v", err)
	}

	tampered := mustReadNativeTable(t, host, tableID)
	if tampered.ActiveHand == nil || len(tampered.ActiveHand.State.ActionLog) == 0 {
		t.Fatal("expected accepted action log to tamper")
	}
	tampered.ActiveHand.State.ActionLog[0].Action.Type = game.ActionFold

	if err := guest.acceptRemoteTable(tampered); err == nil || !strings.Contains(err.Error(), "active hand state does not match transcript replay") {
		t.Fatalf("expected tampered ActionLog to be rejected, got %v", err)
	}
}

func TestAcceptRemoteTableRejectsUnauthorizedHostTransition(t *testing.T) {
	t.Parallel()

	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")
	outsider := newMeshTestRuntime(t, "outsider")

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)

	t.Run("same epoch host change", func(t *testing.T) {
		tampered := mustReadNativeTable(t, host, tableID)
		tampered.CurrentHost.Peer = NativePeerAddress{
			Alias:             outsider.profileName,
			LastSeenAt:        nowISO(),
			PeerID:            outsider.selfPeerID(),
			PeerURL:           outsider.selfPeerURL(),
			ProtocolPubkeyHex: outsider.protocolIdentity.PublicKeyHex,
		}

		if err := guest.acceptRemoteTable(tampered); err == nil || !strings.Contains(err.Error(), "changed host without advancing epoch") {
			t.Fatalf("expected same-epoch host change to be rejected, got %v", err)
		}
	})

	t.Run("unauthorized epoch advance", func(t *testing.T) {
		tampered := mustReadNativeTable(t, host, tableID)
		tampered.CurrentEpoch++
		if tampered.PublicState != nil {
			tampered.PublicState.Epoch = tampered.CurrentEpoch
		}
		tampered.CurrentHost.Peer = NativePeerAddress{
			Alias:             outsider.profileName,
			LastSeenAt:        nowISO(),
			PeerID:            outsider.selfPeerID(),
			PeerURL:           outsider.selfPeerURL(),
			ProtocolPubkeyHex: outsider.protocolIdentity.PublicKeyHex,
		}

		if err := guest.acceptRemoteTable(tampered); err == nil || !strings.Contains(err.Error(), "unauthorized host") {
			t.Fatalf("expected unauthorized epoch-advanced host to be rejected, got %v", err)
		}
	})

	t.Run("same host epoch advance", func(t *testing.T) {
		tampered := mustReadNativeTable(t, host, tableID)
		tampered.CurrentEpoch++
		if tampered.PublicState != nil {
			tampered.PublicState.Epoch = tampered.CurrentEpoch
		}

		if err := guest.acceptRemoteTable(tampered); err == nil || !strings.Contains(err.Error(), "advanced epoch without rotating host") {
			t.Fatalf("expected same-host epoch advance to be rejected, got %v", err)
		}
	})
}

func TestAcceptRemoteTableRejectsMissingPrivateDeliveryAfterActivation(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)

	tampered := mustReadNativeTable(t, host, tableID)
	if tampered.ActiveHand == nil {
		t.Fatal("expected active hand to tamper")
	}

	filtered := make([]game.HandTranscriptRecord, 0, len(tampered.ActiveHand.Cards.Transcript.Records))
	for _, record := range tampered.ActiveHand.Cards.Transcript.Records {
		if record.Kind == nativeHandMessagePrivateDelivery {
			continue
		}
		filtered = append(filtered, record)
	}
	if len(filtered) == len(tampered.ActiveHand.Cards.Transcript.Records) {
		t.Fatal("expected private delivery records to remove")
	}
	tampered.ActiveHand.Cards.Transcript.Records = filtered
	tampered.ActiveHand.Cards.Transcript = rebuildTranscriptForTest(t, tampered.ActiveHand.Cards.Transcript)
	if tampered.PublicState != nil && tampered.PublicState.DealerCommitment != nil {
		tampered.PublicState.DealerCommitment.RootHash = tampered.ActiveHand.Cards.Transcript.RootHash
	}

	if err := guest.acceptRemoteTable(tampered); err == nil || !strings.Contains(err.Error(), "missing private delivery shares") {
		t.Fatalf("expected missing private delivery shares to be rejected, got %v", err)
	}
}

func TestAcceptRemoteTableRejectsPlaintextCardsInPrivateDeliveryShare(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)

	tampered := mustReadNativeTable(t, host, tableID)
	if tampered.ActiveHand == nil {
		t.Fatal("expected active hand to tamper")
	}

	found := false
	for index := range tampered.ActiveHand.Cards.Transcript.Records {
		record := &tampered.ActiveHand.Cards.Transcript.Records[index]
		if record.Kind != nativeHandMessagePrivateDelivery {
			continue
		}
		record.Cards = []game.CardCode{"As", "Kd"}
		found = true
		break
	}
	if !found {
		t.Fatal("expected private delivery record to tamper")
	}
	tampered.ActiveHand.Cards.Transcript = rebuildTranscriptForTest(t, tampered.ActiveHand.Cards.Transcript)
	if tampered.PublicState != nil && tampered.PublicState.DealerCommitment != nil {
		tampered.PublicState.DealerCommitment.RootHash = tampered.ActiveHand.Cards.Transcript.RootHash
	}

	if err := guest.acceptRemoteTable(tampered); err == nil || !strings.Contains(err.Error(), "plaintext cards") {
		t.Fatalf("expected plaintext-card private delivery share to be rejected, got %v", err)
	}
}

func TestAcceptRemoteTableRejectsHostRotationServedFromWrongEndpoint(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")
	witness := newMeshTestRuntime(t, "witness")

	if _, err := host.BootstrapPeer(witness.selfPeerURL(), "", nil); err != nil {
		t.Fatalf("bootstrap witness peer: %v", err)
	}
	tableID, _ := createStartedTwoPlayerTable(t, host, guest, witness.selfPeerID())

	tampered := mustReadNativeTable(t, host, tableID)
	if len(tampered.Witnesses) != 1 {
		t.Fatalf("expected single witness, got %d", len(tampered.Witnesses))
	}
	tampered.CurrentEpoch++
	tampered.Config.HostPeerID = witness.selfPeerID()
	if tampered.PublicState != nil {
		tampered.PublicState.Epoch = tampered.CurrentEpoch
	}
	tampered.CurrentHost = cloneJSON(tampered.Witnesses[0])
	tampered.CurrentHost.Peer.PeerURL = host.selfPeerURL()

	if err := guest.acceptRemoteTable(tampered); err == nil || !strings.Contains(err.Error(), "unexpected endpoint") {
		t.Fatalf("expected host rotation served from wrong endpoint to be rejected, got %v", err)
	}
}

func TestAcceptRemoteTableRejectsTamperedSettledState(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")
	auditor := newMeshTestRuntime(t, "auditor")

	if _, err := host.BootstrapPeer(auditor.selfPeerURL(), "", nil); err != nil {
		t.Fatalf("bootstrap auditor peer: %v", err)
	}

	tableID, _ := createStartedTwoPlayerTable(t, host, guest, auditor.selfPeerID())
	current := mustReadNativeTable(t, host, tableID)
	if !host.localTableView(current).Local.CanAct {
		t.Fatalf("expected host to be able to act at preflop start, got phase=%v actingSeat=%v", current.ActiveHand.State.Phase, current.ActiveHand.State.ActingSeatIndex)
	}
	foldRequest, err := host.buildSignedActionRequest(current, game.Action{Type: game.ActionFold})
	if err != nil {
		t.Fatalf("build fold request: %v", err)
	}
	settled, err := host.handleActionFromPeer(foldRequest)
	if err != nil {
		t.Fatalf("host folds to settle hand: %v", err)
	}
	if settled.ActiveHand == nil || settled.ActiveHand.State.Phase != game.StreetSettled {
		t.Fatalf("expected settled hand, got %+v", settled.ActiveHand)
	}

	t.Run("public state", func(t *testing.T) {
		tampered := mustReadNativeTable(t, host, tableID)
		if tampered.PublicState == nil {
			t.Fatal("expected public state to tamper")
		}
		tampered.PublicState.Board = []string{"As", "Ad", "Ac", "Ah", "2c"}

		if err := guest.acceptRemoteTable(tampered); err == nil || (!strings.Contains(err.Error(), "public state") && !strings.Contains(err.Error(), "locally derived successor")) {
			t.Fatalf("expected tampered public state to be rejected, got %v", err)
		}
	})

	t.Run("snapshot", func(t *testing.T) {
		tampered := mustReadNativeTable(t, host, tableID)
		if tampered.LatestSnapshot == nil || tampered.LatestFullySignedSnapshot == nil {
			t.Fatal("expected settled snapshots to tamper")
		}
		tampered.LatestSnapshot.ChipBalances[host.walletID.PlayerID]++
		tampered.LatestFullySignedSnapshot.ChipBalances[host.walletID.PlayerID]++

		if err := guest.acceptRemoteTable(tampered); err == nil || (!strings.Contains(err.Error(), "snapshot") && !strings.Contains(err.Error(), "locally derived successor")) {
			t.Fatalf("expected tampered settled snapshot to be rejected, got %v", err)
		}
	})

	t.Run("snapshot latest event hash anchor", func(t *testing.T) {
		tampered := mustReadNativeTable(t, host, tableID)
		if tampered.PublicState == nil || tampered.LatestSnapshot == nil || tampered.LatestFullySignedSnapshot == nil {
			t.Fatal("expected settled state and snapshots to tamper")
		}
		if len(tampered.Snapshots) == 0 {
			t.Fatal("expected snapshot history to tamper")
		}
		latestIndex := len(tampered.Snapshots) - 1
		wrongHash := tampered.LastEventHash
		if strings.TrimSpace(wrongHash) == "" {
			t.Fatal("expected accepted last event hash")
		}
		if strings.TrimSpace(stringValue(tampered.Snapshots[latestIndex].LatestEventHash)) == wrongHash {
			t.Fatal("expected settled snapshot latest event hash to differ from last event hash")
		}

		tampered.PublicState.LatestEventHash = wrongHash
		tampered.Snapshots[latestIndex].LatestEventHash = wrongHash
		resignHistoricalSnapshotForTest(t, host, &tampered.Snapshots[latestIndex])
		tampered.LatestSnapshot = cloneSnapshot(&tampered.Snapshots[latestIndex])
		tampered.LatestFullySignedSnapshot = cloneSnapshot(&tampered.Snapshots[latestIndex])

		if err := auditor.acceptRemoteTable(tampered); err == nil || (!strings.Contains(err.Error(), "not anchored") && !strings.Contains(err.Error(), "locally derived successor")) {
			t.Fatalf("expected settled snapshot latest event hash anchor tampering to be rejected, got %v", err)
		}
	})
}

func TestProtocolDeadlineForcesFailoverDespiteFreshHeartbeat(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")
	witness := newMeshTestRuntime(t, "witness")

	if _, err := host.BootstrapPeer(witness.selfPeerURL(), "", nil); err != nil {
		t.Fatalf("bootstrap witness peer: %v", err)
	}
	tableID, _ := createJoinedTwoPlayerTable(t, host, guest, witness.selfPeerID())
	startNextHandForTest(t, host, tableID)
	if err := guest.Close(); err != nil {
		t.Fatalf("close guest runtime: %v", err)
	}

	startedAt := time.Now()
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		touchLocalHostHeartbeat(t, host, tableID)
		witness.Tick()

		table := mustReadNativeTable(t, witness, tableID)
		if table.CurrentHost.Peer.PeerID != witness.selfPeerID() {
			if elapsedMillis(table.LastHostHeartbeatAt) > nativeHostFailureMS {
				t.Fatal("expected protocol failover to happen before host heartbeat expiry")
			}
		}

		if table.CurrentHost.Peer.PeerID == witness.selfPeerID() {
			if time.Since(startedAt) >= time.Duration(nativeHostFailureMS)*time.Millisecond {
				t.Fatal("expected protocol deadline failover before heartbeat-based host expiry")
			}
			if !tableHasEventType(table, "HandAbort") {
				t.Fatal("expected protocol failover to preserve timeout enforcement")
			}
			if table.ActiveHand == nil || table.ActiveHand.State.Phase != game.StreetSettled {
				t.Fatalf("expected protocol failover to settle the active hand, got %+v", table.ActiveHand)
			}
			if len(table.ActiveHand.State.Winners) != 1 || table.ActiveHand.State.Winners[0].PlayerID != host.walletID.PlayerID {
				t.Fatalf("expected host seat to win after guest timeout, got %+v", table.ActiveHand.State.Winners)
			}
			return
		}

		time.Sleep(100 * time.Millisecond)
	}

	t.Fatal("timed out waiting for protocol deadline failover")
}

func TestTableFetchAuthRejectsStaleSignature(t *testing.T) {
	t.Parallel()

	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)

	staleSignedAt := addMillis(nowISO(), -int((nativeTableFetchAuthMaxAge+time.Second)/time.Millisecond))
	signatureHex, err := settlementcore.SignStructuredData(guest.walletID.PrivateKeyHex, nativeTableFetchAuthPayload(tableID, guest.walletID.PlayerID, staleSignedAt))
	if err != nil {
		t.Fatalf("sign stale fetch auth: %v", err)
	}
	hostInfo, err := guest.fetchPeerInfo(host.selfPeerURL())
	if err != nil {
		t.Fatalf("fetch host peer info: %v", err)
	}
	request, requestKey, err := guest.newOutboundEnvelope(
		nativeTransportMessageTablePull,
		nativeTransportChannelTable,
		tableID,
		hostInfo.Peer.PeerID,
		nativeTableFetchRequest{
			PlayerID:     guest.walletID.PlayerID,
			SignatureHex: signatureHex,
			SignedAt:     staleSignedAt,
			TableID:      tableID,
		},
		hostInfo.TransportPubkeyHex,
	)
	if err != nil {
		t.Fatalf("build stale fetch request envelope: %v", err)
	}
	response, err := guest.exchangePeerTransport(host.selfPeerURL(), request)
	if err != nil {
		t.Fatalf("send stale fetch request: %v", err)
	}
	body, err := guest.decodeResponseEnvelope(response, requestKey)
	if err != nil {
		t.Fatalf("decode stale fetch response envelope: %v", err)
	}
	var table nativeTableState
	if err := json.Unmarshal(body, &table); err != nil {
		t.Fatalf("decode stale fetch response: %v", err)
	}
	assertTranscriptProtectedCards(t, table)
}

func TestFetchPeerInfoRefreshesExpiredCache(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	hostURL := host.selfPeerURL()
	guest.peerInfoCache[hostURL] = nativeCachedPeerInfo{
		FetchedAt: time.Now().Add(-nativePeerInfoCacheTTL - time.Second),
		PeerSelf: nativePeerSelf{
			Peer: NativePeerAddress{
				PeerID:            "peer-stale",
				PeerURL:           hostURL,
				ProtocolPubkeyHex: guest.protocolIdentity.PublicKeyHex,
			},
			ProtocolID: guest.protocolID,
		},
	}

	peerInfo, err := guest.fetchPeerInfo(hostURL)
	if err != nil {
		t.Fatalf("refresh expired peer cache: %v", err)
	}
	if peerInfo.Peer.PeerID != host.selfPeerID() {
		t.Fatalf("expected refreshed peer id %q, got %q", host.selfPeerID(), peerInfo.Peer.PeerID)
	}
	cached, ok := guest.peerInfoCache[hostURL]
	if !ok {
		t.Fatal("expected refreshed peer info to be cached")
	}
	if cached.PeerSelf.Peer.PeerID != host.selfPeerID() {
		t.Fatalf("expected cached peer id %q after refresh, got %q", host.selfPeerID(), cached.PeerSelf.Peer.PeerID)
	}
}

func TestJoinTableRejectsReusingReservedFundingRefsAcrossTables(t *testing.T) {
	firstHost := newMeshTestRuntime(t, "first-host")
	secondHost := newMeshTestRuntime(t, "second-host")
	guest := newMeshTestRuntime(t, "guest")

	firstCreate, err := firstHost.CreateTable(map[string]any{"name": "First"})
	if err != nil {
		t.Fatalf("create first table: %v", err)
	}
	if _, err := guest.JoinTable(stringValue(firstCreate["inviteCode"]), 4_000); err != nil {
		t.Fatalf("join first table: %v", err)
	}

	secondCreate, err := secondHost.CreateTable(map[string]any{"name": "Second"})
	if err != nil {
		t.Fatalf("create second table: %v", err)
	}
	if _, err := guest.JoinTable(stringValue(secondCreate["inviteCode"]), 4_000); err == nil || !strings.Contains(err.Error(), "insufficient available sats") {
		t.Fatalf("expected second join to fail on reserved funds, got %v", err)
	}
}

func TestDefaultDealerlessBlindsUseArkDustFloor(t *testing.T) {
	smallBlind, bigBlind := defaultDealerlessBlinds(330, map[string]any{})
	if smallBlind != 165 || bigBlind != 330 {
		t.Fatalf("expected dust-aware defaults 165/330, got %d/%d", smallBlind, bigBlind)
	}
}

func TestValidateDealerlessBlindPotRejectsSubDustOpeningPot(t *testing.T) {
	if err := validateDealerlessBlindPot(50, 100, 330); err == nil {
		t.Fatal("expected sub-dust opening blind pot to be rejected")
	}
	if err := validateDealerlessBlindPot(165, 330, 330); err != nil {
		t.Fatalf("expected dust-compatible opening blind pot to pass, got %v", err)
	}
}

func TestValidateAcceptedCustodyHistoryRejectsTamperedApprovalSignature(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)
	table := mustReadNativeTable(t, guest, tableID)
	tampered := cloneJSON(table)

	transitionIndex := -1
	for index, transition := range tampered.CustodyTransitions {
		if len(transition.Approvals) > 0 {
			transitionIndex = index
			break
		}
	}
	if transitionIndex < 0 {
		t.Fatal("expected finalized custody transition with approvals")
	}

	transition := tampered.CustodyTransitions[transitionIndex]
	transition.Approvals[0].SignatureHex = "00"
	transition.Proof.Signatures[0] = transition.Approvals[0]
	transition.Proof.TransitionHash = tablecustody.HashCustodyTransition(transition)
	tampered.CustodyTransitions[transitionIndex] = transition

	if err := guest.validateAcceptedCustodyHistory(nil, tampered); err == nil || !strings.Contains(err.Error(), "custody approval") {
		t.Fatalf("expected tampered approval to be rejected, got %v", err)
	}
}

func TestCashOutAppendsCustodyTransition(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createJoinedTwoPlayerTable(t, host, guest)
	if _, err := host.CashOut(tableID); err != nil {
		t.Fatalf("cash out: %v", err)
	}

	table := mustReadNativeTable(t, host, tableID)
	if len(table.CustodyTransitions) == 0 {
		t.Fatal("expected custody history after cash out")
	}
	transition := table.CustodyTransitions[len(table.CustodyTransitions)-1]
	if transition.Kind != tablecustody.TransitionKindCashOut {
		t.Fatalf("expected latest transition kind %s, got %s", tablecustody.TransitionKindCashOut, transition.Kind)
	}
	if len(transition.Approvals) != 1 || transition.Approvals[0].PlayerID != host.walletID.PlayerID {
		t.Fatalf("expected only local approval on cash out, got %+v", transition.Approvals)
	}
	if !tableHasEventType(table, "CashOut") {
		t.Fatal("expected CashOut event after cash out")
	}
	if latestStackAmount(table.LatestCustodyState, host.walletID.PlayerID) != 0 {
		t.Fatalf("expected cash out to clear local stack, got %d", latestStackAmount(table.LatestCustodyState, host.walletID.PlayerID))
	}
}

func TestCashOutTransitionHashIncludesApprovals(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createJoinedTwoPlayerTable(t, host, guest)
	table := mustReadNativeTable(t, host, tableID)
	transition, err := host.buildFundsCustodyTransition(table, tablecustody.TransitionKindCashOut, "completed")
	if err != nil {
		t.Fatalf("build cash-out transition: %v", err)
	}
	transition.ArkIntentID = "intent-test"
	transition.ArkTxID = "tx-test"
	transition.NextState.StateHash = tablecustody.HashCustodyState(transition.NextState)
	transition.NextStateHash = transition.NextState.StateHash
	transition.Proof = tablecustody.CustodyProof{
		ArkIntentID:     transition.ArkIntentID,
		ArkTxID:         transition.ArkTxID,
		FinalizedAt:     nowISO(),
		ReplayValidated: true,
		StateHash:       transition.NextStateHash,
	}
	fundsRequest, err := host.buildSignedFundsRequest(table, "cashout")
	if err != nil {
		t.Fatalf("build cash-out request: %v", err)
	}
	approvals, err := host.collectCustodyApprovals(table, transition, authorizerForFundsRequest(fundsRequest), host.requiredCustodySigners(table, transition))
	if err != nil {
		t.Fatalf("collect cash-out approvals: %v", err)
	}
	transition.Approvals = approvals
	transition.Proof.Signatures = append([]tablecustody.CustodySignature(nil), approvals...)
	transition.Proof.TransitionHash = tablecustody.HashCustodyTransition(transition)
	if transition.Proof.TransitionHash == "" {
		t.Fatal("expected cash-out proof transition hash")
	}
	if want := tablecustody.HashCustodyTransition(transition); transition.Proof.TransitionHash != want {
		t.Fatalf("expected cash-out transition hash %s, got %s", want, transition.Proof.TransitionHash)
	}
	if err := tablecustody.ValidateTransition(table.LatestCustodyState, transition); err != nil {
		t.Fatalf("validate cash-out transition: %v", err)
	}
}

func TestSubsequentFundsTransitionDoesNotReuseCompletedSeatFundingRefs(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createJoinedTwoPlayerTable(t, host, guest)
	if _, err := host.CashOut(tableID); err != nil {
		t.Fatalf("host cash out: %v", err)
	}

	table := mustReadNativeTable(t, host, tableID)
	transition, err := host.buildFundsCustodyTransitionForPlayer(table, guest.walletID.PlayerID, tablecustody.TransitionKindCashOut, "completed")
	if err != nil {
		t.Fatalf("build guest cash-out transition: %v", err)
	}

	for _, claim := range transition.NextState.StackClaims {
		if claim.PlayerID != host.walletID.PlayerID {
			continue
		}
		if len(claim.VTXORefs) != 0 {
			t.Fatalf("expected completed seat refs to stay empty, got %+v", claim.VTXORefs)
		}
		if backed := stackClaimBackedAmount(claim); backed != 0 {
			t.Fatalf("expected completed seat backed amount to stay zero, got %d", backed)
		}
	}
	if err := validateAcceptedCustodyRefs(table.LatestCustodyState, transition, false); err != nil {
		t.Fatalf("validate guest cash-out transition refs: %v", err)
	}
}

func TestEmergencyExitAppendsCustodyTransition(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createJoinedTwoPlayerTable(t, host, guest)
	if _, err := host.Exit(tableID); err != nil {
		t.Fatalf("emergency exit: %v", err)
	}

	table := mustReadNativeTable(t, host, tableID)
	if len(table.CustodyTransitions) == 0 {
		t.Fatal("expected custody history after emergency exit")
	}
	transition := table.CustodyTransitions[len(table.CustodyTransitions)-1]
	if transition.Kind != tablecustody.TransitionKindEmergencyExit {
		t.Fatalf("expected latest transition kind %s, got %s", tablecustody.TransitionKindEmergencyExit, transition.Kind)
	}
	if len(transition.Approvals) != 1 || transition.Approvals[0].PlayerID != host.walletID.PlayerID {
		t.Fatalf("expected only local approval on emergency exit, got %+v", transition.Approvals)
	}
	if !tableHasEventType(table, "EmergencyExit") {
		t.Fatal("expected EmergencyExit event after exit")
	}
	if latestStackAmount(table.LatestCustodyState, host.walletID.PlayerID) != 0 {
		t.Fatalf("expected emergency exit to clear local stack, got %d", latestStackAmount(table.LatestCustodyState, host.walletID.PlayerID))
	}
}

func TestGuestCashOutCancelsPendingNextHandStart(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createJoinedTwoPlayerTable(t, host, guest)
	err := host.store.withTableLock(tableID, func() error {
		table, err := host.store.readTable(tableID)
		if err != nil || table == nil {
			return err
		}
		table.NextHandAt = addMillis(nowISO(), -1)
		return host.persistAndReplicate(table, true)
	})
	if err != nil {
		t.Fatalf("arm overdue next hand: %v", err)
	}

	if _, err := guest.CashOut(tableID); err != nil {
		t.Fatalf("guest cash out: %v", err)
	}

	host.Tick()

	hostTable := mustReadNativeTable(t, host, tableID)
	if hostTable.ActiveHand != nil {
		t.Fatalf("expected no active hand after guest cash-out, got phase=%s", hostTable.ActiveHand.State.Phase)
	}
	if hostTable.Config.Status != "seating" {
		t.Fatalf("expected host table to return to seating after guest cash-out, got %q", hostTable.Config.Status)
	}
	if hostTable.NextHandAt != "" {
		t.Fatalf("expected pending next hand timer to clear after guest cash-out, got %q", hostTable.NextHandAt)
	}
	if latestStackAmount(hostTable.LatestCustodyState, guest.walletID.PlayerID) != 0 {
		t.Fatalf("expected guest stack to be zero after cash-out, got %d", latestStackAmount(hostTable.LatestCustodyState, guest.walletID.PlayerID))
	}
}

func TestSyntheticRealModeGuestCashOutUsesHostAuthority(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createJoinedTwoPlayerTable(t, host, guest)
	enableSyntheticRealMode(host, guest)

	if _, err := guest.CashOut(tableID); err != nil {
		t.Fatalf("guest cash out in synthetic real mode: %v", err)
	}

	hostTable := mustReadNativeTable(t, host, tableID)
	if hostTable.Config.Status != "seating" {
		t.Fatalf("expected host table to return to seating, got %q", hostTable.Config.Status)
	}
	if latestStackAmount(hostTable.LatestCustodyState, guest.walletID.PlayerID) != 0 {
		t.Fatalf("expected guest stack to be zero after synthetic real cash-out, got %d", latestStackAmount(hostTable.LatestCustodyState, guest.walletID.PlayerID))
	}
	if !tableHasEventType(hostTable, "CashOut") {
		t.Fatal("expected host to append CashOut event after guest synthetic real cash-out")
	}
}

func TestSyntheticRealModeGuestCashOutAfterSettledHand(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)
	enableSyntheticRealMode(host, guest)

	waitForLocalCanAct(t, []*meshRuntime{host, guest}, host, tableID)
	if _, err := host.SendAction(tableID, game.Action{Type: game.ActionCall}); err != nil {
		t.Fatalf("host send preflop call: %v", err)
	}
	waitForLocalCanAct(t, []*meshRuntime{host, guest}, guest, tableID)
	if _, err := guest.SendAction(tableID, game.Action{Type: game.ActionBet, TotalSats: 800}); err != nil {
		t.Fatalf("guest send preflop bet: %v", err)
	}
	waitForLocalCanAct(t, []*meshRuntime{host, guest}, host, tableID)
	if _, err := host.SendAction(tableID, game.Action{Type: game.ActionCall}); err != nil {
		t.Fatalf("host send preflop call to showdown line: %v", err)
	}
	for index, actor := range []*meshRuntime{guest, host, guest, host, guest, host} {
		waitForLocalCanAct(t, []*meshRuntime{host, guest}, actor, tableID)
		if _, err := actor.SendAction(tableID, game.Action{Type: game.ActionCheck}); err != nil {
			t.Fatalf("send river-line check %d: %v", index, err)
		}
	}
	waitForHandPhase(t, []*meshRuntime{host, guest}, host, tableID, game.StreetSettled)
	waitForHandPhase(t, []*meshRuntime{host, guest}, guest, tableID, game.StreetSettled)
	waitForCustodySync(t, []*meshRuntime{host, guest}, host, guest, tableID)

	if _, err := guest.CashOut(tableID); err != nil {
		t.Fatalf("guest cash out after settled synthetic real hand: %v", err)
	}

	hostTable := mustReadNativeTable(t, host, tableID)
	if hostTable.Config.Status != "seating" {
		t.Fatalf("expected host table to return to seating, got %q", hostTable.Config.Status)
	}
	if latestStackAmount(hostTable.LatestCustodyState, guest.walletID.PlayerID) != 0 {
		t.Fatalf("expected guest stack to be zero after settled-hand cash-out, got %d", latestStackAmount(hostTable.LatestCustodyState, guest.walletID.PlayerID))
	}
}

func TestGuestCashOutRefreshesRemoteCustodyAfterPeerCashOut(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createJoinedTwoPlayerTable(t, host, guest)

	host.tableSyncSender = func(peerURL string, input nativeTableSyncRequest) error {
		return nil
	}
	if _, err := host.CashOut(tableID); err != nil {
		t.Fatalf("host cash out: %v", err)
	}

	staleGuest := mustReadNativeTable(t, guest, tableID)
	if latestStackAmount(staleGuest.LatestCustodyState, host.walletID.PlayerID) == 0 {
		t.Fatal("expected guest view to remain stale when host cash-out replication is dropped")
	}
	guest.lastSyncAt[tableID] = time.Now()

	if _, err := guest.CashOut(tableID); err != nil {
		t.Fatalf("guest cash out after stale host state: %v", err)
	}

	hostTable := mustReadNativeTable(t, host, tableID)
	if latestStackAmount(hostTable.LatestCustodyState, guest.walletID.PlayerID) != 0 {
		t.Fatalf("expected guest stack to be zero after stale-state cash-out recovery, got %d", latestStackAmount(hostTable.LatestCustodyState, guest.walletID.PlayerID))
	}
}

func TestSyntheticRealModeSettledHandCashOutProofRefsCoverRemainingClaims(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)
	enableSyntheticRealMode(host, guest)

	waitForLocalCanAct(t, []*meshRuntime{host, guest}, host, tableID)
	if _, err := host.SendAction(tableID, game.Action{Type: game.ActionCall}); err != nil {
		t.Fatalf("host send preflop call: %v", err)
	}
	waitForLocalCanAct(t, []*meshRuntime{host, guest}, guest, tableID)
	if _, err := guest.SendAction(tableID, game.Action{Type: game.ActionBet, TotalSats: 800}); err != nil {
		t.Fatalf("guest send preflop bet: %v", err)
	}
	waitForLocalCanAct(t, []*meshRuntime{host, guest}, host, tableID)
	if _, err := host.SendAction(tableID, game.Action{Type: game.ActionCall}); err != nil {
		t.Fatalf("host send preflop call to showdown line: %v", err)
	}
	for _, actor := range []*meshRuntime{guest, host, guest, host, guest, host} {
		waitForLocalCanAct(t, []*meshRuntime{host, guest}, actor, tableID)
		if _, err := actor.SendAction(tableID, game.Action{Type: game.ActionCheck}); err != nil {
			t.Fatalf("send showdown-line check: %v", err)
		}
	}
	waitForHandPhase(t, []*meshRuntime{host, guest}, host, tableID, game.StreetSettled)
	waitForHandPhase(t, []*meshRuntime{host, guest}, guest, tableID, game.StreetSettled)
	waitForCustodySync(t, []*meshRuntime{host, guest}, host, guest, tableID)

	table := mustReadNativeTable(t, host, tableID)
	transition, err := host.buildFundsCustodyTransitionForPlayer(table, guest.walletID.PlayerID, tablecustody.TransitionKindCashOut, "completed")
	if err != nil {
		t.Fatalf("build settled-hand guest cash-out transition: %v", err)
	}
	normalized, _, err := host.normalizedCustodySigningTransition(table, transition)
	if err != nil {
		t.Fatalf("normalize settled-hand guest cash-out transition: %v", err)
	}
	fundsRequest, err := guest.buildSignedFundsRequest(table, "cashout")
	if err != nil {
		t.Fatalf("build settled-hand guest cash-out request: %v", err)
	}
	if err := host.validatePrebuiltCustodySigningTransition(table, transition.PrevStateHash, custodyTransitionRequestHash(normalized), normalized, authorizerForFundsRequest(fundsRequest)); err != nil {
		t.Fatalf("validate normalized settled-hand guest cash-out transition: %v", err)
	}
	wallet, err := guest.walletSummary()
	if err != nil {
		t.Fatalf("guest wallet summary: %v", err)
	}
	result, _, _, err := host.settleTableFundsForPlayer(table, transition, authorizerForFundsRequest(fundsRequest), guest.walletID.PlayerID, wallet.ArkAddress)
	if err != nil {
		t.Fatalf("settle settled-hand guest cash-out transition: %v", err)
	}
	transition.ArkIntentID = result.IntentID
	transition.ArkTxID = result.ArkTxID
	transition.Proof = tablecustody.CustodyProof{
		ArkIntentID:     result.IntentID,
		ArkTxID:         result.ArkTxID,
		FinalizedAt:     result.FinalizedAt,
		ReplayValidated: true,
		StateHash:       transition.NextStateHash,
		VTXORefs:        append(stackProofRefs(transition.NextState), result.OutputRefs["wallet-return"]...),
	}
	transition.Proof.TransitionHash = tablecustody.HashCustodyTransition(transition)
	if err := validateAcceptedCustodyRefs(table.LatestCustodyState, transition, true); err != nil {
		t.Fatalf("validate settled-hand guest cash-out refs: %v", err)
	}
}

func TestSettledHandCashOutSignerPrepareAcceptsNormalizedTransition(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)
	enableSyntheticRealMode(host, guest)

	waitForLocalCanAct(t, []*meshRuntime{host, guest}, host, tableID)
	if _, err := host.SendAction(tableID, game.Action{Type: game.ActionCall}); err != nil {
		t.Fatalf("host send preflop call: %v", err)
	}
	waitForLocalCanAct(t, []*meshRuntime{host, guest}, guest, tableID)
	if _, err := guest.SendAction(tableID, game.Action{Type: game.ActionBet, TotalSats: 800}); err != nil {
		t.Fatalf("guest send preflop bet: %v", err)
	}
	waitForLocalCanAct(t, []*meshRuntime{host, guest}, host, tableID)
	if _, err := host.SendAction(tableID, game.Action{Type: game.ActionCall}); err != nil {
		t.Fatalf("host send preflop call to showdown line: %v", err)
	}
	for _, actor := range []*meshRuntime{guest, host, guest, host, guest, host} {
		waitForLocalCanAct(t, []*meshRuntime{host, guest}, actor, tableID)
		if _, err := actor.SendAction(tableID, game.Action{Type: game.ActionCheck}); err != nil {
			t.Fatalf("send showdown-line check: %v", err)
		}
	}
	waitForHandPhase(t, []*meshRuntime{host, guest}, host, tableID, game.StreetSettled)
	waitForHandPhase(t, []*meshRuntime{host, guest}, guest, tableID, game.StreetSettled)
	waitForCustodySync(t, []*meshRuntime{host, guest}, host, guest, tableID)

	table := mustReadNativeTable(t, host, tableID)
	transition, err := host.buildFundsCustodyTransitionForPlayer(table, guest.walletID.PlayerID, tablecustody.TransitionKindCashOut, "completed")
	if err != nil {
		t.Fatalf("build settled-hand guest cash-out transition: %v", err)
	}
	fundsRequest, err := guest.buildSignedFundsRequest(table, "cashout")
	if err != nil {
		t.Fatalf("build settled-hand guest cash-out request: %v", err)
	}
	signingTransition, plan, err := host.normalizedCustodySigningTransition(table, transition)
	if err != nil {
		t.Fatalf("normalize settled-hand guest cash-out transition: %v", err)
	}
	wallet, err := guest.walletSummary()
	if err != nil {
		t.Fatalf("guest wallet summary: %v", err)
	}
	claim, ok := latestStackClaimForPlayer(table.LatestCustodyState, guest.walletID.PlayerID)
	if !ok {
		t.Fatal("missing guest stack claim")
	}
	feeSats, err := host.estimatedCustodyBatchFee(len(plan.Inputs), 1, 0, 0)
	if err != nil {
		t.Fatalf("estimate cash-out fee: %v", err)
	}
	settledAmount := stackClaimBackedAmount(claim) - feeSats
	if settledAmount <= 0 {
		t.Fatalf("expected positive settled amount, got %d", settledAmount)
	}
	output, err := custodyBatchOutputFromReceiver("wallet-return", guest.walletID.PlayerID, sdktypes.Receiver{
		To:     wallet.ArkAddress,
		Amount: uint64(settledAmount),
	}, nil)
	if err != nil {
		t.Fatalf("build wallet-return output: %v", err)
	}
	if output.Onchain {
		t.Fatal("expected Ark wallet cash-out to use an offchain output in the signer-prepare regression")
	}
	if _, err := guest.handleCustodySignerPrepareFromPeer(nativeCustodySignerPrepareRequest{
		DerivationPath:          "test-cashout-prepare",
		ExpectedPrevStateHash:   table.LatestCustodyState.StateHash,
		ExpectedOffchainOutputs: []custodyBatchOutput{output},
		Authorizer:              authorizerForFundsRequest(fundsRequest),
		PlayerID:                guest.walletID.PlayerID,
		TableID:                 table.Config.TableID,
		TransitionHash:          custodyTransitionRequestHash(signingTransition),
		Transition:              signingTransition,
	}); err != nil {
		t.Fatalf("prepare normalized settled-hand guest cash-out signer: %v", err)
	}
}

func TestActionTransitionSemanticValidationRejectsWrongSuccessor(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)
	enableSyntheticRealMode(host, guest)

	waitForLocalCanAct(t, []*meshRuntime{host, guest}, host, tableID)
	waitForActionableHandState(t, []*meshRuntime{host, guest}, guest, tableID)
	table := waitForLocalCanAct(t, []*meshRuntime{host, guest}, host, tableID)

	actionRequest, err := host.buildSignedActionRequest(table, game.Action{Type: game.ActionCall})
	if err != nil {
		t.Fatalf("build signed call request: %v", err)
	}
	nextState, err := game.ApplyHoldemAction(table.ActiveHand.State, *table.ActiveHand.State.ActingSeatIndex, actionRequest.Action)
	if err != nil {
		t.Fatalf("apply call action: %v", err)
	}
	wrongTransition, err := host.buildCustodyTransition(table, tablecustody.TransitionKindAction, &nextState, &actionRequest.Action, nil)
	if err != nil {
		t.Fatalf("build action transition: %v", err)
	}
	if wrongTransition.Action == nil {
		t.Fatal("expected action descriptor on transition")
	}
	wrongTransition.Action.Type = string(game.ActionFold)
	recomputeCustodyTransitionHashesForTest(&wrongTransition)

	t.Run("approval", func(t *testing.T) {
		if _, err := guest.handleCustodyApprovalFromPeer(nativeCustodyApprovalRequest{
			ExpectedPrevStateHash: wrongTransition.PrevStateHash,
			Authorizer:            authorizerForActionRequest(actionRequest),
			PlayerID:              guest.walletID.PlayerID,
			TableID:               tableID,
			Transition:            wrongTransition,
		}); err == nil || !strings.Contains(err.Error(), "does not match the locally derived successor") {
			t.Fatalf("expected semantic approval rejection, got %v", err)
		}
	})

	t.Run("psbt sign", func(t *testing.T) {
		signingTransition, plan, err := host.normalizedCustodySigningTransition(table, wrongTransition)
		if err != nil {
			t.Fatalf("normalize wrong action transition: %v", err)
		}
		proofPSBT, err := buildCustodyProofPSBTForTest(plan)
		if err != nil {
			t.Fatalf("build proof psbt: %v", err)
		}
		if _, err := guest.handleCustodyTxSignFromPeer(nativeCustodyTxSignRequest{
			ExpectedPrevStateHash: wrongTransition.PrevStateHash,
			Authorizer:            authorizerForActionRequest(actionRequest),
			PlayerID:              guest.walletID.PlayerID,
			PSBT:                  proofPSBT,
			Purpose:               "proof",
			TableID:               tableID,
			Transition:            signingTransition,
			TransitionHash:        custodyTransitionRequestHash(signingTransition),
		}); err == nil || !strings.Contains(err.Error(), "does not match the locally derived successor") {
			t.Fatalf("expected semantic psbt-sign rejection, got %v", err)
		}
	})

	t.Run("signer prepare", func(t *testing.T) {
		signingTransition, plan, err := host.normalizedCustodySigningTransition(table, wrongTransition)
		if err != nil {
			t.Fatalf("normalize wrong action transition: %v", err)
		}
		expectedOutputs := make([]custodyBatchOutput, 0, len(plan.Outputs))
		for _, output := range plan.Outputs {
			batchOutput := custodyBatchOutputFromSpec(output)
			if batchOutput.Onchain {
				continue
			}
			expectedOutputs = append(expectedOutputs, batchOutput)
		}
		if _, err := guest.handleCustodySignerPrepareFromPeer(nativeCustodySignerPrepareRequest{
			DerivationPath:          "test-action-prepare",
			ExpectedPrevStateHash:   wrongTransition.PrevStateHash,
			ExpectedOffchainOutputs: expectedOutputs,
			Authorizer:              authorizerForActionRequest(actionRequest),
			PlayerID:                guest.walletID.PlayerID,
			TableID:                 tableID,
			Transition:              signingTransition,
			TransitionHash:          custodyTransitionRequestHash(signingTransition),
		}); err == nil || !strings.Contains(err.Error(), "does not match the locally derived successor") {
			t.Fatalf("expected semantic signer-prepare rejection, got %v", err)
		}
	})
}

func TestActionTransitionSemanticValidationRejectsTamperedCustodyBindings(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)
	enableSyntheticRealMode(host, guest)

	waitForLocalCanAct(t, []*meshRuntime{host, guest}, host, tableID)
	waitForActionableHandState(t, []*meshRuntime{host, guest}, guest, tableID)
	table := waitForLocalCanAct(t, []*meshRuntime{host, guest}, host, tableID)

	actionRequest, err := host.buildSignedActionRequest(table, game.Action{Type: game.ActionCall})
	if err != nil {
		t.Fatalf("build signed call request: %v", err)
	}
	nextState, err := game.ApplyHoldemAction(table.ActiveHand.State, *table.ActiveHand.State.ActingSeatIndex, actionRequest.Action)
	if err != nil {
		t.Fatalf("apply call action: %v", err)
	}
	wrongTransition, err := host.buildCustodyTransitionWithOverrides(table, tablecustody.TransitionKindAction, &nextState, &actionRequest.Action, nil, actionRequestBindingOverrides(actionRequest))
	if err != nil {
		t.Fatalf("build action transition: %v", err)
	}
	wrongTransition.NextState.ActionDeadlineAt = addMillis(wrongTransition.NextState.ActionDeadlineAt, 1_000)
	recomputeCustodyTransitionHashesForTest(&wrongTransition)

	t.Run("approval", func(t *testing.T) {
		if _, err := guest.handleCustodyApprovalFromPeer(nativeCustodyApprovalRequest{
			ExpectedPrevStateHash: wrongTransition.PrevStateHash,
			Authorizer:            authorizerForActionRequest(actionRequest),
			PlayerID:              guest.walletID.PlayerID,
			TableID:               tableID,
			Transition:            wrongTransition,
		}); err == nil || !strings.Contains(err.Error(), "does not match the locally derived successor") {
			t.Fatalf("expected binding-aware approval rejection, got %v", err)
		}
	})

	t.Run("psbt sign", func(t *testing.T) {
		signingTransition, plan, err := host.normalizedCustodySigningTransition(table, wrongTransition)
		if err != nil {
			t.Fatalf("normalize wrong action transition: %v", err)
		}
		proofPSBT, err := buildCustodyProofPSBTForTest(plan)
		if err != nil {
			t.Fatalf("build proof psbt: %v", err)
		}
		if _, err := guest.handleCustodyTxSignFromPeer(nativeCustodyTxSignRequest{
			ExpectedPrevStateHash: wrongTransition.PrevStateHash,
			Authorizer:            authorizerForActionRequest(actionRequest),
			PlayerID:              guest.walletID.PlayerID,
			PSBT:                  proofPSBT,
			Purpose:               "proof",
			TableID:               tableID,
			Transition:            signingTransition,
			TransitionHash:        custodyTransitionRequestHash(signingTransition),
		}); err == nil || !strings.Contains(err.Error(), "does not match the locally derived successor") {
			t.Fatalf("expected binding-aware psbt-sign rejection, got %v", err)
		}
	})

	t.Run("signer prepare", func(t *testing.T) {
		signingTransition, plan, err := host.normalizedCustodySigningTransition(table, wrongTransition)
		if err != nil {
			t.Fatalf("normalize wrong action transition: %v", err)
		}
		expectedOutputs := make([]custodyBatchOutput, 0, len(plan.Outputs))
		for _, output := range plan.Outputs {
			batchOutput := custodyBatchOutputFromSpec(output)
			if batchOutput.Onchain {
				continue
			}
			expectedOutputs = append(expectedOutputs, batchOutput)
		}
		if _, err := guest.handleCustodySignerPrepareFromPeer(nativeCustodySignerPrepareRequest{
			DerivationPath:          "test-action-binding-prepare",
			ExpectedPrevStateHash:   wrongTransition.PrevStateHash,
			ExpectedOffchainOutputs: expectedOutputs,
			Authorizer:              authorizerForActionRequest(actionRequest),
			PlayerID:                guest.walletID.PlayerID,
			TableID:                 tableID,
			Transition:              signingTransition,
			TransitionHash:          custodyTransitionRequestHash(signingTransition),
		}); err == nil || !strings.Contains(err.Error(), "does not match the locally derived successor") {
			t.Fatalf("expected binding-aware signer-prepare rejection, got %v", err)
		}
	})
}

func TestFundsTransitionSemanticValidationRejectsTamperedCustodyBindings(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createJoinedTwoPlayerTable(t, host, guest)
	table := mustReadNativeTable(t, host, tableID)

	fundsRequest, err := guest.buildSignedFundsRequest(table, "cashout")
	if err != nil {
		t.Fatalf("build signed cash-out request: %v", err)
	}
	wrongTransition, err := host.buildFundsCustodyTransitionForPlayer(table, guest.walletID.PlayerID, tablecustody.TransitionKindCashOut, "completed")
	if err != nil {
		t.Fatalf("build cash-out transition: %v", err)
	}
	wrongTransition.NextState.ChallengeAnchor = strings.Repeat("b", 64)
	recomputeCustodyTransitionHashesForTest(&wrongTransition)

	t.Run("approval", func(t *testing.T) {
		if _, err := guest.handleCustodyApprovalFromPeer(nativeCustodyApprovalRequest{
			ExpectedPrevStateHash: wrongTransition.PrevStateHash,
			Authorizer:            authorizerForFundsRequest(fundsRequest),
			PlayerID:              guest.walletID.PlayerID,
			TableID:               tableID,
			Transition:            wrongTransition,
		}); err == nil || !strings.Contains(err.Error(), "does not match the locally derived successor") {
			t.Fatalf("expected binding-aware approval rejection, got %v", err)
		}
	})

	t.Run("psbt sign", func(t *testing.T) {
		signingTransition, plan, err := host.normalizedCustodySigningTransition(table, wrongTransition)
		if err != nil {
			t.Fatalf("normalize wrong action transition: %v", err)
		}
		proofPSBT, err := buildCustodyProofPSBTForTest(plan)
		if err != nil {
			t.Fatalf("build proof psbt: %v", err)
		}
		if _, err := guest.handleCustodyTxSignFromPeer(nativeCustodyTxSignRequest{
			ExpectedPrevStateHash: wrongTransition.PrevStateHash,
			Authorizer:            authorizerForFundsRequest(fundsRequest),
			PlayerID:              guest.walletID.PlayerID,
			PSBT:                  proofPSBT,
			Purpose:               "proof",
			TableID:               tableID,
			Transition:            signingTransition,
			TransitionHash:        custodyTransitionRequestHash(signingTransition),
		}); err == nil || !strings.Contains(err.Error(), "does not match the locally derived successor") {
			t.Fatalf("expected binding-aware psbt-sign rejection, got %v", err)
		}
	})

	t.Run("signer prepare", func(t *testing.T) {
		signingTransition, plan, err := host.normalizedCustodySigningTransition(table, wrongTransition)
		if err != nil {
			t.Fatalf("normalize wrong action transition: %v", err)
		}
		expectedOutputs := make([]custodyBatchOutput, 0, len(plan.Outputs))
		for _, output := range plan.Outputs {
			batchOutput := custodyBatchOutputFromSpec(output)
			if batchOutput.Onchain {
				continue
			}
			expectedOutputs = append(expectedOutputs, batchOutput)
		}
		if _, err := guest.handleCustodySignerPrepareFromPeer(nativeCustodySignerPrepareRequest{
			DerivationPath:          "test-funds-binding-prepare",
			ExpectedPrevStateHash:   wrongTransition.PrevStateHash,
			ExpectedOffchainOutputs: expectedOutputs,
			Authorizer:              authorizerForFundsRequest(fundsRequest),
			PlayerID:                guest.walletID.PlayerID,
			TableID:                 tableID,
			Transition:              signingTransition,
			TransitionHash:          custodyTransitionRequestHash(signingTransition),
		}); err == nil || !strings.Contains(err.Error(), "does not match the locally derived successor") {
			t.Fatalf("expected binding-aware signer-prepare rejection, got %v", err)
		}
	})
}

func TestShowdownPayoutSemanticValidationRejectsTamperedCustodyBindings(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)
	enableSyntheticRealMode(host, guest)

	waitForLocalCanAct(t, []*meshRuntime{host, guest}, host, tableID)
	if _, err := host.SendAction(tableID, game.Action{Type: game.ActionCall}); err != nil {
		t.Fatalf("host send preflop call: %v", err)
	}
	waitForLocalCanAct(t, []*meshRuntime{host, guest}, guest, tableID)
	if _, err := guest.SendAction(tableID, game.Action{Type: game.ActionBet, TotalSats: 800}); err != nil {
		t.Fatalf("guest send preflop bet: %v", err)
	}
	waitForLocalCanAct(t, []*meshRuntime{host, guest}, host, tableID)
	if _, err := host.SendAction(tableID, game.Action{Type: game.ActionCall}); err != nil {
		t.Fatalf("host send preflop showdown call: %v", err)
	}
	for index, actor := range []*meshRuntime{guest, host, guest, host, guest, host} {
		waitForLocalCanAct(t, []*meshRuntime{host, guest}, actor, tableID)
		if _, err := actor.SendAction(tableID, game.Action{Type: game.ActionCheck}); err != nil {
			t.Fatalf("send showdown-line check %d: %v", index, err)
		}
	}

	table := waitForHandPhase(t, []*meshRuntime{host, guest}, host, tableID, game.StreetShowdownReveal)
	resolution := &tablecustody.TimeoutResolution{
		ActionType:               string(game.ActionFold),
		ActingPlayerID:           guest.walletID.PlayerID,
		DeadPlayerIDs:            []string{guest.walletID.PlayerID},
		LostEligibilityPlayerIDs: []string{guest.walletID.PlayerID},
		Policy:                   defaultCustodyTimeoutPolicy,
		Reason:                   "protocol timeout during showdown-reveal",
	}
	nextState, err := game.ForceFoldSeat(table.ActiveHand.State, 1)
	if err != nil {
		t.Fatalf("force fold missing showdown player: %v", err)
	}
	settledTable := cloneJSON(table)
	settledTable.ActiveHand.State = nextState
	publicState := host.publicStateFromHand(settledTable, nextState)
	settledTable.PublicState = &publicState
	settledTable.ActiveHand.Cards.PhaseDeadlineAt = ""
	wrongTransition, err := host.buildCustodyTransition(settledTable, tablecustody.TransitionKindShowdownPayout, &settledTable.ActiveHand.State, nil, resolution)
	if err != nil {
		t.Fatalf("build showdown payout transition: %v", err)
	}
	wrongTransition.NextState.ChallengeAnchor = strings.Repeat("c", 64)
	recomputeCustodyTransitionHashesForTest(&wrongTransition)

	if err := host.validateCustodyTransitionSemantics(settledTable, wrongTransition, nil); err == nil || !strings.Contains(err.Error(), "does not match the locally derived successor") {
		t.Fatalf("expected binding-aware showdown semantic rejection, got %v", err)
	}
	if err := host.store.writeTable(&settledTable); err != nil {
		t.Fatalf("write settled showdown table: %v", err)
	}
	if _, err := host.handleCustodyApprovalFromPeer(nativeCustodyApprovalRequest{
		ExpectedPrevStateHash: wrongTransition.PrevStateHash,
		PlayerID:              host.walletID.PlayerID,
		TableID:               tableID,
		Transition:            wrongTransition,
	}); err == nil || !strings.Contains(err.Error(), "does not match the locally derived successor") {
		t.Fatalf("expected binding-aware showdown approval rejection, got %v", err)
	}
}

func TestTimeoutTransitionSemanticValidationRejectsTamperedTranscriptBindings(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)
	waitForLocalCanAct(t, []*meshRuntime{host, guest}, host, tableID)
	if _, err := host.SendAction(tableID, game.Action{Type: game.ActionCall}); err != nil {
		t.Fatalf("host send call: %v", err)
	}
	table := waitForLocalCanAct(t, []*meshRuntime{host, guest}, guest, tableID)
	expiredState := cloneJSON(*table.LatestCustodyState)
	expiredState.ActionDeadlineAt = addMillis(nowISO(), -1)
	table.LatestCustodyState = &expiredState

	legalActions := game.GetLegalActions(table.ActiveHand.State, table.ActiveHand.State.ActingSeatIndex)
	actionTypes := make([]string, 0, len(legalActions))
	for _, legalAction := range legalActions {
		actionTypes = append(actionTypes, string(legalAction.Type))
	}
	resolution := tablecustody.BuildTimeoutResolution(timeoutPolicyFromState(table.LatestCustodyState), guest.walletID.PlayerID, actionTypes, []string{guest.walletID.PlayerID})
	action := game.Action{Type: game.ActionFold}
	if resolution.ActionType == string(game.ActionCheck) {
		action = game.Action{Type: game.ActionCheck}
	}
	nextState, err := game.ApplyHoldemAction(table.ActiveHand.State, *table.ActiveHand.State.ActingSeatIndex, action)
	if err != nil {
		t.Fatalf("apply timeout action: %v", err)
	}
	wrongTransition, err := host.buildCustodyTransition(table, tablecustody.TransitionKindTimeout, &nextState, &action, &resolution)
	if err != nil {
		t.Fatalf("build timeout transition: %v", err)
	}
	wrongTransition.NextState.ChallengeAnchor = strings.Repeat("d", 64)
	recomputeCustodyTransitionHashesForTest(&wrongTransition)

	if err := host.validateCustodyTransitionSemantics(table, wrongTransition, nil); err == nil || !strings.Contains(err.Error(), "does not match the locally derived successor") {
		t.Fatalf("expected binding-aware timeout semantic rejection, got %v", err)
	}
	if err := host.store.writeTable(&table); err != nil {
		t.Fatalf("write timeout table: %v", err)
	}
	if _, err := host.handleCustodyApprovalFromPeer(nativeCustodyApprovalRequest{
		ExpectedPrevStateHash: wrongTransition.PrevStateHash,
		PlayerID:              host.walletID.PlayerID,
		TableID:               tableID,
		Transition:            wrongTransition,
	}); err == nil || !strings.Contains(err.Error(), "does not match the locally derived successor") {
		t.Fatalf("expected binding-aware timeout approval rejection, got %v", err)
	}
}

func TestBlindPostSemanticValidationRejectsTamperedTranscriptBindings(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createJoinedTwoPlayerTable(t, host, guest)
	table := mustReadNativeTable(t, host, tableID)

	handID := randomUUID()
	handNumber := 1
	wrongTransition, err := host.deriveBlindPostCustodyTransition(table, handID, handNumber)
	if err != nil {
		t.Fatalf("derive blind-post transition: %v", err)
	}
	wrongTransition.NextState.ChallengeAnchor = strings.Repeat("e", 64)
	recomputeCustodyTransitionHashesForTest(&wrongTransition)

	if err := host.validateCustodyTransitionSemantics(table, wrongTransition, nil); err == nil || !strings.Contains(err.Error(), "does not match the locally derived successor") {
		t.Fatalf("expected binding-aware blind-post semantic rejection, got %v", err)
	}
	if _, err := guest.handleCustodyApprovalFromPeer(nativeCustodyApprovalRequest{
		ExpectedPrevStateHash: wrongTransition.PrevStateHash,
		PlayerID:              guest.walletID.PlayerID,
		TableID:               tableID,
		Transition:            wrongTransition,
	}); err == nil || !strings.Contains(err.Error(), "does not match the locally derived successor") {
		t.Fatalf("expected binding-aware blind-post approval rejection, got %v", err)
	}
}

func TestBlindPostSemanticValidationRejectsTamperedActionDeadline(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createJoinedTwoPlayerTable(t, host, guest)
	table := mustReadNativeTable(t, host, tableID)

	handID := randomUUID()
	handNumber := 1
	wrongTransition, err := host.deriveBlindPostCustodyTransition(table, handID, handNumber)
	if err != nil {
		t.Fatalf("derive blind-post transition: %v", err)
	}
	wrongTransition.NextState.ActionDeadlineAt = addMillis(wrongTransition.NextState.ActionDeadlineAt, 1_000)
	recomputeCustodyTransitionHashesForTest(&wrongTransition)

	if err := host.validateCustodyTransitionSemantics(table, wrongTransition, nil); err == nil || !strings.Contains(err.Error(), "does not match the locally derived successor") {
		t.Fatalf("expected deadline-aware blind-post semantic rejection, got %v", err)
	}
	if _, err := guest.handleCustodyApprovalFromPeer(nativeCustodyApprovalRequest{
		ExpectedPrevStateHash: wrongTransition.PrevStateHash,
		PlayerID:              guest.walletID.PlayerID,
		TableID:               tableID,
		Transition:            wrongTransition,
	}); err == nil || !strings.Contains(err.Error(), "does not match the locally derived successor") {
		t.Fatalf("expected deadline-aware blind-post approval rejection, got %v", err)
	}
}

func TestFinalizeCustodyTransitionFailsClosedWithoutRealArkSettlement(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)
	table := mustReadNativeTable(t, host, tableID)
	host.config.UseMockSettlement = false

	transition, err := host.buildSeatLockTransition(table)
	if err != nil {
		t.Fatalf("build custody transition: %v", err)
	}
	if err := host.finalizeCustodyTransition(&table, &transition, nil); err == nil {
		t.Fatal("expected finalize to fail closed in real settlement mode")
	}
}

func TestCashOutFailsClosedWithoutRealArkSettlement(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createJoinedTwoPlayerTable(t, host, guest)
	host.config.UseMockSettlement = false

	if _, err := host.CashOut(tableID); err == nil {
		t.Fatal("expected cash-out to fail closed in real settlement mode")
	}
}

func TestCustodyTxSigningRejectsPSBTOutsideAuthorizedTransition(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")
	enableSyntheticRealMode(host, guest)

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)
	waitForLocalCanAct(t, []*meshRuntime{host, guest}, host, tableID)
	waitForActionableHandState(t, []*meshRuntime{host, guest}, guest, tableID)
	acting := waitForLocalCanAct(t, []*meshRuntime{host, guest}, host, tableID)
	actionRequest, err := host.buildSignedActionRequest(acting, game.Action{Type: game.ActionCall})
	if err != nil {
		t.Fatalf("build signed call request: %v", err)
	}
	nextState, err := game.ApplyHoldemAction(acting.ActiveHand.State, *acting.ActiveHand.State.ActingSeatIndex, game.Action{Type: game.ActionCall})
	if err != nil {
		t.Fatalf("apply call action: %v", err)
	}
	transition, err := host.buildCustodyTransitionWithOverrides(acting, tablecustody.TransitionKindAction, &nextState, &game.Action{Type: game.ActionCall}, nil, actionRequestBindingOverrides(actionRequest))
	if err != nil {
		t.Fatalf("build custody transition: %v", err)
	}
	signingTransition, plan, err := host.normalizedCustodySigningTransition(acting, transition)
	if err != nil {
		t.Fatalf("normalize custody signing transition: %v", err)
	}
	intentInputs, leafProofs, arkFields, locktime, err := custodyIntentInputs(plan.Inputs)
	if err != nil {
		t.Fatalf("build intent inputs: %v", err)
	}
	txOutputs := make([]*wire.TxOut, 0, len(plan.Outputs))
	for _, output := range plan.Outputs {
		txOut, err := decodeBatchOutputTxOut(custodyBatchOutputFromSpec(output))
		if err != nil {
			t.Fatalf("decode custody output: %v", err)
		}
		txOutputs = append(txOutputs, txOut)
	}
	if len(txOutputs) == 0 {
		t.Fatal("expected custody outputs for call transition")
	}
	txOutputs[0] = &wire.TxOut{Value: txOutputs[0].Value + 1, PkScript: txOutputs[0].PkScript}
	message, err := custodyRegisterMessage(custodyOnchainOutputIndexes(offchainCustodyBatchOutputs(nil)), nil)
	if err != nil {
		t.Fatalf("register message: %v", err)
	}
	maliciousPSBT, err := custodyBuildProofPSBT(message, intentInputs, txOutputs, leafProofs, arkFields, locktime)
	if err != nil {
		t.Fatalf("build malicious psbt: %v", err)
	}
	request := nativeCustodyTxSignRequest{
		ExpectedPrevStateHash: transition.PrevStateHash,
		Authorizer:            authorizerForActionRequest(actionRequest),
		PlayerID:              guest.walletID.PlayerID,
		PSBT:                  maliciousPSBT,
		Purpose:               "proof",
		TableID:               tableID,
		Transition:            signingTransition,
		TransitionHash:        custodyTransitionRequestHash(signingTransition),
	}
	if _, err := guest.handleCustodyTxSignFromPeer(request); err == nil || !strings.Contains(err.Error(), "authorized") {
		t.Fatalf("expected malicious psbt to be rejected, got %v", err)
	}
}

func TestValidateCustodyTransitionSemanticsRejectsEarlyTimeout(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)
	table := waitForLocalCanAct(t, []*meshRuntime{host, guest}, host, tableID)
	legalActions := game.GetLegalActions(table.ActiveHand.State, table.ActiveHand.State.ActingSeatIndex)
	actionTypes := make([]string, 0, len(legalActions))
	for _, legalAction := range legalActions {
		actionTypes = append(actionTypes, string(legalAction.Type))
	}
	resolution := tablecustody.BuildTimeoutResolution(timeoutPolicyFromState(table.LatestCustodyState), host.walletID.PlayerID, actionTypes, []string{host.walletID.PlayerID})
	action := game.Action{Type: game.ActionFold}
	if resolution.ActionType == string(game.ActionCheck) {
		action = game.Action{Type: game.ActionCheck}
	}
	nextState, err := game.ApplyHoldemAction(table.ActiveHand.State, *table.ActiveHand.State.ActingSeatIndex, action)
	if err != nil {
		t.Fatalf("apply timeout action: %v", err)
	}
	transition, err := host.buildCustodyTransition(table, tablecustody.TransitionKindTimeout, &nextState, &action, &resolution)
	if err != nil {
		t.Fatalf("build timeout transition: %v", err)
	}

	if err := host.validateCustodyTransitionSemantics(table, transition, nil); err == nil || !strings.Contains(err.Error(), "before the custody deadline") {
		t.Fatalf("expected early timeout successor to be rejected, got %v", err)
	}
}

func TestValidateCustodyTransitionSemanticsRejectsWrongDerivedTimeoutResolution(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)
	waitForLocalCanAct(t, []*meshRuntime{host, guest}, host, tableID)
	if _, err := host.SendAction(tableID, game.Action{Type: game.ActionCall}); err != nil {
		t.Fatalf("host send call: %v", err)
	}
	table := waitForLocalCanAct(t, []*meshRuntime{host, guest}, guest, tableID)
	expiredState := cloneJSON(*table.LatestCustodyState)
	expiredState.ActionDeadlineAt = addMillis(nowISO(), -1)
	table.LatestCustodyState = &expiredState

	wrongResolution := tablecustody.TimeoutResolution{
		ActionType:               string(game.ActionCheck),
		ActingPlayerID:           guest.walletID.PlayerID,
		DeadPlayerIDs:            []string{guest.walletID.PlayerID},
		LostEligibilityPlayerIDs: []string{guest.walletID.PlayerID},
		Policy:                   timeoutPolicyFromState(table.LatestCustodyState),
		Reason:                   "action deadline expired",
	}
	nextState, err := game.ApplyHoldemAction(table.ActiveHand.State, *table.ActiveHand.State.ActingSeatIndex, game.Action{Type: game.ActionCheck})
	if err != nil {
		t.Fatalf("apply wrong timeout check: %v", err)
	}
	transition, err := host.buildCustodyTransition(table, tablecustody.TransitionKindTimeout, &nextState, &game.Action{Type: game.ActionCheck}, &wrongResolution)
	if err != nil {
		t.Fatalf("build wrong timeout transition: %v", err)
	}

	if err := host.validateCustodyTransitionSemantics(table, transition, nil); err == nil || !strings.Contains(err.Error(), "does not match the locally derived successor") {
		t.Fatalf("expected wrong timeout successor to be rejected, got %v", err)
	}
}

func TestSyntheticRealModeSupportsCallThenCheck(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)
	enableSyntheticRealMode(host, guest)

	waitForLocalCanAct(t, []*meshRuntime{host, guest}, host, tableID)
	if _, err := host.SendAction(tableID, game.Action{Type: game.ActionCall}); err != nil {
		t.Fatalf("host send call in synthetic real mode: %v", err)
	}
	waitForLocalCanAct(t, []*meshRuntime{host, guest}, guest, tableID)
	if _, err := guest.SendAction(tableID, game.Action{Type: game.ActionCheck}); err != nil {
		t.Fatalf("guest send check in synthetic real mode: %v", err)
	}

	table := waitForHandPhase(t, []*meshRuntime{host, guest}, host, tableID, game.StreetFlop)
	if got := len(table.ActiveHand.State.ActionLog); got != 2 {
		t.Fatalf("expected two actions in synthetic real mode, got %d", got)
	}
	lastTransition := table.CustodyTransitions[len(table.CustodyTransitions)-1]
	if lastTransition.Kind != tablecustody.TransitionKindAction {
		t.Fatalf("expected latest custody transition kind %s, got %s", tablecustody.TransitionKindAction, lastTransition.Kind)
	}
	if lastTransition.ArkTxID != "" {
		t.Fatalf("expected zero-money check transition to avoid Ark settlement, got %q", lastTransition.ArkTxID)
	}
}

func TestAcceptRemoteTableReplaysBlindPostAfterLocalTimingModeSwitch(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)
	enableSyntheticRealMode(host, guest)

	if err := guest.acceptRemoteTable(mustReadNativeTable(t, host, tableID)); err != nil {
		t.Fatalf("accept remote table after timing mode switch: %v", err)
	}
}

func TestActionDrivenShowdownArmsProtocolDeadline(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)

	waitForLocalCanAct(t, []*meshRuntime{host, guest}, host, tableID)
	if _, err := host.SendAction(tableID, game.Action{Type: game.ActionCall}); err != nil {
		t.Fatalf("host send preflop call: %v", err)
	}
	waitForLocalCanAct(t, []*meshRuntime{host, guest}, guest, tableID)
	if _, err := guest.SendAction(tableID, game.Action{Type: game.ActionBet, TotalSats: 800}); err != nil {
		t.Fatalf("guest send preflop bet: %v", err)
	}
	waitForLocalCanAct(t, []*meshRuntime{host, guest}, host, tableID)
	if _, err := host.SendAction(tableID, game.Action{Type: game.ActionCall}); err != nil {
		t.Fatalf("host send preflop call to showdown line: %v", err)
	}
	for index, actor := range []*meshRuntime{guest, host, guest, host, guest, host} {
		waitForLocalCanAct(t, []*meshRuntime{host, guest}, actor, tableID)
		if _, err := actor.SendAction(tableID, game.Action{Type: game.ActionCheck}); err != nil {
			t.Fatalf("send river-line check %d: %v", index, err)
		}
	}

	table := waitForHandPhase(t, []*meshRuntime{host, guest}, host, tableID, game.StreetShowdownReveal)
	if table.ActiveHand.Cards.PhaseDeadlineAt == "" {
		t.Fatal("expected showdown-reveal phase to have a protocol deadline")
	}
}

func TestRealSettlementUsesExtendedHostFailureWindow(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")
	witness := newMeshTestRuntime(t, "witness")

	if _, err := host.BootstrapPeer(witness.selfPeerURL(), "", nil); err != nil {
		t.Fatalf("bootstrap witness peer: %v", err)
	}
	tableID, _ := createStartedTwoPlayerTable(t, host, guest, witness.selfPeerID())

	host.config.UseMockSettlement = false
	guest.config.UseMockSettlement = false
	witness.config.UseMockSettlement = false

	table := mustReadNativeTable(t, witness, tableID)
	table.LastHostHeartbeatAt = addMillis(nowISO(), -(nativeHostFailureMS + 500))
	if err := witness.store.writeTable(&table); err != nil {
		t.Fatalf("write witness table: %v", err)
	}

	witness.Tick()

	updated := mustReadNativeTable(t, witness, tableID)
	if updated.CurrentHost.Peer.PeerID != host.selfPeerID() {
		t.Fatalf("expected host to remain current host under real-mode heartbeat window, got %q", updated.CurrentHost.Peer.PeerID)
	}
}

func newMeshTestRuntime(t *testing.T, profileName string) *meshRuntime {
	t.Helper()

	rootDir := t.TempDir()
	runtimeConfig, err := cfg.ResolveRuntimeConfig(map[string]string{
		"cache-type":    "memory",
		"daemon-dir":    filepath.Join(rootDir, "daemons"),
		"datadir":       rootDir,
		"db-path":       filepath.Join(rootDir, "storage", "core.sqlite"),
		"event-db-path": filepath.Join(rootDir, "storage", "events.badger"),
		"mock":          "true",
		"network":       "regtest",
		"peer-port":     "0",
		"profile-dir":   filepath.Join(rootDir, "profiles"),
		"run-dir":       filepath.Join(rootDir, "runs"),
	})
	if err != nil {
		t.Fatalf("resolve runtime config: %v", err)
	}

	runtime, err := newMeshRuntime(profileName, runtimeConfig, "player")
	if err != nil {
		t.Fatalf("new mesh runtime: %v", err)
	}
	t.Cleanup(func() {
		_ = runtime.Close()
	})

	if _, err := runtime.Bootstrap(profileName, ""); err != nil {
		t.Fatalf("bootstrap mesh runtime %s: %v", profileName, err)
	}
	return runtime
}

func enableSyntheticRealMode(runtimes ...*meshRuntime) {
	for _, runtime := range runtimes {
		if runtime == nil {
			continue
		}
		runtime.config.UseMockSettlement = false
		runtime.custodyArkVerify = func(refs []tablecustody.VTXORef, requireSpendable bool) error {
			return nil
		}
		runtime.custodyBatchExecute = func(table nativeTableState, prevStateHash, transitionHash string, inputs []custodyInputSpec, proofSignerIDs, treeSignerIDs []string, outputs []custodyBatchOutput) (*custodyBatchResult, error) {
			intentID := "synthetic-intent-" + transitionHash[:12]
			txID := transitionHash
			outputRefs := map[string][]tablecustody.VTXORef{}
			vout := uint32(0)
			for _, output := range outputs {
				if output.Onchain {
					continue
				}
				outputRefs[output.ClaimKey] = append(outputRefs[output.ClaimKey], tablecustody.VTXORef{
					AmountSats:    output.AmountSats,
					ArkIntentID:   intentID,
					ArkTxID:       txID,
					ExpiresAt:     addMillis(nowISO(), 86_400_000),
					OwnerPlayerID: output.OwnerPlayerID,
					Script:        output.Script,
					Tapscripts:    append([]string(nil), output.Tapscripts...),
					TxID:          txID,
					VOut:          vout,
				})
				vout++
			}
			return &custodyBatchResult{
				ArkTxID:     txID,
				FinalizedAt: nowISO(),
				IntentID:    intentID,
				OutputRefs:  outputRefs,
			}, nil
		}
	}
}

func createJoinedTwoPlayerTable(t *testing.T, host, guest *meshRuntime, witnessPeerIDs ...string) (string, string) {
	t.Helper()

	createResult, err := host.CreateTable(map[string]any{
		"name":           "Native Runtime Test Table",
		"witnessPeerIds": witnessPeerIDs,
	})
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	inviteCode := stringValue(createResult["inviteCode"])
	if inviteCode == "" {
		t.Fatal("expected invite code from table creation")
	}
	if _, err := host.JoinTable(inviteCode, 4_000); err != nil {
		t.Fatalf("host join table: %v", err)
	}
	if _, err := guest.JoinTable(inviteCode, 4_000); err != nil {
		t.Fatalf("guest join table: %v", err)
	}
	tableID := host.currentTableID()
	if tableID == "" {
		t.Fatal("expected current table id after joins")
	}
	return tableID, inviteCode
}

func createStartedTwoPlayerTable(t *testing.T, host, guest *meshRuntime, witnessPeerIDs ...string) (string, string) {
	t.Helper()

	tableID, inviteCode := createJoinedTwoPlayerTable(t, host, guest, witnessPeerIDs...)
	startNextHandForTest(t, host, tableID)
	waitForHandPhase(t, []*meshRuntime{host, guest}, host, tableID, game.StreetPreflop)
	return tableID, inviteCode
}

func mustReadNativeTable(t *testing.T, runtime *meshRuntime, tableID string) nativeTableState {
	t.Helper()

	table, err := runtime.requireLocalTable(tableID)
	if err != nil {
		t.Fatalf("read local table %s: %v", tableID, err)
	}
	return *table
}

func mustFetchNativeTableWithoutAuth(t *testing.T, runtime *meshRuntime, tableID string) nativeTableState {
	t.Helper()

	peerInfo, err := runtime.fetchPeerInfo(runtime.selfPeerURL())
	if err != nil {
		t.Fatalf("fetch local peer info: %v", err)
	}
	request, requestKey, err := runtime.newOutboundEnvelope(
		nativeTransportMessageTablePull,
		nativeTransportChannelTable,
		tableID,
		peerInfo.Peer.PeerID,
		nativeTableFetchRequest{TableID: tableID},
		peerInfo.TransportPubkeyHex,
	)
	if err != nil {
		t.Fatalf("new anonymous table request: %v", err)
	}
	response, err := runtime.exchangePeerTransport(runtime.selfPeerURL(), request)
	if err != nil {
		t.Fatalf("anonymous table fetch: %v", err)
	}
	body, err := runtime.decodeResponseEnvelope(response, requestKey)
	if err != nil {
		t.Fatalf("decode anonymous table envelope: %v", err)
	}
	var table nativeTableState
	if err := json.Unmarshal(body, &table); err != nil {
		t.Fatalf("decode anonymous table fetch: %v", err)
	}
	return table
}

func startNextHandForTest(t *testing.T, runtime *meshRuntime, tableID string) {
	t.Helper()

	expiredNextHandAt := addMillis(nowISO(), -1)
	peerURLs := []string{}
	err := runtime.store.withTableLock(tableID, func() error {
		table, err := runtime.store.readTable(tableID)
		if err != nil || table == nil {
			return err
		}
		table.LastHostHeartbeatAt = nowISO()
		table.NextHandAt = expiredNextHandAt
		if err := runtime.persistAndReplicate(table, true); err != nil {
			return err
		}
		seenPeerURL := map[string]struct{}{}
		addPeerURL := func(peerURL string) {
			if peerURL == "" || peerURL == runtime.selfPeerURL() {
				return
			}
			if _, ok := seenPeerURL[peerURL]; ok {
				return
			}
			seenPeerURL[peerURL] = struct{}{}
			peerURLs = append(peerURLs, peerURL)
		}
		for _, witness := range table.Witnesses {
			addPeerURL(witness.Peer.PeerURL)
		}
		for _, seat := range table.Seats {
			addPeerURL(firstNonEmptyString(seat.PeerURL, runtime.knownPeerURL(seat.PeerID)))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("start next hand: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for _, peerURL := range peerURLs {
		synced := false
		for time.Now().Before(deadline) {
			remote, err := runtime.fetchRemoteTable(peerURL, tableID)
			if err == nil && remote != nil && remote.NextHandAt == expiredNextHandAt {
				synced = true
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if !synced {
			t.Fatalf("start next hand: peer %s did not accept nextHandAt %q", peerURL, expiredNextHandAt)
		}
	}
	err = runtime.store.withTableLock(tableID, func() error {
		table, err := runtime.store.readTable(tableID)
		if err != nil || table == nil {
			return err
		}
		table.LastHostHeartbeatAt = nowISO()
		table.NextHandAt = expiredNextHandAt
		if err := runtime.startNextHandLocked(table); err != nil {
			return err
		}
		return runtime.persistAndReplicate(table, true)
	})
	if err != nil {
		t.Fatalf("start next hand: %v", err)
	}
	started := mustReadNativeTable(t, runtime, tableID)
	if started.ActiveHand == nil {
		t.Fatalf("expected active hand after direct start, got status=%q nextHandAt=%q", started.Config.Status, started.NextHandAt)
	}
}

func mustBuildJoinRequest(t *testing.T, runtime *meshRuntime, tableID, peerURL string) nativeJoinRequest {
	t.Helper()

	profile, err := runtime.loadProfileState()
	if err != nil {
		t.Fatalf("load profile state: %v", err)
	}
	wallet, err := runtime.walletSummary()
	if err != nil {
		t.Fatalf("wallet summary: %v", err)
	}
	funding, err := runtime.walletRuntime.BuildBuyInFundingBundle(runtime.profileName, 4_000)
	if err != nil {
		t.Fatalf("build buy-in funding bundle: %v", err)
	}
	request := nativeJoinRequest{
		ArkAddress:       wallet.ArkAddress,
		BuyInSats:        4_000,
		FundingRefs:      funding.Refs,
		FundingTotalSats: funding.TotalSats,
		Nickname:         profile.Nickname,
		Peer:             runtime.self.Peer,
		ProfileName:      runtime.profileName,
		ProtocolID:       runtime.protocolID,
		TableID:          tableID,
		WalletPlayerID:   runtime.walletID.PlayerID,
		WalletPubkeyHex:  runtime.walletID.PublicKeyHex,
	}
	if peerURL != "" {
		request.Peer.PeerURL = peerURL
	}
	binding, err := settlementcore.BuildIdentityBinding(tableID, runtime.selfPeerID(), request.Peer.PeerURL, runtime.protocolIdentity, runtime.walletID, nowISO())
	if err != nil {
		t.Fatalf("build join identity binding: %v", err)
	}
	request.IdentityBinding = binding
	return request
}

func touchLocalHostHeartbeat(t *testing.T, runtime *meshRuntime, tableID string) {
	t.Helper()

	table := mustReadNativeTable(t, runtime, tableID)
	table.LastHostHeartbeatAt = nowISO()
	if err := runtime.store.writeTable(&table); err != nil {
		t.Fatalf("write simulated host heartbeat: %v", err)
	}
}

func waitForHandPhase(t *testing.T, runtimes []*meshRuntime, reader *meshRuntime, tableID string, phase game.Street) nativeTableState {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		table := mustReadNativeTable(t, reader, tableID)
		if table.ActiveHand != nil && table.ActiveHand.State.Phase == phase {
			return table
		}
		for _, runtime := range runtimes {
			if runtime != nil {
				runtime.Tick()
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	table := mustReadNativeTable(t, reader, tableID)
	t.Fatalf("timed out waiting for phase %s, last phase=%v", phase, func() any {
		if table.ActiveHand == nil {
			return nil
		}
		return table.ActiveHand.State.Phase
	}())
	return nativeTableState{}
}

func waitForActionLogLength(t *testing.T, runtimes []*meshRuntime, reader *meshRuntime, tableID string, want int) nativeTableState {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		table := mustReadNativeTable(t, reader, tableID)
		if table.ActiveHand != nil && len(table.ActiveHand.State.ActionLog) == want {
			return table
		}
		for _, runtime := range runtimes {
			if runtime != nil {
				runtime.Tick()
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	table := mustReadNativeTable(t, reader, tableID)
	got := -1
	if table.ActiveHand != nil {
		got = len(table.ActiveHand.State.ActionLog)
	}
	t.Fatalf("timed out waiting for action log length %d, got %d", want, got)
	return nativeTableState{}
}

func waitForLocalCanAct(t *testing.T, runtimes []*meshRuntime, reader *meshRuntime, tableID string) nativeTableState {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		table := mustReadNativeTable(t, reader, tableID)
		if reader.localTableView(table).Local.CanAct {
			return table
		}
		for _, runtime := range runtimes {
			if runtime != nil {
				runtime.Tick()
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for local canAct on table %s", tableID)
	return nativeTableState{}
}

func waitForActionableHandState(t *testing.T, runtimes []*meshRuntime, reader *meshRuntime, tableID string) nativeTableState {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		table := mustReadNativeTable(t, reader, tableID)
		if table.ActiveHand != nil && game.PhaseAllowsActions(table.ActiveHand.State.Phase) {
			return table
		}
		for _, runtime := range runtimes {
			if runtime != nil {
				runtime.Tick()
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	table := mustReadNativeTable(t, reader, tableID)
	t.Fatalf("timed out waiting for actionable hand state, last phase=%v", func() any {
		if table.ActiveHand == nil {
			return nil
		}
		return table.ActiveHand.State.Phase
	}())
	return nativeTableState{}
}

func waitForSettledHand(t *testing.T, runtimes []*meshRuntime, reader *meshRuntime, tableID string) nativeTableState {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		table := mustReadNativeTable(t, reader, tableID)
		if table.ActiveHand != nil && table.ActiveHand.State.Phase == game.StreetSettled {
			return table
		}
		for _, runtime := range runtimes {
			if runtime != nil {
				runtime.Tick()
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	table := mustReadNativeTable(t, reader, tableID)
	t.Fatalf("timed out waiting for settled hand, last phase=%v", func() any {
		if table.ActiveHand == nil {
			return nil
		}
		return table.ActiveHand.State.Phase
	}())
	return nativeTableState{}
}

func waitForCustodySync(t *testing.T, runtimes []*meshRuntime, left, right *meshRuntime, tableID string) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		leftTable := mustReadNativeTable(t, left, tableID)
		rightTable := mustReadNativeTable(t, right, tableID)
		leftHash := ""
		rightHash := ""
		if leftTable.LatestCustodyState != nil {
			leftHash = leftTable.LatestCustodyState.StateHash
		}
		if rightTable.LatestCustodyState != nil {
			rightHash = rightTable.LatestCustodyState.StateHash
		}
		if leftHash != "" && leftHash == rightHash && len(leftTable.CustodyTransitions) == len(rightTable.CustodyTransitions) {
			return
		}
		for _, runtime := range runtimes {
			if runtime != nil {
				runtime.Tick()
			}
		}
		time.Sleep(25 * time.Millisecond)
	}

	leftTable := mustReadNativeTable(t, left, tableID)
	rightTable := mustReadNativeTable(t, right, tableID)
	leftHash := ""
	rightHash := ""
	if leftTable.LatestCustodyState != nil {
		leftHash = leftTable.LatestCustodyState.StateHash
	}
	if rightTable.LatestCustodyState != nil {
		rightHash = rightTable.LatestCustodyState.StateHash
	}
	t.Fatalf("timed out waiting for custody sync: left_hash=%s right_hash=%s left_transitions=%d right_transitions=%d", leftHash, rightHash, len(leftTable.CustodyTransitions), len(rightTable.CustodyTransitions))
}

func assertTranscriptProtectedCards(t *testing.T, table nativeTableState) {
	t.Helper()

	if table.ActiveHand == nil {
		t.Fatalf("expected active hand, status=%q publicState=%+v events=%d", table.Config.Status, table.PublicState, len(table.Events))
	}
	if table.ActiveHand.Cards.Transcript.RootHash == "" {
		t.Fatal("expected non-empty transcript root")
	}
	if len(table.ActiveHand.Cards.FinalDeck) != 52 {
		t.Fatalf("expected 52 encrypted deck entries, got %d", len(table.ActiveHand.Cards.FinalDeck))
	}
	root, err := game.ReplayTranscriptRoot(table.ActiveHand.Cards.Transcript)
	if err != nil {
		t.Fatalf("replay transcript root: %v", err)
	}
	if root != table.ActiveHand.Cards.Transcript.RootHash {
		t.Fatalf("expected transcript root %q, got %q", table.ActiveHand.Cards.Transcript.RootHash, root)
	}
	for _, record := range table.ActiveHand.Cards.Transcript.Records {
		switch record.Kind {
		case nativeHandMessageBoardOpen, nativeHandMessageShowdownReveal:
			continue
		default:
			if len(record.Cards) != 0 {
				t.Fatalf("expected non-owner transcript record %q to avoid plaintext cards, got %+v", record.Kind, record.Cards)
			}
		}
	}
}

func assertOwnerLocalCards(t *testing.T, runtime *meshRuntime, table nativeTableState) {
	t.Helper()

	view := runtime.localTableView(table)
	cards, ok := extractStringSlice(view.Local.MyHoleCards)
	if !ok || len(cards) != 2 {
		t.Fatalf("expected owner-local hole cards, got %#v", view.Local.MyHoleCards)
	}
	for _, card := range cards {
		if len(card) != 2 {
			t.Fatalf("expected card code, got %q", card)
		}
	}
}

func extractStringSlice(value any) ([]string, bool) {
	switch typed := value.(type) {
	case []string:
		return typed, true
	case []any:
		values := make([]string, 0, len(typed))
		for _, entry := range typed {
			text, ok := entry.(string)
			if !ok {
				return nil, false
			}
			values = append(values, text)
		}
		return values, true
	default:
		return nil, false
	}
}

func seatIndexForPlayer(t *testing.T, table nativeTableState, playerID string) int {
	t.Helper()

	for _, seat := range table.Seats {
		if seat.PlayerID == playerID {
			return seat.SeatIndex
		}
	}
	t.Fatalf("missing seat for player %s", playerID)
	return -1
}

func runtimeForPeerID(t *testing.T, peerID string, runtimes ...*meshRuntime) *meshRuntime {
	t.Helper()

	for _, runtime := range runtimes {
		if runtime != nil && runtime.selfPeerID() == peerID {
			return runtime
		}
	}
	t.Fatalf("missing runtime for peer %s", peerID)
	return nil
}

func resignHistoricalEventsForTest(t *testing.T, runtime *meshRuntime, events []NativeSignedTableEvent) ([]NativeSignedTableEvent, string) {
	t.Helper()

	resigned := cloneJSON(events)
	prevHash := ""
	for index := range resigned {
		if resigned[index].SenderPeerID != runtime.selfPeerID() || resigned[index].SenderProtocolPubkeyHex != runtime.protocolIdentity.PublicKeyHex {
			t.Fatalf("expected historical event %d to belong to %s, got peer=%s pubkey=%s", index, runtime.selfPeerID(), resigned[index].SenderPeerID, resigned[index].SenderProtocolPubkeyHex)
		}
		if prevHash == "" {
			resigned[index].PrevEventHash = nil
		} else {
			resigned[index].PrevEventHash = prevHash
		}
		unsigned := rawJSONMap(resigned[index])
		delete(unsigned, "signature")
		signatureHex, err := settlementcore.SignStructuredData(runtime.protocolIdentity.PrivateKeyHex, unsigned)
		if err != nil {
			t.Fatalf("re-sign historical event %d: %v", index, err)
		}
		resigned[index].Signature = signatureHex
		eventHash, err := settlementcore.HashStructuredDataHex(unsigned)
		if err != nil {
			t.Fatalf("hash historical event %d: %v", index, err)
		}
		prevHash = eventHash
	}
	return resigned, prevHash
}

func resignAcceptedTableEventsForTest(t *testing.T, runtime *meshRuntime, table *nativeTableState) {
	t.Helper()

	if table == nil {
		t.Fatal("expected table to re-sign")
	}
	table.Events, table.LastEventHash = resignHistoricalEventsForTest(t, runtime, table.Events)
	if table.PublicState != nil {
		table.PublicState.LatestEventHash = table.LastEventHash
	}
}

func resignActionRequestForTest(t *testing.T, runtime *meshRuntime, request *nativeActionRequest) {
	t.Helper()

	if request == nil {
		t.Fatal("expected action request to re-sign")
	}
	request.SignedAt = nowISO()
	signatureHex, err := settlementcore.SignStructuredData(runtime.walletID.PrivateKeyHex, nativeActionAuthPayload(request.TableID, request.PlayerID, request.HandID, request.PrevCustodyStateHash, request.ChallengeAnchor, request.TranscriptRoot, request.Epoch, request.DecisionIndex, request.Action, request.SignedAt))
	if err != nil {
		t.Fatalf("re-sign action request: %v", err)
	}
	request.SignatureHex = signatureHex
}

func resignFundsRequestForTest(t *testing.T, runtime *meshRuntime, request *nativeFundsRequest) {
	t.Helper()

	if request == nil {
		t.Fatal("expected funds request to re-sign")
	}
	request.SignedAt = nowISO()
	signatureHex, err := settlementcore.SignStructuredData(runtime.walletID.PrivateKeyHex, nativeFundsAuthPayload(request.TableID, request.PlayerID, request.PrevCustodyStateHash, request.Kind, request.ArkAddress, request.Epoch, request.SignedAt))
	if err != nil {
		t.Fatalf("re-sign funds request: %v", err)
	}
	request.SignatureHex = signatureHex
}

func findEventIndexByType(table nativeTableState, eventType string) int {
	for index, event := range table.Events {
		if stringValue(event.Body["type"]) == eventType {
			return index
		}
	}
	return -1
}

func recomputeCustodyTransitionHashesForTest(transition *tablecustody.CustodyTransition) {
	if transition == nil {
		return
	}
	transition.NextState.StateHash = tablecustody.HashCustodyState(transition.NextState)
	transition.NextStateHash = transition.NextState.StateHash
	transition.Proof.StateHash = transition.NextStateHash
	transition.Proof.TransitionHash = tablecustody.HashCustodyTransition(*transition)
}

func buildCustodyProofPSBTForTest(plan *custodySettlementPlan) (string, error) {
	if plan == nil {
		return "", errors.New("missing custody settlement plan")
	}
	intentInputs, leafProofs, arkFields, locktime, err := custodyIntentInputs(plan.Inputs)
	if err != nil {
		return "", err
	}
	txOutputs := make([]*wire.TxOut, 0, len(plan.Outputs))
	for _, output := range plan.Outputs {
		txOut, err := decodeBatchOutputTxOut(custodyBatchOutputFromSpec(output))
		if err != nil {
			return "", err
		}
		txOutputs = append(txOutputs, txOut)
	}
	message, err := custodyRegisterMessage(custodyOnchainOutputIndexes(offchainCustodyBatchOutputs(nil)), nil)
	if err != nil {
		return "", err
	}
	return custodyBuildProofPSBT(message, intentInputs, txOutputs, leafProofs, arkFields, locktime)
}

func resignHistoricalSnapshotForTest(t *testing.T, runtime *meshRuntime, snapshot *NativeCooperativeTableSnapshot) {
	t.Helper()

	unsigned := rawJSONMap(*snapshot)
	delete(unsigned, "signatures")
	signatureHex, err := settlementcore.SignStructuredData(runtime.protocolIdentity.PrivateKeyHex, unsigned)
	if err != nil {
		t.Fatalf("re-sign historical snapshot: %v", err)
	}
	snapshot.Signatures = []NativeTableSnapshotSignature{{
		SignatureHex:    signatureHex,
		SignedAt:        nowISO(),
		SignerPeerID:    runtime.selfPeerID(),
		SignerPubkeyHex: runtime.protocolIdentity.PublicKeyHex,
		SignerRole:      runtime.mode,
	}}
}

func rebuildTranscriptForTest(t *testing.T, transcript game.HandTranscript) game.HandTranscript {
	t.Helper()

	rebuilt := game.HandTranscript{
		HandID:     transcript.HandID,
		HandNumber: transcript.HandNumber,
		Records:    []game.HandTranscriptRecord{},
		RootHash:   "",
		TableID:    transcript.TableID,
	}
	for _, record := range transcript.Records {
		nextRecord := cloneJSON(record)
		nextRecord.Index = 0
		nextRecord.StepHash = ""
		nextRecord.RootHash = ""
		next, _, err := game.AppendTranscriptRecord(rebuilt, nextRecord)
		if err != nil {
			t.Fatalf("rebuild transcript: %v", err)
		}
		rebuilt = next
	}
	return rebuilt
}

func tableHasEventType(table nativeTableState, eventType string) bool {
	for _, event := range table.Events {
		if stringValue(event.Body["type"]) == eventType {
			return true
		}
	}
	return false
}
