package wallet

import "encoding/json"

type WalletSummary struct {
	AvailableSats   int    `json:"availableSats"`
	TotalSats       int    `json:"totalSats"`
	ArkAddress      string `json:"arkAddress"`
	BoardingAddress string `json:"boardingAddress"`
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
	HostPeerID   string          `json:"hostPeerId"`
	HostPeerURL  string          `json:"hostPeerUrl"`
	Role         string          `json:"role"`
	TableID      string          `json:"tableId"`
	Visibility   string          `json:"visibility"`
}

type PlayerProfileState struct {
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
