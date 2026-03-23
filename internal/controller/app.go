package controller

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	parker "github.com/danieldresner/arkade_fun/internal"
	cfg "github.com/danieldresner/arkade_fun/internal/config"
	"github.com/danieldresner/arkade_fun/internal/mesh"
	walletpkg "github.com/danieldresner/arkade_fun/internal/wallet"
)

const (
	LocalControllerHeader      = "X-Parker-Local-Controller"
	defaultControllerPort      = 3030
	defaultDevWebPort          = 3010
	indexHTMLName              = "index.html"
	controllerKeepaliveSeconds = 15
)

type Options struct {
	AllowedOrigins []string
	Config         cfg.RuntimeConfig
	ControllerPort int
	WebDistDir     string
}

type App struct {
	allowedOriginSet map[string]struct{}
	allowedOrigins   []string
	config           cfg.RuntimeConfig
	controllerPort   int
	hasBundledWeb    bool
	mux              *http.ServeMux
	profileStore     *walletpkg.ProfileStore
	webDistDir       string
}

type DaemonRuntimeState struct {
	Mesh   *mesh.RuntimeState       `json:"mesh,omitempty"`
	Wallet *walletpkg.WalletSummary `json:"wallet,omitempty"`
}

type ProfileDaemonStatus struct {
	Metadata  *cfg.ProfileDaemonMetadata `json:"metadata"`
	Reachable bool                       `json:"reachable"`
	State     *DaemonRuntimeState        `json:"state"`
}

type LocalProfileStatusResponse struct {
	Daemon  ProfileDaemonStatus           `json:"daemon"`
	Profile walletpkg.LocalProfileSummary `json:"profile"`
}

type LocalControllerHealth struct {
	AllowedOrigins          []string `json:"allowedOrigins"`
	Bind                    string   `json:"bind"`
	OK                      bool     `json:"ok"`
	ProfilesDir             string   `json:"profilesDir"`
	PublicIndexerConfigured bool     `json:"publicIndexerConfigured"`
	WebBundleAvailable      bool     `json:"webBundleAvailable"`
}

type controllerError struct {
	statusCode int
	message    string
}

func (err controllerError) Error() string {
	return err.message
}

func NewApp(options Options) (*App, error) {
	runtimeConfig := options.Config
	if runtimeConfig.DataDir == "" {
		var err error
		runtimeConfig, err = cfg.ResolveRuntimeConfig(nil)
		if err != nil {
			return nil, err
		}
	}

	controllerPort := options.ControllerPort
	if controllerPort == 0 {
		controllerPort = defaultControllerPort
	}
	allowedOrigins := options.AllowedOrigins
	if len(allowedOrigins) == 0 {
		allowedOrigins = resolveAllowedOrigins(controllerPort)
	}

	app := &App{
		allowedOriginSet: map[string]struct{}{},
		allowedOrigins:   allowedOrigins,
		config:           runtimeConfig,
		controllerPort:   controllerPort,
		mux:              http.NewServeMux(),
		profileStore:     walletpkg.NewProfileStore(runtimeConfig.ProfileDir),
		webDistDir:       options.WebDistDir,
	}
	for _, origin := range allowedOrigins {
		app.allowedOriginSet[origin] = struct{}{}
	}
	if options.WebDistDir != "" {
		if _, err := os.Stat(filepath.Join(options.WebDistDir, indexHTMLName)); err == nil {
			app.hasBundledWeb = true
		}
	}

	app.registerRoutes()
	return app, nil
}

func (app *App) AllowedOrigins() []string {
	return append([]string(nil), app.allowedOrigins...)
}

func (app *App) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if handled, err := app.handleLocalAccess(writer, request); handled {
		if err != nil {
			writeControllerError(writer, err)
		}
		return
	}

	if app.hasBundledWeb && shouldServeBundledAsset(request.URL.Path) {
		if err := app.serveBundledAsset(writer, request.URL.Path); err == nil {
			return
		}
	}

	app.mux.ServeHTTP(writer, request)
}

