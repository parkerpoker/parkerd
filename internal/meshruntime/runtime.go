package meshruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	arkclient "github.com/arkade-os/go-sdk/client"
	cfg "github.com/parkerpoker/parkerd/internal/config"
	"github.com/parkerpoker/parkerd/internal/game"
	"github.com/parkerpoker/parkerd/internal/settlementcore"
	"github.com/parkerpoker/parkerd/internal/tablecustody"
	walletpkg "github.com/parkerpoker/parkerd/internal/wallet"
)

type meshRuntime struct {
	backgroundTasks        int
	config                 cfg.RuntimeConfig
	arkTransportFactory    func() (arkclient.TransportClient, error)
	clearPeerURL           string
	custodyApprovalHook    func(request nativeCustodyApprovalRequest) error
	custodyArkVerify       func(refs []tablecustody.VTXORef, requireSpendable bool) error
	custodyBatchExecute    func(table nativeTableState, prevStateHash, transitionHash string, inputs []custodyInputSpec, proofSignerIDs, treeSignerIDs []string, outputs []custodyBatchOutput) (*custodyBatchResult, error)
	custodyExitExecute     func(profileName string, refs []tablecustody.VTXORef, destination string) (walletpkg.CustodyExitResult, error)
	custodyPSBTSign        func(profileName, tx string) (string, error)
	custodyRecoveryExecute func(profileName, signedPSBT string) (walletpkg.CustodyRecoveryResult, error)
	custodySignerAuth      map[string]custodySignerAuthorization
	custodySigners         map[string]walletpkg.CustodySignerSession
	handMessageSenderHook  func(peerURL string, input nativeHandMessageRequest) (nativeTableState, error)
	httpClient             *http.Client
	lastSyncAt             map[string]time.Time
	lastGuestContribution  map[string]guestContributionSendSnapshot
	listener               net.Listener
	mode                   string
	mu                     sync.Mutex
	peerInfoCache          map[string]nativeCachedPeerInfo
	peerIdentity           settlementcore.ScopedIdentity
	profileName            string
	profileStore           *walletpkg.ProfileStore
	protocolID             string
	protocolIdentity       settlementcore.ScopedIdentity
	protocolDrives         map[string]struct{}
	self                   nativePeerSelf
	server                 *http.Server
	fundsSenderHook        func(peerURL string, input nativeFundsRequest) (nativeFundsResponse, error)
	tableSyncSender        func(peerURL string, input nativeTableSyncRequest) error
	started                bool
	store                  *meshStore
	taskCond               *sync.Cond
	torService             *runtimeHiddenService
	transportKeyID         string
	transportPrivate       string
	transportPublic        string
	walletID               settlementcore.LocalIdentity
	walletRuntime          *walletpkg.Runtime
}

const (
	nativeTableAuthPlayerIDHeader  = "X-Parker-Player-Id"
	nativeTableAuthSignedAtHeader  = "X-Parker-Signed-At"
	nativeTableAuthSignatureHeader = "X-Parker-Signature"
	nativeTableFetchAuthMaxAge     = 15 * time.Second
	nativeTableSyncAuthMaxAge      = 15 * time.Second
	defaultCreatedTablesLimit      = 10
	maxCreatedTablesLimit          = 100
)

func newMeshRuntime(profileName string, config cfg.RuntimeConfig, mode string) (*meshRuntime, error) {
	if mode == "" {
		mode = "player"
	}
	store, err := newMeshStore(profileName, config)
	if err != nil {
		return nil, err
	}
	runtime := &meshRuntime{
		config:            config,
		custodySignerAuth: map[string]custodySignerAuthorization{},
		custodySigners:    map[string]walletpkg.CustodySignerSession{},
		httpClient:        &http.Client{Timeout: 5 * time.Second},
		lastSyncAt:        map[string]time.Time{},
		lastGuestContribution: map[string]guestContributionSendSnapshot{},
		mode:              mode,
		peerInfoCache:     map[string]nativeCachedPeerInfo{},
		profileName:       profileName,
		protocolDrives:    map[string]struct{}{},
		profileStore:      walletpkg.NewProfileStore(config.ProfileDir),
		store:             store,
		walletRuntime: walletpkg.NewRuntime(walletpkg.RuntimeConfig{
			ArkServerURL:      config.ArkServerURL,
			Network:           config.Network,
			NigiriDatadir:     config.NigiriDatadir,
			ProfileDir:        config.ProfileDir,
			RunDir:            config.RunDir,
			UseMockSettlement: config.UseMockSettlement,
		}),
	}
	runtime.taskCond = sync.NewCond(&runtime.mu)
	return runtime, nil
}

func (runtime *meshRuntime) Start() error {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()

	if runtime.started {
		return nil
	}
	if err := runtime.ensureBootstrapLocked("", ""); err != nil {
		return err
	}
	if err := runtime.startPeerServerLocked(); err != nil {
		return err
	}
	runtime.started = true
	return nil
}

func (runtime *meshRuntime) Close() error {
	runtime.mu.Lock()
	server := runtime.server
	runtime.server = nil
	listener := runtime.listener
	runtime.listener = nil
	torService := runtime.torService
	runtime.torService = nil
	runtime.clearPeerURL = ""
	runtime.self.Peer.PeerURL = ""
	runtime.started = false
	runtime.mu.Unlock()

	var joined error
	if torService != nil {
		joined = errors.Join(joined, runtime.unregisterTorHiddenService(torService))
	}
	if server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		joined = errors.Join(joined, server.Shutdown(ctx))
	}
	if listener != nil {
		joined = errors.Join(joined, listener.Close())
	}
	runtime.waitForBackgroundTasks()
	if runtime.store != nil {
		joined = errors.Join(joined, runtime.store.close())
	}
	return joined
}

func (runtime *meshRuntime) isRunning() bool {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return runtime.started
}

func (runtime *meshRuntime) beginBackgroundTask() bool {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if !runtime.started {
		return false
	}
	runtime.backgroundTasks++
	return true
}

func (runtime *meshRuntime) endBackgroundTask() {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	runtime.backgroundTasks--
	if runtime.backgroundTasks == 0 {
		runtime.taskCond.Broadcast()
	}
}

func (runtime *meshRuntime) beginProtocolDrive(tableID string) bool {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if !runtime.started {
		return false
	}
	if _, exists := runtime.protocolDrives[tableID]; exists {
		return false
	}
	runtime.protocolDrives[tableID] = struct{}{}
	runtime.backgroundTasks++
	return true
}

func (runtime *meshRuntime) endProtocolDrive(tableID string) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	delete(runtime.protocolDrives, tableID)
	runtime.backgroundTasks--
	if runtime.backgroundTasks == 0 {
		runtime.taskCond.Broadcast()
	}
}

func (runtime *meshRuntime) waitForBackgroundTasks() {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	for runtime.backgroundTasks > 0 {
		runtime.taskCond.Wait()
	}
}

func (runtime *meshRuntime) hostHeartbeatIntervalMS() int {
	return nativeHostHeartbeatMS
}

func (runtime *meshRuntime) hostFailureTimeoutMS() int {
	if runtime != nil && !runtime.config.UseMockSettlement {
		return 120_000
	}
	return nativeHostFailureMS
}

func (runtime *meshRuntime) handProtocolTimeoutMS() int {
	if runtime != nil && !runtime.config.UseMockSettlement {
		// Real Ark custody rounds add network and signer coordination latency to
		// every protocol phase transition, so keep more budget than synthetic mode.
		return 20_000
	}
	return nativeHandProtocolTimeoutMS
}

func (runtime *meshRuntime) actionTimeoutMS() int {
	if runtime != nil && !runtime.config.UseMockSettlement {
		return 16_000
	}
	return nativeActionTimeoutMS
}

func (runtime *meshRuntime) actionTimeoutMSForTable(table nativeTableState) int {
	if table.Config.ActionTimeoutMS > 0 {
		return table.Config.ActionTimeoutMS
	}
	return runtime.actionTimeoutMS()
}

func (runtime *meshRuntime) handProtocolTimeoutMSForTable(table nativeTableState) int {
	if table.Config.HandProtocolTimeoutMS > 0 {
		return table.Config.HandProtocolTimeoutMS
	}
	return runtime.handProtocolTimeoutMS()
}

func (runtime *meshRuntime) nextHandDelayMSForTable(table nativeTableState) int {
	if table.Config.NextHandDelayMS > 0 {
		return table.Config.NextHandDelayMS
	}
	return nativeNextHandDelayMS
}

func (runtime *meshRuntime) Bootstrap(nickname, walletNsec string) (map[string]any, error) {
	runtime.mu.Lock()
	if err := runtime.ensureBootstrapLocked(nickname, walletNsec); err != nil {
		runtime.mu.Unlock()
		return nil, err
	}
	if err := runtime.startPeerServerLocked(); err != nil {
		runtime.mu.Unlock()
		return nil, err
	}
	self := runtime.self
	walletID := runtime.walletID
	peerIdentity := runtime.peerIdentity
	protocolIdentity := runtime.protocolIdentity
	runtime.started = true
	runtime.mu.Unlock()

	wallet := walletpkg.WalletSummary{}
	if runtime.config.UseMockSettlement {
		var err error
		wallet, err = runtime.walletSummary()
		if err != nil {
			return nil, err
		}
	}
	return map[string]any{
		"mesh": map[string]any{
			"peerId":               self.Peer.PeerID,
			"peerPublicKeyHex":     peerIdentity.PublicKeyHex,
			"protocolId":           protocolIdentity.ID,
			"protocolPublicKeyHex": protocolIdentity.PublicKeyHex,
			"wallet":               wallet,
			"walletPlayerId":       walletID.PlayerID,
		},
	}, nil
}

func (runtime *meshRuntime) Tick() {
	_ = runtime.Start()

	tableIDs, err := runtime.store.listTableIDs()
	if err != nil {
		return
	}
	for _, tableID := range tableIDs {
		table, err := runtime.store.readTable(tableID)
		if err != nil || table == nil {
			continue
		}
		selfPeerID := runtime.selfPeerID()
		if table.CurrentHost.Peer.PeerID == selfPeerID {
			if elapsedMillis(table.LastHostHeartbeatAt) >= int64(runtime.hostHeartbeatIntervalMS()) {
				var replicateView *nativeTableState
				_ = runtime.store.withTableLock(tableID, func() error {
					latest, err := runtime.store.readTable(tableID)
					if err != nil || latest == nil {
						return err
					}
					if latest.CurrentHost.Peer.PeerID != selfPeerID {
						return nil
					}
					latest.LastHostHeartbeatAt = nowISO()
					// Persist the heartbeat before any potentially slow protocol work so
					// witnesses polling the host do not mistake an in-flight real-mode
					// settlement step for host failure.
					if err := runtime.persistLocalTable(latest, false); err != nil {
						return err
					}
					if latest.NextHandAt != "" && elapsedMillis(latest.NextHandAt) >= 0 {
						if err := runtime.startNextHandLocked(latest); err != nil {
							debugMeshf("host tick start next hand failed table=%s nextHandAt=%s err=%v", tableID, latest.NextHandAt, err)
							return err
						}
					}
					if err := runtime.advanceHandProtocolLocked(latest); err != nil {
						debugMeshf("host tick advance hand protocol failed table=%s err=%v", tableID, err)
						return err
					}
					if err := runtime.persistLocalTable(latest, true); err != nil {
						return err
					}
					snapshot := cloneJSON(*latest)
					replicateView = &snapshot
					return nil
				})
				if replicateView != nil {
					runtime.replicateTable(*replicateView)
				}
			}
			if _, pendingRequest, err := runtime.pendingEmergencyExitOperation(tableID); err == nil && pendingRequest != nil {
				if _, err := runtime.completeFunds(tableID, string(tablecustody.TransitionKindEmergencyExit), "exited"); err != nil {
					debugMeshf("pending emergency exit retry failed table=%s err=%v", tableID, err)
				}
			}
			continue
		}

		if runtime.shouldPollHost(tableID) && table.CurrentHost.Peer.PeerURL != "" {
			if remote, err := runtime.fetchRemoteTable(table.CurrentHost.Peer.PeerURL, tableID); err == nil && remote != nil {
				if err := runtime.acceptRemoteTable(*remote); err == nil {
					runtime.lastSyncAt[tableID] = time.Now()
					table, _ = runtime.store.readTable(tableID)
				} else {
					debugMeshf("accept remote table during host poll failed table=%s err=%v", tableID, err)
				}
			} else if err != nil {
				debugMeshf("fetch remote table during host poll failed table=%s err=%v", tableID, err)
			}
		}

		if table != nil &&
			table.CurrentHost.Peer.PeerID != runtime.selfPeerID() &&
			table.ActiveHand != nil &&
			!game.PhaseAllowsActions(table.ActiveHand.State.Phase) {
			runtime.driveLocalHandProtocol(tableID)
			table, _ = runtime.store.readTable(tableID)
		}

		if table != nil && protocolDeadlineExpired(*table) && runtime.shouldHandleFailover(*table) {
			debugMeshf("protocol failover check table=%s phase=%v deadline=%s host=%s", tableID, table.ActiveHand.State.Phase, table.ActiveHand.Cards.PhaseDeadlineAt, table.CurrentHost.Peer.PeerID)
			if err := runtime.forceProtocolFailover(tableID, fmt.Sprintf("protocol deadline expired during %s", table.ActiveHand.State.Phase)); err == nil {
				table, _ = runtime.store.readTable(tableID)
			}
		}
		if table != nil && elapsedMillis(table.LastHostHeartbeatAt) > int64(runtime.hostFailureTimeoutMS()) && runtime.shouldHandleFailover(*table) {
			_ = runtime.failoverTable(tableID, "missed host heartbeats")
		}
		if _, pendingRequest, err := runtime.pendingEmergencyExitOperation(tableID); err == nil && pendingRequest != nil {
			if _, err := runtime.completeFunds(tableID, string(tablecustody.TransitionKindEmergencyExit), "exited"); err != nil {
				debugMeshf("pending emergency exit retry failed table=%s err=%v", tableID, err)
			}
		}
	}
}

func (runtime *meshRuntime) CurrentState() (map[string]any, error) {
	wallet, err := runtime.walletSummary()
	if err != nil {
		return nil, err
	}
	mesh, err := runtime.meshState()
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"mesh":   rawJSONMap(mesh),
		"wallet": rawJSONMap(wallet),
	}, nil
}

func (runtime *meshRuntime) QuickState() (map[string]any, error) {
	mesh, err := runtime.meshState()
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"mesh": rawJSONMap(mesh),
	}, nil
}

func (runtime *meshRuntime) walletSummary() (walletpkg.WalletSummary, error) {
	base, err := runtime.walletRuntime.GetWallet(runtime.profileName)
	if err != nil {
		return walletpkg.WalletSummary{}, err
	}
	funds, err := runtime.store.readTableFunds()
	if err != nil {
		return walletpkg.WalletSummary{}, err
	}
	tableLocked := 0
	pendingExit := 0
	for _, table := range funds.Tables {
		switch table.Status {
		case "pending-lock":
			tableLocked += lockedFundsAmount(table)
		case "locked":
			tableLocked += lockedFundsAmount(table)
		case "pending-exit":
			pendingExit += table.CashoutSats
		}
	}
	base.WalletSpendableSats = base.AvailableSats
	base.TableLockedSats = tableLocked
	base.PendingExitSats = pendingExit
	base.AvailableSats = maxInt(base.WalletSpendableSats-tableLocked-pendingExit, 0)
	base.TotalSats = maxInt(base.WalletSpendableSats, base.AvailableSats+tableLocked+pendingExit)
	return base, nil
}

func lockedFundsAmount(entry NativeTableFundsEntry) int {
	if reserved := sumVTXORefs(entry.ReservedFundingRefs); reserved > 0 {
		return reserved
	}
	return maxInt(entry.BuyInSats, latestStackAmountFromFunds(entry))
}

func sumVTXORefs(refs []tablecustody.VTXORef) int {
	total := 0
	for _, ref := range refs {
		total += ref.AmountSats
	}
	return total
}

func stackClaimBackedAmount(claim tablecustody.StackClaim) int {
	return claim.AmountSats + claim.ReservedFeeSats
}

func fundingRefKey(ref tablecustody.VTXORef) string {
	return fmt.Sprintf("%s:%d", ref.TxID, ref.VOut)
}

func (runtime *meshRuntime) reservedFundingRefKeys() (map[string]struct{}, error) {
	funds, err := runtime.store.readTableFunds()
	if err != nil {
		return nil, err
	}
	keys := map[string]struct{}{}
	for _, entry := range funds.Tables {
		switch entry.Status {
		case "pending-lock", "locked":
			for _, ref := range entry.ReservedFundingRefs {
				keys[fundingRefKey(ref)] = struct{}{}
			}
		}
	}
	return keys, nil
}

func (runtime *meshRuntime) selectJoinFundingBundle(buyInSats int) (walletpkg.CustodyFundingBundle, error) {
	targetSats, err := runtime.requiredJoinFundingSats(buyInSats)
	if err != nil {
		return walletpkg.CustodyFundingBundle{}, err
	}
	refs, err := runtime.walletRuntime.ListVtxoRefs(runtime.profileName)
	if err != nil {
		return walletpkg.CustodyFundingBundle{}, err
	}
	reserved, err := runtime.reservedFundingRefKeys()
	if err != nil {
		return walletpkg.CustodyFundingBundle{}, err
	}
	selected := make([]tablecustody.VTXORef, 0, len(refs))
	total := 0
	for _, ref := range refs {
		if _, ok := reserved[fundingRefKey(ref)]; ok {
			continue
		}
		selected = append(selected, ref)
		total += ref.AmountSats
		if total >= targetSats {
			break
		}
	}
	if total < targetSats {
		return walletpkg.CustodyFundingBundle{}, fmt.Errorf("insufficient unreserved spendable vtxos: have %d need %d", total, targetSats)
	}
	return walletpkg.CustodyFundingBundle{
		PlayerID:  runtime.walletID.PlayerID,
		Refs:      selected,
		TotalSats: total,
	}, nil
}

func (runtime *meshRuntime) reserveJoinFundingRefs(tableID string, buyInSats int, refs []tablecustody.VTXORef) error {
	funds, err := runtime.store.readTableFunds()
	if err != nil {
		return err
	}
	entry := funds.Tables[tableID]
	entry.BuyInSats = buyInSats
	entry.LastUpdatedAt = nowISO()
	entry.PlayerID = runtime.walletID.PlayerID
	entry.ReservedFundingRefs = append([]tablecustody.VTXORef(nil), refs...)
	entry.Status = "pending-lock"
	entry.TableID = tableID
	if entry.Operations == nil {
		entry.Operations = []NativeTableFundsOperation{}
	}
	funds.Tables[tableID] = entry
	return runtime.store.writeTableFunds(funds)
}

func (runtime *meshRuntime) releasePendingFundingReservation(tableID string) error {
	funds, err := runtime.store.readTableFunds()
	if err != nil {
		return err
	}
	entry, ok := funds.Tables[tableID]
	if !ok || entry.Status != "pending-lock" {
		return nil
	}
	if len(entry.Operations) == 0 && entry.CheckpointHash == "" {
		delete(funds.Tables, tableID)
		return runtime.store.writeTableFunds(funds)
	}
	entry.ReservedFundingRefs = nil
	entry.Status = ""
	entry.LastUpdatedAt = nowISO()
	funds.Tables[tableID] = entry
	return runtime.store.writeTableFunds(funds)
}

func (runtime *meshRuntime) meshState() (NativeMeshRuntimeState, error) {
	peers, err := runtime.knownPeers()
	if err != nil {
		return NativeMeshRuntimeState{}, err
	}
	tableIDs, err := runtime.store.listTableIDs()
	if err != nil {
		return NativeMeshRuntimeState{}, err
	}
	tables := make([]NativeTableSummary, 0, len(tableIDs))
	for _, tableID := range tableIDs {
		table, err := runtime.store.readTable(tableID)
		if err != nil || table == nil {
			continue
		}
		tables = append(tables, runtime.tableSummary(*table))
	}
	sort.Slice(tables, func(i, j int) bool { return tables[i].TableID < tables[j].TableID })

	runtime.mu.Lock()
	peer := runtime.self
	runtime.mu.Unlock()

	publicAds, err := runtime.store.readPublicAds()
	if err != nil {
		return NativeMeshRuntimeState{}, err
	}
	publicTables := make([]NativeAdvertisement, 0, len(publicAds))
	for _, ad := range publicAds {
		publicTables = append(publicTables, ad)
	}
	sort.Slice(publicTables, func(i, j int) bool { return publicTables[i].TableID < publicTables[j].TableID })

	return NativeMeshRuntimeState{
		FundsWarnings: []map[string]any{},
		Mode:          runtime.mode,
		Peer: NativeMeshPeerState{
			PeerID:         peer.Peer.PeerID,
			PeerURL:        peer.Peer.PeerURL,
			ProtocolID:     runtime.protocolID,
			WalletPlayerID: runtime.walletID.PlayerID,
		},
		Peers:        peers,
		PublicTables: publicTables,
		Tables:       tables,
	}, nil
}

func (runtime *meshRuntime) NetworkPeers() ([]NativePeerAddress, error) {
	return runtime.knownPeers()
}

func (runtime *meshRuntime) BootstrapPeer(peerURL, alias string, roles []string) (NativePeerAddress, error) {
	if strings.TrimSpace(peerURL) == "" {
		return NativePeerAddress{}, errors.New("peer URL is required")
	}
	info, err := runtime.fetchPeerInfo(peerURL)
	peer := NativePeerAddress{
		LastSeenAt: nowISO(),
		PeerID:     provisionalPeerID(peerURL),
		PeerURL:    peerURL,
		Roles:      append([]string{}, roles...),
	}
	if alias != "" {
		peer.Alias = alias
	}
	if err == nil {
		peer = info.Peer
	}
	if alias != "" {
		peer.Alias = alias
	}
	if len(roles) > 0 {
		peer.Roles = roles
	}
	peer.LastSeenAt = nowISO()
	if err := runtime.saveKnownPeer(peer); err != nil {
		return NativePeerAddress{}, err
	}
	return peer, nil
}

func provisionalPeerID(peerURL string) string {
	sum := sha256.Sum256([]byte(peerURL))
	return "peer-" + fmt.Sprintf("%x", sum[:])[:20]
}

func (runtime *meshRuntime) preferredInviteHostPeerURL(hostPeerID, hostPeerURL string) string {
	if strings.TrimSpace(hostPeerID) == "" {
		return hostPeerURL
	}
	knownPeers, err := runtime.knownPeers()
	if err != nil {
		return hostPeerURL
	}
	for _, peer := range knownPeers {
		if peer.PeerID == hostPeerID && strings.TrimSpace(peer.PeerURL) != "" {
			return peer.PeerURL
		}
	}
	return hostPeerURL
}

func (runtime *meshRuntime) resolveJoinHostPeerURL(tableID, hostPeerID, hostPeerURL string) string {
	candidates := make([]string, 0, 4)
	seen := map[string]struct{}{}
	appendCandidate := func(candidate string) {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			return
		}
		if _, ok := seen[candidate]; ok {
			return
		}
		seen[candidate] = struct{}{}
		candidates = append(candidates, candidate)
	}

	appendCandidate(runtime.preferredInviteHostPeerURL(hostPeerID, hostPeerURL))
	appendCandidate(hostPeerURL)

	if strings.TrimSpace(hostPeerID) != "" {
		knownPeers, err := runtime.knownPeers()
		if err == nil {
			for _, peer := range knownPeers {
				if peer.PeerID == hostPeerID {
					appendCandidate(peer.PeerURL)
				}
			}
		}
	}

	for _, candidate := range candidates {
		remoteTable, err := runtime.fetchRemoteTable(candidate, tableID)
		if err == nil && remoteTable != nil && remoteTable.Config.TableID == tableID {
			return candidate
		}
	}

	if len(candidates) > 0 {
		return candidates[0]
	}
	return ""
}

func (runtime *meshRuntime) primeInvitedRemoteTable(tableID, hostPeerID, hostPeerURL, inviteCode string) error {
	if strings.TrimSpace(tableID) == "" || strings.TrimSpace(hostPeerURL) == "" {
		return errors.New("invite is missing host or table information")
	}
	if existing, err := runtime.store.readTable(tableID); err == nil && existing != nil {
		return nil
	}
	remoteTable, err := runtime.fetchRemoteTable(hostPeerURL, tableID)
	if err != nil {
		return err
	}
	if remoteTable == nil {
		return fmt.Errorf("table %s not found", tableID)
	}
	if remoteTable.Config.TableID != tableID {
		return errors.New("invite bootstrap returned a different table id")
	}
	if strings.TrimSpace(hostPeerID) != "" && remoteTable.CurrentHost.Peer.PeerID != hostPeerID {
		return errors.New("invite bootstrap host peer id mismatch")
	}
	if strings.TrimSpace(remoteTable.CurrentHost.Peer.PeerURL) != "" && remoteTable.CurrentHost.Peer.PeerURL != hostPeerURL {
		return errors.New("invite bootstrap host peer url mismatch")
	}
	if err := runtime.validateAcceptedHandTranscript(*remoteTable); err != nil {
		return err
	}
	if err := runtime.validateAcceptedPublicReplay(*remoteTable); err != nil {
		return err
	}
	if err := runtime.validateAcceptedHistoricalLedger(nil, *remoteTable); err != nil {
		return err
	}
	remoteTable.InviteCode = firstNonEmptyString(remoteTable.InviteCode, inviteCode)
	if err := runtime.store.writeTable(remoteTable); err != nil {
		return err
	}
	if err := runtime.store.rewriteEvents(remoteTable.Config.TableID, remoteTable.Events); err != nil {
		return err
	}
	if err := runtime.store.rewriteSnapshots(remoteTable.Config.TableID, remoteTable.Snapshots); err != nil {
		return err
	}
	if remoteTable.CurrentHost.Peer.PeerURL != "" {
		_ = runtime.saveKnownPeer(remoteTable.CurrentHost.Peer)
	}
	for _, witness := range remoteTable.Witnesses {
		if witness.Peer.PeerURL != "" {
			_ = runtime.saveKnownPeer(witness.Peer)
		}
	}
	debugMeshf("primed invited table table=%s host_peer_id=%s host_peer_url=%s", tableID, remoteTable.CurrentHost.Peer.PeerID, remoteTable.CurrentHost.Peer.PeerURL)
	return nil
}

