package controller

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	parker "github.com/danieldresner/arkade_fun/internal"
	cfg "github.com/danieldresner/arkade_fun/internal/config"
	walletpkg "github.com/danieldresner/arkade_fun/internal/wallet"
)

const testOrigin = "http://127.0.0.1:3010"

func TestControllerHeaderAndCORS(t *testing.T) {
	runtimeConfig := newTestRuntimeConfig(t)
	app := newTestControllerApp(t, runtimeConfig)

	request := httptest.NewRequest(http.MethodGet, "/api/local/profiles", nil)
	recorder := httptest.NewRecorder()
	app.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 without controller header, got %d", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), LocalControllerHeader) {
		t.Fatalf("expected controller header error, got %s", recorder.Body.String())
	}

	rejected := httptest.NewRequest(http.MethodGet, "/api/local/profiles", nil)
	rejected.Header.Set(LocalControllerHeader, "1")
	rejected.Header.Set("Origin", "http://evil.example")
	rejectedRecorder := httptest.NewRecorder()
	app.ServeHTTP(rejectedRecorder, rejected)
	if rejectedRecorder.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for rejected origin, got %d", rejectedRecorder.Code)
	}

	preflight := httptest.NewRequest(http.MethodOptions, "/api/local/profiles", nil)
	preflight.Header.Set("Origin", testOrigin)
	preflight.Header.Set("Access-Control-Request-Headers", LocalControllerHeader)
	preflightRecorder := httptest.NewRecorder()
	app.ServeHTTP(preflightRecorder, preflight)
	if preflightRecorder.Code != http.StatusNoContent {
		t.Fatalf("expected 204 preflight, got %d", preflightRecorder.Code)
	}
	if value := preflightRecorder.Header().Get("Access-Control-Allow-Origin"); value != testOrigin {
		t.Fatalf("expected allow origin %s, got %s", testOrigin, value)
	}
}

