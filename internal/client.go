package parker

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Client struct {
	Config      RuntimeConfig
	ProfileName string

	cacheMu     sync.Mutex
	cachedState map[string]any
}

type WatchSession struct {
	Events       <-chan EventEnvelope
	Done         <-chan error
	InitialState map[string]any
	Stop         func()
}

const daemonStartupTimeout = 30 * time.Second
const existingDaemonReachabilityTimeout = 5 * time.Second

func NewClient(profileName string, config RuntimeConfig) *Client {
	return &Client{
		Config:      config,
		ProfileName: profileName,
		cachedState: map[string]any{},
	}
}

func (c *Client) Close() {}

func (c *Client) CurrentState() map[string]any {
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()

	if len(c.cachedState) == 0 {
		return map[string]any{}
	}

	clone := map[string]any{}
	for key, value := range c.cachedState {
		clone[key] = value
	}
	return clone
}

func (c *Client) EnsureRunning(mode string) error {
	if c.isReachable() {
		return nil
	}
	paths := BuildProfileDaemonPaths(c.Config.DaemonDir, c.ProfileName)
	if metadata, err := ReadProfileDaemonMetadata(paths); err == nil && metadata != nil && IsPidAlive(metadata.PID) {
		return c.waitForReachable(existingDaemonReachabilityTimeout)
	}
	if err := c.startDaemonProcess(mode); err != nil {
		return err
	}
	return c.waitForReachable(daemonStartupTimeout)
}

func (c *Client) Inspect(autoStart bool) (map[string]any, error) {
	paths := BuildProfileDaemonPaths(c.Config.DaemonDir, c.ProfileName)
	metadata, err := ReadProfileDaemonMetadata(paths)
	if err != nil {
		return nil, err
	}

	reachable := false
	if autoStart {
		reachable = c.EnsureRunning("") == nil
	} else {
		reachable = c.isReachable()
	}

	var state any
	if reachable {
		state, err = c.Request("status", nil, autoStart)
		if err != nil {
			return nil, err
		}
	}

	return map[string]any{
		"metadata":  metadata,
		"reachable": reachable,
		"state":     state,
	}, nil
}

func (c *Client) StopDaemon() error {
	_, err := c.Request("stop", nil, false)
	return err
}

func (c *Client) Request(method string, params any, autoStart bool) (any, error) {
	request := RequestEnvelope{
		ID:     randomUUID(),
		Method: method,
		Type:   "request",
	}
	if params != nil {
		request.Params = MustMarshalJSON(params)
	}

	response, err := c.ForwardRequest(request, autoStart)
	if err != nil {
		return nil, err
	}
	if !response.OK {
		if response.Error == "" {
			return nil, fmt.Errorf("daemon request %s failed", method)
		}
		return nil, errors.New(response.Error)
	}
	if len(response.Result) == 0 {
		return nil, nil
	}

	var decoded any
	if err := json.Unmarshal(response.Result, &decoded); err != nil {
		return nil, err
	}
	return decoded, nil
}

func (c *Client) ForwardRequest(request RequestEnvelope, autoStart bool) (ResponseEnvelope, error) {
	if autoStart {
		if err := c.EnsureRunning(""); err != nil {
			return ResponseEnvelope{}, err
		}
	}

	connection, err := c.connectSocket()
	if err != nil {
		return ResponseEnvelope{}, err
	}
	defer connection.Close()

	timeout := daemonRequestTimeout(request.Method)
	if err := connection.SetDeadline(time.Now().Add(timeout)); err != nil {
		return ResponseEnvelope{}, err
	}

	if err := writeJSONLine(connection, request); err != nil {
		return ResponseEnvelope{}, err
	}

	reader := bufio.NewReader(connection)
	for {
		line, err := readTrimmedLine(reader)
		if err != nil {
			if isTimeoutError(err) {
				return ResponseEnvelope{}, fmt.Errorf("daemon request %s timed out after %dms", request.Method, timeout.Milliseconds())
			}
			return ResponseEnvelope{}, err
		}
		if line == "" {
			continue
		}

		var base struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(line), &base); err != nil {
			return ResponseEnvelope{}, err
		}
		if base.Type == "event" {
			continue
		}

		var response ResponseEnvelope
		if err := json.Unmarshal([]byte(line), &response); err != nil {
			return ResponseEnvelope{}, err
		}
		if response.ID != request.ID {
			continue
		}
		c.updateCachedStateFromRaw(response.Result)
		return response, nil
	}
}

