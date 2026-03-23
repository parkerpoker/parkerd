package game

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

func Sha256Hex(input string) string {
	sum := sha256.Sum256([]byte(input))
	return hex.EncodeToString(sum[:])
}

func nextRandomFraction(seedHex string, counter int) float64 {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", seedHex, counter)))
	return float64(binary.BigEndian.Uint32(sum[:4])) / 0x1_0000_0000
}

func CreateDeterministicDeck(seedHex string) []Card {
	deck := CreateDeck()
	for index := len(deck) - 1; index > 0; index-- {
		randomIndex := int(math.Floor(nextRandomFraction(seedHex, index) * float64(index+1)))
		deck[index], deck[randomIndex] = deck[randomIndex], deck[index]
	}
	return deck
}

func BuildCommitmentHash(tableID string, seatIndex int, playerID string, seedHex string) string {
	return Sha256Hex(fmt.Sprintf("%s:%d:%s:%s", tableID, seatIndex, playerID, seedHex))
}

func DeriveDeckSeed(tableID string, handNumber int, commitments []DeckCommitment, reveals []DeckCommitment) (string, error) {
	verifiedReveals := make([]string, 0, len(reveals))
	for _, reveal := range reveals {
		if reveal.RevealSeed == "" {
			return "", fmt.Errorf("missing reveal seed for seat %d", reveal.SeatIndex)
		}

		derived := BuildCommitmentHash(tableID, reveal.SeatIndex, reveal.PlayerID, reveal.RevealSeed)
		if derived != reveal.CommitmentHash {
			return "", fmt.Errorf("reveal does not match commitment for seat %d", reveal.SeatIndex)
		}
		verifiedReveals = append(
			verifiedReveals,
			fmt.Sprintf("%d:%s:%s:%s", reveal.SeatIndex, reveal.PlayerID, reveal.CommitmentHash, reveal.RevealSeed),
		)
	}

	sortedCommitments := append([]DeckCommitment(nil), commitments...)
	sort.Slice(sortedCommitments, func(left, right int) bool {
		return sortedCommitments[left].SeatIndex < sortedCommitments[right].SeatIndex
	})

	commitmentParts := make([]string, 0, len(sortedCommitments))
	for _, commitment := range sortedCommitments {
		commitmentParts = append(
			commitmentParts,
			fmt.Sprintf("%d:%s:%s", commitment.SeatIndex, commitment.PlayerID, commitment.CommitmentHash),
		)
	}

	sort.Strings(verifiedReveals)
	parts := append([]string{tableID, strconv.Itoa(handNumber)}, commitmentParts...)
	parts = append(parts, verifiedReveals...)
	return Sha256Hex(strings.Join(parts, "|")), nil
}
