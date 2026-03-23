package game

import "fmt"

func nextSeat(seatIndex int) int {
	if seatIndex == 0 {
		return 1
	}
	return 0
}

func copyIntPointer(value *int) *int {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}

func maxInt(left, right int) int {
	if left > right {
		return left
	}
	return right
}

func buildHoleCards(deck []CardCode, dealerSeatIndex int) [2][2]CardCode {
	var seats [2][2]CardCode
	cardCounts := [2]int{}
	dealSeat := dealerSeatIndex
	deckIndex := 0

	for round := 0; round < 2; round++ {
		for seatOffset := 0; seatOffset < 2; seatOffset++ {
			seats[dealSeat][cardCounts[dealSeat]] = deck[deckIndex]
			cardCounts[dealSeat]++
			deckIndex++
			dealSeat = nextSeat(dealSeat)
		}
	}

	return seats
}

func buildRunout(deck []CardCode) HoldemRunout {
	return HoldemRunout{
		Flop:  [3]CardCode{deck[5], deck[6], deck[7]},
		Turn:  deck[9],
		River: deck[11],
	}
}

func cloneState(state HoldemState) HoldemState {
	clone := state
	clone.ActingSeatIndex = copyIntPointer(state.ActingSeatIndex)
	clone.RaiseLockedSeatIndex = copyIntPointer(state.RaiseLockedSeatIndex)
	clone.Board = append([]CardCode(nil), state.Board...)
	clone.Winners = append([]HoldemWinner(nil), state.Winners...)
	clone.ActionLog = append([]HoldemActionRecord(nil), state.ActionLog...)
	clone.ShowdownScores = map[string]HandScore{}
	for key, value := range state.ShowdownScores {
		clone.ShowdownScores[key] = value
	}
	return clone
}

func recomputePot(players [2]HoldemPlayerState) int {
	total := 0
	for _, player := range players {
		total += player.TotalContributionSats
	}
	return total
}

func getActivePlayers(state HoldemState) []HoldemPlayerState {
	players := make([]HoldemPlayerState, 0, len(state.Players))
	for _, player := range state.Players {
		if player.Status != PlayerStatusFolded {
			players = append(players, player)
		}
	}
	return players
}

func getPlayersAbleToAct(state HoldemState) []HoldemPlayerState {
	players := make([]HoldemPlayerState, 0, len(state.Players))
	for _, player := range state.Players {
		if player.Status == PlayerStatusActive {
			players = append(players, player)
		}
	}
	return players
}

func firstToActForStreet(state HoldemState) int {
	return nextSeat(state.DealerSeatIndex)
}

func awardPot(state *HoldemState, winners []HoldemWinner) {
	baseShare := 0
	if len(winners) > 0 {
		baseShare = state.PotSats / len(winners)
	}
	remainder := 0
	if len(winners) > 0 {
		remainder = state.PotSats % len(winners)
	}

	for index := range winners {
		seat := &state.Players[winners[index].SeatIndex]
		share := baseShare
		if remainder > 0 {
			share++
			remainder--
		}
		seat.StackSats += share
		winners[index].AmountSats = share
	}

	state.Winners = winners
	state.Phase = StreetSettled
	state.ActingSeatIndex = nil
}

func settleByFold(state *HoldemState) error {
	remaining := getActivePlayers(*state)
	if len(remaining) != 1 {
		return fmtErrorf("fold settlement requires exactly one remaining player")
	}

	var folded *HoldemPlayerState
	for index := range state.Players {
		if state.Players[index].Status == PlayerStatusFolded {
			folded = &state.Players[index]
			break
		}
	}

	if folded != nil && remaining[0].TotalContributionSats > folded.TotalContributionSats {
		unmatched := remaining[0].TotalContributionSats - folded.TotalContributionSats
		seat := &state.Players[remaining[0].SeatIndex]
		seat.StackSats += unmatched
		seat.TotalContributionSats -= unmatched
		seat.RoundContributionSats = minInt(seat.RoundContributionSats, folded.TotalContributionSats)
		state.PotSats = recomputePot(state.Players)
	}

	awardPot(state, []HoldemWinner{{
		PlayerID:  remaining[0].PlayerID,
		SeatIndex: remaining[0].SeatIndex,
	}})
	return nil
}

