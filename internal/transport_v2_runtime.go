package parker

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sort"
	"strings"
	"sync"

	"github.com/danieldresner/arkade_fun/internal/game"
	"github.com/danieldresner/arkade_fun/internal/settlementcore"
	walletpkg "github.com/danieldresner/arkade_fun/internal/wallet"
)

const transportV2WireVersion = 2

type transportV2Runtime struct {
	config           RuntimeConfig
	mode             string
	mu               sync.Mutex
	profileName      string
	profileStore     *walletpkg.ProfileStore
	protocolIdentity settlementcore.ScopedIdentity
	peerIdentity     settlementcore.ScopedIdentity
	started          bool
	store            *transportV2Store
	transportKeyID   string
	transportPrivate string
	transportPublic  string
	walletID         settlementcore.LocalIdentity
	walletRuntime    *walletpkg.Runtime
}

func newTransportV2Runtime(profileName string, config RuntimeConfig, mode string) (*transportV2Runtime, error) {
	if mode == "" {
		mode = "player"
	}
	store, err := newTransportV2Store(profileName, config)
	if err != nil {
		return nil, err
	}
	return &transportV2Runtime{
		config:       config,
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

func (runtime *transportV2Runtime) Start() error {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()

	if runtime.started {
		return nil
	}
	if err := runtime.ensureBootstrapLocked(""); err != nil {
		return err
	}
	runtime.started = true
	return nil
}

func (runtime *transportV2Runtime) Close() error {
	runtime.mu.Lock()
	runtime.started = false
	runtime.mu.Unlock()
	if runtime.store != nil {
		return runtime.store.close()
	}
	return nil
}

func (runtime *transportV2Runtime) Bootstrap(nickname string) (map[string]any, error) {
	runtime.mu.Lock()
	if err := runtime.ensureBootstrapLocked(nickname); err != nil {
		runtime.mu.Unlock()
		return nil, err
	}
	runtime.started = true
	runtime.mu.Unlock()

	state, err := runtime.transportState()
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"transport": rawJSONMap(state),
	}, nil
}

func (runtime *transportV2Runtime) Tick() {}

func (runtime *transportV2Runtime) CurrentState() (map[string]any, error) {
	state, err := runtime.transportState()
	if err != nil {
		return nil, err
	}
	wallet, err := runtime.walletRuntime.GetWallet(runtime.profileName)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"transport": rawJSONMap(state),
		"wallet":    rawJSONMap(wallet),
	}, nil
}

func (runtime *transportV2Runtime) QuickState() (map[string]any, error) {
	state, err := runtime.transportState()
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"transport": rawJSONMap(state),
	}, nil
}

func (runtime *transportV2Runtime) WalletSummary() (any, error) {
	return runtime.walletRuntime.GetWallet(runtime.profileName)
}

func (runtime *transportV2Runtime) WalletFaucet(amountSats int) (any, error) {
	if err := runtime.walletRuntime.Faucet(runtime.profileName, amountSats); err != nil {
		return nil, err
	}
	return runtime.walletRuntime.GetWallet(runtime.profileName)
}

func (runtime *transportV2Runtime) WalletOnboard() (any, error) {
	return runtime.walletRuntime.Onboard(runtime.profileName)
}

func (runtime *transportV2Runtime) WalletOffboard(address string, amountSats *int) (any, error) {
	return runtime.walletRuntime.Offboard(runtime.profileName, address, amountSats)
}

func (runtime *transportV2Runtime) WalletDeposit(amountSats int) (any, error) {
	return runtime.walletRuntime.CreateDepositQuote(runtime.profileName, amountSats)
}

func (runtime *transportV2Runtime) WalletWithdraw(amountSats int, invoice string) (any, error) {
	return runtime.walletRuntime.SubmitWithdrawal(runtime.profileName, amountSats, invoice)
}

func (runtime *transportV2Runtime) NetworkPeers() (any, error) {
	return runtime.store.listPeers()
}

func (runtime *transportV2Runtime) BootstrapPeer(endpoint, alias string, roles []string) (any, error) {
	if strings.TrimSpace(endpoint) == "" {
		return nil, errors.New("endpoint is required")
	}
	peerID := provisionalPeerID(endpoint)
	peer := TransportV2PeerSummary{
		Alias:            alias,
		Capabilities:     []string{"direct"},
		DirectOnion:      endpoint,
		Endpoint:         endpoint,
		LastSeenAt:       nowISO(),
		MailboxEndpoints: []string{},
		ManifestEpoch:    1,
		PeerID:           peerID,
		Roles:            append([]string{}, roles...),
		TransportEndpoints: TransportV2PeerEndpoints{
			DirectOnion: endpoint,
			Endpoint:    endpoint,
		},
	}
	if err := runtime.store.writePeer(peer); err != nil {
		return nil, err
	}
	return peer, nil
}

func (runtime *transportV2Runtime) CreateTable(input map[string]any) (any, error) {
	return nil, errors.New("transport v2 table creation is not implemented yet")
}

func (runtime *transportV2Runtime) AnnounceTable(tableID string) (any, error) {
	return nil, errors.New("transport v2 table announce is not implemented yet")
}

func (runtime *transportV2Runtime) JoinTable(inviteCode string, buyInSats int) (any, error) {
	return nil, errors.New("transport v2 join flow is not implemented yet")
}