func (app *App) registerRoutes() {
	app.mux.HandleFunc("GET /health", func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(writer, http.StatusOK, LocalControllerHealth{
			AllowedOrigins:          app.AllowedOrigins(),
			Bind:                    cfg.DefaultControllerHost,
			OK:                      true,
			ProfilesDir:             app.config.ProfileDir,
			PublicIndexerConfigured: app.config.IndexerURL != "",
			WebBundleAvailable:      app.hasBundledWeb,
		})
	})

	app.mux.HandleFunc("GET /api/local/profiles", func(writer http.ResponseWriter, _ *http.Request) {
		profiles, err := app.profileStore.ListProfiles()
		if err != nil {
			writeControllerError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, profiles)
	})

	app.mux.HandleFunc("GET /api/local/profiles/{profile}/status", func(writer http.ResponseWriter, request *http.Request) {
		response, err := app.withProfile(request.PathValue("profile"), func(summary walletpkg.LocalProfileSummary, client *parker.Client) (any, error) {
			status, err := typedInspect(client, false)
			if err != nil {
				return nil, err
			}
			return LocalProfileStatusResponse{
				Daemon:  status,
				Profile: summary,
			}, nil
		})
		if err != nil {
			writeControllerError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, response)
	})

	app.mux.HandleFunc("POST /api/local/profiles/{profile}/daemon/start", func(writer http.ResponseWriter, request *http.Request) {
		payload, err := decodeBodyMap(request)
		if err != nil {
			writeControllerError(writer, controllerError{statusCode: http.StatusBadRequest, message: err.Error()})
			return
		}
		response, err := app.withProfile(request.PathValue("profile"), func(summary walletpkg.LocalProfileSummary, client *parker.Client) (any, error) {
			if err := client.EnsureRunning(parseMode(payload["mode"])); err != nil {
				return nil, err
			}
			status, err := typedInspect(client, false)
			if err != nil {
				return nil, err
			}
			return LocalProfileStatusResponse{
				Daemon:  status,
				Profile: summary,
			}, nil
		})
		if err != nil {
			writeControllerError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, response)
	})

	app.mux.HandleFunc("POST /api/local/profiles/{profile}/daemon/stop", func(writer http.ResponseWriter, request *http.Request) {
		response, err := app.withRunningProfile(request.PathValue("profile"), func(summary walletpkg.LocalProfileSummary, client *parker.Client) (any, error) {
			if err := client.StopDaemon(); err != nil {
				return nil, err
			}
			status, err := waitForDaemonReachability(client, false, 5*time.Second)
			if err != nil {
				return nil, err
			}
			return LocalProfileStatusResponse{
				Daemon:  status,
				Profile: summary,
			}, nil
		})
		if err != nil {
			writeControllerError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, response)
	})

	app.mux.HandleFunc("GET /api/local/profiles/{profile}/watch", func(writer http.ResponseWriter, request *http.Request) {
		if err := app.handleWatch(writer, request); err != nil {
			writeControllerError(writer, err)
		}
	})

	app.mux.HandleFunc("POST /api/local/profiles/{profile}/bootstrap", app.daemonPassthrough("bootstrap"))
	app.mux.HandleFunc("GET /api/local/profiles/{profile}/wallet", app.daemonPassthrough("walletSummary"))
	app.mux.HandleFunc("POST /api/local/profiles/{profile}/wallet/deposit", app.daemonPassthrough("walletDeposit"))
	app.mux.HandleFunc("POST /api/local/profiles/{profile}/wallet/withdraw", app.daemonPassthrough("walletWithdraw"))
	app.mux.HandleFunc("POST /api/local/profiles/{profile}/wallet/onboard", app.daemonPassthrough("walletOnboard"))
	app.mux.HandleFunc("POST /api/local/profiles/{profile}/wallet/offboard", app.daemonPassthrough("walletOffboard"))
	app.mux.HandleFunc("GET /api/local/profiles/{profile}/network/peers", app.daemonPassthrough("meshNetworkPeers"))
	app.mux.HandleFunc("POST /api/local/profiles/{profile}/network/bootstrap", app.daemonPassthrough("meshBootstrapPeer"))
	app.mux.HandleFunc("GET /api/local/profiles/{profile}/tables/public", app.daemonPassthrough("meshPublicTables"))
	app.mux.HandleFunc("POST /api/local/profiles/{profile}/tables", app.handleCreateTable)
	app.mux.HandleFunc("POST /api/local/profiles/{profile}/tables/join", app.daemonPassthrough("meshTableJoin"))
	app.mux.HandleFunc("GET /api/local/profiles/{profile}/tables/{tableId}", app.tablePassthrough("meshGetTable"))
	app.mux.HandleFunc("POST /api/local/profiles/{profile}/tables/{tableId}/announce", app.tablePassthrough("meshTableAnnounce"))
	app.mux.HandleFunc("POST /api/local/profiles/{profile}/tables/{tableId}/action", app.handleTableAction)
	app.mux.HandleFunc("POST /api/local/profiles/{profile}/tables/{tableId}/rotate-host", app.tablePassthrough("meshRotateHost"))
	app.mux.HandleFunc("POST /api/local/profiles/{profile}/tables/{tableId}/cashout", app.tablePassthrough("meshCashOut"))
	app.mux.HandleFunc("POST /api/local/profiles/{profile}/tables/{tableId}/renew", app.tablePassthrough("meshRenew"))
	app.mux.HandleFunc("POST /api/local/profiles/{profile}/tables/{tableId}/exit", app.tablePassthrough("meshExit"))

	app.mux.HandleFunc("POST /api/local/profiles/{profile}/wallet/faucet", func(writer http.ResponseWriter, request *http.Request) {
		response, err := app.withRunningProfile(request.PathValue("profile"), func(_ walletpkg.LocalProfileSummary, client *parker.Client) (any, error) {
			payload, err := decodeBodyMap(request)
			if err != nil {
				return nil, controllerError{statusCode: http.StatusBadRequest, message: err.Error()}
			}
			result, err := client.Request("walletFaucet", payload, true)
			if err != nil {
				return nil, err
			}
			_ = result
			status, err := typedInspect(client, false)
			if err != nil {
				return nil, err
			}
			return status, nil
		})
		if err != nil {
			writeControllerError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, response)
	})

	app.mux.HandleFunc("GET /api/public/tables", func(writer http.ResponseWriter, _ *http.Request) {
		if err := app.proxyIndexerRequest(writer, "/api/public/tables"); err != nil {
			writeControllerError(writer, err)
		}
	})
	app.mux.HandleFunc("GET /api/public/tables/{tableId}", func(writer http.ResponseWriter, request *http.Request) {
		if err := app.proxyIndexerRequest(writer, "/api/public/tables/"+request.PathValue("tableId")); err != nil {
			writeControllerError(writer, err)
		}
	})
}

