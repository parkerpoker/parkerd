package wallet

import (
	"context"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"strings"
	"time"

	arklib "github.com/arkade-os/arkd/pkg/ark-lib"
	arkscript "github.com/arkade-os/arkd/pkg/ark-lib/script"
	sdktypes "github.com/arkade-os/go-sdk/types"
	"github.com/btcsuite/btcd/btcec/v2"
)

const arkCompatProxyPathPrefix = "/parker-ark-compat"

type arkServerCompatibility struct {
	normalizedInfo           []byte
	rewriteLegacyIndexerPath bool
}

func compatibleArkServerURL(ctx context.Context, arkServerURL string) (string, func(), error) {
	compatibility, err := fetchArkServerCompatibility(ctx, arkServerURL)
	if err != nil {
		return "", nil, err
	}
	if !compatibility.needsCompatProxy() {
		return arkServerURL, func() {}, nil
	}
	return startArkCompatProxy(arkServerURL, compatibility)
}

func repairStoredCompatServerURL(ctx context.Context, configStore sdktypes.ConfigStore, arkServerURL string) error {
	cfgData, err := configStore.GetData(ctx)
	if err != nil || cfgData == nil || !isCompatArkServerURL(cfgData.ServerUrl) {
		return err
	}
	return rewriteStoredServerURL(ctx, configStore, arkServerURL)
}

func rewriteStoredServerURL(ctx context.Context, configStore sdktypes.ConfigStore, arkServerURL string) error {
	cfgData, err := configStore.GetData(ctx)
	if err != nil {
		return err
	}
	if cfgData == nil || cfgData.ServerUrl == arkServerURL {
		return nil
	}
	cfgData.ServerUrl = arkServerURL
	return configStore.AddData(ctx, *cfgData)
}

func rewriteStoredClientType(ctx context.Context, configStore sdktypes.ConfigStore, clientType string) error {
	cfgData, err := configStore.GetData(ctx)
	if err != nil {
		return err
	}
	if cfgData == nil || cfgData.ClientType == clientType {
		return nil
	}
	cfgData.ClientType = clientType
	return configStore.AddData(ctx, *cfgData)
}

