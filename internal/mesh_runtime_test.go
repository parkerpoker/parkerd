package parker

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	cfg "github.com/danieldresner/arkade_fun/internal/config"
	"github.com/danieldresner/arkade_fun/internal/game"
	"github.com/danieldresner/arkade_fun/internal/settlementcore"
)

func TestTableTrafficRedactsActiveHandSecretsAndPushesToJoiner(t *testing.T) {
	t.Parallel()

	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)

	joinedTable := mustReadNativeTable(t, guest, tableID)
	assertTableVisibleCards(t, joinedTable, guest.walletID.PlayerID)

	anonymous := mustFetchNativeTableWithoutAuth(t, host, tableID)
	assertTableVisibleCards(t, anonymous, "")

	fetched, err := guest.fetchRemoteTable(host.selfPeerURL(), tableID)
	if err != nil {
		t.Fatalf("fetch remote table: %v", err)
	}
	assertTableVisibleCards(t, *fetched, guest.walletID.PlayerID)

	if _, err := host.SendAction(tableID, game.Action{Type: game.ActionCall}); err != nil {
		t.Fatalf("host send action: %v", err)
	}
	afterHostAction := mustReadNativeTable(t, guest, tableID)
	if got := len(afterHostAction.ActiveHand.State.ActionLog); got != 1 {
		t.Fatalf("expected guest to receive pushed action log update, got %d entries", got)
	}
	assertTableVisibleCards(t, afterHostAction, guest.walletID.PlayerID)

	if _, err := guest.SendAction(tableID, game.Action{Type: game.ActionCheck}); err != nil {
		t.Fatalf("guest send action: %v", err)
	}
	afterGuestAction := mustReadNativeTable(t, guest, tableID)
	if got := len(afterGuestAction.ActiveHand.State.ActionLog); got != 2 {
		t.Fatalf("expected guest table to reflect remote action response, got %d entries", got)
	}
	assertTableVisibleCards(t, afterGuestAction, guest.walletID.PlayerID)
}

func TestGuestSendActionWaitsForSlowReplicationTargetsInParallel(t *testing.T) {
	t.Parallel()

	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")
	witness := newMeshTestRuntime(t, "witness")

	if _, err := host.BootstrapPeer(witness.selfPeerURL(), "", nil); err != nil {
		t.Fatalf("bootstrap witness peer: %v", err)
	}
	tableID, _ := createStartedTwoPlayerTable(t, host, guest, witness.selfPeerID())
	if _, err := host.SendAction(tableID, game.Action{Type: game.ActionCall}); err != nil {
		t.Fatalf("host send action: %v", err)
	}

	host.tableSyncSender = func(peerURL string, input nativeTableSyncRequest) error {
		time.Sleep(700 * time.Millisecond)
		return nil
	}

	startedAt := time.Now()
	if _, err := guest.SendAction(tableID, game.Action{Type: game.ActionCheck}); err != nil {
		t.Fatalf("guest send action: %v", err)
	}
	if elapsed := time.Since(startedAt); elapsed >= 1100*time.Millisecond {
		t.Fatalf("expected guest action response to wait on parallel, not sequential, replication; got %s", elapsed)
	}
}

func TestHandleActionRejectsForgedSeatOwnerSignature(t *testing.T) {
	t.Parallel()

	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)
	table := mustReadNativeTable(t, host, tableID)

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

	if _, err := host.handleActionFromPeer(forged); err == nil || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("expected forged action to be rejected with signature error, got %v", err)
	}
}

func TestHandleActionRejectsReplayedDecisionSignature(t *testing.T) {
	t.Parallel()

	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)
	table := mustReadNativeTable(t, host, tableID)

	staleCall, err := host.buildSignedActionRequest(table, game.Action{Type: game.ActionCall})
	if err != nil {
		t.Fatalf("build stale call request: %v", err)
	}

	if _, err := host.SendAction(tableID, game.Action{Type: game.ActionRaise, TotalSats: 200}); err != nil {
		t.Fatalf("host send raise: %v", err)
	}
	if _, err := guest.SendAction(tableID, game.Action{Type: game.ActionCall}); err != nil {
		t.Fatalf("guest send preflop call: %v", err)
	}
	if _, err := guest.SendAction(tableID, game.Action{Type: game.ActionBet, TotalSats: 100}); err != nil {
		t.Fatalf("guest send flop bet: %v", err)
	}

	if _, err := host.handleActionFromPeer(staleCall); err == nil || !strings.Contains(err.Error(), "decision mismatch") {
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
	if _, err := host.handleActionFromPeer(freshCall); err != nil {
		t.Fatalf("expected fresh call request to succeed, got %v", err)
	}
}

