package meshruntime

import (
	"encoding/json"
	"time"

	arktree "github.com/arkade-os/arkd/pkg/ark-lib/tree"
	"github.com/parkerpoker/parkerd/internal/game"
	"github.com/parkerpoker/parkerd/internal/settlementcore"
	"github.com/parkerpoker/parkerd/internal/tablecustody"
)

type NativePeerAddress struct {
	Alias             string   `json:"alias,omitempty"`
	LastSeenAt        string   `json:"lastSeenAt,omitempty"`
	PeerID            string   `json:"peerId"`
	PeerURL           string   `json:"peerUrl"`
	ProtocolPubkeyHex string   `json:"protocolPubkeyHex,omitempty"`
	RelayPeerID       string   `json:"relayPeerId,omitempty"`
	Roles             []string `json:"roles,omitempty"`
}

type NativeMeshPeerState struct {
	PeerID         string `json:"peerId,omitempty"`
	PeerURL        string `json:"peerUrl,omitempty"`
	ProtocolID     string `json:"protocolId,omitempty"`
	WalletPlayerID string `json:"walletPlayerId,omitempty"`
}

type NativeTableSummary struct {
	CurrentEpoch           int    `json:"currentEpoch"`
	CustodySeq             int    `json:"custodySeq"`
	HandNumber             int    `json:"handNumber"`
	HostPeerID             string `json:"hostPeerId"`
	LatestCustodyStateHash string `json:"latestCustodyStateHash,omitempty"`
	LatestSnapshotID       string `json:"latestSnapshotId,omitempty"`
	Phase                  any    `json:"phase"`
	Role                   string `json:"role"`
	Status                 string `json:"status"`
	TableID                string `json:"tableId"`
	TableName              string `json:"tableName"`
	Visibility             string `json:"visibility"`
}

type NativeCreatedTableEntry struct {
	Config     NativeMeshTableConfig `json:"config"`
	InviteCode string                `json:"inviteCode,omitempty"`
	Summary    NativeTableSummary    `json:"summary"`
}

type NativeCreatedTablesPage struct {
	Items      []NativeCreatedTableEntry `json:"items"`
	NextCursor string                    `json:"nextCursor,omitempty"`
}

type NativeMeshRuntimeState struct {
	FundsWarnings []map[string]any      `json:"fundsWarnings"`
	Mode          string                `json:"mode"`
	Peer          NativeMeshPeerState   `json:"peer"`
	Peers         []NativePeerAddress   `json:"peers"`
	PublicTables  []NativeAdvertisement `json:"publicTables"`
	Tables        []NativeTableSummary  `json:"tables"`
}

type NativeMeshTableConfig struct {
	BigBlindSats              int    `json:"bigBlindSats"`
	BuyInMaxSats              int    `json:"buyInMaxSats"`
	BuyInMinSats              int    `json:"buyInMinSats"`
	CreatedAt                 string `json:"createdAt"`
	DealerMode                string `json:"dealerMode"`
	HostPeerID                string `json:"hostPeerId"`
	HostPlaysAllowed          bool   `json:"hostPlaysAllowed"`
	Name                      string `json:"name"`
	NetworkID                 string `json:"networkId"`
	OccupiedSeats             int    `json:"occupiedSeats"`
	PublicSpectatorDelayHands int    `json:"publicSpectatorDelayHands"`
	SeatCount                 int    `json:"seatCount"`
	SmallBlindSats            int    `json:"smallBlindSats"`
	SpectatorsAllowed         bool   `json:"spectatorsAllowed"`
	Status                    string `json:"status"`
	TableID                   string `json:"tableId"`
	Visibility                string `json:"visibility"`
}

type NativeSeatedPlayer struct {
	ArkAddress        string                 `json:"arkAddress"`
	BuyInSats         int                    `json:"buyInSats"`
	FundingRefs       []tablecustody.VTXORef `json:"fundingRefs,omitempty"`
	Nickname          string                 `json:"nickname"`
	PeerID            string                 `json:"peerId"`
	PlayerID          string                 `json:"playerId"`
	ProtocolPubkeyHex string                 `json:"protocolPubkeyHex"`
	SeatIndex         int                    `json:"seatIndex"`
	Status            string                 `json:"status"`
	WalletPubkeyHex   string                 `json:"walletPubkeyHex"`
}

type NativeDealerCommitment struct {
	CommittedAt string `json:"committedAt"`
	Mode        string `json:"mode"`
	RootHash    string `json:"rootHash"`
}