func (runtime *meshRuntime) primeLocalJoinSeat(tableID string, join nativeJoinRequest) error {
	table, err := runtime.store.readTable(tableID)
	if err != nil {
		return err
	}
	if table == nil {
		return fmt.Errorf("table %s not found", tableID)
	}
	seat := nativeSeatRecord{
		NativeSeatedPlayer: NativeSeatedPlayer{
			ArkAddress:        join.ArkAddress,
			BuyInSats:         join.BuyInSats,
			FundingRefs:       append([]tablecustody.VTXORef(nil), join.FundingRefs...),
			Nickname:          join.Nickname,
			PeerID:            join.Peer.PeerID,
			PlayerID:          join.WalletPlayerID,
			ProtocolPubkeyHex: join.Peer.ProtocolPubkeyHex,
			SeatIndex:         len(table.Seats),
			Status:            "pending-join",
			WalletPubkeyHex:   join.WalletPubkeyHex,
		},
		PeerURL:     join.Peer.PeerURL,
		ProfileName: join.ProfileName,
	}
	for index := range table.Seats {
		if table.Seats[index].PlayerID != join.WalletPlayerID {
			continue
		}
		seat.SeatIndex = table.Seats[index].SeatIndex
		table.Seats[index] = seat
		goto persist
	}
	table.Seats = append(table.Seats, seat)
persist:
	if err := runtime.store.writeTable(table); err != nil {
		return err
	}
	if err := runtime.store.rewriteEvents(table.Config.TableID, table.Events); err != nil {
		return err
	}
	if err := runtime.store.rewriteSnapshots(table.Config.TableID, table.Snapshots); err != nil {
		return err
	}
	debugMeshf("primed local join seat table=%s player=%s peer_id=%s", tableID, join.WalletPlayerID, join.Peer.PeerID)
	return nil
}

func (runtime *meshRuntime) cleanupPrimedLocalJoinSeat(tableID, playerID string) error {
	table, err := runtime.store.readTable(tableID)
	if err != nil || table == nil {
		return err
	}
	nextSeats := make([]nativeSeatRecord, 0, len(table.Seats))
	changed := false
	for _, seat := range table.Seats {
		if seat.PlayerID == playerID && seat.Status == "pending-join" {
			changed = true
			continue
		}
		nextSeats = append(nextSeats, seat)
	}
	if !changed {
		return nil
	}
	for index := range nextSeats {
		nextSeats[index].SeatIndex = index
	}
	table.Seats = nextSeats
	if table.Config.OccupiedSeats > len(nextSeats) {
		table.Config.OccupiedSeats = len(nextSeats)
	}
	if err := runtime.store.writeTable(table); err != nil {
		return err
	}
	if err := runtime.store.rewriteEvents(table.Config.TableID, table.Events); err != nil {
		return err
	}
	if err := runtime.store.rewriteSnapshots(table.Config.TableID, table.Snapshots); err != nil {
		return err
	}
	debugMeshf("cleaned primed local join seat table=%s player=%s", tableID, playerID)
	return nil
}

func defaultDealerlessBlinds(minOffchainSats int, input map[string]any) (int, int) {
	smallBlind := 50
	bigBlind := 100
	if minOffchainSats <= 0 {
		return smallBlind, bigBlind
	}
	if _, ok := input["bigBlindSats"]; !ok {
		bigBlind = maxInt(bigBlind, minOffchainSats)
	}
	if _, ok := input["smallBlindSats"]; !ok {
		smallBlind = maxInt(smallBlind, (minOffchainSats+1)/2)
	}
	if smallBlind >= bigBlind {
		bigBlind = maxInt(bigBlind, smallBlind*2)
	}
	return smallBlind, bigBlind
}

func validateDealerlessBlindPot(smallBlindSats, bigBlindSats, minOffchainSats int) error {
	if minOffchainSats <= 0 {
		return nil
	}
	openingPot := smallBlindSats + bigBlindSats
	if openingPot < minOffchainSats {
		return fmt.Errorf("real Ark custody tables require opening blind pot >= %d sats; got %d", minOffchainSats, openingPot)
	}
	return nil
}

func (runtime *meshRuntime) CreateTable(input map[string]any) (map[string]any, error) {
	if err := runtime.Start(); err != nil {
		return nil, err
	}
	if requestedSeatCount := intFromMap(input, "seatCount", 2); requestedSeatCount > 2 {
		return nil, errors.New("dealerless Ark custody tables currently support at most 2 seats")
	}
	tableID := randomUUID()
	visibility, err := tableVisibilityFromInput(input)
	if err != nil {
		return nil, err
	}
	now := nowISO()
	inviteCode, err := encodeInvite(tableID, runtime.config.Network, runtime.selfPeerID(), runtime.selfPeerURL())
	if err != nil {
		return nil, err
	}
	minOffchainSats := 0
	if !runtime.config.UseMockSettlement {
		arkConfig, err := runtime.arkCustodyConfig()
		if err != nil {
			return nil, err
		}
		minOffchainSats = int(arkConfig.DustSats)
	}
	defaultSmallBlind, defaultBigBlind := defaultDealerlessBlinds(minOffchainSats, input)
	config := NativeMeshTableConfig{
		ActionTimeoutMS:           runtime.actionTimeoutMS(),
		BigBlindSats:              intFromMap(input, "bigBlindSats", defaultBigBlind),
		BuyInMaxSats:              intFromMap(input, "buyInMaxSats", 4000),
		BuyInMinSats:              intFromMap(input, "buyInMinSats", 4000),
		CreatedAt:                 now,
		DealerMode:                nativeDealerMode,
		HandProtocolTimeoutMS:     runtime.handProtocolTimeoutMS(),
		HostPeerID:                runtime.selfPeerID(),
		HostPlaysAllowed:          true,
		Name:                      stringFromMap(input, "name", "Parker Table"),
		NetworkID:                 runtime.config.Network,
		NextHandDelayMS:           nativeNextHandDelayMS,
		OccupiedSeats:             0,
		ProtocolVersion:           nativeProtocolVersion,
		PublicSpectatorDelayHands: 1,
		SeatCount:                 2,
		SmallBlindSats:            intFromMap(input, "smallBlindSats", defaultSmallBlind),
		SpectatorsAllowed:         boolFromMapDefault(input, "spectatorsAllowed", true),
		Status:                    "announced",
		TableID:                   tableID,
		Visibility:                visibility,
	}
	if err := validateDealerlessBlindPot(config.SmallBlindSats, config.BigBlindSats, minOffchainSats); err != nil {
		return nil, err
	}
	witnessPeerIDs := stringSliceFromMap(input, "witnessPeerIds")
	witnesses := make([]nativeKnownParticipant, 0, len(witnessPeerIDs))
	knownPeers, err := runtime.knownPeers()
	if err != nil {
		return nil, err
	}
	knownByID := map[string]NativePeerAddress{}
	for _, peer := range knownPeers {
		knownByID[peer.PeerID] = peer
	}
	for _, witnessPeerID := range witnessPeerIDs {
		if peer, ok := knownByID[witnessPeerID]; ok {
			witnesses = append(witnesses, nativeKnownParticipant{Peer: peer})
		}
	}
	table := &nativeTableState{
		Config:              config,
		CurrentEpoch:        1,
		CurrentHost:         nativeKnownParticipant{ProfileName: runtime.profileName, Peer: runtime.self.Peer},
		Events:              []NativeSignedTableEvent{},
		HostProfileName:     runtime.profileName,
		InviteCode:          inviteCode,
		LastHostHeartbeatAt: now,
		LastSyncedAt:        now,
		Seats:               []nativeSeatRecord{},
		Snapshots:           []NativeCooperativeTableSnapshot{},
		Witnesses:           witnesses,
	}
	if visibility == "public" {
		ad, err := runtime.buildAdvertisement(*table)
		if err != nil {
			return nil, err
		}
		table.Advertisement = &ad
	}
	if err := runtime.appendEvent(table, map[string]any{
		"type":  "TableAnnounce",
		"table": rawJSONMap(config),
	}); err != nil {
		return nil, err
	}
	if err := runtime.persistAndReplicate(table, true); err != nil {
		return nil, err
	}
	if err := runtime.saveCreatedTableReference(*table, inviteCode); err != nil {
		return nil, err
	}
	return map[string]any{
		"inviteCode": inviteCode,
		"table":      rawJSONMap(config),
	}, nil
}

func (runtime *meshRuntime) CreatedTables(cursor string, limit int) (NativeCreatedTablesPage, error) {
	if err := runtime.Start(); err != nil {
		return NativeCreatedTablesPage{}, err
	}

	profile, err := runtime.loadProfileState()
	if err != nil {
		return NativeCreatedTablesPage{}, err
	}

	entries := make([]NativeCreatedTableEntry, 0, len(profile.MeshTables))
	for _, reference := range profile.MeshTables {
		if !reference.CreatedByMe {
			continue
		}
		entry, err := runtime.createdTableEntry(reference)
		if err != nil {
			if errors.Is(err, errStoredProtocolVersionMismatch) || strings.Contains(err.Error(), "protocol version mismatch") {
				continue
			}
			return NativeCreatedTablesPage{}, err
		}
		entries = append(entries, entry)
	}

	sort.Slice(entries, func(i, j int) bool {
		return compareCreatedTableEntries(entries[i], entries[j]) < 0
	})

	cursorValue, err := decodeCreatedTablesCursor(cursor)
	if err != nil {
		return NativeCreatedTablesPage{}, err
	}
	resolvedLimit := clampCreatedTablesLimit(limit)
	page := make([]NativeCreatedTableEntry, 0, resolvedLimit)
	nextCursor := ""

	for _, entry := range entries {
		if cursorValue != nil && compareCreatedTableEntryWithCursor(entry, *cursorValue) <= 0 {
			continue
		}
		if len(page) == resolvedLimit {
			lastIncluded := page[len(page)-1]
			nextCursor, err = encodeCreatedTablesCursor(createdTablesCursor{
				CreatedAt: lastIncluded.Config.CreatedAt,
				TableID:   lastIncluded.Config.TableID,
			})
			if err != nil {
				return NativeCreatedTablesPage{}, err
			}
			break
		}
		page = append(page, entry)
	}

	return NativeCreatedTablesPage{
		Items:      page,
		NextCursor: nextCursor,
	}, nil
}

func (runtime *meshRuntime) AnnounceTable(tableID string) (map[string]any, error) {
	table, err := runtime.requireLocalTable(tableID)
	if err != nil {
		return nil, err
	}
	if table.Advertisement == nil {
		ad, err := runtime.buildAdvertisement(*table)
		if err != nil {
			return nil, err
		}
		table.Advertisement = &ad
	}
	if err := runtime.persistAndReplicate(table, true); err != nil {
		return nil, err
	}
	return rawJSONMap(*table.Advertisement), nil
}

