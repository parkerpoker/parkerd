package daemon

import (
	"errors"
	"sort"
	"strings"
	"sync"

	cfg "github.com/parkerpoker/parkerd/internal/config"
	"github.com/parkerpoker/parkerd/internal/game"
	"github.com/parkerpoker/parkerd/internal/meshruntime"
	"github.com/parkerpoker/parkerd/internal/settlementcore"
	transportpkg "github.com/parkerpoker/parkerd/internal/transport"
)

type daemonRuntimeAdapter struct {
	config           cfg.RuntimeConfig
	inner            meshruntime.Runtime
	mode             string
	mu               sync.Mutex
	profileName      string
	protocolIdentity settlementcore.ScopedIdentity
	peerIdentity     settlementcore.ScopedIdentity
	started          bool
	store            *meshruntime.TransportStore
	transportKeyID   string
	transportPrivate string
	transportPublic  string
	walletID         settlementcore.LocalIdentity
}

func newDaemonRuntimeAdapter(profileName string, config cfg.RuntimeConfig, mode string) (*daemonRuntimeAdapter, error) {
	if mode == "" {
		mode = "player"
	}
	inner, err := meshruntime.NewRuntime(profileName, config, mode)
	if err != nil {
		return nil, err
	}
	return &daemonRuntimeAdapter{
		config:      config,
		inner:       inner,
		mode:        mode,
		profileName: profileName,
		store:       meshruntime.NewTransportStoreWithRepository(profileName, config, inner.Repository(), false),
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
		joined = errors.Join(joined, runtime.store.Close())
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
	return runtime.inner.WalletNsec()
}

func (runtime *daemonRuntimeAdapter) WalletSummary() (any, error) {
	return runtime.inner.WalletSummary()
}

func (runtime *daemonRuntimeAdapter) WalletFaucet(amountSats int) (any, error) {
	return runtime.inner.WalletFaucet(amountSats)
}

func (runtime *daemonRuntimeAdapter) WalletOnboard() (any, error) {
	return runtime.inner.WalletOnboard()
}

func (runtime *daemonRuntimeAdapter) WalletOffboard(address string, amountSats *int) (any, error) {
	return runtime.inner.WalletOffboard(address, amountSats)
}

func (runtime *daemonRuntimeAdapter) WalletDeposit(amountSats int) (any, error) {
	return runtime.inner.WalletDeposit(amountSats)
}

func (runtime *daemonRuntimeAdapter) WalletWithdraw(amountSats int, invoice string) (any, error) {
	return runtime.inner.WalletWithdraw(amountSats, invoice)
}

func (runtime *daemonRuntimeAdapter) NetworkPeers() (any, error) {
	if err := runtime.refreshTransportState(""); err != nil {
		return nil, err
	}
	return runtime.store.ListPeers()
}

func (runtime *daemonRuntimeAdapter) BootstrapPeer(endpoint, alias string, roles []string) (any, error) {
	if strings.TrimSpace(endpoint) == "" {
		return nil, errors.New("endpoint is required")
	}
	if _, err := runtime.inner.BootstrapPeer(endpoint, alias, roles); err != nil {
		return nil, err
	}
	peerInfo, err := runtime.inner.FetchPeerInfo(endpoint)
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
	if err := runtime.store.WritePeer(peer); err != nil {
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
	return runtime.inner.CurrentTableID()
}

func (runtime *daemonRuntimeAdapter) transportState() (transportpkg.TransportRuntimeState, error) {
	peers, err := runtime.store.ListPeers()
	if err != nil {
		return transportpkg.TransportRuntimeState{}, err
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i].PeerID < peers[j].PeerID })

	manifest, err := runtime.store.ReadManifest()
	if err != nil {
		return transportpkg.TransportRuntimeState{}, err
	}
	queues, err := runtime.store.QueueState()
	if err != nil {
		return transportpkg.TransportRuntimeState{}, err
	}

	peerState := transportpkg.TransportLocalPeerState{
		PeerID:               runtime.peerIdentity.ID,
		ProtocolID:           runtime.protocolIdentity.ID,
		TransportKeyID:       runtime.transportKeyID,
		TransportWireVersion: meshruntime.TransportWireVersion,
		WalletPlayerID:       runtime.walletID.PlayerID,
	}
	if manifest != nil {
		peerState.Endpoint = manifest.Endpoint
		peerState.DirectOnion = manifest.DirectOnion
		peerState.GossipOnion = manifest.GossipOnion
	}

	return transportpkg.TransportRuntimeState{
		BootstrapPeers:       append([]string{}, runtime.config.GossipBootstrap...),
		Mailboxes:            append([]string{}, runtime.config.MailboxEndpoints...),
		Mode:                 runtime.mode,
		Peer:                 peerState,
		Peers:                peers,
		Queues:               queues,
		TransportWireVersion: meshruntime.TransportWireVersion,
	}, nil
}

