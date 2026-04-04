package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	parker "github.com/parkerpoker/parkerd/internal"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "%s\n", err.Error())
		os.Exit(1)
	}
}

func run(argv []string) error {
	command, flags, positionals := parker.ParseCommandArgv(argv)
	config, err := parker.ResolveRuntimeConfig(flags)
	if err != nil {
		return err
	}

	if command == "" || command == "help" {
		printHelp()
		return nil
	}

	if command == "daemon" {
		profile, err := requireFlag(flags, "profile", "player1")
		if err != nil {
			return err
		}
		return runDaemonCommand(profile, positionals, flags, config)
	}

	profile, err := requireFlag(flags, "profile", "player1")
	if err != nil {
		return err
	}
	client := parker.NewClient(profile, config)

	switch command {
	case "bootstrap":
		result, err := client.Request("bootstrap", paramsOrNil(buildBootstrapParams(positionals, flags)), true)
		if err != nil {
			return err
		}
		return writeResult(config.OutputJSON, result)
	case "network":
		return runNetworkCommand(client, positionals, config.OutputJSON)
	case "table":
		return runTableCommand(client, positionals, flags, config.OutputJSON)
	case "funds":
		return runFundsCommand(client, positionals, config.OutputJSON)
	case "wallet":
		return runWalletCommand(client, positionals, config.OutputJSON)
	case "interactive":
		return runInteractive(client, config.OutputJSON)
	default:
		return fmt.Errorf("unknown command %s", command)
	}
}

func runNetworkCommand(client *parker.Client, positionals []string, outputJSON bool) error {
	if len(positionals) == 0 {
		return errors.New("unknown network subcommand")
	}
	switch positionals[0] {
	case "peers":
		result, err := client.Request("meshNetworkPeers", nil, true)
		if err != nil {
			return err
		}
		return writeResult(outputJSON, result)
	case "bootstrap":
		if len(positionals) < 2 || positionals[1] != "add" {
			return errors.New("network bootstrap requires the `add` subcommand")
		}
		endpoint, err := requirePositional(positionals, 2, "endpoint")
		if err != nil {
			return err
		}
		params := map[string]any{
			"endpoint": endpoint,
			"peerUrl":  endpoint,
		}
		if len(positionals) > 3 && positionals[3] != "" {
			params["alias"] = positionals[3]
		}
		result, err := client.Request("meshBootstrapPeer", params, true)
		if err != nil {
			return err
		}
		return writeResult(outputJSON, result)
	default:
		return fmt.Errorf("unknown network subcommand %s", positionals[0])
	}
}

