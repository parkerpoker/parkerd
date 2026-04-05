package meshruntime

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	arklib "github.com/arkade-os/arkd/pkg/ark-lib"
	arkscript "github.com/arkade-os/arkd/pkg/ark-lib/script"
	arktree "github.com/arkade-os/arkd/pkg/ark-lib/tree"
	sdktypes "github.com/arkade-os/go-sdk/types"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	cfg "github.com/parkerpoker/parkerd/internal/config"
	"github.com/parkerpoker/parkerd/internal/game"
	"github.com/parkerpoker/parkerd/internal/settlementcore"
	"github.com/parkerpoker/parkerd/internal/tablecustody"
	walletpkg "github.com/parkerpoker/parkerd/internal/wallet"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}

type meshTestRuntimeConfig struct {
	chainTipStatus func(profileName string) (walletpkg.ChainTipStatus, error)
	chainTxStatus  func(profileName, txid string) (walletpkg.ChainTransactionStatus, error)
}

type meshTestRuntimeOption func(*meshTestRuntimeConfig)

const (
	syntheticRealTestActionTimeoutMS   = 4000
	syntheticRealTestProtocolTimeoutMS = 5000
)

func withMeshTestChainTipStatus(fn func(profileName string) (walletpkg.ChainTipStatus, error)) meshTestRuntimeOption {
	return func(config *meshTestRuntimeConfig) {
		config.chainTipStatus = fn
	}
}

func withMeshTestChainTxStatus(fn func(profileName, txid string) (walletpkg.ChainTransactionStatus, error)) meshTestRuntimeOption {
	return func(config *meshTestRuntimeConfig) {
		config.chainTxStatus = fn
	}
}

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

func TestGuestSendActionForcesFailoverWhenCurrentHostWillNotConfirmCandidate(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)

	waitForLocalCanAct(t, []*meshRuntime{host, guest}, host, tableID)
	if _, err := host.SendAction(tableID, game.Action{Type: game.ActionCall}); err != nil {
		t.Fatalf("host send action: %v", err)
	}
	guestTable := waitForLocalCanAct(t, []*meshRuntime{host, guest}, guest, tableID)
	if guestTable.CurrentHost.Peer.PeerID != host.selfPeerID() {
		t.Fatalf("expected host %s before failover retry, got %s", host.selfPeerID(), guestTable.CurrentHost.Peer.PeerID)
	}

	if err := host.Close(); err != nil {
		t.Fatalf("close host transport: %v", err)
	}
	guest.httpClient = &http.Client{
		Timeout: guest.httpClient.Timeout,
		Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return nil, errors.New("simulated host transport failure")
		}),
	}

	view, err := guest.SendAction(tableID, game.Action{Type: game.ActionCheck})
	if err != nil {
		t.Fatalf("expected guest action to succeed via failover retry, got %v", err)
	}
	if view.Config.HostPeerID != guest.selfPeerID() {
		t.Fatalf("expected local view host %s after failover retry, got %s", guest.selfPeerID(), view.Config.HostPeerID)
	}

	updated := mustReadNativeTable(t, guest, tableID)
	if updated.CurrentHost.Peer.PeerID != guest.selfPeerID() {
		t.Fatalf("expected guest host after failover retry, got %s", updated.CurrentHost.Peer.PeerID)
	}
	if updated.CurrentEpoch != guestTable.CurrentEpoch+1 {
		t.Fatalf("expected failover retry to advance epoch to %d, got %d", guestTable.CurrentEpoch+1, updated.CurrentEpoch)
	}
	if got := len(updated.ActiveHand.State.ActionLog); got != 2 {
		t.Fatalf("expected retried action to land in the active hand, got %d actions", got)
	}
	last := updated.ActiveHand.State.ActionLog[len(updated.ActiveHand.State.ActionLog)-1]
	if last.Action.Type != game.ActionCheck {
		t.Fatalf("expected retried guest action %s, got %+v", game.ActionCheck, last.Action)
	}
}

func TestHostTickReleasesTableLockBeforeSlowReplication(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createJoinedTwoPlayerTable(t, host, guest)

	if err := host.store.withTableLock(tableID, func() error {
		table, err := host.store.readTable(tableID)
		if err != nil || table == nil {
			return err
		}
		table.LastHostHeartbeatAt = addMillis(nowISO(), -(host.hostHeartbeatIntervalMS() + 100))
		return host.store.writeTable(table)
	}); err != nil {
		t.Fatalf("age host heartbeat: %v", err)
	}

	replicationStarted := make(chan struct{})
	var replicationCalls int
	var replicationMu sync.Mutex
	host.tableSyncSender = func(peerURL string, input nativeTableSyncRequest) error {
		replicationMu.Lock()
		replicationCalls++
		callNumber := replicationCalls
		replicationMu.Unlock()
		if callNumber == 1 {
			close(replicationStarted)
		}
		time.Sleep(700 * time.Millisecond)
		return nil
	}

	tickDone := make(chan struct{})
	go func() {
		host.Tick()
		close(tickDone)
	}()

	select {
	case <-replicationStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for host tick replication to start")
	}

	start := time.Now()
	if _, err := guest.CashOut(tableID); err != nil {
		t.Fatalf("guest cash out during slow host replication: %v", err)
	}
	if elapsed := time.Since(start); elapsed >= 1200*time.Millisecond {
		t.Fatalf("expected guest request to avoid host tick replication delay, took %s", elapsed)
	}

	select {
	case <-tickDone:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for host tick to finish")
	}
}

func TestTablePullWaitsForLockedTableUpdate(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createJoinedTwoPlayerTable(t, host, guest)
	startNextHandForTest(t, host, tableID)

	initial := mustReadNativeTable(t, host, tableID)
	if initial.ActiveHand == nil {
		t.Fatal("expected active hand before locked table pull test")
	}
	originalDeadline := initial.ActiveHand.Cards.PhaseDeadlineAt
	updatedDeadline := addMillis(nowISO(), 90_000)

	type fetchResult struct {
		completedAt time.Time
		err         error
		table       *nativeTableState
	}
	startFetch := make(chan struct{})
	results := make(chan fetchResult, 1)
	go func() {
		<-startFetch
		table, err := guest.fetchRemoteTable(host.selfPeerURL(), tableID)
		results <- fetchResult{
			completedAt: time.Now(),
			err:         err,
			table:       table,
		}
	}()

	pullCompletedEarly := false
	earlyErr := error(nil)
	earlyDeadline := ""
	if err := host.store.withTableLock(tableID, func() error {
		table, err := host.store.readTable(tableID)
		if err != nil || table == nil {
			return err
		}
		if table.ActiveHand == nil {
			return errors.New("expected active hand while holding host table lock")
		}
		table.LastHostHeartbeatAt = nowISO()
		if err := host.persistLocalTable(table, false); err != nil {
			return err
		}
		close(startFetch)
		time.Sleep(150 * time.Millisecond)
		select {
		case result := <-results:
			pullCompletedEarly = true
			earlyErr = result.err
			if result.table != nil && result.table.ActiveHand != nil {
				earlyDeadline = result.table.ActiveHand.Cards.PhaseDeadlineAt
			}
		default:
		}
		table.ActiveHand.Cards.PhaseDeadlineAt = updatedDeadline
		return host.persistLocalTable(table, false)
	}); err != nil {
		t.Fatalf("perform locked host update: %v", err)
	}

	if pullCompletedEarly {
		t.Fatalf("expected table pull to wait for locked update, got deadline=%q err=%v", earlyDeadline, earlyErr)
	}

	releasedAt := time.Now()
	select {
	case result := <-results:
		if result.err != nil {
			t.Fatalf("fetch remote table after locked update: %v", result.err)
		}
		if result.table == nil || result.table.ActiveHand == nil {
			t.Fatalf("expected active hand from pulled table, got %+v", result.table)
		}
		if result.completedAt.Before(releasedAt) {
			t.Fatalf("expected table pull to complete after lock release, got %s before %s", result.completedAt.Format(time.RFC3339Nano), releasedAt.Format(time.RFC3339Nano))
		}
		if got := result.table.ActiveHand.Cards.PhaseDeadlineAt; got != updatedDeadline {
			t.Fatalf("expected pulled table deadline %q after locked update, got %q (original %q)", updatedDeadline, got, originalDeadline)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for locked table pull to finish")
	}
}

func TestHandleHandMessageReturnsBeforeSlowReplication(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createJoinedTwoPlayerTable(t, host, guest)
	if err := host.store.withTableLock(tableID, func() error {
		table, err := host.store.readTable(tableID)
		if err != nil || table == nil {
			return err
		}
		table.LastHostHeartbeatAt = nowISO()
		table.NextHandAt = addMillis(nowISO(), -1000)
		if err := host.startNextHandLocked(table); err != nil {
			return err
		}
		return host.persistLocalTable(table, true)
	}); err != nil {
		t.Fatalf("start next hand without replication: %v", err)
	}

	host.Tick()
	commitTable := mustReadNativeTable(t, host, tableID)
	commitRecord, err := guest.buildLocalContributionRecord(commitTable)
	if err != nil {
		t.Fatalf("build guest commitment: %v", err)
	}
	if commitRecord == nil || commitRecord.Kind != nativeHandMessageFairnessCommit {
		t.Fatalf("expected guest fairness commit, got %#v", commitRecord)
	}
	commitRequest, err := guest.buildSignedHandMessageRequest(commitTable, *commitRecord)
	if err != nil {
		t.Fatalf("build guest commitment request: %v", err)
	}
	if _, err := host.handleHandMessageFromPeer(commitRequest); err != nil {
		t.Fatalf("host handle guest commitment: %v", err)
	}

	host.Tick()
	revealTable := mustReadNativeTable(t, host, tableID)
	revealRecord, err := guest.buildLocalContributionRecord(revealTable)
	if err != nil {
		t.Fatalf("build guest reveal: %v", err)
	}
	if revealRecord == nil || revealRecord.Kind != nativeHandMessageFairnessReveal {
		t.Fatalf("expected guest fairness reveal, got %#v", revealRecord)
	}
	revealRequest, err := guest.buildSignedHandMessageRequest(revealTable, *revealRecord)
	if err != nil {
		t.Fatalf("build guest reveal request: %v", err)
	}
	if _, err := host.handleHandMessageFromPeer(revealRequest); err != nil {
		t.Fatalf("host handle guest reveal: %v", err)
	}

	table := mustReadNativeTable(t, host, tableID)
	if table.ActiveHand == nil || table.ActiveHand.State.Phase != game.StreetPrivateDelivery {
		t.Fatalf("expected private-delivery phase, got %+v", table.ActiveHand)
	}
	hostSeatIndex := 0
	guestSeatIndex := 1
	if _, ok := findTranscriptRecord(table.ActiveHand.Cards.Transcript, nativeHandMessagePrivateDelivery, &hostSeatIndex, string(game.StreetPrivateDelivery), &guestSeatIndex); !ok {
		t.Fatalf("expected host private-delivery share before guest reply, transcript=%+v", table.ActiveHand.Cards.Transcript)
	}

	host.tableSyncSender = func(peerURL string, input nativeTableSyncRequest) error {
		time.Sleep(1500 * time.Millisecond)
		return nil
	}

	record, err := guest.buildLocalContributionRecord(table)
	if err != nil {
		t.Fatalf("build guest private-delivery share: %v", err)
	}
	if record == nil || record.Kind != nativeHandMessagePrivateDelivery {
		t.Fatalf("expected guest private-delivery record, got %#v", record)
	}
	request, err := guest.buildSignedHandMessageRequest(table, *record)
	if err != nil {
		t.Fatalf("build guest hand message request: %v", err)
	}

	start := time.Now()
	updated, err := host.handleHandMessageFromPeer(request)
	if err != nil {
		t.Fatalf("host handle guest private-delivery share: %v", err)
	}
	if elapsed := time.Since(start); elapsed >= 1200*time.Millisecond {
		t.Fatalf("expected hand message response before replication fanout, took %s", elapsed)
	}
	if updated.Table.ActiveHand == nil {
		t.Fatalf("expected active hand after private-delivery completion")
	}
	if updated.Table.ActiveHand.State.Phase != game.StreetPreflop {
		t.Fatalf("expected preflop after private-delivery completion, got %s", updated.Table.ActiveHand.State.Phase)
	}
	if updated.Table.PublicState == nil || stringValue(updated.Table.PublicState.Phase) != string(game.StreetPreflop) {
		t.Fatalf("expected public state to advance to preflop, got %+v", updated.Table.PublicState)
	}
	persisted := mustReadNativeTable(t, host, tableID)
	if persisted.PublicState == nil || stringValue(persisted.PublicState.Phase) != string(game.StreetPreflop) {
		t.Fatalf("expected persisted public state to advance to preflop, got %+v", persisted.PublicState)
	}
}

func TestDriveLocalHandProtocolDedupesRepeatedGuestContributionSends(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createJoinedTwoPlayerTable(t, host, guest)
	startNextHandForTest(t, host, tableID)
	table := waitForHandPhase(t, []*meshRuntime{host, guest}, guest, tableID, game.StreetCommitment)

	sendCount := 0
	guest.handMessageSenderHook = func(peerURL string, input nativeHandMessageRequest) (nativeHandMessageResponse, error) {
		sendCount++
		if input.Kind != nativeHandMessageFairnessCommit {
			t.Fatalf("expected fairness commit send, got %s", input.Kind)
		}
		recordKey, err := handMessageRequestRecordKey(input)
		if err != nil {
			t.Fatalf("compute fairness commit record key: %v", err)
		}
		return nativeHandMessageResponse{
			AcceptedTranscriptRoot: handTranscriptRoot(table),
			RecordKey:              recordKey,
			Table:                  table,
		}, nil
	}

	guest.driveLocalHandProtocol(tableID)
	guest.driveLocalHandProtocol(tableID)

	if sendCount != 1 {
		t.Fatalf("expected duplicate guest contribution sends to be suppressed, got %d sends", sendCount)
	}
}

func TestDriveLocalHandProtocolDedupesBoardSharesAfterHostAck(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)

	waitForLocalCanAct(t, []*meshRuntime{host, guest}, host, tableID)
	if _, err := host.SendAction(tableID, game.Action{Type: game.ActionCall}); err != nil {
		t.Fatalf("host send preflop call: %v", err)
	}
	waitForLocalCanAct(t, []*meshRuntime{host, guest}, guest, tableID)
	if _, err := guest.SendAction(tableID, game.Action{Type: game.ActionCheck}); err != nil {
		t.Fatalf("guest send preflop check: %v", err)
	}

	table := waitForHandPhase(t, []*meshRuntime{host, guest}, guest, tableID, game.StreetFlopReveal)
	sendCount := 0
	guest.handMessageSenderHook = func(peerURL string, input nativeHandMessageRequest) (nativeHandMessageResponse, error) {
		sendCount++
		if input.Kind != nativeHandMessageBoardShare {
			t.Fatalf("expected board-share send, got %s", input.Kind)
		}
		recordKey, err := handMessageRequestRecordKey(input)
		if err != nil {
			t.Fatalf("compute board-share record key: %v", err)
		}
		return nativeHandMessageResponse{
			AcceptedTranscriptRoot: handTranscriptRoot(table),
			RecordKey:              recordKey,
			Table:                  table,
		}, nil
	}

	guest.driveLocalHandProtocol(tableID)
	guest.driveLocalHandProtocol(tableID)

	if sendCount != 1 {
		t.Fatalf("expected board-share retries to stop after host ack, got %d sends", sendCount)
	}
}

func TestDriveLocalHandProtocolStopsRetryingAfterHandMessageReceiptOnStaleHistory(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createJoinedTwoPlayerTable(t, host, guest)
	startNextHandForTest(t, host, tableID)
	stale := waitForHandPhase(t, []*meshRuntime{host, guest}, guest, tableID, game.StreetCommitment)

	sendCount := 0
	guest.handMessageSenderHook = func(peerURL string, input nativeHandMessageRequest) (nativeHandMessageResponse, error) {
		sendCount++
		response, err := host.handleHandMessageFromPeer(input)
		if err != nil {
			return nativeHandMessageResponse{}, err
		}
		if sendCount == 1 {
			if err := guest.store.withTableLock(tableID, func() error {
				table, err := guest.store.readTable(tableID)
				if err != nil || table == nil {
					return err
				}
				extra := cloneJSON(table.Events[len(table.Events)-1])
				extra.Seq++
				extra.Timestamp = nowISO()
				table.Events = append(table.Events, extra)
				return guest.persistLocalTable(table, false)
			}); err != nil {
				t.Fatalf("seed stale guest history: %v", err)
			}
			if err := guest.acceptRemoteTable(cloneJSON(stale)); err == nil || !strings.Contains(err.Error(), "accepted history would roll back table events") {
				t.Fatalf("expected stale history rollback while processing the hand receipt, got %v", err)
			}
			response.Table = stale
		}
		return response, nil
	}

	guest.driveLocalHandProtocol(tableID)
	pending, ok := guest.lastGuestContribution[tableID]
	if !ok {
		t.Fatal("expected pending guest hand contribution after stale accept failure")
	}
	if !pending.acked {
		t.Fatal("expected pending guest hand contribution to be acked after host receipt")
	}

	guest.driveLocalHandProtocol(tableID)
	if sendCount != 1 {
		t.Fatalf("expected host receipt to stop retries after stale accept failure, got %d sends", sendCount)
	}

	restored := cloneJSON(stale)
	if err := guest.persistLocalTable(&restored, false); err != nil {
		t.Fatalf("restore stale guest table before poll sync: %v", err)
	}
	remote, err := guest.fetchRemoteTable(host.selfPeerURL(), tableID)
	if err != nil {
		t.Fatalf("fetch host table after stale accept failure: %v", err)
	}
	if err := guest.acceptRemoteTable(*remote); err != nil {
		t.Fatalf("reconcile guest table after stale accept failure: %v", err)
	}
	guest.reconcileGuestContribution(tableID, *remote)

	reconciled := mustReadNativeTable(t, guest, tableID)
	guestSeat := seatIndexForPlayer(t, reconciled, guest.walletID.PlayerID)
	if _, ok := findTranscriptRecord(reconciled.ActiveHand.Cards.Transcript, nativeHandMessageFairnessCommit, &guestSeat, string(game.StreetCommitment), nil); !ok {
		t.Fatalf("expected reconciled guest transcript to include the fairness commit, transcript=%+v", reconciled.ActiveHand.Cards.Transcript)
	}
	if reconciled.ActiveHand.State.Phase != game.StreetReveal {
		t.Fatalf("expected reconciled guest hand phase to advance to reveal, got %s", reconciled.ActiveHand.State.Phase)
	}
	if _, ok := guest.lastGuestContribution[tableID]; ok {
		t.Fatal("expected reconciled guest contribution state to clear after poll sync")
	}
}

func TestAcceptRemoteTableRejectsActiveHandTranscriptRollback(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createJoinedTwoPlayerTable(t, host, guest)
	startNextHandForTest(t, host, tableID)
	stale := waitForHandPhase(t, []*meshRuntime{host, guest}, guest, tableID, game.StreetCommitment)

	record, err := guest.buildLocalContributionRecord(stale)
	if err != nil {
		t.Fatalf("build guest commitment: %v", err)
	}
	if record == nil || record.Kind != nativeHandMessageFairnessCommit {
		t.Fatalf("expected guest fairness commit, got %#v", record)
	}
	request, err := guest.buildSignedHandMessageRequest(stale, *record)
	if err != nil {
		t.Fatalf("build guest commitment request: %v", err)
	}
	updated, err := host.handleHandMessageFromPeer(request)
	if err != nil {
		t.Fatalf("host handle guest commitment: %v", err)
	}
	if err := guest.acceptRemoteTable(updated.Table); err != nil {
		t.Fatalf("accept updated guest commitment: %v", err)
	}

	if err := guest.acceptRemoteTable(stale); err == nil || !strings.Contains(err.Error(), "accepted active hand transcript would roll back") {
		t.Fatalf("expected stale active-hand transcript rollback to be rejected, got %v", err)
	}
}

func TestNetworkTableViewRepairsStaleActiveHandPublicState(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)
	stale := mustReadNativeTable(t, host, tableID)
	if stale.ActiveHand == nil || stale.PublicState == nil {
		t.Fatalf("expected started table with active hand and public state")
	}

	stalePublic := cloneJSON(*stale.PublicState)
	stalePublic.ActingSeatIndex = nil
	stalePublic.Phase = string(game.StreetPrivateDelivery)
	stale.PublicState = &stalePublic

	view := host.networkTableView(stale, guest.walletID.PlayerID)
	if view.PublicState == nil {
		t.Fatal("expected network view public state")
	}
	if stringValue(view.PublicState.Phase) != string(view.ActiveHand.State.Phase) {
		t.Fatalf("expected network view phase %s, got %+v", view.ActiveHand.State.Phase, view.PublicState)
	}
	if err := guest.acceptRemoteTable(view); err != nil {
		t.Fatalf("expected guest to accept repaired network view, got %v", err)
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
		ActionDeadlineAt:     host.currentCustodyActionDeadline(table),
		ChallengeAnchor:      firstNonEmptyString(handTranscriptRoot(table), table.LastEventHash),
		DecisionIndex:        len(table.ActiveHand.State.ActionLog),
		Epoch:                table.CurrentEpoch,
		HandID:               table.ActiveHand.State.HandID,
		OptionID:             "call",
		PlayerID:             host.walletID.PlayerID,
		PrevCustodyStateHash: latestCustodyStateHash(table),
		ProfileName:          guest.profileName,
		SignedAt:             signedAt,
		TableID:              tableID,
		TurnAnchorHash:       turnAnchorHash(table),
		TranscriptRoot:       handTranscriptRoot(table),
	}
	signatureHex, err := settlementcore.SignStructuredData(guest.walletID.PrivateKeyHex, nativeActionAuthPayload(forged.TableID, forged.PlayerID, forged.HandID, forged.OptionID, forged.PrevCustodyStateHash, forged.ActionDeadlineAt, forged.ChallengeAnchor, forged.TranscriptRoot, forged.TurnAnchorHash, forged.Epoch, forged.DecisionIndex, forged.Action, forged.SignedAt))
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

	if _, err := host.SendAction(tableID, aggressiveActionForTable(t, table)); err != nil {
		t.Fatalf("host send raise: %v", err)
	}
	waitForLocalCanAct(t, []*meshRuntime{host, guest}, guest, tableID)
	if _, err := guest.SendAction(tableID, game.Action{Type: game.ActionCall}); err != nil {
		t.Fatalf("guest send preflop call: %v", err)
	}
	waitForHandPhase(t, []*meshRuntime{host, guest}, host, tableID, game.StreetFlop)
	flopTable := waitForLocalCanAct(t, []*meshRuntime{host, guest}, guest, tableID)
	if _, err := guest.SendAction(tableID, aggressiveActionForTable(t, flopTable)); err != nil {
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

func TestNativeHandMessageResponseJSONRoundTrip(t *testing.T) {
	t.Parallel()

	response := nativeHandMessageResponse{
		AcceptedTranscriptRoot: "accepted-root",
		Duplicate:              true,
		RecordKey:              "record-key",
		Table: nativeTableState{
			Config: NativeMeshTableConfig{
				ProtocolVersion: nativeProtocolVersion,
				TableID:         "table-1",
			},
		},
	}
	raw, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("marshal hand message response: %v", err)
	}
	var decoded nativeHandMessageResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal hand message response: %v", err)
	}
	if decoded.RecordKey != response.RecordKey {
		t.Fatalf("expected record key %q, got %q", response.RecordKey, decoded.RecordKey)
	}
	if decoded.AcceptedTranscriptRoot != response.AcceptedTranscriptRoot {
		t.Fatalf("expected accepted transcript root %q, got %q", response.AcceptedTranscriptRoot, decoded.AcceptedTranscriptRoot)
	}
	if !decoded.Duplicate {
		t.Fatal("expected duplicate flag after round trip")
	}
	if decoded.Table.Config.TableID != response.Table.Config.TableID {
		t.Fatalf("expected table id %q, got %q", response.Table.Config.TableID, decoded.Table.Config.TableID)
	}
}

func TestHandleHandMessageRejectsProtocolVersionMismatch(t *testing.T) {
	host, guest, _, request := prepareGuestFairnessCommitRequest(t)

	request.ProtocolVersion = "poker/v0"
	signatureHex, err := settlementcore.SignStructuredData(guest.walletID.PrivateKeyHex, nativeHandMessageAuthPayload(request))
	if err != nil {
		t.Fatalf("sign protocol-version-mismatch request: %v", err)
	}
	request.SignatureHex = signatureHex
	if _, err := host.handleHandMessageFromPeer(request); err == nil || !strings.Contains(err.Error(), "protocol version mismatch") {
		t.Fatalf("expected protocol version mismatch, got %v", err)
	}
}

func TestHandleHandMessageAcceptsExactDuplicateReplays(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(t *testing.T) (*meshRuntime, *meshRuntime, string, nativeHandMessageRequest)
	}{
		{name: "fairness commit", prepare: prepareGuestFairnessCommitRequest},
		{name: "fairness reveal", prepare: prepareGuestFairnessRevealRequest},
		{name: "private delivery share", prepare: prepareGuestPrivateDeliveryRequest},
		{name: "board share", prepare: prepareGuestBoardShareRequest},
		{name: "board open", prepare: prepareGuestBoardOpenRequest},
		{name: "showdown reveal", prepare: prepareGuestShowdownRevealRequest},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			host, _, tableID, request := tc.prepare(t)
			first, err := host.handleHandMessageFromPeer(request)
			if err != nil {
				t.Fatalf("accept initial hand message: %v", err)
			}
			if first.Duplicate {
				t.Fatal("did not expect initial hand message acceptance to be marked duplicate")
			}
			if strings.TrimSpace(first.RecordKey) == "" {
				t.Fatal("expected record key on initial hand message acceptance")
			}
			if strings.TrimSpace(first.AcceptedTranscriptRoot) == "" {
				t.Fatal("expected accepted transcript root on initial hand message acceptance")
			}
			beforeReplay := mustReadNativeTable(t, host, tableID)
			replayed, err := host.handleHandMessageFromPeer(request)
			if err != nil {
				t.Fatalf("accept duplicate hand message replay: %v", err)
			}
			if !replayed.Duplicate {
				t.Fatal("expected duplicate hand message replay to be marked duplicate")
			}
			if replayed.RecordKey != first.RecordKey {
				t.Fatalf("expected replay record key %q, got %q", first.RecordKey, replayed.RecordKey)
			}
			if replayed.AcceptedTranscriptRoot != first.AcceptedTranscriptRoot {
				t.Fatalf("expected replay accepted root %q, got %q", first.AcceptedTranscriptRoot, replayed.AcceptedTranscriptRoot)
			}
			afterReplay := mustReadNativeTable(t, host, tableID)
			if handTranscriptRoot(afterReplay) != handTranscriptRoot(beforeReplay) {
				t.Fatalf("expected duplicate replay to leave transcript root at %q, got %q", handTranscriptRoot(beforeReplay), handTranscriptRoot(afterReplay))
			}
		})
	}
}