func (runtime *meshRuntime) JoinTable(inviteCode string, buyInSats int) (NativeMeshTableView, error) {
	if err := runtime.Start(); err != nil {
		return NativeMeshTableView{}, err
	}
	invite, err := decodeInvite(inviteCode)
	if err != nil {
		return NativeMeshTableView{}, err
	}
	if strings.TrimSpace(stringValue(invite["protocolVersion"])) != nativeProtocolVersion {
		return NativeMeshTableView{}, errors.New("invite protocol version mismatch")
	}
	tableID := stringValue(invite["tableId"])
	hostPeerID := stringValue(invite["hostPeerId"])
	hostPeerURL := runtime.resolveJoinHostPeerURL(tableID, hostPeerID, stringValue(invite["hostPeerUrl"]))
	debugMeshf("join table invite table=%s host_peer_id=%s host_peer_url=%s resolved_host_peer_url=%s", tableID, hostPeerID, stringValue(invite["hostPeerUrl"]), hostPeerURL)
	if tableID == "" || hostPeerURL == "" {
		return NativeMeshTableView{}, errors.New("invite is missing host or table information")
	}
	if existing, err := runtime.store.readTable(tableID); err == nil && existing != nil {
		if seat, ok := seatRecordForPlayer(*existing, runtime.walletID.PlayerID); ok && seat.Status != "pending-join" {
			return runtime.localTableView(*existing), nil
		}
	}
	if err := runtime.primeInvitedRemoteTable(tableID, hostPeerID, hostPeerURL, inviteCode); err != nil {
		return NativeMeshTableView{}, err
	}

	profile, err := runtime.loadProfileState()
	if err != nil {
		return NativeMeshTableView{}, err
	}
	wallet, err := runtime.walletSummary()
	if err != nil {
		return NativeMeshTableView{}, err
	}
	requiredFundingSats, err := runtime.requiredJoinFundingSats(buyInSats)
	if err != nil {
		return NativeMeshTableView{}, err
	}
	if wallet.AvailableSats < requiredFundingSats {
		return NativeMeshTableView{}, fmt.Errorf("insufficient available sats for buy-in lock: have %d need %d", wallet.AvailableSats, requiredFundingSats)
	}
	funding, err := runtime.selectJoinFundingBundle(buyInSats)
	if err != nil {
		return NativeMeshTableView{}, err
	}
	if err := runtime.reserveJoinFundingRefs(tableID, buyInSats, funding.Refs); err != nil {
		return NativeMeshTableView{}, err
	}
	releaseReservation := true
	defer func() {
		if !releaseReservation {
			return
		}
		_ = runtime.releasePendingFundingReservation(tableID)
		_ = runtime.cleanupPrimedLocalJoinSeat(tableID, runtime.walletID.PlayerID)
	}()
	request := nativeJoinRequest{
		ArkAddress:       wallet.ArkAddress,
		BuyInSats:        buyInSats,
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
	binding, err := settlementcore.BuildIdentityBinding(tableID, runtime.selfPeerID(), runtime.selfPeerURL(), runtime.protocolIdentity, runtime.walletID, nowISO())
	if err != nil {
		return NativeMeshTableView{}, err
	}
	request.IdentityBinding = binding
	if err := runtime.primeLocalJoinSeat(tableID, request); err != nil {
		return NativeMeshTableView{}, err
	}

	var table nativeTableState
	if hostPeerURL == runtime.selfPeerURL() {
		table, err = runtime.handleJoinFromPeer(request)
	} else {
		table, err = runtime.remoteJoin(hostPeerURL, request)
	}
	if err != nil {
		return NativeMeshTableView{}, err
	}
	if err := runtime.acceptRemoteTable(table); err != nil {
		return NativeMeshTableView{}, err
	}
	releaseReservation = false
	accepted, err := runtime.requireLocalTable(table.Config.TableID)
	if err != nil {
		return NativeMeshTableView{}, err
	}
	return runtime.localTableView(*accepted), nil
}

func (runtime *meshRuntime) GetTable(tableID string) (NativeMeshTableView, error) {
	if err := runtime.Start(); err != nil {
		return NativeMeshTableView{}, err
	}
	if tableID == "" {
		tableID = runtime.currentTableID()
	}
	if tableID == "" {
		return NativeMeshTableView{}, errors.New("table id is required")
	}
	table, err := runtime.refreshLocalTable(tableID)
	if err != nil {
		return NativeMeshTableView{}, err
	}
	if table == nil {
		return NativeMeshTableView{}, fmt.Errorf("table %s not found", tableID)
	}
	return runtime.localTableView(*table), nil
}

func (runtime *meshRuntime) SendAction(tableID string, action game.Action) (result NativeMeshTableView, err error) {
	timing := startMeshTiming(meshTimingFields{
		Metric:   "action_submit_roundtrip_total",
		TableID:  tableID,
		Purpose:  string(action.Type),
		PlayerID: runtime.walletID.PlayerID,
	})
	defer func() {
		timing.End(err)
	}()
	if err := runtime.Start(); err != nil {
		return NativeMeshTableView{}, err
	}
	var (
		table   *nativeTableState
		updated nativeTableState
	)
	for attempt := 0; attempt < 3; attempt++ {
		runtime.syncTableFromKnownParticipants(tableID)
		table, err = runtime.refreshLocalTable(tableID)
		if err != nil {
			return NativeMeshTableView{}, err
		}
		if table == nil {
			return NativeMeshTableView{}, fmt.Errorf("table %s not found", tableID)
		}
		if table.CurrentHost.Peer.PeerID != runtime.selfPeerID() && table.ActiveHand != nil && !game.PhaseAllowsActions(table.ActiveHand.State.Phase) && table.CurrentHost.Peer.PeerURL != "" {
			remote, fetchErr := runtime.fetchRemoteTable(table.CurrentHost.Peer.PeerURL, tableID)
			if fetchErr == nil && remote != nil {
				if acceptErr := runtime.acceptRemoteTable(*remote); acceptErr == nil {
					table, _ = runtime.store.readTable(tableID)
				}
			}
		}
		request, buildErr := runtime.buildSignedActionRequest(*table, action)
		if buildErr != nil {
			return NativeMeshTableView{}, buildErr
		}
		if table.CurrentHost.Peer.PeerID == runtime.selfPeerID() {
			updated, err = runtime.handleActionFromPeer(request)
		} else {
			updated, err = runtime.remoteAction(table.CurrentHost.Peer.PeerURL, request)
		}
		if err != nil {
			if table.CurrentHost.Peer.PeerID != runtime.selfPeerID() &&
				table.CurrentHost.Peer.PeerURL != "" &&
				attempt < 2 &&
				isRetryableActionRequestStateError(err) {
				runtime.syncTableFromKnownParticipants(tableID)
				refreshed, refreshErr := runtime.refreshLocalTable(tableID)
				if refreshErr == nil && refreshed != nil && actionRetryStateChanged(*table, *refreshed) {
					continue
				}
				if refreshErr != nil {
					return NativeMeshTableView{}, refreshErr
				}
			}
			return NativeMeshTableView{}, err
		}
		if acceptErr := runtime.acceptRemoteTable(updated); acceptErr != nil {
			if table.CurrentHost.Peer.PeerID != runtime.selfPeerID() &&
				table.CurrentHost.Peer.PeerURL != "" &&
				attempt < 2 &&
				isRetryableActionResponseStateError(acceptErr) {
				runtime.syncTableFromKnownParticipants(tableID)
				refreshed, refreshErr := runtime.refreshLocalTable(tableID)
				if refreshErr == nil && refreshed != nil && actionRetryStateChanged(*table, *refreshed) {
					continue
				}
				if refreshErr != nil {
					return NativeMeshTableView{}, refreshErr
				}
			}
			return NativeMeshTableView{}, acceptErr
		}
		accepted, err := runtime.requireLocalTable(updated.Config.TableID)
		if err != nil {
			return NativeMeshTableView{}, err
		}
		return runtime.localTableView(*accepted), nil
	}
	return NativeMeshTableView{}, errors.New("action submission exhausted retries")
}

func (runtime *meshRuntime) RotateHost(tableID string) (NativeMeshTableView, error) {
	if err := runtime.failoverTable(tableID, "manual rotation"); err != nil {
		return NativeMeshTableView{}, err
	}
	table, err := runtime.requireLocalTable(tableID)
	if err != nil {
		return NativeMeshTableView{}, err
	}
	return runtime.localTableView(*table), nil
}

func (runtime *meshRuntime) PublicTables() ([]NativePublicTableView, error) {
	if runtime.config.IndexerURL != "" {
		indexerURL := strings.TrimSuffix(runtime.config.IndexerURL, "/") + "/api/public/tables"
		request, err := http.NewRequest(http.MethodGet, indexerURL, nil)
		if err == nil {
			response, err := runtime.httpClient.Do(request)
			if err == nil {
				defer response.Body.Close()
				if response.StatusCode >= 200 && response.StatusCode < 300 {
					var decoded []NativePublicTableView
					if err := json.NewDecoder(response.Body).Decode(&decoded); err == nil {
						return decoded, nil
					}
				}
			}
		}
	}
	ads, err := runtime.store.readPublicAds()
	if err != nil {
		return nil, err
	}
	views := make([]NativePublicTableView, 0, len(ads))
	for _, ad := range ads {
		table, _ := runtime.store.readTable(ad.TableID)
		views = append(views, NativePublicTableView{
			Advertisement: ad,
			LatestState:   table.PublicState,
			RecentUpdates: []map[string]any{},
		})
	}
	sort.Slice(views, func(i, j int) bool { return views[i].Advertisement.TableID < views[j].Advertisement.TableID })
	return views, nil
}

func (runtime *meshRuntime) CashOut(tableID string) (map[string]any, error) {
	return runtime.completeFunds(tableID, "cashout", "completed")
}

func (runtime *meshRuntime) Renew(tableID string) ([]map[string]any, error) {
	table, err := runtime.refreshLocalTable(tableID)
	if err != nil {
		return nil, err
	}
	if table == nil || table.LatestCustodyState == nil {
		return nil, errors.New("latest custody state is unavailable")
	}
	return []map[string]any{{
		"carryForward": true,
		"custodySeq":   table.LatestCustodyState.CustodySeq,
		"stateHash":    table.LatestCustodyState.StateHash,
	}}, nil
}

func (runtime *meshRuntime) Exit(tableID string) (map[string]any, error) {
	return runtime.completeFunds(tableID, "emergency-exit", "exited")
}

func (runtime *meshRuntime) completeFunds(tableID, kind, finalStatus string) (map[string]any, error) {
	if kind == string(tablecustody.TransitionKindEmergencyExit) {
		return runtime.completeEmergencyExit(tableID, finalStatus)
	}
	var (
		response nativeFundsResponse
		table    *nativeTableState
		request  nativeFundsRequest
		err      error
	)
	for attempt := 0; attempt < 2; attempt++ {
		table, err = runtime.syncLatestFundsTable(tableID)
		if err != nil {
			return nil, err
		}
		if table == nil || table.LatestCustodyState == nil {
			return nil, errors.New("latest custody state is unavailable")
		}
		debugMeshf(
			"complete funds table=%s kind=%s self=%s host=%s epoch=%d transitions=%d",
			tableID,
			kind,
			runtime.selfPeerID(),
			table.CurrentHost.Peer.PeerID,
			table.CurrentEpoch,
			len(table.CustodyTransitions),
		)
		request, err = runtime.buildSignedFundsRequest(*table, kind)
		if err != nil {
			return nil, err
		}
		debugMeshf("complete funds route table=%s self_handles=%t host=%s", tableID, table.CurrentHost.Peer.PeerID == runtime.selfPeerID(), table.CurrentHost.Peer.PeerID)
		if table.CurrentHost.Peer.PeerID == runtime.selfPeerID() {
			response, err = runtime.handleFundsFromPeer(request)
		} else {
			response, err = runtime.remoteFunds(table.CurrentHost.Peer.PeerURL, request)
		}
		if err == nil {
			break
		}
		if attempt == 0 &&
			table.CurrentHost.Peer.PeerID != runtime.selfPeerID() &&
			table.CurrentHost.Peer.PeerURL != "" &&
			isRetryableFundsRequestStateError(err) {
			remote, fetchErr := runtime.fetchRemoteTable(table.CurrentHost.Peer.PeerURL, tableID)
			if fetchErr == nil && remote != nil {
				if acceptErr := runtime.acceptRemoteTable(*remote); acceptErr == nil {
					runtime.lastSyncAt[tableID] = time.Now()
					continue
				}
			}
		}
		return nil, err
	}
	if err != nil {
		return nil, err
	}
	if table.CurrentHost.Peer.PeerID != runtime.selfPeerID() {
		if err := runtime.acceptRemoteTable(response.Table); err != nil {
			return nil, err
		}
	}
	status := response.Settlement.Status
	if strings.TrimSpace(status) == "" {
		status = finalStatus
	}
	operation, err := runtime.buildFundsOperation(table.Config.TableID, response.Settlement.AmountSats, kind, status, response.Settlement.StateHash, response.Settlement.ArkIntentID, response.Settlement.ArkTxID, response.Settlement.ExitProofRef, response.Settlement.VTXORefs, &request)
	if err != nil {
		return nil, err
	}
	if err := runtime.appendFundsOperation(operation, response.Settlement.AmountSats, status); err != nil {
		return nil, err
	}
	return map[string]any{
		"prevStateHash":  response.Settlement.PrevStateHash,
		"stateHash":      response.Settlement.StateHash,
		"custodySeq":     response.Settlement.CustodySeq,
		"receipt":        rawJSONMap(operation),
		"transitionHash": response.Settlement.TransitionHash,
		"settledArkTx":   response.Settlement.ArkTxID,
		"status":         status,
	}, nil
}

func (runtime *meshRuntime) completeEmergencyExit(tableID, finalStatus string) (map[string]any, error) {
	pendingOperation, pendingRequest, err := runtime.pendingEmergencyExitOperation(tableID)
	if err != nil {
		return nil, err
	}

	table, err := runtime.syncLatestFundsTable(tableID)
	if err != nil {
		return nil, err
	}
	if table == nil || table.LatestCustodyState == nil {
		return nil, errors.New("latest custody state is unavailable")
	}

	if settlement, acceptedRequest, ok, err := runtime.acceptedEmergencyExitSettlement(*table, runtime.walletID.PlayerID); err != nil {
		return nil, err
	} else if ok {
		request := pendingRequest
		if request == nil && acceptedRequest != nil {
			cloned := cloneJSON(*acceptedRequest)
			request = &cloned
		}
		status := settlement.Status
		if strings.TrimSpace(status) == "" {
			status = finalStatus
		}
		if existing, err := runtime.latestFundsOperation(table.Config.TableID, string(tablecustody.TransitionKindEmergencyExit), status); err != nil {
			return nil, err
		} else if existing != nil {
			if status == "pending-exit" || pendingOperation == nil {
				return map[string]any{
					"prevStateHash":  settlement.PrevStateHash,
					"stateHash":      settlement.StateHash,
					"custodySeq":     settlement.CustodySeq,
					"receipt":        rawJSONMap(*existing),
					"transitionHash": settlement.TransitionHash,
					"settledArkTx":   settlement.ArkTxID,
					"status":         status,
				}, nil
			}
		}
		if status == "pending-exit" && pendingOperation != nil {
			return map[string]any{
				"prevStateHash":  settlement.PrevStateHash,
				"stateHash":      settlement.StateHash,
				"custodySeq":     settlement.CustodySeq,
				"receipt":        rawJSONMap(*pendingOperation),
				"transitionHash": settlement.TransitionHash,
				"settledArkTx":   settlement.ArkTxID,
				"status":         status,
			}, nil
		}
		operation, err := runtime.buildFundsOperation(table.Config.TableID, settlement.AmountSats, string(tablecustody.TransitionKindEmergencyExit), status, settlement.StateHash, settlement.ArkIntentID, settlement.ArkTxID, settlement.ExitProofRef, settlement.VTXORefs, request)
		if err != nil {
			return nil, err
		}
		if err := runtime.appendFundsOperation(operation, settlement.AmountSats, status); err != nil {
			return nil, err
		}
		return map[string]any{
			"prevStateHash":  settlement.PrevStateHash,
			"stateHash":      settlement.StateHash,
			"custodySeq":     settlement.CustodySeq,
			"receipt":        rawJSONMap(operation),
			"transitionHash": settlement.TransitionHash,
			"settledArkTx":   settlement.ArkTxID,
			"status":         status,
		}, nil
	}

	request := pendingRequest
	if request == nil {
		if tableHasLiveHand(*table) {
			return nil, errors.New("funds settlement requires the current hand to be settled first")
		}
		transition, err := runtime.buildFundsCustodyTransitionForPlayer(*table, runtime.walletID.PlayerID, tablecustody.TransitionKindEmergencyExit, finalStatus)
		if err != nil {
			return nil, err
		}
		approvalTransition, _, err := runtime.normalizedCustodyApprovalTransition(*table, transition)
		if err != nil {
			return nil, err
		}
		if _, err := runtime.collectCustodyApprovals(*table, approvalTransition, nil, runtime.requiredCustodySigners(*table, approvalTransition)); err != nil {
			return nil, err
		}
		sourceRefs := canonicalVTXORefs(runtime.currentCustodyRefsByPlayer(*table)[runtime.walletID.PlayerID])
		if len(sourceRefs) == 0 {
			return nil, errors.New("latest custody state has no spendable custody claim to settle")
		}
		executionResult, err := runtime.executeLocalCustodyExit(sourceRefs, "")
		if err != nil {
			return nil, err
		}
		signedRequest, err := runtime.buildSignedFundsRequest(*table, string(tablecustody.TransitionKindEmergencyExit), fundsExitExecutionFromResult(executionResult))
		if err != nil {
			return nil, err
		}
		request = &signedRequest
	} else if request.ExitExecution != nil &&
		request.PrevCustodyStateHash == latestCustodyStateHash(*table) &&
		request.Epoch != table.CurrentEpoch {
		resignedRequest, err := runtime.buildSignedFundsRequest(*table, string(tablecustody.TransitionKindEmergencyExit), request.ExitExecution)
		if err != nil {
			return nil, err
		}
		request = &resignedRequest
	}

	var response nativeFundsResponse
	if table.CurrentHost.Peer.PeerID == runtime.selfPeerID() {
		response, err = runtime.handleFundsFromPeer(*request)
	} else {
		response, err = runtime.remoteFunds(table.CurrentHost.Peer.PeerURL, *request)
	}
	if err != nil {
		operation, pendingErr := runtime.persistPendingEmergencyExit(*table, *request, pendingOperation)
		if pendingErr != nil {
			return nil, pendingErr
		}
		debugMeshf("emergency exit submission pending table=%s host=%s err=%v", tableID, table.CurrentHost.Peer.PeerID, err)
		return map[string]any{
			"prevStateHash": request.PrevCustodyStateHash,
			"stateHash":     operation.StateHash,
			"custodySeq":    operation.CustodySeq,
			"receipt":       rawJSONMap(operation),
			"settledArkTx":  operation.ArkTxID,
			"status":        operation.Status,
		}, nil
	}

	if table.CurrentHost.Peer.PeerID != runtime.selfPeerID() {
		if err := runtime.acceptRemoteTable(response.Table); err != nil {
			return nil, err
		}
	}
	status := response.Settlement.Status
	if strings.TrimSpace(status) == "" {
		status = finalStatus
	}
	operation, err := runtime.buildFundsOperation(table.Config.TableID, response.Settlement.AmountSats, string(tablecustody.TransitionKindEmergencyExit), status, response.Settlement.StateHash, response.Settlement.ArkIntentID, response.Settlement.ArkTxID, response.Settlement.ExitProofRef, response.Settlement.VTXORefs, request)
	if err != nil {
		return nil, err
	}
	if err := runtime.appendFundsOperation(operation, response.Settlement.AmountSats, status); err != nil {
		return nil, err
	}
	return map[string]any{
		"prevStateHash":  response.Settlement.PrevStateHash,
		"stateHash":      response.Settlement.StateHash,
		"custodySeq":     response.Settlement.CustodySeq,
		"receipt":        rawJSONMap(operation),
		"transitionHash": response.Settlement.TransitionHash,
		"settledArkTx":   response.Settlement.ArkTxID,
		"status":         status,
	}, nil
}

func (runtime *meshRuntime) syncLatestFundsTable(tableID string) (*nativeTableState, error) {
	table, err := runtime.refreshLocalTable(tableID)
	if err != nil || table == nil {
		return table, err
	}
	if table.CurrentHost.Peer.PeerID == runtime.selfPeerID() || table.CurrentHost.Peer.PeerURL == "" {
		return table, nil
	}
	// Funds requests are signed against the latest custody hash, so force one
	// authoritative host refresh instead of relying on periodic polling.
	remote, err := runtime.fetchRemoteTable(table.CurrentHost.Peer.PeerURL, tableID)
	if err != nil {
		return nil, err
	}
	if remote == nil {
		return table, nil
	}
	if err := runtime.acceptRemoteTable(*remote); err != nil {
		if !isStaleRemoteTableError(err) {
			return nil, err
		}
		debugMeshf("funds refresh saw stale host table table=%s host=%s err=%v", tableID, table.CurrentHost.Peer.PeerID, err)
		visiblePlayerID := ""
		for _, seat := range table.Seats {
			if seat.PeerID == table.CurrentHost.Peer.PeerID {
				visiblePlayerID = seat.PlayerID
				break
			}
		}
		syncRequest, syncErr := runtime.buildTableSyncRequest(runtime.networkTableView(*table, visiblePlayerID))
		if syncErr != nil {
			debugMeshf("funds refresh failed building recovery sync table=%s err=%v", tableID, syncErr)
			return nil, err
		}
		if syncErr = runtime.sendTableSync(table.CurrentHost.Peer.PeerURL, syncRequest); syncErr != nil {
			debugMeshf("funds refresh failed pushing local table to host table=%s err=%v", tableID, syncErr)
			return nil, err
		}
		debugMeshf("funds refresh pushed local table to stale host table=%s host=%s", tableID, table.CurrentHost.Peer.PeerID)
		return table, nil
	}
	runtime.lastSyncAt[tableID] = time.Now()
	table, err = runtime.store.readTable(tableID)
	if err != nil {
		return nil, err
	}
	if table == nil {
		return nil, nil
	}
	return table, nil
}

func isRetryableFundsRequestStateError(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	return strings.Contains(message, "funds request custody mismatch") ||
		strings.Contains(message, "funds request epoch mismatch")
}

func isRetryableActionRequestStateError(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	return strings.Contains(message, "action request custody mismatch") ||
		strings.Contains(message, "action request epoch mismatch") ||
		strings.Contains(message, "action request must be sent to the current host") ||
		strings.Contains(message, "i/o timeout") ||
		strings.Contains(message, "connection refused") ||
		strings.Contains(message, "EOF")
}

func isRetryableActionResponseStateError(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	return strings.Contains(message, "accepted history would roll back table events") ||
		strings.Contains(message, "accepted history would roll back table snapshots")
}

func actionRetryStateChanged(previous, refreshed nativeTableState) bool {
	if snapshotProtocolDrive(previous) != snapshotProtocolDrive(refreshed) {
		return true
	}
	if previous.LastEventHash != refreshed.LastEventHash {
		return true
	}
	if len(previous.Events) != len(refreshed.Events) {
		return true
	}
	return len(previous.Snapshots) != len(refreshed.Snapshots)
}

func cloneFundsExitExecution(execution *nativeFundsExitExecution) *nativeFundsExitExecution {
	if execution == nil {
		return nil
	}
	cloned := cloneJSON(*execution)
	cloned.BroadcastTxIDs = append([]string(nil), execution.BroadcastTxIDs...)
	cloned.SourceRefs = canonicalVTXORefs(execution.SourceRefs)
	return &cloned
}

func fundsExitExecutionFromResult(result walletpkg.CustodyExitResult) *nativeFundsExitExecution {
	return &nativeFundsExitExecution{
		BroadcastTxIDs: append([]string(nil), result.BroadcastTxIDs...),
		Pending:        result.Pending,
		SourceRefs:     canonicalVTXORefs(result.SourceRefs),
		SweepTxID:      result.SweepTxID,
	}
}

func fundsExitExecutionTxIDs(execution *nativeFundsExitExecution) []string {
	if execution == nil {
		return nil
	}
	txIDs := append([]string(nil), execution.BroadcastTxIDs...)
	if strings.TrimSpace(execution.SweepTxID) != "" {
		txIDs = append(txIDs, execution.SweepTxID)
	}
	return txIDs
}

func fundsExitExecutionArkTxID(execution *nativeFundsExitExecution) string {
	if execution == nil {
		return ""
	}
	if strings.TrimSpace(execution.SweepTxID) != "" {
		return execution.SweepTxID
	}
	if len(execution.BroadcastTxIDs) == 0 {
		return ""
	}
	return execution.BroadcastTxIDs[0]
}

func canonicalVTXORefs(refs []tablecustody.VTXORef) []tablecustody.VTXORef {
	canonical := append([]tablecustody.VTXORef(nil), refs...)
	sort.SliceStable(canonical, func(left, right int) bool {
		leftRef := canonical[left]
		rightRef := canonical[right]
		switch {
		case leftRef.TxID != rightRef.TxID:
			return leftRef.TxID < rightRef.TxID
		case leftRef.VOut != rightRef.VOut:
			return leftRef.VOut < rightRef.VOut
		case leftRef.ArkTxID != rightRef.ArkTxID:
			return leftRef.ArkTxID < rightRef.ArkTxID
		case leftRef.ArkIntentID != rightRef.ArkIntentID:
			return leftRef.ArkIntentID < rightRef.ArkIntentID
		case leftRef.AmountSats != rightRef.AmountSats:
			return leftRef.AmountSats < rightRef.AmountSats
		default:
			return leftRef.OwnerPlayerID < rightRef.OwnerPlayerID
		}
	})
	return canonical
}

func sameCanonicalVTXORefs(left, right []tablecustody.VTXORef) bool {
	return reflect.DeepEqual(canonicalVTXORefs(left), canonicalVTXORefs(right))
}

func (runtime *meshRuntime) buildSignedFundsRequest(table nativeTableState, kind string, exitExecution ...*nativeFundsExitExecution) (nativeFundsRequest, error) {
	if table.LatestCustodyState == nil {
		return nativeFundsRequest{}, errors.New("latest custody state is unavailable")
	}
	transitionKind, _, err := fundsTransitionKindAndStatus(kind)
	if err != nil {
		return nativeFundsRequest{}, err
	}
	request := nativeFundsRequest{
		Epoch:                table.CurrentEpoch,
		Kind:                 kind,
		PlayerID:             runtime.walletID.PlayerID,
		PrevCustodyStateHash: latestCustodyStateHash(table),
		ProfileName:          runtime.profileName,
		ProtocolVersion:      nativeProtocolVersion,
		SignedAt:             nowISO(),
		TableID:              table.Config.TableID,
	}
	if len(exitExecution) > 0 && exitExecution[0] != nil {
		if transitionKind != tablecustody.TransitionKindEmergencyExit {
			return nativeFundsRequest{}, errors.New("exit execution proof is only supported for emergency exit")
		}
		request.ExitExecution = cloneFundsExitExecution(exitExecution[0])
	}
	if transitionKind == tablecustody.TransitionKindEmergencyExit && request.ExitExecution == nil {
		return nativeFundsRequest{}, errors.New("emergency exit requires local execution proof")
	}
	if kind == "cashout" {
		wallet, err := runtime.walletSummary()
		if err != nil {
			return nativeFundsRequest{}, err
		}
		if strings.TrimSpace(wallet.ArkAddress) == "" {
			return nativeFundsRequest{}, errors.New("wallet has no Ark address for cash-out settlement")
		}
		request.ArkAddress = wallet.ArkAddress
	}
	signatureHex, err := settlementcore.SignStructuredData(runtime.walletID.PrivateKeyHex, nativeFundsAuthPayload(request))
	if err != nil {
		return nativeFundsRequest{}, err
	}
	request.SignatureHex = signatureHex
	return request, nil
}

func fundsTransitionKindAndStatus(kind string) (tablecustody.TransitionKind, string, error) {
	switch kind {
	case "cashout", string(tablecustody.TransitionKindCashOut):
		return tablecustody.TransitionKindCashOut, "completed", nil
	case string(tablecustody.TransitionKindEmergencyExit):
		return tablecustody.TransitionKindEmergencyExit, "exited", nil
	default:
		return "", "", fmt.Errorf("unsupported funds operation kind %q", kind)
	}
}

func validateSettlementRequestProtocolVersion(protocolVersion string) error {
	if strings.TrimSpace(protocolVersion) != nativeProtocolVersion {
		return fmt.Errorf("settlement request protocol version mismatch")
	}
	return nil
}

func validateAcceptedTableProtocolVersion(table nativeTableState) error {
	if strings.TrimSpace(table.Config.ProtocolVersion) != nativeProtocolVersion {
		return errors.New("table protocol version mismatch")
	}
	if table.Advertisement != nil && strings.TrimSpace(table.Advertisement.ProtocolVersion) != nativeProtocolVersion {
		return errors.New("advertisement protocol version mismatch")
	}
	return nil
}

func (runtime *meshRuntime) executeLocalCustodyExit(sourceRefs []tablecustody.VTXORef, destination string) (walletpkg.CustodyExitResult, error) {
	if runtime.custodyExitExecute != nil {
		return runtime.custodyExitExecute(runtime.profileName, append([]tablecustody.VTXORef(nil), sourceRefs...), destination)
	}
	return runtime.walletRuntime.UnilateralExitCustodyRefs(runtime.profileName, sourceRefs, destination)
}

func (runtime *meshRuntime) signLocalCustodyPSBT(current string) (string, error) {
	if runtime.custodyPSBTSign != nil {
		return runtime.custodyPSBTSign(runtime.profileName, current)
	}
	return runtime.walletRuntime.SignCustodyTransaction(runtime.profileName, current)
}

func (runtime *meshRuntime) executeLocalCustodyRecovery(signedPSBT string) (walletpkg.CustodyRecoveryResult, error) {
	if runtime.custodyRecoveryExecute != nil {
		return runtime.custodyRecoveryExecute(runtime.profileName, signedPSBT)
	}
	return runtime.walletRuntime.ExecuteCustodyRecoveryTransaction(runtime.profileName, signedPSBT)
}

func emergencyExitProofRef(state tablecustody.CustodyState, playerID string, execution *nativeFundsExitExecution) string {
	if execution == nil {
		return ""
	}
	return tablecustody.BuildExitProofRef(state, playerID, canonicalVTXORefs(execution.SourceRefs), fundsExitExecutionTxIDs(execution))
}

func (runtime *meshRuntime) validateEmergencyExitExecution(table nativeTableState, request nativeFundsRequest) error {
	if request.Kind != string(tablecustody.TransitionKindEmergencyExit) {
		return nil
	}
	if request.ExitExecution == nil {
		return errors.New("funds request is missing emergency exit execution proof")
	}
	expectedSourceRefs := canonicalVTXORefs(runtime.currentCustodyRefsByPlayer(table)[request.PlayerID])
	if len(expectedSourceRefs) == 0 {
		return errors.New("funds request is missing the latest custody refs for emergency exit")
	}
	if !sameCanonicalVTXORefs(request.ExitExecution.SourceRefs, expectedSourceRefs) {
		return errors.New("funds request emergency exit source refs mismatch")
	}
	if len(fundsExitExecutionTxIDs(request.ExitExecution)) == 0 {
		return errors.New("funds request emergency exit proof is missing txids")
	}
	return nil
}

func (runtime *meshRuntime) acceptedEmergencyExitSettlement(table nativeTableState, playerID string) (*nativeFundsSettlement, *nativeFundsRequest, bool, error) {
	for index := len(table.Events) - 1; index >= 0; index-- {
		event := table.Events[index]
		if stringValue(event.Body["type"]) != "EmergencyExit" {
			continue
		}
		if strings.TrimSpace(stringValue(event.Body["playerId"])) != playerID {
			continue
		}
		transition, _, err := linkedCustodyTransitionForEvent(table, event)
		if err != nil {
			return nil, nil, false, err
		}
		request, hasRequest, err := fundsRequestFromEvent(event)
		if err != nil {
			return nil, nil, false, err
		}
		if !hasRequest || request == nil {
			return nil, nil, false, errors.New("accepted emergency exit is missing its signed funds request")
		}
		amount, err := decodeJSONValue[int](event.Body["amountSats"])
		if err != nil {
			return nil, nil, false, err
		}
		status := strings.TrimSpace(stringValue(event.Body["status"]))
		if status == "" {
			status = "exited"
		}
		settlement := nativeFundsSettlement{
			AmountSats:     amount,
			ArkTxID:        transition.ArkTxID,
			CustodySeq:     transition.CustodySeq,
			ExitProofRef:   transition.Proof.ExitProofRef,
			PrevStateHash:  transition.PrevStateHash,
			StateHash:      transition.NextStateHash,
			Status:         status,
			TransitionHash: transition.Proof.TransitionHash,
		}
		if request.ExitExecution != nil {
			settlement.VTXORefs = canonicalVTXORefs(request.ExitExecution.SourceRefs)
		}
		return &settlement, request, true, nil
	}
	return nil, nil, false, nil
}

func (runtime *meshRuntime) persistPendingEmergencyExit(table nativeTableState, request nativeFundsRequest, existing *NativeTableFundsOperation) (NativeTableFundsOperation, error) {
	if existing != nil {
		return cloneJSON(*existing), nil
	}
	if request.ExitExecution == nil {
		return NativeTableFundsOperation{}, errors.New("emergency exit request is missing execution proof")
	}
	amountSats := sumVTXORefs(request.ExitExecution.SourceRefs)
	exitProofRef := emergencyExitProofRef(*table.LatestCustodyState, request.PlayerID, request.ExitExecution)
	operation, err := runtime.buildFundsOperation(
		table.Config.TableID,
		amountSats,
		string(tablecustody.TransitionKindEmergencyExit),
		"pending-exit",
		table.LatestCustodyState.StateHash,
		"",
		fundsExitExecutionArkTxID(request.ExitExecution),
		exitProofRef,
		canonicalVTXORefs(request.ExitExecution.SourceRefs),
		&request,
	)
	if err != nil {
		return NativeTableFundsOperation{}, err
	}
	if err := runtime.appendFundsOperation(operation, amountSats, "pending-exit"); err != nil {
		return NativeTableFundsOperation{}, err
	}
	return operation, nil
}

func (runtime *meshRuntime) handleFundsFromPeer(request nativeFundsRequest) (nativeFundsResponse, error) {
	var response nativeFundsResponse
	err := runtime.store.withTableLock(request.TableID, func() error {
		table, err := runtime.store.readTable(request.TableID)
		if err != nil || table == nil {
			return fmt.Errorf("table %s not found", request.TableID)
		}
		if table.CurrentHost.Peer.PeerID != runtime.selfPeerID() {
			return errors.New("funds request must be sent to the current host")
		}
		seat, ok := seatRecordForPlayer(*table, request.PlayerID)
		if !ok {
			return errors.New("player is not seated")
		}
		if err := runtime.validateFundsRequest(*table, seat, request); err != nil {
			return err
		}
		settlement, err := runtime.applyFundsRequestLocked(table, request)
		if err != nil {
			return err
		}
		response = nativeFundsResponse{
			Settlement: settlement,
			Table:      runtime.networkTableView(*table, request.PlayerID),
		}
		return nil
	})
	return response, err
}

func (runtime *meshRuntime) applyFundsRequestLocked(table *nativeTableState, request nativeFundsRequest) (nativeFundsSettlement, error) {
	if table.LatestCustodyState == nil {
		return nativeFundsSettlement{}, errors.New("latest custody state is unavailable")
	}
	if tableHasLiveHand(*table) {
		return nativeFundsSettlement{}, errors.New("funds settlement requires the current hand to be settled first")
	}
	transitionKind, finalStatus, err := fundsTransitionKindAndStatus(request.Kind)
	if err != nil {
		return nativeFundsSettlement{}, err
	}
	claim, ok := latestStackClaimForPlayer(table.LatestCustodyState, request.PlayerID)
	if !ok {
		return nativeFundsSettlement{}, errors.New("latest custody state is missing the requested stack claim")
	}
	amount := stackClaimBackedAmount(claim)
	if amount <= 0 {
		return nativeFundsSettlement{}, errors.New("latest custody state has no spendable custody claim to settle")
	}
	previousStateHash := table.LatestCustodyState.StateHash
	sourceRefs := canonicalVTXORefs(runtime.currentCustodyRefsByPlayer(*table)[request.PlayerID])
	transition, err := runtime.buildFundsCustodyTransitionForPlayer(*table, request.PlayerID, transitionKind, finalStatus)
	if err != nil {
		return nativeFundsSettlement{}, err
	}
	authorizer := authorizerForFundsRequest(request)

	receiptStatus := finalStatus
	receiptIntentID := ""
	receiptArkTxID := ""
	exitProofRef := ""
	receiptRefs := []tablecustody.VTXORef{}
	approvalTransition, _, err := runtime.normalizedCustodyApprovalTransition(*table, transition)
	if err != nil {
		return nativeFundsSettlement{}, err
	}

	if runtime.config.UseMockSettlement {
		if err := runtime.finalizeCustodyTransition(table, &transition, authorizer); err != nil {
			return nativeFundsSettlement{}, err
		}
	} else if transitionKind == tablecustody.TransitionKindCashOut {
		approvals, err := runtime.collectCustodyApprovals(*table, approvalTransition, authorizer, runtime.requiredCustodySigners(*table, approvalTransition))
		if err != nil {
			return nativeFundsSettlement{}, err
		}
		result, settledAmount, _, err := runtime.settleTableFundsForPlayer(*table, transition, authorizer, request.PlayerID)
		if err != nil {
			return nativeFundsSettlement{}, err
		}
		amount = settledAmount
		receiptIntentID = result.IntentID
		receiptArkTxID = result.ArkTxID
		receiptRefs = append(receiptRefs, result.OutputRefs["wallet-return"]...)
		transition.ArkIntentID = result.IntentID
		transition.ArkTxID = result.ArkTxID
		transition.Approvals = append([]tablecustody.CustodySignature(nil), approvals...)
		transition.Proof = tablecustody.CustodyProof{
			ArkIntentID:       result.IntentID,
			ArkTxID:           result.ArkTxID,
			FinalizedAt:       result.FinalizedAt,
			RequestHash:       approvalTransition.Proof.RequestHash,
			ReplayValidated:   true,
			SettlementWitness: custodySettlementWitnessFromResult(result),
			Signatures:        append([]tablecustody.CustodySignature(nil), approvals...),
			StateHash:         transition.NextStateHash,
			VTXORefs:          append(stackProofRefs(transition.NextState), receiptRefs...),
		}
		transition.Proof.TransitionHash = tablecustody.HashCustodyTransition(transition)
		if err := tablecustody.ValidateTransition(table.LatestCustodyState, transition); err != nil {
			return nativeFundsSettlement{}, err
		}
	} else {
		approvals, err := runtime.collectCustodyApprovals(*table, approvalTransition, authorizer, runtime.requiredCustodySigners(*table, approvalTransition))
		if err != nil {
			return nativeFundsSettlement{}, err
		}
		execution := cloneFundsExitExecution(request.ExitExecution)
		if execution == nil {
			return nativeFundsSettlement{}, errors.New("funds request is missing emergency exit execution proof")
		}
		if execution.Pending {
			receiptStatus = "pending-exit"
		}
		receiptRefs = append(receiptRefs, canonicalVTXORefs(execution.SourceRefs)...)
		receiptArkTxID = fundsExitExecutionArkTxID(execution)
		exitProofRef = emergencyExitProofRef(*table.LatestCustodyState, request.PlayerID, execution)
		transition.ArkTxID = receiptArkTxID
		transition.Approvals = append([]tablecustody.CustodySignature(nil), approvals...)
		transition.Proof = tablecustody.CustodyProof{
			ArkTxID:         receiptArkTxID,
			ExitProofRef:    exitProofRef,
			FinalizedAt:     nowISO(),
			RequestHash:     approvalTransition.Proof.RequestHash,
			ReplayValidated: true,
			Signatures:      append([]tablecustody.CustodySignature(nil), approvals...),
			StateHash:       transition.NextStateHash,
			VTXORefs:        append(stackProofRefs(transition.NextState), sourceRefs...),
		}
		transition.Proof.TransitionHash = tablecustody.HashCustodyTransition(transition)
		if err := tablecustody.ValidateTransition(table.LatestCustodyState, transition); err != nil {
			return nativeFundsSettlement{}, err
		}
	}

	runtime.applyCustodyTransition(table, transition)
	for index := range table.Seats {
		if table.Seats[index].PlayerID == request.PlayerID {
			table.Seats[index].Status = receiptStatus
			table.Seats[index].NativeSeatedPlayer.Status = receiptStatus
		}
	}
	table.ActiveHand = nil
	table.ActiveHandStartAt = ""
	table.NextHandAt = ""
	if activeCustodySeatCount(*table) < 2 {
		table.Config.Status = "seating"
	} else {
		table.Config.Status = "ready"
	}
	table.PublicState = runtime.publicStateFromLatestCustody(*table, table.Config.Status)
	checkpointHash := ""
	if len(table.Snapshots) > 0 && table.PublicState != nil {
		snapshot, err := runtime.buildSnapshot(*table, *table.PublicState)
		if err != nil {
			return nativeFundsSettlement{}, err
		}
		table.LatestSnapshot = &snapshot
		table.LatestFullySignedSnapshot = &snapshot
		table.Snapshots = append(table.Snapshots, snapshot)
		checkpointHash = runtime.snapshotHash(snapshot)
	}
	eventType := "CashOut"
	if transitionKind == tablecustody.TransitionKindEmergencyExit {
		eventType = "EmergencyExit"
	}
	body := map[string]any{
		"amountSats":             amount,
		"custodySeq":             transition.CustodySeq,
		"exitProofRef":           exitProofRef,
		"fundsRequest":           rawJSONMap(request),
		"latestCustodyStateHash": transition.NextStateHash,
		"playerId":               request.PlayerID,
		"status":                 receiptStatus,
		"type":                   eventType,
		"transitionHash":         transition.Proof.TransitionHash,
	}
	if checkpointHash != "" && table.PublicState != nil {
		body["balances"] = table.PublicState.ChipBalances
		body["checkpointHash"] = checkpointHash
		body["publicState"] = rawJSONMap(*table.PublicState)
	}
	if err := runtime.appendEvent(table, body); err != nil {
		return nativeFundsSettlement{}, err
	}
	if err := runtime.persistAndReplicate(table, true); err != nil {
		return nativeFundsSettlement{}, err
	}

	return nativeFundsSettlement{
		AmountSats:     amount,
		ArkIntentID:    receiptIntentID,
		ArkTxID:        receiptArkTxID,
		CustodySeq:     transition.CustodySeq,
		ExitProofRef:   exitProofRef,
		PrevStateHash:  previousStateHash,
		StateHash:      transition.NextStateHash,
		Status:         receiptStatus,
		TransitionHash: transition.Proof.TransitionHash,
		VTXORefs:       receiptRefs,
	}, nil
}

func (runtime *meshRuntime) requireLocalTable(tableID string) (*nativeTableState, error) {
	table, err := runtime.store.readTable(tableID)
	if err != nil {
		return nil, err
	}
	if table == nil {
		return nil, fmt.Errorf("table %s not found", tableID)
	}
	return table, nil
}

func (runtime *meshRuntime) refreshLocalTable(tableID string) (*nativeTableState, error) {
	table, err := runtime.store.readTable(tableID)
	if err != nil || table == nil {
		return table, err
	}
	if table.CurrentHost.Peer.PeerID != runtime.selfPeerID() && runtime.shouldPollHost(tableID) && table.CurrentHost.Peer.PeerURL != "" {
		remote, err := runtime.fetchRemoteTable(table.CurrentHost.Peer.PeerURL, tableID)
		if err == nil && remote != nil {
			if err := runtime.acceptRemoteTable(*remote); err != nil {
				if isStaleRemoteTableError(err) {
					return table, nil
				}
				return nil, err
			}
			runtime.lastSyncAt[tableID] = time.Now()
			table, err = runtime.store.readTable(tableID)
			if err != nil {
				return nil, err
			}
		}
	}
	if table != nil && protocolDeadlineExpired(*table) && runtime.shouldHandleFailover(*table) {
		if err := runtime.forceProtocolFailover(tableID, fmt.Sprintf("protocol deadline expired during %s", table.ActiveHand.State.Phase)); err == nil {
			table, _ = runtime.store.readTable(tableID)
		}
	}
	if table != nil && elapsedMillis(table.LastHostHeartbeatAt) > int64(runtime.hostFailureTimeoutMS()) && runtime.shouldHandleFailover(*table) {
		if err := runtime.failoverTable(tableID, "missed host heartbeats"); err == nil {
			table, _ = runtime.store.readTable(tableID)
		}
	}
	if table != nil &&
		table.CurrentHost.Peer.PeerID != runtime.selfPeerID() &&
		table.ActiveHand != nil &&
		!game.PhaseAllowsActions(table.ActiveHand.State.Phase) {
		runtime.driveLocalHandProtocol(tableID)
		table, err = runtime.store.readTable(tableID)
		if err != nil {
			return nil, err
		}
	}
	return table, nil
}

func (runtime *meshRuntime) ensureBootstrapLocked(nickname, walletNsec string) error {
	if runtime.walletID.PlayerID != "" && runtime.protocolID != "" {
		if nickname == "" && walletNsec == "" {
			return nil
		}
	}

	state, err := runtime.walletRuntime.EnsureProfile(runtime.profileName, nickname, walletNsec)
	if err != nil {
		return err
	}
	if nickname != "" {
		state.Nickname = nickname
	}
	if state.PeerPrivateKeyHex == "" {
		state.PeerPrivateKeyHex, err = settlementcore.RandomHex(32)
		if err != nil {
			return err
		}
	}
	if state.ProtocolPrivateKeyHex == "" {
		state.ProtocolPrivateKeyHex, err = settlementcore.RandomHex(32)
		if err != nil {
			return err
		}
	}
	if state.TransportPrivateKeyHex == "" {
		state.TransportPrivateKeyHex, err = randomX25519PrivateKeyHex()
		if err != nil {
			return err
		}
	}
	if state.KnownPeers == nil {
		state.KnownPeers = []walletpkg.KnownPeerState{}
	}
	if state.MeshTables == nil {
		state.MeshTables = map[string]walletpkg.MeshTableReferenceState{}
	}
	if err := runtime.profileStore.Save(state); err != nil {
		return err
	}

	walletID, err := settlementcore.CreateLocalIdentity(state.WalletPrivateKeyHex)
	if err != nil {
		return err
	}
	peerIdentity, err := settlementcore.CreateScopedIdentity(settlementcore.PeerIdentityScope, state.PeerPrivateKeyHex)
	if err != nil {
		return err
	}
	protocolIdentity, err := settlementcore.CreateScopedIdentity(settlementcore.ProtocolIdentityScope, state.ProtocolPrivateKeyHex)
	if err != nil {
		return err
	}
	transportPublic, err := x25519PublicKeyHex(state.TransportPrivateKeyHex)
	if err != nil {
		return err
	}
	runtime.walletID = walletID
	runtime.peerIdentity = peerIdentity
	runtime.protocolIdentity = protocolIdentity
	runtime.protocolID = protocolIdentity.ID
	runtime.transportPrivate = state.TransportPrivateKeyHex
	runtime.transportPublic = transportPublic
	runtime.transportKeyID = transportKeyID(transportPublic)
	existingPeerURL := runtime.self.Peer.PeerURL
	runtime.self = nativePeerSelf{
		Alias: state.Nickname,
		Mode:  runtime.mode,
		Peer: NativePeerAddress{
			Alias:             state.Nickname,
			LastSeenAt:        nowISO(),
			PeerID:            peerIdentity.ID,
			PeerURL:           existingPeerURL,
			ProtocolPubkeyHex: protocolIdentity.PublicKeyHex,
			Roles:             []string{runtime.mode},
		},
		ProfileName:        runtime.profileName,
		ProtocolID:         protocolIdentity.ID,
		TransportPubkeyHex: transportPublic,
		WalletPlayerID:     walletID.PlayerID,
	}
	return nil
}

func (runtime *meshRuntime) startPeerServerLocked() error {
	if runtime.listener != nil {
		return runtime.ensureAdvertisedPeerURLLocked()
	}
	host := runtime.config.PeerHost
	if host == "" {
		host = "127.0.0.1"
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(runtime.config.PeerPort)))
	if err != nil {
		return err
	}
	runtime.listener = listener
	runtime.server = nil
	if err := ensureRuntimeStateDir(runtime.store.paths.StateDir); err != nil {
		_ = listener.Close()
		runtime.listener = nil
		return err
	}
	if err := runtime.ensureAdvertisedPeerURLLocked(); err != nil {
		_ = listener.Close()
		runtime.listener = nil
		runtime.clearPeerURL = ""
		runtime.torService = nil
		runtime.self.Peer.PeerURL = ""
		return err
	}
	go runtime.servePeerTransport(listener)
	return nil
}

