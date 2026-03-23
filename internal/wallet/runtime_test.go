package wallet

import (
	"path/filepath"
	"testing"
)

func TestBootstrapCreatesCompatibleProfileState(t *testing.T) {
	t.Parallel()

	baseDir := t.TempDir()
	runtime := NewRuntime(RuntimeConfig{
		ArkServerURL:      "http://127.0.0.1:7070",
		Network:           "regtest",
		ProfileDir:        filepath.Join(baseDir, "profiles"),
		RunDir:            filepath.Join(baseDir, "runs"),
		UseMockSettlement: true,
	})

	result, err := runtime.Bootstrap("alice", "Alice")
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	if result.State.ProfileName != "alice" {
		t.Fatalf("expected profile name alice, received %q", result.State.ProfileName)
	}
	if result.State.Nickname != "Alice" {
		t.Fatalf("expected nickname Alice, received %q", result.State.Nickname)
	}
	if result.Identity.PlayerID == "" || result.Identity.PublicKeyHex == "" || result.Identity.PrivateKeyHex == "" {
		t.Fatalf("expected populated identity, received %+v", result.Identity)
	}
	if result.Wallet.TotalSats != 50_000 || result.Wallet.AvailableSats != 50_000 {
		t.Fatalf("expected mock wallet totals, received %+v", result.Wallet)
	}

	loaded, err := runtime.store.Load("alice")
	if err != nil {
		t.Fatalf("load profile: %v", err)
	}
	if loaded == nil || loaded.WalletPrivateKeyHex == "" || loaded.MockWallet == nil {
		t.Fatalf("expected saved compatible profile state, received %+v", loaded)
	}
}

func TestGetWalletUsesSavedMockWallet(t *testing.T) {
	t.Parallel()

	baseDir := t.TempDir()
	runtime := NewRuntime(RuntimeConfig{
		ArkServerURL:      "http://127.0.0.1:7070",
		Network:           "regtest",
		ProfileDir:        filepath.Join(baseDir, "profiles"),
		RunDir:            filepath.Join(baseDir, "runs"),
		UseMockSettlement: true,
	})

	if _, err := runtime.Bootstrap("bob", ""); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if err := runtime.Faucet("bob", 12_345); err != nil {
		t.Fatalf("faucet: %v", err)
	}

	wallet, err := runtime.GetWallet("bob")
	if err != nil {
		t.Fatalf("get wallet: %v", err)
	}
	if wallet.TotalSats != 62_345 || wallet.AvailableSats != 62_345 {
		t.Fatalf("expected faucet-adjusted mock wallet, received %+v", wallet)
	}
}