func (app *App) daemonPassthrough(method string) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		response, err := app.withRunningProfile(request.PathValue("profile"), func(_ walletpkg.LocalProfileSummary, client *parker.Client) (any, error) {
			payload, err := decodeBodyMap(request)
			if err != nil {
				return nil, controllerError{statusCode: http.StatusBadRequest, message: err.Error()}
			}
			return client.Request(method, payloadOrNil(payload), true)
		})
		if err != nil {
			writeControllerError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, response)
	}
}

func (app *App) tablePassthrough(method string) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		response, err := app.withRunningProfile(request.PathValue("profile"), func(_ walletpkg.LocalProfileSummary, client *parker.Client) (any, error) {
			return client.Request(method, map[string]any{"tableId": request.PathValue("tableId")}, true)
		})
		if err != nil {
			writeControllerError(writer, err)
			return
		}
		writeJSON(writer, http.StatusOK, response)
	}
}

func (app *App) handleCreateTable(writer http.ResponseWriter, request *http.Request) {
	response, err := app.withRunningProfile(request.PathValue("profile"), func(_ walletpkg.LocalProfileSummary, client *parker.Client) (any, error) {
		payload, err := decodeBodyMap(request)
		if err != nil {
			return nil, controllerError{statusCode: http.StatusBadRequest, message: err.Error()}
		}
		tableValue, ok := payload["table"].(map[string]any)
		if !ok && len(payload) > 0 {
			tableValue = payload
		}
		if len(tableValue) == 0 {
			return client.Request("meshCreateTable", nil, true)
		}
		return client.Request("meshCreateTable", map[string]any{"table": tableValue}, true)
	})
	if err != nil {
		writeControllerError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, response)
}

