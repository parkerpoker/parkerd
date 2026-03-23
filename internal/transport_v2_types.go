package parker

type TransportV2PeerEndpoints struct {
	DirectOnion      string   `json:"directOnion,omitempty"`
	Endpoint         string   `json:"endpoint,omitempty"`
	GossipOnion      string   `json:"gossipOnion,omitempty"`
	MailboxEndpoints []string `json:"mailboxEndpoints,omitempty"`
}

type TransportV2PeerManifest struct {
	Capabilities         []string                 `json:"capabilities,omitempty"`
	CreatedAt            string                   `json:"createdAt"`
	DirectOnion          string                   `json:"directOnion,omitempty"`
	Endpoint             string                   `json:"endpoint,omitempty"`
	ExpiresAt            string                   `json:"expiresAt"`
	GossipOnion          string                   `json:"gossipOnion,omitempty"`
	MailboxEndpoints     []string                 `json:"mailboxEndpoints,omitempty"`
	ManifestEpoch        int                      `json:"manifestEpoch"`
	PeerID               string                   `json:"peerId"`
	ProtocolID           string                   `json:"protocolId"`
	Signature            string                   `json:"signature"`
	SignatureKeyID       string                   `json:"signatureKeyId"`
	SigningKey           string                   `json:"signingKey"`
	TransportEncKey      string                   `json:"transportEncKey"`
	TransportEndpoints   TransportV2PeerEndpoints `json:"transportEndpoints"`
	TransportWireVersion int                      `json:"transportWireVersion"`
}

type TransportV2PeerSummary struct {
	Alias              string                   `json:"alias,omitempty"`
	Capabilities       []string                 `json:"capabilities,omitempty"`
	DirectOnion        string                   `json:"directOnion,omitempty"`
	Endpoint           string                   `json:"endpoint,omitempty"`
	GossipOnion        string                   `json:"gossipOnion,omitempty"`
	LastSeenAt         string                   `json:"lastSeenAt,omitempty"`
	MailboxEndpoints   []string                 `json:"mailboxEndpoints,omitempty"`
	ManifestEpoch      int                      `json:"manifestEpoch,omitempty"`
	PeerID             string                   `json:"peerId"`
	ProtocolID         string                   `json:"protocolId,omitempty"`
	Roles              []string                 `json:"roles,omitempty"`
	TransportEndpoints TransportV2PeerEndpoints `json:"transportEndpoints"`
}

type TransportV2LocalPeerState struct {
	DirectOnion          string `json:"directOnion,omitempty"`
	Endpoint             string `json:"endpoint,omitempty"`
	GossipOnion          string `json:"gossipOnion,omitempty"`
	PeerID               string `json:"peerId,omitempty"`
	ProtocolID           string `json:"protocolId,omitempty"`
	TransportKeyID       string `json:"transportKeyId,omitempty"`
	TransportWireVersion int    `json:"transportWireVersion"`
	WalletPlayerID       string `json:"walletPlayerId,omitempty"`
}

type TransportV2QueueState struct {
	DeadLetter int `json:"deadLetter"`
	Dedupe     int `json:"dedupe"`
	Inbox      int `json:"inbox"`
	Outbox     int `json:"outbox"`
}

type TransportV2RuntimeState struct {
	BootstrapPeers       []string                  `json:"bootstrapPeers,omitempty"`
	Mailboxes            []string                  `json:"mailboxes,omitempty"`
	Mode                 string                    `json:"mode"`
	Peer                 TransportV2LocalPeerState `json:"peer"`
	Peers                []TransportV2PeerSummary  `json:"peers"`
	Queues               TransportV2QueueState     `json:"queues"`
	TransportMode        string                    `json:"transportMode"`
	TransportWireVersion int                       `json:"transportWireVersion"`
}

type TransportV2Envelope struct {
	Attempt              int      `json:"attempt"`
	BodyCiphertext       string   `json:"bodyCiphertext"`
	BodyHash             string   `json:"bodyHash"`
	CausalRefs           []string `json:"causalRefs,omitempty"`
	Channel              string   `json:"channel"`
	CreatedAt            string   `json:"createdAt"`
	DedupeKey            string   `json:"dedupeKey"`
	EncryptionEphemeral  string   `json:"encryptionEphemeralPub,omitempty"`
	EncryptionKeyID      string   `json:"encryptionKeyId,omitempty"`
	EncryptionMode       string   `json:"encryptionMode"`
	ExpiresAt            string   `json:"expiresAt"`
	MessageID            string   `json:"messageId"`
	MessageType          string   `json:"messageType"`
	Nonce                string   `json:"nonce"`
	RecipientID          string   `json:"recipientPeerId,omitempty"`
	RetryOf              string   `json:"retryOf,omitempty"`
	SenderPeerID         string   `json:"senderPeerId"`
	Signature            string   `json:"signature"`
	SignatureKeyID       string   `json:"signatureKeyId"`
	TableID              string   `json:"tableId,omitempty"`
	TransportWireVersion int      `json:"transportWireVersion"`
}

type TransportV2DedupeRecord struct {
	AcceptedAt   string `json:"acceptedAt"`
	DedupeKey    string `json:"dedupeKey"`
	MessageID    string `json:"messageId"`
	SenderPeerID string `json:"senderPeerId"`
}

type TransportV2DeadLetter struct {
	CreatedAt string              `json:"createdAt"`
	Envelope  TransportV2Envelope `json:"envelope"`
	Reason    string              `json:"reason"`
}
