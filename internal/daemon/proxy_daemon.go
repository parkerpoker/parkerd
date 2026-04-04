package daemon

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	cfg "github.com/parkerpoker/parkerd/internal/config"
	"github.com/parkerpoker/parkerd/internal/game"
)

const NativeStartupBanner = "go parker daemon starting"

const daemonPollInterval = 500 * time.Millisecond

type ProxyDaemon struct {
	config      cfg.RuntimeConfig
	listener    net.Listener
	metadata    *cfg.ProfileDaemonMetadata
	mode        string
	paths       cfg.ProfileDaemonPaths
	profileName string
	readyCh     chan struct{}
	stopCh      chan struct{}
	stoppedCh   chan struct{}

	cacheMu     sync.RWMutex
	cachedState map[string]any

	metadataMu sync.Mutex
	readyOnce  sync.Once
	runtime    daemonRuntime
	stopOnce   sync.Once
	watchers   sync.Map
}

func NewProxyDaemon(profileName string, config cfg.RuntimeConfig, mode string) (*ProxyDaemon, error) {
	runtime, err := newDaemonRuntime(profileName, config, mode)
	if err != nil {
		return nil, err
	}
	return &ProxyDaemon{
		config:      config,
		mode:        mode,
		paths:       cfg.BuildProfileDaemonPaths(config.DaemonDir, profileName),
		profileName: profileName,
		readyCh:     make(chan struct{}),
		stopCh:      make(chan struct{}),
		stoppedCh:   make(chan struct{}),
		cachedState: map[string]any{},
		runtime:     runtime,
	}, nil
}