func TestFailoverAfterHandAbortSchedulesNextHand(t *testing.T) {
	t.Parallel()

	host := newMeshTestRuntime(t, "host")
	player := newMeshTestRuntime(t, "player")
	witness := newMeshTestRuntime(t, "witness")

	if _, err := host.BootstrapPeer(witness.selfPeerURL(), "", nil); err != nil {
		t.Fatalf("bootstrap witness peer: %v", err)
	}
	tableID, _ := createStartedTwoPlayerTable(t, host, player, witness.selfPeerID())

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
	if failedOver.ActiveHand != nil {
		t.Fatal("expected active hand to be cleared after HandAbort failover")
	}
	if failedOver.NextHandAt == "" {
		t.Fatal("expected failover to schedule the next hand")
	}
	if failedOver.Config.Status != "ready" {
		t.Fatalf("expected restored table status ready, got %q", failedOver.Config.Status)
	}
	if !tableHasEventType(failedOver, "HandAbort") {
		t.Fatal("expected failover to append HandAbort")
	}

	failedOver.LastHostHeartbeatAt = addMillis(nowISO(), -(nativeHostHeartbeatMS + 100))
	failedOver.NextHandAt = addMillis(nowISO(), -1)
	if err := witness.store.writeTable(&failedOver); err != nil {
		t.Fatalf("accelerate next hand: %v", err)
	}
	witness.Tick()

	restarted := mustReadNativeTable(t, witness, tableID)
	if restarted.ActiveHand == nil {
		t.Fatal("expected scheduled next hand to start on witness tick")
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
	if _, err := host.decodeResponseEnvelope(response, requestKey); err == nil || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("expected forged sync to be rejected with signature error, got %v", err)
	}
	after := mustReadNativeTable(t, guest, tableID)
	if after.CurrentEpoch != before.CurrentEpoch {
		t.Fatalf("expected guest table epoch to remain %d, got %d", before.CurrentEpoch, after.CurrentEpoch)
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
	assertTableVisibleCards(t, table, "")
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

func createStartedTwoPlayerTable(t *testing.T, host, guest *meshRuntime, witnessPeerIDs ...string) (string, string) {
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
	startNextHandForTest(t, host, tableID)
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

func assertTableVisibleCards(t *testing.T, table nativeTableState, visiblePlayerID string) {
	t.Helper()

	if table.ActiveHand == nil {
		t.Fatalf("expected active hand, status=%q publicState=%+v events=%d", table.Config.Status, table.PublicState, len(table.Events))
	}
	if table.ActiveHand.State.DeckSeedHex != "" {
		t.Fatalf("expected deck seed to be redacted, got %q", table.ActiveHand.State.DeckSeedHex)
	}
	if table.ActiveHand.State.Runout != (game.HoldemRunout{}) {
		t.Fatalf("expected runout to be redacted, got %+v", table.ActiveHand.State.Runout)
	}
	for index, player := range table.ActiveHand.State.Players {
		if player.HoleCards != [2]game.CardCode{} {
			t.Fatalf("expected player %d hole cards to be redacted, got %+v", index, player.HoleCards)
		}
	}

	switch visiblePlayerID {
	case "":
		if len(table.ActiveHand.HoleCardsByPlayerID) != 0 {
			t.Fatalf("expected no visible hole cards, got %+v", table.ActiveHand.HoleCardsByPlayerID)
		}
	default:
		cards, ok := table.ActiveHand.HoleCardsByPlayerID[visiblePlayerID]
		if !ok || len(cards) != 2 {
			t.Fatalf("expected visible cards for %s, got %+v", visiblePlayerID, table.ActiveHand.HoleCardsByPlayerID)
		}
		if len(table.ActiveHand.HoleCardsByPlayerID) != 1 {
			t.Fatalf("expected only one visible hole-card entry, got %+v", table.ActiveHand.HoleCardsByPlayerID)
		}
	}
}

func tableHasEventType(table nativeTableState, eventType string) bool {
	for _, event := range table.Events {
		if stringValue(event.Body["type"]) == eventType {
			return true
		}
	}
	return false
}