func runTableCommand(client *parker.Client, positionals []string, flags parker.FlagMap, outputJSON bool) error {
	if len(positionals) == 0 {
		return errors.New("unknown table subcommand")
	}
	switch positionals[0] {
	case "create":
		table := map[string]any{}
		if value, ok := parker.FlagString(flags, "name"); ok {
			table["name"] = value
		}
		if value, ok := parker.FlagString(flags, "turn-timeout-mode"); ok {
			table["turnTimeoutMode"] = value
		}
		visibility, err := resolveTableVisibility(flags)
		if err != nil {
			return err
		}
		if visibility != "" {
			table["visibility"] = visibility
		}
		if witnessPeerIDs := parseCSVFlag(flags, "witness-peer-ids", "witness-peer-id"); len(witnessPeerIDs) > 0 {
			table["witnessPeerIds"] = witnessPeerIDs
		}
		result, err := client.Request("meshCreateTable", map[string]any{"table": table}, true)
		if err != nil {
			return err
		}
		return writeResult(outputJSON, result)
	case "created":
		params := map[string]any{}
		if value, ok := parker.FlagString(flags, "cursor"); ok {
			params["cursor"] = value
		}
		if value, ok := parker.FlagString(flags, "limit"); ok {
			limit, err := strconv.Atoi(value)
			if err != nil {
				return fmt.Errorf("limit must be a number")
			}
			params["limit"] = limit
		}
		result, err := client.Request("meshCreatedTables", paramsOrNil(params), true)
		if err != nil {
			return err
		}
		return writeResult(outputJSON, result)
	case "announce":
		result, err := client.Request("meshTableAnnounce", optionalTableParams(positionals, 1), true)
		if err != nil {
			return err
		}
		return writeResult(outputJSON, result)
	case "join":
		inviteCode, err := requirePositional(positionals, 1, "inviteCode")
		if err != nil {
			return err
		}
		buyInSats, err := parseOptionalNumber(optionalValue(positionals, 2), 4_000)
		if err != nil {
			return err
		}
		result, err := client.Request("meshTableJoin", map[string]any{
			"inviteCode": inviteCode,
			"buyInSats":  buyInSats,
		}, true)
		if err != nil {
			return err
		}
		return writeResult(outputJSON, result)
	case "watch":
		if len(positionals) > 1 && positionals[1] != "" {
			result, err := client.Request("meshGetTable", map[string]any{"tableId": positionals[1]}, true)
			if err != nil {
				return err
			}
			return writeResult(outputJSON, result)
		}
		return runWatchCommand(client, outputJSON)
	case "rotate-host":
		result, err := client.Request("meshRotateHost", optionalTableParams(positionals, 1), true)
		if err != nil {
			return err
		}
		return writeResult(outputJSON, result)
	case "action":
		payload, err := parseActionPayload(positionals[1:])
		if err != nil {
			return err
		}
		params := map[string]any{"payload": payload}
		if tableID, ok := parker.FlagString(flags, "table-id"); ok {
			params["tableId"] = tableID
		}
		result, err := client.Request("meshSendAction", params, true)
		if err != nil {
			return err
		}
		return writeResult(outputJSON, result)
	case "public":
		result, err := client.Request("meshPublicTables", nil, true)
		if err != nil {
			return err
		}
		return writeResult(outputJSON, result)
	default:
		return fmt.Errorf("unknown table subcommand %s", positionals[0])
	}
}

func runFundsCommand(client *parker.Client, positionals []string, outputJSON bool) error {
	if len(positionals) == 0 {
		return errors.New("unknown funds subcommand")
	}
	switch positionals[0] {
	case "buy-in":
		inviteCode, err := requirePositional(positionals, 1, "inviteCode")
		if err != nil {
			return err
		}
		buyInSats, err := parseOptionalNumber(optionalValue(positionals, 2), 4_000)
		if err != nil {
			return err
		}
		result, err := client.Request("meshTableJoin", map[string]any{
			"inviteCode": inviteCode,
			"buyInSats":  buyInSats,
		}, true)
		if err != nil {
			return err
		}
		return writeResult(outputJSON, result)
	case "cashout":
		result, err := client.Request("meshCashOut", optionalTableParams(positionals, 1), true)
		if err != nil {
			return err
		}
		return writeResult(outputJSON, result)
	case "renew":
		result, err := client.Request("meshRenew", optionalTableParams(positionals, 1), true)
		if err != nil {
			return err
		}
		return writeResult(outputJSON, result)
	case "exit":
		result, err := client.Request("meshExit", optionalTableParams(positionals, 1), true)
		if err != nil {
			return err
		}
		return writeResult(outputJSON, result)
	default:
		return fmt.Errorf("unknown funds subcommand %s", positionals[0])
	}
}