type NativePublicTableState struct {
	ActingSeatIndex      any                     `json:"actingSeatIndex"`
	Board                []string                `json:"board"`
	ChipBalances         map[string]int          `json:"chipBalances"`
	CurrentBetSats       int                     `json:"currentBetSats"`
	DealerCommitment     *NativeDealerCommitment `json:"dealerCommitment"`
	DealerSeatIndex      any                     `json:"dealerSeatIndex"`
	Epoch                int                     `json:"epoch"`
	FoldedPlayerIDs      []string                `json:"foldedPlayerIds"`
	HandID               any                     `json:"handId"`
	HandNumber           int                     `json:"handNumber"`
	LatestEventHash      any                     `json:"latestEventHash"`
	LivePlayerIDs        []string                `json:"livePlayerIds"`
	MinRaiseToSats       int                     `json:"minRaiseToSats"`
	Phase                any                     `json:"phase"`
	PotSats              int                     `json:"potSats"`
	PreviousSnapshotHash any                     `json:"previousSnapshotHash"`
	RoundContributions   map[string]int          `json:"roundContributions"`
	SeatedPlayers        []NativeSeatedPlayer    `json:"seatedPlayers"`
	SnapshotID           string                  `json:"snapshotId"`
	Status               string                  `json:"status"`
	TableID              string                  `json:"tableId"`
	TotalContributions   map[string]int          `json:"totalContributions"`
	UpdatedAt            string                  `json:"updatedAt"`
}

type NativeTableSnapshotSignature struct {
	SignatureHex    string `json:"signatureHex"`
	SignedAt        string `json:"signedAt"`
	SignerPeerID    string `json:"signerPeerId"`
	SignerPubkeyHex string `json:"signerPubkeyHex"`
	SignerRole      string `json:"signerRole"`
}

type NativeCooperativeTableSnapshot struct {
	CreatedAt            string                         `json:"createdAt"`
	ChipBalances         map[string]int                 `json:"chipBalances"`
	DealerCommitmentRoot any                            `json:"dealerCommitmentRoot"`
	Epoch                int                            `json:"epoch"`
	FoldedPlayerIDs      []string                       `json:"foldedPlayerIds"`
	HandID               any                            `json:"handId"`
	HandNumber           int                            `json:"handNumber"`
	LatestEventHash      any                            `json:"latestEventHash"`
	LivePlayerIDs        []string                       `json:"livePlayerIds"`
	Phase                any                            `json:"phase"`
	PotSats              int                            `json:"potSats"`
	PreviousSnapshotHash any                            `json:"previousSnapshotHash"`
	SeatedPlayers        []NativeSeatedPlayer           `json:"seatedPlayers"`
	SidePots             []int                          `json:"sidePots"`
	Signatures           []NativeTableSnapshotSignature `json:"signatures"`
	SnapshotID           string                         `json:"snapshotId"`
	TableID              string                         `json:"tableId"`
	TurnIndex            any                            `json:"turnIndex"`
}

type NativeAdvertisement struct {
	AdExpiresAt           string         `json:"adExpiresAt"`
	BuyInMaxSats          int            `json:"buyInMaxSats"`
	BuyInMinSats          int            `json:"buyInMinSats"`
	Currency              string         `json:"currency"`
	GeographicHint        string         `json:"geographicHint,omitempty"`
	HostModeCapabilities  []string       `json:"hostModeCapabilities"`
	HostPeerID            string         `json:"hostPeerId"`
	HostPeerURL           string         `json:"hostPeerUrl,omitempty"`
	HostProtocolPubkeyHex string         `json:"hostProtocolPubkeyHex"`
	HostSignatureHex      string         `json:"hostSignatureHex"`
	LatencyHintMS         int            `json:"latencyHintMs,omitempty"`
	NetworkID             string         `json:"networkId"`
	OccupiedSeats         int            `json:"occupiedSeats"`
	ProtocolVersion       string         `json:"protocolVersion"`
	SeatCount             int            `json:"seatCount"`
	SpectatorsAllowed     bool           `json:"spectatorsAllowed"`
	Stakes                map[string]int `json:"stakes"`
	TableID               string         `json:"tableId"`
	TableName             string         `json:"tableName"`
	Visibility            string         `json:"visibility"`
	WitnessCount          int            `json:"witnessCount"`
}

type NativePublicTableView struct {
	Advertisement NativeAdvertisement     `json:"advertisement"`
	LatestState   *NativePublicTableState `json:"latestState"`
	RecentUpdates []map[string]any        `json:"recentUpdates"`
}

