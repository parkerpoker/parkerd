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
		if saveErr := runtime.saveProfileState(state); saveErr != nil {
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
		witness, err := mockCustodyExitWitness(refs, true)
		if err != nil {
			return CustodyExitResult{}, err
		}
		summary, err := VerifyUnilateralExitWitness(refs, *witness)
		if err != nil {
			return CustodyExitResult{}, err
		}
		return CustodyExitResult{
			BroadcastTxIDs: append([]string(nil), summary.BroadcastTxIDs...),
			Pending:        false,
			SourceRefs:     append([]tablecustody.VTXORef(nil), refs...),
			SweepTxID:      summary.SweepTxID,
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

func decodeCustodyExitTxHex(txHex, txid string) (*wire.MsgTx, error) {
	var tx wire.MsgTx
	if err := tx.Deserialize(hex.NewDecoder(strings.NewReader(txHex))); err != nil {
		return nil, err
	}
	if txid != "" && tx.TxHash().String() != txid {
		return nil, fmt.Errorf("txid mismatch: expected %s, got %s", txid, tx.TxHash().String())
	}
	return &tx, nil
}

func custodyExitOutpointKey(txid string, vout uint32) string {
	return fmt.Sprintf("%s:%d", strings.TrimSpace(txid), vout)
}

func uniqueExitTxIDs(txids []string) []string {
	seen := map[string]struct{}{}
	unique := make([]string, 0, len(txids))
	for _, txid := range txids {
		trimmed := strings.TrimSpace(txid)
		if trimmed == "" {
			continue
		}
		if _, ok := seen[trimmed]; ok {
			continue
		}
		seen[trimmed] = struct{}{}
		unique = append(unique, trimmed)
	}
	return unique
}

func encodeCustodyExitTxHex(tx *wire.MsgTx) (string, error) {
	if tx == nil {
		return "", errors.New("custody exit tx is missing")
	}
	var encoded bytes.Buffer
	if err := tx.Serialize(&encoded); err != nil {
		return "", err
	}
	return hex.EncodeToString(encoded.Bytes()), nil
}

func mockCustodyExitWitness(refs []tablecustody.VTXORef, includeSweep bool) (*tablecustody.CustodyExitWitness, error) {
	if len(refs) == 0 {
		return nil, errors.New("custody exit requires source refs")
	}
	anchorTx := wire.NewMsgTx(2)
	for _, ref := range refs {
		hash, err := chainhash.NewHashFromStr(strings.TrimSpace(ref.TxID))
		if err != nil {
			return nil, fmt.Errorf("mock custody exit source ref %s is invalid: %w", custodyExitOutpointKey(ref.TxID, ref.VOut), err)
		}
		anchorTx.AddTxIn(&wire.TxIn{
			PreviousOutPoint: wire.OutPoint{
				Hash:  *hash,
				Index: ref.VOut,
			},
		})
	}
	anchorTx.AddTxOut(&wire.TxOut{Value: 1, PkScript: []byte{txscript.OP_TRUE}})
	anchorHex, err := encodeCustodyExitTxHex(anchorTx)
	if err != nil {
		return nil, err
	}
	witness := &tablecustody.CustodyExitWitness{
		BroadcastTransactions: []tablecustody.CustodyExitTransaction{{
			TransactionHex: anchorHex,
			TransactionID:  anchorTx.TxHash().String(),
		}},
	}
	if !includeSweep {
		return witness, nil
	}
	sweepTx := wire.NewMsgTx(2)
	sweepTx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  anchorTx.TxHash(),
			Index: 0,
		},
	})
	sweepTx.AddTxOut(&wire.TxOut{Value: 1, PkScript: []byte{txscript.OP_TRUE}})
	sweepHex, err := encodeCustodyExitTxHex(sweepTx)
	if err != nil {
		return nil, err
	}
	witness.SweepTransaction = &tablecustody.CustodyExitTransaction{
		TransactionHex: sweepHex,
		TransactionID:  sweepTx.TxHash().String(),
	}
	return witness, nil
}

func sameExitTxIDs(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if strings.TrimSpace(left[index]) != strings.TrimSpace(right[index]) {
			return false
		}
	}
	return true
}