func runWalletCommand(client *parker.Client, positionals []string, outputJSON bool) error {
	subcommand := "summary"
	if len(positionals) > 0 && positionals[0] != "" {
		subcommand = positionals[0]
	}
	switch subcommand {
	case "nsec":
		result, err := client.Request("walletNsec", nil, true)
		if err != nil {
			return err
		}
		return writeResult(outputJSON, result)
	case "summary":
		result, err := client.Request("walletSummary", nil, true)
		if err != nil {
			return err
		}
		return writeResult(outputJSON, result)
	case "deposit":
		amountSats, err := parseRequiredNumber(optionalValue(positionals, 1), "amountSats")
		if err != nil {
			return err
		}
		result, err := client.Request("walletDeposit", map[string]any{"amountSats": amountSats}, true)
		if err != nil {
			return err
		}
		return writeResult(outputJSON, result)
	case "withdraw":
		amountSats, err := parseRequiredNumber(optionalValue(positionals, 1), "amountSats")
		if err != nil {
			return err
		}
		invoice, err := requirePositional(positionals, 2, "invoice")
		if err != nil {
			return err
		}
		result, err := client.Request("walletWithdraw", map[string]any{
			"amountSats": amountSats,
			"invoice":    invoice,
		}, true)
		if err != nil {
			return err
		}
		return writeResult(outputJSON, result)
	case "faucet":
		amountSats, err := parseRequiredNumber(optionalValue(positionals, 1), "amountSats")
		if err != nil {
			return err
		}
		if _, err := client.Request("walletFaucet", map[string]any{"amountSats": amountSats}, true); err != nil {
			return err
		}
		result, err := client.Request("walletSummary", nil, true)
		if err != nil {
			return err
		}
		return writeResult(outputJSON, result)
	case "onboard":
		txid, err := client.Request("walletOnboard", nil, true)
		if err != nil {
			return err
		}
		wallet, err := client.Request("walletSummary", nil, true)
		if err != nil {
			return err
		}
		return writeResult(outputJSON, map[string]any{
			"txid":   txid,
			"wallet": wallet,
		})
	case "offboard":
		address, err := requirePositional(positionals, 1, "address")
		if err != nil {
			return err
		}
		params := map[string]any{"address": address}
		if len(positionals) > 2 && positionals[2] != "" {
			amountSats, err := parseRequiredNumber(positionals[2], "amountSats")
			if err != nil {
				return err
			}
			params["amountSats"] = amountSats
		}
		txid, err := client.Request("walletOffboard", params, true)
		if err != nil {
			return err
		}
		return writeResult(outputJSON, map[string]any{"txid": txid})
	default:
		return fmt.Errorf("unknown wallet subcommand %s", subcommand)
	}
}

func runDaemonCommand(profile string, positionals []string, flags parker.FlagMap, config parker.RuntimeConfig) error {
	subcommand := "status"
	if len(positionals) > 0 && positionals[0] != "" {
		subcommand = positionals[0]
	}
	client := parker.NewClient(profile, config)
	mode := parseMode(flags)

	switch subcommand {
	case "start":
		if err := client.EnsureRunning(mode); err != nil {
			return err
		}
		result, err := client.Inspect(false)
		if err != nil {
			return err
		}
		return writeResult(config.OutputJSON, result)
	case "status":
		result, err := client.Inspect(false)
		if err != nil {
			return err
		}
		return writeResult(config.OutputJSON, result)
	case "stop":
		if _, err := client.Request("stop", nil, false); err != nil {
			return err
		}
		return writeResult(config.OutputJSON, map[string]any{"profile": profile, "stopping": true})
	case "watch":
		return runWatchCommand(client, config.OutputJSON)
	default:
		return fmt.Errorf("unknown daemon subcommand %s", subcommand)
	}
}