func fetchArkServerCompatibility(ctx context.Context, arkServerURL string) (arkServerCompatibility, error) {
	infoURL, err := joinURLPath(arkServerURL, "v1/info")
	if err != nil {
		return arkServerCompatibility{}, err
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, infoURL, nil)
	if err != nil {
		return arkServerCompatibility{}, err
	}

	client := newArkCompatHTTPClient()
	response, err := client.Do(request)
	if err != nil {
		return arkServerCompatibility{}, fmt.Errorf("fetch Ark server info: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4<<10))
		return arkServerCompatibility{}, fmt.Errorf("fetch Ark server info: %s: %s", response.Status, strings.TrimSpace(string(body)))
	}

	decoder := json.NewDecoder(response.Body)
	decoder.UseNumber()
	var payload map[string]any
	if err := decoder.Decode(&payload); err != nil {
		return arkServerCompatibility{}, fmt.Errorf("decode Ark server info: %w", err)
	}

	rewriteLegacyIndexerPath, err := detectLegacyIndexerPathRewrite(ctx, client, arkServerURL)
	if err != nil {
		return arkServerCompatibility{}, err
	}

	normalized, changed, err := normalizeArkServerInfo(payload)
	if err != nil {
		return arkServerCompatibility{}, err
	}

	compatibility := arkServerCompatibility{
		rewriteLegacyIndexerPath: rewriteLegacyIndexerPath,
	}
	if !changed {
		return compatibility, nil
	}

	body, err := json.Marshal(normalized)
	if err != nil {
		return arkServerCompatibility{}, fmt.Errorf("encode normalized Ark server info: %w", err)
	}
	compatibility.normalizedInfo = body
	return compatibility, nil
}

func normalizeArkServerInfo(payload map[string]any) (map[string]any, bool, error) {
	signerPubKey := jsonString(payload["signerPubkey"])
	if signerPubKey == "" {
		return nil, false, fmt.Errorf("normalize Ark server info: missing signerPubkey")
	}

	changed := false
	if jsonString(payload["forfeitPubkey"]) == "" {
		payload["forfeitPubkey"] = signerPubKey
		changed = true
	}

	if jsonString(payload["checkpointTapscript"]) == "" {
		unilateralExitDelay, err := jsonInt64(payload["unilateralExitDelay"])
		if err != nil {
			return nil, false, fmt.Errorf("normalize Ark server info: %w", err)
		}
		checkpointTapscript, err := deriveCheckpointTapscript(signerPubKey, unilateralExitDelay)
		if err != nil {
			return nil, false, err
		}
		payload["checkpointTapscript"] = checkpointTapscript
		changed = true
	}

	fieldChanged, err := normalizeIntField(payload, "sessionDuration")
	if err != nil {
		return nil, false, fmt.Errorf("normalize Ark server info sessionDuration: %w", err)
	}
	changed = changed || fieldChanged

	scheduledSessionChanged, err := normalizeScheduledSession(payload)
	if err != nil {
		return nil, false, err
	}
	changed = changed || scheduledSessionChanged

	return payload, changed, nil
}

func normalizeScheduledSession(payload map[string]any) (bool, error) {
	rawScheduledSession, ok := payload["scheduledSession"]
	if !ok || rawScheduledSession == nil {
		return false, nil
	}

	scheduledSession, ok := rawScheduledSession.(map[string]any)
	if !ok {
		return false, nil
	}

	changed := false
	for _, field := range []string{"duration", "nextStartTime", "nextEndTime", "period"} {
		fieldChanged, err := normalizeIntField(scheduledSession, field)
		if err != nil {
			return false, fmt.Errorf("normalize Ark server info scheduledSession.%s: %w", field, err)
		}
		changed = changed || fieldChanged
	}
	payload["scheduledSession"] = scheduledSession
	return changed, nil
}

func normalizeIntField(payload map[string]any, field string) (bool, error) {
	rawValue, ok := payload[field]
	if !ok || rawValue == nil {
		return false, nil
	}
	if _, ok := rawValue.(string); !ok {
		return false, nil
	}

	value, err := jsonInt64(rawValue)
	if err != nil {
		return false, err
	}
	payload[field] = value
	return true, nil
}

func deriveCheckpointTapscript(signerPubKeyHex string, unilateralExitDelay int64) (string, error) {
	pubKeyBytes, err := hex.DecodeString(signerPubKeyHex)
	if err != nil {
		return "", fmt.Errorf("decode signer pubkey: %w", err)
	}
	signerPubKey, err := btcec.ParsePubKey(pubKeyBytes)
	if err != nil {
		return "", fmt.Errorf("parse signer pubkey: %w", err)
	}

	locktimeType := arklib.LocktimeTypeBlock
	if unilateralExitDelay >= 512 {
		locktimeType = arklib.LocktimeTypeSecond
	}
	scriptBytes, err := (&arkscript.CSVMultisigClosure{
		MultisigClosure: arkscript.MultisigClosure{
			PubKeys: []*btcec.PublicKey{signerPubKey},
		},
		Locktime: arklib.RelativeLocktime{
			Type:  locktimeType,
			Value: uint32(unilateralExitDelay),
		},
	}).Script()
	if err != nil {
		return "", fmt.Errorf("derive checkpoint tapscript: %w", err)
	}
	return hex.EncodeToString(scriptBytes), nil
}

func detectLegacyIndexerPathRewrite(ctx context.Context, client *http.Client, arkServerURL string) (bool, error) {
	indexerVtxosURL, err := joinURLPath(arkServerURL, "v1/indexer/vtxos")
	if err != nil {
		return false, err
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, indexerVtxosURL, nil)
	if err != nil {
		return false, err
	}

	response, err := client.Do(request)
	if err != nil {
		return false, fmt.Errorf("probe Ark indexer route: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode == http.StatusNotFound {
		return true, nil
	}
	return false, nil
}

func newArkCompatHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 15 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
		},
	}
}

