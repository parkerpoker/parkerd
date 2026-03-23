package game

import (
	"sort"
)

type HandScore struct {
	Category   int
	RankValues []int
	BestFive   []CardCode
	Label      string
}

var handLabels = []string{
	"high-card",
	"pair",
	"two-pair",
	"three-of-a-kind",
	"straight",
	"flush",
	"full-house",
	"four-of-a-kind",
	"straight-flush",
}

func compareRankArrays(left, right []int) int {
	maxLength := len(left)
	if len(right) > maxLength {
		maxLength = len(right)
	}
	for index := 0; index < maxLength; index++ {
		leftValue := 0
		if index < len(left) {
			leftValue = left[index]
		}
		rightValue := 0
		if index < len(right) {
			rightValue = right[index]
		}
		if leftValue > rightValue {
			return 1
		}
		if leftValue < rightValue {
			return -1
		}
	}
	return 0
}

func sortCardsDesc(cards []Card) []Card {
	clone := append([]Card(nil), cards...)
	sort.SliceStable(clone, func(left, right int) bool {
		return clone[left].RankValue > clone[right].RankValue
	})
	return clone
}

func getStraightHigh(sortedDistinctRanksDesc []int) *int {
	ranksCopy := append([]int(nil), sortedDistinctRanksDesc...)
	if len(ranksCopy) > 0 && ranksCopy[0] == 14 {
		ranksCopy = append(ranksCopy, 1)
	}

	run := 1
	var bestHigh *int
	for index := 0; index < len(ranksCopy)-1; index++ {
		if ranksCopy[index] == ranksCopy[index+1]+1 {
			run++
			if run >= 5 {
				value := ranksCopy[index-3]
				bestHigh = &value
			}
		} else {
			run = 1
		}
	}
	return bestHigh
}

func compareFiveCardHands(left, right HandScore) int {
	if left.Category > right.Category {
		return 1
	}
	if left.Category < right.Category {
		return -1
	}
	return compareRankArrays(left.RankValues, right.RankValues)
}

func evaluateFiveCardHand(cards []Card) HandScore {
	sorted := sortCardsDesc(cards)
	suits := map[string]struct{}{}
	for _, card := range sorted {
		suits[card.Suit] = struct{}{}
	}
	isFlush := len(suits) == 1

	counts := map[int]int{}
	for _, card := range sorted {
		counts[card.RankValue]++
	}

	type entry struct {
		rank  int
		count int
	}

	entries := make([]entry, 0, len(counts))
	for rank, count := range counts {
		entries = append(entries, entry{rank: rank, count: count})
	}
	sort.Slice(entries, func(left, right int) bool {
		if entries[left].count != entries[right].count {
			return entries[left].count > entries[right].count
		}
		return entries[left].rank > entries[right].rank
	})

	distinctRanksDesc := make([]int, 0, len(counts))
	for rank := range counts {
		distinctRanksDesc = append(distinctRanksDesc, rank)
	}
	sort.Slice(distinctRanksDesc, func(left, right int) bool {
		return distinctRanksDesc[left] > distinctRanksDesc[right]
	})

	straightHigh := getStraightHigh(distinctRanksDesc)

	bestFive := make([]CardCode, len(sorted))
	for index, card := range sorted {
		bestFive[index] = card.Code
	}

	if isFlush && straightHigh != nil {
		return HandScore{
			Category:   8,
			RankValues: []int{*straightHigh},
			BestFive:   bestFive,
			Label:      handLabels[8],
		}
	}

	if len(entries) > 0 && entries[0].count == 4 {
		quad := entries[0].rank
		kicker := entries[1].rank
		return HandScore{
			Category:   7,
			RankValues: []int{quad, kicker},
			BestFive:   bestFive,
			Label:      handLabels[7],
		}
	}

	if len(entries) > 1 && entries[0].count == 3 && entries[1].count == 2 {
		return HandScore{
			Category:   6,
			RankValues: []int{entries[0].rank, entries[1].rank},
			BestFive:   bestFive,
			Label:      handLabels[6],
		}
	}

	if isFlush {
		rankValues := make([]int, 0, len(sorted))
		for _, card := range sorted {
			rankValues = append(rankValues, card.RankValue)
		}
		return HandScore{
			Category:   5,
			RankValues: rankValues,
			BestFive:   bestFive,
			Label:      handLabels[5],
		}
	}

	if straightHigh != nil {
		return HandScore{
			Category:   4,
			RankValues: []int{*straightHigh},
			BestFive:   bestFive,
			Label:      handLabels[4],
		}
	}

	if len(entries) > 0 && entries[0].count == 3 {
		kickers := make([]int, 0, len(entries)-1)
		for _, candidate := range entries[1:] {
			kickers = append(kickers, candidate.rank)
		}
		sort.Slice(kickers, func(left, right int) bool {
			return kickers[left] > kickers[right]
		})
		rankValues := append([]int{entries[0].rank}, kickers...)
		return HandScore{
			Category:   3,
			RankValues: rankValues,
			BestFive:   bestFive,
			Label:      handLabels[3],
		}
	}

	if len(entries) > 1 && entries[0].count == 2 && entries[1].count == 2 {
		pairs := []int{entries[0].rank, entries[1].rank}
		sort.Slice(pairs, func(left, right int) bool {
			return pairs[left] > pairs[right]
		})
		kicker := entries[2].rank
		return HandScore{
			Category:   2,
			RankValues: []int{pairs[0], pairs[1], kicker},
			BestFive:   bestFive,
			Label:      handLabels[2],
		}
	}

	if len(entries) > 0 && entries[0].count == 2 {
		kickers := make([]int, 0, len(entries)-1)
		for _, candidate := range entries[1:] {
			kickers = append(kickers, candidate.rank)
		}
		sort.Slice(kickers, func(left, right int) bool {
			return kickers[left] > kickers[right]
		})
		rankValues := append([]int{entries[0].rank}, kickers...)
		return HandScore{
			Category:   1,
			RankValues: rankValues,
			BestFive:   bestFive,
			Label:      handLabels[1],
		}
	}

	rankValues := make([]int, 0, len(sorted))
	for _, card := range sorted {
		rankValues = append(rankValues, card.RankValue)
	}
	return HandScore{
		Category:   0,
		RankValues: rankValues,
		BestFive:   bestFive,
		Label:      handLabels[0],
	}
}

func combinations[T any](items []T, size int) [][]T {
	if size == 0 {
		return [][]T{{}}
	}
	if len(items) < size || len(items) == 0 {
		return nil
	}

	head := items[0]
	tail := items[1:]

	withHead := combinations(tail, size-1)
	for index := range withHead {
		withHead[index] = append([]T{head}, withHead[index]...)
	}

	return append(withHead, combinations(tail, size)...)
}

func ScoreSevenCardHand(cardCodes []CardCode) (HandScore, error) {
	if len(cardCodes) != 7 {
		return HandScore{}, fmtErrorf("expected 7 cards, received %d", len(cardCodes))
	}

	var best *HandScore
	for _, candidate := range combinations(cardCodes, 5) {
		cards := make([]Card, 0, len(candidate))
		for _, code := range candidate {
			cards = append(cards, ParseCard(code))
		}
		score := evaluateFiveCardHand(cards)
		if best == nil || compareFiveCardHands(score, *best) > 0 {
			copyScore := score
			best = &copyScore
		}
	}

	if best == nil {
		return HandScore{}, fmtErrorf("could not score seven card hand")
	}

	return *best, nil
}

func CompareScoredHands(left, right HandScore) int {
	return compareFiveCardHands(left, right)
}
