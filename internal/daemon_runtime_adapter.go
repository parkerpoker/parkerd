package parker

import (
	"errors"
	"sort"
	"strings"
	"sync"

	"github.com/parkerpoker/parkerd/internal/game"
	"github.com/parkerpoker/parkerd/internal/settlementcore"
	walletpkg "github.com/parkerpoker/parkerd/internal/wallet"
)

const transportWireVersion = 2

type daemonRuntimeAdapter struct {
	config           RuntimeConfig
	inner            *meshRuntime
	mode             string
	mu               sync.Mutex
	profileName      string
	profileStore     *walletpkg.ProfileStore
	protocolIdentity settlementcore.ScopedIdentity
	peerIdentity     settlementcore.ScopedIdentity
	started          bool
	store            *transportStore
	transportKeyID   string
	transportPrivate string
	transportPublic  string
	walletID         settlementcore.LocalIdentity
	walletRuntime    *walletpkg.Runtime
}

func newDaemonRuntimeAdapter(profileName string, config RuntimeConfig, mode string) (*daemonRuntimeAdapter, error) {
	if mode == "" {
		mode = "player"
	}
	inner, err := newMeshRuntime(profileName, config, mode)
	if err != nil {
		return nil, err
	}
	return &daemonRuntimeAdapter{
		config:        config,
		inner:         inner,
		mode:          mode,
		profileName:   profileName,
		profileStore:  inner.profileStore,
		store:         newTransportStoreWithRepository(profileName, config, inner.store.repository, false),
		walletRuntime: inner.walletRuntime,
	}, nil
}

func (runtime *daemonRuntimeAdapter) Start() error {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()

	if err := runtime.inner.Start(); err != nil {
		return err
	}
	if err := runtime.refreshTransportStateLocked(""); err != nil {
		return err
	}
	runtime.started = true
	return nil
}

func (runtime *daemonRuntimeAdapter) Close() error {
	runtime.mu.Lock()
	runtime.started = false
	runtime.mu.Unlock()
	var joined error
	if runtime.store != nil {
		joined = errors.Join(joined, runtime.store.close())
	}
	if runtime.inner != nil {
		joined = errors.Join(joined, runtime.inner.Close())
	}
	return joined
}

func (runtime *daemonRuntimeAdapter) Bootstrap(nickname, walletNsec string) (map[string]any, error) {
	base, err := runtime.inner.Bootstrap(nickname, walletNsec)
	if err != nil {
		return nil, err
	}
	runtime.mu.Lock()
	if err := runtime.refreshTransportStateLocked(nickname); err != nil {
		runtime.mu.Unlock()
		return nil, err
	}
	runtime.started = true
	runtime.mu.Unlock()

	state, err := runtime.transportState()
	if err != nil {
		return nil, err
	}
	if base == nil {
		base = map[string]any{}
	}
	base["transport"] = rawJSONMap(state)
	return base, nil
}

func (runtime *daemonRuntimeAdapter) Tick() {
	runtime.inner.Tick()
	_ = runtime.refreshTransportState("")
}

func (runtime *daemonRuntimeAdapter) CurrentState() (map[string]any, error) {
	base, err := runtime.inner.CurrentState()
	if err != nil {
		return nil, err
	}
	state, err := runtime.transportState()
	if err != nil {
		return nil, err
	}
	base["transport"] = rawJSONMap(state)
	return base, nil
}

func (runtime *daemonRuntimeAdapter) QuickState() (map[string]any, error) {
	base, err := runtime.inner.QuickState()
	if err != nil {
		return nil, err
	}
	state, err := runtime.transportState()
	if err != nil {
		return nil, err
	}
	base["transport"] = rawJSONMap(state)
	return base, nil
}

func (runtime *daemonRuntimeAdapter) WalletNsec() (any, error) {
	return runtime.walletRuntime.WalletNsec(runtime.profileName)
}

func (runtime *daemonRuntimeAdapter) WalletSummary() (any, error) {
	return runtime.inner.walletSummary()
}

func (runtime *daemonRuntimeAdapter) WalletFaucet(amountSats int) (any, error) {
	if err := runtime.walletRuntime.Faucet(runtime.profileName, amountSats); err != nil {
		return nil, err
	}
	return runtime.inner.walletSummary()
}

func (runtime *daemonRuntimeAdapter) WalletOnboard() (any, error) {
	return runtime.walletRuntime.Onboard(runtime.profileName)
}

func (runtime *daemonRuntimeAdapter) WalletOffboard(address string, amountSats *int) (any, error) {
	return runtime.walletRuntime.Offboard(runtime.profileName, address, amountSats)
}

func (runtime *daemonRuntimeAdapter) WalletDeposit(amountSats int) (any, error) {
	return runtime.walletRuntime.CreateDepositQuote(runtime.profileName, amountSats)
}

