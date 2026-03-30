package wallet

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	arklib "github.com/arkade-os/arkd/pkg/ark-lib"
	arkscript "github.com/arkade-os/arkd/pkg/ark-lib/script"
	arktree "github.com/arkade-os/arkd/pkg/ark-lib/tree"
	arksdk "github.com/arkade-os/go-sdk"
	sdkstore "github.com/arkade-os/go-sdk/store"
	sdktypes "github.com/arkade-os/go-sdk/types"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/parkerpoker/parkerd/internal/tablecustody"
)

type Runtime struct {
	config RuntimeConfig
	mu     sync.Mutex
	store  *ProfileStore
}

const nigiriFaucetTimeout = 45 * time.Second

type RuntimeConfig struct {
	ArkServerURL      string
	Network           string
	NigiriDatadir     string
	ProfileDir        string
	RunDir            string
	UseMockSettlement bool
}

func NewRuntime(config RuntimeConfig) *Runtime {
	return &Runtime{
		config: config,
		store:  NewProfileStore(config.ProfileDir),
	}
}

func (runtime *Runtime) EnsureProfile(profileName, nickname, walletNsec string) (PlayerProfileState, error) {
	state, err := runtime.ensureBootstrap(profileName, nickname, walletNsec)
	if err != nil {
		return PlayerProfileState{}, err
	}
	return *state, nil
}

func (runtime *Runtime) Bootstrap(profileName, nickname, walletNsec string) (BootstrapResult, error) {
	state, err := runtime.EnsureProfile(profileName, nickname, walletNsec)
	if err != nil {
		return BootstrapResult{}, err
	}

	identity, err := localIdentity(state.WalletPrivateKeyHex)
	if err != nil {
		return BootstrapResult{}, err
	}

	wallet, err := runtime.GetWallet(profileName)
	if err != nil {
		return BootstrapResult{}, err
	}

	return BootstrapResult{
		Identity: identity,
		State:    state,
		Wallet:   wallet,
	}, nil
}

func (runtime *Runtime) WalletNsec(profileName string) (string, error) {
	state, err := runtime.ensureBootstrap(profileName, "", "")
	if err != nil {
		return "", err
	}
	return encodeWalletNsec(state.WalletPrivateKeyHex)
}

func (runtime *Runtime) GetWallet(profileName string) (WalletSummary, error) {
	state, err := runtime.ensureBootstrap(profileName, "", "")
	if err != nil {
		return WalletSummary{}, err
	}

	if runtime.config.UseMockSettlement {
		if state.MockWallet == nil {
			identity, err := localIdentity(state.WalletPrivateKeyHex)
			if err != nil {
				return WalletSummary{}, err
			}
			mock := createMockWallet(identity.PlayerID)
			state.MockWallet = &mock
			if err := runtime.store.Save(*state); err != nil {
				return WalletSummary{}, err
			}
		}
		return *state.MockWallet, nil
	}

	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return runtime.getWalletLocked(profileName, *state)
}

