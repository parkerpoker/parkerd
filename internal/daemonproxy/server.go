package daemonproxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/danieldresner/arkade_fun/internal/compat"
	"github.com/danieldresner/arkade_fun/internal/rpc"
)

const GoStartupBanner = "parker-daemon-go proxy started"

type Server struct {
	BackendDaemonDir string
	BackendPaths     compat.ProfileDaemonPaths
	Config           compat.Config
	Mode             string
	Paths            compat.ProfileDaemonPaths
	Profile          string
	RepoRoot         string

	backendCmdMu sync.Mutex
	backendCmd   *exec.Cmd

	cachedStateMu sync.RWMutex
	cachedState   json.RawMessage

	listener net.Listener

	metadataMu sync.Mutex
	metadata   compat.ProfileDaemonMetadata

	stopCh    chan struct{}
	stoppedCh chan struct{}
	stopOnce  sync.Once

	watchersMu sync.Mutex
	watchers   map[*socketWriter]struct{}
}

type socketWriter struct {
	conn net.Conn
	mu   sync.Mutex
}

func NewServer(profile string, config compat.Config, mode string) (*Server, error) {
	repoRoot, err := compat.FindRepoRoot()
	if err != nil {
		return nil, err
	}

	paths := compat.BuildProfileDaemonPaths(config.DaemonDir, profile)
	if err := os.MkdirAll(paths.StateDir, 0o755); err != nil {
		return nil, fmt.Errorf("create daemon state dir: %w", err)
	}
	backendDaemonDir := filepath.Join(paths.StateDir, "backend-daemons")
	if err := os.MkdirAll(backendDaemonDir, 0o755); err != nil {
		return nil, fmt.Errorf("create backend daemon dir: %w", err)
	}

	return &Server{
		BackendDaemonDir: backendDaemonDir,
		BackendPaths:     compat.BuildProfileDaemonPaths(backendDaemonDir, profile),
		Config:           config,
		Mode:             mode,
		Paths:            paths,
		Profile:          profile,
		RepoRoot:         repoRoot,
		cachedState:      json.RawMessage(`{}`),
		stopCh:           make(chan struct{}),
		stoppedCh:        make(chan struct{}),
		watchers:         make(map[*socketWriter]struct{}),
	}, nil
}

func (server *Server) Start() error {
	if existing, err := compat.ReadProfileDaemonMetadata(server.Paths); err == nil && existing != nil && existing.PID != os.Getpid() && !compat.IsPidAlive(existing.PID) {
		_ = compat.CleanupProfileDaemonArtifacts(server.Paths)
	}

	listener, err := net.Listen("unix", server.Paths.SocketPath)
	if err != nil {
		return err
	}
	server.listener = listener

	now := time.Now().UTC().Format(time.RFC3339)
	server.metadata = compat.ProfileDaemonMetadata{
		LastHeartbeat: now,
		LogPath:       server.Paths.LogPath,
		Mode:          server.Mode,
		PID:           os.Getpid(),
		Profile:       server.Profile,
		SocketPath:    server.Paths.SocketPath,
		StartedAt:     now,
		Status:        "starting",
	}
	if err := server.writeMetadata(); err != nil {
		return err
	}
	if err := server.appendLogEnvelope(map[string]any{
		"level":   "info",
		"message": GoStartupBanner,
		"scope":   "parker-daemon-go",
		"data": map[string]any{
			"backendImplementation": "ts",
			"mode":                  server.Mode,
			"profile":               server.Profile,
		},
	}); err != nil {
		return err
	}

	if err := server.startBackendProcess(); err != nil {
		return err
	}

	go server.acceptLoop()
	go server.runHeartbeat()
	go server.bridgeBackendWatch()
	return nil
}

func (server *Server) Wait() {
	<-server.stoppedCh
}

func (server *Server) Stop() {
	server.stopOnce.Do(func() {
		close(server.stopCh)
		server.setMetadataStatus("stopping")

		if server.listener != nil {
			_ = server.listener.Close()
		}

		server.watchersMu.Lock()
		for watcher := range server.watchers {
			_ = watcher.Close()
		}
		server.watchers = map[*socketWriter]struct{}{}
		server.watchersMu.Unlock()

		server.stopBackendProcess()
		_ = compat.CleanupProfileDaemonArtifacts(server.Paths)
		close(server.stoppedCh)
	})
}

func (server *Server) acceptLoop() {
	for {
		connection, err := server.listener.Accept()
		if err != nil {
			select {
			case <-server.stopCh:
				return
			default:
			}
			if strings.Contains(err.Error(), "use of closed network connection") {
				return
			}
			continue
		}
		go server.handleConnection(connection)
	}
}

