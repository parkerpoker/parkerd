package game

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math/big"
	"sort"
	"strings"

	"github.com/danieldresner/arkade_fun/internal/settlementcore"
)

var (
	mentalPrime   = mustBigInt("7fffffffffffffffffffffffffffffff")
	mentalTotient = new(big.Int).Sub(new(big.Int).Set(mentalPrime), big.NewInt(1))

	mentalCardValueByCode = map[CardCode]int{}
	mentalCardCodeByValue = map[int]CardCode{}
)

func init() {
	deck := CreateDeck()
	for index, card := range deck {
		value := index + 2
		mentalCardValueByCode[card.Code] = value
		mentalCardCodeByValue[value] = card.Code
	}
}

func mustBigInt(hexValue string) *big.Int {
	value, ok := new(big.Int).SetString(hexValue, 16)
	if !ok {
		panic(fmt.Sprintf("invalid big integer %q", hexValue))
	}
	return value
}

func normalizeBigIntHex(hexValue string) (string, error) {
	trimmed := strings.TrimSpace(hexValue)
	if trimmed == "" {
		return "", fmt.Errorf("missing bigint hex value")
	}
	value, ok := new(big.Int).SetString(trimmed, 16)
	if !ok {
		return "", fmt.Errorf("invalid bigint hex value %q", hexValue)
	}
	if value.Sign() < 0 || value.Cmp(mentalPrime) >= 0 {
		return "", fmt.Errorf("bigint value is out of range")
	}
	return strings.TrimLeft(value.Text(16), "0"), nil
}

func encodeMentalValue(value *big.Int) string {
	if value == nil {
		return ""
	}
	return strings.TrimLeft(value.Text(16), "0")
}

func decodeMentalValue(hexValue string) (*big.Int, error) {
	normalized, err := normalizeBigIntHex(hexValue)
	if err != nil {
		return nil, err
	}
	value, ok := new(big.Int).SetString(normalized, 16)
	if !ok {
		return nil, fmt.Errorf("invalid bigint hex value")
	}
	return value, nil
}

func GenerateMentalKeyPair() (MentalKeyPair, error) {
	limit := new(big.Int).Sub(mentalTotient, big.NewInt(3))
	one := big.NewInt(1)
	two := big.NewInt(2)

	for {
		candidate, err := rand.Int(rand.Reader, limit)
		if err != nil {
			return MentalKeyPair{}, err
		}
		candidate.Add(candidate, two)
		if new(big.Int).GCD(nil, nil, candidate, mentalTotient).Cmp(one) != 0 {
			continue
		}
		inverse := new(big.Int).ModInverse(candidate, mentalTotient)
		if inverse == nil {
			continue
		}
		return MentalKeyPair{
			PrivateExponentHex: encodeMentalValue(inverse),
			PublicExponentHex:  encodeMentalValue(candidate),
		}, nil
	}
}

func orderedMentalDeck() []string {
	deck := CreateDeck()
	values := make([]string, 0, len(deck))
	for _, card := range deck {
		values = append(values, encodeMentalValue(big.NewInt(int64(mentalCardValueByCode[card.Code]))))
	}
	return values
}

func EncryptMentalValueHex(valueHex, publicExponentHex string) (string, error) {
	value, err := decodeMentalValue(valueHex)
	if err != nil {
		return "", err
	}
	publicExponent, err := decodeMentalValue(publicExponentHex)
	if err != nil {
		return "", err
	}
	return encodeMentalValue(new(big.Int).Exp(value, publicExponent, mentalPrime)), nil
}

func DecryptMentalValueHex(valueHex, privateExponentHex string) (string, error) {
	value, err := decodeMentalValue(valueHex)
	if err != nil {
		return "", err
	}
	privateExponent, err := decodeMentalValue(privateExponentHex)
	if err != nil {
		return "", err
	}
	return encodeMentalValue(new(big.Int).Exp(value, privateExponent, mentalPrime)), nil
}

func DecodeMentalCardHex(valueHex string) (CardCode, error) {
	value, err := decodeMentalValue(valueHex)
	if err != nil {
		return "", err
	}
	decoded, ok := mentalCardCodeByValue[int(value.Int64())]
	if !ok {
		return "", fmt.Errorf("mental card value %s is not a card", valueHex)
	}
	return decoded, nil
}

