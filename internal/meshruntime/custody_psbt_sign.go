package meshruntime

import (
	"encoding/hex"
	"slices"
	"strings"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

func signCustodyPSBTWithKey(current, privateKeyHex, publicKeyHex string) (string, error) {
	packet, err := psbt.NewFromRawBytes(strings.NewReader(current), true)
	if err != nil {
		return "", err
	}
	privKeyBytes, err := hex.DecodeString(privateKeyHex)
	if err != nil {
		return "", err
	}
	privKey, _ := btcec.PrivKeyFromBytes(privKeyBytes)
	xOnlyHex, err := xOnlyPubkeyHexFromCompressed(publicKeyHex)
	if err != nil {
		return "", err
	}
	xOnlyPubkey, err := hex.DecodeString(xOnlyHex)
	if err != nil {
		return "", err
	}
	prevOuts := txscript.NewMultiPrevOutFetcher(nil)
	for inputIndex, txIn := range packet.UnsignedTx.TxIn {
		input := packet.Inputs[inputIndex]
		if input.WitnessUtxo == nil {
			continue
		}
		prevOuts.AddPrevOut(txIn.PreviousOutPoint, &wire.TxOut{
			PkScript: append([]byte(nil), input.WitnessUtxo.PkScript...),
			Value:    input.WitnessUtxo.Value,
		})
	}
	sigHashes := txscript.NewTxSigHashes(packet.UnsignedTx, prevOuts)
	for inputIndex := range packet.Inputs {
		input := packet.Inputs[inputIndex]
		if input.WitnessUtxo == nil || len(input.TaprootLeafScript) == 0 {
			continue
		}
		leafScript := input.TaprootLeafScript[0]
		tokenizer := txscript.MakeScriptTokenizer(0, leafScript.Script)
		signsThisLeaf := false
		for tokenizer.Next() {
			if data := tokenizer.Data(); len(data) == len(xOnlyPubkey) && slices.Equal(data, xOnlyPubkey) {
				signsThisLeaf = true
				break
			}
		}
		if tokenizer.Err() != nil || !signsThisLeaf {
			continue
		}
		leaf := txscript.NewTapLeaf(leafScript.LeafVersion, leafScript.Script)
		leafHash := leaf.TapHash()
		alreadySigned := false
		for _, signature := range input.TaprootScriptSpendSig {
			if signature != nil && string(signature.XOnlyPubKey) == string(xOnlyPubkey) {
				alreadySigned = true
				break
			}
		}
		if alreadySigned {
			continue
		}
		sigHash := input.SighashType
		if sigHash == 0 {
			sigHash = txscript.SigHashDefault
		}
		signature, err := txscript.RawTxInTapscriptSignature(
			packet.UnsignedTx,
			sigHashes,
			inputIndex,
			input.WitnessUtxo.Value,
			input.WitnessUtxo.PkScript,
			leaf,
			sigHash,
			privKey,
		)
		if err != nil {
			return "", err
		}
		if sigHash != txscript.SigHashDefault {
			signature = signature[:len(signature)-1]
		}
		input.TaprootScriptSpendSig = append(input.TaprootScriptSpendSig, &psbt.TaprootScriptSpendSig{
			XOnlyPubKey: append([]byte(nil), xOnlyPubkey...),
			LeafHash:    leafHash.CloneBytes(),
			Signature:   append([]byte(nil), signature...),
			SigHash:     sigHash,
		})
		packet.Inputs[inputIndex] = input
	}
	return packet.B64Encode()
}

func (runtime *meshRuntime) maybeSignCustodyPSBTAsOperator(purpose, current string) (string, error) {
	return current, nil
}
