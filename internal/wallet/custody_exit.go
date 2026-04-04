package wallet

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	arkscript "github.com/arkade-os/arkd/pkg/ark-lib/script"
	arktxutils "github.com/arkade-os/arkd/pkg/ark-lib/txutils"
	arksdk "github.com/arkade-os/go-sdk"
	arkexplorer "github.com/arkade-os/go-sdk/explorer"
	mempoolexplorer "github.com/arkade-os/go-sdk/explorer/mempool"
	arkgrpcindexer "github.com/arkade-os/go-sdk/indexer/grpc"
	"github.com/arkade-os/go-sdk/redemption"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/lightningnetwork/lnd/input"
	"github.com/lightningnetwork/lnd/lntypes"
	"github.com/parkerpoker/parkerd/internal/tablecustody"
)

const unilateralExitSweepTimeout = 90 * time.Second

func finalizedSignedCustodyTxFromPacket(packet *psbt.Packet) (*wire.MsgTx, error) {
	if packet == nil {
		return nil, errors.New("custody signed psbt is missing")
	}
	finalTx := packet.UnsignedTx.Copy()
	for inputIndex, input := range packet.Inputs {
		if len(input.TaprootLeafScript) != 1 {
			return nil, fmt.Errorf("custody signed psbt input %d is missing its authorized leaf proof", inputIndex)
		}
		leafScript := input.TaprootLeafScript[0]
		closure, err := arkscript.DecodeClosure(leafScript.Script)
		if err != nil {
			return nil, err
		}
		leafHash := txscript.NewTapLeaf(leafScript.LeafVersion, leafScript.Script).TapHash()
		signatures := make(map[string][]byte, len(input.TaprootScriptSpendSig))
		for _, signature := range input.TaprootScriptSpendSig {
			if signature == nil || !bytes.Equal(signature.LeafHash, leafHash[:]) {
				return nil, fmt.Errorf("custody signed psbt input %d contains a signature for the wrong leaf", inputIndex)
			}
			rawSignature := append([]byte(nil), signature.Signature...)
			if signature.SigHash != txscript.SigHashDefault {
				rawSignature = append(rawSignature, byte(signature.SigHash))
			}
			signatures[hex.EncodeToString(signature.XOnlyPubKey)] = rawSignature
		}
		witness, err := closure.Witness(leafScript.ControlBlock, signatures)
		if err != nil {
			return nil, err
		}
		finalTx.TxIn[inputIndex].Witness = witness
	}
	return finalTx, nil
}

func (runtime *Runtime) liveOrCachedArkConfig(ctx context.Context, profileName string, state PlayerProfileState, client arksdk.ArkClient) (*CustodyArkConfig, error) {
	config, err := client.GetConfigData(ctx)
	switch {
	case err == nil && config != nil:
		if config.SignerPubKey == nil || config.ForfeitPubKey == nil {
			return nil, errors.New("ark client config is incomplete")
		}
		cached := CustodyArkConfig{
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
		}
		state.CachedArkConfig = &cached
		if saveErr := runtime.store.Save(state); saveErr != nil {
			return nil, saveErr
		}
		return &cached, nil
	case err == nil:
		return nil, nil
	default:
		if cached, ok := cachedArkConfig(state); ok {
			debugWalletf("using cached ark config for custody recovery profile=%s err=%v", profileName, err)
			return &cached, nil
		}
		return nil, err
	}
}

