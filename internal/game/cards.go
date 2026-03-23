package game

const (
	suitClubs    = "c"
	suitDiamonds = "d"
	suitHearts   = "h"
	suitSpades   = "s"
)

var suits = []string{suitClubs, suitDiamonds, suitHearts, suitSpades}
var ranks = []string{"2", "3", "4", "5", "6", "7", "8", "9", "T", "J", "Q", "K", "A"}

type Card struct {
	Code      CardCode
	Rank      string
	Suit      string
	RankValue int
}

var rankValueByRank = map[string]int{
	"2": 2,
	"3": 3,
	"4": 4,
	"5": 5,
	"6": 6,
	"7": 7,
	"8": 8,
	"9": 9,
	"T": 10,
	"J": 11,
	"Q": 12,
	"K": 13,
	"A": 14,
}

func ParseCard(code CardCode) Card {
	if len(code) != 2 {
		return Card{Code: code}
	}

	rank := string(code[0])
	suit := string(code[1])
	return Card{
		Code:      code,
		Rank:      rank,
		Suit:      suit,
		RankValue: rankValueByRank[rank],
	}
}

func CreateDeck() []Card {
	deck := make([]Card, 0, 52)
	for _, suit := range suits {
		for _, rank := range ranks {
			deck = append(deck, ParseCard(CardCode(rank+suit)))
		}
	}
	return deck
}