func (runtime *daemonRuntimeAdapter) refreshTransportState(nickname string) error {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return runtime.refreshTransportStateLocked(nickname)
}

func (runtime *daemonRuntimeAdapter) refreshTransportStateLocked(nickname string) error {
	if err := runtime.inner.UpdateNickname(nickname); err != nil {
		return err
	}

	identity := runtime.inner.Identity()
	runtime.walletID = identity.WalletID
	runtime.peerIdentity = identity.PeerIdentity
	runtime.protocolIdentity = identity.ProtocolIdentity
	runtime.transportPrivate = identity.TransportPrivate
	runtime.transportPublic = identity.TransportPublic
	runtime.transportKeyID = identity.TransportKeyID

	manifest, err := runtime.buildManifest()
	if err != nil {
		return err
	}
	if err := runtime.store.WriteManifest(manifest); err != nil {
		return err
	}
	peers, err := runtime.inner.KnownPeers()
	if err != nil {
		return err
	}
	for _, peer := range peers {
		if err := runtime.store.WritePeer(runtime.transportPeerFromKnownPeer(peer)); err != nil {
			return err
		}
	}
	return nil
}

func (runtime *daemonRuntimeAdapter) buildManifest() (transportpkg.TransportPeerManifest, error) {
	endpoint := runtime.inner.SelfPeerURL()
	if endpoint == "" {
		endpoint = "parker://" + runtime.peerIdentity.ID
	}
	transportEndpoints := transportEndpointsForPeerURL(endpoint, runtime.config.MailboxEndpoints)
	manifest := transportpkg.TransportPeerManifest{
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
		TransportWireVersion: meshruntime.TransportWireVersion,
	}
	unsigned := rawJSONMap(manifest)
	delete(unsigned, "signature")
	signature, err := settlementcore.SignStructuredData(runtime.protocolIdentity.PrivateKeyHex, unsigned)
	if err != nil {
		return transportpkg.TransportPeerManifest{}, err
	}
	manifest.Signature = signature
	return manifest, nil
}

func (runtime *daemonRuntimeAdapter) transportPeerFromSelf(peer meshruntime.PeerInfo) transportpkg.TransportPeerSummary {
	endpoint := peer.Peer.PeerURL
	transportEndpoints := transportEndpointsForPeerURL(endpoint, nil)
	return transportpkg.TransportPeerSummary{
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

func (runtime *daemonRuntimeAdapter) transportPeerFromKnownPeer(peer meshruntime.NativePeerAddress) transportpkg.TransportPeerSummary {
	transportEndpoints := transportEndpointsForPeerURL(peer.PeerURL, nil)
	return transportpkg.TransportPeerSummary{
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

func transportEndpointsForPeerURL(endpoint string, mailboxes []string) transportpkg.TransportPeerEndpoints {
	return meshruntime.TransportEndpointsForPeerURL(endpoint, mailboxes)
}