func (d *ProxyDaemon) Start() error {
	if existing, err := cfg.ReadProfileDaemonMetadata(d.paths); err == nil && existing != nil && existing.PID != os.Getpid() && !cfg.IsPidAlive(existing.PID) {
		if err := cfg.CleanupDaemonArtifacts(d.paths); err != nil {
			return err
		}
	}

	for _, dir := range []string{
		d.paths.StateDir,
		filepath.Dir(d.paths.LogPath),
		filepath.Dir(d.paths.SocketPath),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	listener, err := net.Listen("unix", d.paths.SocketPath)
	if err != nil {
		return err
	}
	d.listener = listener
	if err := os.MkdirAll(filepath.Dir(d.paths.MetadataPath), 0o755); err != nil {
		_ = listener.Close()
		return err
	}

	metadata := &cfg.ProfileDaemonMetadata{
		LastHeartbeat: timestampNow(),
		LogPath:       d.paths.LogPath,
		Mode:          d.mode,
		PID:           os.Getpid(),
		Profile:       d.profileName,
		SocketPath:    d.paths.SocketPath,
		StartedAt:     timestampNow(),
		Status:        "starting",
	}
	d.metadata = metadata
	if err := cfg.WriteProfileDaemonMetadata(d.paths, *metadata); err != nil {
		_ = listener.Close()
		return err
	}

	d.appendLogEnvelope(map[string]any{
		"level":   "info",
		"message": NativeStartupBanner,
		"scope":   "parker-daemon-go",
		"data": map[string]any{
			"mode":    d.mode,
			"profile": d.profileName,
		},
	})

	go d.acceptLoop()

	if err := d.runtime.Start(); err != nil {
		_ = d.Stop()
		return err
	}
	if state, err := d.runtime.QuickState(); err == nil {
		d.setCachedState(state)
	}
	d.readyOnce.Do(func() {
		close(d.readyCh)
	})
	if err := d.writeRunningMetadata(); err != nil {
		_ = d.Stop()
		return err
	}

	go d.runPoller()
	go d.runHeartbeat()
	return nil
}

func (d *ProxyDaemon) Wait() {
	<-d.stoppedCh
}

func (d *ProxyDaemon) Stop() error {
	var stopErr error
	d.stopOnce.Do(func() {
		close(d.stopCh)
		stopErr = d.markStopping()
		stopErr = errors.Join(stopErr, d.runtime.Close())
		if d.listener != nil {
			stopErr = errors.Join(stopErr, d.listener.Close())
		}
		d.watchers.Range(func(key, _ any) bool {
			if connection, ok := key.(net.Conn); ok {
				_ = connection.Close()
			}
			d.watchers.Delete(key)
			return true
		})
		stopErr = errors.Join(stopErr, cfg.CleanupDaemonArtifacts(d.paths))
		close(d.stoppedCh)
	})
	return stopErr
}

func (d *ProxyDaemon) acceptLoop() {
	for {
		connection, err := d.listener.Accept()
		if err != nil {
			select {
			case <-d.stopCh:
				return
			default:
			}
			continue
		}
		go d.handleConnection(connection)
	}
}

func (d *ProxyDaemon) handleConnection(connection net.Conn) {
	defer func() {
		d.watchers.Delete(connection)
		_ = connection.Close()
	}()

	reader := bufio.NewReader(connection)
	for {
		line, err := readTrimmedLine(reader)
		if err != nil {
			return
		}
		if line == "" {
			continue
		}

		var request RequestEnvelope
		if err := json.Unmarshal([]byte(line), &request); err != nil {
			continue
		}

		response := d.dispatch(request, connection)
		if err := writeJSONLine(connection, response); err != nil {
			return
		}
	}
}

func (d *ProxyDaemon) dispatch(request RequestEnvelope, connection net.Conn) ResponseEnvelope {
	switch request.Method {
	case "ping":
		return okResponse(request.ID, map[string]any{"ok": true})
	case "watch":
		d.watchers.Store(connection, struct{}{})
		return okResponse(request.ID, d.currentState())
	case "status":
		return okResponse(request.ID, d.currentState())
	case "stop":
		go func() {
			_ = d.Stop()
		}()
		return okResponse(request.ID, map[string]any{"stopping": true})
	}

	select {
	case <-d.readyCh:
	case <-d.stopCh:
		return errorResponse(request.ID, "daemon is stopping")
	}

	params := decodeMap(request.Params)
	result, err := d.handleRuntimeRequest(request.Method, params)
	if err != nil {
		d.appendLogEnvelope(map[string]any{
			"level":   "error",
			"message": "daemon request failed",
			"scope":   "parker-daemon-go",
			"data": map[string]any{
				"error":   err.Error(),
				"method":  request.Method,
				"profile": d.profileName,
			},
		})
		return errorResponse(request.ID, err.Error())
	}
	d.appendLogEnvelope(map[string]any{
		"level":   "info",
		"message": "daemon request handled",
		"scope":   "parker-daemon-go",
		"data": map[string]any{
			"method":  request.Method,
			"profile": d.profileName,
		},
	})
	go d.refreshCachedState()
	return okResponse(request.ID, result)
}

func (d *ProxyDaemon) handleRuntimeRequest(method string, params map[string]any) (any, error) {
	switch method {
	case "bootstrap":
		return d.runtime.Bootstrap(
			stringFromMap(params, "nickname", ""),
			stringFromMap(params, "walletNsec", ""),
		)
	case "walletNsec":
		return d.runtime.WalletNsec()
	case "walletSummary":
		return d.runtime.WalletSummary()
	case "walletFaucet":
		amount := intFromMap(params, "amountSats", 0)
		if amount <= 0 {
			return nil, errors.New("amountSats is required")
		}
		return d.runtime.WalletFaucet(amount)
	case "walletOnboard":
		return d.runtime.WalletOnboard()
	case "walletOffboard":
		address := stringFromMap(params, "address", "")
		if address == "" {
			return nil, errors.New("address is required")
		}
		var amountPtr *int
		if amount, ok := optionalInt(params, "amountSats"); ok {
			amountPtr = &amount
		}
		return d.runtime.WalletOffboard(address, amountPtr)
	case "walletDeposit":
		return d.runtime.WalletDeposit(intFromMap(params, "amountSats", 0))
	case "walletWithdraw":
		return d.runtime.WalletWithdraw(intFromMap(params, "amountSats", 0), stringFromMap(params, "invoice", ""))
	case "meshNetworkPeers":
		return d.runtime.NetworkPeers()
	case "meshBootstrapPeer":
		endpoint := firstNonEmpty(stringFromMap(params, "endpoint", ""), stringFromMap(params, "peerUrl", ""))
		return d.runtime.BootstrapPeer(endpoint, stringFromMap(params, "alias", ""), stringSliceFromMap(params, "roles"))
	case "meshCreateTable":
		tableParams := nestedMap(params, "table")
		return d.runtime.CreateTable(tableParams)
	case "meshCreatedTables":
		return d.runtime.CreatedTables(stringFromMap(params, "cursor", ""), intFromMap(params, "limit", 0))
	case "meshTableAnnounce":
		return d.runtime.AnnounceTable(stringFromMap(params, "tableId", d.runtime.currentTableID()))
	case "meshTableJoin":
		return d.runtime.JoinTable(stringFromMap(params, "inviteCode", ""), intFromMap(params, "buyInSats", 4000))
	case "meshGetTable":
		return d.runtime.GetTable(stringFromMap(params, "tableId", d.runtime.currentTableID()))
	case "meshSendAction":
		payload := nestedMap(params, "payload")
		tableID := stringFromMap(params, "tableId", d.runtime.currentTableID())
		action, err := decodeAction(payload)
		if err != nil {
			return nil, err
		}
		return d.runtime.SendAction(tableID, action)
	case "meshOpenTurnChallenge":
		return d.runtime.OpenTurnChallenge(stringFromMap(params, "tableId", d.runtime.currentTableID()))
	case "meshResolveTurnChallenge":
		return d.runtime.ResolveTurnChallenge(
			stringFromMap(params, "tableId", d.runtime.currentTableID()),
			stringFromMap(params, "optionId", ""),
		)
	case "meshRotateHost":
		return d.runtime.RotateHost(stringFromMap(params, "tableId", d.runtime.currentTableID()))
	case "meshPublicTables":
		return d.runtime.PublicTables()
	case "meshCashOut":
		return d.runtime.CashOut(stringFromMap(params, "tableId", d.runtime.currentTableID()))
	case "meshRenew":
		return d.runtime.Renew(stringFromMap(params, "tableId", d.runtime.currentTableID()))
	case "meshExit":
		return d.runtime.Exit(stringFromMap(params, "tableId", d.runtime.currentTableID()))
	default:
		return nil, fmt.Errorf("unsupported daemon method %s", method)
	}
}

func (d *ProxyDaemon) runPoller() {
	ticker := time.NewTicker(daemonPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-d.stopCh:
			return
		case <-ticker.C:
			d.runtime.Tick()
			state, err := d.runtime.CurrentState()
			if err != nil {
				continue
			}
			if d.stateChanged(state) {
				d.setCachedState(state)
				d.broadcast(EventEnvelope{
					Event:   "state",
					Payload: MustMarshalJSON(state),
					Type:    "event",
				})
			}
		}
	}
}