func (runtime *meshRuntime) handleJoinFromPeer(join nativeJoinRequest) (nativeTableState, error) {
	var updated nativeTableState
	err := runtime.store.withTableLock(join.TableID, func() error {
		debugMeshf("handle join from peer table=%s player=%s peer_id=%s host_self=%s", join.TableID, join.WalletPlayerID, join.Peer.PeerID, runtime.selfPeerID())
		table, err := runtime.store.readTable(join.TableID)
		if err != nil || table == nil {
			return fmt.Errorf("table %s not found", join.TableID)
		}
		if table.CurrentHost.Peer.PeerID != runtime.selfPeerID() {
			return errors.New("join request must be sent to the current host")
		}
		if err := runtime.validateJoinRequest(*table, join); err != nil {
			return err
		}
		peer := join.Peer
		peer.LastSeenAt = nowISO()
		if err := runtime.saveKnownPeer(peer); err != nil {
			return err
		}
		seatIndex := len(table.Seats)
		replaceSeatIndex := -1
		for index, seat := range table.Seats {
			if seat.PlayerID != join.WalletPlayerID {
				continue
			}
			if seat.Status == "pending-join" && seat.PeerID == join.Peer.PeerID {
				replaceSeatIndex = index
				seatIndex = seat.SeatIndex
				break
			}
			updated = *table
			return nil
		}
		if replaceSeatIndex < 0 && len(table.Seats) >= table.Config.SeatCount {
			return errors.New("table is full")
		}
		seat := nativeSeatRecord{
			NativeSeatedPlayer: NativeSeatedPlayer{
				ArkAddress:        join.ArkAddress,
				BuyInSats:         join.BuyInSats,
				FundingRefs:       append([]tablecustody.VTXORef(nil), join.FundingRefs...),
				Nickname:          join.Nickname,
				PeerID:            join.Peer.PeerID,
				PlayerID:          join.WalletPlayerID,
				ProtocolPubkeyHex: join.Peer.ProtocolPubkeyHex,
				SeatIndex:         seatIndex,
				Status:            "active",
				WalletPubkeyHex:   join.WalletPubkeyHex,
			},
			PeerURL:     join.Peer.PeerURL,
			ProfileName: join.ProfileName,
		}
		if replaceSeatIndex >= 0 {
			table.Seats[replaceSeatIndex] = seat
		} else {
			table.Seats = append(table.Seats, seat)
		}
		table.Config.OccupiedSeats = len(table.Seats)
		seatLockTransition, err := runtime.buildSeatLockTransition(*table)
		if err != nil {
			return err
		}
		if err := runtime.syncTableToCustodySigners(*table, runtime.custodyTreeSignerIDs(*table, seatLockTransition)); err != nil {
			return err
		}
		if err := runtime.finalizeCustodyTransition(table, &seatLockTransition, nil); err != nil {
			return err
		}
		runtime.applyCustodyTransition(table, seatLockTransition)
		if err := runtime.appendEvent(table, map[string]any{
			"custodySeq":             seatLockTransition.CustodySeq,
			"fundingRefs":            join.FundingRefs,
			"latestCustodyStateHash": seatLockTransition.NextStateHash,
			"type":                   "SeatLocked",
			"buyInSats":              join.BuyInSats,
			"peerId":                 join.Peer.PeerID,
			"playerId":               join.WalletPlayerID,
			"seatIndex":              seatIndex,
		}); err != nil {
			return err
		}
		if len(table.Seats) == table.Config.SeatCount {
			table.Config.Status = "ready"
			readyState := runtime.buildReadyPublicState(*table)
			table.PublicState = &readyState
			snapshot, err := runtime.buildSnapshot(*table, readyState)
			if err != nil {
				return err
			}
			table.LatestSnapshot = &snapshot
			table.LatestFullySignedSnapshot = &snapshot
			table.Snapshots = append(table.Snapshots, snapshot)
			if err := runtime.appendEvent(table, map[string]any{
				"type":        "TableReady",
				"balances":    readyState.ChipBalances,
				"publicState": rawJSONMap(readyState),
			}); err != nil {
				return err
			}
			table.NextHandAt = addMillis(nowISO(), runtime.nextHandDelayMSForTable(*table))
			if err := runtime.startNextHandLocked(table); err != nil {
				return err
			}
		} else {
			table.Config.Status = "seating"
		}
		if err := runtime.persistAndReplicate(table, true); err != nil {
			return err
		}
		updated = *table
		return nil
	})
	return updated, err
}

func (runtime *meshRuntime) handleActionFromPeer(request nativeActionRequest) (updated nativeTableState, err error) {
	timingFields := meshTimingFields{
		Metric:   "action_transition_total",
		TableID:  request.TableID,
		Purpose:  "peer-request",
		PlayerID: request.PlayerID,
	}
	timing := startMeshTiming(timingFields)
	defer func() {
		timing.EndWith(timingFields, err)
	}()
	err = runtime.store.withTableLock(request.TableID, func() error {
		table, err := runtime.store.readTable(request.TableID)
		if err != nil || table == nil {
			return fmt.Errorf("table %s not found", request.TableID)
		}
		if table.CurrentHost.Peer.PeerID != runtime.selfPeerID() {
			return errors.New("action request must be sent to the current host")
		}
		if table.ActiveHand == nil {
			return errors.New("hand is not active")
		}
		seat, ok := seatRecordForPlayer(*table, request.PlayerID)
		if !ok {
			return errors.New("player is not seated")
		}
		if err := runtime.validateActionRequest(*table, seat, request); err != nil {
			return err
		}
		seatIndex := seat.SeatIndex
		authorizer := authorizerForActionRequest(request)
		requiredSigners := runtime.requiredCustodySigners(*table, tablecustody.CustodyTransition{
			Kind:    tablecustody.TransitionKindAction,
			TableID: table.Config.TableID,
		})
		if err := runtime.syncTableToCustodySigners(*table, requiredSigners); err != nil {
			return err
		}
		nextState, err := game.ApplyHoldemAction(table.ActiveHand.State, seatIndex, request.Action)
		if err != nil {
			return err
		}
		custodyTransition, err := runtime.buildCustodyTransitionWithOverrides(*table, tablecustody.TransitionKindAction, &nextState, &request.Action, nil, actionRequestBindingOverrides(request))
		if err != nil {
			return err
		}
		timingFields.CustodySeq = custodyTransition.CustodySeq
		timingFields.TransitionKind = string(custodyTransition.Kind)
		timingFields.Phase = tablePhaseForTiming(*table)
		timingFields.RequestHash = custodyApprovalTargetHash(custodyTransition)
		if err := runtime.finalizeCustodyTransition(table, &custodyTransition, authorizer); err != nil {
			return err
		}
		if err := runtime.attachDeterministicRecoveryBundles(*table, &custodyTransition, authorizer, &nextState); err != nil {
			return err
		}
		table.ActiveHand.State = nextState
		runtime.applyCustodyTransition(table, custodyTransition)
		if err := runtime.appendEvent(table, map[string]any{
			"actionRequest":  rawJSONMap(request),
			"custodySeq":     custodyTransition.CustodySeq,
			"playerId":       request.PlayerID,
			"seatIndex":      seatIndex,
			"transitionHash": custodyTransition.Proof.TransitionHash,
			"type":           "PlayerAction",
		}); err != nil {
			return err
		}
		if err := runtime.advanceHandProtocolLocked(table); err != nil {
			return err
		}
		if err := runtime.persistAndReplicate(table, true); err != nil {
			return err
		}
		updated = *table
		return nil
	})
	return updated, err
}

func (runtime *meshRuntime) startNextHandLocked(table *nativeTableState) error {
	if table.NextHandAt != "" && elapsedMillis(table.NextHandAt) < 0 {
		return nil
	}
	if table.ActiveHand != nil {
		return nil
	}
	if len(table.Seats) < 2 || activeCustodySeatCount(*table) < 2 {
		return nil
	}
	table.ActiveHandStartAt = runtime.canonicalNextHandAt(*table)
	chips := map[string]int{}
	if table.LatestCustodyState != nil {
		for _, claim := range table.LatestCustodyState.StackClaims {
			chips[claim.PlayerID] = claim.AmountSats
		}
	} else if table.LatestFullySignedSnapshot != nil {
		for playerID, amount := range table.LatestFullySignedSnapshot.ChipBalances {
			chips[playerID] = amount
		}
	} else {
		for _, seat := range table.Seats {
			chips[seat.PlayerID] = seat.BuyInSats
		}
	}
	handNumber := 1
	if table.PublicState != nil {
		handNumber = table.PublicState.HandNumber + 1
	}
	dealerSeat := (handNumber - 1) % 2
	hand, err := game.CreateHoldemHand(game.HoldemHandConfig{
		BigBlindSats:    table.Config.BigBlindSats,
		DealerSeatIndex: dealerSeat,
		HandID:          randomUUID(),
		HandNumber:      handNumber,
		Seats: []game.HoldemSeatConfig{
			{PlayerID: table.Seats[0].PlayerID, StackSats: chips[table.Seats[0].PlayerID]},
			{PlayerID: table.Seats[1].PlayerID, StackSats: chips[table.Seats[1].PlayerID]},
		},
		SmallBlindSats: table.Config.SmallBlindSats,
	})
	if err != nil {
		return err
	}
	table.ActiveHand = &nativeActiveHand{
		Cards: nativeHandCardState{
			PhaseDeadlineAt: addMillis(nowISO(), runtime.handProtocolTimeoutMSForTable(*table)),
			Transcript: game.HandTranscript{
				HandID:     hand.HandID,
				HandNumber: hand.HandNumber,
				Records:    []game.HandTranscriptRecord{},
				RootHash:   "",
				TableID:    table.Config.TableID,
			},
		},
		State: hand,
	}
	blindTransition, err := runtime.buildCustodyTransition(*table, tablecustody.TransitionKindBlindPost, &hand, nil, nil)
	if err != nil {
		return err
	}
	if err := runtime.finalizeCustodyTransition(table, &blindTransition, nil); err != nil {
		return err
	}
	if err := runtime.attachDeterministicRecoveryBundles(*table, &blindTransition, nil, &hand); err != nil {
		return err
	}
	runtime.applyCustodyTransition(table, blindTransition)
	table.Config.Status = "active"
	table.NextHandAt = ""
	if err := runtime.appendEvent(table, map[string]any{
		"dealerSeatIndex": dealerSeat,
		"custodySeq":      blindTransition.CustodySeq,
		"handId":          hand.HandID,
		"handNumber":      hand.HandNumber,
		"transitionHash":  blindTransition.Proof.TransitionHash,
		"type":            "HandStart",
	}); err != nil {
		return err
	}
	return runtime.advanceHandProtocolLocked(table)
}

func (runtime *meshRuntime) syncTableFromKnownParticipants(tableID string) {
	table, err := runtime.store.readTable(tableID)
	if err != nil || table == nil {
		return
	}
	seen := map[string]struct{}{}
	peerURLs := []string{}
	addPeerURL := func(peerURL string) {
		if peerURL == "" || peerURL == runtime.selfPeerURL() {
			return
		}
		if _, ok := seen[peerURL]; ok {
			return
		}
		seen[peerURL] = struct{}{}
		peerURLs = append(peerURLs, peerURL)
	}
	for _, seat := range table.Seats {
		addPeerURL(firstNonEmptyString(seat.PeerURL, runtime.knownPeerURL(seat.PeerID)))
	}
	for _, witness := range table.Witnesses {
		addPeerURL(firstNonEmptyString(witness.Peer.PeerURL, runtime.knownPeerURL(witness.Peer.PeerID)))
	}
	addPeerURL(firstNonEmptyString(table.CurrentHost.Peer.PeerURL, runtime.knownPeerURL(table.CurrentHost.Peer.PeerID)))
	for _, peerURL := range peerURLs {
		remote, fetchErr := runtime.fetchRemoteTable(peerURL, tableID)
		if fetchErr != nil || remote == nil {
			continue
		}
		if acceptErr := runtime.acceptRemoteTable(*remote); acceptErr != nil {
			debugMeshf("failover pre-sync rejected remote table=%s peer=%s err=%v", tableID, peerURL, acceptErr)
		}
	}
}

func (runtime *meshRuntime) syncTableBeforeFailover(tableID string) {
	runtime.syncTableFromKnownParticipants(tableID)
}

func (runtime *meshRuntime) rotateHostTable(tableID, reason string, requireHostFailure bool, resetProtocolDeadline bool) error {
	observedHostFailure := false
	if requireHostFailure {
		if table, err := runtime.store.readTable(tableID); err == nil && table != nil {
			observedHostFailure = elapsedMillis(table.LastHostHeartbeatAt) > int64(runtime.hostFailureTimeoutMS())
		}
	}
	debugMeshf("rotate host start table=%s reason=%s requireHostFailure=%t observedHostFailure=%t", tableID, reason, requireHostFailure, observedHostFailure)
	runtime.syncTableBeforeFailover(tableID)
	return runtime.store.withTableLock(tableID, func() error {
		table, err := runtime.store.readTable(tableID)
		if err != nil || table == nil {
			return err
		}
		if requireHostFailure && !observedHostFailure && elapsedMillis(table.LastHostHeartbeatAt) <= int64(runtime.hostFailureTimeoutMS()) {
			debugMeshf("rotate host skipped table=%s reason=%s heartbeatAge=%d", tableID, reason, elapsedMillis(table.LastHostHeartbeatAt))
			return nil
		}
		if !runtime.shouldHandleFailover(*table) {
			debugMeshf("rotate host skipped table=%s reason=%s handler=false host=%s", tableID, reason, table.CurrentHost.Peer.PeerID)
			return nil
		}
		previousHost := table.CurrentHost
		table.CurrentEpoch++
		table.CurrentHost = nativeKnownParticipant{ProfileName: runtime.profileName, Peer: runtime.self.Peer}
		table.Config.HostPeerID = runtime.selfPeerID()
		table.LastHostHeartbeatAt = nowISO()
		lease := map[string]any{
			"epoch":       table.CurrentEpoch,
			"hostPeerId":  table.CurrentHost.Peer.PeerID,
			"leaseExpiry": addMillis(nowISO(), runtime.hostFailureTimeoutMS()),
			"leaseStart":  nowISO(),
			"signatures":  []map[string]any{},
			"tableId":     table.Config.TableID,
			"witnessSet":  peerIDsFromParticipants(table.Witnesses),
		}
		if err := runtime.appendEvent(table, map[string]any{
			"type":               "HostRotated",
			"lease":              lease,
			"newEpoch":           table.CurrentEpoch,
			"newHostPeerId":      table.CurrentHost.Peer.PeerID,
			"previousHostPeerId": previousHost.Peer.PeerID,
		}); err != nil {
			return err
		}
		if table.ActiveHand != nil {
			if resetProtocolDeadline && shouldTrackProtocolDeadline(table.ActiveHand.State.Phase) {
				runtime.setProtocolDeadline(table)
			}
			if err := runtime.advanceHandProtocolLocked(table); err != nil {
				debugMeshf("rotate host advance failed table=%s reason=%s err=%v", tableID, reason, err)
				return err
			}
		}
		if table.ActiveHand == nil {
			table.NextHandAt = addMillis(nowISO(), runtime.nextHandDelayMSForTable(*table))
			if err := runtime.startNextHandLocked(table); err != nil {
				debugMeshf("rotate host restart hand failed table=%s reason=%s err=%v", tableID, reason, err)
				return err
			}
		}
		debugMeshf("rotate host complete table=%s reason=%s newHost=%s phase=%v", tableID, reason, table.CurrentHost.Peer.PeerID, func() any {
			if table.ActiveHand == nil {
				return nil
			}
			return table.ActiveHand.State.Phase
		}())
		return runtime.persistAndReplicate(table, true)
	})
}

