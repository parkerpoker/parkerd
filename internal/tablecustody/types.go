package tablecustody

import arktree "github.com/arkade-os/arkd/pkg/ark-lib/tree"

type TimeoutPolicy string

const (
	TimeoutPolicyAutoCheckOrFold TimeoutPolicy = "auto-check-or-fold"
	TimeoutPolicyAutoFold        TimeoutPolicy = "auto-fold"
)

type TransitionKind string

const (
	TransitionKindBuyInLock      TransitionKind = "buy-in-lock"
	TransitionKindBlindPost      TransitionKind = "blind-post"
	TransitionKindAction         TransitionKind = "action"
	TransitionKindTimeout        TransitionKind = "timeout"
	TransitionKindShowdownPayout TransitionKind = "showdown-payout"
	TransitionKindCashOut        TransitionKind = "cash-out"
	TransitionKindEmergencyExit  TransitionKind = "emergency-exit"
	TransitionKindCarryForward   TransitionKind = "carry-forward"
)

type VTXORef struct {
	AmountSats    int      `json:"amountSats"`
	ArkIntentID   string   `json:"arkIntentId,omitempty"`
	ArkTxID       string   `json:"arkTxid,omitempty"`
	ExpiresAt     string   `json:"expiresAt,omitempty"`
	OwnerPlayerID string   `json:"ownerPlayerId,omitempty"`
	Script        string   `json:"script,omitempty"`
	Tapscripts    []string `json:"tapscripts,omitempty"`
	TxID          string   `json:"txid"`
	VOut          uint32   `json:"vout"`
}

type StackClaim struct {
	AllIn                 bool      `json:"allIn,omitempty"`
	AmountSats            int       `json:"amountSats"`
	Folded                bool      `json:"folded,omitempty"`
	PlayerID              string    `json:"playerId"`
	ReservedFeeSats       int       `json:"reservedFeeSats,omitempty"`
	RoundContributionSats int       `json:"roundContributionSats,omitempty"`
	SeatIndex             int       `json:"seatIndex"`
	Status                string    `json:"status,omitempty"`
	TotalContributionSats int       `json:"totalContributionSats,omitempty"`
	VTXORefs              []VTXORef `json:"vtxoRefs,omitempty"`
}

type PotSlice struct {
	CapSats              int            `json:"capSats"`
	ContributedPlayerIDs []string       `json:"contributedPlayerIds,omitempty"`
	Contributions        map[string]int `json:"contributions"`
	EligiblePlayerIDs    []string       `json:"eligiblePlayerIds"`
	OddChipPlayerIDs     []string       `json:"oddChipPlayerIds,omitempty"`
	PotID                string         `json:"potId"`
	Sequence             int            `json:"sequence"`
	Status               string         `json:"status,omitempty"`
	TotalSats            int            `json:"totalSats"`
	VTXORefs             []VTXORef      `json:"vtxoRefs,omitempty"`
	WinnerPlayerIDs      []string       `json:"winnerPlayerIds,omitempty"`
}

type TimeoutResolution struct {
	ActionType               string        `json:"actionType,omitempty"`
	ActingPlayerID           string        `json:"actingPlayerId"`
	DeadPlayerIDs            []string      `json:"deadPlayerIds,omitempty"`
	LostEligibilityPlayerIDs []string      `json:"lostEligibilityPlayerIds,omitempty"`
	Policy                   TimeoutPolicy `json:"policy"`
	Reason                   string        `json:"reason,omitempty"`
}

type CustodySignature struct {
	ApprovalHash    string `json:"approvalHash,omitempty"`
	PlayerID        string `json:"playerId"`
	SignatureHex    string `json:"signatureHex"`
	SignedAt        string `json:"signedAt"`
	WalletPubkeyHex string `json:"walletPubkeyHex,omitempty"`
}

type CustodySettlementWitness struct {
	ArkIntentID      string             `json:"arkIntentId,omitempty"`
	ArkTxID          string             `json:"arkTxid,omitempty"`
	FinalizedAt      string             `json:"finalizedAt,omitempty"`
	ProofPSBT        string             `json:"proofPsbt,omitempty"`
	CommitmentTx     string             `json:"commitmentTx,omitempty"`
	BatchExpiryType  string             `json:"batchExpiryType,omitempty"`
	BatchExpiryValue uint32             `json:"batchExpiryValue,omitempty"`
	VtxoTree         arktree.FlatTxTree `json:"vtxoTree,omitempty"`
	ConnectorTree    arktree.FlatTxTree `json:"connectorTree,omitempty"`
}

type CustodyRecoveryOutput struct {
	AmountSats    int      `json:"amountSats"`
	OwnerPlayerID string   `json:"ownerPlayerId,omitempty"`
	Script        string   `json:"script,omitempty"`
	Tapscripts    []string `json:"tapscripts,omitempty"`
}

type CustodyRecoveryBundle struct {
	AuthorizedOutputs []CustodyRecoveryOutput `json:"authorizedOutputs,omitempty"`
	BundleHash        string                  `json:"bundleHash,omitempty"`
	EarliestExecuteAt string                  `json:"earliestExecuteAt,omitempty"`
	Kind              TransitionKind          `json:"kind"`
	SignedPSBT        string                  `json:"signedPsbt,omitempty"`
	SourcePotRefs     []VTXORef               `json:"sourcePotRefs,omitempty"`
	TimeoutResolution *TimeoutResolution      `json:"timeoutResolution,omitempty"`
}

