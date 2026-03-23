package game

type CardCode string

type Street string

const (
	StreetPreflop  Street = "preflop"
	StreetFlop     Street = "flop"
	StreetTurn     Street = "turn"
	StreetRiver    Street = "river"
	StreetShowdown Street = "showdown"
	StreetSettled  Street = "settled"
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
	HoleCards             [2]CardCode
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

type HoldemRunout struct {
	Flop  [3]CardCode
	Turn  CardCode
	River CardCode
}

type HoldemActionRecord struct {
	ActorPlayerID string
	Action        Action
	Phase         Street
}

type HoldemHandConfig struct {
	HandID          string
	HandNumber      int
	DeckSeedHex     string
	DealerSeatIndex int
	SmallBlindSats  int
	BigBlindSats    int
	Seats           [2]HoldemSeatConfig
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
	Runout               HoldemRunout
	DeckSeedHex          string
	Players              [2]HoldemPlayerState
	Winners              []HoldemWinner
	ShowdownScores       map[string]HandScore
	ActionLog            []HoldemActionRecord
}

type CheckpointShape struct {
	Phase               Street
	ActingSeatIndex     *int
	DealerSeatIndex     int
	Board               []CardCode
	PlayerStacks        map[string]int
	RoundContributions  map[string]int
	TotalContributions  map[string]int
	PotSats             int
	CurrentBetSats      int
	MinRaiseToSats      int
	HoleCardsByPlayerID map[string][]CardCode
}
