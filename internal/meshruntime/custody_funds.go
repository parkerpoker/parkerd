package meshruntime

import (
	"errors"

	"github.com/parkerpoker/parkerd/internal/game"
	"github.com/parkerpoker/parkerd/internal/tablecustody"
)

func tableHasLiveHand(table nativeTableState) bool {
	return table.ActiveHand != nil && table.ActiveHand.State.Phase != game.StreetSettled
}

func (runtime *meshRuntime) buildFundsCustodyTransitionForPlayer(table nativeTableState, playerID string, kind tablecustody.TransitionKind, finalStatus string) (tablecustody.CustodyTransition, error) {
	if table.LatestCustodyState == nil {
		return tablecustody.CustodyTransition{}, errors.New("latest custody state is unavailable")
	}
	binding := runtime.custodyStateBinding(table, nil)
	binding.ActingPlayerID = playerID

	balances := runtime.custodyBalancesFromHand(table, nil)
	foundLocal := false
	for index := range balances {
		if balances[index].PlayerID != playerID {
			continue
		}
		balances[index].StackSats = 0
		balances[index].ReservedFeeSats = 0
		balances[index].Status = finalStatus
		balances[index].VTXORefs = nil
		foundLocal = true
	}
	if !foundLocal {
		return tablecustody.CustodyTransition{}, errors.New("latest custody state is missing the target stack claim")
	}
	return tablecustody.BuildTransition(kind, binding, balances, table.LatestCustodyState, nil, nil)
}

func (runtime *meshRuntime) buildFundsCustodyTransition(table nativeTableState, kind tablecustody.TransitionKind, finalStatus string) (tablecustody.CustodyTransition, error) {
	return runtime.buildFundsCustodyTransitionForPlayer(table, runtime.walletID.PlayerID, kind, finalStatus)
}

func (runtime *meshRuntime) publicStateFromLatestCustody(table nativeTableState, status string) *NativePublicTableState {
	if table.LatestCustodyState == nil {
		return nil
	}
	handID := table.LatestCustodyState.HandID
	handNumber := table.LatestCustodyState.HandNumber
	if status != "active" {
		handID = ""
		handNumber = 0
	}
	chipBalances := map[string]int{}
	livePlayerIDs := make([]string, 0, len(table.Seats))
	seatedPlayers := make([]NativeSeatedPlayer, 0, len(table.Seats))
	roundContributions := map[string]int{}
	totalContributions := map[string]int{}
	for _, seat := range table.Seats {
		seated := seat.NativeSeatedPlayer
		chipBalances[seat.PlayerID] = 0
		roundContributions[seat.PlayerID] = 0
		totalContributions[seat.PlayerID] = 0
		for _, claim := range table.LatestCustodyState.StackClaims {
			if claim.PlayerID != seat.PlayerID {
				continue
			}
			chipBalances[seat.PlayerID] = claim.AmountSats
			roundContributions[seat.PlayerID] = claim.RoundContributionSats
			totalContributions[seat.PlayerID] = claim.TotalContributionSats
			seated.Status = firstNonEmptyString(claim.Status, seat.Status)
			if claim.AmountSats > 0 && seated.Status == "active" {
				livePlayerIDs = append(livePlayerIDs, seat.PlayerID)
			}
			break
		}
		seatedPlayers = append(seatedPlayers, seated)
	}
	return &NativePublicTableState{
		ActingSeatIndex:      nil,
		Board:                []string{},
		ChipBalances:         chipBalances,
		CurrentBetSats:       0,
		DealerCommitment:     nil,
		DealerSeatIndex:      nil,
		Epoch:                table.CurrentEpoch,
		FoldedPlayerIDs:      []string{},
		HandID:               handID,
		HandNumber:           handNumber,
		LatestEventHash:      table.LastEventHash,
		LivePlayerIDs:        livePlayerIDs,
		MinRaiseToSats:       table.Config.BigBlindSats,
		Phase:                nil,
		PotSats:              0,
		PreviousSnapshotHash: nil,
		RoundContributions:   roundContributions,
		SeatedPlayers:        seatedPlayers,
		SnapshotID:           randomUUID(),
		Status:               status,
		TableID:              table.Config.TableID,
		TotalContributions:   totalContributions,
		UpdatedAt:            nowISO(),
	}
}

func activeCustodySeatCount(table nativeTableState) int {
	if table.LatestCustodyState == nil {
		return len(table.Seats)
	}
	count := 0
	for _, seat := range table.Seats {
		if seat.Status != "" && seat.Status != "active" {
			continue
		}
		for _, claim := range table.LatestCustodyState.StackClaims {
			if claim.PlayerID == seat.PlayerID && claim.AmountSats > 0 {
				count++
				break
			}
		}
	}
	return count
}
