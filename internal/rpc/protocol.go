package rpc

import "encoding/json"

const (
	HeartbeatIntervalMilliseconds = 5_000
	DefaultRequestTimeoutSeconds  = 60
	DefaultWatchAckTimeoutSeconds = 5
)

var DaemonMethods = []string{
	"bootstrap",
	"meshBootstrapPeer",
	"meshCashOut",
	"meshCreateTable",
	"meshCreatedTables",
	"meshExit",
	"meshGetTable",
	"meshNetworkPeers",
	"meshPublicTables",
	"meshRenew",
	"meshRotateHost",
	"meshSendAction",
	"meshTableAnnounce",
	"meshTableJoin",
	"ping",
	"status",
	"stop",
	"watch",
	"walletDeposit",
	"walletFaucet",
	"walletNsec",
	"walletOffboard",
	"walletOnboard",
	"walletSummary",
	"walletWithdraw",
}

var WatchEvents = []string{"log", "state"}

type RequestEnvelope struct {
	ID     string         `json:"id"`
	Method string         `json:"method"`
	Params map[string]any `json:"params,omitempty"`
	Type   string         `json:"type"`
}

type ResponseEnvelope struct {
	Error  string          `json:"error,omitempty"`
	ID     string          `json:"id"`
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Type   string          `json:"type"`
}

type EventEnvelope struct {
	Event   string          `json:"event"`
	Payload json.RawMessage `json:"payload"`
	Type    string          `json:"type"`
}

type RuntimeState struct {
	Mesh      *MeshRuntimeState `json:"mesh,omitempty"`
	Transport any               `json:"transport,omitempty"`
}

type MeshRuntimeState struct {
	Peer *MeshPeerState `json:"peer,omitempty"`
}

type MeshPeerState struct {
	PeerID     string `json:"peerId,omitempty"`
	PeerURL    string `json:"peerUrl,omitempty"`
	ProtocolID string `json:"protocolId,omitempty"`
}
