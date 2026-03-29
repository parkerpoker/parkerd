package parker

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	cfg "github.com/danieldresner/arkade_fun/internal/config"
	"github.com/danieldresner/arkade_fun/internal/game"
	"github.com/danieldresner/arkade_fun/internal/settlementcore"
)

func TestTableTrafficKeepsHoleCardsOwnerLocalAndPushesTranscriptUpdates(t *testing.T) {
	t.Parallel()

	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)
	waitForHandPhase(t, []*meshRuntime{host, guest}, host, tableID, game.StreetPreflop)

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
	waitForHandPhase(t, []*meshRuntime{host, guest, witness}, host, tableID, game.StreetPreflop)

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

func TestHandleActionRejectsForgedSeatOwnerSignature(t *testing.T) {
	t.Parallel()

	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)
	table := waitForHandPhase(t, []*meshRuntime{host, guest}, host, tableID, game.StreetPreflop)
	coordinator := runtimeForPeerID(t, table.CurrentHost.Peer.PeerID, host, guest)

	signedAt := nowISO()
	forged := nativeActionRequest{
		Action:        game.Action{Type: game.ActionCall},
		DecisionIndex: len(table.ActiveHand.State.ActionLog),
		Epoch:         table.CurrentEpoch,
		HandID:        table.ActiveHand.State.HandID,
		PlayerID:      host.walletID.PlayerID,
		ProfileName:   guest.profileName,
		SignedAt:      signedAt,
		TableID:       tableID,
	}
	signatureHex, err := settlementcore.SignStructuredData(guest.walletID.PrivateKeyHex, nativeActionAuthPayload(forged.TableID, forged.PlayerID, forged.HandID, forged.Epoch, forged.DecisionIndex, forged.Action, forged.SignedAt))
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
	table := waitForHandPhase(t, []*meshRuntime{host, guest}, host, tableID, game.StreetPreflop)
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
	table := waitForHandPhase(t, []*meshRuntime{host, guest}, host, tableID, game.StreetPreflop)
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

func TestFailoverKeepsActiveTranscriptDrivenHandRunning(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	player := newMeshTestRuntime(t, "player")
	witness := newMeshTestRuntime(t, "witness")

	if _, err := host.BootstrapPeer(witness.selfPeerURL(), "", nil); err != nil {
		t.Fatalf("bootstrap witness peer: %v", err)
	}
	tableID, _ := createStartedTwoPlayerTable(t, host, player, witness.selfPeerID())
	started := waitForHandPhase(t, []*meshRuntime{host, player, witness}, host, tableID, game.StreetPreflop)
	originalHandID := started.ActiveHand.State.HandID
	originalRoot := started.ActiveHand.Cards.Transcript.RootHash

	table := mustReadNativeTable(t, witness, tableID)
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
	if err := guest.Close(); err != nil {
		t.Fatalf("close guest runtime: %v", err)
	}

	startNextHandForTest(t, host, tableID)
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
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("timed out waiting for missing reveal timeout to settle the hand")
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

func TestSyncRouteRejectsForgedEnvelope(t *testing.T) {
	t.Parallel()

	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)
	waitForHandPhase(t, []*meshRuntime{host, guest}, host, tableID, game.StreetPreflop)

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
	waitForHandPhase(t, []*meshRuntime{host, guest}, host, tableID, game.StreetPreflop)

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
	waitForHandPhase(t, []*meshRuntime{host, guest}, host, tableID, game.StreetPreflop)

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
	waitForHandPhase(t, []*meshRuntime{host, guest}, host, tableID, game.StreetPreflop)

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

		if err := guest.acceptRemoteTable(tampered); err == nil || !strings.Contains(err.Error(), "historical event") {
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
	waitForHandPhase(t, []*meshRuntime{host, guest}, host, tableID, game.StreetPreflop)

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

		if err := guest.acceptRemoteTable(tampered); err == nil || !strings.Contains(err.Error(), "public state") {
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

		if err := guest.acceptRemoteTable(tampered); err == nil || !strings.Contains(err.Error(), "snapshot") {
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

		if err := auditor.acceptRemoteTable(tampered); err == nil || !strings.Contains(err.Error(), "not anchored") {
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
	if err := guest.Close(); err != nil {
		t.Fatalf("close guest runtime: %v", err)
	}

	startNextHandForTest(t, host, tableID)

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
	waitForHandPhase(t, []*meshRuntime{host, guest}, host, tableID, game.StreetPreflop)

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

	table := mustReadNativeTable(t, runtime, tableID)
	table.LastHostHeartbeatAt = addMillis(nowISO(), -(nativeHostHeartbeatMS + 100))
	table.NextHandAt = addMillis(nowISO(), -1)
	if err := runtime.store.writeTable(&table); err != nil {
		t.Fatalf("write scheduled hand start: %v", err)
	}
	runtime.Tick()
	started := mustReadNativeTable(t, runtime, tableID)
	if started.ActiveHand == nil {
		t.Fatalf("expected host tick to start active hand, got status=%q nextHandAt=%q", started.Config.Status, started.NextHandAt)
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
	request := nativeJoinRequest{
		ArkAddress:      wallet.ArkAddress,
		BuyInSats:       4_000,
		Nickname:        profile.Nickname,
		Peer:            runtime.self.Peer,
		ProfileName:     runtime.profileName,
		ProtocolID:      runtime.protocolID,
		TableID:         tableID,
		WalletPlayerID:  runtime.walletID.PlayerID,
		WalletPubkeyHex: runtime.walletID.PublicKeyHex,
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

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, runtime := range runtimes {
			if runtime != nil {
				runtime.Tick()
			}
		}
		table := mustReadNativeTable(t, reader, tableID)
		if table.ActiveHand != nil && table.ActiveHand.State.Phase == phase {
			return table
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

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, runtime := range runtimes {
			if runtime != nil {
				runtime.Tick()
			}
		}
		table := mustReadNativeTable(t, reader, tableID)
		if table.ActiveHand != nil && len(table.ActiveHand.State.ActionLog) == want {
			return table
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
		for _, runtime := range runtimes {
			if runtime != nil {
				runtime.Tick()
			}
		}
		table := mustReadNativeTable(t, reader, tableID)
		if reader.localTableView(table).Local.CanAct {
			return table
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for local canAct on table %s", tableID)
	return nativeTableState{}
}

func waitForSettledHand(t *testing.T, runtimes []*meshRuntime, reader *meshRuntime, tableID string) nativeTableState {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, runtime := range runtimes {
			if runtime != nil {
				runtime.Tick()
			}
		}
		table := mustReadNativeTable(t, reader, tableID)
		if table.ActiveHand != nil && table.ActiveHand.State.Phase == game.StreetSettled {
			return table
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