func TestControllerRoutesAndSSE(t *testing.T) {
	runtimeConfig := newTestRuntimeConfig(t)
	bootstrapProfile(t, runtimeConfig, "alice", "Alice")
	app := newTestControllerApp(t, runtimeConfig)

	profilesResponse := localRequest(t, app, http.MethodGet, "/api/local/profiles", nil)
	if profilesResponse.Code != http.StatusOK {
		t.Fatalf("profiles status = %d body=%s", profilesResponse.Code, profilesResponse.Body.String())
	}
	var profiles []walletpkg.LocalProfileSummary
	if err := json.Unmarshal(profilesResponse.Body.Bytes(), &profiles); err != nil {
		t.Fatalf("decode profiles: %v", err)
	}
	if len(profiles) != 1 || profiles[0].Nickname != "Alice" {
		t.Fatalf("unexpected profiles payload: %+v", profiles)
	}

	startResponse := localRequest(t, app, http.MethodPost, "/api/local/profiles/alice/daemon/start", map[string]any{})
	if startResponse.Code != http.StatusOK {
		t.Fatalf("daemon start status = %d body=%s", startResponse.Code, startResponse.Body.String())
	}
	var started LocalProfileStatusResponse
	if err := json.Unmarshal(startResponse.Body.Bytes(), &started); err != nil {
		t.Fatalf("decode daemon start: %v", err)
	}
	if !started.Daemon.Reachable {
		t.Fatalf("expected started daemon to be reachable")
	}

	bootstrapResponse := localRequest(t, app, http.MethodPost, "/api/local/profiles/alice/bootstrap", map[string]any{
		"nickname": "Alice Browser",
	})
	if bootstrapResponse.Code != http.StatusOK {
		t.Fatalf("bootstrap status = %d body=%s", bootstrapResponse.Code, bootstrapResponse.Body.String())
	}
	var bootstrapResult struct {
		Transport struct {
			Peer struct {
				Endpoint string `json:"endpoint"`
			} `json:"peer"`
		} `json:"transport"`
		Mesh struct {
			WalletPlayerID string `json:"walletPlayerId"`
		} `json:"mesh"`
	}
	if err := json.Unmarshal(bootstrapResponse.Body.Bytes(), &bootstrapResult); err != nil {
		t.Fatalf("decode bootstrap result: %v", err)
	}
	if !strings.HasPrefix(bootstrapResult.Mesh.WalletPlayerID, "player-") {
		t.Fatalf("unexpected wallet player id %q", bootstrapResult.Mesh.WalletPlayerID)
	}
	if bootstrapResult.Transport.Peer.Endpoint == "" {
		t.Fatalf("expected bootstrap to return a transport endpoint")
	}

	walletResponse := localRequest(t, app, http.MethodGet, "/api/local/profiles/alice/wallet", nil)
	if walletResponse.Code != http.StatusOK {
		t.Fatalf("wallet status = %d body=%s", walletResponse.Code, walletResponse.Body.String())
	}
	var wallet walletpkg.WalletSummary
	if err := json.Unmarshal(walletResponse.Body.Bytes(), &wallet); err != nil {
		t.Fatalf("decode wallet: %v", err)
	}
	if wallet.AvailableSats <= 0 {
		t.Fatalf("expected mock wallet sats, got %+v", wallet)
	}

	walletNsecResponse := localRequest(t, app, http.MethodGet, "/api/local/profiles/alice/wallet/nsec", nil)
	if walletNsecResponse.Code != http.StatusOK {
		t.Fatalf("wallet nsec status = %d body=%s", walletNsecResponse.Code, walletNsecResponse.Body.String())
	}
	var walletNsec string
	if err := json.Unmarshal(walletNsecResponse.Body.Bytes(), &walletNsec); err != nil {
		t.Fatalf("decode wallet nsec: %v", err)
	}
	if !strings.HasPrefix(walletNsec, "nsec1") {
		t.Fatalf("expected wallet nsec prefix, received %q", walletNsec)
	}

	networkBootstrapResponse := localRequest(t, app, http.MethodPost, "/api/local/profiles/alice/network/bootstrap", map[string]any{
		"alias":    "ghost-host",
		"endpoint": bootstrapResult.Transport.Peer.Endpoint,
		"roles":    []string{"host"},
	})
	if networkBootstrapResponse.Code != http.StatusOK {
		t.Fatalf("network bootstrap status = %d body=%s", networkBootstrapResponse.Code, networkBootstrapResponse.Body.String())
	}

	createTableResponse := localRequest(t, app, http.MethodPost, "/api/local/profiles/alice/tables", map[string]any{
		"table": map[string]any{
			"bigBlindSats":      100,
			"buyInMaxSats":      4000,
			"buyInMinSats":      4000,
			"name":              "Controller Table",
			"public":            true,
			"smallBlindSats":    50,
			"spectatorsAllowed": true,
		},
	})
	if createTableResponse.Code != http.StatusOK {
		t.Fatalf("create table status = %d body=%s", createTableResponse.Code, createTableResponse.Body.String())
	}
	var created struct {
		Table struct {
			TableID string `json:"tableId"`
		} `json:"table"`
	}
	if err := json.Unmarshal(createTableResponse.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create table: %v", err)
	}
	tableID := created.Table.TableID
	if tableID == "" {
		t.Fatalf("expected table ID in create table response")
	}

	getTableResponse := localRequest(t, app, http.MethodGet, "/api/local/profiles/alice/tables/"+tableID, nil)
	if getTableResponse.Code != http.StatusOK {
		t.Fatalf("get table status = %d body=%s", getTableResponse.Code, getTableResponse.Body.String())
	}
	var table struct {
		Config struct {
			Name    string `json:"name"`
			TableID string `json:"tableId"`
		} `json:"config"`
		Local struct {
			MyPlayerID string `json:"myPlayerId"`
		} `json:"local"`
	}
	if err := json.Unmarshal(getTableResponse.Body.Bytes(), &table); err != nil {
		t.Fatalf("decode table: %v", err)
	}
	if table.Config.Name != "Controller Table" || table.Config.TableID != tableID {
		t.Fatalf("unexpected table config payload: %+v", table.Config)
	}
	if !strings.HasPrefix(table.Local.MyPlayerID, "player-") {
		t.Fatalf("unexpected local player id %q", table.Local.MyPlayerID)
	}

	publicTablesResponse := localRequest(t, app, http.MethodGet, "/api/local/profiles/alice/tables/public", nil)
	if publicTablesResponse.Code != http.StatusOK {
		t.Fatalf("public tables status = %d body=%s", publicTablesResponse.Code, publicTablesResponse.Body.String())
	}

	server := httptest.NewServer(app)
	defer server.Close()

	streamRequest, err := http.NewRequest(http.MethodGet, server.URL+"/api/local/profiles/alice/watch", nil)
	if err != nil {
		t.Fatalf("build stream request: %v", err)
	}
	streamRequest.Header.Set(LocalControllerHeader, "1")
	streamRequest.Header.Set("Origin", testOrigin)
	streamResponse, err := server.Client().Do(streamRequest)
	if err != nil {
		t.Fatalf("open watch stream: %v", err)
	}
	defer streamResponse.Body.Close()
	if streamResponse.StatusCode != http.StatusOK {
		t.Fatalf("watch status = %d", streamResponse.StatusCode)
	}

	reader := bufio.NewReader(streamResponse.Body)
	firstEvent := readSSEEvent(t, reader)
	if firstEvent != "state" {
		t.Fatalf("expected first SSE event to be state, got %q", firstEvent)
	}

	publicTablesFetch, err := http.NewRequest(http.MethodGet, server.URL+"/api/local/profiles/alice/tables/public", nil)
	if err != nil {
		t.Fatalf("build public tables request: %v", err)
	}
	publicTablesFetch.Header.Set(LocalControllerHeader, "1")
	publicTablesFetch.Header.Set("Origin", testOrigin)
	fetchResponse, err := server.Client().Do(publicTablesFetch)
	if err != nil {
		t.Fatalf("fetch public tables during watch: %v", err)
	}
	_ = fetchResponse.Body.Close()

	secondEvent := readSSEEvent(t, reader)
	if secondEvent != "log" {
		t.Fatalf("expected second SSE event to be log, got %q", secondEvent)
	}

	stopDaemon(t, runtimeConfig, "alice")
}