func (runtime *daemonRuntimeAdapter) WalletWithdraw(amountSats int, invoice string) (any, error) {
	return runtime.walletRuntime.SubmitWithdrawal(runtime.profileName, amountSats, invoice)
}

func (runtime *daemonRuntimeAdapter) NetworkPeers() (any, error) {
	if err := runtime.refreshTransportState(""); err != nil {
		return nil, err
	}
	return runtime.store.listPeers()
}

func (runtime *daemonRuntimeAdapter) BootstrapPeer(endpoint, alias string, roles []string) (any, error) {
	if strings.TrimSpace(endpoint) == "" {
		return nil, errors.New("endpoint is required")
	}
	if _, err := runtime.inner.BootstrapPeer(endpoint, alias, roles); err != nil {
		return nil, err
	}
	peerInfo, err := runtime.inner.fetchPeerInfo(endpoint)
	if err != nil {
		return nil, err
	}
	peer := runtime.transportPeerFromSelf(peerInfo)
	if alias != "" {
		peer.Alias = alias
	}
	if len(roles) > 0 {
		peer.Roles = append([]string{}, roles...)
	}
	if err := runtime.store.writePeer(peer); err != nil {
		return nil, err
	}
	return peer, nil
}

func (runtime *daemonRuntimeAdapter) CreateTable(input map[string]any) (any, error) {
	return runtime.inner.CreateTable(input)
}

func (runtime *daemonRuntimeAdapter) AnnounceTable(tableID string) (any, error) {
	return runtime.inner.AnnounceTable(tableID)
}

func (runtime *daemonRuntimeAdapter) JoinTable(inviteCode string, buyInSats int) (any, error) {
	return runtime.inner.JoinTable(inviteCode, buyInSats)
}

func (runtime *daemonRuntimeAdapter) GetTable(tableID string) (any, error) {
	return runtime.inner.GetTable(tableID)
}

func (runtime *daemonRuntimeAdapter) SendAction(tableID string, action game.Action) (any, error) {
	return runtime.inner.SendAction(tableID, action)
}

func (runtime *daemonRuntimeAdapter) RotateHost(tableID string) (any, error) {
	return runtime.inner.RotateHost(tableID)
}

func (runtime *daemonRuntimeAdapter) PublicTables() (any, error) {
	return runtime.inner.PublicTables()
}

func (runtime *daemonRuntimeAdapter) CashOut(tableID string) (any, error) {
	return runtime.inner.CashOut(tableID)
}

func (runtime *daemonRuntimeAdapter) Renew(tableID string) (any, error) {
	return runtime.inner.Renew(tableID)
}

func (runtime *daemonRuntimeAdapter) Exit(tableID string) (any, error) {
	return runtime.inner.Exit(tableID)
}

func (runtime *daemonRuntimeAdapter) currentTableID() string {
	return runtime.inner.currentTableID()
}

func (runtime *daemonRuntimeAdapter) transportState() (TransportRuntimeState, error) {
	peers, err := runtime.store.listPeers()
	if err != nil {
		return TransportRuntimeState{}, err
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i].PeerID < peers[j].PeerID })

	manifest, err := runtime.store.readManifest()
	if err != nil {
		return TransportRuntimeState{}, err
	}
	queues, err := runtime.store.queueState()
	if err != nil {
		return TransportRuntimeState{}, err
	}

	peerState := TransportLocalPeerState{
		PeerID:               runtime.peerIdentity.ID,
		ProtocolID:           runtime.protocolIdentity.ID,
		TransportKeyID:       runtime.transportKeyID,
		TransportWireVersion: transportWireVersion,
		WalletPlayerID:       runtime.walletID.PlayerID,
	}
	if manifest != nil {
		peerState.Endpoint = manifest.Endpoint
		peerState.DirectOnion = manifest.DirectOnion
		peerState.GossipOnion = manifest.GossipOnion
	}

	return TransportRuntimeState{
		BootstrapPeers:       append([]string{}, runtime.config.GossipBootstrap...),
		Mailboxes:            append([]string{}, runtime.config.MailboxEndpoints...),
		Mode:                 runtime.mode,
		Peer:                 peerState,
		Peers:                peers,
		Queues:               queues,
		TransportWireVersion: transportWireVersion,
	}, nil
}

func (runtime *daemonRuntimeAdapter) refreshTransportState(nickname string) error {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return runtime.refreshTransportStateLocked(nickname)
}