func (app *App) handleTableAction(writer http.ResponseWriter, request *http.Request) {
	response, err := app.withRunningProfile(request.PathValue("profile"), func(_ walletpkg.LocalProfileSummary, client *parker.Client) (any, error) {
		payload, err := decodeBodyMap(request)
		if err != nil {
			return nil, controllerError{statusCode: http.StatusBadRequest, message: err.Error()}
		}
		actionValue, ok := payload["payload"].(map[string]any)
		if !ok {
			return nil, controllerError{statusCode: http.StatusBadRequest, message: "payload is required"}
		}
		actionType, ok := actionValue["type"].(string)
		if !ok || strings.TrimSpace(actionType) == "" {
			return nil, controllerError{statusCode: http.StatusBadRequest, message: "payload.type is required"}
		}
		actionValue["type"] = actionType
		return client.Request("meshSendAction", map[string]any{
			"payload": actionValue,
			"tableId": request.PathValue("tableId"),
		}, true)
	})
	if err != nil {
		writeControllerError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, response)
}

func (app *App) handleWatch(writer http.ResponseWriter, request *http.Request) error {
	if _, err := app.requireProfileSummary(request.PathValue("profile")); err != nil {
		return err
	}

	client := parker.NewClient(request.PathValue("profile"), app.config)
	status, err := typedInspect(client, false)
	if err != nil {
		return err
	}
	if !status.Reachable {
		return controllerError{statusCode: http.StatusServiceUnavailable, message: "daemon is not running"}
	}

	flusher, ok := writer.(http.Flusher)
	if !ok {
		return controllerError{statusCode: http.StatusInternalServerError, message: "streaming is unavailable"}
	}

	session, err := client.StartWatch(false)
	if err != nil {
		return err
	}
	defer session.Stop()

	headers := writer.Header()
	headers.Set("Cache-Control", "no-store")
	headers.Set("Connection", "keep-alive")
	headers.Set("Content-Type", "text/event-stream; charset=utf-8")
	headers.Set("X-Accel-Buffering", "no")
	writer.WriteHeader(http.StatusOK)

	if err := writeSSE(writer, flusher, "state", session.InitialState); err != nil {
		return nil
	}

	ticker := time.NewTicker(controllerKeepaliveSeconds * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-request.Context().Done():
			return nil
		case err := <-session.Done:
			if err != nil {
				return nil
			}
			return nil
		case event, ok := <-session.Events:
			if !ok {
				return nil
			}
			var payload any
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				continue
			}
			if err := writeSSE(writer, flusher, event.Event, payload); err != nil {
				return nil
			}
		case <-ticker.C:
			if _, err := io.WriteString(writer, ": keepalive\n\n"); err != nil {
				return nil
			}
			flusher.Flush()
		}
	}
}