func (runtime *meshRuntime) failoverTable(tableID, reason string) error {
	return runtime.rotateHostTable(tableID, reason, true, true)
}

func (runtime *meshRuntime) forceProtocolFailover(tableID, reason string) error {
	return runtime.rotateHostTable(tableID, reason, false, false)
}

func (runtime *meshRuntime) persistLocalTable(table *nativeTableState, publish bool) error {
	normalizePublicTranscriptRoot(table)
	runtime.refreshPersistedActiveHandPublicState(table)
	table.LastSyncedAt = nowISO()
	if err := runtime.store.writeTable(table); err != nil {
		return err
	}
	if err := runtime.store.rewriteEvents(table.Config.TableID, table.Events); err != nil {
		return err
	}
	if err := runtime.store.rewriteSnapshots(table.Config.TableID, table.Snapshots); err != nil {
		return err
	}
	if err := runtime.syncPrivateAndFunds(*table); err != nil {
		return err
	}
	if publish && table.Advertisement != nil && table.Config.Visibility == "public" {
		_ = runtime.store.upsertPublicAd(*table.Advertisement)
		_ = runtime.publishPublicState(*table)
	}
	return nil
}

func (runtime *meshRuntime) refreshPersistedActiveHandPublicState(table *nativeTableState) {
	if table == nil || table.ActiveHand == nil {
		return
	}
	expected := runtime.publicStateFromHand(*table, table.ActiveHand.State)
	if table.PublicState != nil &&
		stringValue(table.PublicState.HandID) == table.ActiveHand.State.HandID &&
		table.PublicState.HandNumber == table.ActiveHand.State.HandNumber {
		expected.PreviousSnapshotHash = table.PublicState.PreviousSnapshotHash
	}
	if reflect.DeepEqual(comparablePublicState(table.PublicState), comparablePublicState(&expected)) {
		return
	}
	table.PublicState = &expected
}

func (runtime *meshRuntime) persistAndReplicate(table *nativeTableState, publish bool) error {
	if err := runtime.persistLocalTable(table, publish); err != nil {
		return err
	}
	runtime.replicateTable(*table)
	return nil
}

func (runtime *meshRuntime) syncPrivateAndFunds(table nativeTableState) error {
	if err := runtime.storeLocalHoleCards(table); err != nil {
		return err
	}
	funds, err := runtime.store.readTableFunds()
	if err != nil {
		return err
	}
	entry, ok := funds.Tables[table.Config.TableID]
	if !ok {
		entry = NativeTableFundsEntry{
			LastUpdatedAt: nowISO(),
			Operations:    []NativeTableFundsOperation{},
			PlayerID:      runtime.walletID.PlayerID,
			Status:        "",
			TableID:       table.Config.TableID,
		}
	}
	frozenStatus := entry.Status == "completed" || entry.Status == "exited" || entry.Status == "pending-exit"
	for _, seat := range table.Seats {
		if seat.PlayerID == runtime.walletID.PlayerID {
			if entry.BuyInSats == 0 {
				entry.BuyInSats = seat.BuyInSats
			}
			if !frozenStatus {
				entry.ReservedFundingRefs = append([]tablecustody.VTXORef(nil), seat.FundingRefs...)
				entry.Status = "locked"
			}
		}
	}
	if table.LatestCustodyState != nil {
		entry.CheckpointHash = table.LatestCustodyState.StateHash
		entry.LastUpdatedAt = nowISO()
	} else if table.LatestFullySignedSnapshot != nil {
		entry.CheckpointHash = runtime.snapshotHash(*table.LatestFullySignedSnapshot)
		entry.LastUpdatedAt = nowISO()
	}
	funds.Tables[table.Config.TableID] = entry
	return runtime.store.writeTableFunds(funds)
}

func (runtime *meshRuntime) replicateTable(table nativeTableState) {
	if !runtime.isRunning() {
		return
	}
	targets := map[string]string{}
	for _, witness := range table.Witnesses {
		if witness.Peer.PeerURL != "" && witness.Peer.PeerID != runtime.selfPeerID() {
			targets[witness.Peer.PeerURL] = ""
		}
	}
	for _, seat := range table.Seats {
		if seat.PeerID == runtime.selfPeerID() {
			continue
		}
		peerURL := seat.PeerURL
		if peerURL == "" {
			peerURL = runtime.knownPeerURL(seat.PeerID)
		}
		if peerURL != "" {
			targets[peerURL] = seat.PlayerID
		}
	}
	var waitGroup sync.WaitGroup
	for peerURL, visiblePlayerID := range targets {
		syncRequest, err := runtime.buildTableSyncRequest(runtime.networkTableView(table, visiblePlayerID))
		if err != nil {
			continue
		}
		waitGroup.Add(1)
		go func(peerURL string, syncRequest nativeTableSyncRequest) {
			defer waitGroup.Done()
			if !runtime.isRunning() {
				return
			}
			if runtime.tableSyncSender != nil {
				_ = runtime.tableSyncSender(peerURL, syncRequest)
				return
			}
			_ = runtime.sendTableSync(peerURL, syncRequest)
		}(peerURL, syncRequest)
	}
	waitGroup.Wait()
}

func (runtime *meshRuntime) publishPublicState(table nativeTableState) error {
	if table.Advertisement == nil || runtime.config.IndexerURL == "" {
		return nil
	}
	indexer := strings.TrimSuffix(runtime.config.IndexerURL, "/")
	if _, err := runtime.postJSON(indexer, "/api/indexer/table-ads", *table.Advertisement); err != nil {
		return err
	}
	update := map[string]any{
		"advertisement": rawJSONMap(*table.Advertisement),
		"publicState":   rawJSONMap(table.PublicState),
		"publishedAt":   nowISO(),
		"tableId":       table.Config.TableID,
		"type":          "PublicTableSnapshot",
	}
	_, _ = runtime.postJSON(indexer, "/api/indexer/table-updates", update)
	return nil
}

func (runtime *meshRuntime) buildAdvertisement(table nativeTableState) (NativeAdvertisement, error) {
	unsigned := map[string]any{
		"adExpiresAt":           addMillis(nowISO(), 3600_000),
		"buyInMaxSats":          table.Config.BuyInMaxSats,
		"buyInMinSats":          table.Config.BuyInMinSats,
		"currency":              "sats",
		"hostModeCapabilities":  []string{nativeDealerMode},
		"hostPeerId":            table.CurrentHost.Peer.PeerID,
		"hostPeerUrl":           table.CurrentHost.Peer.PeerURL,
		"hostProtocolPubkeyHex": runtime.protocolIdentity.PublicKeyHex,
		"networkId":             table.Config.NetworkID,
		"occupiedSeats":         table.Config.OccupiedSeats,
		"protocolVersion":       nativeProtocolVersion,
		"seatCount":             table.Config.SeatCount,
		"spectatorsAllowed":     table.Config.SpectatorsAllowed,
		"stakes": map[string]int{
			"bigBlindSats":   table.Config.BigBlindSats,
			"smallBlindSats": table.Config.SmallBlindSats,
		},
		"tableId":      table.Config.TableID,
		"tableName":    table.Config.Name,
		"visibility":   table.Config.Visibility,
		"witnessCount": len(table.Witnesses),
	}
	signatureHex, err := settlementcore.SignStructuredData(runtime.protocolIdentity.PrivateKeyHex, unsigned)
	if err != nil {
		return NativeAdvertisement{}, err
	}
	return NativeAdvertisement{
		AdExpiresAt:           stringValue(unsigned["adExpiresAt"]),
		BuyInMaxSats:          table.Config.BuyInMaxSats,
		BuyInMinSats:          table.Config.BuyInMinSats,
		Currency:              "sats",
		HostModeCapabilities:  []string{nativeDealerMode},
		HostPeerID:            table.CurrentHost.Peer.PeerID,
		HostPeerURL:           table.CurrentHost.Peer.PeerURL,
		HostProtocolPubkeyHex: runtime.protocolIdentity.PublicKeyHex,
		HostSignatureHex:      signatureHex,
		NetworkID:             table.Config.NetworkID,
		OccupiedSeats:         table.Config.OccupiedSeats,
		ProtocolVersion:       nativeProtocolVersion,
		SeatCount:             table.Config.SeatCount,
		SpectatorsAllowed:     table.Config.SpectatorsAllowed,
		Stakes: map[string]int{
			"bigBlindSats":   table.Config.BigBlindSats,
			"smallBlindSats": table.Config.SmallBlindSats,
		},
		TableID:      table.Config.TableID,
		TableName:    table.Config.Name,
		Visibility:   table.Config.Visibility,
		WitnessCount: len(table.Witnesses),
	}, nil
}

func (runtime *meshRuntime) appendEvent(table *nativeTableState, body map[string]any) error {
	prevHash := any(nil)
	if table.LastEventHash != "" {
		prevHash = table.LastEventHash
	}
	event := NativeSignedTableEvent{
		Body:                    body,
		Epoch:                   table.CurrentEpoch,
		HandID:                  nil,
		MessageType:             stringValue(body["type"]),
		NetworkID:               runtime.config.Network,
		PrevEventHash:           prevHash,
		ProtocolVersion:         nativeProtocolVersion,
		Seq:                     len(table.Events),
		SenderPeerID:            runtime.selfPeerID(),
		SenderProtocolPubkeyHex: runtime.protocolIdentity.PublicKeyHex,
		SenderRole:              runtime.mode,
		TableID:                 table.Config.TableID,
		Timestamp:               nowISO(),
	}
	if table.PublicState != nil && table.PublicState.HandID != nil {
		event.HandID = table.PublicState.HandID
	}
	unsigned := rawJSONMap(event)
	delete(unsigned, "signature")
	signatureHex, err := settlementcore.SignStructuredData(runtime.protocolIdentity.PrivateKeyHex, unsigned)
	if err != nil {
		return err
	}
	event.Signature = signatureHex
	eventHash, err := settlementcore.HashStructuredDataHex(unsigned)
	if err != nil {
		return err
	}
	table.LastEventHash = eventHash
	table.Events = append(table.Events, event)
	if table.PublicState != nil {
		table.PublicState.LatestEventHash = eventHash
		table.PublicState.UpdatedAt = nowISO()
	}
	return nil
}

func (runtime *meshRuntime) buildReadyPublicState(table nativeTableState) NativePublicTableState {
	chips := map[string]int{}
	seatedPlayers := make([]NativeSeatedPlayer, 0, len(table.Seats))
	for _, seat := range table.Seats {
		chips[seat.PlayerID] = seat.BuyInSats
		seatedPlayers = append(seatedPlayers, seat.NativeSeatedPlayer)
	}
	return NativePublicTableState{
		ActingSeatIndex:      nil,
		Board:                []string{},
		ChipBalances:         chips,
		CurrentBetSats:       0,
		DealerCommitment:     nil,
		DealerSeatIndex:      nil,
		Epoch:                table.CurrentEpoch,
		FoldedPlayerIDs:      []string{},
		HandID:               nil,
		HandNumber:           0,
		LatestEventHash:      table.LastEventHash,
		LivePlayerIDs:        playerIDsFromSeats(table.Seats),
		MinRaiseToSats:       table.Config.BigBlindSats,
		Phase:                nil,
		PotSats:              0,
		PreviousSnapshotHash: nil,
		RoundContributions:   zeroContributions(table.Seats),
		SeatedPlayers:        seatedPlayers,
		SnapshotID:           randomUUID(),
		Status:               "ready",
		TableID:              table.Config.TableID,
		TotalContributions:   zeroContributions(table.Seats),
		UpdatedAt:            nowISO(),
	}
}

func (runtime *meshRuntime) publicStateFromHand(table nativeTableState, hand game.HoldemState) NativePublicTableState {
	seatedPlayers := make([]NativeSeatedPlayer, 0, len(table.Seats))
	chipBalances := map[string]int{}
	roundContributions := map[string]int{}
	totalContributions := map[string]int{}
	live := []string{}
	folded := []string{}
	for _, seat := range table.Seats {
		playerState := hand.Players[seat.SeatIndex]
		seated := seat.NativeSeatedPlayer
		switch playerState.Status {
		case game.PlayerStatusFolded:
			seated.Status = "folded"
			folded = append(folded, seat.PlayerID)
		case game.PlayerStatusAllIn:
			seated.Status = "all-in"
			live = append(live, seat.PlayerID)
		default:
			seated.Status = "active"
			live = append(live, seat.PlayerID)
		}
		seatedPlayers = append(seatedPlayers, seated)
		chipBalances[seat.PlayerID] = playerState.StackSats
		roundContributions[seat.PlayerID] = playerState.RoundContributionSats
		totalContributions[seat.PlayerID] = playerState.TotalContributionSats
	}
	var actingSeat any
	if hand.ActingSeatIndex != nil {
		actingSeat = *hand.ActingSeatIndex
	}
	phase := any(string(hand.Phase))
	var dealerCommitment *NativeDealerCommitment
	if table.ActiveHand != nil && table.ActiveHand.Cards.Transcript.RootHash != "" {
		dealerCommitment = &NativeDealerCommitment{
			CommittedAt: nowISO(),
			Mode:        nativeDealerMode,
			RootHash:    table.ActiveHand.Cards.Transcript.RootHash,
		}
	}
	return NativePublicTableState{
		ActingSeatIndex:      actingSeat,
		Board:                stringCards(hand.Board),
		ChipBalances:         chipBalances,
		CurrentBetSats:       hand.CurrentBetSats,
		DealerCommitment:     dealerCommitment,
		DealerSeatIndex:      hand.DealerSeatIndex,
		Epoch:                table.CurrentEpoch,
		FoldedPlayerIDs:      folded,
		HandID:               hand.HandID,
		HandNumber:           hand.HandNumber,
		LatestEventHash:      table.LastEventHash,
		LivePlayerIDs:        live,
		MinRaiseToSats:       hand.MinRaiseToSats,
		Phase:                phase,
		PotSats:              hand.PotSats,
		PreviousSnapshotHash: previousSnapshotHash(table),
		RoundContributions:   roundContributions,
		SeatedPlayers:        seatedPlayers,
		SnapshotID:           randomUUID(),
		Status:               "active",
		TableID:              table.Config.TableID,
		TotalContributions:   totalContributions,
		UpdatedAt:            nowISO(),
	}
}

func (runtime *meshRuntime) publicStateFromSnapshot(table nativeTableState, snapshot NativeCooperativeTableSnapshot) NativePublicTableState {
	return NativePublicTableState{
		ActingSeatIndex:      snapshot.TurnIndex,
		Board:                []string{},
		ChipBalances:         snapshot.ChipBalances,
		CurrentBetSats:       0,
		DealerCommitment:     nil,
		DealerSeatIndex:      nil,
		Epoch:                snapshot.Epoch,
		FoldedPlayerIDs:      snapshot.FoldedPlayerIDs,
		HandID:               snapshot.HandID,
		HandNumber:           snapshot.HandNumber,
		LatestEventHash:      snapshot.LatestEventHash,
		LivePlayerIDs:        snapshot.LivePlayerIDs,
		MinRaiseToSats:       table.Config.BigBlindSats,
		Phase:                snapshot.Phase,
		PotSats:              snapshot.PotSats,
		PreviousSnapshotHash: snapshot.PreviousSnapshotHash,
		RoundContributions:   zeroContributionsFromPlayers(snapshot.SeatedPlayers),
		SeatedPlayers:        snapshot.SeatedPlayers,
		SnapshotID:           randomUUID(),
		Status:               "ready",
		TableID:              table.Config.TableID,
		TotalContributions:   zeroContributionsFromPlayers(snapshot.SeatedPlayers),
		UpdatedAt:            nowISO(),
	}
}

func comparableDealerCommitment(commitment *NativeDealerCommitment) *NativeDealerCommitment {
	if commitment == nil {
		return nil
	}
	comparable := cloneJSON(*commitment)
	comparable.CommittedAt = ""
	if strings.TrimSpace(comparable.RootHash) == "" {
		return nil
	}
	return &comparable
}

func comparablePublicState(state *NativePublicTableState) *NativePublicTableState {
	if state == nil {
		return nil
	}
	comparable := cloneJSON(*state)
	comparable.DealerCommitment = comparableDealerCommitment(comparable.DealerCommitment)
	comparable.SnapshotID = ""
	comparable.UpdatedAt = ""
	return &comparable
}

func comparableSnapshotForReplay(snapshot *NativeCooperativeTableSnapshot) *NativeCooperativeTableSnapshot {
	if snapshot == nil {
		return nil
	}
	comparable := cloneJSON(*snapshot)
	comparable.CreatedAt = ""
	comparable.LatestEventHash = nil
	comparable.Signatures = nil
	comparable.SnapshotID = ""
	return &comparable
}

func comparableSnapshotForHistory(snapshot *NativeCooperativeTableSnapshot) *NativeCooperativeTableSnapshot {
	if snapshot == nil {
		return nil
	}
	comparable := cloneJSON(*snapshot)
	comparable.CreatedAt = ""
	comparable.Signatures = nil
	return &comparable
}

func comparableSnapshotForRollbackAnchor(snapshot *NativeCooperativeTableSnapshot) *NativeCooperativeTableSnapshot {
	if snapshot == nil {
		return nil
	}
	comparable := cloneJSON(*snapshot)
	comparable.CreatedAt = ""
	comparable.PreviousSnapshotHash = nil
	comparable.Signatures = nil
	comparable.SnapshotID = ""
	return &comparable
}

func comparableSignedEvent(event NativeSignedTableEvent) NativeSignedTableEvent {
	return cloneJSON(event)
}

func unsignedSignedTableEvent(event NativeSignedTableEvent) map[string]any {
	unsigned := rawJSONMap(event)
	delete(unsigned, "signature")
	return unsigned
}

func unsignedSnapshot(snapshot NativeCooperativeTableSnapshot) map[string]any {
	unsigned := rawJSONMap(snapshot)
	delete(unsigned, "signatures")
	return unsigned
}

type nativeAcceptedEventHistory struct {
	ByHash   map[string]NativeSignedTableEvent
	Hashes   []string
	LastHash string
}

func (runtime *meshRuntime) acceptedEventHistory(table nativeTableState) (nativeAcceptedEventHistory, error) {
	history := nativeAcceptedEventHistory{
		ByHash: map[string]NativeSignedTableEvent{},
		Hashes: []string{},
	}
	expectedPrevHash := ""
	for index, event := range table.Events {
		if event.TableID != table.Config.TableID {
			return nativeAcceptedEventHistory{}, fmt.Errorf("event %d table id mismatch", index)
		}
		if event.NetworkID != table.Config.NetworkID {
			return nativeAcceptedEventHistory{}, fmt.Errorf("event %d network mismatch", index)
		}
		if event.ProtocolVersion != nativeProtocolVersion {
			return nativeAcceptedEventHistory{}, fmt.Errorf("event %d protocol version mismatch", index)
		}
		if event.Seq != index {
			return nativeAcceptedEventHistory{}, fmt.Errorf("event %d sequence mismatch", index)
		}
		if event.MessageType != stringValue(event.Body["type"]) {
			return nativeAcceptedEventHistory{}, fmt.Errorf("event %d message type mismatch", index)
		}
		if prevHash := strings.TrimSpace(stringValue(event.PrevEventHash)); prevHash != expectedPrevHash {
			return nativeAcceptedEventHistory{}, fmt.Errorf("event %d previous hash mismatch", index)
		}
		unsigned := unsignedSignedTableEvent(event)
		ok, err := settlementcore.VerifyStructuredData(event.SenderProtocolPubkeyHex, unsigned, event.Signature)
		if err != nil {
			return nativeAcceptedEventHistory{}, err
		}
		if !ok {
			return nativeAcceptedEventHistory{}, fmt.Errorf("event %d signature is invalid", index)
		}
		eventHash, err := settlementcore.HashStructuredDataHex(unsigned)
		if err != nil {
			return nativeAcceptedEventHistory{}, err
		}
		expectedPrevHash = eventHash
		history.ByHash[eventHash] = event
		history.Hashes = append(history.Hashes, eventHash)
	}
	if strings.TrimSpace(table.LastEventHash) != expectedPrevHash {
		return nativeAcceptedEventHistory{}, errors.New("table last event hash does not match accepted event history")
	}
	history.LastHash = expectedPrevHash
	return history, nil
}

func (runtime *meshRuntime) validateAcceptedEventHistory(table nativeTableState) error {
	_, err := runtime.acceptedEventHistory(table)
	return err
}

func validateAcceptedEventAnchor(label string, anchor any, history nativeAcceptedEventHistory) error {
	anchorHash := strings.TrimSpace(stringValue(anchor))
	if anchorHash == "" {
		if len(history.Hashes) == 0 {
			return nil
		}
		return fmt.Errorf("%s latest event hash is missing", label)
	}
	if _, ok := history.ByHash[anchorHash]; !ok {
		return fmt.Errorf("%s latest event hash does not match accepted event history", label)
	}
	return nil
}

func findEventByType(events []NativeSignedTableEvent, eventType string) (NativeSignedTableEvent, bool) {
	for _, event := range events {
		if stringValue(event.Body["type"]) == eventType {
			return event, true
		}
	}
	return NativeSignedTableEvent{}, false
}

func handHasAcceptedResultEvent(table nativeTableState, handID string) bool {
	trimmedHandID := strings.TrimSpace(handID)
	if trimmedHandID == "" {
		return false
	}
	for _, event := range table.Events {
		if stringValue(event.Body["type"]) != "HandResult" {
			continue
		}
		if strings.TrimSpace(stringValue(event.Body["handId"])) == trimmedHandID {
			return true
		}
	}
	return false
}

func findHandResultEventByCheckpoint(events []NativeSignedTableEvent, checkpointHash string) (NativeSignedTableEvent, bool) {
	for _, event := range events {
		if stringValue(event.Body["type"]) != "HandResult" {
			continue
		}
		if strings.TrimSpace(stringValue(event.Body["checkpointHash"])) == checkpointHash {
			return event, true
		}
	}
	return NativeSignedTableEvent{}, false
}

func findCheckpointAnchorEventByCheckpoint(events []NativeSignedTableEvent, checkpointHash string) (NativeSignedTableEvent, bool) {
	for _, event := range events {
		switch stringValue(event.Body["type"]) {
		case "HandResult", "CashOut", "EmergencyExit":
		default:
			continue
		}
		if strings.TrimSpace(stringValue(event.Body["checkpointHash"])) == checkpointHash {
			return event, true
		}
	}
	return NativeSignedTableEvent{}, false
}

func (runtime *meshRuntime) validateAcceptedEventAnchors(table nativeTableState, history nativeAcceptedEventHistory) error {
	if table.PublicState != nil {
		if err := validateAcceptedEventAnchor("public state", table.PublicState.LatestEventHash, history); err != nil {
			return err
		}
		if table.ActiveHand != nil && strings.TrimSpace(stringValue(table.PublicState.LatestEventHash)) != history.LastHash {
			return errors.New("active hand public state latest event hash does not match accepted event history")
		}
	}

	for index, snapshot := range table.Snapshots {
		if err := validateAcceptedEventAnchor(fmt.Sprintf("snapshot %d", index), snapshot.LatestEventHash, history); err != nil {
			return err
		}
		checkpointHash := runtime.snapshotHash(snapshot)
		if anchorEvent, ok := findCheckpointAnchorEventByCheckpoint(table.Events, checkpointHash); ok {
			if strings.TrimSpace(stringValue(snapshot.LatestEventHash)) != strings.TrimSpace(stringValue(anchorEvent.PrevEventHash)) {
				return fmt.Errorf("snapshot %d latest event hash does not match its %s anchor", index, strings.ToLower(stringValue(anchorEvent.Body["type"])))
			}
			continue
		}
		if snapshot.HandNumber == 0 && stringValue(snapshot.HandID) == "" {
			readyEvent, ok := findEventByType(table.Events, "TableReady")
			if !ok {
				return fmt.Errorf("snapshot %d is missing its table-ready event anchor", index)
			}
			if strings.TrimSpace(stringValue(snapshot.LatestEventHash)) != strings.TrimSpace(stringValue(readyEvent.PrevEventHash)) {
				return fmt.Errorf("snapshot %d latest event hash does not match its table-ready anchor", index)
			}
			continue
		}
		if index > 0 {
			previous := table.Snapshots[index-1]
			if reflect.DeepEqual(comparableSnapshotForRollbackAnchor(&snapshot), comparableSnapshotForRollbackAnchor(&previous)) {
				if strings.TrimSpace(stringValue(snapshot.LatestEventHash)) != strings.TrimSpace(stringValue(previous.LatestEventHash)) {
					return fmt.Errorf("snapshot %d latest event hash does not match its rollback anchor", index)
				}
				continue
			}
		}
		return fmt.Errorf("snapshot %d is not anchored by accepted event history", index)
	}

	if table.ActiveHand == nil && table.PublicState != nil && table.LatestSnapshot != nil {
		if table.LatestSnapshot.HandNumber == 0 && stringValue(table.LatestSnapshot.HandID) == "" {
			if strings.TrimSpace(stringValue(table.PublicState.LatestEventHash)) != history.LastHash {
				return errors.New("ready public state latest event hash does not match accepted event history")
			}
		} else if strings.TrimSpace(stringValue(table.PublicState.LatestEventHash)) != strings.TrimSpace(stringValue(table.LatestSnapshot.LatestEventHash)) {
			return errors.New("public state latest event hash does not match latest snapshot")
		}
	}
	return nil
}