func runInteractive(client *parker.Client, outputJSON bool) error {
	reader := bufio.NewReader(os.Stdin)
	_, _ = fmt.Fprintln(os.Stdout, "interactive mode ready; type `help` for commands")
	for {
		_, _ = fmt.Fprint(os.Stdout, "parker> ")
		line, err := reader.ReadString('\n')
		if err != nil {
			return nil
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if line == "exit" || line == "quit" {
			return nil
		}
		if line == "help" {
			printHelp()
			continue
		}

		parts := strings.Fields(line)
		switch parts[0] {
		case "bootstrap":
			_, flags, positionals := parker.ParseCommandArgv(parts)
			result, err := client.Request("bootstrap", paramsOrNil(buildBootstrapParams(positionals, flags)), true)
			if err != nil {
				_, _ = fmt.Fprintf(os.Stderr, "%s\n", err.Error())
				continue
			}
			_ = writeResult(outputJSON, result)
		case "wallet":
			if err := runWalletCommand(client, parts[1:], outputJSON); err != nil {
				_, _ = fmt.Fprintf(os.Stderr, "%s\n", err.Error())
			}
		case "table":
			if len(parts) > 1 && parts[1] == "action" {
				payload, err := parseActionPayload(parts[2:])
				if err != nil {
					_, _ = fmt.Fprintf(os.Stderr, "%s\n", err.Error())
					continue
				}
				result, err := client.Request("meshSendAction", map[string]any{"payload": payload}, true)
				if err != nil {
					_, _ = fmt.Fprintf(os.Stderr, "%s\n", err.Error())
					continue
				}
				_ = writeResult(outputJSON, result)
				continue
			}
			_, _ = fmt.Fprintf(os.Stderr, "unknown interactive command %s\n", parts[0])
		default:
			_, _ = fmt.Fprintf(os.Stderr, "unknown interactive command %s\n", parts[0])
		}
	}
}

func runWatchCommand(client *parker.Client, outputJSON bool) error {
	session, err := client.StartWatch(true)
	if err != nil {
		return err
	}
	defer session.Stop()

	go func() {
		for event := range session.Events {
			_ = writeResult(outputJSON, event)
		}
	}()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)
	<-signals
	return nil
}

func writeResult(outputJSON bool, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if outputJSON {
		envelope, err := marshalResultEnvelope(raw)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(os.Stdout, "%s\n", envelope)
		return err
	}
	_, err = fmt.Fprintf(os.Stdout, "%s\n", formatHumanReadable(raw))
	return err
}

func marshalResultEnvelope(raw json.RawMessage) ([]byte, error) {
	if len(raw) == 0 {
		raw = json.RawMessage("null")
	}
	payload := struct {
		Data  json.RawMessage `json:"data"`
		Level string          `json:"level"`
	}{
		Data:  raw,
		Level: "result",
	}
	return json.Marshal(payload)
}

func formatHumanReadable(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "null"
	}

	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return string(raw)
	}
	if value, ok := decoded.(string); ok {
		return value
	}

	buffer := &bytes.Buffer{}
	encoder := json.NewEncoder(buffer)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(decoded); err != nil {
		return string(raw)
	}
	return strings.TrimSuffix(buffer.String(), "\n")
}

func paramsOrNil(params map[string]any) any {
	if len(params) == 0 {
		return nil
	}
	return params
}

func buildBootstrapParams(positionals []string, flags parker.FlagMap) map[string]any {
	params := map[string]any{}
	if len(positionals) > 0 && positionals[0] != "" {
		params["nickname"] = positionals[0]
	}
	if walletNsec, ok := parker.FlagString(flags, "wallet-nsec"); ok {
		params["walletNsec"] = walletNsec
	}
	return params
}

func optionalTableParams(positionals []string, index int) map[string]any {
	if len(positionals) <= index || positionals[index] == "" {
		return nil
	}
	return map[string]any{"tableId": positionals[index]}
}

func optionalValue(values []string, index int) string {
	if len(values) <= index {
		return ""
	}
	return values[index]
}

func requireFlag(flags parker.FlagMap, name, fallback string) (string, error) {
	if value, ok := parker.FlagString(flags, name); ok {
		return value, nil
	}
	if fallback != "" {
		return fallback, nil
	}
	return "", fmt.Errorf("--%s is required", name)
}

func requirePositional(positionals []string, index int, label string) (string, error) {
	if index >= len(positionals) || positionals[index] == "" {
		return "", fmt.Errorf("%s is required", label)
	}
	return positionals[index], nil
}

func parseActionPayload(positionals []string) (map[string]any, error) {
	if len(positionals) == 0 {
		return nil, errors.New("actionType is required")
	}
	actionType := positionals[0]
	switch actionType {
	case "bet", "raise":
		if len(positionals) < 2 {
			return nil, errors.New("totalSats is required")
		}
		totalSats, err := parseRequiredNumber(positionals[1], "totalSats")
		if err != nil {
			return nil, err
		}
		return map[string]any{"type": actionType, "totalSats": totalSats}, nil
	case "fold", "check", "call":
		return map[string]any{"type": actionType}, nil
	default:
		return nil, fmt.Errorf("unsupported mesh action %s", actionType)
	}
}