func TestHandleHandMessageRejectsConflictingReplays(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(t *testing.T) (*meshRuntime, *meshRuntime, string, nativeHandMessageRequest)
	}{
		{name: "fairness commit", prepare: prepareGuestFairnessCommitRequest},
		{name: "fairness reveal", prepare: prepareGuestFairnessRevealRequest},
		{name: "private delivery share", prepare: prepareGuestPrivateDeliveryRequest},
		{name: "board share", prepare: prepareGuestBoardShareRequest},
		{name: "board open", prepare: prepareGuestBoardOpenRequest},
		{name: "showdown reveal", prepare: prepareGuestShowdownRevealRequest},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			host, guest, _, request := tc.prepare(t)
			first, err := host.handleHandMessageFromPeer(request)
			if err != nil {
				t.Fatalf("accept initial hand message: %v", err)
			}
			conflicting := tamperHandMessageForConflict(t, guest, request)
			conflictingKey, err := handMessageRequestRecordKey(conflicting)
			if err != nil {
				t.Fatalf("compute conflicting record key: %v", err)
			}
			if conflictingKey == first.RecordKey {
				t.Fatal("expected conflicting replay to change the record key")
			}
			if _, err := host.handleHandMessageFromPeer(conflicting); err == nil || !strings.Contains(err.Error(), "conflicts with existing transcript slot") {
				t.Fatalf("expected conflicting replay to be rejected, got %v", err)
			}
		})
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

func TestPersistAndReplicateNormalizesPublicTranscriptRoot(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)

	if err := host.store.withTableLock(tableID, func() error {
		table, err := host.store.readTable(tableID)
		if err != nil || table == nil {
			return err
		}
		if table.PublicState == nil || table.PublicState.DealerCommitment == nil {
			t.Fatalf("expected active hand dealer commitment, got %+v", table.PublicState)
		}
		table.PublicState.DealerCommitment.RootHash = "bogus-root"
		return host.persistAndReplicate(table, true)
	}); err != nil {
		t.Fatalf("persist normalized transcript root: %v", err)
	}

	hostTable := mustReadNativeTable(t, host, tableID)
	if got, want := publicTranscriptRoot(hostTable), handTranscriptRoot(hostTable); got != want {
		t.Fatalf("expected host public transcript root %q after normalization, got %q", want, got)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		guestTable := mustReadNativeTable(t, guest, tableID)
		if publicTranscriptRoot(guestTable) == handTranscriptRoot(guestTable) && publicTranscriptRoot(guestTable) != "bogus-root" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}

	guestTable := mustReadNativeTable(t, guest, tableID)
	t.Fatalf("expected guest transcript root normalization to replicate, got public=%q transcript=%q", publicTranscriptRoot(guestTable), handTranscriptRoot(guestTable))
}

func TestRefreshLocalTableIgnoresStaleHostPoll(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)

	guestTable := mustReadNativeTable(t, guest, tableID)
	if err := guest.store.withTableLock(tableID, func() error {
		table, err := guest.store.readTable(tableID)
		if err != nil || table == nil {
			return err
		}
		table.CurrentHost = nativeKnownParticipant{ProfileName: guest.profileName, Peer: guest.self.Peer}
		table.CurrentEpoch++
		if table.PublicState != nil {
			table.PublicState.Epoch = table.CurrentEpoch
		}
		if err := guest.store.writeTable(table); err != nil {
			return err
		}
		if err := guest.store.rewriteEvents(tableID, table.Events); err != nil {
			return err
		}
		return guest.store.rewriteSnapshots(tableID, table.Snapshots)
	}); err != nil {
		t.Fatalf("promote guest to current host: %v", err)
	}

	expectedEvents := 0
	if err := host.store.withTableLock(tableID, func() error {
		table, err := host.store.readTable(tableID)
		if err != nil || table == nil {
			return err
		}
		table.CurrentHost = nativeKnownParticipant{ProfileName: guest.profileName, Peer: guest.self.Peer}
		table.CurrentEpoch = guestTable.CurrentEpoch + 1
		extra := cloneJSON(table.Events[len(table.Events)-1])
		extra.Seq++
		extra.Timestamp = nowISO()
		table.Events = append(table.Events, extra)
		expectedEvents = len(table.Events)
		if err := host.store.writeTable(table); err != nil {
			return err
		}
		return host.store.rewriteEvents(tableID, table.Events)
	}); err != nil {
		t.Fatalf("seed stale host-local history: %v", err)
	}

	refreshed, err := host.refreshLocalTable(tableID)
	if err != nil {
		t.Fatalf("refresh local table with stale host poll: %v", err)
	}
	if refreshed == nil {
		t.Fatal("expected local table after stale host poll")
	}
	if got := len(refreshed.Events); got != expectedEvents {
		t.Fatalf("expected local table events to remain at %d after stale host poll, got %d", expectedEvents, got)
	}
}