func (runtime *Runtime) UnilateralExitCustodyRefs(profileName string, refs []tablecustody.VTXORef, destination string) (CustodyExitResult, error) {
	if runtime.config.UseMockSettlement {
		return CustodyExitResult{
			BroadcastTxIDs: []string{"mock-exit-" + suffix(profileName, 8)},
			Pending:        false,
			SourceRefs:     append([]tablecustody.VTXORef(nil), refs...),
			SweepTxID:      "mock-sweep-" + suffix(profileName, 8),
		}, nil
	}
	if len(refs) == 0 {
		return CustodyExitResult{}, errors.New("custody exit requires source refs")
	}

	state, err := runtime.ensureBootstrap(profileName, "", "")
	if err != nil {
		return CustodyExitResult{}, err
	}

	runtime.mu.Lock()
	defer runtime.mu.Unlock()

	client, unlock, cleanup, err := runtime.openArkClient(profileName, *state)
	if err != nil {
		return CustodyExitResult{}, err
	}
	defer cleanup()
	if err := unlock(); err != nil {
		return CustodyExitResult{}, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), unilateralExitSweepTimeout)
	defer cancel()

	cachedConfig, err := runtime.liveOrCachedArkConfig(ctx, profileName, *state, client)
	if err != nil {
		return CustodyExitResult{}, err
	}
	if cachedConfig == nil {
		return CustodyExitResult{}, errors.New("ark config is unavailable")
	}

	explorerSvc, err := mempoolexplorer.NewExplorer(cachedConfig.ExplorerURL, cachedConfig.Network, mempoolexplorer.WithTracker(false))
	if err != nil {
		return CustodyExitResult{}, err
	}
	defer explorerSvc.Stop()

	indexerSvc, err := arkgrpcindexer.NewClient(runtime.config.ArkServerURL)
	if err != nil {
		return CustodyExitResult{}, err
	}
	defer indexerSvc.Close()

	result := CustodyExitResult{
		Pending:    true,
		SourceRefs: append([]tablecustody.VTXORef(nil), refs...),
	}
	broadcasted := map[string]struct{}{}
	for _, vtxo := range sdkVtxosFromRefs(refs) {
		branch, err := redemption.NewRedeemBranch(ctx, explorerSvc, indexerSvc, vtxo)
		if err != nil {
			return CustodyExitResult{}, err
		}
		nextTx, err := branch.NextRedeemTx()
		if err != nil {
			var pending redemption.ErrPendingConfirmation
			switch {
			case errors.As(err, &pending):
				continue
			case strings.Contains(err.Error(), "already redeemed"):
				continue
			default:
				return CustodyExitResult{}, err
			}
		}
		var parent wire.MsgTx
		if err := parent.Deserialize(hex.NewDecoder(strings.NewReader(nextTx))); err != nil {
			return CustodyExitResult{}, err
		}
		childTx, err := runtime.bumpCustodyAnchorTx(ctx, profileName, *state, client, explorerSvc, &parent)
		if err != nil {
			return CustodyExitResult{}, err
		}
		txid, err := explorerSvc.Broadcast(nextTx, childTx)
		if err != nil {
			return CustodyExitResult{}, err
		}
		if _, ok := broadcasted[txid]; ok {
			continue
		}
		broadcasted[txid] = struct{}{}
		result.BroadcastTxIDs = append(result.BroadcastTxIDs, txid)
	}

	sweepTxID, err := client.CompleteUnroll(ctx, destination)
	switch {
	case err == nil:
		result.SweepTxID = sweepTxID
		result.Pending = false
	case errors.Is(err, arksdk.ErrWaitingForConfirmation):
	case strings.Contains(err.Error(), "no mature funds available"):
	default:
		return CustodyExitResult{}, err
	}

	return result, nil
}

func (runtime *Runtime) ExecuteCustodyRecoveryTransaction(profileName, signedPSBT string) (CustodyRecoveryResult, error) {
	if runtime.config.UseMockSettlement {
		txid := "mock-recovery-" + suffix(profileName, 8)
		return CustodyRecoveryResult{
			BroadcastTxIDs: []string{txid},
			RecoveryTxID:   txid,
		}, nil
	}
	if strings.TrimSpace(signedPSBT) == "" {
		return CustodyRecoveryResult{}, errors.New("custody recovery requires a signed psbt")
	}

	state, err := runtime.ensureBootstrap(profileName, "", "")
	if err != nil {
		return CustodyRecoveryResult{}, err
	}

	runtime.mu.Lock()
	defer runtime.mu.Unlock()

	client, unlock, cleanup, err := runtime.openArkClient(profileName, *state)
	if err != nil {
		return CustodyRecoveryResult{}, err
	}
	defer cleanup()
	if err := unlock(); err != nil {
		return CustodyRecoveryResult{}, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), unilateralExitSweepTimeout)
	defer cancel()

	cachedConfig, err := runtime.liveOrCachedArkConfig(ctx, profileName, *state, client)
	if err != nil {
		return CustodyRecoveryResult{}, err
	}
	if cachedConfig == nil {
		return CustodyRecoveryResult{}, errors.New("ark config is unavailable")
	}

	explorerSvc, err := mempoolexplorer.NewExplorer(cachedConfig.ExplorerURL, cachedConfig.Network, mempoolexplorer.WithTracker(false))
	if err != nil {
		return CustodyRecoveryResult{}, err
	}
	defer explorerSvc.Stop()

	packet, err := psbt.NewFromRawBytes(strings.NewReader(signedPSBT), true)
	if err != nil {
		return CustodyRecoveryResult{}, err
	}
	parentTx, err := finalizedSignedCustodyTxFromPacket(packet)
	if err != nil {
		return CustodyRecoveryResult{}, err
	}
	recoveryTxID := parentTx.TxHash().String()
	childTx, err := runtime.bumpCustodyAnchorTx(ctx, profileName, *state, client, explorerSvc, parentTx)
	if err != nil {
		return CustodyRecoveryResult{}, err
	}

	var serialized bytes.Buffer
	if err := parentTx.Serialize(&serialized); err != nil {
		return CustodyRecoveryResult{}, err
	}
	parentHex := hex.EncodeToString(serialized.Bytes())
	broadcastTxID, err := explorerSvc.Broadcast(parentHex, childTx)
	if err != nil {
		return CustodyRecoveryResult{}, err
	}

	result := CustodyRecoveryResult{
		BroadcastTxIDs: []string{broadcastTxID},
		RecoveryTxID:   recoveryTxID,
	}
	if broadcastTxID != recoveryTxID {
		result.BroadcastTxIDs = append(result.BroadcastTxIDs, recoveryTxID)
	}
	return result, nil
}

