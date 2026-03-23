package daemonproxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/danieldresner/arkade_fun/internal/compat"
	"github.com/danieldresner/arkade_fun/internal/rpc"
)

type Client struct {
	Config  compat.Config
	Profile string
}

const daemonStartupTimeout = 30 * time.Second

type InspectResult struct {
	Metadata  *compat.ProfileDaemonMetadata `json:"metadata"`
	Reachable bool                          `json:"reachable"`
	State     json.RawMessage               `json:"state"`
}

func NewClient(profile string, config compat.Config) *Client {
	return &Client{
		Config:  config,
		Profile: profile,
	}
}

func (client *Client) EnsureRunning(mode string) error {
	reachable, err := client.isReachable(false)
	if err == nil && reachable {
		return nil
	}

	paths := client.paths()
	if metadata, readErr := compat.ReadProfileDaemonMetadata(paths); readErr == nil && metadata != nil && !compat.IsPidAlive(metadata.PID) {
		_ = compat.CleanupProfileDaemonArtifacts(paths)
	}

	repoRoot, err := compat.FindRepoRoot()
	if err != nil {
		return err
	}
	wrapperPath := filepath.Join(repoRoot, "scripts", "bin", "parker-daemon")
	command := exec.Command(wrapperPath, "--profile", client.Profile)
	if mode != "" {
		command.Args = append(command.Args, "--mode", mode)
	}
	command.Dir = repoRoot
	command.Env = compat.ApplyConfigEnv(os.Environ(), client.Config)

	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer devNull.Close()

	command.Stdin = devNull
	command.Stdout = devNull
	command.Stderr = devNull
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		return err
	}
	_ = command.Process.Release()

	deadline := time.Now().Add(daemonStartupTimeout)
	for time.Now().Before(deadline) {
		reachable, err = client.isReachable(false)
		if err == nil && reachable {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for daemon for profile %s", client.Profile)
}

func (client *Client) Inspect(autoStart bool) (InspectResult, error) {
	metadata, err := compat.ReadProfileDaemonMetadata(client.paths())
	if err != nil {
		return InspectResult{}, err
	}

	reachable, err := client.isReachable(autoStart)
	if err != nil {
		reachable = false
	}

	state := json.RawMessage("null")
	if reachable {
		raw, err := client.Request("status", nil, autoStart)
		if err != nil {
			return InspectResult{}, err
		}
		state = raw
	}

	return InspectResult{
		Metadata:  metadata,
		Reachable: reachable,
		State:     state,
	}, nil
}

func (client *Client) Request(method string, params map[string]any, autoStart bool) (json.RawMessage, error) {
	if autoStart {
		if err := client.EnsureRunning(""); err != nil {
			return nil, err
		}
	}
	response, err := rpc.Call(client.paths().SocketPath, rpc.RequestEnvelope{
		ID:     compat.NewRequestID(),
		Method: method,
		Params: params,
		Type:   "request",
	}, 60*time.Second)
	if err != nil {
		return nil, err
	}
	if !response.OK {
		return nil, errors.New(response.Error)
	}
	return response.Result, nil
}

func (client *Client) Watch(onEvent func(json.RawMessage) error) (func(), error) {
	if err := client.EnsureRunning(""); err != nil {
		return nil, err
	}

	connection, ack, err := rpc.OpenWatch(client.paths().SocketPath, rpc.RequestEnvelope{
		ID:     compat.NewRequestID(),
		Method: "watch",
		Type:   "request",
	}, 5*time.Second)
	if err != nil {
		return nil, err
	}
	if !ack.OK {
		_ = connection.Close()
		return nil, errors.New(ack.Error)
	}

	stop := func() {
		_ = connection.Close()
	}

	go func() {
		for {
			raw, readErr := connection.ReadRawLine()
			if readErr != nil {
				return
			}
			if messageType, typeErr := rpc.PeekMessageType(raw); typeErr == nil && messageType == "event" {
				_ = onEvent(json.RawMessage(raw))
			}
		}
	}()
	return stop, nil
}

func MarshalResultEnvelope(raw json.RawMessage) ([]byte, error) {
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

func FormatHumanReadable(raw json.RawMessage) string {
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

	formatted := &bytes.Buffer{}
	encoder := json.NewEncoder(formatted)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(decoded); err != nil {
		return string(raw)
	}
	return strings.TrimSuffix(formatted.String(), "\n")
}

func ParseActionPayload(positionals []string) (map[string]any, error) {
	if len(positionals) == 0 {
		return nil, errors.New("actionType is required")
	}
	actionType := positionals[0]
	switch actionType {
	case "bet", "raise":
		if len(positionals) < 2 {
			return nil, errors.New("totalSats is required")
		}
		totalSats, err := ParseRequiredNumber(positionals[1], "totalSats")
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"type":      actionType,
			"totalSats": totalSats,
		}, nil
	case "fold", "check", "call":
		return map[string]any{"type": actionType}, nil
	default:
		return nil, fmt.Errorf("unsupported mesh action %s", actionType)
	}
}

func ParseRequiredNumber(raw string, label string) (int, error) {
	if raw == "" {
		return 0, fmt.Errorf("%s is required", label)
	}
	value, err := strconvAtoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be numeric", label)
	}
	return value, nil
}

func ParseOptionalNumber(raw string, fallback int) (int, error) {
	if raw == "" {
		return fallback, nil
	}
	return ParseRequiredNumber(raw, "number")
}

func RequirePositional(positionals []string, index int, label string) (string, error) {
	if index >= len(positionals) || positionals[index] == "" {
		return "", fmt.Errorf("%s is required", label)
	}
	return positionals[index], nil
}

func RequireFlag(flags compat.FlagMap, name string, fallback string) (string, error) {
	if value, ok := flags.Lookup(name); ok && value != "" {
		return value, nil
	}
	if fallback != "" {
		return fallback, nil
	}
	return "", fmt.Errorf("--%s is required", name)
}

func ParseMode(flags compat.FlagMap) string {
	if value, ok := flags.Lookup("mode"); ok {
		switch value {
		case "player", "host", "witness", "indexer":
			return value
		}
	}
	return ""
}

func (client *Client) paths() compat.ProfileDaemonPaths {
	return compat.BuildProfileDaemonPaths(client.Config.DaemonDir, client.Profile)
}

func (client *Client) isReachable(autoStart bool) (bool, error) {
	if autoStart {
		if err := client.EnsureRunning(""); err != nil {
			return false, err
		}
	}
	response, err := rpc.Call(client.paths().SocketPath, rpc.RequestEnvelope{
		ID:     compat.NewRequestID(),
		Method: "ping",
		Type:   "request",
	}, 5*time.Second)
	if err != nil {
		return false, err
	}
	return response.OK, nil
}

func strconvAtoi(raw string) (int, error) {
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, err
	}
	return value, nil
}
