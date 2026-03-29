package parker

import (
	"bytes"
	"context"
	"crypto/sha256"
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

	"github.com/danieldresner/arkade_fun/internal/game"
	"github.com/danieldresner/arkade_fun/internal/settlementcore"
	walletpkg "github.com/danieldresner/arkade_fun/internal/wallet"
)

type meshRuntime struct {
	config           RuntimeConfig
	clearPeerURL     string
	httpClient       *http.Client
	lastSyncAt       map[string]time.Time
	listener         net.Listener
	mode             string
	mu               sync.Mutex
	peerInfoCache    map[string]nativeCachedPeerInfo
	peerIdentity     settlementcore.ScopedIdentity
	profileName      string
	profileStore     *walletpkg.ProfileStore
	protocolID       string
	protocolIdentity settlementcore.ScopedIdentity
	self             nativePeerSelf
	server           *http.Server
	tableSyncSender  func(peerURL string, input nativeTableSyncRequest) error
	started          bool
	store            *meshStore
	torService       *runtimeHiddenService
	transportKeyID   string
	transportPrivate string
	transportPublic  string
	walletID         settlementcore.LocalIdentity
	walletRuntime    *walletpkg.Runtime
}

const (
	nativeTableAuthPlayerIDHeader  = "X-Parker-Player-Id"
	nativeTableAuthSignedAtHeader  = "X-Parker-Signed-At"
	nativeTableAuthSignatureHeader = "X-Parker-Signature"
	nativeTableFetchAuthMaxAge     = 15 * time.Second
	nativeTableSyncAuthMaxAge      = 15 * time.Second
)