func startArkCompatProxy(targetURL string, compatibility arkServerCompatibility) (string, func(), error) {
	target, err := url.Parse(targetURL)
	if err != nil {
		return "", nil, fmt.Errorf("parse Ark server URL: %w", err)
	}

	proxy := httputil.NewSingleHostReverseProxy(target)
	originalDirector := proxy.Director
	proxy.Director = func(request *http.Request) {
		request.URL.Path = rewriteCompatProxyPath(request.URL.Path, compatibility.rewriteLegacyIndexerPath)
		request.URL.RawPath = rewriteCompatProxyPath(request.URL.RawPath, compatibility.rewriteLegacyIndexerPath)
		originalDirector(request)
	}

	mux := http.NewServeMux()
	mux.HandleFunc(arkCompatProxyPathPrefix+"/v1/info", func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || len(compatibility.normalizedInfo) == 0 {
			proxy.ServeHTTP(writer, request)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write(compatibility.normalizedInfo)
	})
	mux.Handle(arkCompatProxyPathPrefix+"/", proxy)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, fmt.Errorf("listen for Ark compat proxy: %w", err)
	}
	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		_ = server.Serve(listener)
	}()

	baseURL := fmt.Sprintf("http://%s%s", listener.Addr().String(), arkCompatProxyPathPrefix)
	cleanup := func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
		_ = listener.Close()
	}
	return baseURL, cleanup, nil
}

func (compatibility arkServerCompatibility) needsCompatProxy() bool {
	return len(compatibility.normalizedInfo) > 0 || compatibility.rewriteLegacyIndexerPath
}

func trimCompatPath(value string) string {
	if value == "" {
		return value
	}
	trimmed := strings.TrimPrefix(value, arkCompatProxyPathPrefix)
	if trimmed == "" {
		return "/"
	}
	return trimmed
}

func rewriteCompatProxyPath(value string, rewriteLegacyIndexerPath bool) string {
	trimmed := trimCompatPath(value)
	if rewriteLegacyIndexerPath && strings.HasPrefix(trimmed, "/v1/indexer/") {
		return "/v1/" + strings.TrimPrefix(trimmed, "/v1/indexer/")
	}
	if rewriteLegacyIndexerPath && trimmed == "/v1/indexer" {
		return "/v1"
	}
	return trimmed
}

func joinURLPath(rawURL string, elem string) (string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("parse URL %q: %w", rawURL, err)
	}
	parsed.Path = path.Join(parsed.Path, elem)
	if !strings.HasPrefix(parsed.Path, "/") {
		parsed.Path = "/" + parsed.Path
	}
	parsed.RawPath = parsed.EscapedPath()
	return parsed.String(), nil
}

func isCompatArkServerURL(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	return parsed.Path == arkCompatProxyPathPrefix || strings.HasPrefix(parsed.Path, arkCompatProxyPathPrefix+"/")
}

func jsonString(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(typed)
	case json.Number:
		return typed.String()
	default:
		return strings.TrimSpace(fmt.Sprint(typed))
	}
}

func jsonInt64(value any) (int64, error) {
	switch typed := value.(type) {
	case nil:
		return 0, fmt.Errorf("missing numeric value")
	case json.Number:
		return typed.Int64()
	case string:
		if typed == "" {
			return 0, fmt.Errorf("missing numeric value")
		}
		number := json.Number(typed)
		return number.Int64()
	case float64:
		return int64(typed), nil
	default:
		number := json.Number(fmt.Sprint(typed))
		return number.Int64()
	}
}