func EncodeMentalCard(card CardCode) (string, error) {
	value, ok := mentalCardValueByCode[card]
	if !ok {
		return "", fmt.Errorf("unknown card %q", card)
	}
	return encodeMentalValue(big.NewInt(int64(value))), nil
}

func MentalDeckStageRoot(stage []string) (string, error) {
	return settlementcore.HashStructuredDataHex(map[string]any{
		"stage": stage,
		"type":  "mental-deck-stage",
	})
}

func mentalDeckPermutation(seedHex string, size int) ([]int, error) {
	trimmed := strings.TrimSpace(seedHex)
	if trimmed == "" {
		return nil, fmt.Errorf("missing shuffle seed")
	}
	if _, err := hex.DecodeString(trimmed); err != nil {
		return nil, fmt.Errorf("invalid shuffle seed: %w", err)
	}

	indices := make([]int, size)
	for index := range indices {
		indices[index] = index
	}
	for index := size - 1; index > 0; index-- {
		randomIndex := int(nextRandomFraction(trimmed, index) * float64(index+1))
		indices[index], indices[randomIndex] = indices[randomIndex], indices[index]
	}
	return indices, nil
}

func ApplyMentalShuffle(previous []string, publicExponentHex, shuffleSeedHex string) ([]string, error) {
	encrypted := make([]string, 0, len(previous))
	for _, valueHex := range previous {
		nextValueHex, err := EncryptMentalValueHex(valueHex, publicExponentHex)
		if err != nil {
			return nil, err
		}
		encrypted = append(encrypted, nextValueHex)
	}

	permutation, err := mentalDeckPermutation(shuffleSeedHex, len(encrypted))
	if err != nil {
		return nil, err
	}
	nextStage := make([]string, len(encrypted))
	for index, sourceIndex := range permutation {
		nextStage[index] = encrypted[sourceIndex]
	}
	return nextStage, nil
}

func BuildFairnessCommitment(tableID string, handNumber int, seatIndex int, playerID, phase, shuffleSeedHex, lockPublicExponentHex string) (string, error) {
	return settlementcore.HashStructuredDataHex(map[string]any{
		"handNumber":            handNumber,
		"lockPublicExponentHex": lockPublicExponentHex,
		"phase":                 phase,
		"playerId":              playerID,
		"seatIndex":             seatIndex,
		"shuffleSeedHex":        shuffleSeedHex,
		"tableId":               tableID,
		"type":                  "dealerless-fairness-commitment",
	})
}

func VerifyFairnessReveal(tableID string, handNumber int, seatIndex int, playerID, phase, commitmentHash, shuffleSeedHex, lockPublicExponentHex string) error {
	derived, err := BuildFairnessCommitment(tableID, handNumber, seatIndex, playerID, phase, shuffleSeedHex, lockPublicExponentHex)
	if err != nil {
		return err
	}
	if derived != commitmentHash {
		return fmt.Errorf("fairness reveal does not match commitment for seat %d", seatIndex)
	}
	return nil
}

func ReplayMentalDeck(reveals []MentalDeckReveal) (MentalDeckReplay, error) {
	sortedReveals := append([]MentalDeckReveal(nil), reveals...)
	sort.Slice(sortedReveals, func(left, right int) bool {
		return sortedReveals[left].SeatIndex < sortedReveals[right].SeatIndex
	})

	stage := orderedMentalDeck()
	stageRoots := make([]string, 0, len(sortedReveals))
	stageRootBySeat := map[int]string{}
	stageBySeat := map[int][]string{}
	for _, reveal := range sortedReveals {
		nextStage, err := ApplyMentalShuffle(stage, reveal.LockPublicExponentHex, reveal.ShuffleSeedHex)
		if err != nil {
			return MentalDeckReplay{}, err
		}
		root, err := MentalDeckStageRoot(nextStage)
		if err != nil {
			return MentalDeckReplay{}, err
		}
		stageRoots = append(stageRoots, root)
		stageRootBySeat[reveal.SeatIndex] = root
		stageBySeat[reveal.SeatIndex] = append([]string(nil), nextStage...)
		stage = nextStage
	}

	return MentalDeckReplay{
		FinalDeck:             stage,
		RevealStageRoots:      stageRoots,
		RevealStageRootBySeat: stageRootBySeat,
		RevealStagesBySeat:    stageBySeat,
	}, nil
}