func (app *App) handleLocalAccess(writer http.ResponseWriter, request *http.Request) (bool, error) {
	if !strings.HasPrefix(request.URL.Path, "/api/local/") {
		return false, nil
	}

	origin := request.Header.Get("Origin")
	if origin != "" {
		writer.Header().Set("Access-Control-Allow-Headers", "Content-Type, "+LocalControllerHeader)
		writer.Header().Set("Access-Control-Allow-Methods", "GET,POST,OPTIONS")
		writer.Header().Set("Vary", "Origin")
		if _, ok := app.allowedOriginSet[origin]; !ok {
			return true, controllerError{statusCode: http.StatusForbidden, message: fmt.Sprintf("origin %s is not allowed by the local controller", origin)}
		}
		writer.Header().Set("Access-Control-Allow-Origin", origin)
	}

	if request.Method == http.MethodOptions {
		requestedHeaders := strings.Split(strings.ToLower(request.Header.Get("Access-Control-Request-Headers")), ",")
		for index := range requestedHeaders {
			requestedHeaders[index] = strings.TrimSpace(requestedHeaders[index])
		}
		for _, header := range requestedHeaders {
			if header == strings.ToLower(LocalControllerHeader) {
				writer.WriteHeader(http.StatusNoContent)
				return true, nil
			}
		}
		return true, controllerError{statusCode: http.StatusBadRequest, message: LocalControllerHeader + " is required for browser access"}
	}

	if request.Header.Get(LocalControllerHeader) == "" {
		return true, controllerError{statusCode: http.StatusBadRequest, message: LocalControllerHeader + " header is required"}
	}
	return false, nil
}

func (app *App) proxyIndexerRequest(writer http.ResponseWriter, path string) error {
	if app.config.IndexerURL == "" {
		return controllerError{statusCode: http.StatusServiceUnavailable, message: "indexer is not configured"}
	}

	response, err := http.Get(strings.TrimRight(app.config.IndexerURL, "/") + path)
	if err != nil {
		return err
	}
	defer response.Body.Close()

	body, err := io.ReadAll(response.Body)
	if err != nil {
		return err
	}
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Type", firstNonEmpty(response.Header.Get("Content-Type"), "application/json; charset=utf-8"))
	writer.WriteHeader(response.StatusCode)
	_, err = writer.Write(body)
	return err
}

func (app *App) serveBundledAsset(writer http.ResponseWriter, requestPath string) error {
	path := strings.TrimPrefix(requestPath, "/")
	if path == "" {
		path = indexHTMLName
	}
	targetPath := filepath.Join(app.webDistDir, filepath.Clean(path))
	info, err := os.Stat(targetPath)
	if err != nil || info.IsDir() {
		targetPath = filepath.Join(app.webDistDir, indexHTMLName)
	}
	body, err := os.ReadFile(targetPath)
	if err != nil {
		return err
	}
	writer.Header().Set("Content-Type", firstNonEmpty(mime.TypeByExtension(filepath.Ext(targetPath)), "text/html; charset=utf-8"))
	writer.WriteHeader(http.StatusOK)
	_, err = writer.Write(body)
	return err
}

func (app *App) requireProfileSummary(profile string) (walletpkg.LocalProfileSummary, error) {
	summary, err := app.profileStore.LoadSummary(profile)
	if err != nil {
		return walletpkg.LocalProfileSummary{}, err
	}
	if summary == nil {
		return walletpkg.LocalProfileSummary{}, controllerError{statusCode: http.StatusNotFound, message: fmt.Sprintf("profile %s was not found", profile)}
	}
	return *summary, nil
}

func (app *App) withProfile(profile string, fn func(walletpkg.LocalProfileSummary, *parker.Client) (any, error)) (any, error) {
	summary, err := app.requireProfileSummary(profile)
	if err != nil {
		return nil, err
	}
	client := parker.NewClient(profile, app.config)
	defer client.Close()
	return fn(summary, client)
}

func (app *App) withRunningProfile(profile string, fn func(walletpkg.LocalProfileSummary, *parker.Client) (any, error)) (any, error) {
	return app.withProfile(profile, func(summary walletpkg.LocalProfileSummary, client *parker.Client) (any, error) {
		status, err := typedInspect(client, false)
		if err != nil {
			return nil, err
		}
		if !status.Reachable {
			return nil, controllerError{statusCode: http.StatusServiceUnavailable, message: "daemon is not running"}
		}
		return fn(summary, client)
	})
}

