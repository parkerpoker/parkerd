package tablecustody

import (
	"fmt"
	"sort"
)

func DerivePotSlices(participants []SliceParticipant) ([]PotSlice, error) {
	if len(participants) == 0 {
		return nil, nil
	}

	distinctCaps := make([]int, 0, len(participants))
	seenCaps := map[int]struct{}{}
	for _, participant := range participants {
		if participant.PlayerID == "" {
			return nil, fmt.Errorf("pot participant is missing player id")
		}
		if participant.ContributionSats < 0 {
			return nil, fmt.Errorf("pot participant %s has negative contribution", participant.PlayerID)
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

	slices := make([]PotSlice, 0, len(distinctCaps))
	previousCap := 0
	for index, capSats := range distinctCaps {
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
		totalSats := (capSats - previousCap) * len(contributors)
		if totalSats <= 0 {
			previousCap = capSats
			continue
		}
		slices = append(slices, PotSlice{
			CapSats:              capSats,
			ContributedPlayerIDs: sortedStrings(contributedPlayerIDs),
			Contributions:        contributions,
			EligiblePlayerIDs:    sortedStrings(eligiblePlayerIDs),
			PotID:                fmt.Sprintf("pot-%02d", index),
			Sequence:             index,
			Status:               "open",
			TotalSats:            totalSats,
		})
		previousCap = capSats
	}
	return slices, nil
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