func VerifyUnilateralExitWitness(refs []tablecustody.VTXORef, witness tablecustody.CustodyExitWitness) (CustodyExitWitnessSummary, error) {
	summary := CustodyExitWitnessSummary{}
	if len(refs) == 0 {
		return summary, errors.New("custody exit requires source refs")
	}
	if len(witness.BroadcastTransactions) == 0 {
		return summary, errors.New("custody exit witness is missing broadcast transactions")
	}
	sourceOutpoints := map[string]struct{}{}
	for _, ref := range refs {
		sourceOutpoints[custodyExitOutpointKey(ref.TxID, ref.VOut)] = struct{}{}
	}
	broadcastSet := map[string]struct{}{}
	broadcastTxs := map[string]*wire.MsgTx{}
	summary.BroadcastTxIDs = make([]string, 0, len(witness.BroadcastTransactions))
	for index, artifact := range witness.BroadcastTransactions {
		txid := strings.TrimSpace(artifact.TransactionID)
		if txid == "" {
			return summary, fmt.Errorf("custody exit witness broadcast transaction %d is missing a txid", index)
		}
		if artifact.TransactionHex == "" {
			return summary, fmt.Errorf("custody exit witness broadcast transaction %d is missing tx hex", index)
		}
		if _, ok := broadcastSet[txid]; ok {
			return summary, fmt.Errorf("custody exit witness broadcast tx %s is duplicated", txid)
		}
		tx, err := decodeCustodyExitTxHex(artifact.TransactionHex, txid)
		if err != nil {
			return summary, fmt.Errorf("decode custody exit tx %s: %w", txid, err)
		}
		broadcastSet[txid] = struct{}{}
		broadcastTxs[txid] = tx
		summary.BroadcastTxIDs = append(summary.BroadcastTxIDs, txid)
	}
	spentSources := map[string]string{}
	for txid, tx := range broadcastTxs {
		chained := false
		for _, input := range tx.TxIn {
			prevTxID := input.PreviousOutPoint.Hash.String()
			prevKey := custodyExitOutpointKey(prevTxID, input.PreviousOutPoint.Index)
			if _, ok := sourceOutpoints[prevKey]; ok {
				spentSources[prevKey] = txid
				chained = true
				continue
			}
			if _, ok := broadcastSet[prevTxID]; ok {
				chained = true
			}
		}
		if !chained {
			return summary, fmt.Errorf("custody exit tx %s does not spend the claimed exit path", txid)
		}
	}
	for sourceKey := range sourceOutpoints {
		if _, ok := spentSources[sourceKey]; !ok {
			return summary, fmt.Errorf("custody exit proof is missing the spend for source ref %s", sourceKey)
		}
	}
	if witness.SweepTransaction == nil {
		return summary, nil
	}
	txid := strings.TrimSpace(witness.SweepTransaction.TransactionID)
	if txid == "" {
		return summary, errors.New("custody exit witness sweep transaction is missing a txid")
	}
	if witness.SweepTransaction.TransactionHex == "" {
		return summary, errors.New("custody exit witness sweep transaction is missing tx hex")
	}
	sweepTx, err := decodeCustodyExitTxHex(witness.SweepTransaction.TransactionHex, txid)
	if err != nil {
		return summary, fmt.Errorf("decode custody exit sweep tx %s: %w", txid, err)
	}
	for _, input := range sweepTx.TxIn {
		if _, ok := broadcastSet[input.PreviousOutPoint.Hash.String()]; ok {
			summary.SweepTxID = txid
			return summary, nil
		}
	}
	return summary, fmt.Errorf("custody exit sweep tx %s does not spend the claimed exit path", txid)
}

func (runtime *Runtime) BuildUnilateralExitWitness(profileName string, refs []tablecustody.VTXORef, broadcastTxIDs []string, sweepTxID string) (*tablecustody.CustodyExitWitness, error) {
	if runtime.config.UseMockSettlement {
		witness, err := mockCustodyExitWitness(refs, strings.TrimSpace(sweepTxID) != "")
		if err != nil {
			return nil, err
		}
		summary, err := VerifyUnilateralExitWitness(refs, *witness)
		if err != nil {
			return nil, err
		}
		if !sameExitTxIDs(summary.BroadcastTxIDs, uniqueExitTxIDs(broadcastTxIDs)) || strings.TrimSpace(summary.SweepTxID) != strings.TrimSpace(sweepTxID) {
			return nil, errors.New("custody exit proof txids do not match the mock exit path")
		}
		return witness, nil
	}
	if len(refs) == 0 {
		return nil, errors.New("custody exit requires source refs")
	}
	claimedBroadcastTxIDs := uniqueExitTxIDs(broadcastTxIDs)
	if len(claimedBroadcastTxIDs) == 0 {
		return nil, errors.New("custody exit proof is missing broadcast txids")
	}

	config, err := runtime.explorerConfig(profileName)
	if err != nil {
		return nil, err
	}
	explorerSvc, err := mempoolexplorer.NewExplorer(config.ExplorerURL, config.Network, mempoolexplorer.WithTracker(false))
	if err != nil {
		return nil, err
	}
	defer explorerSvc.Stop()

	witness := &tablecustody.CustodyExitWitness{
		BroadcastTransactions: make([]tablecustody.CustodyExitTransaction, 0, len(claimedBroadcastTxIDs)),
	}
	for _, txid := range claimedBroadcastTxIDs {
		txHex, err := explorerSvc.GetTxHex(txid)
		if err != nil {
			return nil, fmt.Errorf("unable to verify custody exit tx %s: %w", txid, err)
		}
		witness.BroadcastTransactions = append(witness.BroadcastTransactions, tablecustody.CustodyExitTransaction{
			TransactionHex: txHex,
			TransactionID:  txid,
		})
	}
	trimmedSweepTxID := strings.TrimSpace(sweepTxID)
	if trimmedSweepTxID != "" {
		sweepTxHex, err := explorerSvc.GetTxHex(trimmedSweepTxID)
		if err != nil {
			return nil, fmt.Errorf("unable to verify custody exit sweep tx %s: %w", trimmedSweepTxID, err)
		}
		witness.SweepTransaction = &tablecustody.CustodyExitTransaction{
			TransactionHex: sweepTxHex,
			TransactionID:  trimmedSweepTxID,
		}
	}
	if _, err := VerifyUnilateralExitWitness(refs, *witness); err != nil {
		return nil, err
	}
	return witness, nil
}

func (runtime *Runtime) VerifyUnilateralExitExecution(profileName string, refs []tablecustody.VTXORef, broadcastTxIDs []string, sweepTxID string) error {
	_, err := runtime.BuildUnilateralExitWitness(profileName, refs, broadcastTxIDs, sweepTxID)
	return err
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