func (runtime *transportV2Runtime) GetTable(tableID string) (any, error) {
	return nil, errors.New("transport v2 table reads are not implemented yet")
}

func (runtime *transportV2Runtime) SendAction(tableID string, action game.Action) (any, error) {
	return nil, errors.New("transport v2 actions are not implemented yet")
}

func (runtime *transportV2Runtime) RotateHost(tableID string) (any, error) {
	return nil, errors.New("transport v2 failover is not implemented yet")
}

func (runtime *transportV2Runtime) PublicTables() (any, error) {
	return []map[string]any{}, nil
}

func (runtime *transportV2Runtime) CashOut(tableID string) (any, error) {
	return nil, errors.New("transport v2 funds flows are not implemented yet")
}

func (runtime *transportV2Runtime) Renew(tableID string) (any, error) {
	return nil, errors.New("transport v2 funds flows are not implemented yet")
}

func (runtime *transportV2Runtime) Exit(tableID string) (any, error) {
	return nil, errors.New("transport v2 funds flows are not implemented yet")
}

func (runtime *transportV2Runtime) currentTableID() string {
	profile, err := runtime.loadProfileState()
	if err != nil || profile == nil {
		return ""
	}
	if profile.CurrentTable != nil {
		return profile.CurrentTable.TableID
	}
	return profile.CurrentMeshTableID
}

func (runtime *transportV2Runtime) transportState() (TransportV2RuntimeState, error) {
	peers, err := runtime.store.listPeers()
	if err != nil {
		return TransportV2RuntimeState{}, err
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i].PeerID < peers[j].PeerID })

	manifest, err := runtime.store.readManifest()
	if err != nil {
		return TransportV2RuntimeState{}, err
	}
	queues, err := runtime.store.queueState()
	if err != nil {
		return TransportV2RuntimeState{}, err
	}

	peerState := TransportV2LocalPeerState{
		PeerID:               runtime.peerIdentity.ID,
		ProtocolID:           runtime.protocolIdentity.ID,
		TransportKeyID:       runtime.transportKeyID,
		TransportWireVersion: transportV2WireVersion,
		WalletPlayerID:       runtime.walletID.PlayerID,
	}
	if manifest != nil {
		peerState.Endpoint = manifest.Endpoint
		peerState.DirectOnion = manifest.DirectOnion
		peerState.GossipOnion = manifest.GossipOnion
	}

	return TransportV2RuntimeState{
		BootstrapPeers:       append([]string{}, runtime.config.GossipBootstrap...),
		Mailboxes:            append([]string{}, runtime.config.MailboxEndpoints...),
		Mode:                 runtime.mode,
		Peer:                 peerState,
		Peers:                peers,
		Queues:               queues,
		TransportMode:        runtime.config.TransportMode,
		TransportWireVersion: transportV2WireVersion,
	}, nil
}

func (runtime *transportV2Runtime) ensureBootstrapLocked(nickname string) error {
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
	runtime.transportPrivate = state.TransportPrivateKeyHex
	runtime.transportPublic = transportPublic
	runtime.transportKeyID = transportKeyID(transportPublic)

	manifest, err := runtime.buildManifest()
	if err != nil {
		return err
	}
	return runtime.store.writeManifest(manifest)
}

func (runtime *transportV2Runtime) buildManifest() (TransportV2PeerManifest, error) {
	manifest := TransportV2PeerManifest{
		Capabilities:     []string{"gossip", "direct", "mailbox"},
		CreatedAt:        nowISO(),
		Endpoint:         "parker://" + runtime.peerIdentity.ID,
		ExpiresAt:        addMillis(nowISO(), 7*24*60*60*1000),
		MailboxEndpoints: append([]string{}, runtime.config.MailboxEndpoints...),
		ManifestEpoch:    1,
		PeerID:           runtime.peerIdentity.ID,
		ProtocolID:       runtime.protocolIdentity.ID,
		SignatureKeyID:   runtime.protocolIdentity.ID,
		SigningKey:       runtime.protocolIdentity.PublicKeyHex,
		TransportEncKey:  runtime.transportPublic,
		TransportEndpoints: TransportV2PeerEndpoints{
			Endpoint:         "parker://" + runtime.peerIdentity.ID,
			MailboxEndpoints: append([]string{}, runtime.config.MailboxEndpoints...),
		},
		TransportWireVersion: transportV2WireVersion,
	}
	unsigned := rawJSONMap(manifest)
	delete(unsigned, "signature")
	signature, err := settlementcore.SignStructuredData(runtime.protocolIdentity.PrivateKeyHex, unsigned)
	if err != nil {
		return TransportV2PeerManifest{}, err
	}
	manifest.Signature = signature
	return manifest, nil
}

func (runtime *transportV2Runtime) loadProfileState() (*walletpkg.PlayerProfileState, error) {
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

func randomX25519PrivateKeyHex() (string, error) {
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(key.Bytes()), nil
}

func x25519PublicKeyHex(privateKeyHex string) (string, error) {
	privateKeyBytes, err := hex.DecodeString(privateKeyHex)
	if err != nil {
		return "", err
	}
	privateKey, err := ecdh.X25519().NewPrivateKey(privateKeyBytes)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(privateKey.PublicKey().Bytes()), nil
}

func transportKeyID(publicKeyHex string) string {
	if len(publicKeyHex) <= 16 {
		return "transport-" + publicKeyHex
	}
	return "transport-" + publicKeyHex[:16]
}