func parseRequiredNumber(raw, label string) (int, error) {
	if raw == "" {
		return 0, fmt.Errorf("%s is required", label)
	}
	var value int
	if _, err := fmt.Sscanf(raw, "%d", &value); err != nil {
		return 0, fmt.Errorf("%s must be numeric", label)
	}
	return value, nil
}

func parseOptionalNumber(raw string, fallback int) (int, error) {
	if raw == "" {
		return fallback, nil
	}
	return parseRequiredNumber(raw, "number")
}

func parseMode(flags parker.FlagMap) string {
	if value, ok := parker.FlagString(flags, "mode"); ok {
		switch value {
		case "player", "host", "witness", "indexer":
			return value
		}
	}
	return ""
}

func parseCSVFlag(flags parker.FlagMap, names ...string) []string {
	for _, name := range names {
		value, ok := parker.FlagString(flags, name)
		if !ok {
			continue
		}
		parts := strings.Split(value, ",")
		items := make([]string, 0, len(parts))
		for _, part := range parts {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			items = append(items, part)
		}
		if len(items) > 0 {
			return items
		}
	}
	return nil
}

func printHelp() {
	_, _ = fmt.Fprint(os.Stdout, strings.Join([]string{
		"parker-cli commands:",
		"  bootstrap [nickname] [--wallet-nsec <nsec>] --profile <name>",
		"  wallet [nsec|summary|deposit <sats>|withdraw <sats> <invoice>|faucet <sats>|onboard|offboard <address> [sats]] --profile <name>",
		"  network peers|bootstrap add <endpoint> [alias] --profile <name>",
		"  table create [--name <name>] [--visibility <public|private>] [--public|--private] [--witness-peer-ids <id[,id]>] [--turn-timeout-mode <direct|chain-challenge>] | created [--cursor <cursor>] [--limit <n>] | announce [tableId] | join <invite> [buyIn] | public | watch [tableId] | rotate-host [tableId] | action <fold|check|call|bet|raise> [sats] [--table-id <id>] --profile <name>",
		"  funds buy-in <invite> [buyIn] | cashout [tableId] | renew [tableId] | exit [tableId] --profile <name>",
		"  daemon <start|status|stop|watch> --profile <name> [--mode <player|host|witness|indexer>]",
		"  interactive --profile <name>",
		"Shared flags:",
		"  --network <regtest|mutinynet> --indexer-url <url> --ark-server-url <url> --boltz-url <url> --peer-host <host> --peer-port <port> --use-tor --tor-socks-addr <addr> --tor-control-addr <addr> --tor-cookie-auth <path|auto> --tor-target-host <host> --gossip-bootstrap-peers <csv> --mailbox-endpoints <csv> --mock --json",
		"",
	}, "\n"))
}

func resolveTableVisibility(flags parker.FlagMap) (string, error) {
	visibility, hasVisibility := parker.FlagString(flags, "visibility")
	if hasVisibility {
		visibility = strings.ToLower(strings.TrimSpace(visibility))
		switch visibility {
		case "public", "private":
		default:
			return "", fmt.Errorf("visibility must be public or private")
		}
	}

	publicFlag := parker.FlagBool(flags, "public")
	privateFlag := parker.FlagBool(flags, "private")
	if publicFlag && privateFlag {
		return "", fmt.Errorf("cannot set both --public and --private")
	}
	if hasVisibility {
		if publicFlag && visibility != "public" {
			return "", fmt.Errorf("--public conflicts with --visibility=%s", visibility)
		}
		if privateFlag && visibility != "private" {
			return "", fmt.Errorf("--private conflicts with --visibility=%s", visibility)
		}
		return visibility, nil
	}
	if publicFlag {
		return "public", nil
	}
	if privateFlag {
		return "private", nil
	}
	return "", nil
}
