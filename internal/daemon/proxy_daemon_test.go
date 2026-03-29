package daemon

import (
	"encoding/hex"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/btcutil/bech32"
	cfg "github.com/parkerpoker/parkerd/internal/config"
)

func TestBootstrapRPCRejectsInvalidWalletNsec(t *testing.T) {
	config, err := cfg.ResolveRuntimeConfig(map[string]string{
		"datadir": t.TempDir(),
		"mock":    "true",
	})
	if err != nil {
		t.Fatalf("resolve runtime config: %v", err)
	}

	runtime, err := newDaemonRuntimeAdapter("alice", config, "player")
	if err != nil {
		t.Fatalf("new daemon runtime adapter: %v", err)
	}
	defer runtime.Close()

	daemon := &ProxyDaemon{profileName: "alice", runtime: runtime}
	_, err = daemon.handleRuntimeRequest("bootstrap", map[string]any{
		"nickname":   "Alice",
		"walletNsec": "not-an-nsec",
	})
	if err == nil {
		t.Fatal("expected bootstrap RPC to reject an invalid wallet nsec")
	}
	if !strings.Contains(err.Error(), "invalid walletNsec") {
		t.Fatalf("expected clear wallet nsec validation error, received %q", err.Error())
	}
}

func TestWalletNsecRPCReturnsStoredSeed(t *testing.T) {
	config, err := cfg.ResolveRuntimeConfig(map[string]string{
		"datadir": t.TempDir(),
		"mock":    "true",
	})
	if err != nil {
		t.Fatalf("resolve runtime config: %v", err)
	}

	runtime, err := newDaemonRuntimeAdapter("alice", config, "player")
	if err != nil {
		t.Fatalf("new daemon runtime adapter: %v", err)
	}
	defer runtime.Close()

	privateKey, err := hex.DecodeString("1111111111111111111111111111111111111111111111111111111111111111")
	if err != nil {
		t.Fatalf("decode private key: %v", err)
	}
	expectedWalletNsec, err := bech32.EncodeFromBase256("nsec", privateKey)
	if err != nil {
		t.Fatalf("encode wallet nsec: %v", err)
	}
	if _, err := runtime.Bootstrap("Alice", expectedWalletNsec); err != nil {
		t.Fatalf("bootstrap runtime: %v", err)
	}

	daemon := &ProxyDaemon{profileName: "alice", runtime: runtime}
	value, err := daemon.handleRuntimeRequest("walletNsec", nil)
	if err != nil {
		t.Fatalf("wallet nsec rpc: %v", err)
	}

	walletNsec, ok := value.(string)
	if !ok {
		t.Fatalf("expected wallet nsec string, received %T", value)
	}
	if walletNsec != expectedWalletNsec {
		t.Fatalf("expected wallet nsec %q, received %q", expectedWalletNsec, walletNsec)
	}
}