func (runtime *Runtime) bumpCustodyAnchorTx(ctx context.Context, profileName string, state PlayerProfileState, client arksdk.ArkClient, explorerSvc arkexplorer.Explorer, parent *wire.MsgTx) (string, error) {
	anchor, err := arktxutils.FindAnchorOutpoint(parent)
	if err != nil {
		return "", err
	}

	weightEstimator := input.TxWeightEstimator{}
	weightEstimator.AddNestedP2WSHInput(lntypes.VByte(3).ToWU())
	weightEstimator.AddTaprootKeySpendInput(txscript.SigHashDefault)
	weightEstimator.AddP2TROutput()
	childVSize := weightEstimator.Weight().ToVB()

	packageSize := childVSize + computeVSize(parent)
	feeRate, err := explorerSvc.GetFeeRate()
	if err != nil {
		return "", err
	}
	fees := uint64(math.Ceil(float64(packageSize) * feeRate))

	onchainAddrs, _, _, _, err := client.GetAddresses(ctx)
	if err != nil {
		onchainAddrs = cachedOnchainAddresses(state)
		if len(onchainAddrs) == 0 {
			return "", err
		}
		debugWalletf("using cached onchain addresses for custody recovery profile=%s err=%v", profileName, err)
	}
	selectedCoins := make([]arkexplorer.Utxo, 0)
	selectedAmount := uint64(0)
	amountToSelect := int64(fees) - arktxutils.ANCHOR_VALUE
	for _, addr := range onchainAddrs {
		utxos, err := explorerSvc.GetUtxos(addr)
		if err != nil {
			return "", err
		}
		for _, utxo := range utxos {
			selectedCoins = append(selectedCoins, utxo)
			selectedAmount += utxo.Amount
			amountToSelect = int64(fees) - int64(selectedAmount)
			if amountToSelect <= 0 {
				break
			}
		}
		if amountToSelect <= 0 {
			break
		}
	}
	if amountToSelect > 0 {
		return "", fmt.Errorf("not enough onchain funds to fee-bump unilateral exit")
	}

	changeAddr, _, _, err := client.Receive(ctx)
	if err != nil {
		if len(onchainAddrs) == 0 {
			return "", err
		}
		changeAddr = onchainAddrs[len(onchainAddrs)-1]
		debugWalletf("using cached change address for custody recovery profile=%s err=%v", profileName, err)
	}
	pkScript, err := payToAddressScript(changeAddr)
	if err != nil {
		return "", err
	}
	changeAmount := selectedAmount - fees

	inputs := []*wire.OutPoint{anchor}
	sequences := []uint32{wire.MaxTxInSequenceNum}
	outputs := []*wire.TxOut{{Value: int64(changeAmount), PkScript: pkScript}}

	for _, utxo := range selectedCoins {
		txid, err := chainhash.NewHashFromStr(utxo.Txid)
		if err != nil {
			return "", err
		}
		inputs = append(inputs, &wire.OutPoint{Hash: *txid, Index: utxo.Vout})
		sequences = append(sequences, wire.MaxTxInSequenceNum)
	}

	packet, err := psbt.New(inputs, outputs, 3, 0, sequences)
	if err != nil {
		return "", err
	}
	packet.Inputs[0].WitnessUtxo = arktxutils.AnchorOutput()
	unsigned, err := packet.B64Encode()
	if err != nil {
		return "", err
	}

	signed, err := client.SignTransaction(ctx, unsigned)
	if err != nil {
		return "", err
	}
	signedPacket, err := psbt.NewFromRawBytes(strings.NewReader(signed), true)
	if err != nil {
		return "", err
	}
	for inputIndex := range signedPacket.Inputs[1:] {
		if _, err := psbt.MaybeFinalize(signedPacket, inputIndex+1); err != nil {
			return "", err
		}
	}
	childTx, err := arktxutils.ExtractWithAnchors(signedPacket)
	if err != nil {
		return "", err
	}

	var serialized bytes.Buffer
	if err := childTx.Serialize(&serialized); err != nil {
		return "", err
	}
	return hex.EncodeToString(serialized.Bytes()), nil
}

func payToAddressScript(address string) ([]byte, error) {
	decoded, err := btcutil.DecodeAddress(address, nil)
	if err != nil {
		return nil, err
	}
	return txscript.PayToAddrScript(decoded)
}

func computeVSize(tx *wire.MsgTx) lntypes.VByte {
	baseSize := tx.SerializeSizeStripped()
	totalSize := tx.SerializeSize()
	weight := totalSize + baseSize*3
	return lntypes.WeightUnit(uint64(weight)).ToVB()
}