func (d *ProxyDaemon) runHeartbeat() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-d.stopCh:
			return
		case <-ticker.C:
			_ = d.writeHeartbeat()
		}
	}
}

func (d *ProxyDaemon) currentState() map[string]any {
	d.cacheMu.RLock()
	defer d.cacheMu.RUnlock()

	if len(d.cachedState) == 0 {
		return map[string]any{}
	}

	clone := map[string]any{}
	for key, value := range d.cachedState {
		clone[key] = value
	}
	return clone
}

func (d *ProxyDaemon) stateChanged(next map[string]any) bool {
	current := MustMarshalJSON(d.currentState())
	return string(current) != string(MustMarshalJSON(next))
}

func (d *ProxyDaemon) setCachedState(state map[string]any) {
	if state == nil {
		state = map[string]any{}
	}
	d.cacheMu.Lock()
	d.cachedState = state
	d.cacheMu.Unlock()
	_ = d.writeRunningMetadata()
}

func (d *ProxyDaemon) refreshCachedState() {
	state, err := d.runtime.CurrentState()
	if err != nil {
		return
	}
	d.setCachedState(state)
}

func (d *ProxyDaemon) writeRunningMetadata() error {
	d.metadataMu.Lock()
	defer d.metadataMu.Unlock()

	if d.metadata == nil {
		return nil
	}
	d.metadata.Mode = d.mode
	d.metadata.Status = "running"
	d.metadata.LastHeartbeat = timestampNow()

	if transport := normalizeStateMap(d.currentState()["transport"]); len(transport) > 0 {
		if peer := normalizeStateMap(transport["peer"]); len(peer) > 0 {
			d.metadata.PeerID = firstNonEmpty(stringValue(peer["peerId"]), d.metadata.PeerID)
			d.metadata.PeerEndpoint = firstNonEmpty(stringValue(peer["endpoint"]), d.metadata.PeerEndpoint)
			d.metadata.DirectOnion = firstNonEmpty(stringValue(peer["directOnion"]), d.metadata.DirectOnion)
			d.metadata.GossipOnion = firstNonEmpty(stringValue(peer["gossipOnion"]), d.metadata.GossipOnion)
			d.metadata.ProtocolID = firstNonEmpty(stringValue(peer["protocolId"]), d.metadata.ProtocolID)
		}
	}

	if mesh := normalizeStateMap(d.currentState()["mesh"]); len(mesh) > 0 {
		if peer := normalizeStateMap(mesh["peer"]); len(peer) > 0 {
			d.metadata.PeerID = firstNonEmpty(stringValue(peer["peerId"]), d.metadata.PeerID)
			d.metadata.PeerURL = firstNonEmpty(stringValue(peer["peerUrl"]), d.metadata.PeerURL)
			d.metadata.PeerEndpoint = firstNonEmpty(stringValue(peer["peerUrl"]), d.metadata.PeerEndpoint)
			d.metadata.ProtocolID = firstNonEmpty(stringValue(peer["protocolId"]), d.metadata.ProtocolID)
		}
		d.metadata.PeerID = firstNonEmpty(stringValue(mesh["peerId"]), d.metadata.PeerID)
		d.metadata.PeerURL = firstNonEmpty(stringValue(mesh["peerUrl"]), d.metadata.PeerURL)
		d.metadata.PeerEndpoint = firstNonEmpty(stringValue(mesh["peerUrl"]), d.metadata.PeerEndpoint)
		d.metadata.ProtocolID = firstNonEmpty(stringValue(mesh["protocolId"]), d.metadata.ProtocolID)
	}

	return cfg.WriteProfileDaemonMetadata(d.paths, *d.metadata)
}