type NativeTableLocalView struct {
	CanAct       bool               `json:"canAct"`
	LegalActions []game.LegalAction `json:"legalActions"`
	MyHoleCards  any                `json:"myHoleCards"`
	MyPlayerID   any                `json:"myPlayerId"`
	MySeatIndex  any                `json:"mySeatIndex"`
}

type NativeMeshTableView struct {
	Config                    NativeMeshTableConfig            `json:"config"`
	CustodyTransitions        []tablecustody.CustodyTransition `json:"custodyTransitions,omitempty"`
	Events                    []NativeSignedTableEvent         `json:"events"`
	LatestCustodyState        *tablecustody.CustodyState       `json:"latestCustodyState,omitempty"`
	LatestFullySignedSnapshot *NativeCooperativeTableSnapshot  `json:"latestFullySignedSnapshot"`
	LatestSnapshot            *NativeCooperativeTableSnapshot  `json:"latestSnapshot"`
	Local                     NativeTableLocalView             `json:"local"`
	PublicState               *NativePublicTableState          `json:"publicState"`
}

type NativeSignedTableEvent struct {
	Body                    map[string]any `json:"body"`
	Epoch                   int            `json:"epoch"`
	HandID                  any            `json:"handId"`
	MessageType             string         `json:"messageType"`
	NetworkID               string         `json:"networkId"`
	PrevEventHash           any            `json:"prevEventHash"`
	ProtocolVersion         string         `json:"protocolVersion"`
	Seq                     int            `json:"seq"`
	SenderPeerID            string         `json:"senderPeerId"`
	SenderProtocolPubkeyHex string         `json:"senderProtocolPubkeyHex"`
	SenderRole              string         `json:"senderRole"`
	Signature               string         `json:"signature"`
	TableID                 string         `json:"tableId"`
	Timestamp               string         `json:"timestamp"`
}

type NativeTableFundsOperation struct {
	AmountSats      int                    `json:"amountSats"`
	CheckpointHash  string                 `json:"checkpointHash,omitempty"`
	CreatedAt       string                 `json:"createdAt"`
	CustodySeq      int                    `json:"custodySeq,omitempty"`
	ArkIntentID     string                 `json:"arkIntentId,omitempty"`
	ArkTxID         string                 `json:"arkTxid,omitempty"`
	ExitProofRef    string                 `json:"exitProofRef,omitempty"`
	Kind            string                 `json:"kind"`
	NetworkID       string                 `json:"networkId"`
	OperationID     string                 `json:"operationId"`
	PlayerID        string                 `json:"playerId"`
	PrevStateHash   string                 `json:"prevStateHash,omitempty"`
	Provider        string                 `json:"provider"`
	SignatureHex    string                 `json:"signatureHex"`
	SignerPubkeyHex string                 `json:"signerPubkeyHex"`
	StateHash       string                 `json:"stateHash,omitempty"`
	Status          string                 `json:"status"`
	TableID         string                 `json:"tableId"`
	VTXORefs        []tablecustody.VTXORef `json:"vtxoRefs,omitempty"`
	VTXOExpiry      string                 `json:"vtxoExpiry,omitempty"`
}

type NativeTableFundsEntry struct {
	BuyInSats           int                         `json:"buyInSats"`
	CashoutSats         int                         `json:"cashoutSats,omitempty"`
	CheckpointHash      string                      `json:"checkpointHash,omitempty"`
	LastUpdatedAt       string                      `json:"lastUpdatedAt"`
	Operations          []NativeTableFundsOperation `json:"operations"`
	PlayerID            string                      `json:"playerId"`
	ReservedFundingRefs []tablecustody.VTXORef      `json:"reservedFundingRefs,omitempty"`
	Status              string                      `json:"status"`
	TableID             string                      `json:"tableId"`
}

type NativeTableFundsState struct {
	NetworkID string                           `json:"networkId"`
	Profile   string                           `json:"profile"`
	Tables    map[string]NativeTableFundsEntry `json:"tables"`
}

type nativeSeatRecord struct {
	NativeSeatedPlayer
	PeerURL     string `json:"peerUrl,omitempty"`
	ProfileName string `json:"profileName"`
}

type nativeKnownParticipant struct {
	ProfileName string            `json:"profileName"`
	Peer        NativePeerAddress `json:"peer"`
}

type nativeHandCardState struct {
	FinalDeck       []string            `json:"finalDeck,omitempty"`
	PhaseDeadlineAt string              `json:"phaseDeadlineAt,omitempty"`
	Transcript      game.HandTranscript `json:"transcript"`
}

type nativeActiveHand struct {
	Cards nativeHandCardState `json:"cards"`
	State game.HoldemState    `json:"state"`
}

