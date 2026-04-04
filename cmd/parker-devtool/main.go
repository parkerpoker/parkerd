package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "%s\n", err.Error())
		os.Exit(1)
	}
}

func run(argv []string) error {
	if len(argv) == 0 {
		return errors.New("usage: parker-devtool <json-field|free-port|select-table-action> [args]")
	}

	switch argv[0] {
	case "json-field":
		if len(argv) != 2 {
			return errors.New("usage: parker-devtool json-field <path>")
		}
		return runJSONField(argv[1])
	case "free-port":
		if len(argv) != 1 {
			return errors.New("usage: parker-devtool free-port")
		}
		return runFreePort()
	case "select-table-action":
		return runSelectTableAction(argv[1:])
	default:
		return fmt.Errorf("unknown parker-devtool command %q", argv[0])
	}
}

func runJSONField(path string) error {
	value, err := decodeInput()
	if err != nil {
		return err
	}
	selected, err := lookupPath(value, strings.Split(path, "."))
	if err != nil {
		return err
	}
	printJSONValue(selected)
	return nil
}

func runFreePort() error {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer listener.Close()

	address, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		return errors.New("unexpected listener address type")
	}
	_, err = fmt.Fprintf(os.Stdout, "%d\n", address.Port)
	return err
}

func runSelectTableAction(argv []string) error {
	flags := flag.NewFlagSet("select-table-action", flag.ContinueOnError)
	flags.SetOutput(io.Discard)

	alicePlayerID := flags.String("alice", "", "alice player id")
	bobPlayerID := flags.String("bob", "", "bob player id")
	avoidShowdown := flags.Bool("avoid-showdown", false, "prefer a fold before showdown")
	preferEarlySettlement := flags.Bool("prefer-early-settlement", false, "prefer a fold when facing a bet")
	var players namedPlayersFlag
	flags.Var(&players, "player", "player mapping in the form profile=playerId")
	if err := flags.Parse(argv); err != nil {
		return err
	}
	if len(players) == 0 {
		if strings.TrimSpace(*alicePlayerID) == "" || strings.TrimSpace(*bobPlayerID) == "" {
			return errors.New("select-table-action requires either two --player profile=playerId values or both --alice and --bob")
		}
		players = append(players,
			namedPlayer{Profile: "alice", PlayerID: *alicePlayerID},
			namedPlayer{Profile: "bob", PlayerID: *bobPlayerID},
		)
	}
	if len(players) != 2 {
		return errors.New("select-table-action requires exactly two players")
	}

	var envelope tableEnvelope
	body, err := io.ReadAll(os.Stdin)
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return err
	}

	state := envelope.Data.PublicState
	if state == nil || strings.TrimSpace(state.HandID) == "" || strings.TrimSpace(state.Phase) == "" {
		return nil
	}
	if state.Phase == "settled" {
		_, err := fmt.Fprintln(os.Stdout, "settled")
		return err
	}
	if !phaseAllowsTableActions(state.Phase) {
		return nil
	}

	firstPlayer := players[0]
	secondPlayer := players[1]

	firstSeat, ok := findSeat(state.SeatedPlayers, firstPlayer.PlayerID)
	if !ok {
		return fmt.Errorf("missing seat for %s", firstPlayer.Profile)
	}
	secondSeat, ok := findSeat(state.SeatedPlayers, secondPlayer.PlayerID)
	if !ok {
		return fmt.Errorf("missing seat for %s", secondPlayer.Profile)
	}

	actor := secondPlayer.Profile
	actorPlayerID := secondPlayer.PlayerID
	switch state.ActingSeatIndex {
	case firstSeat.SeatIndex:
		actor = firstPlayer.Profile
		actorPlayerID = firstPlayer.PlayerID
	case secondSeat.SeatIndex:
	default:
		return fmt.Errorf("acting seat %d does not match the configured players", state.ActingSeatIndex)
	}

	contribution := state.RoundContributions[actorPlayerID]
	toCall := maxInt(0, state.CurrentBetSats-contribution)

	if envelope.Data.Local.CanAct {
		if selected, ok := selectMenuActionLine(envelope.Data.Local.TurnMenu, state.Phase, contribution, toCall, *avoidShowdown, *preferEarlySettlement); ok {
			_, err = fmt.Fprintf(os.Stdout, "%s %s\n", actor, selected)
			return err
		}
	}
	if selected, ok := selectMenuActionLine(envelope.Data.PendingTurnMenu, state.Phase, contribution, toCall, *avoidShowdown, *preferEarlySettlement); ok {
		_, err = fmt.Fprintf(os.Stdout, "%s %s\n", actor, selected)
		return err
	}

	action := "check"
	amount := ""
	if *avoidShowdown && state.Phase == "river" && toCall == 0 {
		action = "bet"
		amount = strconv.Itoa(maxInt(state.MinRaiseToSats, 800))
	} else if state.Phase == "preflop" && toCall == 0 {
		action = "bet"
		amount = strconv.Itoa(maxInt(state.MinRaiseToSats, 800))
	} else if toCall > 0 {
		if *avoidShowdown && state.Phase == "river" {
			action = "fold"
		} else if *preferEarlySettlement {
			action = "fold"
		} else {
			action = "call"
		}
	}

	if amount == "" {
		_, err = fmt.Fprintf(os.Stdout, "%s %s\n", actor, action)
		return err
	}
	_, err = fmt.Fprintf(os.Stdout, "%s %s %s\n", actor, action, amount)
	return err
}