func (server *Server) handleConnection(connection net.Conn) {
	writer := &socketWriter{conn: connection}
	defer func() {
		server.removeWatcher(writer)
		_ = writer.Close()
	}()

	buffer := make([]byte, 0, 4096)
	readBuffer := make([]byte, 4096)
	for {
		count, err := connection.Read(readBuffer)
		if err != nil {
			return
		}
		buffer = append(buffer, readBuffer[:count]...)
		for {
			newlineIndex := indexByte(buffer, '\n')
			if newlineIndex == -1 {
				break
			}
			line := trimLine(buffer[:newlineIndex])
			buffer = buffer[newlineIndex+1:]
			if len(line) == 0 {
				continue
			}

			var request rpc.RequestEnvelope
			if err := json.Unmarshal(line, &request); err != nil {
				return
			}
			response := server.dispatch(request, writer)
			if err := writer.WriteJSON(response); err != nil {
				return
			}
		}
	}
}

func (server *Server) dispatch(request rpc.RequestEnvelope, writer *socketWriter) rpc.ResponseEnvelope {
	switch request.Method {
	case "ping":
		return okResponse(request.ID, json.RawMessage(`{"ok":true}`))
	case "status":
		if result, ok := server.fetchBackendStatus(); ok {
			return okResponse(request.ID, result)
		}
		return okResponse(request.ID, server.currentState())
	case "watch":
		server.watchersMu.Lock()
		server.watchers[writer] = struct{}{}
		server.watchersMu.Unlock()
		return okResponse(request.ID, server.currentState())
	case "stop":
		go server.Stop()
		return okResponse(request.ID, json.RawMessage(`{"stopping":true}`))
	default:
		response, err := server.forwardRequest(request, 60*time.Second)
		if err != nil {
			return errorResponse(request.ID, err)
		}
		return response
	}
}

func (server *Server) fetchBackendStatus() (json.RawMessage, bool) {
	response, err := rpc.Call(server.BackendPaths.SocketPath, rpc.RequestEnvelope{
		ID:     compat.NewRequestID(),
		Method: "status",
		Type:   "request",
	}, 5*time.Second)
	if err != nil || !response.OK {
		return nil, false
	}
	server.updateState(response.Result)
	return response.Result, true
}

func (server *Server) forwardRequest(request rpc.RequestEnvelope, timeout time.Duration) (rpc.ResponseEnvelope, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		response, err := rpc.Call(server.BackendPaths.SocketPath, request, timeout)
		if err == nil {
			if response.OK && len(response.Result) > 0 {
				if request.Method == "status" {
					server.updateState(response.Result)
				}
			}
			return response, nil
		}
		lastErr = err
		select {
		case <-server.stopCh:
			return rpc.ResponseEnvelope{}, errors.New("daemon is stopping")
		case <-time.After(100 * time.Millisecond):
		}
	}

	if lastErr == nil {
		lastErr = errors.New("backend request timed out")
	}
	return rpc.ResponseEnvelope{}, lastErr
}

func (server *Server) startBackendProcess() error {
	wrapperPath := filepath.Join(server.RepoRoot, "scripts", "bin", "parker-daemon")
	command := exec.Command(wrapperPath, "--profile", server.Profile, "--mode", server.Mode)
	command.Dir = server.RepoRoot
	command.Env = compat.ApplyConfigEnv(os.Environ(), server.Config)
	command.Env = compat.SetEnvValue(command.Env, "PARKER_DAEMON_IMPL", "ts")
	command.Env = compat.SetEnvValue(command.Env, "PARKER_DAEMON_DIR", server.BackendDaemonDir)

	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	command.Stdin = devNull
	command.Stdout = devNull
	command.Stderr = devNull

	if err := command.Start(); err != nil {
		_ = devNull.Close()
		return err
	}

	server.backendCmdMu.Lock()
	server.backendCmd = command
	server.backendCmdMu.Unlock()

	go func() {
		defer devNull.Close()
		err := command.Wait()
		select {
		case <-server.stopCh:
			return
		default:
		}
		message := "backend daemon exited"
		if err != nil {
			message = fmt.Sprintf("backend daemon exited: %v", err)
		}
		_ = server.appendLogEnvelope(map[string]any{
			"level":   "error",
			"message": message,
			"scope":   "parker-daemon-go",
		})
		server.Stop()
	}()

	return nil
}

func (server *Server) stopBackendProcess() {
	_, _ = server.forwardRequest(rpc.RequestEnvelope{
		ID:     compat.NewRequestID(),
		Method: "stop",
		Type:   "request",
	}, 5*time.Second)

	server.backendCmdMu.Lock()
	defer server.backendCmdMu.Unlock()
	if server.backendCmd == nil || server.backendCmd.Process == nil {
		return
	}
	_ = server.backendCmd.Process.Signal(syscall.SIGTERM)
}

