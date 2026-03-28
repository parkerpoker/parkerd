package wallet

import (
	"encoding/hex"
	"path/filepath"
	"testing"

	"github.com/btcsuite/btcd/btcutil/bech32"
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

	result, err := runtime.Bootstrap("alice", "Alice", "")
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
	if loaded.WalletPrivateKeyHex != result.State.WalletPrivateKeyHex {
		t.Fatalf("expected stored wallet key %q, received %q", result.State.WalletPrivateKeyHex, loaded.WalletPrivateKeyHex)
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

	if _, err := runtime.Bootstrap("bob", "", ""); err != nil {
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

func TestBootstrapUsesWalletNsec(t *testing.T) {
	t.Parallel()

	baseDir := t.TempDir()
	runtime := NewRuntime(RuntimeConfig{
		ArkServerURL:      "http://127.0.0.1:7070",
		Network:           "regtest",
		ProfileDir:        filepath.Join(baseDir, "profiles"),
		RunDir:            filepath.Join(baseDir, "runs"),
		UseMockSettlement: true,
	})

	expectedWalletHex := "1111111111111111111111111111111111111111111111111111111111111111"
	result, err := runtime.Bootstrap("alice", "Alice", mustEncodeWalletNsec(t, expectedWalletHex))
	if err != nil {
		t.Fatalf("bootstrap with wallet nsec: %v", err)
	}

	expectedIdentity, err := localIdentity(expectedWalletHex)
	if err != nil {
		t.Fatalf("derive expected identity: %v", err)
	}

	if result.State.WalletPrivateKeyHex != expectedWalletHex {
		t.Fatalf("expected wallet key %q, received %q", expectedWalletHex, result.State.WalletPrivateKeyHex)
	}
	if result.Identity.PlayerID != expectedIdentity.PlayerID {
		t.Fatalf("expected player ID %q, received %q", expectedIdentity.PlayerID, result.Identity.PlayerID)
	}

	loaded, err := runtime.store.Load("alice")
	if err != nil {
		t.Fatalf("load profile: %v", err)
	}
	if loaded == nil {
		t.Fatal("expected saved profile state")
	}
	if loaded.WalletPrivateKeyHex != expectedWalletHex {
		t.Fatalf("expected persisted wallet key %q, received %q", expectedWalletHex, loaded.WalletPrivateKeyHex)
	}
}

func TestWalletNsecReturnsStoredWalletSeed(t *testing.T) {
	t.Parallel()

	baseDir := t.TempDir()
	runtime := NewRuntime(RuntimeConfig{
		ArkServerURL:      "http://127.0.0.1:7070",
		Network:           "regtest",
		ProfileDir:        filepath.Join(baseDir, "profiles"),
		RunDir:            filepath.Join(baseDir, "runs"),
		UseMockSettlement: true,
	})

	expectedWalletHex := "1111111111111111111111111111111111111111111111111111111111111111"
	expectedWalletNsec := mustEncodeWalletNsec(t, expectedWalletHex)
	if _, err := runtime.Bootstrap("alice", "Alice", expectedWalletNsec); err != nil {
		t.Fatalf("bootstrap with wallet nsec: %v", err)
	}

	walletNsec, err := runtime.WalletNsec("alice")
	if err != nil {
		t.Fatalf("wallet nsec: %v", err)
	}
	if walletNsec != expectedWalletNsec {
		t.Fatalf("expected wallet nsec %q, received %q", expectedWalletNsec, walletNsec)
	}
}

func TestWalletNsecEncodesExistingRandomWalletSeed(t *testing.T) {
	t.Parallel()

	baseDir := t.TempDir()
	runtime := NewRuntime(RuntimeConfig{
		ArkServerURL:      "http://127.0.0.1:7070",
		Network:           "regtest",
		ProfileDir:        filepath.Join(baseDir, "profiles"),
		RunDir:            filepath.Join(baseDir, "runs"),
		UseMockSettlement: true,
	})

	bootstrap, err := runtime.Bootstrap("alice", "Alice", "")
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	walletNsec, err := runtime.WalletNsec("alice")
	if err != nil {
		t.Fatalf("wallet nsec: %v", err)
	}
	decoded, err := decodeWalletNsec(walletNsec)
	if err != nil {
		t.Fatalf("decode wallet nsec: %v", err)
	}
	if decoded != bootstrap.State.WalletPrivateKeyHex {
		t.Fatalf("expected decoded wallet key %q, received %q", bootstrap.State.WalletPrivateKeyHex, decoded)
	}
}

func TestBootstrapRejectsDifferentWalletNsecForExistingProfile(t *testing.T) {
	t.Parallel()

	baseDir := t.TempDir()
	runtime := NewRuntime(RuntimeConfig{
		ArkServerURL:      "http://127.0.0.1:7070",
		Network:           "regtest",
		ProfileDir:        filepath.Join(baseDir, "profiles"),
		RunDir:            filepath.Join(baseDir, "runs"),
		UseMockSettlement: true,
	})

	firstWalletHex := "1111111111111111111111111111111111111111111111111111111111111111"
	secondWalletHex := "2222222222222222222222222222222222222222222222222222222222222222"
	if _, err := runtime.Bootstrap("alice", "Alice", mustEncodeWalletNsec(t, firstWalletHex)); err != nil {
		t.Fatalf("bootstrap with initial wallet nsec: %v", err)
	}

	_, err := runtime.Bootstrap("alice", "Alice", mustEncodeWalletNsec(t, secondWalletHex))
	if err == nil {
		t.Fatal("expected bootstrap to reject a different wallet nsec")
	}
	if err.Error() != walletNsecMismatchError {
		t.Fatalf("expected mismatch error %q, received %q", walletNsecMismatchError, err.Error())
	}

	loaded, err := runtime.store.Load("alice")
	if err != nil {
		t.Fatalf("load profile: %v", err)
	}
	if loaded == nil {
		t.Fatal("expected saved profile state")
	}
	if loaded.WalletPrivateKeyHex != firstWalletHex {
		t.Fatalf("expected stored wallet key to remain %q, received %q", firstWalletHex, loaded.WalletPrivateKeyHex)
	}
}

func mustEncodeWalletNsec(t *testing.T, privateKeyHex string) string {
	t.Helper()

	raw, err := hex.DecodeString(privateKeyHex)
	if err != nil {
		t.Fatalf("decode wallet private key hex: %v", err)
	}
	walletNsec, err := bech32.EncodeFromBase256("nsec", raw)
	if err != nil {
		t.Fatalf("encode wallet nsec: %v", err)
	}
	return walletNsec
}