func newTestControllerApp(t *testing.T, runtimeConfig cfg.RuntimeConfig) *App {
	t.Helper()
	app, err := NewApp(Options{
		AllowedOrigins: []string{testOrigin},
		Config:         runtimeConfig,
	})
	if err != nil {
		t.Fatalf("create controller app: %v", err)
	}
	return app
}

func newTestRuntimeConfig(t *testing.T) cfg.RuntimeConfig {
	t.Helper()
	rootDir := t.TempDir()
	runtimeConfig, err := cfg.ResolveRuntimeConfig(map[string]string{
		"cache-type":    "memory",
		"daemon-dir":    filepath.Join(rootDir, "daemons"),
		"datadir":       rootDir,
		"db-path":       filepath.Join(rootDir, "storage", "core.sqlite"),
		"event-db-path": filepath.Join(rootDir, "storage", "events.badger"),
		"mock":          "true",
		"network":       "regtest",
		"peer-port":     "0",
		"profile-dir":   filepath.Join(rootDir, "profiles"),
		"run-dir":       filepath.Join(rootDir, "runs"),
	})
	if err != nil {
		t.Fatalf("resolve runtime config: %v", err)
	}
	return runtimeConfig
}

func bootstrapProfile(t *testing.T, runtimeConfig cfg.RuntimeConfig, profileName, nickname string) {
	t.Helper()
	runtime := walletpkg.NewRuntime(walletpkg.RuntimeConfig{
		ArkServerURL:      runtimeConfig.ArkServerURL,
		Network:           runtimeConfig.Network,
		NigiriDatadir:     runtimeConfig.NigiriDatadir,
		ProfileDir:        runtimeConfig.ProfileDir,
		RunDir:            runtimeConfig.RunDir,
		UseMockSettlement: runtimeConfig.UseMockSettlement,
	})
	if _, err := runtime.Bootstrap(profileName, nickname, ""); err != nil {
		t.Fatalf("bootstrap profile %s: %v", profileName, err)
	}
}

func localRequest(t *testing.T, handler http.Handler, method, path string, payload any) *httptest.ResponseRecorder {
	t.Helper()
	var body []byte
	if payload != nil {
		var err error
		body, err = json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal request payload: %v", err)
		}
	}
	request := httptest.NewRequest(method, path, bytes.NewReader(body))
	request.Header.Set(LocalControllerHeader, "1")
	request.Header.Set("Origin", testOrigin)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func readSSEEvent(t *testing.T, reader *bufio.Reader) string {
	t.Helper()
	type result struct {
		err   error
		event string
	}
	resultCh := make(chan result, 1)
	go func() {
		currentEvent := ""
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				resultCh <- result{err: err}
				return
			}
			trimmed := strings.TrimRight(line, "\r\n")
			if trimmed == "" {
				if currentEvent != "" {
					resultCh <- result{event: currentEvent}
					return
				}
				continue
			}
			if strings.HasPrefix(trimmed, ":") {
				continue
			}
			if strings.HasPrefix(trimmed, "event:") {
				currentEvent = strings.TrimSpace(strings.TrimPrefix(trimmed, "event:"))
			}
		}
	}()

	select {
	case result := <-resultCh:
		if result.err != nil {
			t.Fatalf("read SSE event: %v", result.err)
		}
		return result.event
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for SSE event")
		return ""
	}
}

func stopDaemon(t *testing.T, runtimeConfig cfg.RuntimeConfig, profile string) {
	t.Helper()
	client := parker.NewClient(profile, runtimeConfig)
	defer client.Close()

	status, err := client.Inspect(false)
	if err != nil {
		return
	}
	reachable, _ := status["reachable"].(bool)
	if reachable {
		_ = client.StopDaemon()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			status, err = client.Inspect(false)
			if err != nil {
				return
			}
			reachable, _ = status["reachable"].(bool)
			if !reachable {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
}
