package wallet

import (
	"encoding/json"

	arklib "github.com/arkade-os/arkd/pkg/ark-lib"
	"github.com/arkade-os/arkd/pkg/ark-lib/tree"
	sdktypes "github.com/arkade-os/go-sdk/types"
	"github.com/parkerpoker/parkerd/internal/tablecustody"
)

type WalletSummary struct {
	AvailableSats       int    `json:"availableSats"`
	TotalSats           int    `json:"totalSats"`
	WalletSpendableSats int    `json:"walletSpendableSats"`
	TableLockedSats     int    `json:"tableLockedSats"`
	PendingExitSats     int    `json:"pendingExitSats"`
	ArkAddress          string `json:"arkAddress"`
	BoardingAddress     string `json:"boardingAddress"`
}

type TableSessionState struct {
	InviteCode string `json:"inviteCode"`
	SeatIndex  int    `json:"seatIndex"`
	TableID    string `json:"tableId"`
}

type KnownPeerState struct {
	Alias             string   `json:"alias,omitempty"`
	Capabilities      []string `json:"capabilities,omitempty"`
	DirectEndpoint    string   `json:"directEndpoint,omitempty"`
	Endpoint          string   `json:"endpoint,omitempty"`
	GossipEndpoint    string   `json:"gossipEndpoint,omitempty"`
	LastSeenAt        string   `json:"lastSeenAt,omitempty"`
	MailboxEndpoints  []string `json:"mailboxEndpoints,omitempty"`
	ManifestEpoch     int      `json:"manifestEpoch,omitempty"`
	PeerID            string   `json:"peerId"`
	PeerURL           string   `json:"peerUrl"`
	ProtocolPubkeyHex string   `json:"protocolPubkeyHex,omitempty"`
	RelayPeerID       string   `json:"relayPeerId,omitempty"`
	Roles             []string `json:"roles,omitempty"`
}

type MeshTableReferenceState struct {
	Config       json.RawMessage `json:"config,omitempty"`
	CurrentEpoch int             `json:"currentEpoch"`
	CreatedAt    string          `json:"createdAt,omitempty"`
	CreatedByMe  bool            `json:"createdByMe,omitempty"`
	HostPeerID   string          `json:"hostPeerId"`
	HostPeerURL  string          `json:"hostPeerUrl"`
	InviteCode   string          `json:"inviteCode,omitempty"`
	Role         string          `json:"role"`
	TableID      string          `json:"tableId"`
	Visibility   string          `json:"visibility"`
}

type PlayerProfileState struct {
	CachedArkConfig        *CustodyArkConfig                  `json:"cachedArkConfig,omitempty"`
	CachedOnchainAddresses []string                           `json:"cachedOnchainAddresses,omitempty"`
	CurrentTable           *TableSessionState                 `json:"currentTable,omitempty"`
	CurrentMeshTableID     string                             `json:"currentMeshTableId,omitempty"`
	HandSeeds              map[string]string                  `json:"handSeeds"`
	KnownPeers             []KnownPeerState                   `json:"knownPeers,omitempty"`
	MeshTables             map[string]MeshTableReferenceState `json:"meshTables,omitempty"`
	MockWallet             *WalletSummary                     `json:"mockWallet,omitempty"`
	Nickname               string                             `json:"nickname"`
	PeerPrivateKeyHex      string                             `json:"peerPrivateKeyHex,omitempty"`
	PrivateKeyHex          string                             `json:"privateKeyHex"`
	ProfileName            string                             `json:"profileName"`
	ProtocolPrivateKeyHex  string                             `json:"protocolPrivateKeyHex,omitempty"`
	TransportPrivateKeyHex string                             `json:"transportPrivateKeyHex,omitempty"`
	WalletPrivateKeyHex    string                             `json:"walletPrivateKeyHex,omitempty"`
}

type LocalProfileSummary struct {
	CurrentMeshTableID   string `json:"currentMeshTableId,omitempty"`
	CurrentTableID       string `json:"currentTableId,omitempty"`
	HasPeerIdentity      bool   `json:"hasPeerIdentity"`
	HasProtocolIdentity  bool   `json:"hasProtocolIdentity"`
	HasTransportIdentity bool   `json:"hasTransportIdentity"`
	HasWalletIdentity    bool   `json:"hasWalletIdentity"`
	KnownPeerCount       int    `json:"knownPeerCount"`
	MeshTableCount       int    `json:"meshTableCount"`
	Nickname             string `json:"nickname"`
	ProfileName          string `json:"profileName"`
}

type BootstrapResult struct {
	Identity LocalIdentity      `json:"identity"`
	State    PlayerProfileState `json:"state"`
	Wallet   WalletSummary      `json:"wallet"`
}

type LocalIdentity struct {
	PlayerID      string `json:"playerId"`
	PrivateKeyHex string `json:"privateKeyHex"`
	PublicKeyHex  string `json:"publicKeyHex"`
}

type CustodyFundingBundle struct {
	PlayerID  string                 `json:"playerId"`
	Refs      []tablecustody.VTXORef `json:"refs"`
	TotalSats int                    `json:"totalSats"`
}

type CustodyIntentRequest struct {
	CosignerPubkeys []string               `json:"cosignerPubkeys,omitempty"`
	Notes           []string               `json:"notes,omitempty"`
	Outputs         []sdktypes.Receiver    `json:"outputs,omitempty"`
	Refs            []tablecustody.VTXORef `json:"refs,omitempty"`
}

type CustodyIntentResult struct {
	IntentID string                 `json:"intentId,omitempty"`
	TxID     string                 `json:"txid,omitempty"`
	Refs     []tablecustody.VTXORef `json:"refs,omitempty"`
}

type CustodySignerSession struct {
	DerivationPath string             `json:"derivationPath"`
	PublicKeyHex   string             `json:"publicKeyHex"`
	Session        tree.SignerSession `json:"-"`
}

type CustodyExitResult struct {
	BroadcastTxIDs []string               `json:"broadcastTxIds,omitempty"`
	Pending        bool                   `json:"pending"`
	SourceRefs     []tablecustody.VTXORef `json:"sourceRefs,omitempty"`
	SweepTxID      string                 `json:"sweepTxId,omitempty"`
}

type CustodyRecoveryResult struct {
	BroadcastTxIDs []string `json:"broadcastTxIds,omitempty"`
	RecoveryTxID   string   `json:"recoveryTxid,omitempty"`
}

type CustodyArkConfig struct {
	ArkServerURL          string                  `json:"arkServerUrl"`
	CheckpointTapscript   string                  `json:"checkpointTapscript,omitempty"`
	DustSats              uint64                  `json:"dustSats"`
	ExplorerURL           string                  `json:"explorerUrl,omitempty"`
	ForfeitAddress        string                  `json:"forfeitAddress,omitempty"`
	ForfeitPubkeyHex      string                  `json:"forfeitPubkeyHex"`
	Network               arklib.Network          `json:"network"`
	OffchainInputFeeSats  int                     `json:"offchainInputFeeSats,omitempty"`
	OffchainOutputFeeSats int                     `json:"offchainOutputFeeSats,omitempty"`
	OnchainInputFeeSats   int                     `json:"onchainInputFeeSats,omitempty"`
	OnchainOutputFeeSats  int                     `json:"onchainOutputFeeSats,omitempty"`
	SignerPubkeyHex       string                  `json:"signerPubkeyHex"`
	UnilateralExitDelay   arklib.RelativeLocktime `json:"unilateralExitDelay"`
}