func settleShowdown(state *HoldemState) error {
	contenders := getActivePlayers(*state)
	if len(contenders) == 0 {
		return fmtErrorf("showdown requires at least one contender")
	}

	scores := make([]struct {
		player HoldemPlayerState
		score  HandScore
	}, 0, len(contenders))
	for _, player := range contenders {
		cards := append([]CardCode(nil), player.HoleCards[:]...)
		cards = append(cards, state.Board...)
		score, err := ScoreSevenCardHand(cards)
		if err != nil {
			return err
		}
		state.ShowdownScores[player.PlayerID] = score
		scores = append(scores, struct {
			player HoldemPlayerState
			score  HandScore
		}{player: player, score: score})
	}

	best := scores[0]
	for _, contender := range scores[1:] {
		if CompareScoredHands(contender.score, best.score) > 0 {
			best = contender
		}
	}

	winners := make([]HoldemWinner, 0)
	for _, contender := range scores {
		if CompareScoredHands(contender.score, best.score) == 0 {
			winners = append(winners, HoldemWinner{
				PlayerID:  contender.player.PlayerID,
				SeatIndex: contender.player.SeatIndex,
				HandScore: &contender.score,
			})
		}
	}

	awardPot(state, winners)
	return nil
}

func advanceStreet(state *HoldemState) error {
	for index := range state.Players {
		state.Players[index].RoundContributionSats = 0
		state.Players[index].ActedThisRound = state.Players[index].Status != PlayerStatusActive
	}

	state.CurrentBetSats = 0
	state.LastFullRaiseSats = state.BigBlindSats
	state.MinRaiseToSats = state.BigBlindSats
	state.RaiseLockedSeatIndex = nil

	switch state.Phase {
	case StreetPreflop:
		state.Phase = StreetFlop
		state.Board = append([]CardCode(nil), state.Runout.Flop[:]...)
	case StreetFlop:
		state.Phase = StreetTurn
		state.Board = append(append([]CardCode(nil), state.Board...), state.Runout.Turn)
	case StreetTurn:
		state.Phase = StreetRiver
		state.Board = append(append([]CardCode(nil), state.Board...), state.Runout.River)
	case StreetRiver:
		state.Phase = StreetShowdown
		return settleShowdown(state)
	default:
		return nil
	}

	nextActor := state.Players[firstToActForStreet(*state)]
	if nextActor.Status == PlayerStatusActive {
		seat := nextActor.SeatIndex
		state.ActingSeatIndex = &seat
	} else {
		state.ActingSeatIndex = nil
	}
	return nil
}

func closeActionRoundIfNeeded(state *HoldemState) error {
	if len(getActivePlayers(*state)) == 1 {
		return settleByFold(state)
	}

	ableToAct := getPlayersAbleToAct(*state)
	if len(ableToAct) == 0 {
		switch state.Phase {
		case StreetPreflop:
			state.Board = append([]CardCode(nil), state.Runout.Flop[:]...)
			state.Board = append(state.Board, state.Runout.Turn, state.Runout.River)
		case StreetFlop:
			state.Board = append(append([]CardCode(nil), state.Board...), state.Runout.Turn, state.Runout.River)
		case StreetTurn:
			state.Board = append(append([]CardCode(nil), state.Board...), state.Runout.River)
		}
		state.Phase = StreetShowdown
		return settleShowdown(state)
	}

	roundComplete := true
	for _, player := range ableToAct {
		if !player.ActedThisRound || player.RoundContributionSats != state.CurrentBetSats {
			roundComplete = false
			break
		}
	}
	if roundComplete {
		return advanceStreet(state)
	}

	for _, player := range ableToAct {
		if !player.ActedThisRound || player.RoundContributionSats != state.CurrentBetSats {
			seat := player.SeatIndex
			state.ActingSeatIndex = &seat
			return nil
		}
	}

	state.ActingSeatIndex = nil
	return nil
}