func newMeshRuntime(profileName string, config RuntimeConfig, mode string) (*meshRuntime, error) {
	if mode == "" {
		mode = "player"
	}
	store, err := newMeshStore(profileName, config)
	if err != nil {
		return nil, err
	}
	return &meshRuntime{
		config:        config,
		httpClient:    &http.Client{Timeout: 5 * time.Second},
		lastSyncAt:    map[string]time.Time{},
		mode:          mode,
		peerInfoCache: map[string]nativeCachedPeerInfo{},
		profileName:   profileName,
		profileStore:  walletpkg.NewProfileStore(config.ProfileDir),
		store:         store,
		walletRuntime: walletpkg.NewRuntime(walletpkg.RuntimeConfig{
			ArkServerURL:      config.ArkServerURL,
			Network:           config.Network,
			NigiriDatadir:     config.NigiriDatadir,
			ProfileDir:        config.ProfileDir,
			RunDir:            config.RunDir,
			UseMockSettlement: config.UseMockSettlement,
		}),
	}, nil
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
	if runtime.store != nil {
		joined = errors.Join(joined, runtime.store.close())
	}
	return joined
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
			if elapsedMillis(table.LastHostHeartbeatAt) >= nativeHostHeartbeatMS {
				_ = runtime.store.withTableLock(tableID, func() error {
					latest, err := runtime.store.readTable(tableID)
					if err != nil || latest == nil {
						return err
					}
					if latest.CurrentHost.Peer.PeerID != selfPeerID {
						return nil
					}
					latest.LastHostHeartbeatAt = nowISO()
					if latest.NextHandAt != "" && elapsedMillis(latest.NextHandAt) >= 0 {
						if err := runtime.startNextHandLocked(latest); err != nil {
							return err
						}
					}
					if err := runtime.advanceHandProtocolLocked(latest); err != nil {
						return err
					}
					return runtime.persistAndReplicate(latest, true)
				})
			}
			continue
		}

		if runtime.shouldPollHost(tableID) && table.CurrentHost.Peer.PeerURL != "" {
			if remote, err := runtime.fetchRemoteTable(table.CurrentHost.Peer.PeerURL, tableID); err == nil && remote != nil {
				if err := runtime.acceptRemoteTable(*remote); err == nil {
					runtime.lastSyncAt[tableID] = time.Now()
					table, _ = runtime.store.readTable(tableID)
				}
			}
		}

		if table != nil && protocolDeadlineExpired(*table) && runtime.shouldHandleFailover(*table) {
			if err := runtime.forceProtocolFailover(tableID, fmt.Sprintf("protocol deadline expired during %s", table.ActiveHand.State.Phase)); err == nil {
				table, _ = runtime.store.readTable(tableID)
			}
		}
		if table != nil && elapsedMillis(table.LastHostHeartbeatAt) > nativeHostFailureMS && runtime.shouldHandleFailover(*table) {
			_ = runtime.failoverTable(tableID, "missed host heartbeats")
		}
		if table != nil {
			go runtime.driveLocalHandProtocol(tableID)
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
	overlay := 0
	for _, table := range funds.Tables {
		switch table.Status {
		case "locked":
			overlay -= table.BuyInSats
		case "completed", "exited":
			overlay += table.CashoutSats - table.BuyInSats
		}
	}
	base.AvailableSats += overlay
	base.TotalSats += overlay
	return base, nil
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

func (runtime *meshRuntime) CreateTable(input map[string]any) (map[string]any, error) {
	if err := runtime.Start(); err != nil {
		return nil, err
	}
	tableID := randomUUID()
	visibility := "private"
	if boolFromMap(input, "public") {
		visibility = "public"
	}
	now := nowISO()
	inviteCode, err := encodeInvite(tableID, runtime.config.Network, runtime.selfPeerID(), runtime.selfPeerURL())
	if err != nil {
		return nil, err
	}
	config := NativeMeshTableConfig{
		BigBlindSats:              intFromMap(input, "bigBlindSats", 100),
		BuyInMaxSats:              intFromMap(input, "buyInMaxSats", 4000),
		BuyInMinSats:              intFromMap(input, "buyInMinSats", 4000),
		CreatedAt:                 now,
		DealerMode:                nativeDealerMode,
		HostPeerID:                runtime.selfPeerID(),
		HostPlaysAllowed:          true,
		Name:                      stringFromMap(input, "name", "Parker Table"),
		NetworkID:                 runtime.config.Network,
		OccupiedSeats:             0,
		PublicSpectatorDelayHands: 1,
		SeatCount:                 2,
		SmallBlindSats:            intFromMap(input, "smallBlindSats", 50),
		SpectatorsAllowed:         boolFromMapDefault(input, "spectatorsAllowed", true),
		Status:                    "announced",
		TableID:                   tableID,
		Visibility:                visibility,
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
	return map[string]any{
		"inviteCode": inviteCode,
		"table":      rawJSONMap(config),
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
	tableID := stringValue(invite["tableId"])
	hostPeerURL := stringValue(invite["hostPeerUrl"])
	if tableID == "" || hostPeerURL == "" {
		return NativeMeshTableView{}, errors.New("invite is missing host or table information")
	}

	profile, err := runtime.loadProfileState()
	if err != nil {
		return NativeMeshTableView{}, err
	}
	wallet, err := runtime.walletSummary()
	if err != nil {
		return NativeMeshTableView{}, err
	}
	if wallet.AvailableSats < buyInSats {
		return NativeMeshTableView{}, fmt.Errorf("insufficient available sats for buy-in: have %d need %d", wallet.AvailableSats, buyInSats)
	}
	request := nativeJoinRequest{
		ArkAddress:      wallet.ArkAddress,
		BuyInSats:       buyInSats,
		Nickname:        profile.Nickname,
		Peer:            runtime.self.Peer,
		ProfileName:     runtime.profileName,
		ProtocolID:      runtime.protocolID,
		TableID:         tableID,
		WalletPlayerID:  runtime.walletID.PlayerID,
		WalletPubkeyHex: runtime.walletID.PublicKeyHex,
	}
	binding, err := settlementcore.BuildIdentityBinding(tableID, runtime.selfPeerID(), runtime.selfPeerURL(), runtime.protocolIdentity, runtime.walletID, nowISO())
	if err != nil {
		return NativeMeshTableView{}, err
	}
	request.IdentityBinding = binding

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

func (runtime *meshRuntime) SendAction(tableID string, action game.Action) (NativeMeshTableView, error) {
	if err := runtime.Start(); err != nil {
		return NativeMeshTableView{}, err
	}
	table, err := runtime.refreshLocalTable(tableID)
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
	request, err := runtime.buildSignedActionRequest(*table, action)
	if err != nil {
		return NativeMeshTableView{}, err
	}
	var updated nativeTableState
	if table.CurrentHost.Peer.PeerID == runtime.selfPeerID() {
		updated, err = runtime.handleActionFromPeer(request)
	} else {
		updated, err = runtime.remoteAction(table.CurrentHost.Peer.PeerURL, request)
	}
	if err != nil {
		return NativeMeshTableView{}, err
	}
	if err := runtime.acceptRemoteTable(updated); err != nil {
		return NativeMeshTableView{}, err
	}
	accepted, err := runtime.requireLocalTable(updated.Config.TableID)
	if err != nil {
		return NativeMeshTableView{}, err
	}
	return runtime.localTableView(*accepted), nil
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
	if table == nil || table.LatestFullySignedSnapshot == nil {
		return nil, errors.New("latest fully signed snapshot is unavailable")
	}
	amount := table.LatestFullySignedSnapshot.ChipBalances[runtime.walletID.PlayerID]
	operation, err := runtime.buildFundsOperation(table.Config.TableID, amount, "renewal", "renewed", runtime.snapshotHash(*table.LatestFullySignedSnapshot))
	if err != nil {
		return nil, err
	}
	if err := runtime.appendFundsOperation(operation, 0, "locked"); err != nil {
		return nil, err
	}
	return []map[string]any{rawJSONMap(operation)}, nil
}

func (runtime *meshRuntime) Exit(tableID string) (map[string]any, error) {
	return runtime.completeFunds(tableID, "emergency-exit", "exited")
}

func (runtime *meshRuntime) completeFunds(tableID, kind, finalStatus string) (map[string]any, error) {
	table, err := runtime.refreshLocalTable(tableID)
	if err != nil {
		return nil, err
	}
	if table == nil || table.LatestFullySignedSnapshot == nil {
		return nil, errors.New("latest fully signed snapshot is unavailable")
	}
	amount := table.LatestFullySignedSnapshot.ChipBalances[runtime.walletID.PlayerID]
	checkpointHash := runtime.snapshotHash(*table.LatestFullySignedSnapshot)
	operation, err := runtime.buildFundsOperation(table.Config.TableID, amount, kind, finalStatus, checkpointHash)
	if err != nil {
		return nil, err
	}
	if err := runtime.appendFundsOperation(operation, amount, finalStatus); err != nil {
		return nil, err
	}
	return map[string]any{
		"checkpointHash": checkpointHash,
		"receipt":        rawJSONMap(operation),
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
	if table != nil && elapsedMillis(table.LastHostHeartbeatAt) > nativeHostFailureMS && runtime.shouldHandleFailover(*table) {
		if err := runtime.failoverTable(tableID, "missed host heartbeats"); err == nil {
			table, _ = runtime.store.readTable(tableID)
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
		table, err := runtime.store.readTable(join.TableID)
		if err != nil || table == nil {
			return fmt.Errorf("table %s not found", join.TableID)
		}
		if table.CurrentHost.Peer.PeerID != runtime.selfPeerID() {
			return errors.New("join request must be sent to the current host")
		}
		if err := runtime.validateJoinRequest(join); err != nil {
			return err
		}
		peer := join.Peer
		peer.LastSeenAt = nowISO()
		if err := runtime.saveKnownPeer(peer); err != nil {
			return err
		}
		for _, seat := range table.Seats {
			if seat.PlayerID == join.WalletPlayerID {
				updated = *table
				return nil
			}
		}
		if len(table.Seats) >= table.Config.SeatCount {
			return errors.New("table is full")
		}
		seatIndex := len(table.Seats)
		seat := nativeSeatRecord{
			NativeSeatedPlayer: NativeSeatedPlayer{
				ArkAddress:        join.ArkAddress,
				BuyInSats:         join.BuyInSats,
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
		table.Seats = append(table.Seats, seat)
		table.Config.OccupiedSeats = len(table.Seats)
		if err := runtime.appendEvent(table, map[string]any{
			"type":      "SeatLocked",
			"buyInSats": join.BuyInSats,
			"peerId":    join.Peer.PeerID,
			"playerId":  join.WalletPlayerID,
			"seatIndex": seatIndex,
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
			table.NextHandAt = addMillis(nowISO(), nativeNextHandDelayMS)
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

func (runtime *meshRuntime) handleActionFromPeer(request nativeActionRequest) (nativeTableState, error) {
	var updated nativeTableState
	err := runtime.store.withTableLock(request.TableID, func() error {
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
		nextState, err := game.ApplyHoldemAction(table.ActiveHand.State, seatIndex, request.Action)
		if err != nil {
			return err
		}
		table.ActiveHand.State = nextState
		if err := runtime.appendEvent(table, map[string]any{
			"type": "PlayerAction",
			"intent": map[string]any{
				"action": rawJSONMap(map[string]any{
					"type":      request.Action.Type,
					"totalSats": request.Action.TotalSats,
				}),
				"epoch":       table.CurrentEpoch,
				"handId":      nextState.HandID,
				"playerId":    request.PlayerID,
				"requestedAt": nowISO(),
				"seatIndex":   seatIndex,
				"tableId":     table.Config.TableID,
			},
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
	if len(table.Seats) < 2 {
		return nil
	}
	chips := map[string]int{}
	if table.LatestFullySignedSnapshot != nil {
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
		Seats: [2]game.HoldemSeatConfig{
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
			PhaseDeadlineAt: addMillis(nowISO(), nativeHandProtocolTimeoutMS),
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
	table.Config.Status = "active"
	table.NextHandAt = ""
	if err := runtime.appendEvent(table, map[string]any{
		"type":            "HandStart",
		"dealerSeatIndex": dealerSeat,
		"handId":          hand.HandID,
		"handNumber":      hand.HandNumber,
	}); err != nil {
		return err
	}
	return runtime.advanceHandProtocolLocked(table)
}

func (runtime *meshRuntime) rotateHostTable(tableID, reason string, requireHostFailure bool, resetProtocolDeadline bool) error {
	return runtime.store.withTableLock(tableID, func() error {
		table, err := runtime.store.readTable(tableID)
		if err != nil || table == nil {
			return err
		}
		if requireHostFailure && elapsedMillis(table.LastHostHeartbeatAt) <= nativeHostFailureMS {
			return nil
		}
		if !runtime.shouldHandleFailover(*table) {
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
			"leaseExpiry": addMillis(nowISO(), nativeHostFailureMS),
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
		active := table.ActiveHand != nil && table.ActiveHand.State.Phase != game.StreetSettled && table.ActiveHand.State.Phase != game.StreetAborted
		if active {
			if resetProtocolDeadline {
				setProtocolDeadline(table)
			}
			if err := runtime.advanceHandProtocolLocked(table); err != nil {
				return err
			}
		} else {
			table.NextHandAt = addMillis(nowISO(), nativeNextHandDelayMS)
			if err := runtime.startNextHandLocked(table); err != nil {
				return err
			}
		}
		return runtime.persistAndReplicate(table, true)
	})
}

func (runtime *meshRuntime) failoverTable(tableID, reason string) error {
	return runtime.rotateHostTable(tableID, reason, true, true)
}

func (runtime *meshRuntime) forceProtocolFailover(tableID, reason string) error {
	return runtime.rotateHostTable(tableID, reason, false, false)
}

func (runtime *meshRuntime) persistAndReplicate(table *nativeTableState, publish bool) error {
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
	for _, seat := range table.Seats {
		if seat.PlayerID == runtime.walletID.PlayerID && entry.BuyInSats == 0 {
			entry.BuyInSats = seat.BuyInSats
			entry.Status = "locked"
		}
	}
	if table.LatestFullySignedSnapshot != nil {
		entry.CheckpointHash = runtime.snapshotHash(*table.LatestFullySignedSnapshot)
		entry.LastUpdatedAt = nowISO()
	}
	funds.Tables[table.Config.TableID] = entry
	return runtime.store.writeTableFunds(funds)
}

func (runtime *meshRuntime) replicateTable(table nativeTableState) {
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
		if resultEvent, ok := findHandResultEventByCheckpoint(table.Events, checkpointHash); ok {
			if strings.TrimSpace(stringValue(snapshot.LatestEventHash)) != strings.TrimSpace(stringValue(resultEvent.PrevEventHash)) {
				return fmt.Errorf("snapshot %d latest event hash does not match its hand result anchor", index)
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
	if !reflect.DeepEqual(comparablePublicState(table.PublicState), comparablePublicState(&expectedPublic)) {
		return errors.New("public state does not match transcript replay")
	}
	if replayedState.Phase != game.StreetSettled {
		return nil
	}

	if table.LatestSnapshot == nil {
		return errors.New("settled hand is missing latest snapshot")
	}
	if table.LatestFullySignedSnapshot == nil {
		return errors.New("settled hand is missing latest fully signed snapshot")
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
				legalActions = game.GetLegalActions(table.ActiveHand.State, &seatIndex)
				canAct = table.ActiveHand.State.ActingSeatIndex != nil && *table.ActiveHand.State.ActingSeatIndex == seat.SeatIndex
			}
			break
		}
	}

	return NativeMeshTableView{
		Config:                    table.Config,
		Events:                    cloneJSON(table.Events),
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
	return NativeTableSummary{
		CurrentEpoch:     table.CurrentEpoch,
		HandNumber:       handNumber,
		HostPeerID:       table.CurrentHost.Peer.PeerID,
		LatestSnapshotID: snapshotID,
		Phase:            phase,
		Role:             runtime.roleForTable(table),
		Status:           table.Config.Status,
		TableID:          table.Config.TableID,
		TableName:        table.Config.Name,
		Visibility:       table.Config.Visibility,
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
	profile.CurrentMeshTableID = accepted.Config.TableID
	profile.CurrentTable = &walletpkg.TableSessionState{
		InviteCode: accepted.InviteCode,
		SeatIndex:  runtime.seatIndexForPlayer(accepted),
		TableID:    accepted.Config.TableID,
	}
	profile.MeshTables[accepted.Config.TableID] = walletpkg.MeshTableReferenceState{
		Config:       MustMarshalJSON(accepted.Config),
		CurrentEpoch: accepted.CurrentEpoch,
		HostPeerID:   accepted.CurrentHost.Peer.PeerID,
		HostPeerURL:  accepted.CurrentHost.Peer.PeerURL,
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
	go runtime.driveLocalHandProtocol(accepted.Config.TableID)
	return nil
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
	lowestPeerID := ""
	for _, seat := range table.Seats {
		if lowestPeerID == "" || seat.PeerID < lowestPeerID {
			lowestPeerID = seat.PeerID
		}
	}
	return lowestPeerID == runtime.selfPeerID()
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
	request := nativeActionRequest{
		Action:        action,
		DecisionIndex: decisionIndex,
		Epoch:         table.CurrentEpoch,
		HandID:        handID,
		PlayerID:      runtime.walletID.PlayerID,
		ProfileName:   runtime.profileName,
		SignedAt:      signedAt,
		TableID:       table.Config.TableID,
	}
	signatureHex, err := settlementcore.SignStructuredData(runtime.walletID.PrivateKeyHex, nativeActionAuthPayload(request.TableID, request.PlayerID, request.HandID, request.Epoch, request.DecisionIndex, request.Action, request.SignedAt))
	if err != nil {
		return nativeActionRequest{}, err
	}
	request.SignatureHex = signatureHex
	return request, nil
}

func (runtime *meshRuntime) validateJoinRequest(join nativeJoinRequest) error {
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
	if request.SignedAt == "" || request.SignatureHex == "" {
		return errors.New("action request is missing player signature")
	}
	ok, err := settlementcore.VerifyStructuredData(seat.WalletPubkeyHex, nativeActionAuthPayload(request.TableID, request.PlayerID, request.HandID, request.Epoch, request.DecisionIndex, request.Action, request.SignedAt), request.SignatureHex)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("action request signature is invalid")
	}
	return nil
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
	lowestPeerID := ""
	var lowestSeat *nativeSeatRecord
	for _, seat := range table.Seats {
		if lowestPeerID == "" || seat.PeerID < lowestPeerID {
			lowestPeerID = seat.PeerID
			seatClone := seat
			lowestSeat = &seatClone
		}
	}
	if candidatePeerID != lowestPeerID || lowestSeat == nil {
		return NativePeerAddress{}, false
	}
	return NativePeerAddress{
		Alias:             lowestSeat.Nickname,
		PeerID:            lowestSeat.PeerID,
		PeerURL:           lowestSeat.PeerURL,
		ProtocolPubkeyHex: lowestSeat.ProtocolPubkeyHex,
	}, true
}

func timestampWithinWindow(timestamp string, maxAge time.Duration) bool {
	if strings.TrimSpace(timestamp) == "" {
		return false
	}
	parsed, err := time.Parse(time.RFC3339, timestamp)
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
	entry.CheckpointHash = operation.CheckpointHash
	entry.LastUpdatedAt = nowISO()
	entry.Operations = append(entry.Operations, operation)
	entry.PlayerID = runtime.walletID.PlayerID
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

func (runtime *meshRuntime) buildFundsOperation(tableID string, amountSats int, kind, status, checkpointHash string) (NativeTableFundsOperation, error) {
	unsigned := map[string]any{
		"amountSats":      amountSats,
		"checkpointHash":  checkpointHash,
		"createdAt":       nowISO(),
		"kind":            kind,
		"networkId":       runtime.config.Network,
		"operationId":     randomUUID(),
		"playerId":        runtime.walletID.PlayerID,
		"provider":        nativeFundsProvider,
		"signerPubkeyHex": runtime.walletID.PublicKeyHex,
		"status":          status,
		"tableId":         tableID,
	}
	signatureHex, err := settlementcore.SignStructuredData(runtime.walletID.PrivateKeyHex, unsigned)
	if err != nil {
		return NativeTableFundsOperation{}, err
	}
	return NativeTableFundsOperation{
		AmountSats:      amountSats,
		CheckpointHash:  checkpointHash,
		CreatedAt:       stringValue(unsigned["createdAt"]),
		Kind:            kind,
		NetworkID:       runtime.config.Network,
		OperationID:     stringValue(unsigned["operationId"]),
		PlayerID:        runtime.walletID.PlayerID,
		Provider:        nativeFundsProvider,
		SignatureHex:    signatureHex,
		SignerPubkeyHex: runtime.walletID.PublicKeyHex,
		Status:          status,
		TableID:         tableID,
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

func nativeActionAuthPayload(tableID, playerID, handID string, epoch int, decisionIndex int, action game.Action, signedAt string) map[string]any {
	return map[string]any{
		"action":        rawJSONMap(action),
		"decisionIndex": decisionIndex,
		"epoch":         epoch,
		"handId":        handID,
		"playerId":      playerID,
		"signedAt":      signedAt,
		"tableId":       tableID,
		"type":          "table-action",
	}
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