func (runtime *meshRuntime) validateAcceptedSnapshotHistory(table nativeTableState, history nativeAcceptedEventHistory) error {
	expectedPreviousSnapshotID := ""
	seenSnapshotIDs := map[string]struct{}{}
	snapshotsByID := map[string]NativeCooperativeTableSnapshot{}
	for index, snapshot := range table.Snapshots {
		if snapshot.TableID != table.Config.TableID {
			return fmt.Errorf("snapshot %d table id mismatch", index)
		}
		if snapshot.ProtocolVersion != nativeProtocolVersion {
			return fmt.Errorf("snapshot %d protocol version mismatch", index)
		}
		if snapshot.Epoch > table.CurrentEpoch {
			return fmt.Errorf("snapshot %d epoch exceeds table epoch", index)
		}
		if previousSnapshotID := strings.TrimSpace(stringValue(snapshot.PreviousSnapshotHash)); previousSnapshotID != expectedPreviousSnapshotID {
			return fmt.Errorf("snapshot %d previous snapshot id mismatch", index)
		}
		if strings.TrimSpace(snapshot.SnapshotID) == "" {
			return fmt.Errorf("snapshot %d is missing snapshot id", index)
		}
		if _, exists := seenSnapshotIDs[snapshot.SnapshotID]; exists {
			return fmt.Errorf("duplicate snapshot id %q", snapshot.SnapshotID)
		}
		seenSnapshotIDs[snapshot.SnapshotID] = struct{}{}
		snapshotsByID[snapshot.SnapshotID] = snapshot

		unsigned := unsignedSnapshot(snapshot)
		if len(snapshot.Signatures) == 0 {
			return fmt.Errorf("snapshot %d is missing signatures", index)
		}
		for _, signature := range snapshot.Signatures {
			ok, err := settlementcore.VerifyStructuredData(signature.SignerPubkeyHex, unsigned, signature.SignatureHex)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("snapshot %d signature is invalid", index)
			}
		}
		expectedPreviousSnapshotID = snapshot.SnapshotID
	}

	if len(table.Snapshots) == 0 {
		if table.LatestSnapshot != nil || table.LatestFullySignedSnapshot != nil {
			return errors.New("latest snapshot pointers cannot be set without snapshot history")
		}
		return nil
	}
	if table.LatestSnapshot == nil {
		return errors.New("latest snapshot is missing from accepted snapshot history")
	}
	latestSnapshot, ok := snapshotsByID[table.LatestSnapshot.SnapshotID]
	if !ok {
		return errors.New("latest snapshot does not belong to accepted snapshot history")
	}
	if !reflect.DeepEqual(comparableSnapshotForHistory(table.LatestSnapshot), comparableSnapshotForHistory(&latestSnapshot)) {
		return errors.New("latest snapshot does not match accepted snapshot history")
	}
	if table.LatestFullySignedSnapshot != nil {
		latestFullySignedSnapshot, ok := snapshotsByID[table.LatestFullySignedSnapshot.SnapshotID]
		if !ok {
			return errors.New("latest fully signed snapshot does not belong to accepted snapshot history")
		}
		if !reflect.DeepEqual(comparableSnapshotForHistory(table.LatestFullySignedSnapshot), comparableSnapshotForHistory(&latestFullySignedSnapshot)) {
			return errors.New("latest fully signed snapshot does not match accepted snapshot history")
		}
	}
	return nil
}

func (runtime *meshRuntime) validateAcceptedHistoricalLedger(existing *nativeTableState, incoming nativeTableState) error {
	history, err := runtime.acceptedEventHistory(incoming)
	if err != nil {
		return err
	}
	if err := runtime.validateAcceptedCustodyHistory(existing, incoming); err != nil {
		return err
	}
	if err := runtime.validateAcceptedInitiatorHistory(incoming); err != nil {
		return err
	}
	if err := runtime.validateAcceptedSnapshotHistory(incoming, history); err != nil {
		return err
	}
	if err := runtime.validateAcceptedEventAnchors(incoming, history); err != nil {
		return err
	}
	if existing == nil {
		return nil
	}
	if len(incoming.Events) < len(existing.Events) {
		return errors.New("accepted history would roll back table events")
	}
	if len(incoming.Snapshots) < len(existing.Snapshots) {
		return errors.New("accepted history would roll back table snapshots")
	}
	for index := range existing.Events {
		if !reflect.DeepEqual(comparableSignedEvent(existing.Events[index]), comparableSignedEvent(incoming.Events[index])) {
			return fmt.Errorf("historical event %d was rewritten", index)
		}
	}
	for index := range existing.Snapshots {
		if !reflect.DeepEqual(comparableSnapshotForHistory(&existing.Snapshots[index]), comparableSnapshotForHistory(&incoming.Snapshots[index])) {
			return fmt.Errorf("historical snapshot %d was rewritten", index)
		}
	}
	return nil
}

func (runtime *meshRuntime) validateAcceptedCustodyHistory(existing *nativeTableState, incoming nativeTableState) error {
	if len(incoming.CustodyTransitions) == 0 {
		if incoming.LatestCustodyState != nil {
			return errors.New("latest custody state is not anchored by custody history")
		}
		return nil
	}
	var previous *tablecustody.CustodyState
	for index := range incoming.CustodyTransitions {
		transition := incoming.CustodyTransitions[index]
		if err := tablecustody.ValidateTransition(previous, transition); err != nil {
			return fmt.Errorf("custody transition %d is invalid: %w", index, err)
		}
		if err := runtime.validateAcceptedCustodyTransition(incoming, previous, transition); err != nil {
			return fmt.Errorf("custody transition %d proof is invalid: %w", index, err)
		}
		nextState := transition.NextState
		previous = &nextState
	}
	if incoming.LatestCustodyState == nil {
		return errors.New("latest custody state is missing from accepted custody history")
	}
	if previous != nil && !reflect.DeepEqual(cloneCustodyState(incoming.LatestCustodyState), cloneCustodyState(previous)) {
		return errors.New("latest custody state does not match accepted custody history")
	}
	if existing == nil {
		return nil
	}
	if len(incoming.CustodyTransitions) < len(existing.CustodyTransitions) {
		return errors.New("accepted history would roll back custody transitions")
	}
	for index := range existing.CustodyTransitions {
		if !reflect.DeepEqual(cloneJSON(existing.CustodyTransitions[index]), cloneJSON(incoming.CustodyTransitions[index])) {
			return fmt.Errorf("historical custody transition %d was rewritten", index)
		}
	}
	return nil
}

func (runtime *meshRuntime) validateAcceptedCustodyTransition(table nativeTableState, previous *tablecustody.CustodyState, transition tablecustody.CustodyTransition) error {
	requireArkSettlement := !runtime.config.UseMockSettlement &&
		custodyTransitionRequiresArkSettlement(previous, transition) &&
		!custodyTransitionUsesMockSettlement(transition)
	if transition.Proof.StateHash != transition.NextStateHash {
		return errors.New("custody proof state hash mismatch")
	}
	if strings.TrimSpace(transition.Proof.RequestHash) == "" {
		return errors.New("custody proof request hash is missing")
	}
	if transition.Proof.TransitionHash == "" {
		return errors.New("custody proof transition hash is missing")
	}
	if transition.Proof.ReplayValidated != true {
		return errors.New("custody proof must mark replay validation")
	}
	hasRecoveryWitness := transition.Proof.RecoveryWitness != nil
	if transition.ArkIntentID != "" && transition.Proof.ArkIntentID != "" && transition.Proof.ArkIntentID != transition.ArkIntentID {
		return errors.New("custody proof intent id mismatch")
	}
	if transition.ArkTxID != "" && transition.Proof.ArkTxID != "" && transition.Proof.ArkTxID != transition.ArkTxID {
		return errors.New("custody proof txid mismatch")
	}
	if requireArkSettlement &&
		!hasRecoveryWitness &&
		transition.Kind != tablecustody.TransitionKindEmergencyExit &&
		(strings.TrimSpace(transition.ArkIntentID) == "" || strings.TrimSpace(transition.ArkTxID) == "") {
		return errors.New("custody proof is missing Ark settlement ids")
	}
	baseTable := cloneJSON(table)
	baseTable.LatestCustodyState = previous
	approvalTransition, _, err := runtime.normalizedCustodyApprovalTransition(baseTable, transition)
	if err != nil {
		return err
	}
	if transition.Proof.RequestHash != approvalTransition.Proof.RequestHash {
		return errors.New("custody proof request hash mismatch")
	}
	expectedRecoverySourceRefs := sourcePotRecoveryRefs(&transition.NextState)
	for index, bundle := range transition.Proof.RecoveryBundles {
		if !sameCanonicalVTXORefs(bundle.SourcePotRefs, expectedRecoverySourceRefs) {
			return fmt.Errorf("custody recovery bundle %d source refs do not match the accepted transition", index)
		}
		if err := runtime.validateStoredRecoveryBundle(table, bundle); err != nil {
			return fmt.Errorf("custody recovery bundle %d is invalid: %w", index, err)
		}
	}
	required := runtime.requiredCustodySigners(table, transition)
	if err := runtime.validateCustodyApprovals(table, transition, required); err != nil {
		return err
	}
	if err := validateAcceptedCustodyRefs(previous, transition, requireArkSettlement && !hasRecoveryWitness); err != nil {
		return err
	}
	if hasRecoveryWitness {
		return runtime.validateAcceptedCustodyRecoveryWitness(table, previous, transition)
	}
	if !requireArkSettlement || transition.Kind == tablecustody.TransitionKindEmergencyExit {
		return nil
	}
	return runtime.validateAcceptedCustodySettlementWitness(table, previous, transition)
}

func (runtime *meshRuntime) validateCustodyApprovals(table nativeTableState, transition tablecustody.CustodyTransition, required []string) error {
	requestHash := strings.TrimSpace(transition.Proof.RequestHash)
	if requestHash == "" {
		return errors.New("custody proof request hash is missing")
	}
	approvalByPlayer := map[string]tablecustody.CustodySignature{}
	for _, approval := range transition.Approvals {
		if approval.PlayerID == "" {
			return errors.New("custody approval is missing player id")
		}
		if approval.SignatureHex == "" || approval.SignedAt == "" {
			return fmt.Errorf("custody approval for %s is incomplete", approval.PlayerID)
		}
		if approval.ApprovalHash != requestHash {
			return fmt.Errorf("custody approval for %s targets the wrong request", approval.PlayerID)
		}
		if _, ok := approvalByPlayer[approval.PlayerID]; ok {
			return fmt.Errorf("custody approval for %s is duplicated", approval.PlayerID)
		}
		seat, ok := seatRecordForPlayer(table, approval.PlayerID)
		if !ok {
			return fmt.Errorf("custody approval for unknown player %s", approval.PlayerID)
		}
		if approval.WalletPubkeyHex != "" && approval.WalletPubkeyHex != seat.WalletPubkeyHex {
			return fmt.Errorf("custody approval pubkey mismatch for %s", approval.PlayerID)
		}
		if err := verifyCustodyApproval(seat, transition, approval); err != nil {
			return fmt.Errorf("custody approval verification failed for %s: %w", approval.PlayerID, err)
		}
		approvalByPlayer[approval.PlayerID] = approval
	}
	if len(required) > 0 {
		if len(approvalByPlayer) != len(required) {
			return errors.New("custody approval set is incomplete")
		}
		for _, playerID := range required {
			if _, ok := approvalByPlayer[playerID]; !ok {
				return fmt.Errorf("missing custody approval for %s", playerID)
			}
		}
	}
	if len(transition.Proof.Signatures) != len(transition.Approvals) {
		return errors.New("custody proof signatures do not match approvals")
	}
	for _, signature := range transition.Proof.Signatures {
		approval, ok := approvalByPlayer[signature.PlayerID]
		if !ok || !reflect.DeepEqual(signature, approval) {
			return fmt.Errorf("custody proof signature mismatch for %s", signature.PlayerID)
		}
	}
	return nil
}

func validateAcceptedCustodyRefs(previous *tablecustody.CustodyState, transition tablecustody.CustodyTransition, requireArkProof bool) error {
	stackRefs := []tablecustody.VTXORef{}
	stackRefByKey := map[string]tablecustody.VTXORef{}
	seenRefs := map[string]tablecustody.VTXORef{}
	previousRefs := previousCustodyRefSet(previous)
	for _, claim := range transition.NextState.StackClaims {
		if sumVTXORefs(claim.VTXORefs) != stackClaimBackedAmount(claim) {
			return fmt.Errorf("custody stack refs do not match amount for %s", claim.PlayerID)
		}
		for _, ref := range claim.VTXORefs {
			if requireArkProof {
				if strings.TrimSpace(ref.ArkIntentID) == "" {
					return fmt.Errorf("custody stack ref is missing Ark intent for %s", claim.PlayerID)
				}
				if previousRef, carried := previousRefs[fundingRefKey(ref)]; !carried || !reflect.DeepEqual(previousRef, ref) {
					if ref.ArkIntentID != transition.ArkIntentID {
						return fmt.Errorf("custody stack ref Ark intent does not match transition for %s", claim.PlayerID)
					}
				}
				if strings.TrimSpace(ref.Script) == "" || len(ref.Tapscripts) == 0 {
					return fmt.Errorf("custody stack ref is missing script material for %s", claim.PlayerID)
				}
			}
			if ref.OwnerPlayerID != "" && ref.OwnerPlayerID != claim.PlayerID {
				return fmt.Errorf("custody stack ref owner mismatch for %s", claim.PlayerID)
			}
			key := fundingRefKey(ref)
			if existing, ok := seenRefs[key]; ok && !reflect.DeepEqual(existing, ref) {
				return fmt.Errorf("custody ref %s is reused inconsistently", key)
			}
			seenRefs[key] = ref
			stackRefByKey[key] = ref
			stackRefs = append(stackRefs, ref)
		}
	}
	for _, slice := range transition.NextState.PotSlices {
		if sumVTXORefs(slice.VTXORefs) != slice.TotalSats {
			return fmt.Errorf("custody pot slice refs do not match total for %s", slice.PotID)
		}
		for _, ref := range slice.VTXORefs {
			if requireArkProof {
				if strings.TrimSpace(ref.ArkIntentID) == "" {
					return fmt.Errorf("custody pot ref is missing Ark intent for %s", slice.PotID)
				}
				if previousRef, carried := previousRefs[fundingRefKey(ref)]; !carried || !reflect.DeepEqual(previousRef, ref) {
					if ref.ArkIntentID != transition.ArkIntentID {
						return fmt.Errorf("custody pot ref Ark intent does not match transition for %s", slice.PotID)
					}
				}
				if strings.TrimSpace(ref.Script) == "" || len(ref.Tapscripts) == 0 {
					return fmt.Errorf("custody pot ref is missing script material for %s", slice.PotID)
				}
			}
			key := fundingRefKey(ref)
			if existing, ok := seenRefs[key]; ok && !reflect.DeepEqual(existing, ref) {
				return fmt.Errorf("custody ref %s is reused inconsistently", key)
			}
			seenRefs[key] = ref
		}
	}
	if len(transition.Proof.VTXORefs) < len(stackRefs) {
		return errors.New("custody proof vtxo refs do not cover stack claims")
	}
	for _, ref := range transition.Proof.VTXORefs {
		key := fundingRefKey(ref)
		existing, ok := stackRefByKey[key]
		if ok && !reflect.DeepEqual(existing, ref) {
			return fmt.Errorf("custody proof references unknown vtxo %s", key)
		}
	}
	for _, ref := range stackRefs {
		key := fundingRefKey(ref)
		found := false
		for _, proofRef := range transition.Proof.VTXORefs {
			if fundingRefKey(proofRef) == key && reflect.DeepEqual(proofRef, ref) {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("custody proof is missing stack ref %s", key)
		}
	}
	return nil
}

func canonicalCustodyMoneyStacks(state *tablecustody.CustodyState) []tablecustody.StackClaim {
	if state == nil {
		return nil
	}
	stacks := append([]tablecustody.StackClaim(nil), state.StackClaims...)
	sort.SliceStable(stacks, func(left, right int) bool {
		if stacks[left].SeatIndex != stacks[right].SeatIndex {
			return stacks[left].SeatIndex < stacks[right].SeatIndex
		}
		return stacks[left].PlayerID < stacks[right].PlayerID
	})
	for index := range stacks {
		stacks[index].VTXORefs = append([]tablecustody.VTXORef(nil), stacks[index].VTXORefs...)
		sort.SliceStable(stacks[index].VTXORefs, func(left, right int) bool {
			return fundingRefKey(stacks[index].VTXORefs[left]) < fundingRefKey(stacks[index].VTXORefs[right])
		})
	}
	return stacks
}

func canonicalCustodyMoneyPots(state *tablecustody.CustodyState) []tablecustody.PotSlice {
	if state == nil {
		return nil
	}
	pots := append([]tablecustody.PotSlice(nil), state.PotSlices...)
	sort.SliceStable(pots, func(left, right int) bool {
		if pots[left].Sequence != pots[right].Sequence {
			return pots[left].Sequence < pots[right].Sequence
		}
		return pots[left].PotID < pots[right].PotID
	})
	for index := range pots {
		pots[index].EligiblePlayerIDs = append([]string(nil), pots[index].EligiblePlayerIDs...)
		sort.Strings(pots[index].EligiblePlayerIDs)
		pots[index].ContributedPlayerIDs = append([]string(nil), pots[index].ContributedPlayerIDs...)
		sort.Strings(pots[index].ContributedPlayerIDs)
		pots[index].WinnerPlayerIDs = append([]string(nil), pots[index].WinnerPlayerIDs...)
		sort.Strings(pots[index].WinnerPlayerIDs)
		pots[index].OddChipPlayerIDs = append([]string(nil), pots[index].OddChipPlayerIDs...)
		sort.Strings(pots[index].OddChipPlayerIDs)
		pots[index].VTXORefs = append([]tablecustody.VTXORef(nil), pots[index].VTXORefs...)
		sort.SliceStable(pots[index].VTXORefs, func(left, right int) bool {
			return fundingRefKey(pots[index].VTXORefs[left]) < fundingRefKey(pots[index].VTXORefs[right])
		})
	}
	return pots
}

func custodyTransitionRequiresArkSettlement(previous *tablecustody.CustodyState, transition tablecustody.CustodyTransition) bool {
	if previous == nil {
		return true
	}
	if !reflect.DeepEqual(canonicalCustodyMoneyStacks(previous), canonicalCustodyMoneyStacks(&transition.NextState)) {
		return true
	}
	return !reflect.DeepEqual(canonicalCustodyMoneyPots(previous), canonicalCustodyMoneyPots(&transition.NextState))
}

func custodyTransitionUsesMockSettlement(transition tablecustody.CustodyTransition) bool {
	intentID := firstNonEmptyString(transition.Proof.ArkIntentID, transition.ArkIntentID)
	return strings.HasPrefix(intentID, "mock-intent-")
}

func previousSnapshotForCurrentState(table nativeTableState) nativeTableState {
	base := cloneJSON(table)
	if base.ActiveHand == nil || base.ActiveHand.State.Phase != game.StreetSettled {
		return base
	}
	handID := base.ActiveHand.State.HandID
	if base.LatestSnapshot == nil || stringValue(base.LatestSnapshot.HandID) != handID {
		return base
	}
	if len(base.Snapshots) > 0 && stringValue(base.Snapshots[len(base.Snapshots)-1].HandID) == handID {
		base.Snapshots = append([]NativeCooperativeTableSnapshot(nil), base.Snapshots[:len(base.Snapshots)-1]...)
	}
	if len(base.Snapshots) == 0 {
		base.LatestSnapshot = nil
		base.LatestFullySignedSnapshot = nil
		return base
	}
	latest := cloneJSON(base.Snapshots[len(base.Snapshots)-1])
	base.LatestSnapshot = &latest
	base.LatestFullySignedSnapshot = &latest
	return base
}

func (runtime *meshRuntime) validateAcceptedPublicReplay(table nativeTableState) error {
	if table.ActiveHand == nil {
		return nil
	}
	replayedState, err := runtime.replayAcceptedHandState(table)
	if err != nil {
		return fmt.Errorf("active hand replay validation failed: %w", err)
	}
	if !reflect.DeepEqual(cloneJSON(table.ActiveHand.State), cloneJSON(replayedState)) {
		return fmt.Errorf("active hand state does not match transcript replay: phase=%s expectedBoard=%v gotBoard=%v expectedWinners=%+v gotWinners=%+v expectedPlayers=%+v gotPlayers=%+v", replayedState.Phase, replayedState.Board, table.ActiveHand.State.Board, replayedState.Winners, table.ActiveHand.State.Winners, replayedState.Players, table.ActiveHand.State.Players)
	}
	if table.PublicState == nil {
		return errors.New("active hand is missing public state")
	}

	base := previousSnapshotForCurrentState(table)
	expectedPublic := runtime.publicStateFromHand(base, replayedState)
	comparableActual := comparablePublicState(table.PublicState)
	comparableExpected := comparablePublicState(&expectedPublic)
	if !reflect.DeepEqual(comparableActual, comparableExpected) {
		return fmt.Errorf("public state does not match transcript replay: expected=%+v got=%+v", comparableExpected, comparableActual)
	}
	if replayedState.Phase != game.StreetSettled {
		return nil
	}
	if !handHasAcceptedResultEvent(table, replayedState.HandID) {
		return nil
	}

	if table.LatestSnapshot == nil {
		return errors.New("settled hand is missing latest snapshot")
	}
	if table.LatestFullySignedSnapshot == nil {
		return errors.New("settled hand is missing latest fully signed snapshot")
	}
	if _, abortedHand := acceptedAbortSeatIndex(table); abortedHand {
		return nil
	}
	expectedSnapshot, err := runtime.buildSnapshot(base, expectedPublic)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(comparableSnapshotForReplay(table.LatestSnapshot), comparableSnapshotForReplay(&expectedSnapshot)) {
		return errors.New("latest snapshot does not match transcript replay")
	}
	if !reflect.DeepEqual(comparableSnapshotForReplay(table.LatestFullySignedSnapshot), comparableSnapshotForReplay(&expectedSnapshot)) {
		return errors.New("latest fully signed snapshot does not match transcript replay")
	}
	return nil
}

func (runtime *meshRuntime) buildSnapshot(table nativeTableState, publicState NativePublicTableState) (NativeCooperativeTableSnapshot, error) {
	snapshot := NativeCooperativeTableSnapshot{
		CreatedAt:            nowISO(),
		ChipBalances:         cloneJSON(publicState.ChipBalances),
		DealerCommitmentRoot: nil,
		Epoch:                table.CurrentEpoch,
		FoldedPlayerIDs:      append([]string{}, publicState.FoldedPlayerIDs...),
		HandID:               publicState.HandID,
		HandNumber:           publicState.HandNumber,
		LatestEventHash:      publicState.LatestEventHash,
		LivePlayerIDs:        append([]string{}, publicState.LivePlayerIDs...),
		Phase:                publicState.Phase,
		PotSats:              publicState.PotSats,
		PreviousSnapshotHash: previousSnapshotHash(table),
		ProtocolVersion:      nativeProtocolVersion,
		SeatedPlayers:        cloneJSON(publicState.SeatedPlayers),
		SidePots:             []int{},
		Signatures:           []NativeTableSnapshotSignature{},
		SnapshotID:           randomUUID(),
		TableID:              table.Config.TableID,
		TurnIndex:            publicState.ActingSeatIndex,
	}
	if publicState.DealerCommitment != nil {
		snapshot.DealerCommitmentRoot = publicState.DealerCommitment.RootHash
	}
	unsigned := rawJSONMap(snapshot)
	delete(unsigned, "signatures")
	signatureHex, err := settlementcore.SignStructuredData(runtime.protocolIdentity.PrivateKeyHex, unsigned)
	if err != nil {
		return NativeCooperativeTableSnapshot{}, err
	}
	snapshot.Signatures = []NativeTableSnapshotSignature{{
		SignatureHex:    signatureHex,
		SignedAt:        nowISO(),
		SignerPeerID:    runtime.selfPeerID(),
		SignerPubkeyHex: runtime.protocolIdentity.PublicKeyHex,
		SignerRole:      runtime.mode,
	}}
	return snapshot, nil
}

func (runtime *meshRuntime) snapshotHash(snapshot NativeCooperativeTableSnapshot) string {
	unsigned := rawJSONMap(snapshot)
	delete(unsigned, "signatures")
	hash, err := settlementcore.HashStructuredDataHex(unsigned)
	if err != nil {
		return ""
	}
	return hash
}

func (runtime *meshRuntime) localTableView(table nativeTableState) NativeMeshTableView {
	var mySeatIndex any
	var myPlayerID any
	var myHoleCards any
	canAct := false
	legalActions := []game.LegalAction{}
	if runtime.walletID.PlayerID != "" {
		myPlayerID = runtime.walletID.PlayerID
	}

	for _, seat := range table.Seats {
		if seat.PlayerID == runtime.walletID.PlayerID {
			mySeatIndex = seat.SeatIndex
			myPlayerID = seat.PlayerID
			if table.ActiveHand != nil {
				if privateState, err := runtime.readTablePrivateState(table.Config.TableID); err == nil {
					if cards, ok := privateState.MyHoleCardsByHandID[table.ActiveHand.State.HandID]; ok && len(cards) == 2 {
						myHoleCards = append([]string(nil), cards...)
					}
				}
				seatIndex := seat.SeatIndex
				canAct = table.ActiveHand.State.ActingSeatIndex != nil && *table.ActiveHand.State.ActingSeatIndex == seat.SeatIndex
				if canAct {
					legalActions = game.GetLegalActions(table.ActiveHand.State, &seatIndex)
				}
			}
			break
		}
	}

	return NativeMeshTableView{
		Config:                    table.Config,
		CustodyTransitions:        cloneJSON(table.CustodyTransitions),
		Events:                    cloneJSON(table.Events),
		LatestCustodyState:        cloneCustodyState(table.LatestCustodyState),
		LatestFullySignedSnapshot: cloneSnapshot(table.LatestFullySignedSnapshot),
		LatestSnapshot:            cloneSnapshot(table.LatestSnapshot),
		Local: NativeTableLocalView{
			CanAct:       canAct,
			LegalActions: legalActions,
			MyHoleCards:  myHoleCards,
			MyPlayerID:   myPlayerID,
			MySeatIndex:  mySeatIndex,
		},
		PublicState: clonePublicState(table.PublicState),
	}
}

func (runtime *meshRuntime) tableSummary(table nativeTableState) NativeTableSummary {
	phase := any(nil)
	handNumber := 0
	if table.PublicState != nil {
		phase = table.PublicState.Phase
		handNumber = table.PublicState.HandNumber
	}
	snapshotID := ""
	if table.LatestSnapshot != nil {
		snapshotID = table.LatestSnapshot.SnapshotID
	}
	custodyHash := latestCustodyStateHash(table)
	custodySeq := 0
	if table.LatestCustodyState != nil {
		custodySeq = table.LatestCustodyState.CustodySeq
	}
	return NativeTableSummary{
		CurrentEpoch:           table.CurrentEpoch,
		CustodySeq:             custodySeq,
		HandNumber:             handNumber,
		HostPeerID:             table.CurrentHost.Peer.PeerID,
		LatestCustodyStateHash: custodyHash,
		LatestSnapshotID:       snapshotID,
		Phase:                  phase,
		Role:                   runtime.roleForTable(table),
		Status:                 table.Config.Status,
		TableID:                table.Config.TableID,
		TableName:              table.Config.Name,
		Visibility:             table.Config.Visibility,
	}
}

func (runtime *meshRuntime) roleForTable(table nativeTableState) string {
	if table.CurrentHost.Peer.PeerID == runtime.selfPeerID() {
		return "host"
	}
	for _, witness := range table.Witnesses {
		if witness.Peer.PeerID == runtime.selfPeerID() {
			return "witness"
		}
	}
	return "player"
}

func (runtime *meshRuntime) knownPeers() ([]NativePeerAddress, error) {
	profile, err := runtime.loadProfileState()
	if err != nil {
		return nil, err
	}
	peers := make([]NativePeerAddress, 0, len(profile.KnownPeers))
	for _, peer := range profile.KnownPeers {
		peers = append(peers, NativePeerAddress{
			Alias:             peer.Alias,
			LastSeenAt:        peer.LastSeenAt,
			PeerID:            peer.PeerID,
			PeerURL:           peer.PeerURL,
			ProtocolPubkeyHex: peer.ProtocolPubkeyHex,
			RelayPeerID:       peer.RelayPeerID,
			Roles:             append([]string{}, peer.Roles...),
		})
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i].PeerID < peers[j].PeerID })
	return peers, nil
}

