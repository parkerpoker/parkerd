package wallet

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestFetchExplorerTipHeightParsesResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/blocks/tip/height" {
			t.Fatalf("unexpected path %q", request.URL.Path)
		}
		_, _ = writer.Write([]byte("12345\n"))
	}))
	defer server.Close()

	height, err := fetchExplorerTipHeight(server.URL)
	if err != nil {
		t.Fatalf("fetch explorer tip height: %v", err)
	}
	if height != 12345 {
		t.Fatalf("expected tip height 12345, got %d", height)
	}
}

func TestFetchExplorerTxStatusParsesResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/tx/deadbeef/status" {
			t.Fatalf("unexpected path %q", request.URL.Path)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"confirmed":true,"block_height":321,"block_time":1700000000}`))
	}))
	defer server.Close()

	status, err := fetchExplorerTxStatus(server.URL, "deadbeef")
	if err != nil {
		t.Fatalf("fetch explorer tx status: %v", err)
	}
	if status.TxID != "deadbeef" {
		t.Fatalf("expected txid deadbeef, got %q", status.TxID)
	}
	if !status.Confirmed {
		t.Fatal("expected tx to be confirmed")
	}
	if status.BlockHeight != 321 {
		t.Fatalf("expected block height 321, got %d", status.BlockHeight)
	}
	if status.BlockTime != 1700000000 {
		t.Fatalf("expected block time 1700000000, got %d", status.BlockTime)
	}
	if status.ObservedAt == "" {
		t.Fatal("expected observed-at timestamp")
	}
}

func TestChainTipUsesFreshCacheOnlyWhenLiveFetchFails(t *testing.T) {
	successServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write([]byte("222"))
	}))
	defer successServer.Close()

	runtime := NewRuntime(RuntimeConfig{ProfileDir: t.TempDir()})
	if err := runtime.store.Save(PlayerProfileState{
		CachedArkConfig: &CustodyArkConfig{ExplorerURL: successServer.URL},
		CachedChainTip: &ChainTipStatus{
			Height:     111,
			ObservedAt: walletNowISO(),
		},
		ProfileName: "alice",
	}); err != nil {
		t.Fatalf("seed profile state: %v", err)
	}

	status, err := runtime.ChainTip("alice")
	if err != nil {
		t.Fatalf("chain tip live success: %v", err)
	}
	if status.Height != 222 {
		t.Fatalf("expected live tip height 222, got %d", status.Height)
	}

	failingServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Error(writer, "boom", http.StatusServiceUnavailable)
	}))
	defer failingServer.Close()

	if err := runtime.store.Save(PlayerProfileState{
		CachedArkConfig: &CustodyArkConfig{ExplorerURL: failingServer.URL},
		CachedChainTip: &ChainTipStatus{
			Height:     333,
			ObservedAt: walletNowISO(),
		},
		ProfileName: "bob",
	}); err != nil {
		t.Fatalf("seed fallback profile state: %v", err)
	}

	status, err = runtime.ChainTip("bob")
	if err != nil {
		t.Fatalf("chain tip live failure fallback: %v", err)
	}
	if status.Height != 333 {
		t.Fatalf("expected cached tip height 333 on live failure, got %d", status.Height)
	}
}

func TestChainTipRejectsStaleCache(t *testing.T) {
	failingServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Error(writer, "boom", http.StatusBadGateway)
	}))
	defer failingServer.Close()

	runtime := NewRuntime(RuntimeConfig{ProfileDir: t.TempDir()})
	if err := runtime.store.Save(PlayerProfileState{
		CachedArkConfig: &CustodyArkConfig{ExplorerURL: failingServer.URL},
		CachedChainTip: &ChainTipStatus{
			Height:     444,
			ObservedAt: time.Now().Add(-(maxCachedChainTipAge + time.Second)).UTC().Format(time.RFC3339Nano),
		},
		ProfileName: "alice",
	}); err != nil {
		t.Fatalf("seed stale cache profile: %v", err)
	}

	if _, err := runtime.ChainTip("alice"); err == nil {
		t.Fatal("expected stale cached tip to be rejected")
	}
}