func CreateHoldemHand(config HoldemHandConfig) (HoldemState, error) {
	if config.DealerSeatIndex != 0 && config.DealerSeatIndex != 1 {
		return HoldemState{}, fmtErrorf("dealer seat index must be 0 or 1")
	}
	if config.SmallBlindSats <= 0 || config.BigBlindSats <= 0 {
		return HoldemState{}, fmtErrorf("blinds must be positive")
	}

	deck := CreateDeterministicDeck(config.DeckSeedHex)
	codes := make([]CardCode, len(deck))
	for index, card := range deck {
		codes[index] = card.Code
	}

	holeCards := buildHoleCards(codes, config.DealerSeatIndex)
	runout := buildRunout(codes)
	smallBlindSeat := config.DealerSeatIndex
	bigBlindSeat := nextSeat(config.DealerSeatIndex)

	players := [2]HoldemPlayerState{}
	for seatIndex, seat := range config.Seats {
		players[seatIndex] = HoldemPlayerState{
			PlayerID:  seat.PlayerID,
			SeatIndex: seatIndex,
			StackSats: seat.StackSats,
			Status:    PlayerStatusActive,
			HoleCards: holeCards[seatIndex],
		}
	}

	smallBlindPlayer := &players[smallBlindSeat]
	bigBlindPlayer := &players[bigBlindSeat]
	committedSmallBlind := minInt(smallBlindPlayer.StackSats, config.SmallBlindSats)
	committedBigBlind := minInt(bigBlindPlayer.StackSats, config.BigBlindSats)

	smallBlindPlayer.StackSats -= committedSmallBlind
	smallBlindPlayer.RoundContributionSats = committedSmallBlind
	smallBlindPlayer.TotalContributionSats = committedSmallBlind
	if smallBlindPlayer.StackSats == 0 {
		smallBlindPlayer.Status = PlayerStatusAllIn
	}

	bigBlindPlayer.StackSats -= committedBigBlind
	bigBlindPlayer.RoundContributionSats = committedBigBlind
	bigBlindPlayer.TotalContributionSats = committedBigBlind
	if bigBlindPlayer.StackSats == 0 {
		bigBlindPlayer.Status = PlayerStatusAllIn
	}

	actingSeatIndex := smallBlindSeat
	return HoldemState{
		HandID:            config.HandID,
		HandNumber:        config.HandNumber,
		Phase:             StreetPreflop,
		DealerSeatIndex:   config.DealerSeatIndex,
		ActingSeatIndex:   &actingSeatIndex,
		SmallBlindSats:    config.SmallBlindSats,
		BigBlindSats:      config.BigBlindSats,
		CurrentBetSats:    committedBigBlind,
		MinRaiseToSats:    committedBigBlind + config.BigBlindSats,
		LastFullRaiseSats: config.BigBlindSats,
		PotSats:           committedSmallBlind + committedBigBlind,
		Board:             nil,
		Runout:            runout,
		DeckSeedHex:       config.DeckSeedHex,
		Players:           players,
		Winners:           nil,
		ShowdownScores:    map[string]HandScore{},
		ActionLog:         nil,
	}, nil
}

func GetLegalActions(state HoldemState, seatIndex *int) []LegalAction {
	targetSeat := state.ActingSeatIndex
	if seatIndex != nil {
		targetSeat = seatIndex
	}
	if targetSeat == nil {
		return nil
	}

	seat := *targetSeat
	if seat < 0 || seat > 1 {
		return nil
	}
	player := state.Players[seat]
	if player.Status != PlayerStatusActive {
		return nil
	}

	toCall := maxInt(0, state.CurrentBetSats-player.RoundContributionSats)
	maxTotalSats := player.RoundContributionSats + player.StackSats

	if toCall == 0 {
		actions := []LegalAction{{Type: ActionCheck}}
		if player.StackSats > 0 {
			minTotal := maxInt(state.BigBlindSats, 1)
			maxTotal := maxTotalSats
			actions = append(actions, LegalAction{
				Type:         ActionBet,
				MinTotalSats: &minTotal,
				MaxTotalSats: &maxTotal,
			})
		}
		return actions
	}

	actions := []LegalAction{{Type: ActionFold}, {Type: ActionCall}}
	if player.StackSats > toCall && (state.RaiseLockedSeatIndex == nil || *state.RaiseLockedSeatIndex != seat) {
		minTotal := minInt(state.MinRaiseToSats, maxTotalSats)
		maxTotal := maxTotalSats
		actions = append(actions, LegalAction{
			Type:         ActionRaise,
			MinTotalSats: &minTotal,
			MaxTotalSats: &maxTotal,
		})
	}
	return actions
}

func expectLegal(state HoldemState, seatIndex int, action Action) error {
	legalActions := GetLegalActions(state, &seatIndex)
	var legal *LegalAction
	for index := range legalActions {
		if legalActions[index].Type == action.Type {
			legal = &legalActions[index]
			break
		}
	}
	if legal == nil {
		return fmtErrorf("illegal %s action for seat %d", action.Type, seatIndex)
	}

	switch action.Type {
	case ActionBet, ActionRaise:
		if legal.MinTotalSats != nil && action.TotalSats < *legal.MinTotalSats {
			return fmtErrorf("action total %d is below minimum %d", action.TotalSats, *legal.MinTotalSats)
		}
		if legal.MaxTotalSats != nil && action.TotalSats > *legal.MaxTotalSats {
			return fmtErrorf("action total %d exceeds maximum %d", action.TotalSats, *legal.MaxTotalSats)
		}
	}

	return nil
}