func typedInspect(client *parker.Client, autoStart bool) (ProfileDaemonStatus, error) {
	rawStatus, err := client.Inspect(autoStart)
	if err != nil {
		return ProfileDaemonStatus{}, err
	}
	metadataBytes, _ := json.Marshal(rawStatus["metadata"])
	stateBytes, _ := json.Marshal(rawStatus["state"])

	var metadata *cfg.ProfileDaemonMetadata
	if len(metadataBytes) > 0 && string(metadataBytes) != "null" {
		var decoded cfg.ProfileDaemonMetadata
		if err := json.Unmarshal(metadataBytes, &decoded); err != nil {
			return ProfileDaemonStatus{}, err
		}
		metadata = &decoded
	}

	var state *DaemonRuntimeState
	if len(stateBytes) > 0 && string(stateBytes) != "null" {
		var decoded DaemonRuntimeState
		if err := json.Unmarshal(stateBytes, &decoded); err != nil {
			return ProfileDaemonStatus{}, err
		}
		state = &decoded
	}

	reachable, _ := rawStatus["reachable"].(bool)
	return ProfileDaemonStatus{
		Metadata:  metadata,
		Reachable: reachable,
		State:     state,
	}, nil
}

func waitForDaemonReachability(client *parker.Client, reachable bool, timeout time.Duration) (ProfileDaemonStatus, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		status, err := typedInspect(client, false)
		if err == nil && status.Reachable == reachable {
			return status, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	if reachable {
		return ProfileDaemonStatus{}, controllerError{statusCode: http.StatusServiceUnavailable, message: "daemon did not become reachable in time"}
	}
	return ProfileDaemonStatus{}, controllerError{statusCode: http.StatusServiceUnavailable, message: "daemon did not stop in time"}
}

func resolveAllowedOrigins(controllerPort int) []string {
	configured := strings.TrimSpace(os.Getenv("PARKER_CONTROLLER_ALLOWED_ORIGINS"))
	if configured != "" {
		parts := strings.Split(configured, ",")
		origins := make([]string, 0, len(parts))
		for _, part := range parts {
			part = strings.TrimSpace(part)
			if part != "" {
				origins = append(origins, part)
			}
		}
		if len(origins) > 0 {
			return origins
		}
	}

	return []string{
		fmt.Sprintf("http://127.0.0.1:%d", defaultDevWebPort),
		fmt.Sprintf("http://localhost:%d", defaultDevWebPort),
		fmt.Sprintf("http://127.0.0.1:%d", controllerPort),
		fmt.Sprintf("http://localhost:%d", controllerPort),
	}
}

func parseMode(value any) string {
	mode, _ := value.(string)
	switch mode {
	case "player", "host", "witness", "indexer":
		return mode
	default:
		return ""
	}
}

func shouldServeBundledAsset(path string) bool {
	return !strings.HasPrefix(path, "/api/") && path != "/health"
}

func writeControllerError(writer http.ResponseWriter, err error) {
	statusCode := http.StatusInternalServerError
	message := "internal controller error"
	var controllerErr controllerError
	if errors.As(err, &controllerErr) {
		statusCode = controllerErr.statusCode
		message = controllerErr.message
	} else if err != nil {
		message = err.Error()
	}
	writeJSON(writer, statusCode, map[string]any{
		"error":      message,
		"statusCode": statusCode,
	})
}

func decodeBodyMap(request *http.Request) (map[string]any, error) {
	if request.Body == nil {
		return map[string]any{}, nil
	}
	body, err := io.ReadAll(request.Body)
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return map[string]any{}, nil
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, err
	}
	if decoded == nil {
		return map[string]any{}, nil
	}
	return decoded, nil
}

func payloadOrNil(payload map[string]any) map[string]any {
	if len(payload) == 0 {
		return nil
	}
	return payload
}

func writeJSON(writer http.ResponseWriter, statusCode int, value any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(statusCode)
	encoder := json.NewEncoder(writer)
	_ = encoder.Encode(value)
}

func writeSSE(writer http.ResponseWriter, flusher http.Flusher, event string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(writer, "event: %s\ndata: %s\n\n", event, data); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