func (runtime *daemonRuntimeAdapter) refreshTransportStateLocked(nickname string) error {
	if nickname != "" {
		profile, err := runtime.loadProfileState()
		if err == nil && profile != nil {
			profile.Nickname = nickname
			if saveErr := runtime.profileStore.Save(*profile); saveErr != nil {
				return saveErr
			}
		}
	}
	profile, err := runtime.loadProfileState()
	if err != nil {
		return err
	}
	if profile == nil {
		return errors.New("profile state is required")
	}
	runtime.walletID = runtime.inner.walletID
	runtime.peerIdentity = runtime.inner.peerIdentity
	runtime.protocolIdentity = runtime.inner.protocolIdentity
	runtime.transportPrivate = runtime.inner.transportPrivate
	runtime.transportPublic = runtime.inner.transportPublic
	runtime.transportKeyID = runtime.inner.transportKeyID

	manifest, err := runtime.buildManifest()
	if err != nil {
		return err
	}
	if err := runtime.store.writeManifest(manifest); err != nil {
		return err
	}
	peers, err := runtime.inner.knownPeers()
	if err != nil {
		return err
	}
	for _, peer := range peers {
		if err := runtime.store.writePeer(runtime.transportPeerFromKnownPeer(peer)); err != nil {
			return err
		}
	}
	return nil
}

func (runtime *daemonRuntimeAdapter) buildManifest() (TransportPeerManifest, error) {
	endpoint := runtime.inner.selfPeerURL()
	if endpoint == "" {
		endpoint = "parker://" + runtime.peerIdentity.ID
	}
	transportEndpoints := transportEndpointsForPeerURL(endpoint, runtime.config.MailboxEndpoints)
	manifest := TransportPeerManifest{
		Capabilities:         []string{"direct", "state-sync", "table"},
		CreatedAt:            nowISO(),
		DirectOnion:          transportEndpoints.DirectOnion,
		Endpoint:             endpoint,
		ExpiresAt:            addMillis(nowISO(), 7*24*60*60*1000),
		MailboxEndpoints:     append([]string{}, runtime.config.MailboxEndpoints...),
		ManifestEpoch:        1,
		PeerID:               runtime.peerIdentity.ID,
		ProtocolID:           runtime.protocolIdentity.ID,
		SignatureKeyID:       runtime.protocolIdentity.PublicKeyHex,
		SigningKey:           runtime.protocolIdentity.PublicKeyHex,
		TransportEncKey:      runtime.transportPublic,
		TransportEndpoints:   transportEndpoints,
		TransportWireVersion: transportWireVersion,
	}
	unsigned := rawJSONMap(manifest)
	delete(unsigned, "signature")
	signature, err := settlementcore.SignStructuredData(runtime.protocolIdentity.PrivateKeyHex, unsigned)
	if err != nil {
		return TransportPeerManifest{}, err
	}
	manifest.Signature = signature
	return manifest, nil
}

func (runtime *daemonRuntimeAdapter) transportPeerFromSelf(peer nativePeerSelf) TransportPeerSummary {
	endpoint := peer.Peer.PeerURL
	transportEndpoints := transportEndpointsForPeerURL(endpoint, nil)
	return TransportPeerSummary{
		Alias:              peer.Alias,
		Capabilities:       []string{"direct", "state-sync", "table"},
		DirectOnion:        transportEndpoints.DirectOnion,
		Endpoint:           endpoint,
		LastSeenAt:         nowISO(),
		ManifestEpoch:      1,
		PeerID:             peer.Peer.PeerID,
		ProtocolID:         peer.ProtocolID,
		Roles:              append([]string{}, peer.Peer.Roles...),
		TransportEndpoints: transportEndpoints,
	}
}

func (runtime *daemonRuntimeAdapter) transportPeerFromKnownPeer(peer NativePeerAddress) TransportPeerSummary {
	transportEndpoints := transportEndpointsForPeerURL(peer.PeerURL, nil)
	return TransportPeerSummary{
		Alias:              peer.Alias,
		Capabilities:       []string{"direct", "state-sync", "table"},
		DirectOnion:        transportEndpoints.DirectOnion,
		Endpoint:           peer.PeerURL,
		LastSeenAt:         peer.LastSeenAt,
		ManifestEpoch:      1,
		PeerID:             peer.PeerID,
		Roles:              append([]string{}, peer.Roles...),
		TransportEndpoints: transportEndpoints,
	}
}

func transportEndpointsForPeerURL(endpoint string, mailboxes []string) TransportPeerEndpoints {
	transportEndpoints := TransportPeerEndpoints{
		Endpoint:         endpoint,
		MailboxEndpoints: append([]string{}, mailboxes...),
	}
	if isOnionPeerURL(endpoint) {
		transportEndpoints.DirectOnion = endpoint
	}
	return transportEndpoints
}

func (runtime *daemonRuntimeAdapter) loadProfileState() (*walletpkg.PlayerProfileState, error) {
	state, err := runtime.profileStore.Load(runtime.profileName)
	if err != nil || state == nil {
		return state, err
	}
	if state.KnownPeers == nil {
		state.KnownPeers = []walletpkg.KnownPeerState{}
	}
	if state.MeshTables == nil {
		state.MeshTables = map[string]walletpkg.MeshTableReferenceState{}
	}
	return state, nil
}

func transportKeyID(publicKeyHex string) string {
	if len(publicKeyHex) <= 16 {
		return "transport-" + publicKeyHex
	}
	return "transport-" + publicKeyHex[:16]
}