func (runtime *Runtime) ArkConfig(profileName string) (CustodyArkConfig, error) {
	state, err := runtime.ensureBootstrap(profileName, "", "")
	if err != nil {
		return CustodyArkConfig{}, err
	}
	if runtime.config.UseMockSettlement {
		return CustodyArkConfig{
			ArkServerURL:        runtime.config.ArkServerURL,
			DustSats:            1,
			ForfeitPubkeyHex:    mockCustodyPubkeyHex("parker-mock-ark-forfeit"),
			SignerPubkeyHex:     mockCustodyPubkeyHex("parker-mock-ark-signer"),
			UnilateralExitDelay: arklib.RelativeLocktime{Type: arklib.LocktimeTypeBlock, Value: 512},
		}, nil
	}

	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	client, unlock, cleanup, err := runtime.openArkClient(profileName, *state)
	if err != nil {
		return CustodyArkConfig{}, err
	}
	defer cleanup()
	if err := unlock(); err != nil {
		return CustodyArkConfig{}, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	config, err := client.GetConfigData(ctx)
	if err != nil {
		return CustodyArkConfig{}, err
	}
	if config == nil || config.SignerPubKey == nil || config.ForfeitPubKey == nil {
		return CustodyArkConfig{}, errors.New("ark client config is incomplete")
	}
	return CustodyArkConfig{
		ArkServerURL:          runtime.config.ArkServerURL,
		CheckpointTapscript:   config.CheckpointTapscript,
		DustSats:              config.Dust,
		ExplorerURL:           config.ExplorerURL,
		ForfeitAddress:        config.ForfeitAddress,
		ForfeitPubkeyHex:      hex.EncodeToString(config.ForfeitPubKey.SerializeCompressed()),
		Network:               config.Network,
		OffchainInputFeeSats:  parseSatsString(config.Fees.IntentFees.OffchainInput),
		OffchainOutputFeeSats: parseSatsString(config.Fees.IntentFees.OffchainOutput),
		OnchainInputFeeSats:   int(config.Fees.IntentFees.OnchainInput),
		OnchainOutputFeeSats:  int(config.Fees.IntentFees.OnchainOutput),
		SignerPubkeyHex:       hex.EncodeToString(config.SignerPubKey.SerializeCompressed()),
		UnilateralExitDelay:   config.UnilateralExitDelay,
	}, nil
}

func (runtime *Runtime) getWalletLocked(profileName string, state PlayerProfileState) (WalletSummary, error) {
	debugWalletf("wallet summary start profile=%s", profileName)
	client, unlock, cleanup, err := runtime.openArkClient(profileName, state)
	if err != nil {
		return WalletSummary{}, err
	}
	defer cleanup()
	if err := unlock(); err != nil {
		return WalletSummary{}, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	balance, err := client.Balance(ctx, false)
	if err != nil {
		return WalletSummary{}, err
	}
	debugWalletf("wallet summary balance ready profile=%s offchain=%d onchain=%d", profileName, balance.OffchainBalance.Total, onchainBalanceTotal(balance))
	arkAddress, boardingAddress, err := runtime.currentWalletAddresses(ctx, client)
	if err != nil {
		return WalletSummary{}, err
	}
	debugWalletf("wallet summary addresses ready profile=%s ark=%s boarding=%s", profileName, arkAddress, boardingAddress)

	return WalletSummary{
		AvailableSats:       int(balance.OffchainBalance.Total),
		TotalSats:           int(balance.OffchainBalance.Total + balance.OnchainBalance.SpendableAmount),
		WalletSpendableSats: int(balance.OffchainBalance.Total),
		TableLockedSats:     0,
		PendingExitSats:     0,
		ArkAddress:          arkAddress,
		BoardingAddress:     boardingAddress,
	}, nil
}

func (runtime *Runtime) ListVtxoRefs(profileName string) ([]tablecustody.VTXORef, error) {
	state, err := runtime.ensureBootstrap(profileName, "", "")
	if err != nil {
		return nil, err
	}
	if runtime.config.UseMockSettlement {
		wallet, err := runtime.GetWallet(profileName)
		if err != nil {
			return nil, err
		}
		arkConfig, err := runtime.ArkConfig(profileName)
		if err != nil {
			return nil, err
		}
		identity, err := localIdentity(state.WalletPrivateKeyHex)
		if err != nil {
			return nil, err
		}
		ownerPubkeyBytes, err := hex.DecodeString(identity.PublicKeyHex)
		if err != nil {
			return nil, err
		}
		ownerPubkey, err := btcec.ParsePubKey(ownerPubkeyBytes)
		if err != nil {
			return nil, err
		}
		signerPubkeyBytes, err := hex.DecodeString(arkConfig.SignerPubkeyHex)
		if err != nil {
			return nil, err
		}
		signerPubkey, err := btcec.ParsePubKey(signerPubkeyBytes)
		if err != nil {
			return nil, err
		}
		defaultScript := arkscript.NewDefaultVtxoScript(ownerPubkey, signerPubkey, arkConfig.UnilateralExitDelay)
		defaultTapscripts, err := defaultScript.Encode()
		if err != nil {
			return nil, err
		}
		tapKey, _, err := defaultScript.TapTree()
		if err != nil {
			return nil, err
		}
		pkScript, err := arkscript.P2TRScript(tapKey)
		if err != nil {
			return nil, err
		}
		arkTxID := tablecustody.HashValue(map[string]any{
			"profile": profileName,
			"type":    "mock-ark-vtxo",
		})
		return []tablecustody.VTXORef{{
			AmountSats:    wallet.AvailableSats,
			ArkIntentID:   "mock-intent-" + suffix(profileName, 8),
			ArkTxID:       arkTxID,
			ExpiresAt:     addDurationISO(24 * time.Hour),
			OwnerPlayerID: derivePlayerIDFromState(*state),
			Script:        hex.EncodeToString(pkScript),
			Tapscripts:    append([]string(nil), defaultTapscripts...),
			TxID:          arkTxID,
			VOut:          0,
		}}, nil
	}

	arkConfig, err := runtime.ArkConfig(profileName)
	if err != nil {
		return nil, err
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	client, unlock, cleanup, err := runtime.openArkClient(profileName, *state)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	if err := unlock(); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	spendable, _, err := client.ListVtxos(ctx)
	if err != nil {
		return nil, err
	}
	refs := make([]tablecustody.VTXORef, 0, len(spendable))
	identity, err := localIdentity(state.WalletPrivateKeyHex)
	if err != nil {
		return nil, err
	}
	ownerPubkeyBytes, err := hex.DecodeString(identity.PublicKeyHex)
	if err != nil {
		return nil, err
	}
	ownerPubkey, err := btcec.ParsePubKey(ownerPubkeyBytes)
	if err != nil {
		return nil, err
	}
	signerPubkeyBytes, err := hex.DecodeString(arkConfig.SignerPubkeyHex)
	if err != nil {
		return nil, err
	}
	signerPubkey, err := btcec.ParsePubKey(signerPubkeyBytes)
	if err != nil {
		return nil, err
	}
	defaultScript := arkscript.NewDefaultVtxoScript(ownerPubkey, signerPubkey, arkConfig.UnilateralExitDelay)
	defaultTapscripts, err := defaultScript.Encode()
	if err != nil {
		return nil, err
	}
	for _, vtxo := range spendable {
		refs = append(refs, tablecustody.VTXORef{
			AmountSats:    int(vtxo.Amount),
			ArkTxID:       vtxo.ArkTxid,
			ExpiresAt:     vtxo.ExpiresAt.UTC().Format(time.RFC3339),
			OwnerPlayerID: derivePlayerIDFromState(*state),
			Script:        vtxo.Script,
			Tapscripts:    append([]string(nil), defaultTapscripts...),
			TxID:          vtxo.Txid,
			VOut:          vtxo.VOut,
		})
	}
	return refs, nil
}

func (runtime *Runtime) BuildBuyInFundingBundle(profileName string, amountSats int) (CustodyFundingBundle, error) {
	refs, err := runtime.ListVtxoRefs(profileName)
	if err != nil {
		return CustodyFundingBundle{}, err
	}
	selected := make([]tablecustody.VTXORef, 0, len(refs))
	total := 0
	for _, ref := range refs {
		selected = append(selected, ref)
		total += ref.AmountSats
		if total >= amountSats {
			break
		}
	}
	if total < amountSats {
		return CustodyFundingBundle{}, fmt.Errorf("insufficient spendable vtxos: have %d need %d", total, amountSats)
	}
	state, err := runtime.ensureBootstrap(profileName, "", "")
	if err != nil {
		return CustodyFundingBundle{}, err
	}
	return CustodyFundingBundle{
		PlayerID:  derivePlayerIDFromState(*state),
		Refs:      selected,
		TotalSats: total,
	}, nil
}

func (runtime *Runtime) RegisterCustodyIntent(profileName string, request CustodyIntentRequest) (CustodyIntentResult, error) {
	if runtime.config.UseMockSettlement {
		intentID, err := randomHex(16)
		if err != nil {
			return CustodyIntentResult{}, err
		}
		return CustodyIntentResult{
			IntentID: "mock-intent-" + intentID,
			Refs:     append([]tablecustody.VTXORef(nil), request.Refs...),
		}, nil
	}

	state, err := runtime.ensureBootstrap(profileName, "", "")
	if err != nil {
		return CustodyIntentResult{}, err
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	client, unlock, cleanup, err := runtime.openArkClient(profileName, *state)
	if err != nil {
		return CustodyIntentResult{}, err
	}
	defer cleanup()
	if err := unlock(); err != nil {
		return CustodyIntentResult{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	intentID, err := client.RegisterIntent(ctx, sdkVtxosFromRefs(request.Refs), nil, request.Notes, request.Outputs, request.CosignerPubkeys)
	if err != nil {
		return CustodyIntentResult{}, err
	}
	return CustodyIntentResult{
		IntentID: intentID,
		Refs:     append([]tablecustody.VTXORef(nil), request.Refs...),
	}, nil
}

func (runtime *Runtime) SendCustodyOffChain(profileName string, receivers []sdktypes.Receiver) (CustodyIntentResult, error) {
	if runtime.config.UseMockSettlement {
		txID, err := randomHex(16)
		if err != nil {
			return CustodyIntentResult{}, err
		}
		return CustodyIntentResult{TxID: "mock-send-" + txID}, nil
	}

	state, err := runtime.ensureBootstrap(profileName, "", "")
	if err != nil {
		return CustodyIntentResult{}, err
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	client, unlock, cleanup, err := runtime.openArkClient(profileName, *state)
	if err != nil {
		return CustodyIntentResult{}, err
	}
	defer cleanup()
	if err := unlock(); err != nil {
		return CustodyIntentResult{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	txID, err := client.SendOffChain(ctx, false, receivers)
	if err != nil {
		return CustodyIntentResult{}, err
	}
	return CustodyIntentResult{TxID: txID}, nil
}

func (runtime *Runtime) SignCustodyTransaction(profileName, tx string) (string, error) {
	if runtime.config.UseMockSettlement {
		return "mock-signed-" + tx, nil
	}
	state, err := runtime.ensureBootstrap(profileName, "", "")
	if err != nil {
		return "", err
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	client, unlock, cleanup, err := runtime.openArkClient(profileName, *state)
	if err != nil {
		return "", err
	}
	defer cleanup()
	if err := unlock(); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return client.SignTransaction(ctx, tx)
}

func (runtime *Runtime) NewCustodySignerSession(profileName, derivationPath string) (CustodySignerSession, error) {
	state, err := runtime.ensureBootstrap(profileName, "", "")
	if err != nil {
		return CustodySignerSession{}, err
	}
	sessionKey, pubkeyHex, err := deriveSignerPrivateKeyHex(state.WalletPrivateKeyHex, derivationPath)
	if err != nil {
		return CustodySignerSession{}, err
	}
	privBytes, err := hex.DecodeString(sessionKey)
	if err != nil {
		return CustodySignerSession{}, err
	}
	privKey, _ := btcec.PrivKeyFromBytes(privBytes)
	return CustodySignerSession{
		DerivationPath: derivationPath,
		PublicKeyHex:   pubkeyHex,
		Session:        arktree.NewTreeSignerSession(privKey),
	}, nil
}

func (runtime *Runtime) Faucet(profileName string, amountSats int) error {
	if runtime.config.UseMockSettlement {
		state, err := runtime.ensureBootstrap(profileName, "", "")
		if err != nil {
			return err
		}
		wallet, err := runtime.GetWallet(profileName)
		if err != nil {
			return err
		}
		wallet.AvailableSats += amountSats
		wallet.TotalSats += amountSats
		state.MockWallet = &wallet
		return runtime.store.Save(*state)
	}

	state, err := runtime.ensureBootstrap(profileName, "", "")
	if err != nil {
		return err
	}

	runtime.mu.Lock()
	defer runtime.mu.Unlock()

	wallet, err := runtime.getWalletLocked(profileName, *state)
	if err != nil {
		return err
	}
	debugWalletf("wallet faucet start profile=%s amount=%d boarding=%s", profileName, amountSats, wallet.BoardingAddress)

	args := []string{"faucet", wallet.BoardingAddress, satsToBitcoinString(amountSats)}
	if runtime.config.NigiriDatadir != "" {
		args = append([]string{"--datadir", runtime.config.NigiriDatadir}, args...)
	}
	ctx, cancel := context.WithTimeout(context.Background(), nigiriFaucetTimeout)
	defer cancel()

	commandName := runtime.nigiriCommandName()
	command := exec.CommandContext(ctx, commandName, args...)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err = command.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("nigiri faucet timed out after %s%s", nigiriFaucetTimeout, formatCommandOutput(stdout.String(), stderr.String()))
	}
	if err != nil {
		details := formatCommandOutput(stdout.String(), stderr.String())
		debugWalletf("wallet faucet failed profile=%s err=%v%s", profileName, err, details)
		return fmt.Errorf("nigiri faucet failed: %w%s", err, details)
	}
	debugWalletf("wallet faucet complete profile=%s", profileName)
	return nil
}

func (runtime *Runtime) Onboard(profileName string) (string, error) {
	if runtime.config.UseMockSettlement {
		return "", errors.New("onboard is not available in mock settlement mode")
	}

	state, err := runtime.ensureBootstrap(profileName, "", "")
	if err != nil {
		return "", err
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	client, unlock, cleanup, err := runtime.openArkClient(profileName, *state)
	if err != nil {
		return "", err
	}
	defer cleanup()
	if err := unlock(); err != nil {
		return "", err
	}

	boardingCtx, boardingCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer boardingCancel()
	debugWalletf("wallet onboard waiting for funds profile=%s", profileName)
	if err := runtime.waitForBoardingFunds(boardingCtx, client); err != nil {
		debugWalletf("wallet onboard wait failed profile=%s err=%v", profileName, err)
		return "", err
	}
	debugWalletf("wallet onboard funds detected profile=%s", profileName)

	settleCtx, settleCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer settleCancel()

	lastErr := error(nil)
	for {
		debugWalletf("wallet onboard settle attempt profile=%s", profileName)
		txid, err := client.Settle(settleCtx)
		if err == nil {
			debugWalletf("wallet onboard settled profile=%s txid=%s", profileName, txid)
			return txid, nil
		}
		lastErr = err
		debugWalletf("wallet onboard settle error profile=%s err=%v", profileName, err)
		if !isRetryableOnboardError(err) {
			return "", err
		}
		if settleCtx.Err() != nil {
			break
		}
		select {
		case <-settleCtx.Done():
		case <-time.After(500 * time.Millisecond):
		}
	}

	if lastErr == nil {
		lastErr = settleCtx.Err()
	}
	return "", fmt.Errorf("timed out waiting for Ark onboarding inputs: %w", lastErr)
}

func (runtime *Runtime) Offboard(profileName, address string, amountSats *int) (string, error) {
	if runtime.config.UseMockSettlement {
		return "", errors.New("offboard is not available in mock settlement mode")
	}

	state, err := runtime.ensureBootstrap(profileName, "", "")
	if err != nil {
		return "", err
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	client, unlock, cleanup, err := runtime.openArkClient(profileName, *state)
	if err != nil {
		return "", err
	}
	defer cleanup()
	if err := unlock(); err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	targetAmount := 0
	if amountSats != nil {
		targetAmount = *amountSats
	} else {
		balance, err := client.Balance(ctx, false)
		if err != nil {
			return "", err
		}
		targetAmount = int(balance.OffchainBalance.Total)
	}
	if targetAmount <= 0 {
		return "", errors.New("wallet has no offchain funds to offboard")
	}

	return client.CollaborativeExit(ctx, address, uint64(targetAmount), false)
}

func (runtime *Runtime) CreateDepositQuote(profileName string, amountSats int) (map[string]any, error) {
	if _, err := runtime.ensureBootstrap(profileName, "", ""); err != nil {
		return nil, err
	}
	return nil, errors.New("native Go deposit quotes are not implemented yet")
}

func (runtime *Runtime) SubmitWithdrawal(profileName string, amountSats int, invoice string) (map[string]any, error) {
	if _, err := runtime.ensureBootstrap(profileName, "", ""); err != nil {
		return nil, err
	}
	return nil, errors.New("native Go lightning withdrawals are not implemented yet")
}

func (runtime *Runtime) ensureBootstrap(profileName, nickname, walletNsec string) (*PlayerProfileState, error) {
	existing, err := runtime.store.Load(profileName)
	if err != nil {
		return nil, err
	}

	state := &PlayerProfileState{}
	if existing != nil {
		*state = *existing
	}

	seed, err := resolveWalletPrivateKeyHex(*state, walletNsec)
	if err != nil {
		return nil, err
	}
	if seed == "" {
		seed, err = randomHex(32)
		if err != nil {
			return nil, err
		}
	}

	if state.HandSeeds == nil {
		state.HandSeeds = map[string]string{}
	}
	if state.ProfileName == "" {
		state.ProfileName = profileName
	}
	if nickname != "" {
		state.Nickname = nickname
	}
	if state.Nickname == "" {
		state.Nickname = profileName
	}
	if state.PrivateKeyHex == "" {
		state.PrivateKeyHex = seed
	}
	if state.WalletPrivateKeyHex == "" {
		state.WalletPrivateKeyHex = state.PrivateKeyHex
	}
	if state.PrivateKeyHex == "" {
		state.PrivateKeyHex = state.WalletPrivateKeyHex
	}

	if runtime.config.UseMockSettlement && state.MockWallet == nil {
		identity, err := localIdentity(state.WalletPrivateKeyHex)
		if err != nil {
			return nil, err
		}
		mock := createMockWallet(identity.PlayerID)
		state.MockWallet = &mock
	}

	if err := runtime.store.Save(*state); err != nil {
		return nil, err
	}
	return state, nil
}

func (runtime *Runtime) openArkClient(profileName string, state PlayerProfileState) (arksdk.ArkClient, func() error, func(), error) {
	storeDir := filepath.Join(runtime.config.ProfileDir, slugProfile(profileName)+".arkade")
	if err := os.MkdirAll(storeDir, 0o755); err != nil {
		return nil, nil, nil, err
	}

	storeSvc, err := sdkstore.NewStore(sdkstore.Config{
		ConfigStoreType:  sdktypes.FileStore,
		AppDataStoreType: sdktypes.SQLStore,
		BaseDir:          storeDir,
	})
	if err != nil {
		return nil, nil, nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	realServerURL := runtime.config.ArkServerURL
	if err := repairStoredCompatServerURL(ctx, storeSvc.ConfigStore(), realServerURL); err != nil {
		storeSvc.Close()
		return nil, nil, nil, err
	}
	if err := rewriteStoredServerURL(ctx, storeSvc.ConfigStore(), realServerURL); err != nil {
		storeSvc.Close()
		return nil, nil, nil, err
	}
	if err := rewriteStoredClientType(ctx, storeSvc.ConfigStore(), arksdk.GrpcClient); err != nil {
		storeSvc.Close()
		return nil, nil, nil, err
	}

	client, err := arksdk.LoadArkClient(storeSvc)
	if err != nil {
		if !errors.Is(err, arksdk.ErrNotInitialized) {
			storeSvc.Close()
			return nil, nil, nil, err
		}

		client, err = arksdk.NewArkClient(storeSvc)
		if err != nil {
			storeSvc.Close()
			return nil, nil, nil, err
		}

		if err := client.Init(ctx, arksdk.InitArgs{
			WalletType: arksdk.SingleKeyWallet,
			ClientType: arksdk.GrpcClient,
			ServerUrl:  realServerURL,
			Seed:       state.WalletPrivateKeyHex,
			Password:   runtime.walletPassword(profileName),
		}); err != nil {
			safeStopArkClient(client)
			storeSvc.Close()
			return nil, nil, nil, err
		}
	}

	unlock := func() error {
		if !client.IsLocked(context.Background()) {
			return nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return client.Unlock(ctx, runtime.walletPassword(profileName))
	}

	cleanup := func() {
		if client != nil {
			lockCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = client.Lock(lockCtx)
			cancel()
		}
		safeStopArkClient(client)
		storeSvc.Close()
	}

	return client, unlock, cleanup, nil
}

func safeStopArkClient(client arksdk.ArkClient) {
	if client == nil {
		return
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			_ = recover()
		}()
		client.Stop()
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		debugWalletf("ark client stop timed out")
	}
}

func waitForOnchainFunds(ctx context.Context, client arksdk.ArkClient) error {
	for {
		balance, err := client.Balance(ctx, false)
		if err == nil && onchainBalanceTotal(balance) > 0 {
			return nil
		}
		if ctx.Err() != nil {
			if err != nil {
				return err
			}
			return ctx.Err()
		}
		select {
		case <-ctx.Done():
			if err != nil {
				return err
			}
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (runtime *Runtime) waitForBoardingFunds(ctx context.Context, client arksdk.ArkClient) error {
	return waitForOnchainFunds(ctx, client)
}

func (runtime *Runtime) currentWalletAddresses(ctx context.Context, client arksdk.ArkClient) (string, string, error) {
	onchainAddresses, offchainAddresses, boardingAddresses, _, err := client.GetAddresses(ctx)
	if err != nil {
		return "", "", err
	}
	if len(offchainAddresses) == 0 {
		return "", "", errors.New("wallet has no Ark address")
	}
	if len(boardingAddresses) == 0 {
		return "", "", errors.New("wallet has no boarding address")
	}

	arkAddress := offchainAddresses[len(offchainAddresses)-1]
	boardingAddress := boardingAddresses[len(boardingAddresses)-1]
	if len(onchainAddresses) > 0 && arkAddress == "" {
		arkAddress = onchainAddresses[len(onchainAddresses)-1]
	}
	return arkAddress, boardingAddress, nil
}

func debugWalletf(format string, args ...any) {
	if os.Getenv("PARKER_DEBUG_WALLET") == "" {
		return
	}
	log.Printf("[wallet-debug] "+format, args...)
}

func formatCommandOutput(stdout, stderr string) string {
	details := make([]string, 0, 2)

	if value := truncateCommandOutput(stderr); value != "" {
		details = append(details, fmt.Sprintf("stderr=%q", value))
	}
	if value := truncateCommandOutput(stdout); value != "" {
		details = append(details, fmt.Sprintf("stdout=%q", value))
	}
	if len(details) == 0 {
		return ""
	}
	return " (" + strings.Join(details, ", ") + ")"
}

func truncateCommandOutput(value string) string {
	trimmed := strings.TrimSpace(value)
	if len(trimmed) <= 240 {
		return trimmed
	}
	return trimmed[:240] + "..."
}

func onchainBalanceTotal(balance *arksdk.Balance) uint64 {
	if balance == nil {
		return 0
	}
	total := balance.OnchainBalance.SpendableAmount
	for _, locked := range balance.OnchainBalance.LockedAmount {
		total += locked.Amount
	}
	return total
}

func isRetryableOnboardError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, arksdk.ErrWaitingForConfirmation) {
		return true
	}
	message := err.Error()
	return strings.Contains(message, "missing inputs") ||
		strings.Contains(message, "missingorspent") ||
		strings.Contains(message, "No boarding utxos available after deducting fees") ||
		strings.Contains(message, "404 Not Found")
}

func (runtime *Runtime) walletPassword(profileName string) string {
	return "parker-wallet:" + profileName
}

func (runtime *Runtime) nigiriCommandName() string {
	commandName := os.Getenv("PARKER_NIGIRI_BIN")
	if commandName == "" {
		commandName = "nigiri"
	}
	return commandName
}

func localIdentity(seedHex string) (LocalIdentity, error) {
	seedBytes, err := hex.DecodeString(seedHex)
	if err != nil {
		return LocalIdentity{}, fmt.Errorf("decode private key: %w", err)
	}
	privateKey, publicKey := btcec.PrivKeyFromBytes(seedBytes)
	playerID := derivePlayerID(publicKey.SerializeCompressed())
	return LocalIdentity{
		PlayerID:      playerID,
		PrivateKeyHex: hex.EncodeToString(privateKey.Serialize()),
		PublicKeyHex:  hex.EncodeToString(publicKey.SerializeCompressed()),
	}, nil
}

func mockCustodyPubkeyHex(label string) string {
	seed := sha256.Sum256([]byte(label))
	_, publicKey := btcec.PrivKeyFromBytes(seed[:])
	return hex.EncodeToString(publicKey.SerializeCompressed())
}

func derivePlayerID(publicKey []byte) string {
	sum := sha256.Sum256(publicKey)
	return "player-" + hex.EncodeToString(sum[:])[:20]
}

func randomHex(byteLength int) (string, error) {
	buf := make([]byte, byteLength)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func createMockWallet(playerID string) WalletSummary {
	return WalletSummary{
		AvailableSats:       50_000,
		TotalSats:           50_000,
		WalletSpendableSats: 50_000,
		TableLockedSats:     0,
		PendingExitSats:     0,
		ArkAddress:          "tark1" + suffix(playerID, 16),
		BoardingAddress:     "bcrt1q" + padRight(suffix(playerID, 20), 20, "0"),
	}
}

func satsToBitcoinString(amountSats int) string {
	return fmt.Sprintf("%.8f", float64(amountSats)/100_000_000)
}

func suffix(value string, size int) string {
	if len(value) <= size {
		return value
	}
	return value[len(value)-size:]
}

func padRight(value string, size int, pad string) string {
	for len(value) < size {
		value += pad
	}
	if len(value) > size {
		return value[:size]
	}
	return value
}

func slugProfile(profileName string) string {
	var builder strings.Builder
	for _, char := range profileName {
		switch {
		case char >= 'a' && char <= 'z':
			builder.WriteRune(char)
		case char >= 'A' && char <= 'Z':
			builder.WriteRune(char)
		case char >= '0' && char <= '9':
			builder.WriteRune(char)
		case char == '_' || char == '-':
			builder.WriteRune(char)
		default:
			builder.WriteByte('_')
		}
	}
	return builder.String()
}

func sdkVtxosFromRefs(refs []tablecustody.VTXORef) []sdktypes.Vtxo {
	values := make([]sdktypes.Vtxo, 0, len(refs))
	for _, ref := range refs {
		amount := ref.AmountSats
		if amount < 0 {
			amount = 0
		}
		values = append(values, sdktypes.Vtxo{
			Outpoint: sdktypes.Outpoint{
				Txid: ref.TxID,
				VOut: ref.VOut,
			},
			Amount:  uint64(amount),
			ArkTxid: ref.ArkTxID,
			Script:  ref.Script,
		})
	}
	return values
}

func derivePlayerIDFromState(state PlayerProfileState) string {
	identity, err := localIdentity(state.WalletPrivateKeyHex)
	if err != nil {
		return ""
	}
	return identity.PlayerID
}

func deriveSignerPrivateKeyHex(seedHex, derivationPath string) (string, string, error) {
	sum := sha256.Sum256([]byte(seedHex + ":" + derivationPath))
	privateKey, publicKey := btcec.PrivKeyFromBytes(sum[:])
	return hex.EncodeToString(privateKey.Serialize()), hex.EncodeToString(publicKey.SerializeCompressed()), nil
}

func addDurationISO(delta time.Duration) string {
	return time.Now().UTC().Add(delta).Format(time.RFC3339)
}

func parseSatsString(value string) int {
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return 0
	}
	return parsed
}