func (server *Server) bridgeBackendWatch() {
	for {
		select {
		case <-server.stopCh:
			return
		default:
		}

		request := rpc.RequestEnvelope{
			ID:     compat.NewRequestID(),
			Method: "watch",
			Type:   "request",
		}
		connection, ack, err := rpc.OpenWatch(server.BackendPaths.SocketPath, request, 5*time.Second)
		if err != nil {
			select {
			case <-server.stopCh:
				return
			case <-time.After(100 * time.Millisecond):
				continue
			}
		}

		if !ack.OK {
			_ = connection.Close()
			select {
			case <-server.stopCh:
				return
			case <-time.After(250 * time.Millisecond):
				continue
			}
		}

		server.updateState(ack.Result)
		server.setMetadataStatus("running")

		for {
			raw, err := connection.ReadRawLine()
			if err != nil {
				_ = connection.Close()
				break
			}
			event, err := rpc.ParseEvent(raw)
			if err != nil {
				continue
			}
			switch event.Event {
			case "state":
				server.updateState(event.Payload)
			case "log":
				_ = server.appendRawLog(event.Payload)
			}
			server.broadcastRawEvent(raw)
		}

		select {
		case <-server.stopCh:
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (server *Server) runHeartbeat() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-server.stopCh:
			return
		case <-ticker.C:
			server.metadataMu.Lock()
			server.metadata.LastHeartbeat = time.Now().UTC().Format(time.RFC3339)
			metadata := server.metadata
			server.metadataMu.Unlock()
			_ = compat.WriteProfileDaemonMetadata(server.Paths, metadata)
		}
	}
}

func (server *Server) currentState() json.RawMessage {
	server.cachedStateMu.RLock()
	defer server.cachedStateMu.RUnlock()
	return cloneRawMessage(server.cachedState)
}

func (server *Server) updateState(raw json.RawMessage) {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	server.cachedStateMu.Lock()
	server.cachedState = cloneRawMessage(raw)
	server.cachedStateMu.Unlock()

	var state rpc.RuntimeState
	if err := json.Unmarshal(raw, &state); err != nil {
		return
	}

	server.metadataMu.Lock()
	if state.Mesh != nil && state.Mesh.Peer != nil {
		if state.Mesh.Peer.PeerID != "" {
			server.metadata.PeerID = state.Mesh.Peer.PeerID
		}
		if state.Mesh.Peer.PeerURL != "" {
			server.metadata.PeerURL = state.Mesh.Peer.PeerURL
		}
		if state.Mesh.Peer.ProtocolID != "" {
			server.metadata.ProtocolID = state.Mesh.Peer.ProtocolID
		}
	}
	metadata := server.metadata
	server.metadataMu.Unlock()
	_ = compat.WriteProfileDaemonMetadata(server.Paths, metadata)
}

func (server *Server) setMetadataStatus(status string) {
	server.metadataMu.Lock()
	server.metadata.Status = status
	if status == "stopping" {
		server.metadata.LastHeartbeat = time.Now().UTC().Format(time.RFC3339)
	}
	metadata := server.metadata
	server.metadataMu.Unlock()
	_ = compat.WriteProfileDaemonMetadata(server.Paths, metadata)
}

func (server *Server) writeMetadata() error {
	server.metadataMu.Lock()
	metadata := server.metadata
	server.metadataMu.Unlock()
	return compat.WriteProfileDaemonMetadata(server.Paths, metadata)
}

func (server *Server) appendLogEnvelope(payload map[string]any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return server.appendRawLog(raw)
}

func (server *Server) appendRawLog(raw []byte) error {
	if len(raw) == 0 {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(server.Paths.LogPath), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(server.Paths.LogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := file.Write(raw); err != nil {
		return err
	}
	_, err = file.Write([]byte{'\n'})
	return err
}

func (server *Server) broadcastRawEvent(raw []byte) {
	server.watchersMu.Lock()
	defer server.watchersMu.Unlock()
	for watcher := range server.watchers {
		if err := watcher.WriteRawLine(raw); err != nil {
			_ = watcher.Close()
			delete(server.watchers, watcher)
		}
	}
}

func (server *Server) removeWatcher(writer *socketWriter) {
	server.watchersMu.Lock()
	delete(server.watchers, writer)
	server.watchersMu.Unlock()
}

func (writer *socketWriter) WriteJSON(value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return writer.WriteRawLine(payload)
}

func (writer *socketWriter) WriteRawLine(payload []byte) error {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if _, err := writer.conn.Write(payload); err != nil {
		return err
	}
	_, err := writer.conn.Write([]byte{'\n'})
	return err
}

func (writer *socketWriter) Close() error {
	return writer.conn.Close()
}

func okResponse(id string, result json.RawMessage) rpc.ResponseEnvelope {
	return rpc.ResponseEnvelope{
		ID:     id,
		OK:     true,
		Result: result,
		Type:   "response",
	}
}

func errorResponse(id string, err error) rpc.ResponseEnvelope {
	return rpc.ResponseEnvelope{
		Error: err.Error(),
		ID:    id,
		OK:    false,
		Type:  "response",
	}
}

func cloneRawMessage(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(`{}`)
	}
	copyValue := make([]byte, len(raw))
	copy(copyValue, raw)
	return copyValue
}

func indexByte(buffer []byte, needle byte) int {
	for index, value := range buffer {
		if value == needle {
			return index
		}
	}
	return -1
}

func trimLine(raw []byte) []byte {
	for len(raw) > 0 {
		last := raw[len(raw)-1]
		if last != '\n' && last != '\r' {
			break
		}
		raw = raw[:len(raw)-1]
	}
	return raw
}