func (runtime *meshRuntime) saveKnownPeer(peer NativePeerAddress) error {
	profile, err := runtime.loadProfileState()
	if err != nil {
		return err
	}
	next := []walletpkg.KnownPeerState{}
	replaced := false
	for _, existing := range profile.KnownPeers {
		if existing.PeerID == peer.PeerID || existing.PeerURL == peer.PeerURL {
			next = append(next, walletpkg.KnownPeerState{
				Alias:             peer.Alias,
				LastSeenAt:        peer.LastSeenAt,
				PeerID:            peer.PeerID,
				PeerURL:           peer.PeerURL,
				ProtocolPubkeyHex: peer.ProtocolPubkeyHex,
				RelayPeerID:       peer.RelayPeerID,
				Roles:             append([]string{}, peer.Roles...),
			})
			replaced = true
			continue
		}
		next = append(next, existing)
	}
	if !replaced {
		next = append(next, walletpkg.KnownPeerState{
			Alias:             peer.Alias,
			LastSeenAt:        peer.LastSeenAt,
			PeerID:            peer.PeerID,
			PeerURL:           peer.PeerURL,
			ProtocolPubkeyHex: peer.ProtocolPubkeyHex,
			RelayPeerID:       peer.RelayPeerID,
			Roles:             append([]string{}, peer.Roles...),
		})
	}
	profile.KnownPeers = next
	return runtime.profileStore.Save(*profile)
}

func (runtime *meshRuntime) loadProfileState() (*walletpkg.PlayerProfileState, error) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return runtime.loadProfileStateLocked()
}

func (runtime *meshRuntime) loadProfileStateLocked() (*walletpkg.PlayerProfileState, error) {
	state, err := runtime.profileStore.Load(runtime.profileName)
	if err != nil {
		return nil, err
	}
	if state == nil {
		return &walletpkg.PlayerProfileState{
			HandSeeds:   map[string]string{},
			KnownPeers:  []walletpkg.KnownPeerState{},
			MeshTables:  map[string]walletpkg.MeshTableReferenceState{},
			Nickname:    runtime.profileName,
			ProfileName: runtime.profileName,
		}, nil
	}
	if state.HandSeeds == nil {
		state.HandSeeds = map[string]string{}
	}
	if state.KnownPeers == nil {
		state.KnownPeers = []walletpkg.KnownPeerState{}
	}
	if state.MeshTables == nil {
		state.MeshTables = map[string]walletpkg.MeshTableReferenceState{}
	}
	return state, nil
}

func (runtime *meshRuntime) currentTableID() string {
	profile, err := runtime.loadProfileState()
	if err != nil {
		return ""
	}
	if profile.CurrentTable != nil {
		return profile.CurrentTable.TableID
	}
	return profile.CurrentMeshTableID
}

func (runtime *meshRuntime) acceptRemoteTable(table nativeTableState) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("accept remote table: %v", recovered)
		}
	}()

	existing, err := runtime.store.readTable(table.Config.TableID)
	if err != nil {
		return err
	}
	accepted := cloneJSON(table)
	if err := runtime.normalizeAcceptedActiveHand(existing, &accepted); err != nil {
		return err
	}
	if err := runtime.validateAcceptedRemoteTable(existing, accepted); err != nil {
		return err
	}
	if err := runtime.store.writeTable(&accepted); err != nil {
		return err
	}
	if err := runtime.store.rewriteEvents(accepted.Config.TableID, accepted.Events); err != nil {
		return err
	}
	if err := runtime.store.rewriteSnapshots(accepted.Config.TableID, accepted.Snapshots); err != nil {
		return err
	}
	if accepted.Advertisement != nil {
		_ = runtime.store.upsertPublicAd(*accepted.Advertisement)
	}
	if accepted.CurrentHost.Peer.PeerURL != "" {
		_ = runtime.saveKnownPeer(accepted.CurrentHost.Peer)
	}
	for _, witness := range accepted.Witnesses {
		if witness.Peer.PeerURL != "" {
			_ = runtime.saveKnownPeer(witness.Peer)
		}
	}
	for _, seat := range accepted.Seats {
		if seat.PeerURL == "" {
			continue
		}
		_ = runtime.saveKnownPeer(NativePeerAddress{
			Alias:             seat.Nickname,
			LastSeenAt:        nowISO(),
			PeerID:            seat.PeerID,
			PeerURL:           seat.PeerURL,
			ProtocolPubkeyHex: seat.ProtocolPubkeyHex,
		})
	}
	profile, err := runtime.loadProfileState()
	if err != nil {
		return err
	}
	previousReference := profile.MeshTables[accepted.Config.TableID]
	profile.CurrentMeshTableID = accepted.Config.TableID
	profile.CurrentTable = &walletpkg.TableSessionState{
		InviteCode: accepted.InviteCode,
		SeatIndex:  runtime.seatIndexForPlayer(accepted),
		TableID:    accepted.Config.TableID,
	}
	profile.MeshTables[accepted.Config.TableID] = walletpkg.MeshTableReferenceState{
		Config:       MustMarshalJSON(accepted.Config),
		CurrentEpoch: accepted.CurrentEpoch,
		CreatedAt:    firstNonEmptyString(accepted.Config.CreatedAt, previousReference.CreatedAt),
		CreatedByMe:  previousReference.CreatedByMe,
		HostPeerID:   accepted.CurrentHost.Peer.PeerID,
		HostPeerURL:  accepted.CurrentHost.Peer.PeerURL,
		InviteCode:   firstNonEmptyString(accepted.InviteCode, previousReference.InviteCode),
		Role:         runtime.roleForTable(accepted),
		TableID:      accepted.Config.TableID,
		Visibility:   accepted.Config.Visibility,
	}
	if err := runtime.profileStore.Save(*profile); err != nil {
		return err
	}
	if err := runtime.syncPrivateAndFunds(accepted); err != nil {
		return err
	}
	return nil
}

func (runtime *meshRuntime) saveCreatedTableReference(table nativeTableState, inviteCode string) error {
	profile, err := runtime.loadProfileState()
	if err != nil {
		return err
	}
	profile.MeshTables[table.Config.TableID] = walletpkg.MeshTableReferenceState{
		Config:       MustMarshalJSON(table.Config),
		CurrentEpoch: table.CurrentEpoch,
		CreatedAt:    table.Config.CreatedAt,
		CreatedByMe:  true,
		HostPeerID:   table.CurrentHost.Peer.PeerID,
		HostPeerURL:  table.CurrentHost.Peer.PeerURL,
		InviteCode:   inviteCode,
		Role:         "host",
		TableID:      table.Config.TableID,
		Visibility:   table.Config.Visibility,
	}
	return runtime.profileStore.Save(*profile)
}

func (runtime *meshRuntime) createdTableEntry(reference walletpkg.MeshTableReferenceState) (NativeCreatedTableEntry, error) {
	config, err := decodeCreatedTableConfig(reference)
	if err != nil {
		return NativeCreatedTableEntry{}, err
	}
	summary := NativeTableSummary{
		CurrentEpoch: reference.CurrentEpoch,
		HostPeerID:   reference.HostPeerID,
		Role:         reference.Role,
		Status:       config.Status,
		TableID:      reference.TableID,
		TableName:    config.Name,
		Visibility:   firstNonEmptyString(reference.Visibility, config.Visibility),
	}

	table, err := runtime.store.readTable(reference.TableID)
	if err != nil {
		return NativeCreatedTableEntry{}, err
	}
	if table != nil {
		config = table.Config
		summary = runtime.tableSummary(*table)
	}

	inviteCode := reference.InviteCode
	if inviteCode == "" && table != nil {
		inviteCode = table.InviteCode
	}

	return NativeCreatedTableEntry{
		Config:     config,
		InviteCode: inviteCode,
		Summary:    summary,
	}, nil
}

func (runtime *meshRuntime) seatIndexForPlayer(table nativeTableState) int {
	for _, seat := range table.Seats {
		if seat.PlayerID == runtime.walletID.PlayerID {
			return seat.SeatIndex
		}
	}
	return -1
}

func (runtime *meshRuntime) buildTableSyncRequest(table nativeTableState) (nativeTableSyncRequest, error) {
	sentAt := nowISO()
	tableHash, err := settlementcore.HashStructuredDataHex(table)
	if err != nil {
		return nativeTableSyncRequest{}, err
	}
	request := nativeTableSyncRequest{
		SenderPeerID:            runtime.selfPeerID(),
		SenderProtocolPubkeyHex: runtime.protocolIdentity.PublicKeyHex,
		SentAt:                  sentAt,
		Table:                   table,
	}
	signatureHex, err := settlementcore.SignStructuredData(runtime.protocolIdentity.PrivateKeyHex, nativeTableSyncAuthPayload(table.Config.TableID, request.SenderPeerID, tableHash, sentAt))
	if err != nil {
		return nativeTableSyncRequest{}, err
	}
	request.SignatureHex = signatureHex
	return request, nil
}

func (runtime *meshRuntime) acceptTableSync(request nativeTableSyncRequest) error {
	if err := runtime.validateTableSyncRequest(request); err != nil {
		return err
	}
	return runtime.acceptRemoteTable(request.Table)
}

func (runtime *meshRuntime) postJSON(endpoint, path string, input any) ([]byte, error) {
	body, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequest(http.MethodPost, strings.TrimSuffix(endpoint, "/")+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := runtime.httpClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("%s returned %d: %s", path, response.StatusCode, strings.TrimSpace(string(data)))
	}
	return data, nil
}

func (runtime *meshRuntime) shouldPollHost(tableID string) bool {
	last := runtime.lastSyncAt[tableID]
	return time.Since(last) >= nativeTableSyncInterval
}

func isStaleRemoteTableError(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	return strings.Contains(message, "would roll back table epoch") ||
		strings.Contains(message, "would roll back table events") ||
		strings.Contains(message, "would roll back table snapshots") ||
		strings.Contains(message, "would roll back custody transitions")
}

func (runtime *meshRuntime) shouldHandleFailover(table nativeTableState) bool {
	if table.CurrentHost.Peer.PeerID == runtime.selfPeerID() {
		return false
	}
	for _, witness := range table.Witnesses {
		if witness.Peer.PeerID == runtime.selfPeerID() {
			return true
		}
	}
	if len(table.Witnesses) > 0 {
		return false
	}
	if runtime.seatIndexForPlayer(table) < 0 {
		return false
	}
	candidate, ok := lowestEligibleFailoverSeat(table)
	return ok && candidate.PeerID == runtime.selfPeerID()
}

func (runtime *meshRuntime) knownPeerURL(peerID string) string {
	peers, err := runtime.knownPeers()
	if err != nil {
		return ""
	}
	for _, peer := range peers {
		if peer.PeerID == peerID {
			return peer.PeerURL
		}
	}
	return ""
}

func (runtime *meshRuntime) buildSignedActionRequest(table nativeTableState, action game.Action) (nativeActionRequest, error) {
	decisionIndex, err := nativeActionDecisionIndex(table)
	if err != nil {
		return nativeActionRequest{}, err
	}
	handID := ""
	if table.ActiveHand != nil {
		handID = table.ActiveHand.State.HandID
	}
	if handID == "" && table.PublicState != nil {
		handID = stringValue(table.PublicState.HandID)
	}
	if handID == "" {
		return nativeActionRequest{}, errors.New("hand is not active")
	}
	signedAt := nowISO()
	challengeAnchor, transcriptRoot := runtime.currentCustodyTranscriptBindings(table)
	request := nativeActionRequest{
		Action:               action,
		ChallengeAnchor:      challengeAnchor,
		DecisionIndex:        decisionIndex,
		Epoch:                table.CurrentEpoch,
		HandID:               handID,
		PlayerID:             runtime.walletID.PlayerID,
		PrevCustodyStateHash: latestCustodyStateHash(table),
		ProfileName:          runtime.profileName,
		SignedAt:             signedAt,
		TableID:              table.Config.TableID,
		TranscriptRoot:       transcriptRoot,
	}
	signatureHex, err := settlementcore.SignStructuredData(runtime.walletID.PrivateKeyHex, nativeActionAuthPayload(request.TableID, request.PlayerID, request.HandID, request.PrevCustodyStateHash, request.ChallengeAnchor, request.TranscriptRoot, request.Epoch, request.DecisionIndex, request.Action, request.SignedAt))
	if err != nil {
		return nativeActionRequest{}, err
	}
	request.SignatureHex = signatureHex
	return request, nil
}

func (runtime *meshRuntime) validateJoinRequest(table nativeTableState, join nativeJoinRequest) error {
	if join.BuyInSats <= 0 {
		return errors.New("join request buy-in must be positive")
	}
	if len(join.FundingRefs) == 0 || join.FundingTotalSats < join.BuyInSats {
		return errors.New("join request is missing funded buy-in refs")
	}
	seenFundingRefs := map[string]struct{}{}
	for _, ref := range join.FundingRefs {
		if strings.TrimSpace(ref.TxID) == "" {
			return errors.New("join request funding ref is missing txid")
		}
		if ref.AmountSats <= 0 {
			return errors.New("join request funding ref amount must be positive")
		}
		if !runtime.config.UseMockSettlement {
			if strings.TrimSpace(ref.Script) == "" || len(ref.Tapscripts) == 0 {
				return errors.New("join request funding ref is missing Ark script material")
			}
		}
		if ref.OwnerPlayerID != "" && ref.OwnerPlayerID != join.WalletPlayerID {
			return errors.New("join request funding ref owner mismatch")
		}
		key := fundingRefKey(ref)
		if _, ok := seenFundingRefs[key]; ok {
			return errors.New("join request reuses a funding ref")
		}
		seenFundingRefs[key] = struct{}{}
	}
	for _, seat := range table.Seats {
		if seat.Status == "pending-join" && seat.PlayerID == join.WalletPlayerID && seat.PeerID == join.Peer.PeerID {
			continue
		}
		for _, ref := range seat.FundingRefs {
			if _, ok := seenFundingRefs[fundingRefKey(ref)]; ok {
				return errors.New("join request funding refs are already locked at this table")
			}
		}
	}
	if table.LatestCustodyState != nil {
		for _, claim := range table.LatestCustodyState.StackClaims {
			for _, ref := range claim.VTXORefs {
				if _, ok := seenFundingRefs[fundingRefKey(ref)]; ok {
					return errors.New("join request funding refs overlap active custody claims")
				}
			}
		}
	}
	if !runtime.config.UseMockSettlement {
		if err := runtime.verifyCustodyRefsOnArk(join.FundingRefs, true); err != nil {
			return fmt.Errorf("join request funding refs failed Ark verification: %w", err)
		}
	}
	if join.IdentityBinding.TableID == "" || join.IdentityBinding.SignatureHex == "" {
		return errors.New("join request is missing identity binding")
	}
	if join.IdentityBinding.TableID != join.TableID {
		return errors.New("join request table binding mismatch")
	}
	if join.IdentityBinding.PeerID != join.Peer.PeerID {
		return errors.New("join request peer binding mismatch")
	}
	if join.IdentityBinding.PeerURL != join.Peer.PeerURL {
		return errors.New("join request peer URL binding mismatch")
	}
	if join.IdentityBinding.ProtocolID != join.ProtocolID {
		return errors.New("join request protocol binding mismatch")
	}
	if join.IdentityBinding.ProtocolPubkeyHex != join.Peer.ProtocolPubkeyHex {
		return errors.New("join request protocol key mismatch")
	}
	if join.IdentityBinding.WalletPlayerID != join.WalletPlayerID {
		return errors.New("join request player binding mismatch")
	}
	if join.IdentityBinding.WalletPubkeyHex != join.WalletPubkeyHex {
		return errors.New("join request wallet key mismatch")
	}
	expectedPlayerID, err := settlementcore.DerivePlayerID(join.WalletPubkeyHex)
	if err != nil {
		return err
	}
	if expectedPlayerID != join.WalletPlayerID {
		return errors.New("join request player id does not match wallet key")
	}
	expectedProtocolID, err := settlementcore.DeriveScopedID(settlementcore.ProtocolIdentityScope, join.Peer.ProtocolPubkeyHex)
	if err != nil {
		return err
	}
	if expectedProtocolID != join.ProtocolID {
		return errors.New("join request protocol id does not match protocol key")
	}
	ok, err := settlementcore.VerifyIdentityBinding(join.IdentityBinding)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("join request identity binding is invalid")
	}
	peerSelf, err := runtime.fetchPeerInfo(join.Peer.PeerURL)
	if err != nil {
		return fmt.Errorf("join request peer endpoint verification failed: %w", err)
	}
	if peerSelf.Peer.PeerID != join.Peer.PeerID ||
		peerSelf.Peer.ProtocolPubkeyHex != join.Peer.ProtocolPubkeyHex ||
		peerSelf.ProtocolID != join.ProtocolID ||
		peerSelf.WalletPlayerID != join.WalletPlayerID {
		return errors.New("join request peer endpoint does not serve the claimed identity")
	}
	return nil
}

func (runtime *meshRuntime) validateTableSyncRequest(request nativeTableSyncRequest) error {
	if request.Table.Config.TableID == "" || request.SenderPeerID == "" || request.SenderProtocolPubkeyHex == "" || request.SentAt == "" || request.SignatureHex == "" {
		return errors.New("sync request is missing required authentication fields")
	}
	if !timestampWithinWindow(request.SentAt, nativeTableSyncAuthMaxAge) {
		return errors.New("sync request is stale")
	}
	if request.SenderPeerID != request.Table.CurrentHost.Peer.PeerID {
		return errors.New("sync request sender does not match table host")
	}
	if request.SenderProtocolPubkeyHex != request.Table.CurrentHost.Peer.ProtocolPubkeyHex {
		return errors.New("sync request protocol key does not match table host")
	}
	tableHash, err := settlementcore.HashStructuredDataHex(request.Table)
	if err != nil {
		return err
	}
	ok, err := settlementcore.VerifyStructuredData(request.SenderProtocolPubkeyHex, nativeTableSyncAuthPayload(request.Table.Config.TableID, request.SenderPeerID, tableHash, request.SentAt), request.SignatureHex)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("sync request signature is invalid")
	}
	existing, err := runtime.store.readTable(request.Table.Config.TableID)
	if err != nil {
		return err
	}
	return runtime.validateSyncTransition(existing, request.Table, request.SenderPeerID)
}

func (runtime *meshRuntime) validateActionRequest(table nativeTableState, seat nativeSeatRecord, request nativeActionRequest) error {
	if table.ActiveHand == nil {
		return errors.New("action request requires an active hand")
	}
	if request.TableID != table.Config.TableID {
		return errors.New("action request table mismatch")
	}
	if request.PlayerID != seat.PlayerID {
		return errors.New("action request player mismatch")
	}
	if request.HandID == "" || request.HandID != table.ActiveHand.State.HandID {
		return errors.New("action request hand mismatch")
	}
	if request.Epoch != table.CurrentEpoch {
		return errors.New("action request epoch mismatch")
	}
	expectedDecisionIndex, err := nativeActionDecisionIndex(table)
	if err != nil {
		return err
	}
	if request.DecisionIndex != expectedDecisionIndex {
		return errors.New("action request decision mismatch")
	}
	if request.PrevCustodyStateHash != latestCustodyStateHash(table) {
		return errors.New("action request custody mismatch")
	}
	expectedChallengeAnchor, expectedTranscriptRoot := runtime.currentCustodyTranscriptBindings(table)
	if request.ChallengeAnchor != expectedChallengeAnchor {
		return errors.New("action request challenge anchor mismatch")
	}
	if request.TranscriptRoot != expectedTranscriptRoot {
		return errors.New("action request transcript root mismatch")
	}
	return verifyNativeActionRequestSignature(seat, request)
}

func (runtime *meshRuntime) validateFundsRequest(table nativeTableState, seat nativeSeatRecord, request nativeFundsRequest) error {
	if err := validateSettlementRequestProtocolVersion(request.ProtocolVersion); err != nil {
		return err
	}
	if request.TableID != table.Config.TableID {
		return errors.New("funds request table mismatch")
	}
	if request.PlayerID != seat.PlayerID {
		return errors.New("funds request player mismatch")
	}
	if request.Epoch != table.CurrentEpoch {
		return errors.New("funds request epoch mismatch")
	}
	if request.PrevCustodyStateHash != latestCustodyStateHash(table) {
		return errors.New("funds request custody mismatch")
	}
	if _, _, err := fundsTransitionKindAndStatus(request.Kind); err != nil {
		return err
	}
	if request.Kind == "cashout" && strings.TrimSpace(request.ArkAddress) == "" {
		return errors.New("funds request is missing cash-out Ark address")
	}
	if request.Kind != string(tablecustody.TransitionKindEmergencyExit) && request.ExitExecution != nil {
		return errors.New("funds request includes unexpected exit execution proof")
	}
	if request.Kind == string(tablecustody.TransitionKindEmergencyExit) {
		if err := runtime.validateEmergencyExitExecution(table, request); err != nil {
			return err
		}
	}
	return verifyNativeFundsRequestSignature(seat, request)
}

