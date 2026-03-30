package tablecustody

import (
	"fmt"
	"sort"
)

type PotDerivation struct {
	MatchedContributionSats   map[string]int
	Slices                    []PotSlice
	UnmatchedContributionSats map[string]int
}

func DerivePotSlices(participants []SliceParticipant) ([]PotSlice, error) {
	derivation, err := DerivePotStructure(participants)
	if err != nil {
		return nil, err
	}
	return derivation.Slices, nil
}

func DerivePotStructure(participants []SliceParticipant) (PotDerivation, error) {
	derivation := PotDerivation{
		MatchedContributionSats:   map[string]int{},
		Slices:                    nil,
		UnmatchedContributionSats: map[string]int{},
	}
	if len(participants) == 0 {
		return derivation, nil
	}

	distinctCaps := make([]int, 0, len(participants))
	seenCaps := map[int]struct{}{}
	for _, participant := range participants {
		if participant.PlayerID == "" {
			return PotDerivation{}, fmt.Errorf("pot participant is missing player id")
		}
		if participant.ContributionSats < 0 {
			return PotDerivation{}, fmt.Errorf("pot participant %s has negative contribution", participant.PlayerID)
		}
		if participant.ContributionSats == 0 {
			continue
		}
		if _, ok := seenCaps[participant.ContributionSats]; ok {
			continue
		}
		seenCaps[participant.ContributionSats] = struct{}{}
		distinctCaps = append(distinctCaps, participant.ContributionSats)
	}
	sort.Ints(distinctCaps)

	previousCap := 0
	for _, capSats := range distinctCaps {
		contributors := make([]SliceParticipant, 0, len(participants))
		eligiblePlayerIDs := make([]string, 0, len(participants))
		contributedPlayerIDs := make([]string, 0, len(participants))
		contributions := map[string]int{}

		for _, participant := range participants {
			if participant.ContributionSats < capSats {
				continue
			}
			contributors = append(contributors, participant)
			contributedPlayerIDs = append(contributedPlayerIDs, participant.PlayerID)
			contributions[participant.PlayerID] = capSats - previousCap
			if !participant.Folded {
				eligiblePlayerIDs = append(eligiblePlayerIDs, participant.PlayerID)
			}
		}

		if len(contributors) == 0 {
			continue
		}
		if len(contributors) < 2 {
			break
		}
		totalSats := (capSats - previousCap) * len(contributors)
		if totalSats <= 0 {
			previousCap = capSats
			continue
		}
		for _, participant := range contributors {
			derivation.MatchedContributionSats[participant.PlayerID] += capSats - previousCap
		}
		sequence := len(derivation.Slices)
		derivation.Slices = append(derivation.Slices, PotSlice{
			CapSats:              capSats,
			ContributedPlayerIDs: sortedStrings(contributedPlayerIDs),
			Contributions:        contributions,
			EligiblePlayerIDs:    sortedStrings(eligiblePlayerIDs),
			PotID:                fmt.Sprintf("pot-%02d", sequence),
			Sequence:             sequence,
			Status:               "open",
			TotalSats:            totalSats,
		})
		previousCap = capSats
	}
	for _, participant := range participants {
		unmatched := participant.ContributionSats - derivation.MatchedContributionSats[participant.PlayerID]
		if unmatched > 0 {
			derivation.UnmatchedContributionSats[participant.PlayerID] = unmatched
		}
	}
	return derivation, nil
}

func SplitAmount(totalSats int, winners []PayoutCandidate, dealerSeatIndex int) (map[string]int, []string) {
	payouts := map[string]int{}
	if totalSats <= 0 || len(winners) == 0 {
		return payouts, nil
	}

	ordered := append([]PayoutCandidate(nil), winners...)
	sort.SliceStable(ordered, func(left, right int) bool {
		return seatDistanceFromDealer(ordered[left].SeatIndex, ordered[right].SeatIndex, dealerSeatIndex, ordered) < 0
	})

	share := totalSats / len(ordered)
	remainder := totalSats % len(ordered)
	oddChips := make([]string, 0, remainder)
	for _, winner := range ordered {
		payouts[winner.PlayerID] += share
	}
	for index := 0; index < remainder; index++ {
		payouts[ordered[index].PlayerID]++
		oddChips = append(oddChips, ordered[index].PlayerID)
	}
	return payouts, oddChips
}

func seatDistanceFromDealer(leftSeatIndex, rightSeatIndex, dealerSeatIndex int, winners []PayoutCandidate) int {
	playerCount := 0
	maxSeatIndex := dealerSeatIndex
	for _, winner := range winners {
		playerCount++
		if winner.SeatIndex > maxSeatIndex {
			maxSeatIndex = winner.SeatIndex
		}
	}
	if playerCount == 0 {
		if leftSeatIndex < rightSeatIndex {
			return -1
		}
		if leftSeatIndex > rightSeatIndex {
			return 1
		}
		return 0
	}
	modulus := maxSeatIndex + 1
	leftDistance := (leftSeatIndex - dealerSeatIndex + modulus) % modulus
	rightDistance := (rightSeatIndex - dealerSeatIndex + modulus) % modulus
	if leftDistance < rightDistance {
		return -1
	}
	if leftDistance > rightDistance {
		return 1
	}
	if leftSeatIndex < rightSeatIndex {
		return -1
	}
	if leftSeatIndex > rightSeatIndex {
		return 1
	}
	return 0
}
