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
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/danieldresner/arkade_fun/internal/game"
	"github.com/danieldresner/arkade_fun/internal/settlementcore"
	walletpkg "github.com/danieldresner/arkade_fun/internal/wallet"
)

type nativeRuntime struct {
	config           RuntimeConfig
	httpClient       *http.Client
	lastSyncAt       map[string]time.Time
	listener         net.Listener
	mode             string
	mu               sync.Mutex
	peerIdentity     settlementcore.ScopedIdentity
	profileName      string
	profileStore     *walletpkg.ProfileStore
	protocolID       string
	protocolIdentity settlementcore.ScopedIdentity
	self             nativePeerSelf
	server           *http.Server
	started          bool
	store            *nativeStore
	walletID         settlementcore.LocalIdentity
	walletRuntime    *walletpkg.Runtime
}

func newNativeRuntime(profileName string, config RuntimeConfig, mode string) (*nativeRuntime, error) {
	if mode == "" {
		mode = "player"
	}
	store, err := newNativeStore(profileName, config)
	if err != nil {
		return nil, err
	}
	return &nativeRuntime{
		config:       config,
		httpClient:   &http.Client{Timeout: 5 * time.Second},
		lastSyncAt:   map[string]time.Time{},
		mode:         mode,
		profileName:  profileName,
		profileStore: walletpkg.NewProfileStore(config.ProfileDir),
		store:        store,
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

func (runtime *nativeRuntime) Start() error {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()

	if runtime.started {
		return nil
	}
	if err := runtime.ensureBootstrapLocked(""); err != nil {
		return err
	}
	if err := runtime.startPeerServerLocked(); err != nil {
		return err
	}
	runtime.started = true
	return nil
}

func (runtime *nativeRuntime) Close() error {
	runtime.mu.Lock()
	server := runtime.server
	runtime.server = nil
	listener := runtime.listener
	runtime.listener = nil
	runtime.started = false
	runtime.mu.Unlock()

	var joined error
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

func (runtime *nativeRuntime) Bootstrap(nickname string) (map[string]any, error) {
	runtime.mu.Lock()
	if err := runtime.ensureBootstrapLocked(nickname); err != nil {
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

func (runtime *nativeRuntime) Tick() {
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
					return runtime.persistAndReplicate(latest, true)
				})
			}
			continue
		}

		if runtime.shouldPollHost(tableID) && table.CurrentHost.Peer.PeerURL != "" {
			if remote, err := runtime.fetchRemoteTable(table.CurrentHost.Peer.PeerURL, tableID); err == nil && remote != nil {
				_ = runtime.acceptRemoteTable(*remote)
				runtime.lastSyncAt[tableID] = time.Now()
				table = remote
			}
		}

		if table != nil && elapsedMillis(table.LastHostHeartbeatAt) > nativeHostFailureMS && runtime.shouldHandleFailover(*table) {
			_ = runtime.failoverTable(tableID, "missed host heartbeats")
		}
	}
}

func (runtime *nativeRuntime) CurrentState() (map[string]any, error) {
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

func (runtime *nativeRuntime) QuickState() (map[string]any, error) {
	mesh, err := runtime.meshState()
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"mesh": rawJSONMap(mesh),
	}, nil
}

