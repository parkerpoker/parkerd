package game

type CardCode string

type Street string

const (
	StreetCommitment      Street = "commitment"
	StreetReveal          Street = "reveal"
	StreetFinalization    Street = "finalization"
	StreetPrivateDelivery Street = "private-delivery"
	StreetPreflop         Street = "preflop"
	StreetFlopReveal      Street = "flop-reveal"
	StreetFlop            Street = "flop"
	StreetTurnReveal      Street = "turn-reveal"
	StreetTurn            Street = "turn"
	StreetRiverReveal     Street = "river-reveal"
	StreetRiver           Street = "river"
	StreetShowdownReveal  Street = "showdown-reveal"
	StreetSettled         Street = "settled"
	StreetAborted         Street = "aborted"
)

type ActionType string

const (
	ActionFold  ActionType = "fold"
	ActionCheck ActionType = "check"
	ActionCall  ActionType = "call"
	ActionBet   ActionType = "bet"
	ActionRaise ActionType = "raise"
)

type PlayerStatus string

const (
	PlayerStatusActive PlayerStatus = "active"
	PlayerStatusFolded PlayerStatus = "folded"
	PlayerStatusAllIn  PlayerStatus = "all-in"
)

type DeckCommitment struct {
	SeatIndex      int
	PlayerID       string
	CommitmentHash string
	RevealSeed     string
}

type MentalDeckReveal struct {
	PlayerID              string
	SeatIndex             int
	ShuffleSeedHex        string
	LockPublicExponentHex string
}

type MentalKeyPair struct {
	PrivateExponentHex string
	PublicExponentHex  string
}

type MentalDeckReplay struct {
	FinalDeck             []string
	RevealStageRoots      []string
	RevealStageRootBySeat map[int]string
	RevealStagesBySeat    map[int][]string
}

type HandTranscriptRecord struct {
	Index                 int        `json:"index"`
	Kind                  string     `json:"kind"`
	Phase                 string     `json:"phase"`
	PlayerID              string     `json:"playerId,omitempty"`
	SeatIndex             *int       `json:"seatIndex,omitempty"`
	CommitmentHash        string     `json:"commitmentHash,omitempty"`
	ShuffleSeedHex        string     `json:"shuffleSeedHex,omitempty"`
	LockPublicExponentHex string     `json:"lockPublicExponentHex,omitempty"`
	DeckStage             []string   `json:"deckStage,omitempty"`
	DeckStageRoot         string     `json:"deckStageRoot,omitempty"`
	RecipientSeatIndex    *int       `json:"recipientSeatIndex,omitempty"`
	CardPositions         []int      `json:"cardPositions,omitempty"`
	PartialCiphertexts    []string   `json:"partialCiphertexts,omitempty"`
	Cards                 []CardCode `json:"cards,omitempty"`
	Reason                string     `json:"reason,omitempty"`
	StepHash              string     `json:"stepHash"`
	RootHash              string     `json:"rootHash"`
}

type HandTranscript struct {
	HandID     string                 `json:"handId"`
	HandNumber int                    `json:"handNumber"`
	Records    []HandTranscriptRecord `json:"records"`
	RootHash   string                 `json:"rootHash"`
	TableID    string                 `json:"tableId"`
}

type Action struct {
	Type      ActionType
	TotalSats int
}

type LegalAction struct {
	Type         ActionType
	MinTotalSats *int
	MaxTotalSats *int
}

type HoldemSeatConfig struct {
	PlayerID  string
	StackSats int
}

type HoldemPlayerState struct {
	PlayerID              string
	SeatIndex             int
	StackSats             int
	Status                PlayerStatus
	RoundContributionSats int
	TotalContributionSats int
	ActedThisRound        bool
}

type HoldemWinner struct {
	PlayerID   string
	SeatIndex  int
	AmountSats int
	HandScore  *HandScore
}

type HoldemActionRecord struct {
	ActorPlayerID string
	Action        Action
	Phase         Street
}

type HoldemHandConfig struct {
	HandID          string
	HandNumber      int
	DealerSeatIndex int
	SmallBlindSats  int
	BigBlindSats    int
	Seats           []HoldemSeatConfig
}

type HoldemState struct {
	HandID               string
	HandNumber           int
	Phase                Street
	DealerSeatIndex      int
	ActingSeatIndex      *int
	SmallBlindSats       int
	BigBlindSats         int
	CurrentBetSats       int
	MinRaiseToSats       int
	LastFullRaiseSats    int
	RaiseLockedSeatIndex *int
	PotSats              int
	Board                []CardCode
	Players              []HoldemPlayerState
	Winners              []HoldemWinner
	ShowdownScores       map[string]HandScore
	ActionLog            []HoldemActionRecord
}

type HoldemDealPlan struct {
	BoardPositionsByPhase   map[Street][]int
	HoleCardPositionsBySeat map[int][]int
}

type CheckpointShape struct {
	Phase              Street
	ActingSeatIndex    *int
	DealerSeatIndex    int
	Board              []CardCode
	PlayerStacks       map[string]int
	RoundContributions map[string]int
	TotalContributions map[string]int
	PotSats            int
	CurrentBetSats     int
	MinRaiseToSats     int
}