type CustodyRecoveryWitness struct {
	BroadcastTxIDs      []string `json:"broadcastTxIds,omitempty"`
	BundleHash          string   `json:"bundleHash,omitempty"`
	ExecutedAt          string   `json:"executedAt,omitempty"`
	RecoveryTxID        string   `json:"recoveryTxid,omitempty"`
	SourceTransitionHash string  `json:"sourceTransitionHash,omitempty"`
}

type CustodyProof struct {
	ArkIntentID       string                    `json:"arkIntentId,omitempty"`
	ArkTxID           string                    `json:"arkTxid,omitempty"`
	ExitProofRef      string                    `json:"exitProofRef,omitempty"`
	FinalizedAt       string                    `json:"finalizedAt,omitempty"`
	RequestHash       string                    `json:"requestHash,omitempty"`
	RecoveryBundles   []CustodyRecoveryBundle   `json:"recoveryBundles,omitempty"`
	RecoveryWitness   *CustodyRecoveryWitness   `json:"recoveryWitness,omitempty"`
	ReplayValidated   bool                      `json:"replayValidated"`
	SettlementWitness *CustodySettlementWitness `json:"settlementWitness,omitempty"`
	Signatures        []CustodySignature        `json:"signatures,omitempty"`
	StateHash         string                    `json:"stateHash"`
	TransitionHash    string                    `json:"transitionHash"`
	VTXORefs          []VTXORef                 `json:"vtxoRefs,omitempty"`
}

type CustodyState struct {
	ActionDeadlineAt string        `json:"actionDeadlineAt,omitempty"`
	ActingPlayerID   string        `json:"actingPlayerId,omitempty"`
	ChallengeAnchor  string        `json:"challengeAnchor,omitempty"`
	CreatedAt        string        `json:"createdAt,omitempty"`
	CustodySeq       int           `json:"custodySeq"`
	DecisionIndex    int           `json:"decisionIndex"`
	Epoch            int           `json:"epoch"`
	HandID           string        `json:"handId,omitempty"`
	HandNumber       int           `json:"handNumber"`
	LegalActionsHash string        `json:"legalActionsHash,omitempty"`
	PotSlices        []PotSlice    `json:"potSlices"`
	PrevStateHash    string        `json:"prevStateHash,omitempty"`
	PublicStateHash  string        `json:"publicStateHash,omitempty"`
	StackClaims      []StackClaim  `json:"stackClaims"`
	StateHash        string        `json:"stateHash"`
	TableID          string        `json:"tableId"`
	TimeoutPolicy    TimeoutPolicy `json:"timeoutPolicy"`
	TranscriptRoot   string        `json:"transcriptRoot,omitempty"`
}

type ActionDescriptor struct {
	TotalSats int    `json:"totalSats,omitempty"`
	Type      string `json:"type,omitempty"`
}

type CustodyTransition struct {
	Action            *ActionDescriptor  `json:"action,omitempty"`
	ActingPlayerID    string             `json:"actingPlayerId,omitempty"`
	Approvals         []CustodySignature `json:"approvals,omitempty"`
	ArkIntentID       string             `json:"arkIntentId,omitempty"`
	ArkTxID           string             `json:"arkTxid,omitempty"`
	CustodySeq        int                `json:"custodySeq"`
	DecisionIndex     int                `json:"decisionIndex"`
	Kind              TransitionKind     `json:"kind"`
	NextState         CustodyState       `json:"nextState"`
	NextStateHash     string             `json:"nextStateHash"`
	PrevStateHash     string             `json:"prevStateHash,omitempty"`
	Proof             CustodyProof       `json:"proof"`
	ProposedAt        string             `json:"proposedAt,omitempty"`
	ProposedBy        string             `json:"proposedBy,omitempty"`
	TableID           string             `json:"tableId"`
	TimeoutResolution *TimeoutResolution `json:"timeoutResolution,omitempty"`
	TransitionID      string             `json:"transitionId,omitempty"`
}

type StateBinding struct {
	ActionDeadlineAt string        `json:"actionDeadlineAt,omitempty"`
	ActingPlayerID   string        `json:"actingPlayerId,omitempty"`
	ChallengeAnchor  string        `json:"challengeAnchor,omitempty"`
	CreatedAt        string        `json:"createdAt,omitempty"`
	DecisionIndex    int           `json:"decisionIndex"`
	Epoch            int           `json:"epoch"`
	HandID           string        `json:"handId,omitempty"`
	HandNumber       int           `json:"handNumber"`
	LegalActionsHash string        `json:"legalActionsHash,omitempty"`
	PublicStateHash  string        `json:"publicStateHash,omitempty"`
	TableID          string        `json:"tableId"`
	TimeoutPolicy    TimeoutPolicy `json:"timeoutPolicy"`
	TranscriptRoot   string        `json:"transcriptRoot,omitempty"`
}

type PlayerBalance struct {
	AllIn                 bool      `json:"allIn,omitempty"`
	Folded                bool      `json:"folded,omitempty"`
	PlayerID              string    `json:"playerId"`
	ReservedFeeSats       int       `json:"reservedFeeSats,omitempty"`
	RoundContributionSats int       `json:"roundContributionSats,omitempty"`
	SeatIndex             int       `json:"seatIndex"`
	StackSats             int       `json:"stackSats"`
	Status                string    `json:"status,omitempty"`
	TotalContributionSats int       `json:"totalContributionSats,omitempty"`
	VTXORefs              []VTXORef `json:"vtxoRefs,omitempty"`
}

type SliceParticipant struct {
	ContributionSats int    `json:"contributionSats"`
	Folded           bool   `json:"folded,omitempty"`
	PlayerID         string `json:"playerId"`
	SeatIndex        int    `json:"seatIndex"`
}

type PayoutCandidate struct {
	PlayerID  string `json:"playerId"`
	SeatIndex int    `json:"seatIndex"`
}
