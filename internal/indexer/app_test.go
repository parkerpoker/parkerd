package indexer

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	cfg "github.com/danieldresner/arkade_fun/internal/config"
)

func TestIndexerStoresAndServesPublicTables(t *testing.T) {
	t.Parallel()

	rootDir := t.TempDir()
	runtimeConfig, err := cfg.ResolveRuntimeConfig(map[string]string{
		"cache-type":    "memory",
		"datadir":       rootDir,
		"db-path":       filepath.Join(rootDir, "storage", "core.sqlite"),
		"event-db-path": filepath.Join(rootDir, "storage", "events.badger"),
		"mock":          "true",
		"network":       "regtest",
	})
	if err != nil {
		t.Fatalf("resolve runtime config: %v", err)
	}

	app, err := NewApp(runtimeConfig)
	if err != nil {
		t.Fatalf("create app: %v", err)
	}
	defer app.Close()

	tableID := "11111111-1111-4111-8111-111111111111"
	advertisement := map[string]any{
		"protocolVersion":       "poker/v1",
		"networkId":             "regtest",
		"tableId":               tableID,
		"hostPeerId":            "peer-host-1",
		"hostPeerUrl":           "parker://127.0.0.1:7777",
		"tableName":             "Public Test Table",
		"stakes":                map[string]int{"smallBlindSats": 50, "bigBlindSats": 100},
		"currency":              "sats",
		"seatCount":             2,
		"occupiedSeats":         1,
		"spectatorsAllowed":     true,
		"hostModeCapabilities":  []string{"host-dealer-v1"},
		"witnessCount":          1,
		"buyInMinSats":          4000,
		"buyInMaxSats":          4000,
		"visibility":            "public",
		"latencyHintMs":         25,
		"adExpiresAt":           "2026-03-23T00:00:10.000Z",
		"hostProtocolPubkeyHex": "020202020202020202020202020202020202020202020202020202020202020202",
		"hostSignatureHex":      strings.Repeat("aa", 64),
	}
	publicState := map[string]any{
		"snapshotId":           "22222222-2222-4222-8222-222222222222",
		"tableId":              tableID,
		"epoch":                1,
		"status":               "ready",
		"handId":               nil,
		"handNumber":           0,
		"phase":                nil,
		"actingSeatIndex":      nil,
		"dealerSeatIndex":      0,
		"board":                []string{},
		"seatedPlayers":        []any{},
		"chipBalances":         map[string]int{},
		"roundContributions":   map[string]int{},
		"totalContributions":   map[string]int{},
		"potSats":              0,
		"currentBetSats":       0,
		"minRaiseToSats":       100,
		"livePlayerIds":        []string{},
		"foldedPlayerIds":      []string{},
		"dealerCommitment":     nil,
		"previousSnapshotHash": nil,
		"latestEventHash":      nil,
		"updatedAt":            "2026-03-23T00:00:00.000Z",
	}

	postJSON(t, app, "/api/indexer/table-ads", advertisement, http.StatusOK)
	postJSON(t, app, "/api/indexer/table-updates", map[string]any{
		"type":          "PublicTableSnapshot",
		"tableId":       tableID,
		"advertisement": advertisement,
		"publicState":   publicState,
		"publishedAt":   "2026-03-23T00:00:01.000Z",
	}, http.StatusOK)

	listResponse := performRequest(t, app, http.MethodGet, "/api/public/tables", nil)
	if listResponse.Code != http.StatusOK {
		t.Fatalf("list tables status = %d, body=%s", listResponse.Code, listResponse.Body.String())
	}
	var list []PublicTableView
	if err := json.Unmarshal(listResponse.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 public table, got %d", len(list))
	}
	if list[0].Advertisement.TableID != tableID {
		t.Fatalf("expected table ID %s, got %s", tableID, list[0].Advertisement.TableID)
	}
	if list[0].LatestState == nil || list[0].LatestState.SnapshotID != "22222222-2222-4222-8222-222222222222" {
		t.Fatalf("unexpected latest state: %+v", list[0].LatestState)
	}

	tableResponse := performRequest(t, app, http.MethodGet, "/api/public/tables/"+tableID, nil)
	if tableResponse.Code != http.StatusOK {
		t.Fatalf("get table status = %d, body=%s", tableResponse.Code, tableResponse.Body.String())
	}
	var view PublicTableView
	if err := json.Unmarshal(tableResponse.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode view: %v", err)
	}
	if len(view.RecentUpdates) != 1 {
		t.Fatalf("expected 1 recent update, got %d", len(view.RecentUpdates))
	}
}

func postJSON(t *testing.T, handler http.Handler, path string, payload any, expectedStatus int) {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	response := performRequest(t, handler, http.MethodPost, path, body)
	if response.Code != expectedStatus {
		t.Fatalf("POST %s status = %d, body=%s", path, response.Code, response.Body.String())
	}
}

func performRequest(t *testing.T, handler http.Handler, method, path string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, bytes.NewReader(body))
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}