type nativeTableState struct {
	Advertisement             *NativeAdvertisement             `json:"advertisement,omitempty"`
	ActiveHand                *nativeActiveHand                `json:"activeHand,omitempty"`
	Config                    NativeMeshTableConfig            `json:"config"`
	CurrentHost               nativeKnownParticipant           `json:"currentHost"`
	CurrentEpoch              int                              `json:"currentEpoch"`
	CustodyTransitions        []tablecustody.CustodyTransition `json:"custodyTransitions,omitempty"`
	Events                    []NativeSignedTableEvent         `json:"events"`
	HostProfileName           string                           `json:"hostProfileName"`
	InviteCode                string                           `json:"inviteCode"`
	LastEventHash             string                           `json:"lastEventHash,omitempty"`
	LastHostHeartbeatAt       string                           `json:"lastHostHeartbeatAt"`
	LastSyncedAt              string                           `json:"lastSyncedAt"`
	LatestCustodyState        *tablecustody.CustodyState       `json:"latestCustodyState,omitempty"`
	LatestFullySignedSnapshot *NativeCooperativeTableSnapshot  `json:"latestFullySignedSnapshot,omitempty"`
	LatestSnapshot            *NativeCooperativeTableSnapshot  `json:"latestSnapshot,omitempty"`
	NextHandAt                string                           `json:"nextHandAt,omitempty"`
	PublicState               *NativePublicTableState          `json:"publicState,omitempty"`
	Seats                     []nativeSeatRecord               `json:"seats"`
	Snapshots                 []NativeCooperativeTableSnapshot `json:"snapshots"`
	Witnesses                 []nativeKnownParticipant         `json:"witnesses,omitempty"`
}

type nativePeerSelf struct {
	Alias              string            `json:"alias"`
	Mode               string            `json:"mode"`
	Peer               NativePeerAddress `json:"peer"`
	ProfileName        string            `json:"profileName"`
	ProtocolID         string            `json:"protocolId"`
	TransportPubkeyHex string            `json:"transportPubkeyHex,omitempty"`
	WalletPlayerID     string            `json:"walletPlayerId"`
}

type nativeCachedPeerInfo struct {
	FetchedAt time.Time
	PeerSelf  nativePeerSelf
}

type nativeJoinRequest struct {
	FundingRefs      []tablecustody.VTXORef         `json:"fundingRefs,omitempty"`
	FundingTotalSats int                            `json:"fundingTotalSats,omitempty"`
	BuyInSats        int                            `json:"buyInSats"`
	IdentityBinding  settlementcore.IdentityBinding `json:"identityBinding"`
	TableID          string                         `json:"tableId"`
	ProfileName      string                         `json:"profileName"`
	WalletPlayerID   string                         `json:"walletPlayerId"`
	Peer             NativePeerAddress              `json:"peer"`
	ProtocolID       string                         `json:"protocolId"`
	WalletPubkeyHex  string                         `json:"walletPubkeyHex"`
	Nickname         string                         `json:"nickname"`
	ArkAddress       string                         `json:"arkAddress"`
}

type nativeActionRequest struct {
	Action               game.Action `json:"action"`
	DecisionIndex        int         `json:"decisionIndex"`
	Epoch                int         `json:"epoch"`
	HandID               string      `json:"handId"`
	PlayerID             string      `json:"playerId"`
	PrevCustodyStateHash string      `json:"prevCustodyStateHash,omitempty"`
	ProfileName          string      `json:"profileName"`
	SignatureHex         string      `json:"signatureHex"`
	SignedAt             string      `json:"signedAt"`
	TableID              string      `json:"tableId"`
}

type nativeCustodyApprovalRequest struct {
	ExpectedPrevStateHash string                         `json:"expectedPrevStateHash"`
	PlayerID              string                         `json:"playerId"`
	TableID               string                         `json:"tableId"`
	Transition            tablecustody.CustodyTransition `json:"transition"`
}

type nativeCustodyApprovalResponse struct {
	Approval tablecustody.CustodySignature `json:"approval"`
}

type nativeCustodyTxSignRequest struct {
	ExpectedPrevStateHash string                         `json:"expectedPrevStateHash,omitempty"`
	PSBT                  string                         `json:"psbt"`
	PlayerID              string                         `json:"playerId"`
	Purpose               string                         `json:"purpose,omitempty"`
	TableID               string                         `json:"tableId"`
	TransitionHash        string                         `json:"transitionHash,omitempty"`
	Transition            tablecustody.CustodyTransition `json:"transition"`
}

