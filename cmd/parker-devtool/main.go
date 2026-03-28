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
	if err := flags.Parse(argv); err != nil {
		return err
	}
	if strings.TrimSpace(*alicePlayerID) == "" || strings.TrimSpace(*bobPlayerID) == "" {
		return errors.New("select-table-action requires --alice and --bob")
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

	aliceSeat, ok := findSeat(state.SeatedPlayers, *alicePlayerID)
	if !ok {
		return errors.New("missing alice seat")
	}
	bobSeat, ok := findSeat(state.SeatedPlayers, *bobPlayerID)
	if !ok {
		return errors.New("missing bob seat")
	}

	actor := "bob"
	actorPlayerID := *bobPlayerID
	switch state.ActingSeatIndex {
	case aliceSeat.SeatIndex:
		actor = "alice"
		actorPlayerID = *alicePlayerID
	case bobSeat.SeatIndex:
	default:
		return fmt.Errorf("acting seat %d does not match alice or bob", state.ActingSeatIndex)
	}

	contribution := state.RoundContributions[actorPlayerID]
	toCall := maxInt(0, state.CurrentBetSats-contribution)

	action := "check"
	amount := ""
	if state.Phase == "preflop" && toCall == 0 {
		action = "bet"
		amount = strconv.Itoa(maxInt(state.MinRaiseToSats, 800))
	} else if toCall > 0 {
		action = "call"
	}

	if amount == "" {
		_, err = fmt.Fprintf(os.Stdout, "%s %s\n", actor, action)
		return err
	}
	_, err = fmt.Fprintf(os.Stdout, "%s %s %s\n", actor, action, amount)
	return err
}

type tableEnvelope struct {
	Data tableEnvelopeData `json:"data"`
}

type tableEnvelopeData struct {
	PublicState *tablePublicState `json:"publicState"`
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