func TestFailoverKeepsActiveTranscriptDrivenHandRunning(t *testing.T) {
	t.Parallel()

	host := newMeshTestRuntime(t, "host")
	player := newMeshTestRuntime(t, "player")
	witness := newMeshTestRuntime(t, "witness")

	if _, err := host.BootstrapPeer(witness.selfPeerURL(), "", nil); err != nil {
		t.Fatalf("bootstrap witness peer: %v", err)
	}
	tableID, _ := createStartedTwoPlayerTable(t, host, player, witness.selfPeerID())
	hostTable := waitForHandPhase(t, []*meshRuntime{host, player, witness}, host, tableID, game.StreetPreflop)
	var table nativeTableState
	deadline := time.Now().Add(45 * time.Second)
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

func TestAcceptRemoteTableAfterNoBlameAbortKeepsReadySnapshotAnchor(t *testing.T) {
	t.Parallel()

	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createJoinedTwoPlayerTable(t, host, guest)
	startNextHandForTest(t, host, tableID)
	waitForHandPhase(t, []*meshRuntime{host, guest}, host, tableID, game.StreetCommitment)

	beforeAbort := mustReadNativeTable(t, host, tableID)
	if beforeAbort.LatestSnapshot == nil || beforeAbort.LatestFullySignedSnapshot == nil {
		t.Fatal("expected ready snapshot before abort")
	}
	if len(beforeAbort.Snapshots) != 1 {
		t.Fatalf("expected single ready snapshot before abort, got %d", len(beforeAbort.Snapshots))
	}
	readySnapshotID := beforeAbort.LatestSnapshot.SnapshotID

	if err := host.store.withTableLock(tableID, func() error {
		table, err := host.store.readTable(tableID)
		if err != nil {
			return err
		}
		if table == nil {
			t.Fatal("expected table to abort")
		}
		if err := host.abortActiveHandLocked(table, "protocol timeout during commitment", nil); err != nil {
			return err
		}
		return host.store.writeTable(table)
	}); err != nil {
		t.Fatalf("abort active hand: %v", err)
	}

	aborted := mustReadNativeTable(t, host, tableID)
	if !tableHasEventType(aborted, "HandAbort") {
		t.Fatal("expected HandAbort event after abort")
	}
	if aborted.ActiveHand != nil {
		t.Fatalf("expected no active hand after abort, got %+v", aborted.ActiveHand)
	}
	if got := len(aborted.Snapshots); got != 1 {
		t.Fatalf("expected abort to preserve single ready snapshot, got %d", got)
	}
	if aborted.LatestSnapshot == nil || aborted.LatestFullySignedSnapshot == nil {
		t.Fatal("expected ready snapshot pointers after abort")
	}
	if aborted.LatestSnapshot.SnapshotID != readySnapshotID {
		t.Fatalf("expected latest snapshot %q after abort, got %q", readySnapshotID, aborted.LatestSnapshot.SnapshotID)
	}
	if aborted.LatestFullySignedSnapshot.SnapshotID != readySnapshotID {
		t.Fatalf("expected latest fully signed snapshot %q after abort, got %q", readySnapshotID, aborted.LatestFullySignedSnapshot.SnapshotID)
	}
	if aborted.PublicState == nil {
		t.Fatal("expected public state after abort")
	}
	if got := aborted.PublicState.HandNumber; got != 0 {
		t.Fatalf("expected ready public state hand number 0 after abort, got %d", got)
	}
	if stringValue(aborted.PublicState.HandID) != "" {
		t.Fatalf("expected ready public state hand id to be cleared, got %q", stringValue(aborted.PublicState.HandID))
	}
	if strings.TrimSpace(stringValue(aborted.PublicState.LatestEventHash)) != strings.TrimSpace(aborted.LastEventHash) {
		t.Fatalf("expected ready public state latest event hash %q, got %q", aborted.LastEventHash, stringValue(aborted.PublicState.LatestEventHash))
	}

	if err := guest.acceptRemoteTable(aborted); err != nil {
		t.Fatalf("accept aborted remote table: %v", err)
	}

	accepted := mustReadNativeTable(t, guest, tableID)
	if got := len(accepted.Snapshots); got != 1 {
		t.Fatalf("expected guest to accept single ready snapshot after abort, got %d", got)
	}
	if accepted.LatestSnapshot == nil || accepted.LatestFullySignedSnapshot == nil {
		t.Fatal("expected guest latest snapshot pointers after abort")
	}
	if accepted.LatestSnapshot.SnapshotID != readySnapshotID {
		t.Fatalf("expected guest latest snapshot %q after abort, got %q", readySnapshotID, accepted.LatestSnapshot.SnapshotID)
	}
	if accepted.LatestFullySignedSnapshot.SnapshotID != readySnapshotID {
		t.Fatalf("expected guest latest fully signed snapshot %q after abort, got %q", readySnapshotID, accepted.LatestFullySignedSnapshot.SnapshotID)
	}
}

func TestGetTableAfterPostSettledNoBlameAbortKeepsReadySnapshotAnchor(t *testing.T) {
	t.Parallel()

	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createJoinedTwoPlayerTable(t, host, guest)
	startNextHandForTest(t, host, tableID)
	waitForLocalCanAct(t, []*meshRuntime{host, guest}, host, tableID)

	current := mustReadNativeTable(t, host, tableID)
	if current.ActiveHand == nil || current.ActiveHand.State.ActingSeatIndex == nil {
		t.Fatalf("expected actionable hand before settling, got %+v", current.ActiveHand)
	}

	folder := host
	if seatPlayerID(current, *current.ActiveHand.State.ActingSeatIndex) != host.walletID.PlayerID {
		folder = guest
	}
	if _, err := folder.SendAction(tableID, game.Action{Type: game.ActionFold}); err != nil {
		t.Fatalf("fold to settle hand: %v", err)
	}

	waitForHandPhase(t, []*meshRuntime{host, guest}, host, tableID, game.StreetSettled)
	waitForHandPhase(t, []*meshRuntime{host, guest}, guest, tableID, game.StreetSettled)
	waitForCustodySync(t, []*meshRuntime{host, guest}, host, guest, tableID)

	startNextHandForTest(t, host, tableID)
	waitForHandPhase(t, []*meshRuntime{host, guest}, host, tableID, game.StreetCommitment)

	if err := host.store.withTableLock(tableID, func() error {
		table, err := host.store.readTable(tableID)
		if err != nil {
			return err
		}
		if table == nil {
			t.Fatal("expected table to abort")
		}
		if err := host.abortActiveHandLocked(table, "manual post-settled abort", nil); err != nil {
			return err
		}
		return host.persistAndReplicate(table, true)
	}); err != nil {
		t.Fatalf("replicate no-blame abort after settled hand: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		host.Tick()
		guest.Tick()
		if tableHasEventType(mustReadNativeTable(t, guest, tableID), "HandAbort") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !tableHasEventType(mustReadNativeTable(t, guest, tableID), "HandAbort") {
		t.Fatal("expected guest to accept the post-settled no-blame abort")
	}

	view, err := guest.GetTable(tableID)
	if err != nil {
		t.Fatalf("guest get table after post-settled no-blame abort: %v", err)
	}
	if view.Config.Status != "ready" {
		t.Fatalf("expected ready status after post-settled abort, got %q", view.Config.Status)
	}
	if view.PublicState == nil {
		t.Fatal("expected public state after post-settled abort")
	}
	if view.LatestSnapshot == nil {
		t.Fatal("expected latest snapshot after post-settled abort")
	}
	if stringValue(view.PublicState.Phase) != string(game.StreetSettled) {
		t.Fatalf("expected ready public state to restore the settled snapshot, got phase=%q", stringValue(view.PublicState.Phase))
	}
	if view.PublicState.HandNumber != view.LatestSnapshot.HandNumber {
		t.Fatalf("expected public state hand number %d to match latest snapshot, got %d", view.LatestSnapshot.HandNumber, view.PublicState.HandNumber)
	}
	if strings.TrimSpace(stringValue(view.PublicState.LatestEventHash)) == strings.TrimSpace(stringValue(view.LatestSnapshot.LatestEventHash)) {
		t.Fatal("expected post-settled abort to advance the public-state event hash beyond the latest snapshot anchor")
	}
}

func TestNoBlameAbortStartsFreshNextHandNumber(t *testing.T) {
	t.Parallel()

	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createJoinedTwoPlayerTable(t, host, guest)
	startNextHandForTest(t, host, tableID)
	waitForHandPhase(t, []*meshRuntime{host, guest}, host, tableID, game.StreetCommitment)

	if err := host.store.withTableLock(tableID, func() error {
		table, err := host.store.readTable(tableID)
		if err != nil {
			return err
		}
		if table == nil {
			t.Fatal("expected table to abort")
		}
		if err := host.abortActiveHandLocked(table, "protocol timeout during commitment", nil); err != nil {
			return err
		}
		return host.persistAndReplicate(table, true)
	}); err != nil {
		t.Fatalf("replicate no-blame abort: %v", err)
	}

	aborted := mustReadNativeTable(t, host, tableID)
	if aborted.NextHandAt == "" {
		t.Fatal("expected no-blame abort to schedule a follow-up hand")
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		host.Tick()
		guest.Tick()
		table := mustReadNativeTable(t, guest, tableID)
		if tableHasEventType(table, "HandAbort") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !tableHasEventType(mustReadNativeTable(t, guest, tableID), "HandAbort") {
		t.Fatal("expected guest to accept the no-blame abort before restarting the next hand")
	}

	startNextHandForTest(t, host, tableID)
	started := mustReadNativeTable(t, host, tableID)
	if started.ActiveHand == nil {
		t.Fatalf("expected active hand after no-blame abort restart, got status=%q", started.Config.Status)
	}
	if got := started.ActiveHand.State.HandNumber; got != 2 {
		t.Fatalf("expected restarted hand number 2 after aborting hand 1, got %d", got)
	}
	if countTableEventsByType(started, "HandStart") != 2 {
		t.Fatalf("expected two HandStart events after restarting post-abort, got %d", countTableEventsByType(started, "HandStart"))
	}
}

func TestStartNextHandAfterPrivateDeliveryAbort(t *testing.T) {
	t.Parallel()

	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createJoinedTwoPlayerTable(t, host, guest)
	startNextHandForTest(t, host, tableID)
	waitForHandPhase(t, []*meshRuntime{host, guest}, host, tableID, game.StreetPrivateDelivery)

	if err := host.store.withTableLock(tableID, func() error {
		table, err := host.store.readTable(tableID)
		if err != nil {
			return err
		}
		if table == nil {
			t.Fatal("expected table to abort")
		}
		offendingSeatIndex := 1
		if err := host.abortActiveHandLocked(table, "protocol timeout during private-delivery", &offendingSeatIndex); err != nil {
			return err
		}
		return host.persistAndReplicate(table, true)
	}); err != nil {
		t.Fatalf("replicate blamed abort: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		host.Tick()
		guest.Tick()
		if tableHasEventType(mustReadNativeTable(t, guest, tableID), "HandAbort") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !tableHasEventType(mustReadNativeTable(t, guest, tableID), "HandAbort") {
		t.Fatal("expected guest to accept the blamed abort before restarting the next hand")
	}

	if _, err := host.StartNextHand(tableID); err != nil {
		t.Fatalf("start next hand after private-delivery abort: %v", err)
	}

	started := mustReadNativeTable(t, host, tableID)
	if started.ActiveHand == nil {
		t.Fatalf("expected active hand after blamed abort restart, got status=%q", started.Config.Status)
	}
	if got := started.ActiveHand.State.HandNumber; got != 2 {
		t.Fatalf("expected restarted hand number 2 after aborting hand 1, got %d", got)
	}
}

func TestStartNextHandAfterPrivateDeliveryAbortSyntheticRealMode(t *testing.T) {
	t.Parallel()

	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")
	enableSyntheticRealMode(host, guest)

	tableID, _ := createJoinedTwoPlayerTable(t, host, guest)
	startNextHandForTest(t, host, tableID)
	waitForHandPhase(t, []*meshRuntime{host, guest}, host, tableID, game.StreetPrivateDelivery)

	if err := host.store.withTableLock(tableID, func() error {
		table, err := host.store.readTable(tableID)
		if err != nil {
			return err
		}
		if table == nil {
			t.Fatal("expected table to abort")
		}
		offendingSeatIndex := 1
		if err := host.abortActiveHandLocked(table, "protocol timeout during private-delivery", &offendingSeatIndex); err != nil {
			return err
		}
		return host.persistAndReplicate(table, true)
	}); err != nil {
		t.Fatalf("replicate synthetic real blamed abort: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		host.Tick()
		guest.Tick()
		if tableHasEventType(mustReadNativeTable(t, guest, tableID), "HandAbort") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !tableHasEventType(mustReadNativeTable(t, guest, tableID), "HandAbort") {
		t.Fatal("expected guest to accept the blamed abort before restarting the next hand in synthetic real mode")
	}

	if _, err := host.StartNextHand(tableID); err != nil {
		t.Fatalf("start next hand after private-delivery abort in synthetic real mode: %v", err)
	}

	started := mustReadNativeTable(t, host, tableID)
	if started.ActiveHand == nil {
		t.Fatalf("expected active hand after synthetic real blamed abort restart, got status=%q", started.Config.Status)
	}
	if got := started.ActiveHand.State.HandNumber; got != 2 {
		t.Fatalf("expected restarted hand number 2 after aborting hand 1 in synthetic real mode, got %d", got)
	}
}

func TestNoBlameAbortClearsPendingTurnMenuForReplicas(t *testing.T) {
	t.Parallel()

	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createJoinedTwoPlayerTable(t, host, guest)
	startNextHandForTest(t, host, tableID)
	waitForHandPhase(t, []*meshRuntime{host, guest}, host, tableID, game.StreetPreflop)

	deadline := time.Now().Add(3 * time.Second)
	preAbortMenuObserved := false
	for time.Now().Before(deadline) {
		host.Tick()
		guest.Tick()
		view, err := guest.GetTable(tableID)
		if err == nil && view.PendingTurnMenu != nil {
			preAbortMenuObserved = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !preAbortMenuObserved {
		t.Fatal("expected guest to observe a pending turn menu before the no-blame abort")
	}

	if err := host.store.withTableLock(tableID, func() error {
		table, err := host.store.readTable(tableID)
		if err != nil {
			return err
		}
		if table == nil {
			t.Fatal("expected table to abort")
		}
		if err := host.abortActiveHandLocked(table, "operator abort during actionable turn", nil); err != nil {
			return err
		}
		return host.persistAndReplicate(table, true)
	}); err != nil {
		t.Fatalf("replicate actionable no-blame abort: %v", err)
	}

	deadline = time.Now().Add(3 * time.Second)
	abortVisibleOnGuest := false
	for time.Now().Before(deadline) {
		host.Tick()
		guest.Tick()
		view, err := guest.GetTable(tableID)
		if err == nil && view.PendingTurnMenu == nil && view.PendingTurnChallenge == nil && tableHasEventType(mustReadNativeTable(t, guest, tableID), "HandAbort") {
			abortVisibleOnGuest = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !abortVisibleOnGuest {
		t.Fatal("expected guest GetTable to recover after no-blame abort and clear pending turn state")
	}

	if _, err := host.StartNextHand(tableID); err != nil {
		t.Fatalf("start next hand after actionable no-blame abort: %v", err)
	}
}

func TestStartNextHandPrefersExplicitNextHandTimerOverHistoricalCustodyTimestamp(t *testing.T) {
	t.Parallel()

	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createJoinedTwoPlayerTable(t, host, guest)

	var actionDeadlineAt string
	var phaseDeadlineAt string
	if err := host.store.withTableLock(tableID, func() error {
		table, err := host.store.readTable(tableID)
		if err != nil || table == nil {
			return err
		}
		staleCreatedAt := addMillis(nowISO(), -10_000)
		table.NextHandAt = addMillis(nowISO(), -1)
		table.LatestCustodyState.CreatedAt = staleCreatedAt
		if len(table.CustodyTransitions) > 0 {
			table.CustodyTransitions[len(table.CustodyTransitions)-1].NextState.CreatedAt = staleCreatedAt
		}
		return host.store.writeTable(table)
	}); err != nil {
		t.Fatalf("prepare host explicit next-hand timer: %v", err)
	}
	if err := guest.store.withTableLock(tableID, func() error {
		table, err := guest.store.readTable(tableID)
		if err != nil || table == nil {
			return err
		}
		table.NextHandAt = addMillis(nowISO(), -1)
		return guest.store.writeTable(table)
	}); err != nil {
		t.Fatalf("prepare guest explicit next-hand timer: %v", err)
	}

	if err := host.store.withTableLock(tableID, func() error {
		table, err := host.store.readTable(tableID)
		if err != nil || table == nil {
			return err
		}
		if err := host.startNextHandLocked(table); err != nil {
			return err
		}
		return host.persistAndReplicate(table, true)
	}); err != nil {
		t.Fatalf("start next hand with explicit timer: %v", err)
	}

	started := mustReadNativeTable(t, host, tableID)
	if started.ActiveHand == nil {
		t.Fatal("expected active hand after explicit next-hand restart")
	}
	if started.LatestCustodyState == nil {
		t.Fatal("expected blind-post custody state after restart")
	}
	actionDeadlineAt = started.LatestCustodyState.ActionDeadlineAt
	phaseDeadlineAt = started.ActiveHand.Cards.PhaseDeadlineAt

	if actionDeadlineAt == "" {
		t.Fatal("expected action deadline after restart")
	}
	if elapsedMillis(actionDeadlineAt) >= 0 {
		t.Fatalf("expected action deadline %q to remain in the future", actionDeadlineAt)
	}
	if phaseDeadlineAt == "" {
		t.Fatal("expected protocol deadline after restart")
	}
	if elapsedMillis(phaseDeadlineAt) >= 0 {
		t.Fatalf("expected phase deadline %q to remain in the future", phaseDeadlineAt)
	}
}

func TestStartNextHandReleasesSettledActiveHandState(t *testing.T) {
	t.Parallel()

	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createJoinedTwoPlayerTable(t, host, guest)
	startNextHandForTest(t, host, tableID)
	waitForLocalCanAct(t, []*meshRuntime{host, guest}, host, tableID)

	current := mustReadNativeTable(t, host, tableID)
	if current.ActiveHand == nil || current.ActiveHand.State.ActingSeatIndex == nil {
		t.Fatalf("expected actionable hand before settling, got %+v", current.ActiveHand)
	}

	folder := host
	if seatPlayerID(current, *current.ActiveHand.State.ActingSeatIndex) != host.walletID.PlayerID {
		folder = guest
	}
	if _, err := folder.SendAction(tableID, game.Action{Type: game.ActionFold}); err != nil {
		t.Fatalf("fold to settle hand: %v", err)
	}

	waitForHandPhase(t, []*meshRuntime{host, guest}, host, tableID, game.StreetSettled)
	waitForHandPhase(t, []*meshRuntime{host, guest}, guest, tableID, game.StreetSettled)
	waitForCustodySync(t, []*meshRuntime{host, guest}, host, guest, tableID)

	settled := mustReadNativeTable(t, host, tableID)
	if settled.ActiveHand == nil || settled.ActiveHand.State.Phase != game.StreetSettled {
		t.Fatalf("expected settled active hand state before restart, got %+v", settled.ActiveHand)
	}

	view, err := host.StartNextHand(tableID)
	if err != nil {
		t.Fatalf("start next hand after settled hand: %v", err)
	}
	if view.PublicState == nil {
		t.Fatalf("expected new active hand view after restart, got status=%q", view.Config.Status)
	}
	if stringValue(view.PublicState.Phase) == string(game.StreetSettled) {
		t.Fatalf("expected restarted public phase to leave settled state, got %+v", view.PublicState)
	}
	if view.PublicState.HandNumber != 2 {
		t.Fatalf("expected restarted hand number 2, got %d", view.PublicState.HandNumber)
	}

	started := mustReadNativeTable(t, host, tableID)
	if started.ActiveHand == nil {
		t.Fatalf("expected persisted active hand after settled restart, got status=%q", started.Config.Status)
	}
	if started.ActiveHand.State.HandNumber != 2 {
		t.Fatalf("expected persisted restarted hand number 2, got %d", started.ActiveHand.State.HandNumber)
	}
}

func TestRefreshLocalTableSyncsFromKnownParticipantsAfterHostRotation(t *testing.T) {
	t.Parallel()

	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")
	witness := newMeshTestRuntime(t, "witness")

	if _, err := host.BootstrapPeer(witness.selfPeerURL(), "", nil); err != nil {
		t.Fatalf("bootstrap witness peer: %v", err)
	}
	tableID, _ := createJoinedTwoPlayerTable(t, host, guest, witness.selfPeerID())

	staleGuest := mustReadNativeTable(t, guest, tableID)
	for _, runtime := range []*meshRuntime{host, guest, witness} {
		if err := runtime.store.withTableLock(tableID, func() error {
			table, err := runtime.store.readTable(tableID)
			if err != nil || table == nil {
				return err
			}
			table.LastHostHeartbeatAt = addMillis(nowISO(), -(runtime.hostFailureTimeoutMS() + 500))
			return runtime.persistLocalTable(table, false)
		}); err != nil {
			t.Fatalf("mark stale heartbeat for %s: %v", runtime.profileName, err)
		}
	}

	if err := witness.failoverTable(tableID, "test host rotation discovery"); err != nil {
		t.Fatalf("witness failover: %v", err)
	}

	if err := guest.persistLocalTable(&staleGuest, false); err != nil {
		t.Fatalf("restore stale guest table: %v", err)
	}

	refreshed, err := guest.refreshLocalTable(tableID)
	if err != nil {
		t.Fatalf("refresh guest table after host rotation: %v", err)
	}
	if refreshed == nil {
		t.Fatal("expected refreshed guest table after host rotation")
	}
	if refreshed.CurrentEpoch != 2 {
		t.Fatalf("expected refreshed guest epoch 2, got %d", refreshed.CurrentEpoch)
	}
	if refreshed.CurrentHost.Peer.PeerID != witness.selfPeerID() {
		t.Fatalf("expected refreshed guest host %s, got %s", witness.selfPeerID(), refreshed.CurrentHost.Peer.PeerID)
	}
	if !tableHasEventType(*refreshed, "HostRotated") {
		t.Fatal("expected refreshed guest table to include HostRotated")
	}
}

func TestOldHostAcceptsRotatedEpochFromNewHost(t *testing.T) {
	t.Parallel()

	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")
	witness := newMeshTestRuntime(t, "witness")

	if _, err := host.BootstrapPeer(witness.selfPeerURL(), "", nil); err != nil {
		t.Fatalf("bootstrap witness peer: %v", err)
	}
	tableID, _ := createStartedTwoPlayerTable(t, host, guest, witness.selfPeerID())

	staleHost := mustReadNativeTable(t, host, tableID)
	if staleHost.CurrentHost.Peer.PeerID != host.selfPeerID() {
		t.Fatalf("expected original host %s before failover, got %s", host.selfPeerID(), staleHost.CurrentHost.Peer.PeerID)
	}

	if err := witness.forceProtocolFailover(tableID, "test old host resync"); err != nil {
		t.Fatalf("force protocol failover: %v", err)
	}
	rotated := mustReadNativeTable(t, witness, tableID)
	if rotated.CurrentEpoch != staleHost.CurrentEpoch+1 {
		t.Fatalf("expected rotated epoch %d, got %d", staleHost.CurrentEpoch+1, rotated.CurrentEpoch)
	}
	if rotated.CurrentHost.Peer.PeerID != witness.selfPeerID() {
		t.Fatalf("expected witness host %s after failover, got %s", witness.selfPeerID(), rotated.CurrentHost.Peer.PeerID)
	}

	if err := host.persistLocalTable(&staleHost, false); err != nil {
		t.Fatalf("restore stale host table: %v", err)
	}
	if err := host.acceptRemoteTable(rotated); err != nil {
		t.Fatalf("accept rotated table on stale old host: %v", err)
	}

	accepted := mustReadNativeTable(t, host, tableID)
	if accepted.CurrentEpoch != rotated.CurrentEpoch {
		t.Fatalf("expected old host epoch %d after acceptance, got %d", rotated.CurrentEpoch, accepted.CurrentEpoch)
	}
	if accepted.CurrentHost.Peer.PeerID != rotated.CurrentHost.Peer.PeerID {
		t.Fatalf("expected old host to adopt new host %s, got %s", rotated.CurrentHost.Peer.PeerID, accepted.CurrentHost.Peer.PeerID)
	}
	if !tableHasEventType(accepted, "HostRotated") {
		t.Fatal("expected accepted rotated table to include HostRotated")
	}
}

func TestFailoverDuringCustodySettlement(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")
	witness := newMeshTestRuntime(t, "witness")

	if _, err := host.BootstrapPeer(witness.selfPeerURL(), "", nil); err != nil {
		t.Fatalf("bootstrap witness peer: %v", err)
	}
	enableSyntheticRealMode(host, guest, witness)
	tableID, _ := createJoinedTwoPlayerTable(t, host, guest, witness.selfPeerID())
	startNextHandForTest(t, host, tableID)
	waitForLocalCanAct(t, []*meshRuntime{host, guest}, host, tableID)

	base := mustReadNativeTable(t, host, tableID)
	if base.ActiveHand == nil || base.ActiveHand.State.ActingSeatIndex == nil {
		t.Fatalf("expected actionable hand before settlement failover test, got %+v", base.ActiveHand)
	}
	settledState, err := game.ForceFoldSeat(base.ActiveHand.State, *base.ActiveHand.State.ActingSeatIndex)
	if err != nil {
		t.Fatalf("force fold to settled hand: %v", err)
	}
	prepared := cloneJSON(base)
	prepared.ActiveHand.State = settledState
	publicState := host.publicStateFromHand(prepared, settledState)
	prepared.PublicState = &publicState
	prepared.ActiveHand.Cards.PhaseDeadlineAt = ""
	prepared.NextHandAt = ""

	for _, runtime := range []*meshRuntime{host, guest, witness} {
		cloned := cloneJSON(prepared)
		if err := runtime.persistLocalTable(&cloned, false); err != nil {
			t.Fatalf("persist prepared settled table for %s: %v", runtime.profileName, err)
		}
	}

	transition, err := host.buildCustodyTransition(prepared, tablecustody.TransitionKindShowdownPayout, &settledState, nil, nil)
	if err != nil {
		t.Fatalf("build payout transition for custody settlement failover: %v", err)
	}
	plan, err := host.buildCustodySettlementPlan(prepared, transition)
	if err != nil {
		t.Fatalf("build payout settlement plan for custody settlement failover: %v", err)
	}
	if len(plan.Inputs) == 0 {
		t.Fatal("expected failover settlement plan to consume Ark custody inputs")
	}

	batchStarted := make(chan struct{})
	releaseBatch := make(chan struct{})
	host.custodyBatchExecute = func(table nativeTableState, prevStateHash, transitionHash string, inputs []custodyInputSpec, proofSignerIDs, treeSignerIDs []string, outputs []custodyBatchOutput) (*custodyBatchResult, error) {
		select {
		case <-batchStarted:
		default:
			close(batchStarted)
		}
		<-releaseBatch
		return buildSyntheticCustodyBatchResultForTest(host, table, transitionHash, inputs, proofSignerIDs, treeSignerIDs, outputs)
	}

	finalizeDone := make(chan error, 1)
	go func() {
		table, err := host.store.readTable(tableID)
		if err != nil || table == nil {
			finalizeDone <- err
			return
		}
		pending := cloneJSON(transition)
		finalizeDone <- host.finalizeCustodyTransition(table, &pending, nil)
	}()

	select {
	case <-batchStarted:
	case err := <-finalizeDone:
		if err != nil {
			t.Fatalf("host custody finalize before batch block: %v", err)
		}
		t.Fatal("expected host custody finalize to block inside Ark batch execution")
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for host settlement batch to start")
	}

	if err := witness.store.withTableLock(tableID, func() error {
		table, err := witness.store.readTable(tableID)
		if err != nil || table == nil {
			return err
		}
		previousHost := table.CurrentHost
		table.CurrentEpoch++
		table.CurrentHost = nativeKnownParticipant{ProfileName: witness.profileName, Peer: witness.self.Peer}
		table.Config.HostPeerID = witness.selfPeerID()
		table.LastHostHeartbeatAt = nowISO()
		table.PendingTurnMenu = nil
		lease := map[string]any{
			"epoch":       table.CurrentEpoch,
			"hostPeerId":  table.CurrentHost.Peer.PeerID,
			"leaseExpiry": addMillis(nowISO(), witness.hostFailureTimeoutMS()),
			"leaseStart":  nowISO(),
			"signatures":  []map[string]any{},
			"tableId":     table.Config.TableID,
			"witnessSet":  peerIDsFromParticipants(table.Witnesses),
		}
		if err := witness.appendEvent(table, map[string]any{
			"type":               "HostRotated",
			"lease":              lease,
			"newEpoch":           table.CurrentEpoch,
			"newHostPeerId":      table.CurrentHost.Peer.PeerID,
			"previousHostPeerId": previousHost.Peer.PeerID,
		}); err != nil {
			return err
		}
		if err := witness.advanceHandProtocolLocked(table); err != nil {
			return err
		}
		return witness.persistAndReplicate(table, true)
	}); err != nil {
		t.Fatalf("apply local failover during custody settlement: %v", err)
	}
	rotated := mustReadNativeTable(t, witness, tableID)
	if rotated.CurrentHost.Peer.PeerID != witness.selfPeerID() {
		t.Fatalf("expected witness to become host during custody settlement failover, got %q", rotated.CurrentHost.Peer.PeerID)
	}
	if rotated.ActiveHand == nil || rotated.ActiveHand.State.Phase != game.StreetSettled {
		t.Fatalf("expected settled active hand to survive custody settlement failover, got %+v", rotated.ActiveHand)
	}
	if !tableHasEventType(rotated, "HostRotated") {
		t.Fatal("expected HostRotated event during custody settlement failover")
	}
	if tableHasEventType(rotated, "HandAbort") {
		t.Fatal("did not expect custody settlement failover to abort the settled hand")
	}

	close(releaseBatch)
	select {
	case err := <-finalizeDone:
		if err != nil {
			t.Fatalf("host finalize after settlement failover: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for blocked host settlement finalize to return")
	}

	afterRelease := mustReadNativeTable(t, witness, tableID)
	if afterRelease.CurrentHost.Peer.PeerID != witness.selfPeerID() {
		t.Fatalf("expected witness to remain host after settlement batch release, got %q", afterRelease.CurrentHost.Peer.PeerID)
	}
	if afterRelease.ActiveHand == nil || afterRelease.ActiveHand.State.Phase != game.StreetSettled {
		t.Fatalf("expected settled hand to remain intact after settlement batch release, got %+v", afterRelease.ActiveHand)
	}
}

func TestSyntheticRealModeMissingShowdownRevealTimeoutSettles(t *testing.T) {
	t.Parallel()

	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createStartedTwoPlayerTableInSyntheticRealMode(t, host, guest)

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

func TestActionTimeoutWaitsForCurrentActionWindowAfterStartingPhases(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)
	table := mustReadNativeTable(t, host, tableID)
	if table.ActiveHand == nil || !game.PhaseAllowsActions(table.ActiveHand.State.Phase) || table.ActiveHand.State.ActingSeatIndex == nil {
		t.Fatalf("expected actionable started hand, got %+v", table.ActiveHand)
	}
	if table.LatestCustodyState == nil {
		t.Fatal("expected latest custody state")
	}
	if table.LatestCustodyState.PublicStateHash == host.publicMoneyStateHash(table, &table.ActiveHand.State) {
		t.Fatal("expected current actionable state to outrun the latest custody checkpoint")
	}

	table.LatestCustodyState.ActionDeadlineAt = addMillis(nowISO(), -1000)

	handled, err := host.handleActionTimeoutLocked(&table)
	if err != nil {
		t.Fatalf("handle action timeout: %v", err)
	}
	if handled {
		t.Fatal("expected action timeout to wait for the current action window")
	}
	if deadline := host.currentCustodyActionDeadline(table); deadline == "" || elapsedMillis(deadline) >= 0 {
		t.Fatalf("expected a future effective action deadline, got %q", deadline)
	}
	if _, err := host.deriveTimeoutCustodyTransition(table); err == nil || !strings.Contains(err.Error(), "before the custody deadline") {
		t.Fatalf("expected timeout derivation to wait for the effective action deadline, got %v", err)
	}
}

func TestActionTransitionExtendsEffectiveActionDeadlineAfterStartingPhases(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)
	table := mustReadNativeTable(t, host, tableID)
	if table.ActiveHand == nil || !game.PhaseAllowsActions(table.ActiveHand.State.Phase) || table.ActiveHand.State.ActingSeatIndex == nil {
		t.Fatalf("expected actionable started hand, got %+v", table.ActiveHand)
	}
	if table.LatestCustodyState == nil {
		t.Fatal("expected latest custody state")
	}
	if table.LatestCustodyState.PublicStateHash == host.publicMoneyStateHash(table, &table.ActiveHand.State) {
		t.Fatal("expected current actionable state to outrun the latest custody checkpoint")
	}

	table.LatestCustodyState.ActionDeadlineAt = addMillis(nowISO(), -1000)
	currentDeadline := host.currentCustodyActionDeadline(table)
	if currentDeadline == "" || elapsedMillis(currentDeadline) >= 0 {
		t.Fatalf("expected a future effective action deadline, got %q", currentDeadline)
	}

	legalActions := game.GetLegalActions(table.ActiveHand.State, table.ActiveHand.State.ActingSeatIndex)
	if len(legalActions) == 0 {
		t.Fatal("expected legal actions")
	}
	action := game.Action{Type: legalActions[0].Type}
	if legalActions[0].MinTotalSats != nil {
		action.TotalSats = *legalActions[0].MinTotalSats
	}
	for _, candidate := range legalActions {
		if candidate.Type == game.ActionCall || candidate.Type == game.ActionCheck {
			action = game.Action{Type: candidate.Type}
			if candidate.MinTotalSats != nil {
				action.TotalSats = *candidate.MinTotalSats
			}
			break
		}
	}
	nextState, err := game.ApplyHoldemAction(table.ActiveHand.State, *table.ActiveHand.State.ActingSeatIndex, action)
	if err != nil {
		t.Fatalf("apply action: %v", err)
	}
	transition, err := host.buildCustodyTransition(table, tablecustody.TransitionKindAction, &nextState, &action, nil)
	if err != nil {
		t.Fatalf("build action transition: %v", err)
	}

	expectedDeadline := ""
	switch {
	case game.PhaseAllowsActions(nextState.Phase):
		expectedDeadline = addMillis(currentDeadline, host.actionTimeoutMSForTable(table))
	case shouldTrackProtocolDeadline(nextState.Phase):
		expectedDeadline = addMillis(currentDeadline, host.handProtocolTimeoutMSForTable(table))
	default:
		expectedDeadline = currentDeadline
	}
	if transition.NextState.ActionDeadlineAt != expectedDeadline {
		t.Fatalf("expected next action deadline %q, got %q", expectedDeadline, transition.NextState.ActionDeadlineAt)
	}
}

func TestSettlingActionCarriesCurrentCustodyDeadline(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)
	table := mustReadNativeTable(t, host, tableID)
	if table.ActiveHand == nil || !game.PhaseAllowsActions(table.ActiveHand.State.Phase) || table.ActiveHand.State.ActingSeatIndex == nil {
		t.Fatalf("expected actionable started hand, got %+v", table.ActiveHand)
	}

	currentDeadline := host.currentCustodyActionDeadline(table)
	if currentDeadline == "" {
		t.Fatal("expected current custody deadline")
	}

	action := game.Action{Type: game.ActionFold}
	nextState, err := game.ApplyHoldemAction(table.ActiveHand.State, *table.ActiveHand.State.ActingSeatIndex, action)
	if err != nil {
		t.Fatalf("apply fold action: %v", err)
	}
	if game.PhaseAllowsActions(nextState.Phase) || shouldTrackProtocolDeadline(nextState.Phase) {
		t.Fatalf("expected fold to settle the hand, got phase %s", nextState.Phase)
	}

	transition, err := host.buildCustodyTransition(table, tablecustody.TransitionKindAction, &nextState, &action, nil)
	if err != nil {
		t.Fatalf("build settling action transition: %v", err)
	}
	if transition.NextState.ActionDeadlineAt != currentDeadline {
		t.Fatalf("expected settling action deadline %q, got %q", currentDeadline, transition.NextState.ActionDeadlineAt)
	}
}

func TestBuildCustodySettlementPlanDoesNotDuplicateChangedStackRefs(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)
	table := mustReadNativeTable(t, host, tableID)
	if table.ActiveHand == nil || table.ActiveHand.State.ActingSeatIndex == nil {
		t.Fatalf("expected actionable hand, got %+v", table.ActiveHand)
	}

	legalActions := game.GetLegalActions(table.ActiveHand.State, table.ActiveHand.State.ActingSeatIndex)
	if len(legalActions) == 0 {
		t.Fatal("expected legal actions")
	}
	action := game.Action{Type: legalActions[0].Type}
	for _, candidate := range legalActions {
		if candidate.Type == game.ActionCall || candidate.Type == game.ActionCheck {
			action = game.Action{Type: candidate.Type}
			if candidate.MinTotalSats != nil {
				action.TotalSats = *candidate.MinTotalSats
			}
			break
		}
	}
	nextState, err := game.ApplyHoldemAction(table.ActiveHand.State, *table.ActiveHand.State.ActingSeatIndex, action)
	if err != nil {
		t.Fatalf("apply action: %v", err)
	}
	transition, err := host.buildCustodyTransition(table, tablecustody.TransitionKindAction, &nextState, &action, nil)
	if err != nil {
		t.Fatalf("build action transition: %v", err)
	}
	plan, err := host.buildCustodySettlementPlan(table, transition)
	if err != nil {
		t.Fatalf("build action settlement plan: %v", err)
	}

	seenRefs := map[string]int{}
	for _, input := range plan.Inputs {
		seenRefs[fundingRefKey(input.Ref)]++
	}
	for key, count := range seenRefs {
		if count != 1 {
			t.Fatalf("expected settlement plan to spend %s exactly once, got %d", key, count)
		}
	}

	actingPlayerID := seatPlayerID(table, *table.ActiveHand.State.ActingSeatIndex)
	claim, ok := latestStackClaimForPlayer(table.LatestCustodyState, actingPlayerID)
	if !ok {
		t.Fatalf("missing active stack claim for %s", actingPlayerID)
	}
	for _, ref := range claim.VTXORefs {
		if seenRefs[fundingRefKey(ref)] != 1 {
			t.Fatalf("expected prior acting stack ref %s to appear exactly once in the settlement plan", fundingRefKey(ref))
		}
	}
}

func TestBuildCustodySettlementPlanConsumesAllStackRefsForClosingActionRound(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)
	runtimes := []*meshRuntime{host, guest}

	table := waitForLocalCanAct(t, runtimes, host, tableID)
	caller := host
	bettor := guest
	if seatPlayerID(table, *table.ActiveHand.State.ActingSeatIndex) != host.walletID.PlayerID {
		table = waitForLocalCanAct(t, runtimes, guest, tableID)
		caller = guest
		bettor = host
	}
	if _, err := caller.SendAction(tableID, game.Action{Type: game.ActionCall}); err != nil {
		t.Fatalf("caller completes blind: %v", err)
	}

	table = waitForLocalCanAct(t, runtimes, bettor, tableID)
	legal := game.GetLegalActions(table.ActiveHand.State, table.ActiveHand.State.ActingSeatIndex)
	bet := game.Action{}
	for _, candidate := range legal {
		if candidate.Type != game.ActionBet && candidate.Type != game.ActionRaise {
			continue
		}
		bet = game.Action{Type: candidate.Type}
		if candidate.MinTotalSats != nil {
			bet.TotalSats = *candidate.MinTotalSats
		}
		break
	}
	if bet.Type == "" || bet.TotalSats == 0 {
		t.Fatalf("expected a betting action after the blind-completing call, got %#v", legal)
	}
	if _, err := bettor.SendAction(tableID, bet); err != nil {
		t.Fatalf("bettor opens the round: %v", err)
	}

	table = waitForLocalCanAct(t, runtimes, caller, tableID)
	call := game.Action{Type: game.ActionCall}
	nextState, err := game.ApplyHoldemAction(table.ActiveHand.State, *table.ActiveHand.State.ActingSeatIndex, call)
	if err != nil {
		t.Fatalf("apply closing call: %v", err)
	}
	transition, err := host.buildCustodyTransition(table, tablecustody.TransitionKindAction, &nextState, &call, nil)
	if err != nil {
		t.Fatalf("build closing action transition: %v", err)
	}
	plan, err := host.buildCustodySettlementPlan(table, transition)
	if err != nil {
		t.Fatalf("build closing action settlement plan: %v", err)
	}

	seenRefs := map[string]int{}
	orderedRefs := make([]string, 0, len(plan.Inputs))
	for _, input := range plan.Inputs {
		key := fundingRefKey(input.Ref)
		seenRefs[key]++
		orderedRefs = append(orderedRefs, key)
	}
	if !sort.StringsAreSorted(orderedRefs) {
		t.Fatalf("expected settlement plan inputs to be sorted by funding ref, got %v", orderedRefs)
	}
	for _, claim := range table.LatestCustodyState.StackClaims {
		for _, ref := range claim.VTXORefs {
			key := fundingRefKey(ref)
			if seenRefs[key] != 1 {
				t.Fatalf("expected current stack ref %s to appear exactly once in the settlement plan, got %d", key, seenRefs[key])
			}
		}
	}
	for _, slice := range table.LatestCustodyState.PotSlices {
		for _, ref := range slice.VTXORefs {
			key := fundingRefKey(ref)
			if seenRefs[key] != 1 {
				t.Fatalf("expected current pot ref %s to appear exactly once in the settlement plan, got %d", key, seenRefs[key])
			}
		}
	}
}

func TestRefreshPersistedSettledHandKeepsPreviousSnapshotHash(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)
	waitForLocalCanAct(t, []*meshRuntime{host, guest}, host, tableID)
	if _, err := host.SendAction(tableID, game.Action{Type: game.ActionFold}); err != nil {
		t.Fatalf("host fold to settle hand: %v", err)
	}

	table := waitForHandPhase(t, []*meshRuntime{host, guest}, host, tableID, game.StreetSettled)
	if table.PublicState == nil {
		t.Fatal("expected public state after settled hand")
	}
	previousSnapshotHash := table.PublicState.PreviousSnapshotHash
	if strings.TrimSpace(stringValue(previousSnapshotHash)) == "" {
		t.Fatal("expected settled hand to keep the previous snapshot hash")
	}

	corrupted := cloneJSON(table)
	corrupted.PublicState.Phase = string(game.StreetRiver)
	host.refreshPersistedActiveHandPublicState(&corrupted)
	if got := stringValue(corrupted.PublicState.Phase); got != string(game.StreetSettled) {
		t.Fatalf("expected settled phase to be repaired, got %q", got)
	}
	if got := stringValue(corrupted.PublicState.PreviousSnapshotHash); got != stringValue(previousSnapshotHash) {
		t.Fatalf("expected previous snapshot hash %q to be preserved, got %q", stringValue(previousSnapshotHash), got)
	}
}

func TestFirstActionApprovalUsesPreActionCustodyDeadline(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)
	table := mustReadNativeTable(t, host, tableID)
	if table.ActiveHand == nil || !game.PhaseAllowsActions(table.ActiveHand.State.Phase) || table.ActiveHand.State.ActingSeatIndex == nil {
		t.Fatalf("expected actionable started hand, got %+v", table.ActiveHand)
	}
	if table.LatestCustodyState == nil {
		t.Fatal("expected latest custody state")
	}

	table.LatestCustodyState.PublicStateHash = host.publicMoneyStateHash(table, &table.ActiveHand.State)
	table.LatestCustodyState.ActionDeadlineAt = addMillis(nowISO(), 30_000)
	hostTable := cloneJSON(table)
	guestTable := cloneJSON(table)
	if err := host.store.writeTable(&hostTable); err != nil {
		t.Fatalf("write host table: %v", err)
	}
	if err := guest.store.writeTable(&guestTable); err != nil {
		t.Fatalf("write guest table: %v", err)
	}

	legalActions := game.GetLegalActions(hostTable.ActiveHand.State, hostTable.ActiveHand.State.ActingSeatIndex)
	if len(legalActions) == 0 {
		t.Fatal("expected legal actions")
	}
	action := game.Action{Type: legalActions[0].Type}
	if legalActions[0].MinTotalSats != nil {
		action.TotalSats = *legalActions[0].MinTotalSats
	}
	for _, candidate := range legalActions {
		if candidate.Type == game.ActionCall || candidate.Type == game.ActionCheck {
			action = game.Action{Type: candidate.Type}
			if candidate.MinTotalSats != nil {
				action.TotalSats = *candidate.MinTotalSats
			}
			break
		}
	}

	actionRequest, err := host.buildSignedActionRequest(hostTable, action)
	if err != nil {
		t.Fatalf("build signed action request: %v", err)
	}
	nextState, err := game.ApplyHoldemAction(hostTable.ActiveHand.State, *hostTable.ActiveHand.State.ActingSeatIndex, actionRequest.Action)
	if err != nil {
		t.Fatalf("apply action: %v", err)
	}

	correctTransition, err := host.buildCustodyTransitionWithOverrides(hostTable, tablecustody.TransitionKindAction, &nextState, &actionRequest.Action, nil, actionRequestBindingOverrides(actionRequest))
	if err != nil {
		t.Fatalf("build correct action transition: %v", err)
	}
	correctApprovalTransition, _, err := host.normalizedCustodyApprovalTransition(hostTable, correctTransition)
	if err != nil {
		t.Fatalf("normalize correct action transition: %v", err)
	}
	if _, err := guest.handleCustodyApprovalFromPeer(nativeCustodyApprovalRequest{
		ExpectedPrevStateHash: correctApprovalTransition.PrevStateHash,
		Authorizer:            authorizerForActionRequest(actionRequest),
		PlayerID:              guest.walletID.PlayerID,
		ProtocolVersion:       nativeProtocolVersion,
		TableID:               tableID,
		Transition:            correctApprovalTransition,
	}); err != nil {
		t.Fatalf("expected first-action approval built from the pre-action table to succeed: %v", err)
	}

	wrongTable := cloneJSON(hostTable)
	wrongTable.ActiveHand.State = nextState
	wrongTransition, err := host.buildCustodyTransition(wrongTable, tablecustody.TransitionKindAction, &nextState, &actionRequest.Action, nil)
	if err != nil {
		t.Fatalf("build wrong action transition: %v", err)
	}
	wrongApprovalTransition, _, err := host.normalizedCustodyApprovalTransition(hostTable, wrongTransition)
	if err != nil {
		t.Fatalf("normalize wrong action transition: %v", err)
	}
	if correctTransition.NextState.ActionDeadlineAt == wrongTransition.NextState.ActionDeadlineAt {
		t.Fatalf("expected mutated prebuild transition to drift its action deadline, both were %q", correctTransition.NextState.ActionDeadlineAt)
	}
	if _, err := guest.handleCustodyApprovalFromPeer(nativeCustodyApprovalRequest{
		ExpectedPrevStateHash: wrongApprovalTransition.PrevStateHash,
		Authorizer:            authorizerForActionRequest(actionRequest),
		PlayerID:              guest.walletID.PlayerID,
		ProtocolVersion:       nativeProtocolVersion,
		TableID:               tableID,
		Transition:            wrongApprovalTransition,
	}); err == nil || !strings.Contains(err.Error(), "does not match the locally derived successor") {
		t.Fatalf("expected mutated first-action approval request to be rejected, got %v", err)
	}
}

func TestShowdownPayoutPlanConsumesRemovedPotRefs(t *testing.T) {
	t.Parallel()

	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createStartedTwoPlayerTableInSyntheticRealMode(t, host, guest)

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

func TestCreatedTablesSkipsLegacyProtocolReferences(t *testing.T) {
	host := newMeshTestRuntime(t, "host")

	createResult, err := host.CreateTable(map[string]any{"name": "Legacy Filter Table"})
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	tableID := stringValue(rawJSONMap(createResult["table"])["tableId"])
	if tableID == "" {
		t.Fatal("expected table id from create result")
	}

	profile, err := host.loadProfileState()
	if err != nil {
		t.Fatalf("load profile state: %v", err)
	}
	reference := profile.MeshTables[tableID]
	config, err := decodeCreatedTableConfig(reference)
	if err != nil {
		t.Fatalf("decode created table config: %v", err)
	}
	config.ProtocolVersion = "poker/v3"
	reference.Config = MustMarshalJSON(config)
	profile.MeshTables[tableID] = reference
	if err := host.profileStore.Save(*profile); err != nil {
		t.Fatalf("save mutated profile state: %v", err)
	}

	page, err := host.CreatedTables("", 10)
	if err != nil {
		t.Fatalf("created tables with legacy reference: %v", err)
	}
	if len(page.Items) != 0 {
		t.Fatalf("expected legacy created table reference to be skipped, got %+v", page.Items)
	}
}

func TestJoinTableRejectsLegacyInviteProtocolVersion(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	createResult, err := host.CreateTable(map[string]any{"name": "Legacy Invite Table"})
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	inviteCode := stringValue(createResult["inviteCode"])
	if inviteCode == "" {
		t.Fatal("expected invite code from create result")
	}

	invite, err := decodeInvite(inviteCode)
	if err != nil {
		t.Fatalf("decode invite: %v", err)
	}
	invite["protocolVersion"] = "poker/v3"
	encodedInvite, err := json.Marshal(invite)
	if err != nil {
		t.Fatalf("marshal tampered invite: %v", err)
	}
	legacyInvite := base64.RawURLEncoding.EncodeToString(encodedInvite)

	if _, err := guest.JoinTable(legacyInvite, 4_000); err == nil || !strings.Contains(err.Error(), "invite protocol version mismatch") {
		t.Fatalf("expected legacy invite to be rejected, got %v", err)
	}
}

func TestAcceptRemoteTableRejectsLegacyProtocolVersions(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createJoinedTwoPlayerTable(t, host, guest)
	base := mustReadNativeTable(t, host, tableID)

	t.Run("table config", func(t *testing.T) {
		tampered := cloneJSON(base)
		tampered.Config.ProtocolVersion = "poker/v3"
		if err := guest.acceptRemoteTable(tampered); err == nil || !strings.Contains(err.Error(), "protocol version mismatch") {
			t.Fatalf("expected legacy table config to be rejected, got %v", err)
		}
	})

	t.Run("event history", func(t *testing.T) {
		tampered := cloneJSON(base)
		tampered.Events[0].ProtocolVersion = "poker/v3"
		resignAcceptedTableEventsForTest(t, host, &tampered)
		if err := guest.acceptRemoteTable(tampered); err == nil || !strings.Contains(err.Error(), "protocol version mismatch") {
			t.Fatalf("expected legacy event protocol version to be rejected, got %v", err)
		}
	})

	t.Run("snapshot history", func(t *testing.T) {
		tampered := cloneJSON(base)
		if len(tampered.Snapshots) == 0 {
			t.Fatal("expected snapshot history to test protocol rejection")
		}
		tampered.Snapshots[0].ProtocolVersion = "poker/v3"
		if tampered.LatestSnapshot != nil && tampered.LatestSnapshot.SnapshotID == tampered.Snapshots[0].SnapshotID {
			tampered.LatestSnapshot.ProtocolVersion = "poker/v3"
		}
		if tampered.LatestFullySignedSnapshot != nil && tampered.LatestFullySignedSnapshot.SnapshotID == tampered.Snapshots[0].SnapshotID {
			tampered.LatestFullySignedSnapshot.ProtocolVersion = "poker/v3"
		}
		if err := guest.acceptRemoteTable(tampered); err == nil || !strings.Contains(err.Error(), "protocol version mismatch") {
			t.Fatalf("expected legacy snapshot protocol version to be rejected, got %v", err)
		}
	})
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

func TestAcceptRemoteTableRefreshesLocalProtocolDeadlineOnSamePhaseTranscriptProgress(t *testing.T) {
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
	if initial.ActiveHand.State.Phase != game.StreetCommitment {
		t.Fatalf("expected commitment phase, got %s", initial.ActiveHand.State.Phase)
	}
	if initial.ActiveHand.Cards.PhaseDeadlineAt == "" {
		t.Fatal("expected locally derived protocol deadline")
	}
	if err := guest.store.withTableLock(tableID, func() error {
		table, err := guest.store.readTable(tableID)
		if err != nil || table == nil {
			return err
		}
		if table.ActiveHand == nil {
			return errors.New("expected active hand while aging local deadline")
		}
		table.ActiveHand.Cards.PhaseDeadlineAt = addMillis(nowISO(), -60_000)
		return guest.store.writeTable(table)
	}); err != nil {
		t.Fatalf("age guest protocol deadline: %v", err)
	}
	stale := mustReadNativeTable(t, guest, tableID)
	staleDeadline, err := parseISOTimestamp(stale.ActiveHand.Cards.PhaseDeadlineAt)
	if err != nil {
		t.Fatalf("parse stale deadline: %v", err)
	}

	if err := host.store.withTableLock(tableID, func() error {
		table, err := host.store.readTable(tableID)
		if err != nil || table == nil {
			return err
		}
		record, err := guest.buildLocalContributionRecord(*table)
		if err != nil {
			return err
		}
		if record == nil {
			return errors.New("expected guest to have a same-phase protocol contribution")
		}
		if err := host.appendHandTranscriptRecord(table, *record); err != nil {
			return err
		}
		return host.persistLocalTable(table, false)
	}); err != nil {
		t.Fatalf("advance host transcript without replication: %v", err)
	}
	progressed := mustReadNativeTable(t, host, tableID)
	if progressed.ActiveHand == nil {
		t.Fatal("expected active hand after host protocol progress")
	}
	if progressed.ActiveHand.State.Phase != game.StreetCommitment {
		t.Fatalf("expected commitment phase after host progress, got %s", progressed.ActiveHand.State.Phase)
	}
	guestSeat := seatIndexForPlayer(t, progressed, guest.walletID.PlayerID)
	if _, ok := findTranscriptRecord(progressed.ActiveHand.Cards.Transcript, nativeHandMessageFairnessCommit, &guestSeat, string(game.StreetCommitment), nil); !ok {
		t.Fatal("expected guest fairness commit to advance the same protocol phase")
	}

	if err := guest.acceptRemoteTable(progressed); err != nil {
		t.Fatalf("accept same-phase progressed remote table: %v", err)
	}
	accepted := mustReadNativeTable(t, guest, tableID)
	if accepted.ActiveHand == nil {
		t.Fatal("expected active hand after accepting progressed table")
	}
	refreshedDeadline, err := parseISOTimestamp(accepted.ActiveHand.Cards.PhaseDeadlineAt)
	if err != nil {
		t.Fatalf("parse refreshed deadline: %v", err)
	}
	if !refreshedDeadline.After(staleDeadline) {
		t.Fatalf("expected same-phase transcript progress to refresh the local deadline, got %q after %q", accepted.ActiveHand.Cards.PhaseDeadlineAt, stale.ActiveHand.Cards.PhaseDeadlineAt)
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
		tampered.Events, tampered.LastEventHash = resignHistoricalEventsForTest(t, tampered.Events, host, guest)
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

	t.Run("protocol version tampered after signing", func(t *testing.T) {
		tampered := cloneJSON(base)
		forged := cloneJSON(*request)
		forged.ProtocolVersion = "poker/v3"
		tampered.Events[eventIndex].Body["fundsRequest"] = rawJSONMap(forged)
		resignAcceptedTableEventsForTest(t, host, &tampered)

		if err := guest.acceptRemoteTable(tampered); err == nil || !strings.Contains(err.Error(), "protocol version is invalid") {
			t.Fatalf("expected funds request protocol-version tampering to be rejected, got %v", err)
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
		baseTable := cloneJSON(tampered)
		if transitionIndex == 0 {
			baseTable.LatestCustodyState = nil
		} else {
			previous := cloneJSON(tampered.CustodyTransitions[transitionIndex-1].NextState)
			baseTable.LatestCustodyState = &previous
		}
		approvalTransition, _, err := replica.normalizedCustodyApprovalTransition(baseTable, forgedTransition)
		if err != nil {
			t.Fatalf("normalize forged funds approval transition: %v", err)
		}
		forgedTransition.Proof.RequestHash = approvalTransition.Proof.RequestHash
		forgedTransition.Approvals = nil
		for _, originalApproval := range tampered.CustodyTransitions[transitionIndex].Approvals {
			var approval tablecustody.CustodySignature
			switch originalApproval.PlayerID {
			case host.walletID.PlayerID:
				approval, err = host.localCustodyApproval(approvalTransition)
			case guest.walletID.PlayerID:
				approval, err = guest.localCustodyApproval(approvalTransition)
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

func TestFundsRequestSignatureRejectsProtocolVersionTamperedAfterSigning(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createJoinedTwoPlayerTable(t, host, guest)
	table := mustReadNativeTable(t, host, tableID)

	request, err := guest.buildSignedFundsRequest(table, "cashout")
	if err != nil {
		t.Fatalf("build signed cash-out request: %v", err)
	}
	seat, ok := seatRecordForPlayer(table, guest.walletID.PlayerID)
	if !ok {
		t.Fatal("expected guest seat")
	}
	if err := verifyNativeFundsRequestSignature(seat, request); err != nil {
		t.Fatalf("verify original signed funds request: %v", err)
	}

	forged := cloneJSON(request)
	forged.ProtocolVersion = "poker/v3"
	if err := verifyNativeFundsRequestSignature(seat, forged); err == nil || !strings.Contains(err.Error(), "signature is invalid") {
		t.Fatalf("expected protocol-version tampering to invalidate funds signature, got %v", err)
	}
}

func TestAcceptRemoteTableRejectsTamperedEmergencyExitExecutionProof(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createJoinedTwoPlayerTable(t, host, guest)
	enableSyntheticRealMode(host, guest)
	approveExpectedExitExecution := func(expectedRefs []tablecustody.VTXORef, expectedBroadcast []string, expectedSweep string) func(string, nativeFundsExitExecution) error {
		return func(profileName string, execution nativeFundsExitExecution) error {
			if !sameCanonicalVTXORefs(execution.SourceRefs, expectedRefs) {
				return errors.New("unexpected exit execution refs")
			}
			if !reflect.DeepEqual(uniqueNonEmptyStrings(execution.BroadcastTxIDs), uniqueNonEmptyStrings(expectedBroadcast)) {
				return errors.New("exit execution proof does not spend the claimed custody refs")
			}
			if strings.TrimSpace(execution.SweepTxID) != strings.TrimSpace(expectedSweep) {
				return errors.New("exit execution proof does not spend the claimed custody refs")
			}
			return nil
		}
	}
	guestTable := mustReadNativeTable(t, guest, tableID)
	expectedRefs := canonicalVTXORefs(guest.currentCustodyRefsByPlayer(guestTable)[guest.walletID.PlayerID])
	exitResult := buildSyntheticExitResultForTest(t, expectedRefs, true)
	guest.custodyExitExecute = func(profileName string, refs []tablecustody.VTXORef, destination string) (walletpkg.CustodyExitResult, error) {
		return buildSyntheticExitResultForTest(t, refs, true), nil
	}
	host.custodyExitVerify = approveExpectedExitExecution(expectedRefs, exitResult.BroadcastTxIDs, exitResult.SweepTxID)

	if _, err := guest.Exit(tableID); err != nil {
		t.Fatalf("guest emergency exit: %v", err)
	}

	base := mustReadNativeTable(t, host, tableID)
	eventIndex := findEventIndexByType(base, "EmergencyExit")
	if eventIndex < 0 {
		t.Fatal("expected EmergencyExit event")
	}
	request, hasRequest, err := fundsRequestFromEvent(base.Events[eventIndex])
	if err != nil {
		t.Fatalf("decode emergency exit request: %v", err)
	}
	if !hasRequest || request == nil || request.ExitExecution == nil {
		t.Fatal("expected emergency exit execution proof in canonical event")
	}

	baselineReplica := newMeshTestRuntime(t, "guest-replica")
	baselineReplica.walletID = guest.walletID
	if err := baselineReplica.acceptRemoteTable(base); err != nil {
		t.Fatalf("accept remote table with stored emergency exit witness: %v", err)
	}

	t.Run("tampered source refs", func(t *testing.T) {
		replica := newMeshTestRuntime(t, "guest-replica-source")
		replica.walletID = guest.walletID
		tampered := cloneJSON(base)
		forged := cloneJSON(*request)
		forged.ExitExecution.SourceRefs[0].TxID = "tampered-exit-ref"
		resignFundsRequestForTest(t, guest, &forged)
		tampered.Events[eventIndex].Body["fundsRequest"] = rawJSONMap(forged)
		resignAcceptedTableEventsForTest(t, host, &tampered)

		if err := replica.acceptRemoteTable(tampered); err == nil || !strings.Contains(err.Error(), "emergency exit execution is invalid") {
			t.Fatalf("expected tampered emergency exit refs to be rejected, got %v", err)
		}
	})

	t.Run("tampered sweep txid", func(t *testing.T) {
		replica := newMeshTestRuntime(t, "guest-replica-sweep")
		replica.walletID = guest.walletID
		tampered := cloneJSON(base)
		forged := cloneJSON(*request)
		forged.ExitExecution.SweepTxID = "tampered-sweep"
		resignFundsRequestForTest(t, guest, &forged)
		tampered.Events[eventIndex].Body["fundsRequest"] = rawJSONMap(forged)
		resignAcceptedTableEventsForTest(t, host, &tampered)

		if err := replica.acceptRemoteTable(tampered); err == nil || (!strings.Contains(err.Error(), "emergency exit execution is invalid") && !strings.Contains(err.Error(), "emergency exit witness txids mismatch")) {
			t.Fatalf("expected tampered emergency exit txid to be rejected, got %v", err)
		}
	})

	t.Run("tampered broadcast txids", func(t *testing.T) {
		replica := newMeshTestRuntime(t, "guest-replica-broadcast")
		replica.walletID = guest.walletID
		tampered := cloneJSON(base)
		forged := cloneJSON(*request)
		forged.ExitExecution.BroadcastTxIDs = []string{"forged-exit-anchor"}
		resignFundsRequestForTest(t, guest, &forged)
		tampered.Events[eventIndex].Body["fundsRequest"] = rawJSONMap(forged)
		resignAcceptedTableEventsForTest(t, host, &tampered)

		if err := replica.acceptRemoteTable(tampered); err == nil || !strings.Contains(err.Error(), "emergency exit witness txids mismatch") {
			t.Fatalf("expected unrelated emergency exit txids to be rejected, got %v", err)
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

func TestFailoverFinalizesSettledHandWithoutHandResult(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")
	witness := newMeshTestRuntime(t, "witness")

	if _, err := host.BootstrapPeer(witness.selfPeerURL(), "", nil); err != nil {
		t.Fatalf("bootstrap witness peer: %v", err)
	}
	tableID, _ := createStartedTwoPlayerTable(t, host, guest, witness.selfPeerID())

	waitForLocalCanAct(t, []*meshRuntime{host, guest, witness}, host, tableID)
	if _, err := host.SendAction(tableID, game.Action{Type: game.ActionFold}); err != nil {
		t.Fatalf("host fold to settle hand: %v", err)
	}
	settled := waitForHandPhase(t, []*meshRuntime{host, guest, witness}, host, tableID, game.StreetSettled)
	if settled.LatestSnapshot == nil || settled.LatestFullySignedSnapshot == nil {
		t.Fatal("expected settled snapshots before simulating failover recovery")
	}
	if got := stringValue(settled.LatestSnapshot.Phase); got != string(game.StreetSettled) {
		t.Fatalf("expected settled snapshot phase, got %q", got)
	}
	if len(settled.Snapshots) < 2 {
		t.Fatalf("expected settled snapshot history, got %d snapshots", len(settled.Snapshots))
	}
	if len(settled.Events) == 0 || stringValue(settled.Events[len(settled.Events)-1].Body["type"]) != "HandResult" {
		t.Fatalf("expected settled hand to end with HandResult, got %+v", settled.Events[len(settled.Events)-1])
	}

	regressed := cloneJSON(settled)
	handResult := regressed.Events[len(regressed.Events)-1]
	regressed.Events = regressed.Events[:len(regressed.Events)-1]
	regressed.LastEventHash = stringValue(handResult.PrevEventHash)
	regressed.Snapshots = regressed.Snapshots[:len(regressed.Snapshots)-1]
	previousSnapshot := cloneJSON(regressed.Snapshots[len(regressed.Snapshots)-1])
	regressed.LatestSnapshot = &previousSnapshot
	regressed.LatestFullySignedSnapshot = &previousSnapshot
	regressed.NextHandAt = ""
	if regressed.PublicState != nil {
		regressed.PublicState.LatestEventHash = regressed.LastEventHash
	}

	writeRegressedTable := func(runtime *meshRuntime) {
		t.Helper()
		table := cloneJSON(regressed)
		if err := runtime.store.writeTable(&table); err != nil {
			t.Fatalf("write regressed table for %s: %v", runtime.profileName, err)
		}
		if err := runtime.store.rewriteEvents(tableID, table.Events); err != nil {
			t.Fatalf("rewrite regressed events for %s: %v", runtime.profileName, err)
		}
		if err := runtime.store.rewriteSnapshots(tableID, table.Snapshots); err != nil {
			t.Fatalf("rewrite regressed snapshots for %s: %v", runtime.profileName, err)
		}
	}
	writeRegressedTable(host)
	writeRegressedTable(guest)
	writeRegressedTable(witness)

	if err := witness.forceProtocolFailover(tableID, "recover settled hand without HandResult"); err != nil {
		t.Fatalf("force protocol failover: %v", err)
	}

	recovered := mustReadNativeTable(t, witness, tableID)
	if recovered.CurrentHost.Peer.PeerID != witness.selfPeerID() {
		t.Fatalf("expected witness to become host, got %s", recovered.CurrentHost.Peer.PeerID)
	}
	if recovered.CurrentEpoch != regressed.CurrentEpoch+1 {
		t.Fatalf("expected epoch %d after failover, got %d", regressed.CurrentEpoch+1, recovered.CurrentEpoch)
	}
	if recovered.LatestSnapshot == nil || stringValue(recovered.LatestSnapshot.Phase) != string(game.StreetSettled) {
		t.Fatalf("expected recovered settled snapshot, got %+v", recovered.LatestSnapshot)
	}
	if recovered.LatestFullySignedSnapshot == nil || stringValue(recovered.LatestFullySignedSnapshot.Phase) != string(game.StreetSettled) {
		t.Fatalf("expected recovered fully signed settled snapshot, got %+v", recovered.LatestFullySignedSnapshot)
	}
	if recovered.NextHandAt == "" {
		t.Fatal("expected recovered settled hand to schedule the next hand")
	}
	if len(recovered.Events) < 2 {
		t.Fatalf("expected recovered events after failover, got %d", len(recovered.Events))
	}
	if got := stringValue(recovered.Events[len(recovered.Events)-1].Body["type"]); got != "HandResult" {
		t.Fatalf("expected failover recovery to append HandResult, got %q", got)
	}
	if got := stringValue(recovered.Events[len(recovered.Events)-2].Body["type"]); got != "HostRotated" {
		t.Fatalf("expected HostRotated before recovered HandResult, got %q", got)
	}
	if err := guest.acceptRemoteTable(recovered); err != nil {
		t.Fatalf("guest accepts recovered failover table: %v", err)
	}
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

func TestTwoPlayerFailoverUsesLowestNonHostSeat(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	host.self.Peer.PeerID = "peer-aaa"
	guest.self.Peer.PeerID = "peer-zzz"

	table := nativeTableState{
		CurrentHost: nativeKnownParticipant{
			ProfileName: host.profileName,
			Peer:        host.self.Peer,
		},
		Seats: []nativeSeatRecord{
			{
				NativeSeatedPlayer: NativeSeatedPlayer{
					Nickname:  "Host",
					PeerID:    host.self.Peer.PeerID,
					PlayerID:  host.walletID.PlayerID,
					SeatIndex: 0,
					Status:    "active",
				},
				PeerURL:     host.self.Peer.PeerURL,
				ProfileName: host.profileName,
			},
			{
				NativeSeatedPlayer: NativeSeatedPlayer{
					Nickname:  "Guest",
					PeerID:    guest.self.Peer.PeerID,
					PlayerID:  guest.walletID.PlayerID,
					SeatIndex: 1,
					Status:    "active",
				},
				PeerURL:     guest.self.Peer.PeerURL,
				ProfileName: guest.profileName,
			},
		},
	}

	if !guest.shouldHandleFailover(table) {
		t.Fatal("expected lowest non-host seat to handle failover")
	}
	if host.shouldHandleFailover(table) {
		t.Fatal("expected current host not to handle failover")
	}
	authorized, ok := guest.authorizedRemoteHostPeer(&table, guest.selfPeerID())
	if !ok {
		t.Fatal("expected lowest non-host seat to be authorized as rotated host")
	}
	if authorized.PeerID != guest.selfPeerID() {
		t.Fatalf("expected authorized rotated host %q, got %q", guest.selfPeerID(), authorized.PeerID)
	}
}

func TestReachableWitnessKeepsSeatOutOfFailover(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")
	witness := newMeshTestRuntime(t, "witness")

	if _, err := guest.BootstrapPeer(witness.selfPeerURL(), "", nil); err != nil {
		t.Fatalf("bootstrap witness peer: %v", err)
	}

	table := nativeTableState{
		CurrentHost: nativeKnownParticipant{
			ProfileName: host.profileName,
			Peer:        host.self.Peer,
		},
		Seats: []nativeSeatRecord{
			{
				NativeSeatedPlayer: NativeSeatedPlayer{
					Nickname:  "Host",
					PeerID:    host.self.Peer.PeerID,
					PlayerID:  host.walletID.PlayerID,
					SeatIndex: 0,
					Status:    "active",
				},
				PeerURL:     host.self.Peer.PeerURL,
				ProfileName: host.profileName,
			},
			{
				NativeSeatedPlayer: NativeSeatedPlayer{
					Nickname:  "Guest",
					PeerID:    guest.self.Peer.PeerID,
					PlayerID:  guest.walletID.PlayerID,
					SeatIndex: 1,
					Status:    "active",
				},
				PeerURL:     guest.self.Peer.PeerURL,
				ProfileName: guest.profileName,
			},
		},
		Witnesses: []nativeKnownParticipant{{
			ProfileName: witness.profileName,
			Peer:        witness.self.Peer,
		}},
	}

	if guest.shouldHandleFailover(table) {
		t.Fatal("expected reachable witness to keep the seat out of failover")
	}
	if !witness.shouldHandleFailover(table) {
		t.Fatal("expected configured witness to handle failover")
	}
}

func TestOfflineWitnessFallsBackToLowestNonHostSeat(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")
	witness := newMeshTestRuntime(t, "witness")

	if _, err := guest.BootstrapPeer(witness.selfPeerURL(), "", nil); err != nil {
		t.Fatalf("bootstrap witness peer: %v", err)
	}
	staleWitness := cloneJSON(witness.self.Peer)
	staleWitness.LastSeenAt = addMillis(nowISO(), -(nativeHostFailureMS + 100))
	if err := guest.saveKnownPeer(staleWitness); err != nil {
		t.Fatalf("age witness liveness: %v", err)
	}
	guest.peerInfoCache = map[string]nativeCachedPeerInfo{}
	if err := witness.Close(); err != nil {
		t.Fatalf("close witness runtime: %v", err)
	}

	table := nativeTableState{
		CurrentHost: nativeKnownParticipant{
			ProfileName: host.profileName,
			Peer:        host.self.Peer,
		},
		Seats: []nativeSeatRecord{
			{
				NativeSeatedPlayer: NativeSeatedPlayer{
					Nickname:  "Host",
					PeerID:    host.self.Peer.PeerID,
					PlayerID:  host.walletID.PlayerID,
					SeatIndex: 0,
					Status:    "active",
				},
				PeerURL:     host.self.Peer.PeerURL,
				ProfileName: host.profileName,
			},
			{
				NativeSeatedPlayer: NativeSeatedPlayer{
					Nickname:  "Guest",
					PeerID:    guest.self.Peer.PeerID,
					PlayerID:  guest.walletID.PlayerID,
					SeatIndex: 1,
					Status:    "active",
				},
				PeerURL:     guest.self.Peer.PeerURL,
				ProfileName: guest.profileName,
			},
		},
		Witnesses: []nativeKnownParticipant{{
			ProfileName: witness.profileName,
			Peer:        staleWitness,
		}},
	}

	if !guest.shouldHandleFailover(table) {
		t.Fatal("expected lowest non-host seat to handle failover when configured witnesses are unavailable")
	}
	authorized, ok := guest.authorizedRemoteHostPeer(&table, guest.selfPeerID())
	if !ok {
		t.Fatal("expected seat fallback host to remain authorized for accepted remote tables")
	}
	if authorized.PeerID != guest.selfPeerID() {
		t.Fatalf("expected authorized seat fallback host %q, got %q", guest.selfPeerID(), authorized.PeerID)
	}
}

func TestCompletedSeatDoesNotReclaimFailoverHost(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	host.self.Peer.PeerID = "peer-aaa"
	guest.self.Peer.PeerID = "peer-zzz"

	table := nativeTableState{
		CurrentHost: nativeKnownParticipant{
			ProfileName: guest.profileName,
			Peer:        guest.self.Peer,
		},
		Seats: []nativeSeatRecord{
			{
				NativeSeatedPlayer: NativeSeatedPlayer{
					Nickname:  "Host",
					PeerID:    host.self.Peer.PeerID,
					PlayerID:  host.walletID.PlayerID,
					SeatIndex: 0,
					Status:    "completed",
				},
				PeerURL:     host.self.Peer.PeerURL,
				ProfileName: host.profileName,
			},
			{
				NativeSeatedPlayer: NativeSeatedPlayer{
					Nickname:  "Guest",
					PeerID:    guest.self.Peer.PeerID,
					PlayerID:  guest.walletID.PlayerID,
					SeatIndex: 1,
					Status:    "active",
				},
				PeerURL:     guest.self.Peer.PeerURL,
				ProfileName: guest.profileName,
			},
		},
	}

	if host.shouldHandleFailover(table) {
		t.Fatal("expected completed seat not to reclaim hosting after cash-out")
	}
	if _, ok := lowestEligibleFailoverSeat(table); ok {
		t.Fatal("expected no non-host failover seat when the only non-host seat is completed")
	}
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

func TestJoinTableAcceptsPendingSeatLockSemanticValidationInSyntheticRealMode(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")
	enableSyntheticRealMode(host, guest)

	tableID, _ := createJoinedTwoPlayerTable(t, host, guest)
	table := mustReadNativeTable(t, guest, tableID)
	if got := len(table.CustodyTransitions); got != 2 {
		t.Fatalf("expected two seat-lock transitions after synthetic-real joins, got %d", got)
	}
	seat, ok := seatRecordForPlayer(table, guest.walletID.PlayerID)
	if !ok {
		t.Fatal("expected guest seat after synthetic-real join")
	}
	if seat.Status != "active" {
		t.Fatalf("expected guest seat to be active after synthetic-real join, got %q", seat.Status)
	}
}

func TestJoinTableSyncsExistingSeatsBeforeRemoteSignerPrepare(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	alice := newMeshTestRuntime(t, "alice")
	bob := newMeshTestRuntime(t, "bob")
	enableSyntheticRealMode(host, alice, bob)

	createResult, err := host.CreateTable(map[string]any{"name": "Three Party Join Table"})
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	inviteCode := stringValue(createResult["inviteCode"])
	if inviteCode == "" {
		t.Fatal("expected invite code from table creation")
	}
	invite, err := decodeInvite(inviteCode)
	if err != nil {
		t.Fatalf("decode invite: %v", err)
	}
	tableID := stringValue(invite["tableId"])
	if tableID == "" {
		t.Fatal("expected table id from table creation")
	}

	if _, err := alice.JoinTable(inviteCode, 4_000); err != nil {
		t.Fatalf("alice join table: %v", err)
	}
	if _, err := bob.JoinTable(inviteCode, 4_000); err != nil {
		t.Fatalf("bob join table: %v", err)
	}
	table := mustReadNativeTable(t, bob, tableID)
	if got := len(table.Seats); got != 2 {
		t.Fatalf("expected two active seats after second join, got %d", got)
	}
	for _, player := range []*meshRuntime{alice, bob} {
		seat, ok := seatRecordForPlayer(table, player.walletID.PlayerID)
		if !ok {
			t.Fatalf("expected seat for player %s after join sync", player.walletID.PlayerID)
		}
		if seat.Status != "active" {
			t.Fatalf("expected player %s seat to be active, got %q", player.walletID.PlayerID, seat.Status)
		}
	}
}

func TestCreateTableRejectsSeatCountAboveTwo(t *testing.T) {
	host := newMeshTestRuntime(t, "host")

	if _, err := host.CreateTable(map[string]any{
		"name":      "Unsupported Three Seat Table",
		"seatCount": 3,
	}); err == nil || !strings.Contains(err.Error(), "at most 2 seats") {
		t.Fatalf("expected seat-count rejection for >2 seats, got %v", err)
	}
}

func TestCustodySignerPrepareAcceptsBuyInLockWhenJoinerSeatMaterialIsStale(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	alice := newMeshTestRuntime(t, "alice")
	bob := newMeshTestRuntime(t, "bob")
	enableSyntheticRealMode(host, alice, bob)

	createResult, err := host.CreateTable(map[string]any{"name": "Stale Buy-In Lock Prepare"})
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	inviteCode := stringValue(createResult["inviteCode"])
	if inviteCode == "" {
		t.Fatal("expected invite code from table creation")
	}
	invite, err := decodeInvite(inviteCode)
	if err != nil {
		t.Fatalf("decode invite: %v", err)
	}
	tableID := stringValue(invite["tableId"])
	if tableID == "" {
		t.Fatal("expected table id from table creation")
	}

	if _, err := alice.JoinTable(inviteCode, 4_000); err != nil {
		t.Fatalf("alice join table: %v", err)
	}

	hostTable := mustReadNativeTable(t, host, tableID)
	bobJoin := mustBuildJoinRequest(t, bob, tableID, host.selfPeerURL())
	pendingJoin := cloneJSON(hostTable)
	pendingJoin.Seats = append(pendingJoin.Seats, nativeSeatRecord{
		NativeSeatedPlayer: NativeSeatedPlayer{
			ArkAddress:        bobJoin.ArkAddress,
			BuyInSats:         bobJoin.BuyInSats,
			FundingRefs:       append([]tablecustody.VTXORef(nil), bobJoin.FundingRefs...),
			Nickname:          bobJoin.Nickname,
			PeerID:            bobJoin.Peer.PeerID,
			PlayerID:          bobJoin.WalletPlayerID,
			ProtocolPubkeyHex: bobJoin.Peer.ProtocolPubkeyHex,
			SeatIndex:         len(pendingJoin.Seats),
			Status:            "active",
			WalletPubkeyHex:   bobJoin.WalletPubkeyHex,
		},
		PeerURL:     bobJoin.Peer.PeerURL,
		ProfileName: bobJoin.ProfileName,
	})
	pendingJoin.Config.OccupiedSeats = len(pendingJoin.Seats)

	transition, err := host.buildSeatLockTransition(pendingJoin)
	if err != nil {
		t.Fatalf("build pending seat-lock transition: %v", err)
	}
	signingTransition, plan, err := host.normalizedCustodyApprovalTransition(pendingJoin, transition)
	if err != nil {
		t.Fatalf("normalize pending seat-lock transition: %v", err)
	}
	transitionHash := custodyTransitionRequestHash(signingTransition)
	expectedOutputs := offchainCustodyBatchOutputs(plan.AuthorizedOutputs)
	if len(expectedOutputs) == 0 {
		t.Fatal("expected offchain outputs for buy-in-lock signer prepare")
	}

	cases := []struct {
		name   string
		mutate func(nativeTableState) nativeTableState
	}{
		{
			name: "missing-joiner-wallet-pubkey",
			mutate: func(table nativeTableState) nativeTableState {
				stale := cloneJSON(table)
				for index := range stale.Seats {
					if stale.Seats[index].PlayerID != bob.walletID.PlayerID {
						continue
					}
					stale.Seats[index].WalletPubkeyHex = ""
					stale.Seats[index].NativeSeatedPlayer.WalletPubkeyHex = ""
				}
				return stale
			},
		},
		{
			name: "missing-joiner-seat",
			mutate: func(table nativeTableState) nativeTableState {
				stale := cloneJSON(table)
				filtered := make([]nativeSeatRecord, 0, len(stale.Seats))
				for _, seat := range stale.Seats {
					if seat.PlayerID == bob.walletID.PlayerID {
						continue
					}
					filtered = append(filtered, seat)
				}
				stale.Seats = filtered
				stale.Config.OccupiedSeats = len(filtered)
				return stale
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			staleAlice := tc.mutate(pendingJoin)
			if err := alice.store.writeTable(&staleAlice); err != nil {
				t.Fatalf("write stale alice table: %v", err)
			}

			if err := alice.validatePrebuiltCustodySigningTransition(staleAlice, signingTransition.PrevStateHash, transitionHash, signingTransition, nil); err == nil {
				t.Fatal("expected generic validation to fail for stale joiner metadata")
			}

			request := nativeCustodySignerPrepareRequest{
				DerivationPath:          "test-buyin-lock-stale-seat-prepare-" + tc.name,
				ExpectedPrevStateHash:   signingTransition.PrevStateHash,
				ExpectedOffchainOutputs: expectedOutputs,
				PlayerID:                alice.walletID.PlayerID,
				ProtocolVersion:         nativeProtocolVersion,
				TableID:                 tableID,
				TransitionHash:          transitionHash,
				Transition:              signingTransition,
			}
			response, err := alice.handleCustodySignerPrepareFromPeer(request)
			if err != nil {
				t.Fatalf("prepare stale buy-in-lock signer: %v", err)
			}
			if strings.TrimSpace(response.SignerPubkeyHex) == "" {
				t.Fatal("expected signer pubkey from stale buy-in-lock prepare")
			}

			tamperedRequest := request
			tamperedRequest.DerivationPath = "test-buyin-lock-stale-seat-tampered-" + tc.name
			tamperedRequest.ExpectedOffchainOutputs = append([]custodyBatchOutput(nil), expectedOutputs...)
			tamperedRequest.ExpectedOffchainOutputs[0].AmountSats++
			if _, err := alice.handleCustodySignerPrepareFromPeer(tamperedRequest); err == nil || !strings.Contains(err.Error(), "authorized") {
				t.Fatalf("expected stale buy-in-lock prepare to reject tampered outputs, got %v", err)
			}
		})
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

func TestCashOutProofSigningAcceptsAuthorizedOutputsForActingPlayer(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")
	tableID, _ := createStartedTwoPlayerTableInSyntheticRealMode(t, host, guest)

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
	transition, err := host.buildFundsCustodyTransitionForPlayer(table, host.walletID.PlayerID, tablecustody.TransitionKindCashOut, "completed")
	if err != nil {
		t.Fatalf("build settled-hand host cash-out transition: %v", err)
	}
	fundsRequest, err := host.buildSignedFundsRequest(table, "cashout")
	if err != nil {
		t.Fatalf("build settled-hand host cash-out request: %v", err)
	}
	signingTransition, plan, err := host.normalizedCustodySigningTransition(table, transition)
	if err != nil {
		t.Fatalf("normalize settled-hand host cash-out transition: %v", err)
	}
	proofPSBT, err := buildCustodyProofPSBTForTest(plan)
	if err != nil {
		t.Fatalf("build proof psbt: %v", err)
	}
	if _, err := host.handleCustodyTxSignFromPeer(nativeCustodyTxSignRequest{
		ExpectedPrevStateHash: transition.PrevStateHash,
		ExpectedOutputs:       append([]custodyBatchOutput(nil), plan.AuthorizedOutputs...),
		Authorizer:            authorizerForFundsRequest(fundsRequest),
		PlayerID:              host.walletID.PlayerID,
		PSBT:                  proofPSBT,
		Purpose:               "proof",
		ProtocolVersion:       nativeProtocolVersion,
		TableID:               tableID,
		Transition:            signingTransition,
		TransitionHash:        custodyTransitionRequestHash(signingTransition),
	}); err != nil {
		t.Fatalf("acting player proof signer should accept authorized cash-out outputs: %v", err)
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

func TestSyntheticRealModeGuestEmergencyExitUsesLocalExecutionProof(t *testing.T) {
	t.Parallel()

	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createJoinedTwoPlayerTable(t, host, guest)
	enableSyntheticRealMode(host, guest)

	guestTable := mustReadNativeTable(t, guest, tableID)
	expectedRefs := canonicalVTXORefs(guest.currentCustodyRefsByPlayer(guestTable)[guest.walletID.PlayerID])
	if len(expectedRefs) == 0 {
		t.Fatal("expected guest custody refs before emergency exit")
	}
	exitResult := buildSyntheticExitResultForTest(t, expectedRefs, true)
	approveExpectedExitExecution := func(profileName string, execution nativeFundsExitExecution) error {
		if !sameCanonicalVTXORefs(execution.SourceRefs, expectedRefs) {
			return errors.New("unexpected exit execution refs")
		}
		if !reflect.DeepEqual(uniqueNonEmptyStrings(execution.BroadcastTxIDs), uniqueNonEmptyStrings(exitResult.BroadcastTxIDs)) {
			return errors.New("unexpected exit execution broadcast txids")
		}
		if execution.SweepTxID != exitResult.SweepTxID {
			return errors.New("unexpected exit execution sweep txid")
		}
		return nil
	}
	host.custodyExitVerify = approveExpectedExitExecution

	exitCalls := 0
	guest.custodyExitExecute = func(profileName string, refs []tablecustody.VTXORef, destination string) (walletpkg.CustodyExitResult, error) {
		exitCalls++
		if destination != "" {
			t.Fatalf("expected empty destination for unilateral exit, got %q", destination)
		}
		if !sameCanonicalVTXORefs(refs, expectedRefs) {
			t.Fatalf("expected local exit refs %+v, got %+v", expectedRefs, refs)
		}
		return buildSyntheticExitResultForTest(t, refs, true), nil
	}

	result, err := guest.Exit(tableID)
	if err != nil {
		t.Fatalf("guest emergency exit in synthetic real mode: %v", err)
	}
	if stringValue(result["status"]) != "exited" {
		t.Fatalf("expected exited status, got %+v", result)
	}
	if exitCalls != 1 {
		t.Fatalf("expected exactly one local exit execution, got %d", exitCalls)
	}

	hostTable := mustReadNativeTable(t, host, tableID)
	if !tableHasEventType(hostTable, "EmergencyExit") {
		t.Fatal("expected host to append EmergencyExit event after guest exit")
	}
	eventIndex := findEventIndexByType(hostTable, "EmergencyExit")
	if eventIndex < 0 {
		t.Fatal("expected EmergencyExit event index")
	}
	request, hasRequest, err := fundsRequestFromEvent(hostTable.Events[eventIndex])
	if err != nil {
		t.Fatalf("decode emergency exit request from event: %v", err)
	}
	if !hasRequest || request == nil || request.ExitExecution == nil {
		t.Fatal("expected canonical emergency-exit request with execution proof")
	}
	if request.Epoch != hostTable.CurrentEpoch {
		t.Fatalf("expected emergency exit request epoch %d, got %d", hostTable.CurrentEpoch, request.Epoch)
	}
	if !sameCanonicalVTXORefs(request.ExitExecution.SourceRefs, expectedRefs) {
		t.Fatalf("expected event execution refs %+v, got %+v", expectedRefs, request.ExitExecution.SourceRefs)
	}
	if request.ExitExecution.SweepTxID != exitResult.SweepTxID {
		t.Fatalf("expected sweep tx id to be preserved, got %q", request.ExitExecution.SweepTxID)
	}
	if request.ExitExecution.Pending {
		t.Fatal("expected guest emergency exit to finalize without pending status")
	}

	transition, transitionIndex, err := linkedCustodyTransitionForEvent(hostTable, hostTable.Events[eventIndex])
	if err != nil {
		t.Fatalf("link emergency exit transition: %v", err)
	}
	if len(transition.Approvals) != 1 || transition.Approvals[0].PlayerID != guest.walletID.PlayerID {
		t.Fatalf("expected only the acting guest approval, got %+v", transition.Approvals)
	}
	previousState := previousCustodyStateForTransition(hostTable, transitionIndex)
	if previousState == nil {
		t.Fatal("expected previous custody state for emergency exit")
	}
	if want := emergencyExitProofRef(*previousState, guest.walletID.PlayerID, request.ExitExecution); transition.Proof.ExitProofRef != want {
		t.Fatalf("expected exit proof ref %q, got %q", want, transition.Proof.ExitProofRef)
	}
	if transition.Proof.ExitWitness == nil {
		t.Fatal("expected emergency exit to persist an exit witness")
	}
}

func TestSyntheticRealModeGuestEmergencyExitRetriesAfterFailoverWithoutRerunningExit(t *testing.T) {
	t.Parallel()

	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createJoinedTwoPlayerTable(t, host, guest)
	enableSyntheticRealMode(host, guest)

	guestTable := mustReadNativeTable(t, guest, tableID)
	expectedRefs := canonicalVTXORefs(guest.currentCustodyRefsByPlayer(guestTable)[guest.walletID.PlayerID])
	if len(expectedRefs) == 0 {
		t.Fatal("expected guest custody refs before emergency exit")
	}
	exitResult := buildSyntheticExitResultForTest(t, expectedRefs, true)
	approveExpectedExitExecution := func(profileName string, execution nativeFundsExitExecution) error {
		if !sameCanonicalVTXORefs(execution.SourceRefs, expectedRefs) {
			return errors.New("unexpected exit execution refs")
		}
		if !reflect.DeepEqual(uniqueNonEmptyStrings(execution.BroadcastTxIDs), uniqueNonEmptyStrings(exitResult.BroadcastTxIDs)) {
			return errors.New("unexpected exit execution broadcast txids")
		}
		if execution.SweepTxID != exitResult.SweepTxID {
			return errors.New("unexpected exit execution sweep txid")
		}
		return nil
	}
	host.custodyExitVerify = approveExpectedExitExecution
	guest.custodyExitVerify = approveExpectedExitExecution

	exitCalls := 0
	guest.custodyExitExecute = func(profileName string, refs []tablecustody.VTXORef, destination string) (walletpkg.CustodyExitResult, error) {
		exitCalls++
		return buildSyntheticExitResultForTest(t, refs, true), nil
	}

	firstSubmission := true
	guest.fundsSenderHook = func(peerURL string, input nativeFundsRequest) (nativeFundsResponse, error) {
		if firstSubmission {
			firstSubmission = false
			return nativeFundsResponse{}, errors.New("simulated host submission failure")
		}
		return nativeFundsResponse{}, errors.New("unexpected remote submission after local failover")
	}

	result, err := guest.Exit(tableID)
	if err != nil {
		t.Fatalf("guest emergency exit with simulated submission failure: %v", err)
	}
	if stringValue(result["status"]) != "pending-exit" {
		t.Fatalf("expected pending exit result after submission failure, got %+v", result)
	}
	if exitCalls != 1 {
		t.Fatalf("expected one local exit execution before retry, got %d", exitCalls)
	}

	pendingOperation, pendingRequest, err := guest.pendingEmergencyExitOperation(tableID)
	if err != nil {
		t.Fatalf("read pending emergency exit operation: %v", err)
	}
	if pendingOperation == nil || pendingRequest == nil {
		t.Fatal("expected pending emergency exit request to be persisted")
	}
	savedRequest := cloneJSON(*pendingRequest)
	if !sameCanonicalVTXORefs(savedRequest.ExitExecution.SourceRefs, expectedRefs) {
		t.Fatalf("expected persisted exit refs %+v, got %+v", expectedRefs, savedRequest.ExitExecution.SourceRefs)
	}

	if err := guest.store.withTableLock(tableID, func() error {
		table, err := guest.store.readTable(tableID)
		if err != nil || table == nil {
			return err
		}
		table.CurrentHost = nativeKnownParticipant{ProfileName: guest.profileName, Peer: guest.self.Peer}
		table.CurrentEpoch++
		table.LastHostHeartbeatAt = nowISO()
		if table.PublicState != nil {
			table.PublicState.Epoch = table.CurrentEpoch
		}
		if err := guest.store.writeTable(table); err != nil {
			return err
		}
		if err := guest.store.rewriteEvents(tableID, table.Events); err != nil {
			return err
		}
		return guest.store.rewriteSnapshots(tableID, table.Snapshots)
	}); err != nil {
		t.Fatalf("promote guest to successor host: %v", err)
	}

	guest.Tick()

	if exitCalls != 1 {
		t.Fatalf("expected failover retry to reuse existing execution proof, got %d exit executions", exitCalls)
	}
	if _, request, err := guest.pendingEmergencyExitOperation(tableID); err != nil {
		t.Fatalf("read pending emergency exit after retry: %v", err)
	} else if request != nil {
		t.Fatal("expected pending emergency exit receipt to clear after retry succeeds")
	}

	acceptedTable := mustReadNativeTable(t, guest, tableID)
	if !tableHasEventType(acceptedTable, "EmergencyExit") {
		t.Fatal("expected successor host to append EmergencyExit event on retry")
	}
	eventIndex := findEventIndexByType(acceptedTable, "EmergencyExit")
	request, hasRequest, err := fundsRequestFromEvent(acceptedTable.Events[eventIndex])
	if err != nil {
		t.Fatalf("decode retried emergency exit request from event: %v", err)
	}
	if !hasRequest || request == nil || request.ExitExecution == nil {
		t.Fatal("expected accepted emergency exit request after failover retry")
	}
	if request.Epoch != acceptedTable.CurrentEpoch {
		t.Fatalf("expected retried request to re-sign for epoch %d, got %d", acceptedTable.CurrentEpoch, request.Epoch)
	}
	if request.SignatureHex == savedRequest.SignatureHex {
		t.Fatal("expected failover retry to re-sign the pending emergency exit request")
	}
	if !sameCanonicalVTXORefs(request.ExitExecution.SourceRefs, savedRequest.ExitExecution.SourceRefs) {
		t.Fatalf("expected failover retry to preserve source refs %+v, got %+v", savedRequest.ExitExecution.SourceRefs, request.ExitExecution.SourceRefs)
	}
	if request.ExitExecution.SweepTxID != savedRequest.ExitExecution.SweepTxID {
		t.Fatalf("expected failover retry to preserve sweep txid %q, got %q", savedRequest.ExitExecution.SweepTxID, request.ExitExecution.SweepTxID)
	}
}

func TestSyntheticRealModeGuestEmergencyExitResignsPendingRequestWhenCustodyHashAdvances(t *testing.T) {
	t.Parallel()

	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createJoinedTwoPlayerTable(t, host, guest)
	enableSyntheticRealMode(host, guest)

	guestTable := mustReadNativeTable(t, guest, tableID)
	expectedRefs := canonicalVTXORefs(guest.currentCustodyRefsByPlayer(guestTable)[guest.walletID.PlayerID])
	if len(expectedRefs) == 0 {
		t.Fatal("expected guest custody refs before emergency exit")
	}
	exitResult := buildSyntheticExitResultForTest(t, expectedRefs, true)
	approveExpectedExitExecution := func(profileName string, execution nativeFundsExitExecution) error {
		if !sameCanonicalVTXORefs(execution.SourceRefs, expectedRefs) {
			return errors.New("unexpected exit execution refs")
		}
		if !reflect.DeepEqual(uniqueNonEmptyStrings(execution.BroadcastTxIDs), uniqueNonEmptyStrings(exitResult.BroadcastTxIDs)) {
			return errors.New("unexpected exit execution broadcast txids")
		}
		if execution.SweepTxID != exitResult.SweepTxID {
			return errors.New("unexpected exit execution sweep txid")
		}
		return nil
	}
	guest.custodyExitVerify = approveExpectedExitExecution

	exitCalls := 0
	guest.custodyExitExecute = func(profileName string, refs []tablecustody.VTXORef, destination string) (walletpkg.CustodyExitResult, error) {
		exitCalls++
		return buildSyntheticExitResultForTest(t, refs, true), nil
	}

	firstSubmission := true
	guest.fundsSenderHook = func(peerURL string, input nativeFundsRequest) (nativeFundsResponse, error) {
		if firstSubmission {
			firstSubmission = false
			return nativeFundsResponse{}, errors.New("simulated host submission failure")
		}
		return nativeFundsResponse{}, errors.New("unexpected remote submission after local retry")
	}

	result, err := guest.Exit(tableID)
	if err != nil {
		t.Fatalf("guest emergency exit with simulated submission failure: %v", err)
	}
	if stringValue(result["status"]) != "pending-exit" {
		t.Fatalf("expected pending exit result after submission failure, got %+v", result)
	}
	if exitCalls != 1 {
		t.Fatalf("expected one local exit execution before retry, got %d", exitCalls)
	}

	pendingOperation, pendingRequest, err := guest.pendingEmergencyExitOperation(tableID)
	if err != nil {
		t.Fatalf("read pending emergency exit operation: %v", err)
	}
	if pendingOperation == nil || pendingRequest == nil {
		t.Fatal("expected pending emergency exit request to be persisted")
	}
	savedRequest := cloneJSON(*pendingRequest)

	const advancedStateHash = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if err := guest.store.withTableLock(tableID, func() error {
		table, err := guest.store.readTable(tableID)
		if err != nil || table == nil {
			return err
		}
		if table.LatestCustodyState == nil {
			return errors.New("expected latest custody state before stale pending exit replay")
		}
		advanced := cloneJSON(*table.LatestCustodyState)
		advanced.StateHash = advancedStateHash
		advanced.CreatedAt = nowISO()
		table.LatestCustodyState = &advanced
		table.CurrentHost = nativeKnownParticipant{ProfileName: guest.profileName, Peer: guest.self.Peer}
		table.LastHostHeartbeatAt = nowISO()
		return guest.store.writeTable(table)
	}); err != nil {
		t.Fatalf("advance guest custody hash before retry: %v", err)
	}
	guest.tableSyncSender = func(peerURL string, input nativeTableSyncRequest) error {
		return nil
	}

	result, err = guest.Exit(tableID)
	if err != nil {
		t.Fatalf("guest emergency exit after custody-hash advancement: %v", err)
	}
	if stringValue(result["status"]) != "exited" {
		t.Fatalf("expected exited status after stale pending replay, got %+v", result)
	}
	if exitCalls != 1 {
		t.Fatalf("expected retry to reuse existing execution proof, got %d exit executions", exitCalls)
	}
	if _, request, err := guest.pendingEmergencyExitOperation(tableID); err != nil {
		t.Fatalf("read pending emergency exit after custody-hash retry: %v", err)
	} else if request != nil {
		t.Fatal("expected pending emergency exit receipt to clear after custody-hash retry succeeds")
	}

	acceptedTable := mustReadNativeTable(t, guest, tableID)
	if !tableHasEventType(acceptedTable, "EmergencyExit") {
		t.Fatal("expected local host retry to append EmergencyExit event")
	}
	eventIndex := findEventIndexByType(acceptedTable, "EmergencyExit")
	request, hasRequest, err := fundsRequestFromEvent(acceptedTable.Events[eventIndex])
	if err != nil {
		t.Fatalf("decode retried emergency exit request from event: %v", err)
	}
	if !hasRequest || request == nil || request.ExitExecution == nil {
		t.Fatal("expected accepted emergency exit request after custody-hash retry")
	}
	if request.PrevCustodyStateHash != advancedStateHash {
		t.Fatalf("expected retried request to re-sign for custody hash %q, got %q", advancedStateHash, request.PrevCustodyStateHash)
	}
	if request.PrevCustodyStateHash == savedRequest.PrevCustodyStateHash {
		t.Fatal("expected retried request to replace the stale custody hash")
	}
	if request.Epoch != savedRequest.Epoch {
		t.Fatalf("expected custody-hash retry to preserve epoch %d, got %d", savedRequest.Epoch, request.Epoch)
	}
	if request.SignatureHex == savedRequest.SignatureHex {
		t.Fatal("expected custody-hash retry to re-sign the pending emergency exit request")
	}
	if !sameCanonicalVTXORefs(request.ExitExecution.SourceRefs, savedRequest.ExitExecution.SourceRefs) {
		t.Fatalf("expected custody-hash retry to preserve source refs %+v, got %+v", savedRequest.ExitExecution.SourceRefs, request.ExitExecution.SourceRefs)
	}
	if request.ExitExecution.SweepTxID != savedRequest.ExitExecution.SweepTxID {
		t.Fatalf("expected custody-hash retry to preserve sweep txid %q, got %q", savedRequest.ExitExecution.SweepTxID, request.ExitExecution.SweepTxID)
	}
}

func TestGuestCashOutCancelsPendingNextHandStart(t *testing.T) {
	t.Parallel()

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
	t.Parallel()

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
	t.Parallel()

	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createStartedTwoPlayerTableInSyntheticRealMode(t, host, guest)

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

func TestFundsRequestRejectedWhileSettledHandPayoutIsDeferred(t *testing.T) {
	t.Parallel()

	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createStartedTwoPlayerTableInSyntheticRealMode(t, host, guest)

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
			t.Fatalf("send showdown-line check %d: %v", index, err)
		}
	}
	waitForHandPhase(t, []*meshRuntime{host, guest}, host, tableID, game.StreetSettled)
	waitForHandPhase(t, []*meshRuntime{host, guest}, guest, tableID, game.StreetSettled)
	waitForCustodySync(t, []*meshRuntime{host, guest}, host, guest, tableID)

	settled := mustReadNativeTable(t, host, tableID)
	if len(settled.CustodyTransitions) < 2 {
		t.Fatalf("expected settled hand custody history, got %d transitions", len(settled.CustodyTransitions))
	}
	if settled.CustodyTransitions[len(settled.CustodyTransitions)-1].Kind != tablecustody.TransitionKindShowdownPayout {
		t.Fatalf("expected showdown payout transition, got %s", settled.CustodyTransitions[len(settled.CustodyTransitions)-1].Kind)
	}
	stale := cloneJSON(settled)
	previousState := cloneJSON(settled.CustodyTransitions[len(settled.CustodyTransitions)-2].NextState)
	stale.LatestCustodyState = &previousState

	wallet, err := guest.walletSummary()
	if err != nil {
		t.Fatalf("guest wallet summary: %v", err)
	}
	request := nativeFundsRequest{
		ArkAddress:           wallet.ArkAddress,
		Epoch:                stale.CurrentEpoch,
		Kind:                 "cashout",
		PlayerID:             guest.walletID.PlayerID,
		PrevCustodyStateHash: latestCustodyStateHash(stale),
		ProfileName:          guest.profileName,
		ProtocolVersion:      nativeProtocolVersion,
		SignedAt:             nowISO(),
		TableID:              stale.Config.TableID,
	}
	resignFundsRequestForTest(t, guest, &request)
	if _, err := guest.buildSignedFundsRequest(stale, "cashout"); err == nil || !strings.Contains(err.Error(), "settled hand payout") {
		t.Fatalf("expected deferred settled-hand payout to block local funds request creation, got %v", err)
	}
	staleApply := cloneJSON(stale)
	if _, err := host.applyFundsRequestLocked(&staleApply, request); err == nil || !strings.Contains(err.Error(), "settled hand payout") {
		t.Fatalf("expected deferred settled-hand payout to block funds application, got %v", err)
	}
}

func TestGuestCashOutRefreshesRemoteCustodyAfterPeerCashOut(t *testing.T) {
	t.Parallel()

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

func TestHostCashOutAfterPeerSettledHandCashOutAcceptsRemoteHistory(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)

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
			t.Fatalf("send showdown-line check %d: %v", index, err)
		}
	}
	waitForHandPhase(t, []*meshRuntime{host, guest}, host, tableID, game.StreetSettled)
	waitForHandPhase(t, []*meshRuntime{host, guest}, guest, tableID, game.StreetSettled)
	waitForCustodySync(t, []*meshRuntime{host, guest}, host, guest, tableID)

	promoteGuestHost := func(runtime *meshRuntime) error {
		return runtime.store.withTableLock(tableID, func() error {
			table, err := runtime.store.readTable(tableID)
			if err != nil || table == nil {
				return err
			}
			table.CurrentHost = nativeKnownParticipant{ProfileName: guest.profileName, Peer: guest.self.Peer}
			table.CurrentEpoch++
			if table.PublicState != nil {
				table.PublicState.Epoch = table.CurrentEpoch
			}
			if err := runtime.store.writeTable(table); err != nil {
				return err
			}
			if err := runtime.store.rewriteEvents(tableID, table.Events); err != nil {
				return err
			}
			return runtime.store.rewriteSnapshots(tableID, table.Snapshots)
		})
	}
	if err := promoteGuestHost(host); err != nil {
		t.Fatalf("promote guest to host on host runtime: %v", err)
	}
	if err := promoteGuestHost(guest); err != nil {
		t.Fatalf("promote guest to host on guest runtime: %v", err)
	}

	if _, err := guest.CashOut(tableID); err != nil {
		t.Fatalf("guest cash out after settled hand: %v", err)
	}
	if _, err := host.CashOut(tableID); err != nil {
		t.Fatalf("host cash out after peer settled-hand cash-out: %v", err)
	}

	hostTable := mustReadNativeTable(t, host, tableID)
	if latestStackAmount(hostTable.LatestCustodyState, guest.walletID.PlayerID) != 0 {
		t.Fatalf("expected guest stack to be zero after first cash-out, got %d", latestStackAmount(hostTable.LatestCustodyState, guest.walletID.PlayerID))
	}
	if latestStackAmount(hostTable.LatestCustodyState, host.walletID.PlayerID) != 0 {
		t.Fatalf("expected host stack to be zero after second cash-out, got %d", latestStackAmount(hostTable.LatestCustodyState, host.walletID.PlayerID))
	}
}

func TestCashOutRetriesRemoteCustodyMismatchAfterAuthoritativeRefresh(t *testing.T) {
	t.Parallel()

	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createJoinedTwoPlayerTable(t, host, guest)

	promoteGuestHost := func(runtime *meshRuntime) error {
		return runtime.store.withTableLock(tableID, func() error {
			table, err := runtime.store.readTable(tableID)
			if err != nil || table == nil {
				return err
			}
			table.CurrentHost = nativeKnownParticipant{ProfileName: guest.profileName, Peer: guest.self.Peer}
			table.CurrentEpoch++
			if table.PublicState != nil {
				table.PublicState.Epoch = table.CurrentEpoch
			}
			if err := runtime.store.writeTable(table); err != nil {
				return err
			}
			if err := runtime.store.rewriteEvents(tableID, table.Events); err != nil {
				return err
			}
			return runtime.store.rewriteSnapshots(tableID, table.Snapshots)
		})
	}
	if err := promoteGuestHost(host); err != nil {
		t.Fatalf("promote guest to host on host runtime: %v", err)
	}
	if err := promoteGuestHost(guest); err != nil {
		t.Fatalf("promote guest to host on guest runtime: %v", err)
	}

	firstCall := true
	host.fundsSenderHook = func(peerURL string, input nativeFundsRequest) (nativeFundsResponse, error) {
		if firstCall {
			firstCall = false
			if _, err := guest.CashOut(tableID); err != nil {
				t.Fatalf("guest cash out during forced remote custody race: %v", err)
			}
			return nativeFundsResponse{}, errors.New("funds request custody mismatch")
		}
		return guest.handleFundsFromPeer(input)
	}

	if _, err := host.CashOut(tableID); err != nil {
		t.Fatalf("host cash out after retryable custody mismatch: %v", err)
	}

	hostTable := mustReadNativeTable(t, host, tableID)
	if latestStackAmount(hostTable.LatestCustodyState, guest.walletID.PlayerID) != 0 {
		t.Fatalf("expected guest stack to be zero after guest cash-out refresh, got %d", latestStackAmount(hostTable.LatestCustodyState, guest.walletID.PlayerID))
	}
	if latestStackAmount(hostTable.LatestCustodyState, host.walletID.PlayerID) != 0 {
		t.Fatalf("expected host stack to be zero after retrying host cash-out, got %d", latestStackAmount(hostTable.LatestCustodyState, host.walletID.PlayerID))
	}
}

func TestSyntheticRealModeSettledHandCashOutProofRefsCoverRemainingClaims(t *testing.T) {
	t.Parallel()

	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createStartedTwoPlayerTableInSyntheticRealMode(t, host, guest)

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
	result, _, _, err := host.settleTableFundsForPlayer(table, transition, authorizerForFundsRequest(fundsRequest), guest.walletID.PlayerID)
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
	t.Parallel()

	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createStartedTwoPlayerTableInSyntheticRealMode(t, host, guest)

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
		ProtocolVersion:         nativeProtocolVersion,
		TableID:                 table.Config.TableID,
		TransitionHash:          custodyTransitionRequestHash(signingTransition),
		Transition:              signingTransition,
	}); err != nil {
		t.Fatalf("prepare normalized settled-hand guest cash-out signer: %v", err)
	}
}

func TestSettledHandCashOutKeepsUntouchedAllInRefReusable(t *testing.T) {
	t.Parallel()

	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createStartedTwoPlayerTableInSyntheticRealMode(t, host, guest)

	table := waitForLocalCanAct(t, []*meshRuntime{host, guest}, host, tableID)
	allInActor := host
	allInCaller := guest
	if seatPlayerID(table, *table.ActiveHand.State.ActingSeatIndex) == guest.walletID.PlayerID {
		allInActor = guest
		allInCaller = host
	}
	if _, err := allInActor.SendAction(tableID, allInActionForTable(t, mustReadNativeTable(t, allInActor, tableID))); err != nil {
		t.Fatalf("send all-in action: %v", err)
	}
	waitForLocalCanAct(t, []*meshRuntime{host, guest}, allInCaller, tableID)
	if _, err := allInCaller.SendAction(tableID, passiveActionForTable(t, mustReadNativeTable(t, allInCaller, tableID))); err != nil {
		t.Fatalf("call all-in action: %v", err)
	}
	waitForHandPhase(t, []*meshRuntime{host, guest}, host, tableID, game.StreetSettled)
	waitForHandPhase(t, []*meshRuntime{host, guest}, guest, tableID, game.StreetSettled)
	waitForCustodySync(t, []*meshRuntime{host, guest}, host, guest, tableID)

	table = mustReadNativeTable(t, host, tableID)
	var winnerID string
	var loserID string
	for _, claim := range table.LatestCustodyState.StackClaims {
		if claim.AmountSats > 0 {
			winnerID = claim.PlayerID
			continue
		}
		loserID = claim.PlayerID
	}
	if winnerID == "" || loserID == "" {
		t.Fatalf("expected winner and loser claims after all-in settlement, got %+v", table.LatestCustodyState.StackClaims)
	}
	loserClaim, ok := latestStackClaimForPlayer(table.LatestCustodyState, loserID)
	if !ok || len(loserClaim.VTXORefs) != 1 {
		t.Fatalf("expected untouched loser claim ref, got %+v", loserClaim)
	}

	transition, err := host.buildFundsCustodyTransitionForPlayer(table, winnerID, tablecustody.TransitionKindCashOut, "completed")
	if err != nil {
		t.Fatalf("build winner cash-out transition: %v", err)
	}
	normalized, plan, err := host.normalizedCustodySigningTransition(table, transition)
	if err != nil {
		t.Fatalf("normalize winner cash-out transition: %v", err)
	}
	if len(plan.Outputs) != 0 {
		t.Fatalf("expected untouched loser claim to be reused instead of reminted, got plan outputs %+v", plan.Outputs)
	}
	reusedLoser, ok := latestStackClaimForPlayer(&normalized.NextState, loserID)
	if !ok || len(reusedLoser.VTXORefs) != 1 || fundingRefKey(reusedLoser.VTXORefs[0]) != fundingRefKey(loserClaim.VTXORefs[0]) {
		t.Fatalf("expected normalized cash-out transition to preserve untouched loser ref, got %+v want %+v", reusedLoser.VTXORefs, loserClaim.VTXORefs)
	}
	if reusedLoser.Status != string(game.PlayerStatusAllIn) {
		t.Fatalf("expected untouched loser status to remain all-in during cash-out replay, got %q", reusedLoser.Status)
	}
}

func TestSyntheticRealModeWinnerCashOutAfterSettledAllInHand(t *testing.T) {
	t.Parallel()

	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createStartedTwoPlayerTableInSyntheticRealMode(t, host, guest)

	table := waitForLocalCanAct(t, []*meshRuntime{host, guest}, host, tableID)
	allInActor := host
	allInCaller := guest
	if seatPlayerID(table, *table.ActiveHand.State.ActingSeatIndex) == guest.walletID.PlayerID {
		allInActor = guest
		allInCaller = host
	}
	if _, err := allInActor.SendAction(tableID, allInActionForTable(t, mustReadNativeTable(t, allInActor, tableID))); err != nil {
		t.Fatalf("send all-in action in synthetic real mode: %v", err)
	}
	waitForLocalCanAct(t, []*meshRuntime{host, guest}, allInCaller, tableID)
	if _, err := allInCaller.SendAction(tableID, passiveActionForTable(t, mustReadNativeTable(t, allInCaller, tableID))); err != nil {
		t.Fatalf("call all-in action in synthetic real mode: %v", err)
	}
	waitForHandPhase(t, []*meshRuntime{host, guest}, host, tableID, game.StreetSettled)
	waitForHandPhase(t, []*meshRuntime{host, guest}, guest, tableID, game.StreetSettled)
	waitForCustodySync(t, []*meshRuntime{host, guest}, host, guest, tableID)

	table = mustReadNativeTable(t, host, tableID)
	winnerID := ""
	for _, claim := range table.LatestCustodyState.StackClaims {
		if claim.AmountSats > 0 {
			winnerID = claim.PlayerID
			break
		}
	}
	if winnerID == "" {
		t.Fatalf("expected a winner after the all-in hand, got %+v", table.LatestCustodyState.StackClaims)
	}

	cashoutRuntime := host
	if winnerID == guest.walletID.PlayerID {
		cashoutRuntime = guest
	}
	if _, err := cashoutRuntime.CashOut(tableID); err != nil {
		t.Fatalf("winner cash out after settled all-in synthetic real hand: %v", err)
	}

	hostTable := waitForLatestCustodyTransitionKind(t, []*meshRuntime{host, guest}, host, tableID, tablecustody.TransitionKindCashOut)
	guestTable := waitForLatestCustodyTransitionKind(t, []*meshRuntime{host, guest}, guest, tableID, tablecustody.TransitionKindCashOut)
	if latestStackAmount(hostTable.LatestCustodyState, winnerID) != 0 {
		t.Fatalf("expected winner stack to be zero after host accepted all-in cash-out, got %d", latestStackAmount(hostTable.LatestCustodyState, winnerID))
	}
	if latestStackAmount(guestTable.LatestCustodyState, winnerID) != 0 {
		t.Fatalf("expected winner stack to be zero after guest accepted all-in cash-out, got %d", latestStackAmount(guestTable.LatestCustodyState, winnerID))
	}
}

func TestCustodyPeerValidationIgnoresProofFieldPollution(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)
	enableSyntheticRealMode(host, guest)

	waitForLocalCanAct(t, []*meshRuntime{host, guest}, host, tableID)
	waitForActionableHandState(t, []*meshRuntime{host, guest}, guest, tableID)
	table := waitForLocalCanAct(t, []*meshRuntime{host, guest}, host, tableID)

	actionRequest, err := host.buildSignedActionRequest(table, game.Action{Type: game.ActionCall})
	if err != nil {
		t.Fatalf("build signed action request: %v", err)
	}
	nextState, err := game.ApplyHoldemAction(table.ActiveHand.State, *table.ActiveHand.State.ActingSeatIndex, actionRequest.Action)
	if err != nil {
		t.Fatalf("apply action request: %v", err)
	}
	transition, err := host.buildCustodyTransitionWithOverrides(table, tablecustody.TransitionKindAction, &nextState, &actionRequest.Action, nil, actionRequestBindingOverrides(actionRequest))
	if err != nil {
		t.Fatalf("build action transition: %v", err)
	}
	approvalTransition, _, err := host.normalizedCustodyApprovalTransition(table, transition)
	if err != nil {
		t.Fatalf("normalize approval transition: %v", err)
	}
	signingTransition, plan, err := host.normalizedCustodySigningTransition(table, transition)
	if err != nil {
		t.Fatalf("normalize signing transition: %v", err)
	}
	pollutedApproval := cloneJSON(approvalTransition)
	pollutedApproval.Proof.StateHash = ""
	pollutedApproval.Proof.VTXORefs = append([]tablecustody.VTXORef{{
		AmountSats: 1,
		TxID:       "polluted-approval-proof",
		VOut:       9,
	}}, pollutedApproval.Proof.VTXORefs...)
	pollutedApproval.Proof.FinalizedAt = nowISO()
	pollutedApproval.Proof.ArkTxID = "polluted-approval-arktx"
	pollutedApproval.Proof.SettlementWitness = &tablecustody.CustodySettlementWitness{
		ArkIntentID:  "polluted-approval-intent",
		ArkTxID:      "polluted-approval-arktx",
		FinalizedAt:  nowISO(),
		ProofPSBT:    "polluted-proof",
		CommitmentTx: "polluted-commitment",
	}

	if _, err := guest.handleCustodyApprovalFromPeer(nativeCustodyApprovalRequest{
		ExpectedPrevStateHash: pollutedApproval.PrevStateHash,
		Authorizer:            authorizerForActionRequest(actionRequest),
		PlayerID:              guest.walletID.PlayerID,
		ProtocolVersion:       nativeProtocolVersion,
		TableID:               tableID,
		Transition:            pollutedApproval,
	}); err != nil {
		t.Fatalf("approval should ignore proof pollution: %v", err)
	}

	pollutedSigning := cloneJSON(signingTransition)
	pollutedSigning.Proof.StateHash = ""
	pollutedSigning.Proof.VTXORefs = append([]tablecustody.VTXORef{{
		AmountSats: 1,
		TxID:       "polluted-signing-proof",
		VOut:       7,
	}}, pollutedSigning.Proof.VTXORefs...)
	pollutedSigning.Proof.FinalizedAt = nowISO()
	pollutedSigning.Proof.ArkIntentID = "polluted-signing-intent"
	pollutedSigning.Proof.SettlementWitness = &tablecustody.CustodySettlementWitness{
		ArkIntentID:  "polluted-signing-intent",
		ArkTxID:      "polluted-signing-txid",
		FinalizedAt:  nowISO(),
		ProofPSBT:    "polluted-proof",
		CommitmentTx: "polluted-commitment",
	}

	proofPSBT, err := buildCustodyProofPSBTForTest(plan)
	if err != nil {
		t.Fatalf("build proof psbt: %v", err)
	}
	if _, err := guest.handleCustodyTxSignFromPeer(nativeCustodyTxSignRequest{
		ExpectedPrevStateHash: pollutedSigning.PrevStateHash,
		Authorizer:            authorizerForActionRequest(actionRequest),
		PlayerID:              guest.walletID.PlayerID,
		PSBT:                  proofPSBT,
		Purpose:               "proof",
		ProtocolVersion:       nativeProtocolVersion,
		TableID:               tableID,
		Transition:            pollutedSigning,
		TransitionHash:        approvalTransition.Proof.RequestHash,
	}); err != nil {
		t.Fatalf("peer proof signing should ignore proof pollution: %v", err)
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
			ProtocolVersion:       nativeProtocolVersion,
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
			ProtocolVersion:       nativeProtocolVersion,
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
			ProtocolVersion:         nativeProtocolVersion,
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
			ProtocolVersion:       nativeProtocolVersion,
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
			ProtocolVersion:       nativeProtocolVersion,
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
			ProtocolVersion:         nativeProtocolVersion,
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
			ProtocolVersion:       nativeProtocolVersion,
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
			ProtocolVersion:       nativeProtocolVersion,
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
			ProtocolVersion:         nativeProtocolVersion,
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

	tableID, _ := createStartedTwoPlayerTableInSyntheticRealMode(t, host, guest)

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
		ProtocolVersion:       nativeProtocolVersion,
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
	expireCurrentTurnActionDeadlineForTest(t, &table)

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
		ProtocolVersion:       nativeProtocolVersion,
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
		ProtocolVersion:       nativeProtocolVersion,
		TableID:               tableID,
		Transition:            wrongTransition,
	}); err == nil || !strings.Contains(err.Error(), "does not match the locally derived successor") {
		t.Fatalf("expected binding-aware blind-post approval rejection, got %v", err)
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
		ProtocolVersion:       nativeProtocolVersion,
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
	expireCurrentTurnActionDeadlineForTest(t, &table)

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

func TestHandleActionSelectionRejectsSelectionAuthCandidateMismatch(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)
	table := waitForLocalCanAct(t, []*meshRuntime{host, guest}, host, tableID)

	if !turnMenuMatchesTable(table, table.PendingTurnMenu) || len(table.PendingTurnMenu.Options) < 2 {
		t.Fatalf("expected multiple deterministic turn options, got %+v", table.PendingTurnMenu)
	}
	selection, err := host.buildActionSelectionRequest(table, table.PendingTurnMenu.Options[0].Action)
	if err != nil {
		t.Fatalf("build action selection request: %v", err)
	}
	selection.CandidateHash = table.PendingTurnMenu.Options[1].CandidateHash

	if _, err := host.handleActionSelectionFromPeer(selection); err == nil || !strings.Contains(err.Error(), "selection auth candidate does not match") {
		t.Fatalf("expected mismatched selection auth candidate to be rejected, got %v", err)
	}
}

func TestHandleActionSettlementRejectsForgedActingPlayerSignature(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createStartedTwoPlayerTableInSyntheticRealMode(t, host, guest)
	waitForLocalCanAct(t, []*meshRuntime{host, guest}, host, tableID)
	if _, err := host.SendAction(tableID, game.Action{Type: game.ActionCall}); err != nil {
		t.Fatalf("host send preflop call: %v", err)
	}
	guestTable := waitForLocalCanAct(t, []*meshRuntime{host, guest}, guest, tableID)
	action := aggressiveActionForTable(t, guestTable)
	selection, err := guest.buildActionSelectionRequest(guestTable, action)
	if err != nil {
		t.Fatalf("build guest action selection request: %v", err)
	}
	lockResponse, err := host.handleActionSelectionFromPeer(selection)
	if err != nil {
		t.Fatalf("lock guest action: %v", err)
	}
	if err := guest.acceptRemoteTable(lockResponse.Table); err != nil {
		t.Fatalf("guest accept locked table: %v", err)
	}

	hostTable := mustReadNativeTable(t, host, tableID)
	transition, _, err := host.settleTurnCandidateTransition(hostTable, *hostTable.PendingTurnMenu.SelectedBundle)
	if err != nil {
		t.Fatalf("settle locked candidate as non-acting peer: %v", err)
	}
	forged := nativeActionSettlementRequest{
		CandidateHash:   hostTable.PendingTurnMenu.SelectedCandidateHash,
		PlayerID:        guest.walletID.PlayerID,
		ProtocolVersion: nativeProtocolVersion,
		TableID:         tableID,
		Transition:      transition,
	}
	forged, err = host.signActionSettlementRequest(forged)
	if err != nil {
		t.Fatalf("sign forged action settlement request: %v", err)
	}

	if _, err := host.handleActionSettlementFromPeer(forged); err == nil || !strings.Contains(err.Error(), "action settlement signature is invalid") {
		t.Fatalf("expected forged action settlement signature to be rejected, got %v", err)
	}
}

func TestSuccessorHostPublishesPersistedSettledLockedAction(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")
	witness := newMeshTestRuntime(t, "witness")

	if _, err := host.BootstrapPeer(witness.selfPeerURL(), "", nil); err != nil {
		t.Fatalf("bootstrap witness peer: %v", err)
	}
	tableID, _ := createStartedTwoPlayerTableInSyntheticRealMode(t, host, guest, witness.selfPeerID())

	waitForLocalCanAct(t, []*meshRuntime{host, guest, witness}, host, tableID)
	if _, err := host.SendAction(tableID, game.Action{Type: game.ActionCall}); err != nil {
		t.Fatalf("host send preflop call: %v", err)
	}
	guestTable := waitForLocalCanAct(t, []*meshRuntime{host, guest, witness}, guest, tableID)
	action := aggressiveActionForTable(t, guestTable)
	selection, err := guest.buildActionSelectionRequest(guestTable, action)
	if err != nil {
		t.Fatalf("build guest action selection request: %v", err)
	}
	lockResponse, err := host.handleActionSelectionFromPeer(selection)
	if err != nil {
		t.Fatalf("lock guest action: %v", err)
	}
	if err := guest.acceptRemoteTable(lockResponse.Table); err != nil {
		t.Fatalf("guest accept locked table: %v", err)
	}
	hostLocked := mustReadNativeTable(t, host, tableID)
	if err := witness.acceptRemoteTable(host.networkTableView(hostLocked, witness.walletID.PlayerID)); err != nil {
		t.Fatalf("witness accept locked table: %v", err)
	}

	guestLocked := mustReadNativeTable(t, guest, tableID)
	settlementRequest, err := guest.buildActionSettlementRequest(guestLocked)
	if err != nil {
		t.Fatalf("build acting-player settlement request: %v", err)
	}

	replicated := cloneJSON(hostLocked)
	replicated.PendingTurnMenu.SettledRequest = cloneJSON(&settlementRequest)
	if err := witness.acceptRemoteTable(host.networkTableView(replicated, witness.walletID.PlayerID)); err != nil {
		t.Fatalf("witness accept replicated settled request: %v", err)
	}
	if err := witness.store.withTableLock(tableID, func() error {
		table, err := witness.store.readTable(tableID)
		if err != nil || table == nil {
			return err
		}
		table.CurrentEpoch++
		table.CurrentHost = nativeKnownParticipant{ProfileName: witness.profileName, Peer: witness.self.Peer}
		table.Config.HostPeerID = witness.selfPeerID()
		table.LastHostHeartbeatAt = nowISO()
		if err := witness.advanceHandProtocolLocked(table); err != nil {
			return err
		}
		return witness.persistLocalTable(table, false)
	}); err != nil {
		t.Fatalf("advance replicated settled request under successor host: %v", err)
	}

	table := mustReadNativeTable(t, witness, tableID)
	if len(table.CustodyTransitions) == 0 {
		t.Fatal("expected successor host to publish a custody transition")
	}
	last := table.CustodyTransitions[len(table.CustodyTransitions)-1]
	if last.Proof.TransitionHash != settlementRequest.Transition.Proof.TransitionHash {
		t.Fatalf("expected successor host to publish the persisted exact transition hash %s, got %s", settlementRequest.Transition.Proof.TransitionHash, last.Proof.TransitionHash)
	}
	if !reflect.DeepEqual(last.Proof.SettlementWitness, settlementRequest.Transition.Proof.SettlementWitness) {
		t.Fatalf("expected successor host to publish the persisted exact settled transition witness, got %+v want %+v", last.Proof.SettlementWitness, settlementRequest.Transition.Proof.SettlementWitness)
	}
}

func TestSuccessorHostKeepsLockedTurnOutOfOrdinaryTimeoutSubstitution(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")
	witness := newMeshTestRuntime(t, "witness")

	if _, err := host.BootstrapPeer(witness.selfPeerURL(), "", nil); err != nil {
		t.Fatalf("bootstrap witness peer: %v", err)
	}
	tableID, _ := createStartedTwoPlayerTable(t, host, guest, witness.selfPeerID())

	waitForLocalCanAct(t, []*meshRuntime{host, guest, witness}, host, tableID)
	if _, err := host.SendAction(tableID, game.Action{Type: game.ActionCall}); err != nil {
		t.Fatalf("host send preflop call: %v", err)
	}
	guestTable := waitForLocalCanAct(t, []*meshRuntime{host, guest, witness}, guest, tableID)
	action := aggressiveActionForTable(t, guestTable)
	selection, err := guest.buildActionSelectionRequest(guestTable, action)
	if err != nil {
		t.Fatalf("build guest action selection request: %v", err)
	}
	lockResponse, err := host.handleActionSelectionFromPeer(selection)
	if err != nil {
		t.Fatalf("lock guest action: %v", err)
	}
	if err := guest.acceptRemoteTable(lockResponse.Table); err != nil {
		t.Fatalf("guest accept locked table: %v", err)
	}
	hostLocked := mustReadNativeTable(t, host, tableID)
	if err := witness.acceptRemoteTable(host.networkTableView(hostLocked, witness.walletID.PlayerID)); err != nil {
		t.Fatalf("witness accept locked table: %v", err)
	}

	before := mustReadNativeTable(t, witness, tableID)
	if before.PendingTurnMenu == nil || strings.TrimSpace(before.PendingTurnMenu.SelectedCandidateHash) == "" {
		t.Fatalf("expected locked pending turn menu before successor timeout check, got %+v", before.PendingTurnMenu)
	}
	beforeTransitions := len(before.CustodyTransitions)
	beforeActionLog := len(before.ActiveHand.State.ActionLog)

	if err := witness.store.withTableLock(tableID, func() error {
		table, err := witness.store.readTable(tableID)
		if err != nil || table == nil {
			return err
		}
		table.CurrentEpoch++
		table.CurrentHost = nativeKnownParticipant{ProfileName: witness.profileName, Peer: witness.self.Peer}
		table.Config.HostPeerID = witness.selfPeerID()
		table.LastHostHeartbeatAt = nowISO()
		table.PendingTurnMenu.ActionDeadlineAt = addMillis(nowISO(), -1_000)
		table.PendingTurnMenu.SettlementDeadlineAt = addMillis(nowISO(), 30_000)
		if err := witness.advanceHandProtocolLocked(table); err != nil {
			return err
		}
		return witness.persistLocalTable(table, false)
	}); err != nil {
		t.Fatalf("advance locked turn under successor host: %v", err)
	}

	after := mustReadNativeTable(t, witness, tableID)
	if after.CurrentHost.Peer.PeerID != witness.selfPeerID() {
		t.Fatalf("expected witness to stay current host, got %q", after.CurrentHost.Peer.PeerID)
	}
	if len(after.CustodyTransitions) != beforeTransitions {
		t.Fatalf("expected locked turn to avoid ordinary timeout publication, transitions=%d want=%d", len(after.CustodyTransitions), beforeTransitions)
	}
	if len(after.ActiveHand.State.ActionLog) != beforeActionLog {
		t.Fatalf("expected locked turn action log to stay at %d before settlement deadline, got %d", beforeActionLog, len(after.ActiveHand.State.ActionLog))
	}
	if after.PendingTurnMenu == nil || strings.TrimSpace(after.PendingTurnMenu.SelectedCandidateHash) == "" {
		t.Fatalf("expected locked pending turn menu to survive successor tick, got %+v", after.PendingTurnMenu)
	}
	if after.PendingTurnMenu.SettledRequest != nil {
		t.Fatalf("did not expect successor host to synthesize a settled request before publication, got %+v", after.PendingTurnMenu.SettledRequest)
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
	if lastTransition.Proof.TransitionHash == "" {
		t.Fatal("expected synthetic real mode check transition to finalize with custody proof metadata")
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

	table := waitForHandPhase(t, []*meshRuntime{host, guest}, host, tableID, game.StreetShowdownReveal)
	if table.ActiveHand.Cards.PhaseDeadlineAt == "" {
		t.Fatal("expected showdown-reveal phase to have a protocol deadline")
	}
}

func TestPotTimeoutLocktimeBacksOffVisibleDeadline(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)
	table := mustReadNativeTable(t, host, tableID)
	if table.ActiveHand == nil || table.ActiveHand.State.ActingSeatIndex == nil {
		t.Fatalf("expected actionable started hand, got %+v", table.ActiveHand)
	}
	legalActions := game.GetLegalActions(table.ActiveHand.State, table.ActiveHand.State.ActingSeatIndex)
	if len(legalActions) == 0 {
		t.Fatal("expected legal actions")
	}
	action := game.Action{Type: legalActions[0].Type}
	if legalActions[0].MinTotalSats != nil {
		action.TotalSats = *legalActions[0].MinTotalSats
	}
	for _, candidate := range legalActions {
		if candidate.Type != game.ActionCall && candidate.Type != game.ActionCheck {
			continue
		}
		action = game.Action{Type: candidate.Type}
		if candidate.MinTotalSats != nil {
			action.TotalSats = *candidate.MinTotalSats
		}
		break
	}
	nextState, err := game.ApplyHoldemAction(table.ActiveHand.State, *table.ActiveHand.State.ActingSeatIndex, action)
	if err != nil {
		t.Fatalf("apply action: %v", err)
	}
	transition, err := host.buildCustodyTransition(table, tablecustody.TransitionKindAction, &nextState, &action, nil)
	if err != nil {
		t.Fatalf("build action transition: %v", err)
	}
	if transition.NextState.ActionDeadlineAt == "" {
		t.Fatal("expected visible action deadline on next custody state")
	}
	var livePot tablecustody.PotSlice
	foundPot := false
	for _, slice := range transition.NextState.PotSlices {
		if slice.TotalSats <= 0 {
			continue
		}
		livePot = slice
		foundPot = true
		break
	}
	if !foundPot {
		t.Fatal("expected a live contested pot on the action successor")
	}
	spec, err := host.potOutputSpec(table, transition, livePot, host.potSpendSignerIDsForTransition(table, transition))
	if err != nil {
		t.Fatalf("build pot output spec: %v", err)
	}
	vtxoScript, err := arkscript.ParseVtxoScript(spec.Tapscripts)
	if err != nil {
		t.Fatalf("parse pot tapscripts: %v", err)
	}
	deadline, err := parseISOTimestamp(transition.NextState.ActionDeadlineAt)
	if err != nil {
		t.Fatalf("parse visible deadline: %v", err)
	}
	expectedLocktime := arklib.AbsoluteLocktime(deadline.Add(-custodyTimeoutLocktimeSlack).Unix())
	foundCLTV := false
	for _, closure := range vtxoScript.ForfeitClosures() {
		cltv, ok := closure.(*arkscript.CLTVMultisigClosure)
		if !ok {
			continue
		}
		foundCLTV = true
		if cltv.Locktime != expectedLocktime {
			t.Fatalf("expected timeout locktime %d, got %d", expectedLocktime, cltv.Locktime)
		}
	}
	if !foundCLTV {
		t.Fatal("expected timeout pot output to include at least one cltv closure")
	}
}

func TestSendActionResyncsAfterHostRotation(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")
	witness := newMeshTestRuntime(t, "witness")

	if _, err := host.BootstrapPeer(witness.selfPeerURL(), "", nil); err != nil {
		t.Fatalf("bootstrap witness peer: %v", err)
	}
	tableID, _ := createStartedTwoPlayerTable(t, host, guest, witness.selfPeerID())
	enableSyntheticRealMode(host, guest, witness)

	waitForLocalCanAct(t, []*meshRuntime{host, guest, witness}, host, tableID)
	if _, err := host.SendAction(tableID, game.Action{Type: game.ActionCall}); err != nil {
		t.Fatalf("host send preflop call: %v", err)
	}
	waitForLocalCanAct(t, []*meshRuntime{host, guest, witness}, guest, tableID)
	if _, err := guest.SendAction(tableID, aggressiveActionForTable(t, mustReadNativeTable(t, guest, tableID))); err != nil {
		t.Fatalf("guest send preflop bet: %v", err)
	}
	waitForLocalCanAct(t, []*meshRuntime{host, guest, witness}, host, tableID)
	if _, err := host.SendAction(tableID, passiveActionForTable(t, mustReadNativeTable(t, host, tableID))); err != nil {
		t.Fatalf("host send preflop call to flop: %v", err)
	}

	staleGuest := mustReadNativeTable(t, guest, tableID)
	if staleGuest.CurrentEpoch != 1 {
		t.Fatalf("expected guest to remain on epoch 1 before failover, got %d", staleGuest.CurrentEpoch)
	}
	if err := witness.forceProtocolFailover(tableID, "test host rotation before guest action"); err != nil {
		t.Fatalf("force protocol failover: %v", err)
	}
	rotated := mustReadNativeTable(t, witness, tableID)
	if rotated.CurrentHost.Peer.PeerID != witness.selfPeerID() {
		t.Fatalf("expected witness to become current host, got %q", rotated.CurrentHost.Peer.PeerID)
	}
	if rotated.CurrentEpoch != staleGuest.CurrentEpoch+1 {
		t.Fatalf("expected failover to advance epoch to %d, got %d", staleGuest.CurrentEpoch+1, rotated.CurrentEpoch)
	}

	actionSent := false
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := guest.SendAction(tableID, game.Action{Type: game.ActionCheck}); err == nil {
			actionSent = true
			break
		} else if !strings.Contains(err.Error(), "hand is still starting") {
			t.Fatalf("guest send flop check after host rotation: %v", err)
		}
		for _, runtime := range []*meshRuntime{host, guest, witness} {
			runtime.Tick()
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !actionSent {
		t.Fatal("timed out sending flop check after host rotation")
	}

	syncedGuest := mustReadNativeTable(t, guest, tableID)
	if syncedGuest.CurrentEpoch != rotated.CurrentEpoch {
		t.Fatalf("expected guest to resync to epoch %d, got %d", rotated.CurrentEpoch, syncedGuest.CurrentEpoch)
	}
	if syncedGuest.CurrentHost.Peer.PeerID != witness.selfPeerID() {
		t.Fatalf("expected guest to route through witness host after resync, got %q", syncedGuest.CurrentHost.Peer.PeerID)
	}
	if syncedGuest.LatestCustodyState == nil || staleGuest.LatestCustodyState == nil {
		t.Fatal("expected custody state before and after resync")
	}
	if syncedGuest.LatestCustodyState.CustodySeq <= staleGuest.LatestCustodyState.CustodySeq {
		t.Fatalf("expected guest custody seq to advance past %d, got %d", staleGuest.LatestCustodyState.CustodySeq, syncedGuest.LatestCustodyState.CustodySeq)
	}
}

func TestForceProtocolFailoverResetsManualRevealDeadline(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")
	witness := newMeshTestRuntime(t, "witness")

	if _, err := host.BootstrapPeer(witness.selfPeerURL(), "", nil); err != nil {
		t.Fatalf("bootstrap witness peer: %v", err)
	}
	tableID, _ := createStartedTwoPlayerTable(t, host, guest, witness.selfPeerID())
	enableSyntheticRealMode(host, guest, witness)

	waitForLocalCanAct(t, []*meshRuntime{host, guest, witness}, host, tableID)
	if _, err := host.SendAction(tableID, game.Action{Type: game.ActionCall}); err != nil {
		t.Fatalf("host send preflop call: %v", err)
	}
	waitForLocalCanAct(t, []*meshRuntime{host, guest, witness}, guest, tableID)
	if _, err := guest.SendAction(tableID, aggressiveActionForTable(t, mustReadNativeTable(t, guest, tableID))); err != nil {
		t.Fatalf("guest send preflop bet: %v", err)
	}
	waitForLocalCanAct(t, []*meshRuntime{host, guest, witness}, host, tableID)
	if _, err := host.SendAction(tableID, passiveActionForTable(t, mustReadNativeTable(t, host, tableID))); err != nil {
		t.Fatalf("host send preflop call to flop: %v", err)
	}

	before := waitForHandPhase(t, []*meshRuntime{host, guest, witness}, witness, tableID, game.StreetFlopReveal)
	if before.ActiveHand == nil || before.ActiveHand.Cards.PhaseDeadlineAt == "" {
		t.Fatal("expected tracked reveal deadline before host rotation")
	}
	if err := witness.store.withTableLock(tableID, func() error {
		table, err := witness.store.readTable(tableID)
		if err != nil || table == nil {
			return err
		}
		if table.ActiveHand == nil {
			return errors.New("expected active hand while aging reveal deadline")
		}
		table.ActiveHand.Cards.PhaseDeadlineAt = addMillis(nowISO(), -1_000)
		return witness.store.writeTable(table)
	}); err != nil {
		t.Fatalf("age witness reveal deadline: %v", err)
	}
	before = mustReadNativeTable(t, witness, tableID)
	beforeDeadline, err := parseISOTimestamp(before.ActiveHand.Cards.PhaseDeadlineAt)
	if err != nil {
		t.Fatalf("parse pre-rotation deadline: %v", err)
	}

	if err := witness.forceProtocolFailover(tableID, "test manual host rotation resets deadline"); err != nil {
		t.Fatalf("force protocol failover: %v", err)
	}

	rotated := mustReadNativeTable(t, witness, tableID)
	if rotated.ActiveHand == nil {
		t.Fatal("expected active hand after host rotation")
	}
	if rotated.CurrentHost.Peer.PeerID != witness.selfPeerID() {
		t.Fatalf("expected witness to become current host, got %q", rotated.CurrentHost.Peer.PeerID)
	}
	if rotated.ActiveHand.State.Phase != game.StreetFlopReveal {
		t.Fatalf("expected flop reveal to remain active after manual rotation, got %s", rotated.ActiveHand.State.Phase)
	}
	if rotated.ActiveHand.Cards.PhaseDeadlineAt == "" {
		t.Fatal("expected manual host rotation to refresh the reveal deadline")
	}
	rotatedDeadline, err := parseISOTimestamp(rotated.ActiveHand.Cards.PhaseDeadlineAt)
	if err != nil {
		t.Fatalf("parse rotated deadline: %v", err)
	}
	if !rotatedDeadline.After(beforeDeadline) {
		t.Fatalf("expected rotated deadline %q to be later than %q", rotated.ActiveHand.Cards.PhaseDeadlineAt, before.ActiveHand.Cards.PhaseDeadlineAt)
	}
}

func TestActionRetryStateChangedDetectsAcceptedHistoryAdvance(t *testing.T) {
	previous := nativeTableState{
		CurrentEpoch: 1,
		CurrentHost: nativeKnownParticipant{
			Peer: NativePeerAddress{PeerID: "host-1", PeerURL: "http://host-1"},
		},
		Events:        []NativeSignedTableEvent{{Seq: 1}},
		LastEventHash: "hash-1",
		Snapshots:     []NativeCooperativeTableSnapshot{{SnapshotID: "snapshot-1"}},
	}
	refreshed := cloneJSON(previous)
	refreshed.Events = append(refreshed.Events, NativeSignedTableEvent{Seq: 2})
	refreshed.LastEventHash = "hash-2"

	if !actionRetryStateChanged(previous, refreshed) {
		t.Fatal("expected accepted-history growth to trigger an action retry")
	}
}

func TestActionRetryStateChangedDetectsLockedTurnProgress(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)
	table := waitForLocalCanAct(t, []*meshRuntime{host, guest}, host, tableID)
	if !turnMenuMatchesTable(table, table.PendingTurnMenu) {
		t.Fatalf("expected actionable pending turn menu, got %+v", table.PendingTurnMenu)
	}
	cache, err := host.loadLocalTurnBundleCache(table)
	if err != nil {
		t.Fatalf("load local turn bundle cache: %v", err)
	}
	if cache == nil {
		t.Fatal("expected local turn bundle cache for actionable lock progress test")
	}
	option := table.PendingTurnMenu.Options[0]
	bundle, ok := findTurnCandidateByOptionFromCache(table.PendingTurnMenu, cache, option.OptionID)
	if !ok {
		t.Fatalf("expected cached bundle for option %s", option.OptionID)
	}
	auth, err := host.buildSelectionAuth(table, bundle.CandidateHash)
	if err != nil {
		t.Fatalf("build selection auth: %v", err)
	}

	previous := cloneJSON(table)
	refreshed := cloneJSON(table)
	refreshed.PendingTurnMenu.SelectedCandidateHash = bundle.CandidateHash
	refreshed.PendingTurnMenu.SelectionAuth = cloneJSON(&auth)
	refreshed.PendingTurnMenu.LockedAt = nowISO()
	refreshed.PendingTurnMenu.SettlementDeadlineAt = addMillis(refreshed.PendingTurnMenu.LockedAt, host.selectionSettlementTimeoutMSForTable(refreshed))
	selectedBundle := cloneJSON(bundle)
	refreshed.PendingTurnMenu.SelectedBundle = &selectedBundle

	if !actionRetryStateChanged(previous, refreshed) {
		t.Fatal("expected locked pending-turn progress to trigger an action retry")
	}
}

func TestActionRetryStateChangedDetectsPersistedSettlementProgress(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)
	table := waitForLocalCanAct(t, []*meshRuntime{host, guest}, host, tableID)
	if !turnMenuMatchesTable(table, table.PendingTurnMenu) {
		t.Fatalf("expected actionable pending turn menu, got %+v", table.PendingTurnMenu)
	}
	cache, err := host.loadLocalTurnBundleCache(table)
	if err != nil {
		t.Fatalf("load local turn bundle cache: %v", err)
	}
	if cache == nil {
		t.Fatal("expected local turn bundle cache for settlement progress test")
	}
	option := table.PendingTurnMenu.Options[0]
	bundle, ok := findTurnCandidateByOptionFromCache(table.PendingTurnMenu, cache, option.OptionID)
	if !ok {
		t.Fatalf("expected cached bundle for option %s", option.OptionID)
	}
	auth, err := host.buildSelectionAuth(table, bundle.CandidateHash)
	if err != nil {
		t.Fatalf("build selection auth: %v", err)
	}
	locked := cloneJSON(table)
	locked.PendingTurnMenu.SelectedCandidateHash = bundle.CandidateHash
	locked.PendingTurnMenu.SelectionAuth = cloneJSON(&auth)
	locked.PendingTurnMenu.LockedAt = nowISO()
	locked.PendingTurnMenu.SettlementDeadlineAt = addMillis(locked.PendingTurnMenu.LockedAt, host.selectionSettlementTimeoutMSForTable(locked))
	selectedBundle := cloneJSON(bundle)
	locked.PendingTurnMenu.SelectedBundle = &selectedBundle
	settlementRequest, err := host.buildActionSettlementRequest(locked)
	if err != nil {
		t.Fatalf("build action settlement request: %v", err)
	}

	refreshed := cloneJSON(locked)
	refreshed.PendingTurnMenu.SettledRequest = cloneJSON(&settlementRequest)
	if !actionRetryStateChanged(locked, refreshed) {
		t.Fatal("expected persisted settled request progress to trigger an action retry")
	}
}

func TestSendActionReturnsStaleWhenDifferentCandidateIsAlreadyLocked(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)

	waitForLocalCanAct(t, []*meshRuntime{host, guest}, host, tableID)
	if _, err := host.SendAction(tableID, game.Action{Type: game.ActionCall}); err != nil {
		t.Fatalf("host send preflop call: %v", err)
	}
	guestTable := waitForLocalCanAct(t, []*meshRuntime{host, guest}, guest, tableID)
	lockedAction := aggressiveActionForTable(t, guestTable)
	staleAction := passiveActionForTable(t, guestTable)
	if reflect.DeepEqual(lockedAction, staleAction) {
		t.Fatalf("expected distinct guest actions for stale-lock test, got %+v", lockedAction)
	}

	selection, err := guest.buildActionSelectionRequest(guestTable, lockedAction)
	if err != nil {
		t.Fatalf("build guest action selection request: %v", err)
	}
	lockResponse, err := host.handleActionSelectionFromPeer(selection)
	if err != nil {
		t.Fatalf("lock guest action: %v", err)
	}
	if err := guest.acceptRemoteTable(lockResponse.Table); err != nil {
		t.Fatalf("guest accept locked table: %v", err)
	}

	if _, err := guest.SendAction(tableID, staleAction); err == nil || !strings.Contains(err.Error(), "different candidate") {
		t.Fatalf("expected stale send action to stop on the conflicting locked candidate, got %v", err)
	}
}

func TestRetryableActionResponseStateErrorIncludesAcceptedHistoryRollback(t *testing.T) {
	if !isRetryableActionResponseStateError(errors.New("accepted history would roll back table events")) {
		t.Fatal("expected event rollback to be retryable")
	}
	if !isRetryableActionResponseStateError(errors.New("accepted history would roll back table snapshots")) {
		t.Fatal("expected snapshot rollback to be retryable")
	}
	if !isRetryableActionResponseStateError(errors.New("accepted active hand transcript would roll back")) {
		t.Fatal("expected active hand transcript rollback to be retryable")
	}
	if !isRetryableActionResponseStateError(errors.New("ready public state latest event hash does not match accepted event history")) {
		t.Fatal("expected ready public state accepted-history mismatch to be retryable")
	}
	if isRetryableActionResponseStateError(errors.New("historical event 0 was rewritten")) {
		t.Fatal("did not expect rewritten history to be retryable")
	}
}

func TestStaleRemoteTableErrorIncludesAcceptedTranscriptRollback(t *testing.T) {
	if !isStaleRemoteTableError(errors.New("accepted active hand transcript would roll back")) {
		t.Fatal("expected active hand transcript rollback to be treated as stale")
	}
	if !isStaleRemoteTableError(errors.New("ready public state latest event hash does not match accepted event history")) {
		t.Fatal("expected ready public state accepted-history mismatch to be treated as stale")
	}
	if isStaleRemoteTableError(errors.New("accepted active hand transcript was rewritten")) {
		t.Fatal("did not expect rewritten transcript history to be treated as stale")
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

func TestRealSettlementUsesExtendedTimingWindows(t *testing.T) {
	runtime := newMeshTestRuntime(t, "host")
	runtime.config.UseMockSettlement = false

	if got := runtime.handProtocolTimeoutMS(); got != 20_000 {
		t.Fatalf("expected real-mode hand protocol timeout 20000ms, got %d", got)
	}
	if got := runtime.actionTimeoutMS(); got != 16_000 {
		t.Fatalf("expected real-mode action timeout 16000ms, got %d", got)
	}
}

func newMeshTestRuntime(t *testing.T, profileName string, options ...meshTestRuntimeOption) *meshRuntime {
	t.Helper()

	config := meshTestRuntimeConfig{}
	for _, option := range options {
		if option != nil {
			option(&config)
		}
	}

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
	runtime.chainTipStatus = config.chainTipStatus
	runtime.chainTxStatus = config.chainTxStatus

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
		runtime.testActionTimeoutMS = syntheticRealTestActionTimeoutMS
		runtime.testHandProtocolMS = syntheticRealTestProtocolTimeoutMS
		runtime.custodyArkVerify = func(refs []tablecustody.VTXORef, requireSpendable bool) error {
			return nil
		}
		runtime.custodyBatchExecute = func(table nativeTableState, prevStateHash, transitionHash string, inputs []custodyInputSpec, proofSignerIDs, treeSignerIDs []string, outputs []custodyBatchOutput) (*custodyBatchResult, error) {
			return buildSyntheticCustodyBatchResultForTest(runtime, table, transitionHash, inputs, proofSignerIDs, treeSignerIDs, outputs)
		}
		runtime.custodyPSBTSign = func(profileName, tx string) (string, error) {
			return syntheticSignCustodyPSBTForTest(runtime, tx)
		}
		runtime.custodyRecoveryExecute = func(profileName, signedPSBT string) (walletpkg.CustodyRecoveryResult, error) {
			return syntheticExecuteCustodyRecoveryForTest(signedPSBT)
		}
	}
}

func encodeSyntheticExitTxHexForTest(t *testing.T, tx *wire.MsgTx) string {
	t.Helper()
	var encoded bytes.Buffer
	if err := tx.Serialize(&encoded); err != nil {
		t.Fatalf("serialize synthetic exit tx: %v", err)
	}
	return hex.EncodeToString(encoded.Bytes())
}

func buildSyntheticExitWitnessForTest(t *testing.T, refs []tablecustody.VTXORef, includeSweep bool) *tablecustody.CustodyExitWitness {
	t.Helper()
	if len(refs) == 0 {
		t.Fatal("expected source refs for synthetic exit witness")
	}
	anchorTx := wire.NewMsgTx(2)
	for _, ref := range refs {
		hash, err := chainhash.NewHashFromStr(strings.TrimSpace(ref.TxID))
		if err != nil {
			t.Fatalf("parse synthetic exit source ref %s:%d: %v", ref.TxID, ref.VOut, err)
		}
		anchorTx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: wire.OutPoint{
				Hash:  *hash,
				Index: ref.VOut,
			},
		})
	}
	anchorTx.AddTxOut(&wire.TxOut{Value: 1, PkScript: []byte{txscript.OP_TRUE}})
	witness := &tablecustody.CustodyExitWitness{
		BroadcastTransactions: []tablecustody.CustodyExitTransaction{{
			TransactionHex: encodeSyntheticExitTxHexForTest(t, anchorTx),
			TransactionID:  anchorTx.TxHash().String(),
		}},
	}
	if !includeSweep {
		return witness
	}
	sweepTx := wire.NewMsgTx(2)
	sweepTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  anchorTx.TxHash(),
			Index: 0,
		},
	})
	sweepTx.AddTxOut(&wire.TxOut{Value: 1, PkScript: []byte{txscript.OP_TRUE}})
	witness.SweepTransaction = &tablecustody.CustodyExitTransaction{
		TransactionHex: encodeSyntheticExitTxHexForTest(t, sweepTx),
		TransactionID:  sweepTx.TxHash().String(),
	}
	return witness
}

func buildSyntheticExitResultForTest(t *testing.T, refs []tablecustody.VTXORef, includeSweep bool) walletpkg.CustodyExitResult {
	t.Helper()
	canonicalRefs := canonicalVTXORefs(refs)
	witness := buildSyntheticExitWitnessForTest(t, canonicalRefs, includeSweep)
	summary, err := walletpkg.VerifyUnilateralExitWitness(canonicalRefs, *witness)
	if err != nil {
		t.Fatalf("verify synthetic exit witness: %v", err)
	}
	return walletpkg.CustodyExitResult{
		BroadcastTxIDs: append([]string(nil), summary.BroadcastTxIDs...),
		Pending:        !includeSweep,
		SourceRefs:     canonicalRefs,
		SweepTxID:      summary.SweepTxID,
	}
}

func syntheticSignCustodyPSBTForTest(runtime *meshRuntime, tx string) (string, error) {
	packet, err := psbt.NewFromRawBytes(strings.NewReader(tx), true)
	if err != nil {
		return "", err
	}
	privKeyBytes, err := hex.DecodeString(runtime.walletID.PrivateKeyHex)
	if err != nil {
		return "", err
	}
	privKey, _ := btcec.PrivKeyFromBytes(privKeyBytes)
	xOnlyHex, err := xOnlyPubkeyHexFromCompressed(runtime.walletID.PublicKeyHex)
	if err != nil {
		return "", err
	}
	xOnlyPubkey, err := hex.DecodeString(xOnlyHex)
	if err != nil {
		return "", err
	}
	prevOuts := txscript.NewMultiPrevOutFetcher(nil)
	for inputIndex, txIn := range packet.UnsignedTx.TxIn {
		input := packet.Inputs[inputIndex]
		if input.WitnessUtxo == nil {
			continue
		}
		prevOuts.AddPrevOut(txIn.PreviousOutPoint, &wire.TxOut{
			PkScript: append([]byte(nil), input.WitnessUtxo.PkScript...),
			Value:    input.WitnessUtxo.Value,
		})
	}
	sigHashes := txscript.NewTxSigHashes(packet.UnsignedTx, prevOuts)
	for inputIndex := range packet.Inputs {
		input := packet.Inputs[inputIndex]
		if input.WitnessUtxo == nil || len(input.TaprootLeafScript) == 0 {
			continue
		}
		leafScript := input.TaprootLeafScript[0]
		tokenizer := txscript.MakeScriptTokenizer(0, leafScript.Script)
		signsThisLeaf := false
		for tokenizer.Next() {
			if data := tokenizer.Data(); len(data) == len(xOnlyPubkey) && slices.Equal(data, xOnlyPubkey) {
				signsThisLeaf = true
				break
			}
		}
		if tokenizer.Err() != nil || !signsThisLeaf {
			continue
		}
		leaf := txscript.NewTapLeaf(leafScript.LeafVersion, leafScript.Script)
		leafHash := leaf.TapHash()
		alreadySigned := false
		for _, signature := range input.TaprootScriptSpendSig {
			if signature != nil && string(signature.XOnlyPubKey) == string(xOnlyPubkey) {
				alreadySigned = true
				break
			}
		}
		if alreadySigned {
			continue
		}
		sigHash := input.SighashType
		if sigHash == 0 {
			sigHash = txscript.SigHashDefault
		}
		signature, err := txscript.RawTxInTapscriptSignature(
			packet.UnsignedTx,
			sigHashes,
			inputIndex,
			input.WitnessUtxo.Value,
			input.WitnessUtxo.PkScript,
			leaf,
			sigHash,
			privKey,
		)
		if err != nil {
			return "", err
		}
		if sigHash != txscript.SigHashDefault {
			signature = signature[:len(signature)-1]
		}
		input.TaprootScriptSpendSig = append(input.TaprootScriptSpendSig, &psbt.TaprootScriptSpendSig{
			XOnlyPubKey: append([]byte(nil), xOnlyPubkey...),
			LeafHash:    leafHash.CloneBytes(),
			Signature:   append([]byte(nil), signature...),
			SigHash:     sigHash,
		})
		packet.Inputs[inputIndex] = input
	}
	return packet.B64Encode()
}

func syntheticExecuteCustodyRecoveryForTest(signedPSBT string) (walletpkg.CustodyRecoveryResult, error) {
	packet, err := psbt.NewFromRawBytes(strings.NewReader(signedPSBT), true)
	if err != nil {
		return walletpkg.CustodyRecoveryResult{}, err
	}
	recoveryTxID := packet.UnsignedTx.TxID()
	return walletpkg.CustodyRecoveryResult{
		BroadcastTxIDs: []string{recoveryTxID},
		RecoveryTxID:   recoveryTxID,
	}, nil
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
	waitForLocalCanAct(t, []*meshRuntime{host, guest}, host, tableID)
	return tableID, inviteCode
}

func createStartedTwoPlayerTableInSyntheticRealMode(t *testing.T, host, guest *meshRuntime, witnessPeerIDs ...string) (string, string) {
	t.Helper()

	enableSyntheticRealMode(host, guest)
	tableID, inviteCode := createJoinedTwoPlayerTable(t, host, guest, witnessPeerIDs...)
	startNextHandForTest(t, host, tableID)
	waitForLocalCanAct(t, []*meshRuntime{host, guest}, host, tableID)
	return tableID, inviteCode
}

func mustBuildExpectedHandMessageRequest(t *testing.T, runtime *meshRuntime, table nativeTableState, kind string) nativeHandMessageRequest {
	t.Helper()

	record, err := runtime.buildLocalContributionRecord(table)
	if err != nil {
		t.Fatalf("build %s record: %v", kind, err)
	}
	if record == nil || record.Kind != kind {
		t.Fatalf("expected %s record, got %#v", kind, record)
	}
	request, err := runtime.buildSignedHandMessageRequest(table, *record)
	if err != nil {
		t.Fatalf("build %s request: %v", kind, err)
	}
	return request
}

func acceptHandMessageResponseForTest(t *testing.T, runtime *meshRuntime, response nativeHandMessageResponse) {
	t.Helper()

	if err := runtime.acceptRemoteTable(response.Table); err != nil {
		t.Fatalf("accept hand message response table: %v", err)
	}
}

func passiveActionForTable(t *testing.T, table nativeTableState) game.Action {
	t.Helper()

	if table.ActiveHand == nil || table.ActiveHand.State.ActingSeatIndex == nil {
		t.Fatalf("expected actionable hand state, got %+v", table.ActiveHand)
	}
	legalActions := game.GetLegalActions(table.ActiveHand.State, table.ActiveHand.State.ActingSeatIndex)
	if len(legalActions) == 0 {
		t.Fatal("expected legal actions")
	}
	action := game.Action{Type: legalActions[0].Type}
	if legalActions[0].MinTotalSats != nil {
		action.TotalSats = *legalActions[0].MinTotalSats
	}
	for _, candidate := range legalActions {
		if candidate.Type != game.ActionCheck && candidate.Type != game.ActionCall {
			continue
		}
		action = game.Action{Type: candidate.Type}
		if candidate.MinTotalSats != nil {
			action.TotalSats = *candidate.MinTotalSats
		}
		break
	}
	return action
}

func finiteMenuOptionsForTable(t *testing.T, table nativeTableState) []NativeActionMenuOption {
	t.Helper()

	if turnMenuMatchesTable(table, table.PendingTurnMenu) && len(table.PendingTurnMenu.Options) > 0 {
		return append([]NativeActionMenuOption(nil), table.PendingTurnMenu.Options...)
	}
	if table.ActiveHand == nil {
		t.Fatalf("expected active hand for finite-menu options, got %+v", table.ActiveHand)
	}
	options, err := deriveFiniteMenuOptions(table.ActiveHand.State, table)
	if err != nil {
		t.Fatalf("derive finite-menu options: %v", err)
	}
	if len(options) == 0 {
		t.Fatal("expected finite-menu options")
	}
	return options
}

func aggressiveActionForTable(t *testing.T, table nativeTableState) game.Action {
	t.Helper()

	var fallback *game.Action
	for _, option := range finiteMenuOptionsForTable(t, table) {
		switch option.Action.Type {
		case game.ActionBet, game.ActionRaise:
			action := option.Action
			if strings.HasSuffix(option.OptionID, "-min") {
				return action
			}
			if fallback == nil {
				fallback = &action
			}
		}
	}
	if fallback != nil {
		return *fallback
	}
	t.Fatalf("expected aggressive finite-menu action, got %+v", finiteMenuOptionsForTable(t, table))
	return game.Action{}
}

func allInActionForTable(t *testing.T, table nativeTableState) game.Action {
	t.Helper()

	var fallback *game.Action
	for _, option := range finiteMenuOptionsForTable(t, table) {
		switch option.Action.Type {
		case game.ActionBet, game.ActionRaise:
			action := option.Action
			if strings.Contains(option.OptionID, "all-in") {
				return action
			}
			if fallback == nil || action.TotalSats > fallback.TotalSats {
				candidate := action
				fallback = &candidate
			}
		}
	}
	if fallback != nil {
		return *fallback
	}
	t.Fatalf("expected all-in finite-menu action, got %+v", finiteMenuOptionsForTable(t, table))
	return game.Action{}
}

func advanceTableByPassiveActionsToPhase(t *testing.T, host, guest *meshRuntime, tableID string, phase game.Street) nativeTableState {
	t.Helper()

	runtimes := []*meshRuntime{host, guest}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		table := mustReadNativeTable(t, host, tableID)
		if table.ActiveHand != nil && table.ActiveHand.State.Phase == phase {
			return table
		}
		if table.ActiveHand != nil && game.PhaseAllowsActions(table.ActiveHand.State.Phase) && table.ActiveHand.State.ActingSeatIndex != nil {
			seat, ok := seatRecordByIndex(table, *table.ActiveHand.State.ActingSeatIndex)
			if !ok {
				t.Fatalf("acting seat %d missing from table", *table.ActiveHand.State.ActingSeatIndex)
			}
			actor := host
			if seat.PlayerID == guest.walletID.PlayerID {
				actor = guest
			}
			actorTable := waitForLocalCanAct(t, runtimes, actor, tableID)
			action := passiveActionForTable(t, actorTable)
			if _, err := actor.SendAction(tableID, action); err != nil {
				t.Fatalf("send passive %s action while advancing to %s: %v", action.Type, phase, err)
			}
			continue
		}
		for _, runtime := range runtimes {
			runtime.Tick()
		}
		time.Sleep(25 * time.Millisecond)
	}
	table := mustReadNativeTable(t, host, tableID)
	t.Fatalf("timed out advancing table to phase %s, last phase=%v", phase, func() any {
		if table.ActiveHand == nil {
			return nil
		}
		return table.ActiveHand.State.Phase
	}())
	return nativeTableState{}
}

func appendTranscriptRecordsForTest(t *testing.T, runtime *meshRuntime, tableID string, records ...game.HandTranscriptRecord) {
	t.Helper()

	if len(records) == 0 {
		return
	}
	if err := runtime.store.withTableLock(tableID, func() error {
		table, err := runtime.store.readTable(tableID)
		if err != nil || table == nil {
			return err
		}
		for _, record := range records {
			if existing, ok, err := findParticipantHandMessageSlotRecord(table.ActiveHand.Cards.Transcript, record); err != nil {
				return err
			} else if ok {
				existingKey, err := handMessageRecordKey(table.Config.TableID, table.ActiveHand.State.HandID, table.ActiveHand.State.HandNumber, existing)
				if err != nil {
					return err
				}
				recordKey, err := handMessageRecordKey(table.Config.TableID, table.ActiveHand.State.HandID, table.ActiveHand.State.HandNumber, record)
				if err != nil {
					return err
				}
				if existingKey == recordKey {
					continue
				}
				return errors.New("conflicting transcript record already present")
			}
			if err := runtime.appendHandTranscriptRecord(table, record); err != nil {
				return err
			}
		}
		return runtime.persistLocalTable(table, false)
	}); err != nil {
		t.Fatalf("append transcript records for test: %v", err)
	}
}

func prepareGuestFairnessCommitRequest(t *testing.T) (*meshRuntime, *meshRuntime, string, nativeHandMessageRequest) {
	t.Helper()

	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")
	tableID, _ := createJoinedTwoPlayerTable(t, host, guest)
	startNextHandForTest(t, host, tableID)
	table := waitForHandPhase(t, []*meshRuntime{host, guest}, guest, tableID, game.StreetCommitment)
	return host, guest, tableID, mustBuildExpectedHandMessageRequest(t, guest, table, nativeHandMessageFairnessCommit)
}

func prepareGuestFairnessRevealRequest(t *testing.T) (*meshRuntime, *meshRuntime, string, nativeHandMessageRequest) {
	t.Helper()

	host, guest, tableID, commitRequest := prepareGuestFairnessCommitRequest(t)
	response, err := host.handleHandMessageFromPeer(commitRequest)
	if err != nil {
		t.Fatalf("accept guest fairness commit: %v", err)
	}
	acceptHandMessageResponseForTest(t, guest, response)
	table := waitForHandPhase(t, []*meshRuntime{host, guest}, guest, tableID, game.StreetReveal)
	return host, guest, tableID, mustBuildExpectedHandMessageRequest(t, guest, table, nativeHandMessageFairnessReveal)
}

func prepareGuestPrivateDeliveryRequest(t *testing.T) (*meshRuntime, *meshRuntime, string, nativeHandMessageRequest) {
	t.Helper()

	host, guest, tableID, revealRequest := prepareGuestFairnessRevealRequest(t)
	response, err := host.handleHandMessageFromPeer(revealRequest)
	if err != nil {
		t.Fatalf("accept guest fairness reveal: %v", err)
	}
	acceptHandMessageResponseForTest(t, guest, response)
	table := waitForHandPhase(t, []*meshRuntime{host, guest}, guest, tableID, game.StreetPrivateDelivery)
	return host, guest, tableID, mustBuildExpectedHandMessageRequest(t, guest, table, nativeHandMessagePrivateDelivery)
}

func prepareGuestBoardShareRequest(t *testing.T) (*meshRuntime, *meshRuntime, string, nativeHandMessageRequest) {
	t.Helper()

	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")
	tableID, _ := createStartedTwoPlayerTable(t, host, guest)
	advanceTableByPassiveActionsToPhase(t, host, guest, tableID, game.StreetFlopReveal)
	table := waitForHandPhase(t, []*meshRuntime{host, guest}, guest, tableID, game.StreetFlopReveal)
	return host, guest, tableID, mustBuildExpectedHandMessageRequest(t, guest, table, nativeHandMessageBoardShare)
}

func prepareGuestBoardOpenRequest(t *testing.T) (*meshRuntime, *meshRuntime, string, nativeHandMessageRequest) {
	t.Helper()

	host, guest, tableID, _ := prepareGuestBoardShareRequest(t)
	guestTable := mustReadNativeTable(t, guest, tableID)
	hostTable := mustReadNativeTable(t, host, tableID)
	phase := string(game.StreetFlopReveal)
	records := make([]game.HandTranscriptRecord, 0, 2)

	hostSeat := seatIndexForPlayer(t, guestTable, host.walletID.PlayerID)
	if _, ok := findTranscriptRecord(guestTable.ActiveHand.Cards.Transcript, nativeHandMessageBoardShare, &hostSeat, phase, nil); !ok {
		hostRecord, err := host.buildLocalContributionRecord(hostTable)
		if err != nil {
			t.Fatalf("build host board-share record: %v", err)
		}
		if hostRecord == nil || hostRecord.Kind != nativeHandMessageBoardShare {
			t.Fatalf("expected host board-share record, got %#v", hostRecord)
		}
		records = append(records, *hostRecord)
	}

	guestRecord, err := guest.buildLocalContributionRecord(guestTable)
	if err != nil {
		t.Fatalf("build guest board-share record: %v", err)
	}
	if guestRecord == nil || guestRecord.Kind != nativeHandMessageBoardShare {
		t.Fatalf("expected guest board-share record, got %#v", guestRecord)
	}
	guestSeat := seatIndexForPlayer(t, guestTable, guest.walletID.PlayerID)
	if _, ok := findTranscriptRecord(guestTable.ActiveHand.Cards.Transcript, nativeHandMessageBoardShare, &guestSeat, phase, nil); !ok {
		records = append(records, *guestRecord)
	}

	appendTranscriptRecordsForTest(t, host, tableID, records...)
	appendTranscriptRecordsForTest(t, guest, tableID, records...)

	table := mustReadNativeTable(t, guest, tableID)
	return host, guest, tableID, mustBuildExpectedHandMessageRequest(t, guest, table, nativeHandMessageBoardOpen)
}

func prepareGuestShowdownRevealRequest(t *testing.T) (*meshRuntime, *meshRuntime, string, nativeHandMessageRequest) {
	t.Helper()

	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")
	tableID, _ := createStartedTwoPlayerTable(t, host, guest)
	advanceTableByPassiveActionsToPhase(t, host, guest, tableID, game.StreetShowdownReveal)
	table := waitForHandPhase(t, []*meshRuntime{host, guest}, guest, tableID, game.StreetShowdownReveal)
	return host, guest, tableID, mustBuildExpectedHandMessageRequest(t, guest, table, nativeHandMessageShowdownReveal)
}

func tamperHandMessageForConflict(t *testing.T, runtime *meshRuntime, request nativeHandMessageRequest) nativeHandMessageRequest {
	t.Helper()

	tampered := cloneJSON(request)
	switch tampered.Kind {
	case nativeHandMessageFairnessCommit:
		tampered.CommitmentHash = strings.Repeat("a", 64)
	case nativeHandMessageFairnessReveal:
		tampered.DeckStageRoot = strings.Repeat("b", 64)
	case nativeHandMessagePrivateDelivery, nativeHandMessageBoardShare:
		if len(tampered.PartialCiphertexts) == 0 {
			t.Fatalf("expected ciphertexts on %s request", tampered.Kind)
		}
		tampered.PartialCiphertexts[0] = "tampered-" + tampered.PartialCiphertexts[0]
	case nativeHandMessageBoardOpen, nativeHandMessageShowdownReveal:
		if len(tampered.Cards) == 0 {
			t.Fatalf("expected cards on %s request", tampered.Kind)
		}
		if tampered.Cards[0] == "As" {
			tampered.Cards[0] = "Kd"
		} else {
			tampered.Cards[0] = "As"
		}
	default:
		t.Fatalf("unsupported hand message kind %q", tampered.Kind)
	}
	signatureHex, err := settlementcore.SignStructuredData(runtime.walletID.PrivateKeyHex, nativeHandMessageAuthPayload(tampered))
	if err != nil {
		t.Fatalf("sign tampered %s request: %v", tampered.Kind, err)
	}
	tampered.SignatureHex = signatureHex
	return tampered
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

	deadline := time.Now().Add(30 * time.Second)
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

	deadline := time.Now().Add(30 * time.Second)
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

	deadline := time.Now().Add(30 * time.Second)
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

func expireCurrentTurnActionDeadlineForTest(t *testing.T, table *nativeTableState) string {
	t.Helper()

	if table == nil || table.LatestCustodyState == nil {
		t.Fatal("expected latest custody state")
	}
	expiredAt := addMillis(nowISO(), -1)
	table.LatestCustodyState.ActionDeadlineAt = expiredAt
	if turnMenuMatchesTable(*table, table.PendingTurnMenu) {
		expiredMenu := cloneJSON(*table.PendingTurnMenu)
		if deliveredAt, err := parseISOTimestamp(expiredMenu.DeliveredAt); err == nil {
			if deadlineAt, err := parseISOTimestamp(expiredMenu.ActionDeadlineAt); err == nil {
				timeoutMS := int(deadlineAt.Sub(deliveredAt) / time.Millisecond)
				expiredMenu.DeliveredAt = addMillis(expiredAt, -timeoutMS)
			}
		}
		expiredMenu.ActionDeadlineAt = expiredAt
		table.PendingTurnMenu = &expiredMenu
	}
	return expiredAt
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

	waitBudget := 10 * time.Second
	for _, runtime := range runtimes {
		if runtime == nil {
			continue
		}
		protocolBudget := time.Duration(runtime.handProtocolTimeoutMS()) * time.Millisecond
		if candidate := protocolBudget + 5*time.Second; candidate > waitBudget {
			waitBudget = candidate
		}
	}
	deadline := time.Now().Add(waitBudget)
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

func resignHistoricalEventsForTest(t *testing.T, events []NativeSignedTableEvent, runtimes ...*meshRuntime) ([]NativeSignedTableEvent, string) {
	t.Helper()

	signers := map[string]*meshRuntime{}
	for _, runtime := range runtimes {
		if runtime == nil {
			continue
		}
		key := runtime.selfPeerID() + "|" + runtime.protocolIdentity.PublicKeyHex
		signers[key] = runtime
	}

	resigned := cloneJSON(events)
	prevHash := ""
	for index := range resigned {
		key := resigned[index].SenderPeerID + "|" + resigned[index].SenderProtocolPubkeyHex
		runtime, ok := signers[key]
		if !ok {
			t.Fatalf("missing signer for historical event %d peer=%s pubkey=%s", index, resigned[index].SenderPeerID, resigned[index].SenderProtocolPubkeyHex)
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
	table.Events, table.LastEventHash = resignHistoricalEventsForTest(t, table.Events, runtime)
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
	signatureHex, err := settlementcore.SignStructuredData(runtime.walletID.PrivateKeyHex, nativeActionAuthPayload(request.TableID, request.PlayerID, request.HandID, request.OptionID, request.PrevCustodyStateHash, request.ActionDeadlineAt, request.ChallengeAnchor, request.TranscriptRoot, request.TurnAnchorHash, request.Epoch, request.DecisionIndex, request.Action, request.SignedAt))
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
	signatureHex, err := settlementcore.SignStructuredData(runtime.walletID.PrivateKeyHex, nativeFundsAuthPayload(*request))
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

func TestValidateCustodyRegisterMessageIgnoresTimestampVariance(t *testing.T) {
	onchainIndexes := []int{0, 2}
	cosignerPubkeys := []string{
		"021111111111111111111111111111111111111111111111111111111111111111",
		"032222222222222222222222222222222222222222222222222222222222222222",
	}

	first, err := custodyRegisterMessage(onchainIndexes, cosignerPubkeys)
	if err != nil {
		t.Fatalf("first register message: %v", err)
	}
	time.Sleep(1100 * time.Millisecond)
	second, err := custodyRegisterMessage(onchainIndexes, cosignerPubkeys)
	if err != nil {
		t.Fatalf("second register message: %v", err)
	}
	if first == second {
		t.Fatal("expected register messages built at different times to differ")
	}
	if err := validateCustodyRegisterMessage(first, onchainIndexes, cosignerPubkeys); err != nil {
		t.Fatalf("validate first register message: %v", err)
	}
	if err := validateCustodyRegisterMessage(second, onchainIndexes, cosignerPubkeys); err != nil {
		t.Fatalf("validate second register message: %v", err)
	}
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
	txOutputs := make([]*wire.TxOut, 0, len(plan.AuthorizedOutputs))
	for _, output := range plan.AuthorizedOutputs {
		txOut, err := decodeBatchOutputTxOut(output)
		if err != nil {
			return "", err
		}
		txOutputs = append(txOutputs, txOut)
	}
	message, err := custodyRegisterMessage(custodyOnchainOutputIndexes(plan.AuthorizedOutputs), nil)
	if err != nil {
		return "", err
	}
	return custodyBuildProofPSBT(message, intentInputs, txOutputs, leafProofs, arkFields, locktime)
}

func syntheticCustodyTreeSignerPubkeys(runtime *meshRuntime, table nativeTableState, treeSignerIDs, proofSignerIDs []string) ([]string, error) {
	playerIDs := append([]string(nil), treeSignerIDs...)
	if len(playerIDs) == 0 {
		playerIDs = append(playerIDs, proofSignerIDs...)
	}
	pubkeys := make([]string, 0, len(playerIDs))
	for _, playerID := range uniqueSortedPlayerIDs(playerIDs) {
		pubkeyHex, err := runtime.walletPubkeyHexForPlayer(table, playerID)
		if err != nil {
			return nil, err
		}
		pubkeys = append(pubkeys, pubkeyHex)
	}
	if len(pubkeys) == 0 {
		pubkeys = append(pubkeys, runtime.walletID.PublicKeyHex)
	}
	sort.Strings(pubkeys)
	return pubkeys, nil
}

func buildSyntheticCustodyBatchResultForTest(runtime *meshRuntime, table nativeTableState, transitionHash string, inputs []custodyInputSpec, proofSignerIDs, treeSignerIDs []string, outputs []custodyBatchOutput) (*custodyBatchResult, error) {
	intentID := "synthetic-intent-" + transitionHash[:12]
	finalizedAt := nowISO()
	cosignerPubkeys, err := syntheticCustodyTreeSignerPubkeys(runtime, table, treeSignerIDs, proofSignerIDs)
	if err != nil {
		return nil, err
	}

	intentInputs, leafProofs, arkFields, locktime, err := custodyIntentInputs(inputs)
	if err != nil {
		return nil, err
	}
	txOutputs := make([]*wire.TxOut, 0, len(outputs))
	receivers := make([]arktree.Leaf, 0, len(outputs))
	for _, output := range outputs {
		if output.Onchain {
			return nil, errors.New("synthetic real mode does not support onchain custody outputs")
		}
		txOut, err := decodeBatchOutputTxOut(output)
		if err != nil {
			return nil, err
		}
		txOutputs = append(txOutputs, txOut)
		receivers = append(receivers, arktree.Leaf{
			Script:              output.Script,
			Amount:              uint64(output.AmountSats),
			CosignersPublicKeys: append([]string(nil), cosignerPubkeys...),
		})
	}
	message, err := custodyRegisterMessage(custodyOnchainOutputIndexes(outputs), cosignerPubkeys)
	if err != nil {
		return nil, err
	}
	proofPSBT, err := custodyBuildProofPSBT(message, intentInputs, txOutputs, leafProofs, arkFields, locktime)
	if err != nil {
		return nil, err
	}

	config, err := runtime.arkCustodyConfig()
	if err != nil {
		return nil, err
	}
	forfeitPubkey, err := compressedPubkeyFromHex(config.ForfeitPubkeyHex)
	if err != nil {
		return nil, err
	}
	batchExpiry := arklib.RelativeLocktime{Type: arklib.LocktimeTypeBlock, Value: 144}
	sweepScript, err := (&arkscript.CSVMultisigClosure{
		MultisigClosure: arkscript.MultisigClosure{PubKeys: []*btcec.PublicKey{forfeitPubkey}},
		Locktime:        batchExpiry,
	}).Script()
	if err != nil {
		return nil, err
	}
	sweepRoot := txscript.AssembleTaprootScriptTree(txscript.NewBaseTapLeaf(sweepScript)).RootNode.TapHash()
	batchScript, batchAmount, err := arktree.BuildBatchOutput(receivers, sweepRoot.CloneBytes())
	if err != nil {
		return nil, err
	}

	prevHash, err := chainhash.NewHashFromStr(transitionHash)
	if err != nil {
		prevHash = &chainhash.Hash{}
	}
	commitmentTx := wire.NewMsgTx(2)
	commitmentTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Hash: *prevHash, Index: 0},
		Sequence:         wire.MaxTxInSequenceNum,
	})
	commitmentTx.AddTxOut(&wire.TxOut{Value: batchAmount, PkScript: batchScript})
	commitmentPacket, err := psbt.NewFromUnsignedTx(commitmentTx)
	if err != nil {
		return nil, err
	}
	commitmentPSBT, err := commitmentPacket.B64Encode()
	if err != nil {
		return nil, err
	}

	vtxoTree, err := arktree.BuildVtxoTree(&wire.OutPoint{Hash: commitmentTx.TxHash(), Index: 0}, receivers, sweepRoot.CloneBytes(), batchExpiry)
	if err != nil {
		return nil, err
	}
	outputRefs, err := matchCustodyBatchOutputRefs(intentID, commitmentTx.TxHash().String(), finalizedAt, batchExpiry, outputs, vtxoTree)
	if err != nil {
		return nil, err
	}
	return &custodyBatchResult{
		ArkTxID:          commitmentTx.TxHash().String(),
		BatchExpiryType:  custodyBatchExpiryType(batchExpiry),
		BatchExpiryValue: batchExpiry.Value,
		CommitmentTx:     commitmentPSBT,
		FinalizedAt:      finalizedAt,
		IntentID:         intentID,
		OutputRefs:       outputRefs,
		ProofPSBT:        proofPSBT,
		VtxoTree:         mustSerializeTxTree(vtxoTree),
	}, nil
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

func countTableEventsByType(table nativeTableState, eventType string) int {
	count := 0
	for _, event := range table.Events {
		if stringValue(event.Body["type"]) == eventType {
			count++
		}
	}
	return count
}