func (runtime *meshRuntime) tableViewerPlayerID(request *http.Request, table nativeTableState) string {
	playerID := strings.TrimSpace(request.Header.Get(nativeTableAuthPlayerIDHeader))
	signedAt := strings.TrimSpace(request.Header.Get(nativeTableAuthSignedAtHeader))
	signatureHex := strings.TrimSpace(request.Header.Get(nativeTableAuthSignatureHeader))
	if playerID == "" || signedAt == "" || signatureHex == "" {
		return ""
	}
	if !timestampWithinWindow(signedAt, nativeTableFetchAuthMaxAge) {
		return ""
	}
	seat, ok := seatRecordForPlayer(table, playerID)
	if !ok || strings.TrimSpace(seat.WalletPubkeyHex) == "" {
		return ""
	}
	ok, err := settlementcore.VerifyStructuredData(seat.WalletPubkeyHex, nativeTableFetchAuthPayload(table.Config.TableID, playerID, signedAt), signatureHex)
	if err != nil || !ok {
		return ""
	}
	return playerID
}

func (runtime *meshRuntime) validateSyncTransition(existing *nativeTableState, incoming nativeTableState, senderPeerID string) error {
	if !runtime.isLocalTableParticipant(incoming) {
		return errors.New("sync request does not target this daemon")
	}
	if err := validateAcceptedTableProtocolVersion(incoming); err != nil {
		return err
	}
	normalized := cloneJSON(incoming)
	if err := runtime.normalizeAcceptedActiveHand(existing, &normalized); err != nil {
		return err
	}
	if err := runtime.validateAcceptedHandTranscript(normalized); err != nil {
		return err
	}
	if err := runtime.validateAcceptedPublicReplay(normalized); err != nil {
		return err
	}
	if err := runtime.validateAcceptedHistoricalLedger(existing, normalized); err != nil {
		return err
	}
	if existing == nil {
		return nil
	}
	if incoming.CurrentEpoch < existing.CurrentEpoch {
		return errors.New("sync request would roll back table epoch")
	}
	if incoming.CurrentEpoch == existing.CurrentEpoch {
		if senderPeerID != existing.CurrentHost.Peer.PeerID {
			return errors.New("sync request sender does not match the current host")
		}
		if len(incoming.Events) < len(existing.Events) {
			return errors.New("sync request would roll back table events")
		}
		if len(incoming.Snapshots) < len(existing.Snapshots) {
			return errors.New("sync request would roll back table snapshots")
		}
	}
	if err := runtime.validateAcceptedHostTransition(existing, incoming, "sync request"); err != nil {
		return err
	}
	return nil
}

func (runtime *meshRuntime) validateAcceptedRemoteTable(existing *nativeTableState, incoming nativeTableState) error {
	if !runtime.isLocalTableParticipant(incoming) {
		return errors.New("remote table does not target this daemon")
	}
	if err := validateAcceptedTableProtocolVersion(incoming); err != nil {
		return err
	}
	if err := runtime.validateAcceptedHandTranscript(incoming); err != nil {
		return err
	}
	if err := runtime.validateAcceptedPublicReplay(incoming); err != nil {
		return err
	}
	if err := runtime.validateAcceptedHistoricalLedger(existing, incoming); err != nil {
		return err
	}
	if existing == nil {
		return nil
	}
	if incoming.CurrentEpoch < existing.CurrentEpoch {
		return errors.New("remote table would roll back table epoch")
	}
	if incoming.CurrentEpoch == existing.CurrentEpoch {
		if len(incoming.Events) < len(existing.Events) {
			return errors.New("remote table would roll back table events")
		}
		if len(incoming.Snapshots) < len(existing.Snapshots) {
			return errors.New("remote table would roll back table snapshots")
		}
	}
	if err := runtime.validateAcceptedHostTransition(existing, incoming, "remote table"); err != nil {
		return err
	}
	return nil
}

func (runtime *meshRuntime) validateAcceptedHostTransition(existing *nativeTableState, incoming nativeTableState, source string) error {
	if existing == nil {
		return nil
	}
	if incoming.CurrentEpoch == existing.CurrentEpoch && incoming.CurrentHost.Peer.PeerID != existing.CurrentHost.Peer.PeerID {
		return fmt.Errorf("%s changed host without advancing epoch", source)
	}
	if incoming.CurrentEpoch > existing.CurrentEpoch && incoming.CurrentHost.Peer.PeerID == existing.CurrentHost.Peer.PeerID {
		return fmt.Errorf("%s advanced epoch without rotating host", source)
	}
	if incoming.CurrentEpoch > existing.CurrentEpoch {
		expectedHost, ok := runtime.authorizedRemoteHostPeer(existing, incoming.CurrentHost.Peer.PeerID)
		if !ok {
			return fmt.Errorf("%s advanced to an unauthorized host", source)
		}
		if strings.TrimSpace(expectedHost.ProtocolPubkeyHex) == "" || strings.TrimSpace(expectedHost.PeerURL) == "" {
			return fmt.Errorf("%s advanced to host without a verifiable endpoint identity", source)
		}
		if incoming.CurrentHost.Peer.ProtocolPubkeyHex != expectedHost.ProtocolPubkeyHex {
			return fmt.Errorf("%s advanced to host with unexpected protocol key", source)
		}
		if incoming.CurrentHost.Peer.PeerURL != expectedHost.PeerURL {
			return fmt.Errorf("%s advanced to host with unexpected endpoint", source)
		}
		peerSelf, err := runtime.fetchPeerInfo(incoming.CurrentHost.Peer.PeerURL)
		if err != nil {
			return fmt.Errorf("%s host endpoint verification failed: %w", source, err)
		}
		if peerSelf.Peer.PeerID != incoming.CurrentHost.Peer.PeerID {
			return fmt.Errorf("%s host endpoint peer id mismatch", source)
		}
		if peerSelf.Peer.ProtocolPubkeyHex != incoming.CurrentHost.Peer.ProtocolPubkeyHex {
			return fmt.Errorf("%s host endpoint protocol key mismatch", source)
		}
	}
	return nil
}

func (runtime *meshRuntime) isLocalTableParticipant(table nativeTableState) bool {
	if table.CurrentHost.Peer.PeerID == runtime.selfPeerID() {
		return true
	}
	for _, witness := range table.Witnesses {
		if witness.Peer.PeerID == runtime.selfPeerID() {
			return true
		}
	}
	return runtime.seatIndexForPlayer(table) >= 0
}

func (runtime *meshRuntime) isAuthorizedRemoteHost(table *nativeTableState, candidatePeerID string) bool {
	_, ok := runtime.authorizedRemoteHostPeer(table, candidatePeerID)
	return ok
}

func (runtime *meshRuntime) authorizedRemoteHostPeer(table *nativeTableState, candidatePeerID string) (NativePeerAddress, bool) {
	if table == nil || candidatePeerID == "" {
		return NativePeerAddress{}, false
	}
	if candidatePeerID == table.CurrentHost.Peer.PeerID {
		return cloneJSON(table.CurrentHost.Peer), true
	}
	for _, witness := range table.Witnesses {
		if witness.Peer.PeerID == candidatePeerID {
			return cloneJSON(witness.Peer), true
		}
	}
	if len(table.Witnesses) > 0 {
		return NativePeerAddress{}, false
	}
	lowestSeat, ok := lowestEligibleFailoverSeat(*table)
	if !ok || candidatePeerID != lowestSeat.PeerID {
		return NativePeerAddress{}, false
	}
	return NativePeerAddress{
		Alias:             lowestSeat.Nickname,
		PeerID:            lowestSeat.PeerID,
		PeerURL:           lowestSeat.PeerURL,
		ProtocolPubkeyHex: lowestSeat.ProtocolPubkeyHex,
	}, true
}

func lowestEligibleFailoverSeat(table nativeTableState) (nativeSeatRecord, bool) {
	lowestPeerID := ""
	var lowestSeat *nativeSeatRecord
	for _, seat := range table.Seats {
		if seat.PeerID == "" || seat.PeerID == table.CurrentHost.Peer.PeerID {
			continue
		}
		if seat.Status == "pending-join" || terminalCustodySeatStatus(seat.Status) {
			continue
		}
		if lowestPeerID == "" || seat.PeerID < lowestPeerID {
			lowestPeerID = seat.PeerID
			seatClone := seat
			lowestSeat = &seatClone
		}
	}
	if lowestSeat == nil {
		return nativeSeatRecord{}, false
	}
	return *lowestSeat, true
}

func timestampWithinWindow(timestamp string, maxAge time.Duration) bool {
	if strings.TrimSpace(timestamp) == "" {
		return false
	}
	parsed, err := parseISOTimestamp(timestamp)
	if err != nil {
		return false
	}
	age := time.Since(parsed)
	if age < 0 {
		age = -age
	}
	return age <= maxAge
}

func (runtime *meshRuntime) networkTableView(table nativeTableState, visiblePlayerID string) nativeTableState {
	view := cloneJSON(table)
	view.ActiveHand = sanitizeActiveHandForNetwork(table.ActiveHand, visiblePlayerID)
	normalizePublicTranscriptRoot(&view)
	runtime.refreshPersistedActiveHandPublicState(&view)
	return view
}

func (runtime *meshRuntime) appendFundsOperation(operation NativeTableFundsOperation, cashoutSats int, status string) error {
	funds, err := runtime.store.readTableFunds()
	if err != nil {
		return err
	}
	entry := funds.Tables[operation.TableID]
	if entry.Operations == nil {
		entry.Operations = []NativeTableFundsOperation{}
	}
	entry.BuyInSats = maxInt(entry.BuyInSats, 0)
	entry.CashoutSats = cashoutSats
	entry.CheckpointHash = firstNonEmptyString(operation.StateHash, operation.CheckpointHash)
	entry.LastUpdatedAt = nowISO()
	entry.Operations = append(entry.Operations, operation)
	entry.PlayerID = runtime.walletID.PlayerID
	if status != "locked" && status != "pending-lock" {
		entry.ReservedFundingRefs = nil
	}
	entry.Status = status
	entry.TableID = operation.TableID
	if entry.BuyInSats == 0 {
		table, _ := runtime.store.readTable(operation.TableID)
		if table != nil {
			for _, seat := range table.Seats {
				if seat.PlayerID == runtime.walletID.PlayerID {
					entry.BuyInSats = seat.BuyInSats
				}
			}
		}
	}
	funds.Tables[operation.TableID] = entry
	return runtime.store.writeTableFunds(funds)
}

func (runtime *meshRuntime) pendingEmergencyExitOperation(tableID string) (*NativeTableFundsOperation, *nativeFundsRequest, error) {
	funds, err := runtime.store.readTableFunds()
	if err != nil {
		return nil, nil, err
	}
	entry, ok := funds.Tables[tableID]
	if !ok || entry.Status != "pending-exit" {
		return nil, nil, nil
	}
	for index := len(entry.Operations) - 1; index >= 0; index-- {
		operation := entry.Operations[index]
		if operation.Kind != string(tablecustody.TransitionKindEmergencyExit) || operation.Status != "pending-exit" || operation.FundsRequest == nil {
			continue
		}
		request := cloneJSON(*operation.FundsRequest)
		clonedOperation := cloneJSON(operation)
		return &clonedOperation, &request, nil
	}
	return nil, nil, nil
}

func (runtime *meshRuntime) latestFundsOperation(tableID, kind, status string) (*NativeTableFundsOperation, error) {
	funds, err := runtime.store.readTableFunds()
	if err != nil {
		return nil, err
	}
	entry, ok := funds.Tables[tableID]
	if !ok {
		return nil, nil
	}
	for index := len(entry.Operations) - 1; index >= 0; index-- {
		operation := entry.Operations[index]
		if operation.Kind != kind || operation.Status != status {
			continue
		}
		cloned := cloneJSON(operation)
		return &cloned, nil
	}
	return nil, nil
}

func (runtime *meshRuntime) buildFundsOperation(tableID string, amountSats int, kind, status, checkpointHash, arkIntentID, arkTxID, exitProofRef string, vtxoRefs []tablecustody.VTXORef, fundsRequest ...*nativeFundsRequest) (NativeTableFundsOperation, error) {
	custodyStateHash := checkpointHash
	prevStateHash := ""
	custodySeq := 0
	recordedRefs := append([]tablecustody.VTXORef(nil), vtxoRefs...)
	if table, _ := runtime.store.readTable(tableID); table != nil && table.LatestCustodyState != nil {
		custodyStateHash = table.LatestCustodyState.StateHash
		prevStateHash = table.LatestCustodyState.PrevStateHash
		custodySeq = table.LatestCustodyState.CustodySeq
		if len(recordedRefs) == 0 {
			for _, claim := range table.LatestCustodyState.StackClaims {
				if claim.PlayerID == runtime.walletID.PlayerID {
					recordedRefs = append(recordedRefs, claim.VTXORefs...)
				}
			}
		}
	}
	unsigned := map[string]any{
		"amountSats":      amountSats,
		"arkIntentId":     arkIntentID,
		"arkTxid":         arkTxID,
		"checkpointHash":  checkpointHash,
		"createdAt":       nowISO(),
		"custodySeq":      custodySeq,
		"exitProofRef":    exitProofRef,
		"prevStateHash":   prevStateHash,
		"kind":            kind,
		"networkId":       runtime.config.Network,
		"operationId":     randomUUID(),
		"playerId":        runtime.walletID.PlayerID,
		"provider":        nativeFundsProvider,
		"signerPubkeyHex": runtime.walletID.PublicKeyHex,
		"stateHash":       custodyStateHash,
		"status":          status,
		"tableId":         tableID,
		"vtxoRefs":        recordedRefs,
	}
	signatureHex, err := settlementcore.SignStructuredData(runtime.walletID.PrivateKeyHex, unsigned)
	if err != nil {
		return NativeTableFundsOperation{}, err
	}
	var persistedRequest *nativeFundsRequest
	if len(fundsRequest) > 0 && fundsRequest[0] != nil {
		cloned := cloneJSON(*fundsRequest[0])
		persistedRequest = &cloned
	}
	return NativeTableFundsOperation{
		AmountSats:      amountSats,
		ArkIntentID:     arkIntentID,
		ArkTxID:         arkTxID,
		CustodySeq:      custodySeq,
		CheckpointHash:  checkpointHash,
		CreatedAt:       stringValue(unsigned["createdAt"]),
		ExitProofRef:    exitProofRef,
		FundsRequest:    persistedRequest,
		PrevStateHash:   prevStateHash,
		Kind:            kind,
		NetworkID:       runtime.config.Network,
		OperationID:     stringValue(unsigned["operationId"]),
		PlayerID:        runtime.walletID.PlayerID,
		Provider:        nativeFundsProvider,
		SignatureHex:    signatureHex,
		SignerPubkeyHex: runtime.walletID.PublicKeyHex,
		StateHash:       custodyStateHash,
		Status:          status,
		TableID:         tableID,
		VTXORefs:        recordedRefs,
	}, nil
}

func (runtime *meshRuntime) selfPeerID() string {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return runtime.self.Peer.PeerID
}

func (runtime *meshRuntime) selfPeerURL() string {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return runtime.self.Peer.PeerURL
}

func (runtime *meshRuntime) writeJSON(writer http.ResponseWriter, value any) error {
	writer.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(writer).Encode(value)
}

func previousSnapshotHash(table nativeTableState) any {
	if table.LatestSnapshot == nil {
		return nil
	}
	hash := table.LatestSnapshot.SnapshotID
	if hash == "" {
		return nil
	}
	return hash
}

func sanitizeActiveHandForNetwork(active *nativeActiveHand, visiblePlayerID string) *nativeActiveHand {
	if active == nil {
		return nil
	}
	_ = visiblePlayerID
	sanitized := cloneJSON(*active)
	sanitized.State = sanitizeHoldemStateForNetwork(sanitized.State)
	return &sanitized
}

func sanitizeHoldemStateForNetwork(state game.HoldemState) game.HoldemState {
	return cloneJSON(state)
}

func cloneCustodyState(state *tablecustody.CustodyState) *tablecustody.CustodyState {
	if state == nil {
		return nil
	}
	cloned := cloneJSON(*state)
	return &cloned
}

func cloneSnapshot(snapshot *NativeCooperativeTableSnapshot) *NativeCooperativeTableSnapshot {
	if snapshot == nil {
		return nil
	}
	cloned := cloneJSON(*snapshot)
	return &cloned
}

func clonePublicState(state *NativePublicTableState) *NativePublicTableState {
	if state == nil {
		return nil
	}
	cloned := cloneJSON(*state)
	return &cloned
}

func playerIDsFromSeats(seats []nativeSeatRecord) []string {
	values := make([]string, 0, len(seats))
	for _, seat := range seats {
		values = append(values, seat.PlayerID)
	}
	return values
}

func peerIDsFromParticipants(participants []nativeKnownParticipant) []string {
	values := make([]string, 0, len(participants))
	for _, participant := range participants {
		values = append(values, participant.Peer.PeerID)
	}
	return values
}

func seatRecordForPlayer(table nativeTableState, playerID string) (nativeSeatRecord, bool) {
	for _, seat := range table.Seats {
		if seat.PlayerID == playerID {
			return seat, true
		}
	}
	return nativeSeatRecord{}, false
}

func nativeActionDecisionIndex(table nativeTableState) (int, error) {
	if table.ActiveHand == nil {
		return 0, errors.New("hand is not active")
	}
	if !game.PhaseAllowsActions(table.ActiveHand.State.Phase) {
		return 0, errors.New("hand is still starting")
	}
	return len(table.ActiveHand.State.ActionLog), nil
}

func nativeActionAuthPayload(tableID, playerID, handID, prevCustodyStateHash, challengeAnchor, transcriptRoot string, epoch int, decisionIndex int, action game.Action, signedAt string) map[string]any {
	payload := map[string]any{
		"action":               rawJSONMap(action),
		"challengeAnchor":      challengeAnchor,
		"decisionIndex":        decisionIndex,
		"epoch":                epoch,
		"handId":               handID,
		"playerId":             playerID,
		"signedAt":             signedAt,
		"tableId":              tableID,
		"transcriptRoot":       transcriptRoot,
		"type":                 "table-action",
		"prevCustodyStateHash": prevCustodyStateHash,
	}
	return payload
}

func nativeFundsAuthPayload(request nativeFundsRequest) map[string]any {
	payload := map[string]any{
		"arkAddress":           request.ArkAddress,
		"epoch":                request.Epoch,
		"kind":                 request.Kind,
		"playerId":             request.PlayerID,
		"prevCustodyStateHash": request.PrevCustodyStateHash,
		"protocolVersion":      request.ProtocolVersion,
		"signedAt":             request.SignedAt,
		"tableId":              request.TableID,
		"type":                 "table-funds",
	}
	if request.ExitExecution != nil {
		payload["exitExecution"] = rawJSONMap(nativeFundsExitExecution{
			BroadcastTxIDs: append([]string(nil), request.ExitExecution.BroadcastTxIDs...),
			Pending:        request.ExitExecution.Pending,
			SourceRefs:     canonicalVTXORefs(request.ExitExecution.SourceRefs),
			SweepTxID:      request.ExitExecution.SweepTxID,
		})
	}
	return payload
}

func nativeTableFetchAuthPayload(tableID, playerID, signedAt string) map[string]any {
	return map[string]any{
		"playerId": playerID,
		"signedAt": signedAt,
		"tableId":  tableID,
		"type":     "table-fetch",
	}
}

func nativeTableSyncAuthPayload(tableID, senderPeerID, tableHash, sentAt string) map[string]any {
	return map[string]any{
		"senderPeerId": senderPeerID,
		"sentAt":       sentAt,
		"tableHash":    tableHash,
		"tableId":      tableID,
		"type":         "table-sync",
	}
}

func zeroContributions(seats []nativeSeatRecord) map[string]int {
	values := map[string]int{}
	for _, seat := range seats {
		values[seat.PlayerID] = 0
	}
	return values
}

func zeroContributionsFromPlayers(players []NativeSeatedPlayer) map[string]int {
	values := map[string]int{}
	for _, player := range players {
		values[player.PlayerID] = 0
	}
	return values
}

func stringCards(cards []game.CardCode) []string {
	values := make([]string, 0, len(cards))
	for _, card := range cards {
		values = append(values, string(card))
	}
	return values
}

type createdTablesCursor struct {
	CreatedAt string `json:"createdAt"`
	TableID   string `json:"tableId"`
}

func clampCreatedTablesLimit(limit int) int {
	if limit <= 0 {
		return defaultCreatedTablesLimit
	}
	if limit > maxCreatedTablesLimit {
		return maxCreatedTablesLimit
	}
	return limit
}

func encodeCreatedTablesCursor(cursor createdTablesCursor) (string, error) {
	data, err := json.Marshal(cursor)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func decodeCreatedTablesCursor(raw string) (*createdTablesCursor, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("decode created tables cursor: %w", err)
	}
	var cursor createdTablesCursor
	if err := json.Unmarshal(data, &cursor); err != nil {
		return nil, fmt.Errorf("decode created tables cursor: %w", err)
	}
	if cursor.CreatedAt == "" || cursor.TableID == "" {
		return nil, errors.New("decode created tables cursor: cursor is missing createdAt or tableId")
	}
	return &cursor, nil
}

func compareCreatedTableEntries(left, right NativeCreatedTableEntry) int {
	if left.Config.CreatedAt > right.Config.CreatedAt {
		return -1
	}
	if left.Config.CreatedAt < right.Config.CreatedAt {
		return 1
	}
	if left.Config.TableID > right.Config.TableID {
		return -1
	}
	if left.Config.TableID < right.Config.TableID {
		return 1
	}
	return 0
}

func compareCreatedTableEntryWithCursor(entry NativeCreatedTableEntry, cursor createdTablesCursor) int {
	return compareCreatedTableEntries(entry, NativeCreatedTableEntry{
		Config: NativeMeshTableConfig{
			CreatedAt: cursor.CreatedAt,
			TableID:   cursor.TableID,
		},
	})
}

func decodeCreatedTableConfig(reference walletpkg.MeshTableReferenceState) (NativeMeshTableConfig, error) {
	if len(reference.Config) == 0 {
		return NativeMeshTableConfig{}, fmt.Errorf("%w: created table config is missing protocol version", errStoredProtocolVersionMismatch)
	}
	var config NativeMeshTableConfig
	if err := json.Unmarshal(reference.Config, &config); err != nil {
		return NativeMeshTableConfig{}, err
	}
	if config.TableID == "" {
		config.TableID = reference.TableID
	}
	if config.CreatedAt == "" {
		config.CreatedAt = reference.CreatedAt
	}
	if config.Visibility == "" {
		config.Visibility = reference.Visibility
	}
	if strings.TrimSpace(config.ProtocolVersion) != nativeProtocolVersion {
		return NativeMeshTableConfig{}, fmt.Errorf("%w: created table protocol version mismatch", errStoredProtocolVersionMismatch)
	}
	return config, nil
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func latestStackAmountFromFunds(entry NativeTableFundsEntry) int {
	if entry.CashoutSats > 0 {
		return entry.CashoutSats
	}
	return entry.BuyInSats
}

func tableVisibilityFromInput(input map[string]any) (string, error) {
	visibility := strings.ToLower(strings.TrimSpace(stringFromMap(input, "visibility", "")))
	hasVisibility := visibility != ""
	if hasVisibility && visibility != "public" && visibility != "private" {
		return "", fmt.Errorf("visibility must be public or private")
	}

	hasPublicFlag := input != nil && input["public"] != nil
	publicValue := boolFromMap(input, "public")
	if hasVisibility && hasPublicFlag {
		if (visibility == "public") != publicValue {
			return "", fmt.Errorf("visibility and public flag disagree")
		}
	}
	if hasVisibility {
		return visibility, nil
	}
	if publicValue {
		return "public", nil
	}
	return "private", nil
}

func stringFromMap(input map[string]any, key, fallback string) string {
	if input == nil {
		return fallback
	}
	if value, ok := input[key].(string); ok && strings.TrimSpace(value) != "" {
		return value
	}
	return fallback
}

func stringSliceFromMap(input map[string]any, key string) []string {
	if input == nil {
		return nil
	}
	raw, ok := input[key].([]any)
	if !ok {
		if typed, ok := input[key].([]string); ok {
			return append([]string{}, typed...)
		}
		return nil
	}
	values := make([]string, 0, len(raw))
	for _, value := range raw {
		if text, ok := value.(string); ok && text != "" {
			values = append(values, text)
		}
	}
	return values
}

func intFromMap(input map[string]any, key string, fallback int) int {
	if input == nil {
		return fallback
	}
	switch value := input[key].(type) {
	case float64:
		return int(value)
	case int:
		return value
	case int64:
		return int(value)
	case json.Number:
		parsed, _ := value.Int64()
		return int(parsed)
	}
	return fallback
}

func boolFromMap(input map[string]any, key string) bool {
	return boolFromMapDefault(input, key, false)
}

func boolFromMapDefault(input map[string]any, key string, fallback bool) bool {
	if input == nil {
		return fallback
	}
	if value, ok := input[key].(bool); ok {
		return value
	}
	return fallback
}

func stringValue(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case fmt.Stringer:
		return typed.String()
	}
	return ""
}

func maxInt(left, right int) int {
	if left > right {
		return left
	}
	return right
}
