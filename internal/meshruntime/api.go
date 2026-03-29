package meshruntime

import (
	cfg "github.com/parkerpoker/parkerd/internal/config"
	"github.com/parkerpoker/parkerd/internal/game"
	"github.com/parkerpoker/parkerd/internal/settlementcore"
	storepkg "github.com/parkerpoker/parkerd/internal/storage"
	transportpkg "github.com/parkerpoker/parkerd/internal/transport"
	walletpkg "github.com/parkerpoker/parkerd/internal/wallet"
)

type Runtime interface {
	Start() error
	Close() error
	Bootstrap(nickname, walletNsec string) (map[string]any, error)
	Tick()
	CurrentState() (map[string]any, error)
	QuickState() (map[string]any, error)
	WalletSummary() (walletpkg.WalletSummary, error)
	WalletNsec() (any, error)
	WalletFaucet(amountSats int) (walletpkg.WalletSummary, error)
	WalletOnboard() (any, error)
	WalletOffboard(address string, amountSats *int) (any, error)
	WalletDeposit(amountSats int) (any, error)
	WalletWithdraw(amountSats int, invoice string) (any, error)
	NetworkPeers() ([]NativePeerAddress, error)
	BootstrapPeer(peerURL, alias string, roles []string) (NativePeerAddress, error)
	CreateTable(input map[string]any) (map[string]any, error)
	CreatedTables(cursor string, limit int) (NativeCreatedTablesPage, error)
	AnnounceTable(tableID string) (map[string]any, error)
	JoinTable(inviteCode string, buyInSats int) (NativeMeshTableView, error)
	GetTable(tableID string) (NativeMeshTableView, error)
	SendAction(tableID string, action game.Action) (NativeMeshTableView, error)
	RotateHost(tableID string) (NativeMeshTableView, error)
	PublicTables() ([]NativePublicTableView, error)
	CashOut(tableID string) (map[string]any, error)
	Renew(tableID string) ([]map[string]any, error)
	Exit(tableID string) (map[string]any, error)
	CurrentTableID() string
	SelfPeerURL() string
	KnownPeers() ([]NativePeerAddress, error)
	FetchPeerInfo(peerURL string) (PeerInfo, error)
	Identity() RuntimeIdentity
	Repository() *storepkg.RuntimeRepository
	UpdateNickname(nickname string) error
}

type RuntimeIdentity struct {
	PeerIdentity     settlementcore.ScopedIdentity
	ProtocolIdentity settlementcore.ScopedIdentity
	TransportKeyID   string
	TransportPrivate string
	TransportPublic  string
	WalletID         settlementcore.LocalIdentity
}

type PeerInfo struct {
	Alias              string
	Mode               string
	Peer               NativePeerAddress
	ProfileName        string
	ProtocolID         string
	TransportPubkeyHex string
	WalletPlayerID     string
}

func NewRuntime(profileName string, config cfg.RuntimeConfig, mode string) (Runtime, error) {
	return newMeshRuntime(profileName, config, mode)
}

func (runtime *meshRuntime) WalletSummary() (walletpkg.WalletSummary, error) {
	return runtime.walletSummary()
}

func (runtime *meshRuntime) WalletNsec() (any, error) {
	return runtime.walletRuntime.WalletNsec(runtime.profileName)
}

func (runtime *meshRuntime) WalletFaucet(amountSats int) (walletpkg.WalletSummary, error) {
	if err := runtime.walletRuntime.Faucet(runtime.profileName, amountSats); err != nil {
		return walletpkg.WalletSummary{}, err
	}
	return runtime.walletSummary()
}

func (runtime *meshRuntime) WalletOnboard() (any, error) {
	return runtime.walletRuntime.Onboard(runtime.profileName)
}

func (runtime *meshRuntime) WalletOffboard(address string, amountSats *int) (any, error) {
	return runtime.walletRuntime.Offboard(runtime.profileName, address, amountSats)
}

func (runtime *meshRuntime) WalletDeposit(amountSats int) (any, error) {
	return runtime.walletRuntime.CreateDepositQuote(runtime.profileName, amountSats)
}

func (runtime *meshRuntime) WalletWithdraw(amountSats int, invoice string) (any, error) {
	return runtime.walletRuntime.SubmitWithdrawal(runtime.profileName, amountSats, invoice)
}

func (runtime *meshRuntime) CurrentTableID() string {
	return runtime.currentTableID()
}

func (runtime *meshRuntime) SelfPeerURL() string {
	return runtime.selfPeerURL()
}

func (runtime *meshRuntime) KnownPeers() ([]NativePeerAddress, error) {
	return runtime.knownPeers()
}

func (runtime *meshRuntime) FetchPeerInfo(peerURL string) (PeerInfo, error) {
	peer, err := runtime.fetchPeerInfo(peerURL)
	if err != nil {
		return PeerInfo{}, err
	}
	return PeerInfo{
		Alias:              peer.Alias,
		Mode:               peer.Mode,
		Peer:               peer.Peer,
		ProfileName:        peer.ProfileName,
		ProtocolID:         peer.ProtocolID,
		TransportPubkeyHex: peer.TransportPubkeyHex,
		WalletPlayerID:     peer.WalletPlayerID,
	}, nil
}

func (runtime *meshRuntime) Identity() RuntimeIdentity {
	return RuntimeIdentity{
		PeerIdentity:     runtime.peerIdentity,
		ProtocolIdentity: runtime.protocolIdentity,
		TransportKeyID:   runtime.transportKeyID,
		TransportPrivate: runtime.transportPrivate,
		TransportPublic:  runtime.transportPublic,
		WalletID:         runtime.walletID,
	}
}

func (runtime *meshRuntime) Repository() *storepkg.RuntimeRepository {
	if runtime.store == nil {
		return nil
	}
	return runtime.store.repository
}

func (runtime *meshRuntime) UpdateNickname(nickname string) error {
	if nickname == "" {
		return nil
	}
	profile, err := runtime.loadProfileState()
	if err != nil {
		return err
	}
	if profile == nil {
		return nil
	}
	profile.Nickname = nickname
	return runtime.profileStore.Save(*profile)
}

func TransportEndpointsForPeerURL(endpoint string, mailboxes []string) transportpkg.TransportPeerEndpoints {
	transportEndpoints := transportpkg.TransportPeerEndpoints{
		Endpoint:         endpoint,
		MailboxEndpoints: append([]string{}, mailboxes...),
	}
	if isOnionPeerURL(endpoint) {
		transportEndpoints.DirectOnion = endpoint
	}
	return transportEndpoints
}