func (runtime *nativeRuntime) walletSummary() (walletpkg.WalletSummary, error) {
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

func (runtime *nativeRuntime) meshState() (NativeMeshRuntimeState, error) {
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

func (runtime *nativeRuntime) NetworkPeers() ([]NativePeerAddress, error) {
	return runtime.knownPeers()
}

func (runtime *nativeRuntime) BootstrapPeer(peerURL, alias string, roles []string) (NativePeerAddress, error) {
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

func (runtime *nativeRuntime) CreateTable(input map[string]any) (map[string]any, error) {
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

func (runtime *nativeRuntime) AnnounceTable(tableID string) (map[string]any, error) {
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

func (runtime *nativeRuntime) JoinTable(inviteCode string, buyInSats int) (NativeMeshTableView, error) {
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
	return runtime.localTableView(table), nil
}

func (runtime *nativeRuntime) GetTable(tableID string) (NativeMeshTableView, error) {
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

func (runtime *nativeRuntime) SendAction(tableID string, action game.Action) (NativeMeshTableView, error) {
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
	request := nativeActionRequest{
		Action:      action,
		PlayerID:    runtime.walletID.PlayerID,
		ProfileName: runtime.profileName,
		TableID:     table.Config.TableID,
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
	return runtime.localTableView(updated), nil
}

func (runtime *nativeRuntime) RotateHost(tableID string) (NativeMeshTableView, error) {
	if err := runtime.failoverTable(tableID, "manual rotation"); err != nil {
		return NativeMeshTableView{}, err
	}
	table, err := runtime.requireLocalTable(tableID)
	if err != nil {
		return NativeMeshTableView{}, err
	}
	return runtime.localTableView(*table), nil
}

func (runtime *nativeRuntime) PublicTables() ([]NativePublicTableView, error) {
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

func (runtime *nativeRuntime) CashOut(tableID string) (map[string]any, error) {
	return runtime.completeFunds(tableID, "cashout", "completed")
}

func (runtime *nativeRuntime) Renew(tableID string) ([]map[string]any, error) {
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

func (runtime *nativeRuntime) Exit(tableID string) (map[string]any, error) {
	return runtime.completeFunds(tableID, "emergency-exit", "exited")
}

func (runtime *nativeRuntime) completeFunds(tableID, kind, finalStatus string) (map[string]any, error) {
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

func (runtime *nativeRuntime) requireLocalTable(tableID string) (*nativeTableState, error) {
	table, err := runtime.store.readTable(tableID)
	if err != nil {
		return nil, err
	}
	if table == nil {
		return nil, fmt.Errorf("table %s not found", tableID)
	}
	return table, nil
}

func (runtime *nativeRuntime) refreshLocalTable(tableID string) (*nativeTableState, error) {
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
			table = remote
		}
	}
	if table != nil && elapsedMillis(table.LastHostHeartbeatAt) > nativeHostFailureMS && runtime.shouldHandleFailover(*table) {
		if err := runtime.failoverTable(tableID, "missed host heartbeats"); err == nil {
			table, _ = runtime.store.readTable(tableID)
		}
	}
	return table, nil
}

func (runtime *nativeRuntime) ensureBootstrapLocked(nickname string) error {
	if runtime.walletID.PlayerID != "" && runtime.protocolID != "" {
		if nickname == "" {
			return nil
		}
	}

	state, err := runtime.walletRuntime.EnsureProfile(runtime.profileName, nickname)
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
	runtime.walletID = walletID
	runtime.peerIdentity = peerIdentity
	runtime.protocolIdentity = protocolIdentity
	runtime.protocolID = protocolIdentity.ID
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
		ProfileName:    runtime.profileName,
		ProtocolID:     protocolIdentity.ID,
		WalletPlayerID: walletID.PlayerID,
	}
	return nil
}

func (runtime *nativeRuntime) startPeerServerLocked() error {
	if runtime.listener != nil {
		if runtime.self.Peer.PeerURL == "" {
			if tcpAddr, ok := runtime.listener.Addr().(*net.TCPAddr); ok {
				host := runtime.config.PeerHost
				if host == "" || host == "0.0.0.0" || host == "::" {
					host = "127.0.0.1"
				}
				runtime.self.Peer.PeerURL = fmt.Sprintf("ws://%s:%d/mesh", host, tcpAddr.Port)
			}
		}
		return nil
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
	tcpAddr, _ := listener.Addr().(*net.TCPAddr)
	port := runtime.config.PeerPort
	if tcpAddr != nil && tcpAddr.Port != 0 {
		port = tcpAddr.Port
	}
	if host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	runtime.self.Peer.PeerURL = fmt.Sprintf("ws://%s:%d/mesh", host, port)

	server := &http.Server{Handler: runtime.routes()}
	runtime.server = server
	go func() {
		_ = server.Serve(listener)
	}()
	return nil
}

func (runtime *nativeRuntime) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/native/peer", func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			writer.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		runtime.writeJSON(writer, runtime.self)
	})
	mux.HandleFunc("/native/table/join", func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost {
			writer.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var join nativeJoinRequest
		if err := json.NewDecoder(request.Body).Decode(&join); err != nil {
			writer.WriteHeader(http.StatusBadRequest)
			_ = runtime.writeJSON(writer, map[string]any{"error": err.Error()})
			return
		}
		table, err := runtime.handleJoinFromPeer(join)
		if err != nil {
			writer.WriteHeader(http.StatusBadRequest)
			_ = runtime.writeJSON(writer, map[string]any{"error": err.Error()})
			return
		}
		_ = runtime.writeJSON(writer, table)
	})
	mux.HandleFunc("/native/table/action", func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost {
			writer.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var action nativeActionRequest
		if err := json.NewDecoder(request.Body).Decode(&action); err != nil {
			writer.WriteHeader(http.StatusBadRequest)
			_ = runtime.writeJSON(writer, map[string]any{"error": err.Error()})
			return
		}
		table, err := runtime.handleActionFromPeer(action)
		if err != nil {
			writer.WriteHeader(http.StatusBadRequest)
			_ = runtime.writeJSON(writer, map[string]any{"error": err.Error()})
			return
		}
		_ = runtime.writeJSON(writer, table)
	})
	mux.HandleFunc("/native/table/sync", func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost {
			writer.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var table nativeTableState
		if err := json.NewDecoder(request.Body).Decode(&table); err != nil {
			writer.WriteHeader(http.StatusBadRequest)
			_ = runtime.writeJSON(writer, map[string]any{"error": err.Error()})
			return
		}
		if err := runtime.acceptRemoteTable(table); err != nil {
			writer.WriteHeader(http.StatusBadRequest)
			_ = runtime.writeJSON(writer, map[string]any{"error": err.Error()})
			return
		}
		_ = runtime.writeJSON(writer, map[string]any{"ok": true})
	})
	mux.HandleFunc("/native/table/", func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			writer.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		tableID := strings.TrimPrefix(request.URL.Path, "/native/table/")
		table, err := runtime.store.readTable(tableID)
		if err != nil || table == nil {
			writer.WriteHeader(http.StatusNotFound)
			_ = runtime.writeJSON(writer, map[string]any{"error": "table not found"})
			return
		}
		_ = runtime.writeJSON(writer, table)
	})
	return mux
}

func (runtime *nativeRuntime) handleJoinFromPeer(join nativeJoinRequest) (nativeTableState, error) {
	var updated nativeTableState
	err := runtime.store.withTableLock(join.TableID, func() error {
		table, err := runtime.store.readTable(join.TableID)
		if err != nil || table == nil {
			return fmt.Errorf("table %s not found", join.TableID)
		}
		if table.CurrentHost.Peer.PeerID != runtime.selfPeerID() {
			return errors.New("join request must be sent to the current host")
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

func (runtime *nativeRuntime) handleActionFromPeer(request nativeActionRequest) (nativeTableState, error) {
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
		seatIndex := -1
		for _, seat := range table.Seats {
			if seat.PlayerID == request.PlayerID {
				seatIndex = seat.SeatIndex
				break
			}
		}
		if seatIndex < 0 {
			return errors.New("player is not seated")
		}
		nextState, err := game.ApplyHoldemAction(table.ActiveHand.State, seatIndex, request.Action)
		if err != nil {
			return err
		}
		table.ActiveHand.State = nextState
		publicState := runtime.publicStateFromHand(*table, nextState)
		table.PublicState = &publicState
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
		if nextState.Phase == game.StreetSettled {
			table.Config.Status = "active"
			snapshot, err := runtime.buildSnapshot(*table, publicState)
			if err != nil {
				return err
			}
			table.LatestSnapshot = &snapshot
			table.LatestFullySignedSnapshot = &snapshot
			table.Snapshots = append(table.Snapshots, snapshot)
			if err := runtime.appendEvent(table, map[string]any{
				"type":           "HandResult",
				"balances":       publicState.ChipBalances,
				"checkpointHash": runtime.snapshotHash(snapshot),
				"handId":         nextState.HandID,
				"publicState":    rawJSONMap(publicState),
				"winners":        rawJSONMap(nextState.Winners),
			}); err != nil {
				return err
			}
			table.NextHandAt = addMillis(nowISO(), nativeNextHandDelayMS)
		}
		if err := runtime.persistAndReplicate(table, true); err != nil {
			return err
		}
		updated = *table
		return nil
	})
	return updated, err
}

func (runtime *nativeRuntime) startNextHandLocked(table *nativeTableState) error {
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
	deckSeed, err := settlementcore.RandomHex(32)
	if err != nil {
		return err
	}
	hand, err := game.CreateHoldemHand(game.HoldemHandConfig{
		BigBlindSats:    table.Config.BigBlindSats,
		DealerSeatIndex: dealerSeat,
		DeckSeedHex:     deckSeed,
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
	commitmentRoot, err := settlementcore.HashStructuredDataHex(map[string]any{
		"deckSeedHex": deckSeed,
		"tableId":     table.Config.TableID,
	})
	if err != nil {
		return err
	}
	holeCards := map[string][]string{}
	for _, player := range hand.Players {
		holeCards[player.PlayerID] = []string{string(player.HoleCards[0]), string(player.HoleCards[1])}
	}
	table.ActiveHand = &nativeActiveHand{
		CommitmentRoot:      commitmentRoot,
		HoleCardsByPlayerID: holeCards,
		State:               hand,
	}
	publicState := runtime.publicStateFromHand(*table, hand)
	table.PublicState = &publicState
	table.Config.Status = "active"
	table.NextHandAt = ""
	return runtime.appendEvent(table, map[string]any{
		"type":            "HandStart",
		"dealerSeatIndex": dealerSeat,
		"handId":          hand.HandID,
		"handNumber":      hand.HandNumber,
		"publicState":     rawJSONMap(publicState),
	})
}

func (runtime *nativeRuntime) failoverTable(tableID, reason string) error {
	return runtime.store.withTableLock(tableID, func() error {
		table, err := runtime.store.readTable(tableID)
		if err != nil || table == nil {
			return err
		}
		if elapsedMillis(table.LastHostHeartbeatAt) <= nativeHostFailureMS {
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
		active := table.PublicState != nil && table.PublicState.HandID != nil && table.PublicState.Phase != nil && table.PublicState.Phase != "settled"
		if active && table.PublicState != nil && table.LatestFullySignedSnapshot != nil {
			rollbackHash := runtime.snapshotHash(*table.LatestFullySignedSnapshot)
			if err := runtime.appendEvent(table, map[string]any{
				"type":                 "HandAbort",
				"handId":               table.PublicState.HandID,
				"reason":               reason,
				"rollbackSnapshotHash": rollbackHash,
			}); err != nil {
				return err
			}
			restored := runtime.publicStateFromSnapshot(*table, *table.LatestFullySignedSnapshot)
			table.PublicState = &restored
			table.ActiveHand = nil
			snapshot, err := runtime.buildSnapshot(*table, restored)
			if err != nil {
				return err
			}
			table.LatestSnapshot = &snapshot
			table.LatestFullySignedSnapshot = &snapshot
			table.Snapshots = append(table.Snapshots, snapshot)
		} else {
			table.NextHandAt = addMillis(nowISO(), nativeNextHandDelayMS)
			if err := runtime.startNextHandLocked(table); err != nil {
				return err
			}
		}
		return runtime.persistAndReplicate(table, true)
	})
}

func (runtime *nativeRuntime) persistAndReplicate(table *nativeTableState, publish bool) error {
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

func (runtime *nativeRuntime) syncPrivateAndFunds(table nativeTableState) error {
	localState := map[string]any{
		"auditBundlesByHandId": map[string]any{},
		"myHoleCardsByHandId":  map[string]any{},
	}
	if table.ActiveHand != nil {
		if cards, ok := table.ActiveHand.HoleCardsByPlayerID[runtime.walletID.PlayerID]; ok && len(cards) == 2 {
			localState["myHoleCardsByHandId"] = map[string]any{
				table.ActiveHand.State.HandID: cards,
			}
		}
	}
	if err := runtime.store.writePrivateState(table.Config.TableID, localState); err != nil {
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

func (runtime *nativeRuntime) replicateTable(table nativeTableState) {
	targets := map[string]struct{}{}
	for _, witness := range table.Witnesses {
		if witness.Peer.PeerURL != "" && witness.Peer.PeerID != runtime.selfPeerID() {
			targets[witness.Peer.PeerURL] = struct{}{}
		}
	}
	for _, seat := range table.Seats {
		if seat.PeerID == runtime.selfPeerID() {
			continue
		}
		if peerURL := runtime.knownPeerURL(seat.PeerID); peerURL != "" {
			targets[peerURL] = struct{}{}
		}
	}
	for peerURL := range targets {
		_, _ = runtime.postJSON(peerURL, "/native/table/sync", table)
	}
}

func (runtime *nativeRuntime) publishPublicState(table nativeTableState) error {
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

func (runtime *nativeRuntime) buildAdvertisement(table nativeTableState) (NativeAdvertisement, error) {
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

func (runtime *nativeRuntime) appendEvent(table *nativeTableState, body map[string]any) error {
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

func (runtime *nativeRuntime) buildReadyPublicState(table nativeTableState) NativePublicTableState {
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

func (runtime *nativeRuntime) publicStateFromHand(table nativeTableState, hand game.HoldemState) NativePublicTableState {
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
	return NativePublicTableState{
		ActingSeatIndex: actingSeat,
		Board:           stringCards(hand.Board),
		ChipBalances:    chipBalances,
		CurrentBetSats:  hand.CurrentBetSats,
		DealerCommitment: &NativeDealerCommitment{
			CommittedAt: nowISO(),
			Mode:        nativeDealerMode,
			RootHash:    table.ActiveHand.CommitmentRoot,
		},
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

func (runtime *nativeRuntime) publicStateFromSnapshot(table nativeTableState, snapshot NativeCooperativeTableSnapshot) NativePublicTableState {
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

func (runtime *nativeRuntime) buildSnapshot(table nativeTableState, publicState NativePublicTableState) (NativeCooperativeTableSnapshot, error) {
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

func (runtime *nativeRuntime) snapshotHash(snapshot NativeCooperativeTableSnapshot) string {
	unsigned := rawJSONMap(snapshot)
	delete(unsigned, "signatures")
	hash, err := settlementcore.HashStructuredDataHex(unsigned)
	if err != nil {
		return ""
	}
	return hash
}

func (runtime *nativeRuntime) localTableView(table nativeTableState) NativeMeshTableView {
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
				if cards, ok := table.ActiveHand.HoleCardsByPlayerID[seat.PlayerID]; ok && len(cards) == 2 {
					myHoleCards = cards
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

func (runtime *nativeRuntime) tableSummary(table nativeTableState) NativeTableSummary {
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

func (runtime *nativeRuntime) roleForTable(table nativeTableState) string {
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

func (runtime *nativeRuntime) knownPeers() ([]NativePeerAddress, error) {
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

func (runtime *nativeRuntime) saveKnownPeer(peer NativePeerAddress) error {
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

func (runtime *nativeRuntime) loadProfileState() (*walletpkg.PlayerProfileState, error) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return runtime.loadProfileStateLocked()
}

func (runtime *nativeRuntime) loadProfileStateLocked() (*walletpkg.PlayerProfileState, error) {
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

func (runtime *nativeRuntime) currentTableID() string {
	profile, err := runtime.loadProfileState()
	if err != nil {
		return ""
	}
	if profile.CurrentTable != nil {
		return profile.CurrentTable.TableID
	}
	return profile.CurrentMeshTableID
}

func (runtime *nativeRuntime) acceptRemoteTable(table nativeTableState) error {
	if err := runtime.store.writeTable(&table); err != nil {
		return err
	}
	if err := runtime.store.rewriteEvents(table.Config.TableID, table.Events); err != nil {
		return err
	}
	if err := runtime.store.rewriteSnapshots(table.Config.TableID, table.Snapshots); err != nil {
		return err
	}
	if table.Advertisement != nil {
		_ = runtime.store.upsertPublicAd(*table.Advertisement)
	}
	profile, err := runtime.loadProfileState()
	if err != nil {
		return err
	}
	profile.CurrentMeshTableID = table.Config.TableID
	profile.CurrentTable = &walletpkg.TableSessionState{
		InviteCode: table.InviteCode,
		SeatIndex:  runtime.seatIndexForPlayer(table),
		TableID:    table.Config.TableID,
	}
	profile.MeshTables[table.Config.TableID] = walletpkg.MeshTableReferenceState{
		Config:       MustMarshalJSON(table.Config),
		CurrentEpoch: table.CurrentEpoch,
		HostPeerID:   table.CurrentHost.Peer.PeerID,
		HostPeerURL:  table.CurrentHost.Peer.PeerURL,
		Role:         runtime.roleForTable(table),
		TableID:      table.Config.TableID,
		Visibility:   table.Config.Visibility,
	}
	if err := runtime.profileStore.Save(*profile); err != nil {
		return err
	}
	return runtime.syncPrivateAndFunds(table)
}

func (runtime *nativeRuntime) seatIndexForPlayer(table nativeTableState) int {
	for _, seat := range table.Seats {
		if seat.PlayerID == runtime.walletID.PlayerID {
			return seat.SeatIndex
		}
	}
	return -1
}

func (runtime *nativeRuntime) fetchPeerInfo(peerURL string) (nativePeerSelf, error) {
	base, err := peerHTTPBase(peerURL)
	if err != nil {
		return nativePeerSelf{}, err
	}
	request, err := http.NewRequest(http.MethodGet, base+"/native/peer", nil)
	if err != nil {
		return nativePeerSelf{}, err
	}
	response, err := runtime.httpClient.Do(request)
	if err != nil {
		return nativePeerSelf{}, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(response.Body)
		return nativePeerSelf{}, fmt.Errorf("peer info request failed: %s", strings.TrimSpace(string(body)))
	}
	var decoded nativePeerSelf
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		return nativePeerSelf{}, err
	}
	return decoded, nil
}

func (runtime *nativeRuntime) fetchRemoteTable(peerURL, tableID string) (*nativeTableState, error) {
	base, err := peerHTTPBase(peerURL)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequest(http.MethodGet, base+"/native/table/"+tableID, nil)
	if err != nil {
		return nil, err
	}
	response, err := runtime.httpClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(response.Body)
		return nil, fmt.Errorf("remote table request failed: %s", strings.TrimSpace(string(body)))
	}
	var table nativeTableState
	if err := json.NewDecoder(response.Body).Decode(&table); err != nil {
		return nil, err
	}
	return &table, nil
}

func (runtime *nativeRuntime) remoteJoin(peerURL string, input nativeJoinRequest) (nativeTableState, error) {
	response, err := runtime.postJSON(peerURL, "/native/table/join", input)
	if err != nil {
		return nativeTableState{}, err
	}
	var table nativeTableState
	if err := json.Unmarshal(response, &table); err != nil {
		return nativeTableState{}, err
	}
	return table, nil
}

func (runtime *nativeRuntime) remoteAction(peerURL string, input nativeActionRequest) (nativeTableState, error) {
	response, err := runtime.postJSON(peerURL, "/native/table/action", input)
	if err != nil {
		return nativeTableState{}, err
	}
	var table nativeTableState
	if err := json.Unmarshal(response, &table); err != nil {
		return nativeTableState{}, err
	}
	return table, nil
}

func (runtime *nativeRuntime) postJSON(peerURL, path string, input any) ([]byte, error) {
	base := peerURL
	if !strings.HasPrefix(path, "/api/") {
		var err error
		base, err = peerHTTPBase(peerURL)
		if err != nil {
			return nil, err
		}
	}
	body, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequest(http.MethodPost, strings.TrimSuffix(base, "/")+path, bytes.NewReader(body))
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

func (runtime *nativeRuntime) shouldPollHost(tableID string) bool {
	last := runtime.lastSyncAt[tableID]
	return time.Since(last) >= nativeTableSyncInterval
}

func (runtime *nativeRuntime) shouldHandleFailover(table nativeTableState) bool {
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

func (runtime *nativeRuntime) knownPeerURL(peerID string) string {
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

func (runtime *nativeRuntime) appendFundsOperation(operation NativeTableFundsOperation, cashoutSats int, status string) error {
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

func (runtime *nativeRuntime) buildFundsOperation(tableID string, amountSats int, kind, status, checkpointHash string) (NativeTableFundsOperation, error) {
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

func (runtime *nativeRuntime) selfPeerID() string {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return runtime.self.Peer.PeerID
}

func (runtime *nativeRuntime) selfPeerURL() string {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return runtime.self.Peer.PeerURL
}

func (runtime *nativeRuntime) writeJSON(writer http.ResponseWriter, value any) error {
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