func (d *ProxyDaemon) writeHeartbeat() error {
	d.metadataMu.Lock()
	defer d.metadataMu.Unlock()

	if d.metadata == nil {
		return nil
	}
	d.metadata.LastHeartbeat = timestampNow()
	return cfg.WriteProfileDaemonMetadata(d.paths, *d.metadata)
}

func (d *ProxyDaemon) markStopping() error {
	d.metadataMu.Lock()
	defer d.metadataMu.Unlock()

	if d.metadata == nil {
		return nil
	}
	d.metadata.Status = "stopping"
	d.metadata.LastHeartbeat = timestampNow()
	return cfg.WriteProfileDaemonMetadata(d.paths, *d.metadata)
}

func (d *ProxyDaemon) broadcast(event EventEnvelope) {
	data, err := json.Marshal(event)
	if err != nil {
		return
	}
	data = append(data, '\n')

	d.watchers.Range(func(key, _ any) bool {
		connection, ok := key.(net.Conn)
		if !ok {
			d.watchers.Delete(key)
			return true
		}
		if _, err := connection.Write(data); err != nil {
			_ = connection.Close()
			d.watchers.Delete(key)
		}
		return true
	})
}

func (d *ProxyDaemon) appendLogPayload(payload json.RawMessage) {
	if len(payload) == 0 {
		return
	}
	file, err := os.OpenFile(d.paths.LogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer file.Close()
	_, _ = file.Write(append(payload, '\n'))
	d.broadcast(EventEnvelope{
		Event:   "log",
		Payload: payload,
		Type:    "event",
	})
}

func (d *ProxyDaemon) appendLogEnvelope(payload map[string]any) {
	d.appendLogPayload(MustMarshalJSON(payload))
}

func okResponse(id string, result any) ResponseEnvelope {
	return ResponseEnvelope{
		ID:     id,
		OK:     true,
		Result: MustMarshalJSON(result),
		Type:   "response",
	}
}

func errorResponse(id string, message string) ResponseEnvelope {
	return ResponseEnvelope{
		Error: message,
		ID:    id,
		OK:    false,
		Type:  "response",
	}
}

func timestampNow() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

func normalizeStateMap(value any) map[string]any {
	if value == nil {
		return nil
	}
	if decoded, ok := value.(map[string]any); ok {
		return decoded
	}
	return decodeMap(MustMarshalJSON(value))
}

func nestedMap(input map[string]any, key string) map[string]any {
	if input == nil {
		return nil
	}
	if value, ok := input[key].(map[string]any); ok {
		return value
	}
	return nil
}

func optionalInt(input map[string]any, key string) (int, bool) {
	if input == nil {
		return 0, false
	}
	switch value := input[key].(type) {
	case float64:
		return int(value), true
	case int:
		return value, true
	case int64:
		return int(value), true
	}
	return 0, false
}

func decodeAction(input map[string]any) (game.Action, error) {
	actionType := game.ActionType(stringFromMap(input, "type", ""))
	switch actionType {
	case game.ActionFold, game.ActionCheck, game.ActionCall:
		return game.Action{Type: actionType}, nil
	case game.ActionBet, game.ActionRaise:
		total := intFromMap(input, "totalSats", 0)
		if total <= 0 {
			return game.Action{}, errors.New("totalSats is required for bet/raise")
		}
		return game.Action{Type: actionType, TotalSats: total}, nil
	default:
		return game.Action{}, fmt.Errorf("unsupported action type %q", actionType)
	}
}