func phaseAllowsTableActions(phase string) bool {
	switch phase {
	case "preflop", "flop", "turn", "river":
		return true
	default:
		return false
	}
}

type namedPlayer struct {
	Profile  string
	PlayerID string
}

type namedPlayersFlag []namedPlayer

func (flagValue *namedPlayersFlag) String() string {
	parts := make([]string, 0, len(*flagValue))
	for _, player := range *flagValue {
		parts = append(parts, player.Profile+"="+player.PlayerID)
	}
	return strings.Join(parts, ",")
}

func (flagValue *namedPlayersFlag) Set(value string) error {
	parts := strings.SplitN(strings.TrimSpace(value), "=", 2)
	if len(parts) != 2 {
		return fmt.Errorf("player mapping must be profile=playerId, got %q", value)
	}
	profile := strings.TrimSpace(parts[0])
	playerID := strings.TrimSpace(parts[1])
	if profile == "" || playerID == "" {
		return fmt.Errorf("player mapping must be profile=playerId, got %q", value)
	}
	*flagValue = append(*flagValue, namedPlayer{Profile: profile, PlayerID: playerID})
	return nil
}

type tableEnvelope struct {
	Data tableEnvelopeData `json:"data"`
}

type tableEnvelopeData struct {
	Local           tableLocalView    `json:"local"`
	PendingTurnMenu *tableTurnMenu    `json:"pendingTurnMenu"`
	PublicState     *tablePublicState `json:"publicState"`
}

type tablePublicState struct {
	ActingSeatIndex    int            `json:"actingSeatIndex"`
	CurrentBetSats     int            `json:"currentBetSats"`
	HandID             string         `json:"handId"`
	MinRaiseToSats     int            `json:"minRaiseToSats"`
	Phase              string         `json:"phase"`
	RoundContributions map[string]int `json:"roundContributions"`
	SeatedPlayers      []tableSeat    `json:"seatedPlayers"`
}

type tableSeat struct {
	PlayerID  string `json:"playerId"`
	SeatIndex int    `json:"seatIndex"`
}

type tableLocalView struct {
	CanAct   bool           `json:"canAct"`
	TurnMenu *tableTurnMenu `json:"turnMenu"`
}

type tableTurnMenu struct {
	Options []tableMenuOption `json:"options"`
}

type tableMenuOption struct {
	Action tableAction `json:"action"`
}

type tableAction struct {
	LegacyTotalSats *int   `json:"TotalSats"`
	LegacyType      string `json:"Type"`
	TotalSats       *int   `json:"totalSats"`
	Type            string `json:"type"`
}