type nativeCustodyTxSignResponse struct {
	SignedPSBT string `json:"signedPsbt"`
}

type nativeCustodySignerPrepareRequest struct {
	DerivationPath        string                         `json:"derivationPath"`
	ExpectedPrevStateHash string                         `json:"expectedPrevStateHash,omitempty"`
	PlayerID              string                         `json:"playerId"`
	TableID               string                         `json:"tableId"`
	TransitionHash        string                         `json:"transitionHash,omitempty"`
	Transition            tablecustody.CustodyTransition `json:"transition"`
}

type nativeCustodySignerPrepareResponse struct {
	SignerPubkeyHex string `json:"signerPubkeyHex"`
}

type nativeCustodySignerStartRequest struct {
	BatchID               string             `json:"batchId"`
	BatchExpiryType       string             `json:"batchExpiryType,omitempty"`
	BatchExpiryValue      uint32             `json:"batchExpiryValue"`
	BatchOutputAmountSats int64              `json:"batchOutputAmountSats"`
	DerivationPath        string             `json:"derivationPath"`
	ExpectedPrevStateHash string             `json:"expectedPrevStateHash,omitempty"`
	PlayerID              string             `json:"playerId"`
	SweepTapTreeRootHex   string             `json:"sweepTapTreeRootHex"`
	TableID               string             `json:"tableId"`
	TransitionHash        string             `json:"transitionHash,omitempty"`
	UnsignedCommitmentTx  string             `json:"unsignedCommitmentTx,omitempty"`
	VtxoTree              arktree.FlatTxTree `json:"vtxoTree"`
}

type nativeCustodySignerNoncesRequest struct {
	BatchID        string                          `json:"batchId"`
	DerivationPath string                          `json:"derivationPath"`
	Nonces         map[string]*arktree.Musig2Nonce `json:"nonces"`
	PlayerID       string                          `json:"playerId"`
	TableID        string                          `json:"tableId"`
	TxID           string                          `json:"txid"`
	TransitionHash string                          `json:"transitionHash,omitempty"`
}

type nativeCustodySignerAggregatedNoncesRequest struct {
	BatchID        string             `json:"batchId"`
	DerivationPath string             `json:"derivationPath"`
	Nonces         arktree.TreeNonces `json:"nonces"`
	PlayerID       string             `json:"playerId"`
	TableID        string             `json:"tableId"`
	TransitionHash string             `json:"transitionHash,omitempty"`
}

type nativeCustodyAckResponse struct {
	OK     bool `json:"ok"`
	Signed bool `json:"signed,omitempty"`
}

type nativeHandMessageRequest struct {
	CardPositions         []int    `json:"cardPositions,omitempty"`
	Cards                 []string `json:"cards,omitempty"`
	CommitmentHash        string   `json:"commitmentHash,omitempty"`
	DeckStage             []string `json:"deckStage,omitempty"`
	DeckStageRoot         string   `json:"deckStageRoot,omitempty"`
	Epoch                 int      `json:"epoch"`
	HandID                string   `json:"handId"`
	HandNumber            int      `json:"handNumber"`
	Kind                  string   `json:"kind"`
	LockPublicExponentHex string   `json:"lockPublicExponentHex,omitempty"`
	PartialCiphertexts    []string `json:"partialCiphertexts,omitempty"`
	Phase                 string   `json:"phase"`
	PlayerID              string   `json:"playerId"`
	ProfileName           string   `json:"profileName"`
	RecipientSeatIndex    *int     `json:"recipientSeatIndex,omitempty"`
	SeatIndex             int      `json:"seatIndex"`
	ShuffleSeedHex        string   `json:"shuffleSeedHex,omitempty"`
	SignatureHex          string   `json:"signatureHex"`
	SignedAt              string   `json:"signedAt"`
	TableID               string   `json:"tableId"`
}

type nativeTableFetchRequest struct {
	PlayerID     string `json:"playerId,omitempty"`
	SignatureHex string `json:"signatureHex,omitempty"`
	SignedAt     string `json:"signedAt,omitempty"`
	TableID      string `json:"tableId"`
}

type nativeTableSyncRequest struct {
	SenderPeerID            string           `json:"senderPeerId"`
	SenderProtocolPubkeyHex string           `json:"senderProtocolPubkeyHex"`
	SentAt                  string           `json:"sentAt"`
	SignatureHex            string           `json:"signatureHex"`
	Table                   nativeTableState `json:"table"`
}

func rawJSONMap(input any) map[string]any {
	raw := MustMarshalJSON(input)
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil || decoded == nil {
		return map[string]any{}
	}
	return decoded
}