func ApplyHoldemAction(state HoldemState, seatIndex int, action Action) (HoldemState, error) {
	if state.Phase == StreetSettled {
		return HoldemState{}, fmtErrorf("hand already settled")
	}
	if state.ActingSeatIndex == nil || *state.ActingSeatIndex != seatIndex {
		return HoldemState{}, fmtErrorf("seat %d cannot act while seat %v is up", seatIndex, state.ActingSeatIndex)
	}
	if err := expectLegal(state, seatIndex, action); err != nil {
		return HoldemState{}, err
	}

	next := cloneState(state)
	player := &next.Players[seatIndex]
	opponent := &next.Players[nextSeat(seatIndex)]
	next.ActionLog = append(next.ActionLog, HoldemActionRecord{
		ActorPlayerID: player.PlayerID,
		Action:        action,
		Phase:         next.Phase,
	})

	switch action.Type {
	case ActionFold:
		player.Status = PlayerStatusFolded
		player.ActedThisRound = true
	case ActionCheck:
		player.ActedThisRound = true
	case ActionCall:
		toCall := maxInt(0, next.CurrentBetSats-player.RoundContributionSats)
		paid := minInt(toCall, player.StackSats)
		player.StackSats -= paid
		player.RoundContributionSats += paid
		player.TotalContributionSats += paid
		player.ActedThisRound = true
		if player.StackSats == 0 {
			player.Status = PlayerStatusAllIn
		}
	case ActionBet:
		paid := action.TotalSats - player.RoundContributionSats
		player.StackSats -= paid
		player.RoundContributionSats = action.TotalSats
		player.TotalContributionSats += paid
		player.ActedThisRound = true
		next.CurrentBetSats = action.TotalSats
		next.LastFullRaiseSats = action.TotalSats
		next.MinRaiseToSats = action.TotalSats + next.LastFullRaiseSats
		next.RaiseLockedSeatIndex = nil
		opponent.ActedThisRound = false
		if player.StackSats == 0 {
			player.Status = PlayerStatusAllIn
		}
	case ActionRaise:
		paid := action.TotalSats - player.RoundContributionSats
		raiseSize := action.TotalSats - next.CurrentBetSats
		player.StackSats -= paid
		player.RoundContributionSats = action.TotalSats
		player.TotalContributionSats += paid
		player.ActedThisRound = true
		next.CurrentBetSats = action.TotalSats
		if raiseSize >= next.LastFullRaiseSats {
			next.LastFullRaiseSats = raiseSize
			next.RaiseLockedSeatIndex = nil
		} else if player.StackSats == 0 {
			lockedSeat := opponent.SeatIndex
			next.RaiseLockedSeatIndex = &lockedSeat
		}
		next.MinRaiseToSats = next.CurrentBetSats + next.LastFullRaiseSats
		opponent.ActedThisRound = false
		if player.StackSats == 0 {
			player.Status = PlayerStatusAllIn
		}
	}

	next.PotSats = recomputePot(next.Players)
	if err := closeActionRoundIfNeeded(&next); err != nil {
		return HoldemState{}, err
	}
	return next, nil
}

func ToCheckpointShape(state HoldemState) CheckpointShape {
	playerStacks := map[string]int{}
	roundContributions := map[string]int{}
	totalContributions := map[string]int{}
	holeCardsByPlayerID := map[string][]CardCode{}
	for _, player := range state.Players {
		playerStacks[player.PlayerID] = player.StackSats
		roundContributions[player.PlayerID] = player.RoundContributionSats
		totalContributions[player.PlayerID] = player.TotalContributionSats
		holeCardsByPlayerID[player.PlayerID] = append([]CardCode(nil), player.HoleCards[:]...)
	}

	return CheckpointShape{
		Phase:               state.Phase,
		ActingSeatIndex:     copyIntPointer(state.ActingSeatIndex),
		DealerSeatIndex:     state.DealerSeatIndex,
		Board:               append([]CardCode(nil), state.Board...),
		PlayerStacks:        playerStacks,
		RoundContributions:  roundContributions,
		TotalContributions:  totalContributions,
		PotSats:             state.PotSats,
		CurrentBetSats:      state.CurrentBetSats,
		MinRaiseToSats:      state.MinRaiseToSats,
		HoleCardsByPlayerID: holeCardsByPlayerID,
	}
}

func fmtErrorf(format string, args ...any) error {
	return fmt.Errorf(format, args...)
}
