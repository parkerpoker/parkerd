package wallet

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/btcsuite/btcd/btcec/v2"
)

func TestNormalizeArkServerInfoAddsCompatibilityFields(t *testing.T) {
	t.Parallel()

	_, publicKey := btcec.PrivKeyFromBytes([]byte{
		1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1,
		1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1,
	})
	signerPubKey := hex.EncodeToString(publicKey.SerializeCompressed())

	payload := map[string]any{
		"signerPubkey":        signerPubKey,
		"unilateralExitDelay": "512",
		"boardingExitDelay":   "1024",
		"network":             "regtest",
		"forfeitAddress":      "bcrt1qexample",
		"checkpointTapscript": "",
		"scheduledSession":    nil,
		"deprecatedSigners":   []any{},
		"serviceStatus":       map[string]any{},
		"sessionDuration":     "15",
		"dust":                "333",
		"utxoMinAmount":       "333",
		"utxoMaxAmount":       "-1",
		"vtxoMinAmount":       "333",
		"vtxoMaxAmount":       "-1",
	}

	normalized, changed, err := normalizeArkServerInfo(payload)
	if err != nil {
		t.Fatalf("normalizeArkServerInfo: %v", err)
	}
	if !changed {
		t.Fatalf("expected compatibility normalization to report changes")
	}
	if normalized["forfeitPubkey"] != signerPubKey {
		t.Fatalf("expected forfeitPubkey fallback to signerPubkey, received %#v", normalized["forfeitPubkey"])
	}

	expectedCheckpoint, err := deriveCheckpointTapscript(signerPubKey, 512)
	if err != nil {
		t.Fatalf("deriveCheckpointTapscript: %v", err)
	}
	if normalized["checkpointTapscript"] != expectedCheckpoint {
		t.Fatalf("expected checkpoint tapscript %q, received %#v", expectedCheckpoint, normalized["checkpointTapscript"])
	}
	if normalized["sessionDuration"] != int64(15) {
		t.Fatalf("expected sessionDuration int64 coercion, received %#v", normalized["sessionDuration"])
	}
}

func TestNormalizeArkServerInfoCoercesScheduledSessionNumbers(t *testing.T) {
	t.Parallel()

	payload := map[string]any{
		"signerPubkey":        "02aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"forfeitPubkey":       "02aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"checkpointTapscript": "abcd",
		"sessionDuration":     int64(10),
		"scheduledSession": map[string]any{
			"duration":      "15",
			"nextStartTime": "1700000000",
			"nextEndTime":   "1700000900",
			"period":        "86400",
		},
	}

	normalized, changed, err := normalizeArkServerInfo(payload)
	if err != nil {
		t.Fatalf("normalizeArkServerInfo: %v", err)
	}
	if !changed {
		t.Fatalf("expected scheduledSession numeric coercion to report changes")
	}

	scheduledSession, ok := normalized["scheduledSession"].(map[string]any)
	if !ok {
		t.Fatalf("expected scheduledSession map, received %#v", normalized["scheduledSession"])
	}
	if scheduledSession["duration"] != int64(15) {
		t.Fatalf("expected duration int64 coercion, received %#v", scheduledSession["duration"])
	}
	if scheduledSession["period"] != int64(86400) {
		t.Fatalf("expected period int64 coercion, received %#v", scheduledSession["period"])
	}
}

func TestStartArkCompatProxyServesNormalizedInfoAndProxiesOtherRoutes(t *testing.T) {
	t.Parallel()

	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v1/info":
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"signerPubkey":"signer-only"}`))
		case "/v1/health":
			writer.WriteHeader(http.StatusOK)
			_, _ = writer.Write([]byte(`ok`))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer backend.Close()

	proxyURL, cleanup, err := startArkCompatProxy(backend.URL, arkServerCompatibility{
		normalizedInfo: []byte(`{"signerPubkey":"signer-only","forfeitPubkey":"signer-only"}`),
	})
	if err != nil {
		t.Fatalf("startArkCompatProxy: %v", err)
	}
	t.Cleanup(cleanup)

	infoRequest, err := http.NewRequestWithContext(context.Background(), http.MethodGet, proxyURL+"/v1/info", nil)
	if err != nil {
		t.Fatalf("new info request: %v", err)
	}
	infoResponse, err := http.DefaultClient.Do(infoRequest)
	if err != nil {
		t.Fatalf("fetch proxied info: %v", err)
	}
	defer infoResponse.Body.Close()
	infoBody, err := io.ReadAll(infoResponse.Body)
	if err != nil {
		t.Fatalf("read proxied info body: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(infoBody, &payload); err != nil {
		t.Fatalf("decode proxied info: %v", err)
	}
	if payload["forfeitPubkey"] != "signer-only" {
		t.Fatalf("expected normalized info response, received %#v", payload)
	}

	healthResponse, err := http.Get(proxyURL + "/v1/health")
	if err != nil {
		t.Fatalf("fetch proxied health: %v", err)
	}
	defer healthResponse.Body.Close()
	healthBody, err := io.ReadAll(healthResponse.Body)
	if err != nil {
		t.Fatalf("read proxied health body: %v", err)
	}
	if string(healthBody) != "ok" {
		t.Fatalf("expected proxied health response, received %q", string(healthBody))
	}
}

func TestRewriteCompatProxyPathPreservesModernIndexerRoutes(t *testing.T) {
	t.Parallel()

	got := rewriteCompatProxyPath("/parker-ark-compat/v1/indexer/vtxos", false)
	if got != "/v1/indexer/vtxos" {
		t.Fatalf("expected modern indexer route to be preserved, received %q", got)
	}
}

func TestRewriteCompatProxyPathSupportsLegacyIndexerRoutes(t *testing.T) {
	t.Parallel()

	got := rewriteCompatProxyPath("/parker-ark-compat/v1/indexer/vtxos", true)
	if got != "/v1/vtxos" {
		t.Fatalf("expected legacy indexer route rewrite, received %q", got)
	}
}

func TestDetectLegacyIndexerPathRewrite(t *testing.T) {
	t.Parallel()

	t.Run("modern", func(t *testing.T) {
		t.Parallel()

		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			switch request.URL.Path {
			case "/v1/indexer/vtxos":
				writer.WriteHeader(http.StatusBadRequest)
				_, _ = writer.Write([]byte(`{"message":"missing outpoints or scripts filter"}`))
			default:
				http.NotFound(writer, request)
			}
		}))
		defer server.Close()

		rewrite, err := detectLegacyIndexerPathRewrite(context.Background(), http.DefaultClient, server.URL)
		if err != nil {
			t.Fatalf("detectLegacyIndexerPathRewrite: %v", err)
		}
		if rewrite {
			t.Fatalf("expected modern indexer route support")
		}
	})

	t.Run("legacy", func(t *testing.T) {
		t.Parallel()

		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			http.NotFound(writer, request)
		}))
		defer server.Close()

		rewrite, err := detectLegacyIndexerPathRewrite(context.Background(), http.DefaultClient, server.URL)
		if err != nil {
			t.Fatalf("detectLegacyIndexerPathRewrite: %v", err)
		}
		if !rewrite {
			t.Fatalf("expected legacy indexer route rewrite to be enabled")
		}
	})
}

func TestRepairStoredCompatServerURLRecognizesCompatPath(t *testing.T) {
	t.Parallel()

	if !isCompatArkServerURL("http://127.0.0.1:4000/parker-ark-compat") {
		t.Fatalf("expected compat URL to be recognized")
	}
	if isCompatArkServerURL("http://127.0.0.1:7070") {
		t.Fatalf("did not expect canonical Ark URL to be treated as compat URL")
	}
}