func (action tableAction) normalizedType() string {
	if strings.TrimSpace(action.Type) != "" {
		return strings.ToLower(strings.TrimSpace(action.Type))
	}
	return strings.ToLower(strings.TrimSpace(action.LegacyType))
}

func (action tableAction) totalSats() (int, bool) {
	if action.TotalSats != nil {
		return *action.TotalSats, true
	}
	if action.LegacyTotalSats != nil {
		return *action.LegacyTotalSats, true
	}
	return 0, false
}

func selectMenuActionLine(menu *tableTurnMenu, phase string, contribution, toCall int, avoidShowdown, preferEarlySettlement bool) (string, bool) {
	if menu == nil || len(menu.Options) == 0 {
		return "", false
	}

	bestAggressiveType := ""
	bestAggressiveTotal := 0
	bestAggressiveOK := false
	haveCheck := false
	haveCall := false
	haveFold := false

	for _, option := range menu.Options {
		actionType := option.Action.normalizedType()
		switch actionType {
		case "bet", "raise":
			total, ok := option.Action.totalSats()
			if !ok || total <= contribution {
				continue
			}
			if !bestAggressiveOK || total < bestAggressiveTotal || (total == bestAggressiveTotal && actionType < bestAggressiveType) {
				bestAggressiveType = actionType
				bestAggressiveTotal = total
				bestAggressiveOK = true
			}
		case "check":
			haveCheck = true
		case "call":
			haveCall = true
		case "fold":
			haveFold = true
		}
	}

	if avoidShowdown && phase == "river" {
		if toCall > 0 {
			if haveFold {
				return "fold", true
			}
			if haveCall {
				return "call", true
			}
		} else if bestAggressiveOK {
			return fmt.Sprintf("%s %d", bestAggressiveType, bestAggressiveTotal), true
		}
	}
	if preferEarlySettlement && toCall > 0 {
		if haveFold {
			return "fold", true
		}
		if haveCall {
			return "call", true
		}
	}

	if phase == "preflop" && toCall == 0 && bestAggressiveOK {
		return fmt.Sprintf("%s %d", bestAggressiveType, bestAggressiveTotal), true
	}
	if toCall > 0 {
		if haveCall {
			return "call", true
		}
		if haveFold {
			return "fold", true
		}
	}
	if haveCheck {
		return "check", true
	}
	if bestAggressiveOK {
		return fmt.Sprintf("%s %d", bestAggressiveType, bestAggressiveTotal), true
	}
	if haveCall {
		return "call", true
	}
	if haveFold {
		return "fold", true
	}
	return "", false
}

func decodeInput() (any, error) {
	decoder := json.NewDecoder(os.Stdin)
	decoder.UseNumber()

	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return value, nil
}

func lookupPath(value any, path []string) (any, error) {
	current := value
	for _, part := range path {
		if strings.TrimSpace(part) == "" {
			continue
		}
		switch typed := current.(type) {
		case map[string]any:
			current = typed[part]
		case []any:
			index, err := strconv.Atoi(part)
			if err != nil {
				return nil, fmt.Errorf("path segment %q is not a valid array index", part)
			}
			if index < 0 || index >= len(typed) {
				return nil, nil
			}
			current = typed[index]
		default:
			return nil, nil
		}
	}
	return current, nil
}

func printJSONValue(value any) {
	switch typed := value.(type) {
	case nil:
		_, _ = fmt.Fprintln(os.Stdout)
	case map[string]any, []any:
		encoded, err := json.Marshal(typed)
		if err != nil {
			_, _ = fmt.Fprintln(os.Stdout)
			return
		}
		_, _ = fmt.Fprintf(os.Stdout, "%s\n", encoded)
	default:
		_, _ = fmt.Fprintf(os.Stdout, "%v\n", typed)
	}
}

func findSeat(seats []tableSeat, playerID string) (tableSeat, bool) {
	for _, seat := range seats {
		if seat.PlayerID == playerID {
			return seat, true
		}
	}
	return tableSeat{}, false
}

func maxInt(left, right int) int {
	if left > right {
		return left
	}
	return right
}