func (c *Client) StartWatch(autoStart bool) (*WatchSession, error) {
	if autoStart {
		if err := c.EnsureRunning(""); err != nil {
			return nil, err
		}
	}

	connection, err := c.connectSocket()
	if err != nil {
		return nil, err
	}

	request := RequestEnvelope{
		ID:     randomUUID(),
		Method: "watch",
		Type:   "request",
	}
	if err := writeJSONLine(connection, request); err != nil {
		connection.Close()
		return nil, err
	}

	reader := bufio.NewReader(connection)
	if err := connection.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		connection.Close()
		return nil, err
	}

	var response ResponseEnvelope
	for {
		line, err := readTrimmedLine(reader)
		if err != nil {
			connection.Close()
			if isTimeoutError(err) {
				return nil, errors.New("timed out waiting for daemon watch acknowledgement")
			}
			return nil, err
		}
		if line == "" {
			continue
		}

		var candidate ResponseEnvelope
		if err := json.Unmarshal([]byte(line), &candidate); err != nil {
			connection.Close()
			return nil, err
		}
		if candidate.Type != "response" || candidate.ID != request.ID {
			continue
		}
		response = candidate
		break
	}

	if !response.OK {
		connection.Close()
		return nil, errors.New(response.Error)
	}

	if err := connection.SetReadDeadline(time.Time{}); err != nil {
		connection.Close()
		return nil, err
	}

	initialState := decodeMap(response.Result)
	c.updateCachedStateFromRaw(response.Result)

	events := make(chan EventEnvelope, 32)
	done := make(chan error, 1)
	var stopOnce sync.Once

	go func() {
		defer close(events)
		for {
			line, err := readTrimmedLine(reader)
			if err != nil {
				if errors.Is(err, io.EOF) || isClosedConnError(err) {
					done <- nil
					close(done)
					return
				}
				done <- err
				close(done)
				return
			}
			if line == "" {
				continue
			}

			var event EventEnvelope
			if err := json.Unmarshal([]byte(line), &event); err != nil {
				done <- err
				close(done)
				return
			}
			if event.Type != "event" {
				continue
			}
			if event.Event == "state" {
				c.updateCachedStateFromRaw(event.Payload)
			}
			events <- event
		}
	}()

	stop := func() {
		stopOnce.Do(func() {
			_ = connection.Close()
		})
	}

	return &WatchSession{
		Events:       events,
		Done:         done,
		InitialState: initialState,
		Stop:         stop,
	}, nil
}

func (c *Client) connectSocket() (net.Conn, error) {
	paths := BuildProfileDaemonPaths(c.Config.DaemonDir, c.ProfileName)
	return net.DialTimeout("unix", paths.SocketPath, 2*time.Second)
}

func (c *Client) isReachable() bool {
	request := RequestEnvelope{
		ID:     randomUUID(),
		Method: "ping",
		Type:   "request",
	}
	response, err := c.ForwardRequest(request, false)
	return err == nil && response.OK
}

