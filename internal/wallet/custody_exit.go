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

	config, err := client.GetConfigData(ctx)
	if err != nil {
		return CustodyExitResult{}, err
	}
	if config == nil {
		return CustodyExitResult{}, errors.New("ark config is unavailable")
	}

	explorerSvc, err := mempoolexplorer.NewExplorer(config.ExplorerURL, config.Network, mempoolexplorer.WithTracker(false))
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
		childTx, err := runtime.bumpCustodyAnchorTx(ctx, client, explorerSvc, &parent)
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

func (runtime *Runtime) bumpCustodyAnchorTx(ctx context.Context, client arksdk.ArkClient, explorerSvc arkexplorer.Explorer, parent *wire.MsgTx) (string, error) {
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
		return "", err
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
		return "", err
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