func (c *Client) startDaemonProcess(mode string) error {
	paths := BuildProfileDaemonPaths(c.Config.DaemonDir, c.ProfileName)
	metadata, err := ReadProfileDaemonMetadata(paths)
	if err != nil {
		return err
	}
	if metadata != nil && !IsPidAlive(metadata.PID) {
		if err := CleanupDaemonArtifacts(paths); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(paths.StateDir, 0o755); err != nil {
		return err
	}

	launcherPath, err := ResolveLauncherPath("parker-daemon")
	if err != nil {
		return err
	}
	args := []string{"--profile", c.ProfileName}
	if mode != "" {
		args = append(args, "--mode", mode)
	}

	command := exec.Command(launcherPath, args...)
	command.Dir = startDirectory()
	command.Env = c.startEnvironment()
	command.Stdin = nil
	logFile, err := os.OpenFile(paths.LogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	command.Stdout = logFile
	command.Stderr = logFile
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		_ = logFile.Close()
		return err
	}
	_ = logFile.Close()
	return command.Process.Release()
}

func (c *Client) startEnvironment() []string {
	environment := append([]string{}, os.Environ()...)
	rootDir := startDirectory()
	environment = append(environment,
		"PARKER_NETWORK="+c.Config.Network,
		"PARKER_ARK_SERVER_URL="+c.Config.ArkServerURL,
		"PARKER_BOLTZ_URL="+c.Config.BoltzAPIURL,
		"PARKER_NIGIRI_BIN="+filepath.Join(rootDir, "scripts", "bin", "nigiri"),
		"PARKER_USE_MOCK_SETTLEMENT="+fmt.Sprintf("%t", c.Config.UseMockSettlement),
		"PARKER_USE_TOR="+fmt.Sprintf("%t", c.Config.UseTor),
		"PARKER_DATADIR="+c.Config.DataDir,
		"PARKER_DB_TYPE="+c.Config.CoreDBType,
		"PARKER_DB_DSN="+c.Config.CoreDBDSN,
		"PARKER_EVENT_DB_TYPE="+c.Config.EventDBType,
		"PARKER_EVENT_DB_DSN="+c.Config.EventDBDSN,
		"PARKER_CACHE_TYPE="+c.Config.CacheType,
		"PARKER_PROFILE_DIR="+c.Config.ProfileDir,
		"PARKER_DAEMON_DIR="+c.Config.DaemonDir,
		"PARKER_PEER_HOST="+c.Config.PeerHost,
		"PARKER_PEER_PORT="+strconv.Itoa(c.Config.PeerPort),
		"PARKER_TOR_SOCKS_ADDR="+c.Config.TorSocksAddr,
		"PARKER_TOR_CONTROL_ADDR="+c.Config.TorControlAddr,
		"PARKER_GOSSIP_ONION_PORT="+strconv.Itoa(c.Config.GossipOnionPort),
		"PARKER_DIRECT_ONION_PORT="+strconv.Itoa(c.Config.DirectOnionPort),
		"PARKER_RUN_DIR="+c.Config.RunDir,
	)
	if c.Config.TorCookieAuth != "" {
		environment = append(environment, "PARKER_TOR_COOKIE_AUTH="+c.Config.TorCookieAuth)
	}
	if c.Config.TorTargetHost != "" {
		environment = append(environment, "PARKER_TOR_TARGET_HOST="+c.Config.TorTargetHost)
	}
	if len(c.Config.GossipBootstrap) > 0 {
		environment = append(environment, "PARKER_GOSSIP_BOOTSTRAP_PEERS="+strings.Join(c.Config.GossipBootstrap, ","))
	}
	if len(c.Config.MailboxEndpoints) > 0 {
		environment = append(environment, "PARKER_MAILBOX_ENDPOINTS="+strings.Join(c.Config.MailboxEndpoints, ","))
	}
	if c.Config.IndexerURL != "" {
		environment = append(environment, "PARKER_INDEXER_URL="+c.Config.IndexerURL)
	}
	if c.Config.NigiriDatadir != "" {
		environment = append(environment, "PARKER_NIGIRI_DATADIR="+c.Config.NigiriDatadir)
	}
	if c.Config.CacheRedisAddr != "" {
		environment = append(environment, "PARKER_CACHE_REDIS_ADDR="+c.Config.CacheRedisAddr)
	}
	if c.Config.CacheRedisPass != "" {
		environment = append(environment, "PARKER_CACHE_REDIS_PASSWORD="+c.Config.CacheRedisPass)
	}
	if c.Config.CacheRedisDB != 0 {
		environment = append(environment, "PARKER_CACHE_REDIS_DB="+strconv.Itoa(c.Config.CacheRedisDB))
	}
	return environment
}

func (c *Client) waitForReachable(timeout time.Duration) error {
	paths := BuildProfileDaemonPaths(c.Config.DaemonDir, c.ProfileName)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		if c.isReachable() {
			return nil
		}
		select {
		case <-ctx.Done():
			details := startFailureDetails(paths)
			if details == "" {
				return fmt.Errorf("timed out waiting for daemon for profile %s", c.ProfileName)
			}
			return fmt.Errorf("timed out waiting for daemon for profile %s: %s", c.ProfileName, details)
		case <-ticker.C:
		}
	}
}

func startFailureDetails(paths ProfileDaemonPaths) string {
	details := make([]string, 0, 2)
	if metadata, err := ReadProfileDaemonMetadata(paths); err == nil && metadata != nil {
		details = append(details, fmt.Sprintf("metadata status=%s pid=%d socket=%s", metadata.Status, metadata.PID, metadata.SocketPath))
	}
	if logTail := tailLogFile(paths.LogPath, 8); logTail != "" {
		details = append(details, "log="+strconv.Quote(logTail))
	}
	return strings.Join(details, "; ")
}

func tailLogFile(path string, maxLines int) string {
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	return strings.Join(lines, " | ")
}

func (c *Client) updateCachedStateFromRaw(raw json.RawMessage) {
	if len(raw) == 0 {
		return
	}
	var candidate map[string]any
	if err := json.Unmarshal(raw, &candidate); err != nil {
		return
	}
	if _, hasMesh := candidate["mesh"]; !hasMesh {
		if _, hasTransport := candidate["transport"]; !hasTransport {
			if _, hasWallet := candidate["wallet"]; !hasWallet {
				return
			}
		}
	}

	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()
	nextState := map[string]any{}
	for key, value := range c.cachedState {
		nextState[key] = value
	}
	for key, value := range candidate {
		nextState[key] = value
	}
	c.cachedState = nextState
}

func readTrimmedLine(reader *bufio.Reader) (string, error) {
	line, err := reader.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func writeJSONLine(writer io.Writer, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if _, err := writer.Write(append(data, '\n')); err != nil {
		return err
	}
	return nil
}

func decodeMap(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return map[string]any{}
	}
	var value map[string]any
	if err := json.Unmarshal(raw, &value); err != nil || value == nil {
		return map[string]any{}
	}
	return value
}

func isTimeoutError(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func isClosedConnError(err error) bool {
	return errors.Is(err, net.ErrClosed) || strings.Contains(strings.ToLower(err.Error()), "use of closed network connection")
}

func daemonRequestTimeout(method string) time.Duration {
	switch method {
	case "walletOnboard":
		// Give boarding funds time to appear, then allow a second phase for onboarding retries.
		return 130 * time.Second
	default:
		return DaemonRequestTimeoutMS * time.Millisecond
	}
}

func startDirectory() string {
	if root, err := FindRepoRoot(); err == nil {
		return root
	}
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return "."
}

func randomUUID() string {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		panic(err)
	}
	bytes[6] = (bytes[6] & 0x0f) | 0x40
	bytes[8] = (bytes[8] & 0x3f) | 0x80

	encoded := hex.EncodeToString(bytes[:])
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		encoded[0:8],
		encoded[8:12],
		encoded[12:16],
		encoded[16:20],
		encoded[20:32],
	)
}
