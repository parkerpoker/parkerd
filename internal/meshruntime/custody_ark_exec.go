package meshruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	arklib "github.com/arkade-os/arkd/pkg/ark-lib"
	arkintent "github.com/arkade-os/arkd/pkg/ark-lib/intent"
	arkscript "github.com/arkade-os/arkd/pkg/ark-lib/script"
	arktree "github.com/arkade-os/arkd/pkg/ark-lib/tree"
	arktxutils "github.com/arkade-os/arkd/pkg/ark-lib/txutils"
	arksdk "github.com/arkade-os/go-sdk"
	arkclient "github.com/arkade-os/go-sdk/client"
	arkgrpc "github.com/arkade-os/go-sdk/client/grpc"
	sdktypes "github.com/arkade-os/go-sdk/types"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/parkerpoker/parkerd/internal/tablecustody"
	walletpkg "github.com/parkerpoker/parkerd/internal/wallet"
)

type custodyBatchOutput struct {
	AmountSats    int
	ClaimKey      string
	Onchain       bool
	OwnerPlayerID string
	Script        string
	Tapscripts    []string
}

type custodyBatchResult struct {
	ArkTxID          string
	BatchExpiryType  string
	BatchExpiryValue uint32
	CommitmentTx     string
	ConnectorTree    arktree.FlatTxTree
	FinalizedAt      string
	IntentID         string
	OutputRefs       map[string][]tablecustody.VTXORef
	ProofPSBT        string
	VtxoTree         arktree.FlatTxTree
}

type custodySignerAuthorization struct {
	ExpectedOffchainOutputs []custodyBatchOutput
	ExpectedPrevStateHash   string
	TransitionHash          string
}

func mockCustodyBatchResult(transitionHash string, outputs []custodyBatchOutput) *custodyBatchResult {
	intentID := "mock-intent-" + transitionHash[:12]
	arkTxID := transitionHash
	finalizedAt := nowISO()
	outputRefs := map[string][]tablecustody.VTXORef{}
	for index, output := range outputs {
		ref := tablecustody.VTXORef{
			AmountSats:    output.AmountSats,
			ArkIntentID:   intentID,
			ArkTxID:       arkTxID,
			ExpiresAt:     addMillis(nowISO(), int((24*time.Hour)/time.Millisecond)),
			OwnerPlayerID: output.OwnerPlayerID,
			Script:        output.Script,
			Tapscripts:    append([]string(nil), output.Tapscripts...),
			TxID:          arkTxID,
			VOut:          uint32(index),
		}
		outputRefs[output.ClaimKey] = append(outputRefs[output.ClaimKey], ref)
	}
	return &custodyBatchResult{
		ArkTxID:     arkTxID,
		FinalizedAt: finalizedAt,
		IntentID:    intentID,
		OutputRefs:  outputRefs,
	}
}

func custodySignerSessionKey(tableID, transitionHash, playerID, derivationPath string) string {
	return tableID + "|" + transitionHash + "|" + playerID + "|" + derivationPath
}

func (runtime *meshRuntime) storeCustodySignerSession(key string, session walletpkg.CustodySignerSession) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	runtime.custodySigners[key] = session
}

func (runtime *meshRuntime) loadCustodySignerSession(key string) (walletpkg.CustodySignerSession, bool) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	session, ok := runtime.custodySigners[key]
	return session, ok
}

func (runtime *meshRuntime) deleteCustodySignerSession(key string) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	delete(runtime.custodySigners, key)
}

func (runtime *meshRuntime) storeCustodySignerAuthorization(key string, authorization custodySignerAuthorization) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	runtime.custodySignerAuth[key] = authorization
}

func (runtime *meshRuntime) loadCustodySignerAuthorization(key string) (custodySignerAuthorization, bool) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	authorization, ok := runtime.custodySignerAuth[key]
	return authorization, ok
}

func (runtime *meshRuntime) deleteCustodySignerAuthorization(key string) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	delete(runtime.custodySignerAuth, key)
}

func (runtime *meshRuntime) newArkTransportClient() (arkclient.TransportClient, error) {
	if runtime.arkTransportFactory != nil {
		return runtime.arkTransportFactory()
	}
	return arkgrpc.NewClient(runtime.config.ArkServerURL)
}

func custodyBatchExpiry(expiry uint32) arklib.RelativeLocktime {
	if expiry >= 512 {
		return arklib.RelativeLocktime{Type: arklib.LocktimeTypeSecond, Value: expiry}
	}
	return arklib.RelativeLocktime{Type: arklib.LocktimeTypeBlock, Value: expiry}
}

func custodyBatchExpiryType(expiry arklib.RelativeLocktime) string {
	if expiry.Type == arklib.LocktimeTypeSecond {
		return "seconds"
	}
	return "blocks"
}

func parseCustodyBatchExpiry(expiryType string, expiryValue uint32) (arklib.RelativeLocktime, error) {
	expiry := arklib.RelativeLocktime{Value: expiryValue}
	switch strings.TrimSpace(expiryType) {
	case "", "blocks":
		expiry.Type = arklib.LocktimeTypeBlock
	case "seconds":
		expiry.Type = arklib.LocktimeTypeSecond
	default:
		return arklib.RelativeLocktime{}, fmt.Errorf("unsupported custody batch expiry type %q", expiryType)
	}
	return expiry, nil
}

func custodyIntentInputs(inputs []custodyInputSpec) ([]arkintent.Input, []*arklib.TaprootMerkleProof, [][]*psbt.Unknown, arklib.AbsoluteLocktime, error) {
	intentInputs := make([]arkintent.Input, 0, len(inputs))
	leafProofs := make([]*arklib.TaprootMerkleProof, 0, len(inputs))
	arkFields := make([][]*psbt.Unknown, 0, len(inputs))
	locktime := arklib.AbsoluteLocktime(0)
	for _, input := range inputs {
		hash, err := chainhash.NewHashFromStr(input.Ref.TxID)
		if err != nil {
			return nil, nil, nil, 0, err
		}
		sequence := uint32(wire.MaxTxInSequenceNum)
		if input.SpendPath.UsesCLTVLocktime {
			sequence = wire.MaxTxInSequenceNum - 1
			if input.SpendPath.Locktime > locktime {
				locktime = input.SpendPath.Locktime
			}
		}
		intentInputs = append(intentInputs, arkintent.Input{
			OutPoint: &wire.OutPoint{
				Hash:  *hash,
				Index: input.Ref.VOut,
			},
			Sequence: sequence,
			WitnessUtxo: &wire.TxOut{
				Value:    int64(input.Ref.AmountSats),
				PkScript: input.SpendPath.PKScript,
			},
		})
		leafProofs = append(leafProofs, input.SpendPath.LeafProof)
		taptreeField, err := arktxutils.VtxoTaprootTreeField.Encode(input.SpendPath.Tapscripts)
		if err != nil {
			return nil, nil, nil, 0, err
		}
		arkFields = append(arkFields, []*psbt.Unknown{taptreeField})
	}
	return intentInputs, leafProofs, arkFields, locktime, nil
}

func custodyRegisterMessage(onchainIndexes []int, cosignerPubkeys []string) (string, error) {
	validAt := time.Now()
	message, err := arkintent.RegisterMessage{
		BaseMessage: arkintent.BaseMessage{
			Type: arkintent.IntentMessageTypeRegister,
		},
		OnchainOutputIndexes: onchainIndexes,
		ExpireAt:             validAt.Add(2 * time.Minute).Unix(),
		ValidAt:              validAt.Unix(),
		CosignersPublicKeys:  cosignerPubkeys,
	}.Encode()
	if err != nil {
		return "", err
	}
	return message, nil
}

func validateCustodyRegisterMessage(message string, onchainIndexes []int, cosignerPubkeys []string) error {
	if strings.TrimSpace(message) == "" {
		return errors.New("missing register message")
	}
	var decoded arkintent.RegisterMessage
	if err := decoded.Decode(strings.TrimSpace(message)); err != nil {
		return err
	}
	if decoded.Type != arkintent.IntentMessageTypeRegister {
		return fmt.Errorf("unexpected register message type %q", decoded.Type)
	}
	if !reflect.DeepEqual(decoded.OnchainOutputIndexes, onchainIndexes) {
		return errors.New("register message onchain outputs mismatch")
	}
	if !reflect.DeepEqual(decoded.CosignersPublicKeys, cosignerPubkeys) {
		return errors.New("register message cosigners mismatch")
	}
	return nil
}

func custodyBuildProofPSBT(message string, inputs []arkintent.Input, outputs []*wire.TxOut, leafProofs []*arklib.TaprootMerkleProof, arkFields [][]*psbt.Unknown, locktime arklib.AbsoluteLocktime) (string, error) {
	proof, err := arkintent.New(message, inputs, outputs)
	if err != nil {
		return "", err
	}
	if locktime != 0 {
		proof.UnsignedTx.LockTime = uint32(locktime)
	}
	for i, input := range proof.Inputs {
		var leafProof *arklib.TaprootMerkleProof
		if i == 0 {
			leafProof = leafProofs[0]
		} else {
			leafProof = leafProofs[i-1]
			input.Unknowns = arkFields[i-1]
		}
		input.TaprootLeafScript = []*psbt.TaprootTapLeafScript{{
			ControlBlock: leafProof.ControlBlock,
			Script:       leafProof.Script,
			LeafVersion:  txscript.BaseLeafVersion,
		}}
		proof.Inputs[i] = input
	}
	return proof.B64Encode()
}

func authorizedCustodyProofLocktime(plan *custodySettlementPlan) arklib.AbsoluteLocktime {
	if plan == nil {
		return 0
	}
	locktime := arklib.AbsoluteLocktime(0)
	for _, input := range plan.Inputs {
		if input.SpendPath.UsesCLTVLocktime && input.SpendPath.Locktime > locktime {
			locktime = input.SpendPath.Locktime
		}
	}
	return locktime
}

func custodyTransitionRequestHash(transition tablecustody.CustodyTransition) string {
	return tablecustody.HashCustodyRequest(transition)
}

func resetCustodyRequestTransition(table nativeTableState, transition tablecustody.CustodyTransition) tablecustody.CustodyTransition {
	request := cloneJSON(transition)
	prevStacks := map[string]tablecustody.StackClaim{}
	prevPots := map[string]tablecustody.PotSlice{}
	if table.LatestCustodyState != nil {
		for _, claim := range table.LatestCustodyState.StackClaims {
			prevStacks[claim.PlayerID] = claim
		}
		for _, slice := range table.LatestCustodyState.PotSlices {
			prevPots[slice.PotID] = slice
		}
	}
	for index := range request.NextState.StackClaims {
		claim := request.NextState.StackClaims[index]
		if prevClaim, ok := prevStacks[claim.PlayerID]; ok && reflect.DeepEqual(comparableStackClaim(prevClaim), comparableStackClaim(claim)) {
			request.NextState.StackClaims[index].VTXORefs = append([]tablecustody.VTXORef(nil), prevClaim.VTXORefs...)
			continue
		}
		if _, ok := prevStacks[claim.PlayerID]; !ok && request.Kind != tablecustody.TransitionKindBuyInLock {
			request.NextState.StackClaims[index].VTXORefs = nil
		}
	}
	for index := range request.NextState.PotSlices {
		slice := request.NextState.PotSlices[index]
		if prevSlice, ok := prevPots[slice.PotID]; ok && reflect.DeepEqual(comparablePotSlice(prevSlice), comparablePotSlice(slice)) && sameCanonicalVTXORefs(prevSlice.VTXORefs, slice.VTXORefs) {
			request.NextState.PotSlices[index].VTXORefs = append([]tablecustody.VTXORef(nil), prevSlice.VTXORefs...)
			continue
		}
		if _, ok := prevPots[slice.PotID]; !ok {
			request.NextState.PotSlices[index].VTXORefs = nil
		}
	}
	request.Approvals = nil
	request.ArkIntentID = ""
	request.ArkTxID = ""
	request.Proof.ArkIntentID = ""
	request.Proof.ArkTxID = ""
	request.Proof.ExitProofRef = ""
	request.Proof.FinalizedAt = ""
	request.Proof.RequestHash = ""
	request.Proof.RecoveryBundles = nil
	request.Proof.RecoveryWitness = nil
	request.Proof.ReplayValidated = false
	request.Proof.SettlementWitness = nil
	request.Proof.StateHash = ""
	request.Proof.Signatures = nil
	request.Proof.VTXORefs = nil
	request.Proof.TransitionHash = ""
	return request
}

func signingPlaceholderRefs(plan *custodySettlementPlan, transition tablecustody.CustodyTransition) map[string][]tablecustody.VTXORef {
	refsByClaimKey := map[string][]tablecustody.VTXORef{}
	if plan == nil {
		return refsByClaimKey
	}
	outputIndexByClaimKey := map[string]int{}
	for _, output := range plan.Outputs {
		index := outputIndexByClaimKey[output.ClaimKey]
		outputIndexByClaimKey[output.ClaimKey] = index + 1
		txID := tablecustody.HashValue(map[string]any{
			"claimKey":   output.ClaimKey,
			"custodySeq": transition.CustodySeq,
			"output":     index,
			"tableId":    transition.TableID,
		})
		refsByClaimKey[output.ClaimKey] = append(refsByClaimKey[output.ClaimKey], tablecustody.VTXORef{
			AmountSats:    output.AmountSats,
			OwnerPlayerID: output.OwnerPlayerID,
			Script:        output.Script,
			Tapscripts:    append([]string(nil), output.Tapscripts...),
			TxID:          txID,
			VOut:          uint32(index),
		})
	}
	return refsByClaimKey
}

func applyTransitionPlannedRefs(transition *tablecustody.CustodyTransition, plan *custodySettlementPlan) {
	if transition == nil {
		return
	}
	plannedRefs := signingPlaceholderRefs(plan, *transition)
	applyTransitionSettlementPlan(transition, plan, plannedRefs)
	transition.NextState.StateHash = tablecustody.HashCustodyState(transition.NextState)
	transition.NextStateHash = transition.NextState.StateHash
	transition.Proof.StateHash = transition.NextStateHash
	transition.Proof.VTXORefs = stackProofRefs(transition.NextState)
	transition.Proof.TransitionHash = ""
}

func offchainCustodyBatchOutputs(outputs []custodyBatchOutput) []custodyBatchOutput {
	filtered := make([]custodyBatchOutput, 0, len(outputs))
	for _, output := range outputs {
		if output.Onchain {
			continue
		}
		filtered = append(filtered, output)
	}
	return filtered
}

func containsPlayerID(values []string, playerID string) bool {
	for _, value := range values {
		if value == playerID {
			return true
		}
	}
	return false
}

func custodyInputAuthorizationKey(input custodyInputSpec) string {
	return fundingRefKey(input.Ref) + "|" + fmt.Sprintf("%d", input.Ref.AmountSats) + "|" + hex.EncodeToString(input.SpendPath.PKScript)
}

func custodyOutputAuthorizationKey(valueSats int64, pkScript []byte) string {
	return fmt.Sprintf("%d|%s", valueSats, hex.EncodeToString(pkScript))
}

func validateCustodyProofPSBT(packet *psbt.Packet, plan *custodySettlementPlan, authorizedOutputs []custodyBatchOutput) error {
	if packet == nil {
		return errors.New("custody proof psbt is missing")
	}
	if len(packet.UnsignedTx.TxIn) != len(plan.Inputs)+1 {
		return errors.New("custody proof psbt input set does not match the authorized transition")
	}
	if len(packet.Inputs) != len(packet.UnsignedTx.TxIn) {
		return errors.New("custody proof psbt input metadata is incomplete")
	}
	if len(plan.Inputs) == 0 {
		return errors.New("custody proof psbt is not expected for a no-op transition")
	}
	expectedInputs := map[string]int{}
	for _, input := range plan.Inputs {
		expectedInputs[custodyInputAuthorizationKey(input)]++
	}
	for index, txIn := range packet.UnsignedTx.TxIn[1:] {
		psbtInput := packet.Inputs[index+1]
		if psbtInput.WitnessUtxo == nil {
			return errors.New("custody proof psbt is missing witness utxo metadata")
		}
		actualKey := fmt.Sprintf(
			"%s:%d|%d|%s",
			txIn.PreviousOutPoint.Hash.String(),
			txIn.PreviousOutPoint.Index,
			psbtInput.WitnessUtxo.Value,
			hex.EncodeToString(psbtInput.WitnessUtxo.PkScript),
		)
		if expectedInputs[actualKey] == 0 {
			return fmt.Errorf("custody proof psbt input %s is not authorized by the transition", actualKey)
		}
		expectedInputs[actualKey]--
	}
	for key, count := range expectedInputs {
		if count != 0 {
			return fmt.Errorf("custody proof psbt is missing authorized input %s", key)
		}
	}

	expectedOutputs := map[string]int{}
	if len(authorizedOutputs) == 0 {
		authorizedOutputs = append(authorizedOutputs, plan.AuthorizedOutputs...)
	}
	if len(authorizedOutputs) == 0 {
		for _, output := range plan.Outputs {
			authorizedOutputs = append(authorizedOutputs, custodyBatchOutputFromSpec(output))
		}
	}
	for _, output := range authorizedOutputs {
		txOut, err := decodeBatchOutputTxOut(output)
		if err != nil {
			return err
		}
		expectedOutputs[custodyOutputAuthorizationKey(txOut.Value, txOut.PkScript)]++
	}
	if len(packet.UnsignedTx.TxOut) != len(authorizedOutputs) {
		return errors.New("custody proof psbt output set does not match the authorized transition")
	}
	for _, txOut := range packet.UnsignedTx.TxOut {
		key := custodyOutputAuthorizationKey(txOut.Value, txOut.PkScript)
		if expectedOutputs[key] == 0 {
			return fmt.Errorf("custody proof psbt output %s is not authorized by the transition", key)
		}
		expectedOutputs[key]--
	}
	for key, count := range expectedOutputs {
		if count != 0 {
			return fmt.Errorf("custody proof psbt is missing authorized output %s", key)
		}
	}
	expectedLocktime := authorizedCustodyProofLocktime(plan)
	if expectedLocktime == 0 {
		if packet.UnsignedTx.LockTime != 0 {
			return errors.New("custody proof psbt has an unexpected locktime")
		}
	} else if packet.UnsignedTx.LockTime != uint32(expectedLocktime) {
		return errors.New("custody proof psbt locktime does not match the authorized transition")
	}
	for index, input := range plan.Inputs {
		expectedSequence := uint32(wire.MaxTxInSequenceNum)
		if input.SpendPath.UsesCLTVLocktime {
			expectedSequence = wire.MaxTxInSequenceNum - 1
		}
		if packet.UnsignedTx.TxIn[index+1].Sequence != expectedSequence {
			return fmt.Errorf("custody proof psbt input %d sequence does not match the authorized spend path", index)
		}
	}
	return nil
}

func custodyBatchOutputSemanticKey(output custodyBatchOutput) string {
	return fmt.Sprintf(
		"%s|%s|%d|%t|%s|%s",
		output.ClaimKey,
		output.OwnerPlayerID,
		output.AmountSats,
		output.Onchain,
		output.Script,
		strings.Join(output.Tapscripts, ","),
	)
}

func validateAuthorizedCustodyBatchOutputs(expected, actual []custodyBatchOutput) error {
	expectedCounts := map[string]int{}
	for _, output := range expected {
		expectedCounts[custodyBatchOutputSemanticKey(output)]++
	}
	if len(actual) != len(expected) {
		return errors.New("custody proof outputs do not match the authorized transition")
	}
	for _, output := range actual {
		key := custodyBatchOutputSemanticKey(output)
		if expectedCounts[key] == 0 {
			return fmt.Errorf("custody proof output %s is not authorized by the transition", output.ClaimKey)
		}
		expectedCounts[key]--
	}
	for key, count := range expectedCounts {
		if count != 0 {
			return fmt.Errorf("custody proof is missing authorized output %s", key)
		}
	}
	return nil
}

func (runtime *meshRuntime) validateRequestedCustodyProofOutputs(playerID string, transition tablecustody.CustodyTransition, plan *custodySettlementPlan, outputs []custodyBatchOutput) error {
	if len(outputs) == 0 {
		return nil
	}
	if plan == nil {
		return errors.New("missing custody settlement plan")
	}
	if err := validateAuthorizedCustodyBatchOutputs(plan.AuthorizedOutputs, outputs); err != nil {
		return err
	}
	if transition.Kind != tablecustody.TransitionKindCashOut && transition.Kind != tablecustody.TransitionKindEmergencyExit {
		return nil
	}
	for _, output := range outputs {
		if output.ClaimKey != "wallet-return" {
			continue
		}
		if output.OwnerPlayerID != "" && output.OwnerPlayerID != transition.ActingPlayerID {
			return fmt.Errorf("custody proof output %s is owned by the wrong player", output.ClaimKey)
		}
		if transition.ActingPlayerID != playerID {
			continue
		}
		walletInfo, err := runtime.walletRuntime.GetWallet(runtime.profileName)
		if err != nil {
			return err
		}
		if strings.TrimSpace(walletInfo.ArkAddress) == "" {
			return errors.New("wallet has no Ark address for custody proof signing")
		}
		expected, err := custodyBatchOutputFromReceiver(output.ClaimKey, playerID, sdktypes.Receiver{
			To:     walletInfo.ArkAddress,
			Amount: uint64(output.AmountSats),
		}, output.Tapscripts)
		if err != nil {
			return err
		}
		if expected.Onchain != output.Onchain || expected.Script != output.Script {
			return errors.New("custody proof wallet-return output does not match the requesting player's Ark wallet")
		}
	}
	return nil
}

func validateCustodyForfeitPSBT(packet *psbt.Packet, plan *custodySettlementPlan, playerID string, forfeitPkScript []byte) error {
	if packet == nil {
		return errors.New("custody forfeit psbt is missing")
	}
	if len(packet.UnsignedTx.TxIn) != 2 || len(packet.Inputs) != 2 {
		return errors.New("custody forfeit psbt input set does not match the authorized transition")
	}
	if len(packet.UnsignedTx.TxOut) != 2 {
		return errors.New("custody forfeit psbt output set does not match the authorized transition")
	}

	authorized := map[string]int{}
	for _, input := range plan.Inputs {
		if !containsPlayerID(input.SpendPath.PlayerIDs, playerID) {
			continue
		}
		authorized[custodyInputAuthorizationKey(input)]++
	}
	if len(authorized) == 0 {
		return errors.New("custody forfeit psbt is not authorized for this player")
	}

	firstInput := packet.Inputs[0]
	secondInput := packet.Inputs[1]
	if firstInput.WitnessUtxo == nil || secondInput.WitnessUtxo == nil {
		return errors.New("custody forfeit psbt is missing witness utxo metadata")
	}
	actualKey := fmt.Sprintf(
		"%s:%d|%d|%s",
		packet.UnsignedTx.TxIn[0].PreviousOutPoint.Hash.String(),
		packet.UnsignedTx.TxIn[0].PreviousOutPoint.Index,
		firstInput.WitnessUtxo.Value,
		hex.EncodeToString(firstInput.WitnessUtxo.PkScript),
	)
	if authorized[actualKey] == 0 {
		return fmt.Errorf("custody forfeit psbt input %s is not authorized by the transition", actualKey)
	}
	if !bytes.Equal(packet.UnsignedTx.TxOut[0].PkScript, forfeitPkScript) {
		return errors.New("custody forfeit psbt does not pay the Ark forfeit address")
	}
	if !bytes.Equal(packet.UnsignedTx.TxOut[1].PkScript, arktxutils.ANCHOR_PKSCRIPT) {
		return errors.New("custody forfeit psbt is missing the anchor output")
	}
	sumInputs := firstInput.WitnessUtxo.Value + secondInput.WitnessUtxo.Value
	sumOutputs := packet.UnsignedTx.TxOut[0].Value + packet.UnsignedTx.TxOut[1].Value
	if sumInputs != sumOutputs {
		return errors.New("custody forfeit psbt does not conserve the authorized value")
	}
	expectedLocktime := uint32(0)
	expectedSequence := uint32(wire.MaxTxInSequenceNum)
	for _, input := range plan.Inputs {
		if !containsPlayerID(input.SpendPath.PlayerIDs, playerID) {
			continue
		}
		if input.SpendPath.UsesCLTVLocktime {
			expectedLocktime = uint32(input.SpendPath.Locktime)
			expectedSequence = wire.MaxTxInSequenceNum - 1
		}
		break
	}
	if packet.UnsignedTx.LockTime != expectedLocktime {
		return errors.New("custody forfeit psbt locktime does not match the authorized spend path")
	}
	if packet.UnsignedTx.TxIn[0].Sequence != expectedSequence {
		return errors.New("custody forfeit psbt sequence does not match the authorized spend path")
	}
	return nil
}

func connectorOutputFromLeaf(connectorLeaf *psbt.Packet) (*wire.TxOut, *wire.OutPoint, error) {
	if connectorLeaf == nil {
		return nil, nil, errors.New("connector leaf is missing")
	}
	for outputIndex, output := range connectorLeaf.UnsignedTx.TxOut {
		if bytes.Equal(arktxutils.ANCHOR_PKSCRIPT, output.PkScript) {
			continue
		}
		return output, &wire.OutPoint{
			Hash:  connectorLeaf.UnsignedTx.TxHash(),
			Index: uint32(outputIndex),
		}, nil
	}
	return nil, nil, errors.New("connector output was not found")
}

func validateSignedCustodyForfeitPSBT(packet *psbt.Packet, input custodyInputSpec, connectorLeaf *psbt.Packet, forfeitPkScript []byte) error {
	if packet == nil {
		return errors.New("custody signed forfeit psbt is missing")
	}
	if len(packet.UnsignedTx.TxIn) != 2 || len(packet.Inputs) != 2 {
		return errors.New("custody signed forfeit psbt input set does not match the expected pair")
	}
	if len(packet.UnsignedTx.TxOut) != 2 {
		return errors.New("custody signed forfeit psbt output set does not match the expected pair")
	}
	connectorOutput, connectorOutpoint, err := connectorOutputFromLeaf(connectorLeaf)
	if err != nil {
		return err
	}
	vtxoHash, err := chainhash.NewHashFromStr(input.Ref.TxID)
	if err != nil {
		return err
	}
	expectedVtxo := wire.OutPoint{Hash: *vtxoHash, Index: input.Ref.VOut}
	vtxoIndex := -1
	connectorIndex := -1
	switch {
	case packet.UnsignedTx.TxIn[0].PreviousOutPoint == expectedVtxo && packet.UnsignedTx.TxIn[1].PreviousOutPoint == *connectorOutpoint:
		vtxoIndex = 0
		connectorIndex = 1
	case packet.UnsignedTx.TxIn[1].PreviousOutPoint == expectedVtxo && packet.UnsignedTx.TxIn[0].PreviousOutPoint == *connectorOutpoint:
		vtxoIndex = 1
		connectorIndex = 0
	default:
		return fmt.Errorf("custody signed forfeit psbt does not spend the expected input %s together with connector %s", fundingRefKey(input.Ref), connectorOutpoint.String())
	}

	vtxoInput := packet.Inputs[vtxoIndex]
	connectorInput := packet.Inputs[connectorIndex]
	if vtxoInput.WitnessUtxo == nil || connectorInput.WitnessUtxo == nil {
		return errors.New("custody signed forfeit psbt is missing witness utxo metadata")
	}
	if vtxoInput.WitnessUtxo.Value != int64(input.Ref.AmountSats) || !bytes.Equal(vtxoInput.WitnessUtxo.PkScript, input.SpendPath.PKScript) {
		return errors.New("custody signed forfeit psbt witness utxo does not match the authorized custody input")
	}
	if connectorInput.WitnessUtxo.Value != connectorOutput.Value || !bytes.Equal(connectorInput.WitnessUtxo.PkScript, connectorOutput.PkScript) {
		return errors.New("custody signed forfeit psbt witness utxo does not match the expected connector output")
	}
	if len(vtxoInput.TaprootScriptSpendSig) == 0 {
		return errors.New("custody signed forfeit psbt is missing tapscript signatures for the custody input")
	}
	if len(vtxoInput.TaprootLeafScript) == 0 {
		return errors.New("custody signed forfeit psbt is missing the custody leaf proof")
	}
	leaf := vtxoInput.TaprootLeafScript[0]
	if !bytes.Equal(leaf.Script, input.SpendPath.Script) {
		return errors.New("custody signed forfeit psbt tapscript does not match the authorized spend path")
	}
	if input.SpendPath.LeafProof != nil && !bytes.Equal(leaf.ControlBlock, input.SpendPath.LeafProof.ControlBlock) {
		return errors.New("custody signed forfeit psbt control block does not match the authorized spend path")
	}
	if !bytes.Equal(packet.UnsignedTx.TxOut[0].PkScript, forfeitPkScript) {
		return errors.New("custody signed forfeit psbt does not pay the Ark forfeit address")
	}
	if !bytes.Equal(packet.UnsignedTx.TxOut[1].PkScript, arktxutils.ANCHOR_PKSCRIPT) {
		return errors.New("custody signed forfeit psbt is missing the anchor output")
	}
	sumInputs := vtxoInput.WitnessUtxo.Value + connectorInput.WitnessUtxo.Value
	sumOutputs := packet.UnsignedTx.TxOut[0].Value + packet.UnsignedTx.TxOut[1].Value
	if sumInputs != sumOutputs {
		return errors.New("custody signed forfeit psbt does not conserve the expected value")
	}

	vtxoSequence := uint32(wire.MaxTxInSequenceNum)
	locktime := uint32(0)
	if input.SpendPath.UsesCLTVLocktime {
		vtxoSequence = wire.MaxTxInSequenceNum - 1
		locktime = uint32(input.SpendPath.Locktime)
	}
	if packet.UnsignedTx.LockTime != locktime {
		return errors.New("custody signed forfeit psbt locktime does not match the authorized spend path")
	}
	if packet.UnsignedTx.TxIn[vtxoIndex].Sequence != vtxoSequence {
		return errors.New("custody signed forfeit psbt sequence does not match the authorized spend path")
	}
	if packet.UnsignedTx.TxIn[connectorIndex].Sequence != wire.MaxTxInSequenceNum {
		return errors.New("custody signed forfeit psbt connector sequence is invalid")
	}

	var inputs []*wire.OutPoint
	var sequences []uint32
	var prevouts []*wire.TxOut
	if vtxoIndex == 0 {
		inputs = []*wire.OutPoint{
			&wire.OutPoint{Hash: expectedVtxo.Hash, Index: expectedVtxo.Index},
			connectorOutpoint,
		}
		sequences = []uint32{vtxoSequence, wire.MaxTxInSequenceNum}
		prevouts = []*wire.TxOut{
			&wire.TxOut{Value: int64(input.Ref.AmountSats), PkScript: input.SpendPath.PKScript},
			connectorOutput,
		}
	} else {
		inputs = []*wire.OutPoint{
			connectorOutpoint,
			&wire.OutPoint{Hash: expectedVtxo.Hash, Index: expectedVtxo.Index},
		}
		sequences = []uint32{wire.MaxTxInSequenceNum, vtxoSequence}
		prevouts = []*wire.TxOut{
			connectorOutput,
			&wire.TxOut{Value: int64(input.Ref.AmountSats), PkScript: input.SpendPath.PKScript},
		}
	}
	rebuilt, err := arktree.BuildForfeitTx(inputs, sequences, prevouts, forfeitPkScript, locktime)
	if err != nil {
		return err
	}
	if rebuilt.UnsignedTx.TxID() != packet.UnsignedTx.TxID() {
		return fmt.Errorf("custody signed forfeit psbt txid mismatch: expected %s, got %s", rebuilt.UnsignedTx.TxID(), packet.UnsignedTx.TxID())
	}
	return nil
}

func validateSignedCustodyForfeits(plan *custodySettlementPlan, connectorLeaves []*psbt.Packet, signedForfeits []string, forfeitPkScript []byte) error {
	if plan == nil {
		return errors.New("custody settlement plan is missing")
	}
	if len(plan.Inputs) == 0 {
		if len(signedForfeits) != 0 {
			return errors.New("custody signed forfeits are unexpected for a no-input transition")
		}
		return nil
	}
	if len(connectorLeaves) != len(plan.Inputs) {
		return fmt.Errorf("connector tree leaf count does not match the authorized custody inputs: %d != %d", len(connectorLeaves), len(plan.Inputs))
	}
	if len(signedForfeits) != len(plan.Inputs) {
		return fmt.Errorf("signed forfeit count does not match the authorized custody inputs: %d != %d", len(signedForfeits), len(plan.Inputs))
	}
	seenInputs := make(map[string]struct{}, len(plan.Inputs))
	seenConnectors := make(map[string]struct{}, len(connectorLeaves))
	for index, input := range plan.Inputs {
		key := fundingRefKey(input.Ref)
		if _, ok := seenInputs[key]; ok {
			return fmt.Errorf("custody settlement plan reuses source input %s", key)
		}
		seenInputs[key] = struct{}{}
		_, connectorOutpoint, err := connectorOutputFromLeaf(connectorLeaves[index])
		if err != nil {
			return err
		}
		connectorKey := connectorOutpoint.String()
		if _, ok := seenConnectors[connectorKey]; ok {
			return fmt.Errorf("connector tree reuses connector %s", connectorKey)
		}
		seenConnectors[connectorKey] = struct{}{}
		packet, err := psbt.NewFromRawBytes(strings.NewReader(signedForfeits[index]), true)
		if err != nil {
			return err
		}
		if err := validateSignedCustodyForfeitPSBT(packet, input, connectorLeaves[index], forfeitPkScript); err != nil {
			return fmt.Errorf("custody signed forfeit %d is invalid: %w", index, err)
		}
	}
	return nil
}

func offchainVtxoTreeOutputs(vtxoTree *arktree.TxTree) []custodyBatchOutput {
	if vtxoTree == nil {
		return nil
	}
	outputs := make([]custodyBatchOutput, 0)
	for _, leaf := range vtxoTree.Leaves() {
		for _, txOut := range leaf.UnsignedTx.TxOut {
			if bytes.Equal(txOut.PkScript, arktxutils.ANCHOR_PKSCRIPT) {
				continue
			}
			outputs = append(outputs, custodyBatchOutput{
				AmountSats: int(txOut.Value),
				Script:     hex.EncodeToString(txOut.PkScript),
			})
		}
	}
	return outputs
}

func validateCustodyOffchainOutputs(expected, actual []custodyBatchOutput) error {
	expectedCounts := map[string]int{}
	for _, output := range expected {
		key := fmt.Sprintf("%d|%s", output.AmountSats, output.Script)
		expectedCounts[key]++
	}
	if len(actual) != len(expected) {
		return errors.New("custody vtxo tree outputs do not match the authorized transition")
	}
	for _, output := range actual {
		key := fmt.Sprintf("%d|%s", output.AmountSats, output.Script)
		if expectedCounts[key] == 0 {
			return fmt.Errorf("custody vtxo tree output %s is not authorized by the transition", key)
		}
		expectedCounts[key]--
	}
	for key, count := range expectedCounts {
		if count != 0 {
			return fmt.Errorf("custody vtxo tree is missing authorized output %s", key)
		}
	}
	return nil
}

func (runtime *meshRuntime) normalizedCustodySigningTransition(table nativeTableState, transition tablecustody.CustodyTransition) (tablecustody.CustodyTransition, *custodySettlementPlan, error) {
	if transition.Kind == tablecustody.TransitionKindTurnChallengeOpen || transition.Kind == tablecustody.TransitionKindTurnChallengeEscape {
		return cloneJSON(transition), nil, nil
	}
	plan, err := runtime.buildCustodySettlementPlan(table, transition)
	if err != nil {
		return tablecustody.CustodyTransition{}, nil, err
	}
	normalized := cloneJSON(transition)
	applyTransitionPlannedRefs(&normalized, plan)
	return normalized, plan, nil
}

func challengeResolutionApprovalSource(table nativeTableState, transition tablecustody.CustodyTransition) (nativeTableState, tablecustody.CustodyTransition, bool, error) {
	if transition.Kind != tablecustody.TransitionKindAction && transition.Kind != tablecustody.TransitionKindTimeout {
		return nativeTableState{}, tablecustody.CustodyTransition{}, false, nil
	}
	if transition.Proof.ChallengeBundle == nil && transition.Proof.ChallengeWitness == nil {
		return nativeTableState{}, tablecustody.CustodyTransition{}, false, nil
	}
	openTransition, openPrevious, _, err := acceptedChallengeOpenContextForTransition(table, transition)
	if err != nil {
		return nativeTableState{}, tablecustody.CustodyTransition{}, false, err
	}
	sourceTable := cloneJSON(table)
	sourceTable.LatestCustodyState = openPrevious
	sourceTransition := cloneJSON(transition)
	sourceTransition.CustodySeq = openTransition.CustodySeq
	sourceTransition.PrevStateHash = openTransition.PrevStateHash
	sourceTransition.NextState.CustodySeq = openTransition.NextState.CustodySeq
	sourceTransition.NextState.PrevStateHash = openTransition.NextState.PrevStateHash
	return sourceTable, sourceTransition, true, nil
}

func (runtime *meshRuntime) normalizedCustodyApprovalTransition(table nativeTableState, transition tablecustody.CustodyTransition) (tablecustody.CustodyTransition, *custodySettlementPlan, error) {
	if transition.Kind == tablecustody.TransitionKindTurnChallengeOpen || transition.Kind == tablecustody.TransitionKindTurnChallengeEscape {
		normalized := cloneJSON(transition)
		normalized.Proof.RequestHash = custodyTransitionRequestHash(normalized)
		return normalized, nil, nil
	}
	if sourceTable, sourceTransition, ok, err := challengeResolutionApprovalSource(table, transition); err != nil {
		return tablecustody.CustodyTransition{}, nil, err
	} else if ok {
		table = sourceTable
		transition = sourceTransition
	}
	request := resetCustodyRequestTransition(table, transition)
	normalized, plan, err := runtime.normalizedCustodySigningTransition(table, request)
	if err != nil {
		return tablecustody.CustodyTransition{}, nil, err
	}
	normalized.Proof.RequestHash = custodyTransitionRequestHash(normalized)
	return normalized, plan, nil
}

func custodyTransitionBatchOutputs(previous *tablecustody.CustodyState, transition tablecustody.CustodyTransition) []custodyBatchOutput {
	outputs := make([]custodyBatchOutput, 0)
	prevStacks := map[string]tablecustody.StackClaim{}
	prevPots := map[string]tablecustody.PotSlice{}
	if previous != nil {
		for _, claim := range previous.StackClaims {
			prevStacks[claim.PlayerID] = claim
		}
		for _, slice := range previous.PotSlices {
			prevPots[slice.PotID] = slice
		}
	}

	for _, claim := range transition.NextState.StackClaims {
		prevClaim, hadPrev := prevStacks[claim.PlayerID]
		if hadPrev && reflect.DeepEqual(comparableStackClaim(prevClaim), comparableStackClaim(claim)) {
			continue
		}
		for _, ref := range claim.VTXORefs {
			outputs = append(outputs, custodyBatchOutput{
				AmountSats:    ref.AmountSats,
				OwnerPlayerID: claim.PlayerID,
				Script:        ref.Script,
			})
		}
	}
	for _, slice := range transition.NextState.PotSlices {
		prevSlice, hadPrev := prevPots[slice.PotID]
		if hadPrev && reflect.DeepEqual(comparablePotSlice(prevSlice), comparablePotSlice(slice)) && sameCanonicalVTXORefs(prevSlice.VTXORefs, slice.VTXORefs) {
			continue
		}
		for _, ref := range slice.VTXORefs {
			outputs = append(outputs, custodyBatchOutput{
				AmountSats: ref.AmountSats,
				Script:     ref.Script,
			})
		}
	}
	return outputs
}

func (runtime *meshRuntime) custodyTreeSignerIDs(table nativeTableState, transition tablecustody.CustodyTransition) []string {
	signerIDs := append([]string(nil), runtime.requiredCustodySigners(table, transition)...)
	if len(signerIDs) == 0 {
		signerIDs = append(signerIDs, playerIDsFromSeats(table.Seats)...)
	}
	if len(signerIDs) == 0 {
		for _, claim := range transition.NextState.StackClaims {
			signerIDs = append(signerIDs, claim.PlayerID)
		}
	}
	return uniqueSortedPlayerIDs(signerIDs)
}

func (runtime *meshRuntime) validatePrebuiltCustodySigningTransition(table nativeTableState, expectedPrevStateHash, transitionHash string, transition tablecustody.CustodyTransition, authorizer *nativeTransitionAuthorizer) error {
	if expectedPrevStateHash != "" && latestCustodyStateHash(table) != expectedPrevStateHash {
		return errors.New("custody signing request references stale state")
	}
	if strings.TrimSpace(transitionHash) == "" {
		return errors.New("custody signing request is missing transition hash")
	}
	if err := runtime.validateCustodyTransitionSemanticsWithOptions(table, transition, authorizer, true); err != nil {
		return err
	}
	approvalTransition, _, err := runtime.normalizedCustodyApprovalTransition(table, transition)
	if err != nil {
		return err
	}
	if approvalTransition.Proof.RequestHash != transitionHash {
		return errors.New("custody signing request transition hash mismatch")
	}
	if err := tablecustody.ValidateTransition(table.LatestCustodyState, approvalTransition); err != nil {
		return err
	}
	if approvalTransition.Kind == tablecustody.TransitionKindTurnChallengeOpen ||
		approvalTransition.Kind == tablecustody.TransitionKindTurnChallengeEscape {
		return nil
	}
	if err := validateAcceptedCustodyRefs(table.LatestCustodyState, approvalTransition, false); err != nil {
		return err
	}
	return nil
}

func (runtime *meshRuntime) validateBuyInLockSignerPrepareTransition(table nativeTableState, expectedPrevStateHash, transitionHash string, transition tablecustody.CustodyTransition, authorizer *nativeTransitionAuthorizer, expectedOffchainOutputs []custodyBatchOutput) error {
	if expectedPrevStateHash != "" && latestCustodyStateHash(table) != expectedPrevStateHash {
		return errors.New("custody signing request references stale state")
	}
	if strings.TrimSpace(transitionHash) == "" {
		return errors.New("custody signing request is missing transition hash")
	}
	if authorizer != nil {
		return errors.New("buy-in-lock signer prepare does not accept transition authorizers")
	}
	previousClaims := map[string]tablecustody.StackClaim{}
	if table.LatestCustodyState != nil {
		for _, claim := range table.LatestCustodyState.StackClaims {
			previousClaims[claim.PlayerID] = claim
		}
	}
	if len(transition.NextState.PotSlices) != 0 {
		return fmt.Errorf("custody %s transition does not match the locally derived successor", transition.Kind)
	}
	if limit := maxInt(0, table.Config.SeatCount); limit > 0 && len(transition.NextState.StackClaims) > limit {
		return fmt.Errorf("custody %s transition does not match the locally derived successor", transition.Kind)
	}

	seenSeatIndexes := map[int]struct{}{}
	seenPlayers := map[string]struct{}{}
	balances := make([]tablecustody.PlayerBalance, 0, len(transition.NextState.StackClaims))
	newClaims := 0
	for _, claim := range transition.NextState.StackClaims {
		if _, ok := seenPlayers[claim.PlayerID]; ok {
			return fmt.Errorf("custody %s transition does not match the locally derived successor", transition.Kind)
		}
		seenPlayers[claim.PlayerID] = struct{}{}
		if _, ok := seenSeatIndexes[claim.SeatIndex]; ok {
			return fmt.Errorf("custody %s transition does not match the locally derived successor", transition.Kind)
		}
		seenSeatIndexes[claim.SeatIndex] = struct{}{}

		if previous, ok := previousClaims[claim.PlayerID]; ok {
			if !reflect.DeepEqual(comparableStackClaim(previous), comparableStackClaim(claim)) {
				return fmt.Errorf("custody %s transition does not match the locally derived successor", transition.Kind)
			}
		} else {
			newClaims++
			switch {
			case claim.AmountSats <= 0,
				claim.RoundContributionSats != 0,
				claim.TotalContributionSats != 0,
				claim.AllIn,
				claim.Folded,
				claim.Status != "active":
				return fmt.Errorf("custody %s transition does not match the locally derived successor", transition.Kind)
			}
		}
		balances = append(balances, tablecustody.PlayerBalance{
			AllIn:                 claim.AllIn,
			Folded:                claim.Folded,
			PlayerID:              claim.PlayerID,
			ReservedFeeSats:       claim.ReservedFeeSats,
			RoundContributionSats: claim.RoundContributionSats,
			SeatIndex:             claim.SeatIndex,
			StackSats:             claim.AmountSats,
			Status:                claim.Status,
			TotalContributionSats: claim.TotalContributionSats,
		})
	}
	for playerID := range previousClaims {
		if _, ok := seenPlayers[playerID]; !ok {
			return fmt.Errorf("custody %s transition does not match the locally derived successor", transition.Kind)
		}
	}
	if newClaims == 0 {
		return fmt.Errorf("custody %s transition does not match the locally derived successor", transition.Kind)
	}

	expected, err := tablecustody.BuildTransition(tablecustody.TransitionKindBuyInLock, runtime.custodyStateBinding(table, tablecustody.TransitionKindBuyInLock, nil, nil), balances, table.LatestCustodyState, nil, nil)
	if err != nil {
		return err
	}
	comparableExpected := comparableSemanticCustodyTransition(expected)
	comparableSupplied := comparableSemanticCustodyTransition(transition)
	if !reflect.DeepEqual(comparableSupplied, comparableExpected) {
		return fmt.Errorf("custody %s transition does not match the locally derived successor", transition.Kind)
	}
	if err := validateStableSemanticCustodyBindings(expected, transition); err != nil {
		return err
	}
	if requestHash := strings.TrimSpace(transition.Proof.RequestHash); requestHash != "" && requestHash != transitionHash {
		return errors.New("custody signing request transition hash mismatch")
	}
	if custodyTransitionRequestHash(transition) != transitionHash {
		return errors.New("custody signing request transition hash mismatch")
	}
	if err := tablecustody.ValidateTransition(table.LatestCustodyState, transition); err != nil {
		return err
	}
	if err := validateAcceptedCustodyRefs(table.LatestCustodyState, transition, false); err != nil {
		return err
	}
	transitionOutputs := offchainCustodyBatchOutputs(custodyTransitionBatchOutputs(table.LatestCustodyState, transition))
	if len(expectedOffchainOutputs) > 0 {
		if err := validateCustodyOffchainOutputs(transitionOutputs, offchainCustodyBatchOutputs(expectedOffchainOutputs)); err != nil {
			return err
		}
	}
	return nil
}

func (runtime *meshRuntime) validateCustodySigningTransition(table nativeTableState, playerID, expectedPrevStateHash, transitionHash string, transition tablecustody.CustodyTransition, authorizer *nativeTransitionAuthorizer) (*custodySettlementPlan, error) {
	normalized, plan, err := runtime.normalizedCustodySigningTransition(table, transition)
	if err != nil {
		return nil, err
	}
	if err := runtime.validatePrebuiltCustodySigningTransition(table, expectedPrevStateHash, transitionHash, normalized, authorizer); err != nil {
		return nil, err
	}
	return plan, nil
}

func acceptedRecoverySigningTransition(table nativeTableState, requestHash string, transition tablecustody.CustodyTransition) (*tablecustody.CustodyTransition, bool) {
	for index := len(table.CustodyTransitions) - 1; index >= 0; index-- {
		candidate := table.CustodyTransitions[index]
		if candidate.Proof.RequestHash != requestHash {
			continue
		}
		if candidate.NextStateHash != transition.NextStateHash {
			continue
		}
		if !reflect.DeepEqual(cloneJSON(candidate), cloneJSON(transition)) {
			continue
		}
		accepted := cloneJSON(candidate)
		return &accepted, true
	}
	return nil, false
}

func (runtime *meshRuntime) validateRecoverySigningTransition(table nativeTableState, expectedPrevStateHash, transitionHash string, transition tablecustody.CustodyTransition, authorizer *nativeTransitionAuthorizer) error {
	currentStateHash := latestCustodyStateHash(table)
	switch {
	case expectedPrevStateHash == "" || currentStateHash == expectedPrevStateHash:
		return runtime.validatePrebuiltCustodySigningTransition(table, expectedPrevStateHash, transitionHash, transition, authorizer)
	case currentStateHash == transition.NextStateHash:
		if _, ok := acceptedRecoverySigningTransition(table, transitionHash, transition); ok {
			return nil
		}
	}
	return errors.New("custody signing request references stale state")
}

func (runtime *meshRuntime) signCustodyPSBTWithPlayer(table nativeTableState, playerID, prevStateHash, transitionHash, purpose, current string, transition tablecustody.CustodyTransition, authorizer *nativeTransitionAuthorizer, outputs []custodyBatchOutput) (string, error) {
	if playerID == runtime.walletID.PlayerID {
		return runtime.signLocalCustodyPSBT(current)
	}
	seat, ok := seatRecordForPlayer(table, playerID)
	if !ok {
		return "", fmt.Errorf("missing seat for signer %s", playerID)
	}
	if seat.PeerURL == "" {
		return "", fmt.Errorf("missing peer url for signer %s", playerID)
	}
	signedPSBT, err := runtime.remoteSignCustodyPSBT(seat.PeerURL, nativeCustodyTxSignRequest{
		ExpectedPrevStateHash: prevStateHash,
		ExpectedOutputs:       append([]custodyBatchOutput(nil), outputs...),
		Authorizer:            cloneTransitionAuthorizer(authorizer),
		PSBT:                  current,
		PlayerID:              playerID,
		Purpose:               purpose,
		ProtocolVersion:       nativeProtocolVersion,
		TableID:               table.Config.TableID,
		Transition:            transition,
		TransitionHash:        transitionHash,
	})
	if err != nil {
		return "", fmt.Errorf("remote custody %s signing for %s: %w", purpose, playerID, err)
	}
	return signedPSBT, nil
}

func (runtime *meshRuntime) fullySignCustodyPSBT(table nativeTableState, prevStateHash, transitionHash, purpose string, signerIDs []string, unsigned string, transition tablecustody.CustodyTransition, authorizer *nativeTransitionAuthorizer, outputs []custodyBatchOutput) (signed string, err error) {
	timingFields := custodyTimingFields(table, transition, "custody_psbt_fully_sign")
	timingFields.Purpose = purpose
	timingFields.OutputCount = len(outputs)
	timing := startMeshTiming(timingFields)
	defer func() {
		timing.EndWith(timingFields, err)
	}()
	signed = unsigned
	for _, playerID := range uniqueSortedPlayerIDs(signerIDs) {
		nextSigned, err := runtime.signCustodyPSBTWithPlayer(table, playerID, prevStateHash, transitionHash, purpose, signed, transition, authorizer, outputs)
		if err != nil {
			return "", fmt.Errorf("custody %s signing via %s: %w", purpose, playerID, err)
		}
		signed = nextSigned
	}
	signed, err = runtime.maybeSignCustodyPSBTAsOperator(purpose, signed)
	if err != nil {
		return "", fmt.Errorf("custody %s operator signing: %w", purpose, err)
	}
	return signed, nil
}

func (runtime *meshRuntime) handleCustodyTxSignFromPeer(request nativeCustodyTxSignRequest) (nativeCustodyTxSignResponse, error) {
	table, err := runtime.requireLocalTable(request.TableID)
	if err != nil {
		return nativeCustodyTxSignResponse{}, err
	}
	if err := validateSettlementRequestProtocolVersion(request.ProtocolVersion); err != nil {
		return nativeCustodyTxSignResponse{}, err
	}
	if request.PlayerID != runtime.walletID.PlayerID {
		return nativeCustodyTxSignResponse{}, errors.New("custody tx signing request is not addressed to this player")
	}
	packet, err := psbt.NewFromRawBytes(strings.NewReader(request.PSBT), true)
	if err != nil {
		return nativeCustodyTxSignResponse{}, err
	}
	switch request.Purpose {
	case "", "proof":
		plan, err := runtime.validateCustodySigningTransition(*table, request.PlayerID, request.ExpectedPrevStateHash, request.TransitionHash, request.Transition, request.Authorizer)
		if err != nil {
			return nativeCustodyTxSignResponse{}, err
		}
		if !containsPlayerID(plan.ProofSignerIDs, request.PlayerID) {
			return nativeCustodyTxSignResponse{}, errors.New("custody tx signing request is not authorized for this player")
		}
		if err := runtime.validateRequestedCustodyProofOutputs(request.PlayerID, request.Transition, plan, request.ExpectedOutputs); err != nil {
			return nativeCustodyTxSignResponse{}, err
		}
		if err := validateCustodyProofPSBT(packet, plan, request.ExpectedOutputs); err != nil {
			return nativeCustodyTxSignResponse{}, err
		}
	case "forfeit":
		plan, err := runtime.validateCustodySigningTransition(*table, request.PlayerID, request.ExpectedPrevStateHash, request.TransitionHash, request.Transition, request.Authorizer)
		if err != nil {
			return nativeCustodyTxSignResponse{}, err
		}
		config, err := runtime.arkCustodyConfig()
		if err != nil {
			return nativeCustodyTxSignResponse{}, err
		}
		parsedForfeitAddr, err := btcutil.DecodeAddress(config.ForfeitAddress, nil)
		if err != nil {
			return nativeCustodyTxSignResponse{}, err
		}
		forfeitPkScript, err := txscript.PayToAddrScript(parsedForfeitAddr)
		if err != nil {
			return nativeCustodyTxSignResponse{}, err
		}
		if err := validateCustodyForfeitPSBT(packet, plan, request.PlayerID, forfeitPkScript); err != nil {
			return nativeCustodyTxSignResponse{}, err
		}
	case "recovery":
		if err := runtime.validateRecoverySigningTransition(*table, request.ExpectedPrevStateHash, request.TransitionHash, request.Transition, request.Authorizer); err != nil {
			return nativeCustodyTxSignResponse{}, err
		}
		sourceRefs := sourcePotRecoveryRefs(&request.Transition.NextState)
		if len(sourceRefs) == 0 {
			return nativeCustodyTxSignResponse{}, errors.New("custody recovery signing request is missing source pot refs")
		}
		spendPaths := make([]custodySpendPath, 0, len(sourceRefs))
		authorizedPlayers := map[string]struct{}{}
		for _, ref := range sourceRefs {
			spendPath, err := runtime.selectPotCSVExitSpendPath(*table, ref)
			if err != nil {
				return nativeCustodyTxSignResponse{}, err
			}
			spendPaths = append(spendPaths, spendPath)
			for _, playerID := range spendPath.PlayerIDs {
				authorizedPlayers[playerID] = struct{}{}
			}
		}
		if _, ok := authorizedPlayers[request.PlayerID]; !ok {
			return nativeCustodyTxSignResponse{}, errors.New("custody recovery signing request is not authorized for this player")
		}
		if len(request.ExpectedOutputs) == 0 {
			return nativeCustodyTxSignResponse{}, errors.New("custody recovery signing request is missing authorized outputs")
		}
		if err := validateCustodyRecoveryPSBT(packet, sourceRefs, spendPaths, request.ExpectedOutputs); err != nil {
			return nativeCustodyTxSignResponse{}, err
		}
	case "challenge-open":
		if err := runtime.validatePrebuiltCustodySigningTransition(*table, request.ExpectedPrevStateHash, request.TransitionHash, request.Transition, request.Authorizer); err != nil {
			return nativeCustodyTxSignResponse{}, err
		}
		turnDeadlineAt := strings.TrimSpace(request.Transition.NextState.ActionDeadlineAt)
		if request.Authorizer != nil && strings.TrimSpace(request.Authorizer.TurnDeadlineAt) != "" {
			turnDeadlineAt = strings.TrimSpace(request.Authorizer.TurnDeadlineAt)
		}
		sourceRefs := turnChallengeSourceRefs(table.LatestCustodyState)
		spendPaths, authorizedPlayers, err := runtime.challengeOpenSpendPaths(*table, sourceRefs, turnDeadlineAt)
		if err != nil {
			return nativeCustodyTxSignResponse{}, err
		}
		if !containsPlayerID(authorizedPlayers, request.PlayerID) {
			return nativeCustodyTxSignResponse{}, errors.New("custody challenge signing request is not authorized for this player")
		}
		spec, err := runtime.challengeRefOutputSpec(*table, sumVTXORefs(sourceRefs))
		if err != nil {
			return nativeCustodyTxSignResponse{}, err
		}
		outputs := challengeOutputsFromBatchOutputs([]custodyBatchOutput{custodyBatchOutputFromSpec(spec)})
		txLocktime, err := challengeOpenBundleLocktime(turnDeadlineAt, spendPaths)
		if err != nil {
			return nativeCustodyTxSignResponse{}, err
		}
		if err := validateCustodyChallengePSBT(packet, sourceRefs, spendPaths, outputs, txLocktime); err != nil {
			return nativeCustodyTxSignResponse{}, err
		}
	case "challenge-escape":
		if err := runtime.validatePrebuiltCustodySigningTransition(*table, request.ExpectedPrevStateHash, request.TransitionHash, request.Transition, request.Authorizer); err != nil {
			return nativeCustodyTxSignResponse{}, err
		}
		if request.Authorizer == nil || request.Authorizer.ChallengeOpenBundle == nil {
			return nativeCustodyTxSignResponse{}, errors.New("custody challenge escape signing request is missing its open bundle authorizer")
		}
		turnDeadlineAt := strings.TrimSpace(request.Transition.NextState.ActionDeadlineAt)
		if request.Authorizer != nil && strings.TrimSpace(request.Authorizer.TurnDeadlineAt) != "" {
			turnDeadlineAt = strings.TrimSpace(request.Authorizer.TurnDeadlineAt)
		}
		validationTable := tableWithTurnDeadline(*table, turnDeadlineAt)
		menu := &NativePendingTurnMenu{ActionDeadlineAt: turnDeadlineAt}
		challengeRef, err := runtime.validateTurnChallengeOpenBundle(validationTable, menu, *request.Authorizer.ChallengeOpenBundle)
		if err != nil {
			return nativeCustodyTxSignResponse{}, err
		}
		spendPath, err := runtime.selectTurnChallengeExitSpendPath(validationTable, challengeRef)
		if err != nil {
			return nativeCustodyTxSignResponse{}, err
		}
		if !containsPlayerID(spendPath.PlayerIDs, request.PlayerID) {
			return nativeCustodyTxSignResponse{}, errors.New("custody challenge escape signing request is not authorized for this player")
		}
		outputs, err := runtime.fullChallengeOutputsForTransition(validationTable, request.Transition)
		if err != nil {
			return nativeCustodyTxSignResponse{}, err
		}
		if err := validateCustodyChallengePSBT(packet, []tablecustody.VTXORef{challengeRef}, []custodySpendPath{spendPath}, outputs, 0); err != nil {
			return nativeCustodyTxSignResponse{}, err
		}
	case "challenge-option", "challenge-timeout":
		if request.Authorizer == nil || request.Authorizer.ChallengeOpenBundle == nil {
			return nativeCustodyTxSignResponse{}, errors.New("custody challenge signing request is missing its open bundle context")
		}
		if request.ExpectedPrevStateHash != "" && latestCustodyStateHash(*table) != request.ExpectedPrevStateHash {
			return nativeCustodyTxSignResponse{}, errors.New("custody challenge signing request references stale state")
		}
		if request.TransitionHash == "" || challengeBundleRequestHash(request.Transition) != request.TransitionHash {
			return nativeCustodyTxSignResponse{}, errors.New("custody challenge signing request transition hash mismatch")
		}
		if err := runtime.validateCustodyTransitionSemanticsWithOptions(*table, request.Transition, request.Authorizer, true); err != nil {
			return nativeCustodyTxSignResponse{}, err
		}
		optionID := ""
		expectedTransition := request.Transition
		switch request.Purpose {
		case "challenge-option":
			if request.Authorizer.ActionRequest == nil {
				return nativeCustodyTxSignResponse{}, errors.New("custody challenge option signing request is missing its action authorizer")
			}
			optionID = request.Authorizer.ActionRequest.OptionID
		}
		menuCtx := &NativePendingTurnMenu{
			ActionDeadlineAt: strings.TrimSpace(request.Authorizer.TurnDeadlineAt),
		}
		if menuCtx.ActionDeadlineAt == "" {
			return nativeCustodyTxSignResponse{}, errors.New("custody challenge signing request is missing its turn deadline")
		}
		if err := runtime.validateTurnChallengeResolutionSigningRequest(*table, menuCtx, *request.Authorizer.ChallengeOpenBundle, expectedTransition, optionID, packet, request.ExpectedOutputs); err != nil {
			return nativeCustodyTxSignResponse{}, err
		}
		challengeRef, err := runtime.validateTurnChallengeOpenBundle(*table, menuCtx, *request.Authorizer.ChallengeOpenBundle)
		if err != nil {
			return nativeCustodyTxSignResponse{}, err
		}
		spendPath, err := runtime.selectCustodySpendPath(*table, challengeRef, activeTurnChallengePlayerIDs(*table), false)
		if err != nil {
			return nativeCustodyTxSignResponse{}, err
		}
		if !containsPlayerID(spendPath.PlayerIDs, request.PlayerID) {
			return nativeCustodyTxSignResponse{}, errors.New("custody challenge signing request is not authorized for this player")
		}
	default:
		return nativeCustodyTxSignResponse{}, fmt.Errorf("unsupported custody signing purpose %q", request.Purpose)
	}
	signedPSBT, err := runtime.signLocalCustodyPSBT(request.PSBT)
	if err != nil {
		return nativeCustodyTxSignResponse{}, err
	}
	return nativeCustodyTxSignResponse{SignedPSBT: signedPSBT}, nil
}

func (runtime *meshRuntime) handleCustodySignerPrepareFromPeer(request nativeCustodySignerPrepareRequest) (nativeCustodySignerPrepareResponse, error) {
	table, err := runtime.requireLocalTable(request.TableID)
	if err != nil {
		return nativeCustodySignerPrepareResponse{}, err
	}
	if err := validateSettlementRequestProtocolVersion(request.ProtocolVersion); err != nil {
		return nativeCustodySignerPrepareResponse{}, err
	}
	if request.PlayerID != runtime.walletID.PlayerID {
		return nativeCustodySignerPrepareResponse{}, errors.New("custody signer prepare request is not addressed to this player")
	}
	if err := runtime.validatePrebuiltCustodySigningTransition(*table, request.ExpectedPrevStateHash, request.TransitionHash, request.Transition, request.Authorizer); err != nil {
		if request.Transition.Kind != tablecustody.TransitionKindBuyInLock {
			return nativeCustodySignerPrepareResponse{}, err
		}
		if fallbackErr := runtime.validateBuyInLockSignerPrepareTransition(*table, request.ExpectedPrevStateHash, request.TransitionHash, request.Transition, request.Authorizer, request.ExpectedOffchainOutputs); fallbackErr != nil {
			return nativeCustodySignerPrepareResponse{}, fallbackErr
		}
	}
	if !containsPlayerID(runtime.custodyTreeSignerIDs(*table, request.Transition), request.PlayerID) {
		return nativeCustodySignerPrepareResponse{}, errors.New("custody signer prepare request is not authorized for this player")
	}
	offchainOutputs := offchainCustodyBatchOutputs(request.ExpectedOffchainOutputs)
	if len(offchainOutputs) == 0 {
		offchainOutputs = offchainCustodyBatchOutputs(custodyTransitionBatchOutputs(table.LatestCustodyState, request.Transition))
	}
	if len(offchainOutputs) == 0 {
		return nativeCustodySignerPrepareResponse{}, errors.New("custody signer prepare request does not authorize any offchain outputs")
	}
	session, err := runtime.walletRuntime.NewCustodySignerSession(runtime.profileName, request.DerivationPath)
	if err != nil {
		return nativeCustodySignerPrepareResponse{}, err
	}
	key := custodySignerSessionKey(request.TableID, request.TransitionHash, request.PlayerID, request.DerivationPath)
	runtime.storeCustodySignerSession(key, session)
	runtime.storeCustodySignerAuthorization(key, custodySignerAuthorization{
		ExpectedOffchainOutputs: offchainOutputs,
		ExpectedPrevStateHash:   request.ExpectedPrevStateHash,
		TransitionHash:          request.TransitionHash,
	})
	debugMeshf("custody signer prepare accepted table=%s player=%s transition=%s offchain_outputs=%d", request.TableID, request.PlayerID, request.TransitionHash, len(offchainOutputs))
	return nativeCustodySignerPrepareResponse{SignerPubkeyHex: session.PublicKeyHex}, nil
}

func (runtime *meshRuntime) handleCustodySignerStartFromPeer(request nativeCustodySignerStartRequest) (nativeCustodyAckResponse, error) {
	key := custodySignerSessionKey(request.TableID, request.TransitionHash, request.PlayerID, request.DerivationPath)
	table, err := runtime.requireLocalTable(request.TableID)
	if err != nil {
		return nativeCustodyAckResponse{}, err
	}
	if err := validateSettlementRequestProtocolVersion(request.ProtocolVersion); err != nil {
		return nativeCustodyAckResponse{}, err
	}
	if request.PlayerID != runtime.walletID.PlayerID {
		return nativeCustodyAckResponse{}, errors.New("custody signer start request is not addressed to this player")
	}
	session, ok := runtime.loadCustodySignerSession(key)
	if !ok {
		return nativeCustodyAckResponse{}, errors.New("custody signer session is not available")
	}
	authorization, ok := runtime.loadCustodySignerAuthorization(key)
	if !ok {
		return nativeCustodyAckResponse{}, errors.New("custody signer authorization is not available")
	}
	if authorization.ExpectedPrevStateHash != "" && latestCustodyStateHash(*table) != authorization.ExpectedPrevStateHash {
		return nativeCustodyAckResponse{}, errors.New("custody signer start request references stale state")
	}
	if request.ExpectedPrevStateHash != "" && request.ExpectedPrevStateHash != authorization.ExpectedPrevStateHash {
		return nativeCustodyAckResponse{}, errors.New("custody signer start request prev state mismatch")
	}
	if authorization.TransitionHash != request.TransitionHash {
		return nativeCustodyAckResponse{}, errors.New("custody signer start request transition hash mismatch")
	}
	rootBytes, err := hex.DecodeString(request.SweepTapTreeRootHex)
	if err != nil {
		return nativeCustodyAckResponse{}, err
	}
	vtxoTree, err := arktree.NewTxTree(request.VtxoTree)
	if err != nil {
		return nativeCustodyAckResponse{}, err
	}
	commitment, err := psbt.NewFromRawBytes(strings.NewReader(request.UnsignedCommitmentTx), true)
	if err != nil {
		return nativeCustodyAckResponse{}, err
	}
	if len(commitment.UnsignedTx.TxOut) == 0 || commitment.UnsignedTx.TxOut[0].Value != request.BatchOutputAmountSats {
		return nativeCustodyAckResponse{}, errors.New("custody signer start request batch amount mismatch")
	}
	batchExpiry := arklib.RelativeLocktime{Value: request.BatchExpiryValue}
	switch request.BatchExpiryType {
	case fmt.Sprintf("%d", arklib.LocktimeTypeSecond):
		batchExpiry.Type = arklib.LocktimeTypeSecond
	default:
		batchExpiry.Type = arklib.LocktimeTypeBlock
	}
	config, err := runtime.arkCustodyConfig()
	if err != nil {
		return nativeCustodyAckResponse{}, err
	}
	forfeitPubkey, err := compressedPubkeyFromHex(config.ForfeitPubkeyHex)
	if err != nil {
		return nativeCustodyAckResponse{}, err
	}
	if err := arktree.ValidateVtxoTree(vtxoTree, commitment, forfeitPubkey, batchExpiry); err != nil {
		return nativeCustodyAckResponse{}, err
	}
	expectedSweepScript, err := (&arkscript.CSVMultisigClosure{
		MultisigClosure: arkscript.MultisigClosure{PubKeys: []*btcec.PublicKey{forfeitPubkey}},
		Locktime:        batchExpiry,
	}).Script()
	if err != nil {
		return nativeCustodyAckResponse{}, err
	}
	expectedSweepRoot := txscript.AssembleTaprootScriptTree(txscript.NewBaseTapLeaf(expectedSweepScript)).RootNode.TapHash()
	if request.SweepTapTreeRootHex != hex.EncodeToString(expectedSweepRoot.CloneBytes()) {
		return nativeCustodyAckResponse{}, errors.New("custody signer start request sweep root mismatch")
	}
	if err := validateCustodyOffchainOutputs(authorization.ExpectedOffchainOutputs, offchainVtxoTreeOutputs(vtxoTree)); err != nil {
		return nativeCustodyAckResponse{}, err
	}
	if err := session.Session.Init(rootBytes, request.BatchOutputAmountSats, vtxoTree); err != nil {
		return nativeCustodyAckResponse{}, err
	}
	nonces, err := session.Session.GetNonces()
	if err != nil {
		return nativeCustodyAckResponse{}, err
	}
	client, err := runtime.newArkTransportClient()
	if err != nil {
		return nativeCustodyAckResponse{}, err
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := client.SubmitTreeNonces(ctx, request.BatchID, session.Session.GetPublicKey(), nonces); err != nil {
		return nativeCustodyAckResponse{}, err
	}
	debugMeshf("custody signer start submitted nonces table=%s player=%s transition=%s batch=%s tx_count=%d", request.TableID, request.PlayerID, request.TransitionHash, request.BatchID, len(nonces))
	return nativeCustodyAckResponse{OK: true}, nil
}

func (runtime *meshRuntime) handleCustodySignerNoncesFromPeer(request nativeCustodySignerNoncesRequest) (nativeCustodyAckResponse, error) {
	key := custodySignerSessionKey(request.TableID, request.TransitionHash, request.PlayerID, request.DerivationPath)
	table, err := runtime.requireLocalTable(request.TableID)
	if err != nil {
		return nativeCustodyAckResponse{}, err
	}
	if err := validateSettlementRequestProtocolVersion(request.ProtocolVersion); err != nil {
		return nativeCustodyAckResponse{}, err
	}
	authorization, ok := runtime.loadCustodySignerAuthorization(key)
	if !ok {
		return nativeCustodyAckResponse{}, errors.New("custody signer authorization is not available")
	}
	if authorization.ExpectedPrevStateHash != "" && latestCustodyStateHash(*table) != authorization.ExpectedPrevStateHash {
		return nativeCustodyAckResponse{}, errors.New("custody signer nonce request references stale state")
	}
	session, ok := runtime.loadCustodySignerSession(key)
	if !ok {
		return nativeCustodyAckResponse{}, errors.New("custody signer session is not available")
	}
	hasAllNonces, err := session.Session.AggregateNonces(request.TxID, request.Nonces)
	if err != nil {
		return nativeCustodyAckResponse{}, err
	}
	debugMeshf("custody signer nonces received table=%s player=%s transition=%s batch=%s txid=%s complete=%t nonce_count=%d", request.TableID, request.PlayerID, request.TransitionHash, request.BatchID, request.TxID, hasAllNonces, len(request.Nonces))
	if !hasAllNonces {
		return nativeCustodyAckResponse{OK: true, Signed: false}, nil
	}
	signatures, err := session.Session.Sign()
	if err != nil {
		return nativeCustodyAckResponse{}, err
	}
	client, err := runtime.newArkTransportClient()
	if err != nil {
		return nativeCustodyAckResponse{}, err
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := client.SubmitTreeSignatures(ctx, request.BatchID, session.Session.GetPublicKey(), signatures); err != nil {
		return nativeCustodyAckResponse{}, err
	}
	debugMeshf("custody signer nonces submitted signatures table=%s player=%s transition=%s batch=%s sig_count=%d", request.TableID, request.PlayerID, request.TransitionHash, request.BatchID, len(signatures))
	runtime.deleteCustodySignerSession(key)
	runtime.deleteCustodySignerAuthorization(key)
	return nativeCustodyAckResponse{OK: true, Signed: true}, nil
}

func (runtime *meshRuntime) handleCustodySignerAggregatedNoncesFromPeer(request nativeCustodySignerAggregatedNoncesRequest) (nativeCustodyAckResponse, error) {
	key := custodySignerSessionKey(request.TableID, request.TransitionHash, request.PlayerID, request.DerivationPath)
	table, err := runtime.requireLocalTable(request.TableID)
	if err != nil {
		return nativeCustodyAckResponse{}, err
	}
	if err := validateSettlementRequestProtocolVersion(request.ProtocolVersion); err != nil {
		return nativeCustodyAckResponse{}, err
	}
	if request.PlayerID != runtime.walletID.PlayerID {
		return nativeCustodyAckResponse{}, errors.New("custody signer aggregated nonces request is not addressed to this player")
	}
	authorization, ok := runtime.loadCustodySignerAuthorization(key)
	if !ok {
		return nativeCustodyAckResponse{}, errors.New("custody signer authorization is not available")
	}
	if authorization.ExpectedPrevStateHash != "" && latestCustodyStateHash(*table) != authorization.ExpectedPrevStateHash {
		return nativeCustodyAckResponse{}, errors.New("custody signer aggregated nonces request references stale state")
	}
	session, ok := runtime.loadCustodySignerSession(key)
	if !ok {
		return nativeCustodyAckResponse{}, errors.New("custody signer session is not available")
	}
	session.Session.SetAggregatedNonces(request.Nonces)
	debugMeshf("custody signer aggregated nonces received table=%s player=%s transition=%s batch=%s tx_count=%d", request.TableID, request.PlayerID, request.TransitionHash, request.BatchID, len(request.Nonces))
	signatures, err := session.Session.Sign()
	if err != nil {
		return nativeCustodyAckResponse{}, err
	}
	client, err := runtime.newArkTransportClient()
	if err != nil {
		return nativeCustodyAckResponse{}, err
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := client.SubmitTreeSignatures(ctx, request.BatchID, session.Session.GetPublicKey(), signatures); err != nil {
		return nativeCustodyAckResponse{}, err
	}
	debugMeshf("custody signer aggregated nonces submitted signatures table=%s player=%s transition=%s batch=%s sig_count=%d", request.TableID, request.PlayerID, request.TransitionHash, request.BatchID, len(signatures))
	runtime.deleteCustodySignerSession(key)
	runtime.deleteCustodySignerAuthorization(key)
	return nativeCustodyAckResponse{OK: true, Signed: true}, nil
}

type custodyBatchEventsHandler struct {
	runtime            *meshRuntime
	table              nativeTableState
	prevStateHash      string
	requestKey         string
	transition         tablecustody.CustodyTransition
	authorizer         *nativeTransitionAuthorizer
	plan               *custodySettlementPlan
	transport          arkclient.TransportClient
	arkConfig          walletpkg.CustodyArkConfig
	intentID           string
	derivationPath     string
	signerSessions     map[string]walletpkg.CustodySignerSession
	signerPubkeys      map[string]string
	outputCount        int
	batchSessionID     string
	batchExpiry        arklib.RelativeLocktime
	commitmentTx       string
	finalVtxoTree      *arktree.TxTree
	finalConnectorTree *arktree.TxTree
	intentRegisteredAt time.Time
	treeSigningAt      time.Time
}

func (handler *custodyBatchEventsHandler) OnBatchStarted(ctx context.Context, event arkclient.BatchStartedEvent) (bool, error) {
	sum := sha256.Sum256([]byte(handler.intentID))
	hashedIntentID := hex.EncodeToString(sum[:])
	for _, value := range event.HashedIntentIds {
		if value != hashedIntentID {
			continue
		}
		if err := handler.transport.ConfirmRegistration(ctx, handler.intentID); err != nil {
			return false, err
		}
		handler.batchSessionID = event.Id
		handler.batchExpiry = custodyBatchExpiry(uint32(event.BatchExpiry))
		return false, nil
	}
	return true, nil
}

func (handler *custodyBatchEventsHandler) OnBatchFinalized(context.Context, arkclient.BatchFinalizedEvent) error {
	return nil
}

func (handler *custodyBatchEventsHandler) OnBatchFailed(_ context.Context, event arkclient.BatchFailedEvent) error {
	return fmt.Errorf("ark batch failed: %s", event.Reason)
}

func (handler *custodyBatchEventsHandler) OnTreeTxEvent(context.Context, arkclient.TreeTxEvent) error {
	return nil
}

func (handler *custodyBatchEventsHandler) OnTreeSignatureEvent(context.Context, arkclient.TreeSignatureEvent) error {
	return nil
}

func (handler *custodyBatchEventsHandler) OnTreeSigningStarted(ctx context.Context, event arkclient.TreeSigningStartedEvent, vtxoTree *arktree.TxTree) (bool, error) {
	requiredPubkeys := map[string]string{}
	for playerID, pubkeyHex := range handler.signerPubkeys {
		requiredPubkeys[pubkeyHex] = playerID
	}
	found := 0
	for _, pubkeyHex := range event.CosignersPubkeys {
		if _, ok := requiredPubkeys[pubkeyHex]; ok {
			found++
		}
	}
	if found == 0 {
		return true, nil
	}
	if !handler.intentRegisteredAt.IsZero() {
		emitMeshTiming(meshTimingFields{
			Metric:         "custody_tree_signing_wait",
			TableID:        handler.table.Config.TableID,
			CustodySeq:     handler.transition.CustodySeq,
			TransitionKind: string(handler.transition.Kind),
			Phase:          tablePhaseForTiming(handler.table),
			RequestHash:    handler.requestKey,
			InputCount:     len(handler.plan.Inputs),
			OutputCount:    handler.outputCount,
		}, time.Since(handler.intentRegisteredAt), nil)
	}
	if found != len(requiredPubkeys) {
		return false, errors.New("not all custody signer pubkeys were included in tree signing")
	}
	handler.treeSigningAt = time.Now()
	debugMeshf("custody tree signing started table=%s transition=%s batch=%s expected_signers=%d found_signers=%d", handler.table.Config.TableID, handler.requestKey, event.Id, len(requiredPubkeys), found)

	operatorPubkey, err := compressedPubkeyFromHex(handler.arkConfig.ForfeitPubkeyHex)
	if err != nil {
		return false, err
	}
	sweepScript, err := (&arkscript.CSVMultisigClosure{
		MultisigClosure: arkscript.MultisigClosure{PubKeys: []*btcec.PublicKey{operatorPubkey}},
		Locktime:        handler.batchExpiry,
	}).Script()
	if err != nil {
		return false, err
	}
	sweepRoot := txscript.AssembleTaprootScriptTree(txscript.NewBaseTapLeaf(sweepScript)).RootNode.TapHash()

	commitment, err := psbt.NewFromRawBytes(strings.NewReader(event.UnsignedCommitmentTx), true)
	if err != nil {
		return false, err
	}
	batchOutputAmount := commitment.UnsignedTx.TxOut[0].Value
	for playerID := range handler.signerPubkeys {
		if playerID == handler.runtime.walletID.PlayerID {
			session := handler.signerSessions[playerID]
			if err := session.Session.Init(sweepRoot.CloneBytes(), batchOutputAmount, vtxoTree); err != nil {
				return false, err
			}
			nonces, err := session.Session.GetNonces()
			if err != nil {
				return false, err
			}
			if err := handler.transport.SubmitTreeNonces(ctx, event.Id, session.Session.GetPublicKey(), nonces); err != nil {
				return false, err
			}
			debugMeshf("custody tree signing local nonces table=%s transition=%s batch=%s player=%s tx_count=%d", handler.table.Config.TableID, handler.requestKey, event.Id, playerID, len(nonces))
			continue
		}
		seat, ok := seatRecordForPlayer(handler.table, playerID)
		if !ok || seat.PeerURL == "" {
			return false, fmt.Errorf("missing peer url for custody signer %s", playerID)
		}
		if err := handler.runtime.remoteStartCustodySigner(seat.PeerURL, nativeCustodySignerStartRequest{
			BatchID:               event.Id,
			BatchExpiryType:       fmt.Sprintf("%d", handler.batchExpiry.Type),
			BatchExpiryValue:      handler.batchExpiry.Value,
			BatchOutputAmountSats: batchOutputAmount,
			DerivationPath:        handler.derivationPath,
			ExpectedPrevStateHash: handler.prevStateHash,
			PlayerID:              playerID,
			ProtocolVersion:       nativeProtocolVersion,
			SweepTapTreeRootHex:   hex.EncodeToString(sweepRoot.CloneBytes()),
			TableID:               handler.table.Config.TableID,
			TransitionHash:        handler.requestKey,
			UnsignedCommitmentTx:  event.UnsignedCommitmentTx,
			VtxoTree:              mustSerializeTxTree(vtxoTree),
		}); err != nil {
			return false, err
		}
		debugMeshf("custody tree signing remote start table=%s transition=%s batch=%s player=%s", handler.table.Config.TableID, handler.requestKey, event.Id, playerID)
	}
	return false, nil
}

func (handler *custodyBatchEventsHandler) OnTreeNonces(ctx context.Context, event arkclient.TreeNoncesEvent) (bool, error) {
	debugMeshf("custody tree nonces event table=%s transition=%s batch=%s txid=%s nonce_count=%d signer_count=%d", handler.table.Config.TableID, handler.requestKey, event.Id, event.Txid, len(event.Nonces), len(handler.signerPubkeys))
	signedCount := 0
	for playerID := range handler.signerPubkeys {
		if playerID == handler.runtime.walletID.PlayerID {
			session := handler.signerSessions[playerID]
			complete, err := session.Session.AggregateNonces(event.Txid, event.Nonces)
			if err != nil {
				return false, err
			}
			if !complete {
				continue
			}
			signatures, err := session.Session.Sign()
			if err != nil {
				return false, err
			}
			if err := handler.transport.SubmitTreeSignatures(ctx, event.Id, session.Session.GetPublicKey(), signatures); err != nil {
				return false, err
			}
			debugMeshf("custody tree nonces local signatures table=%s transition=%s batch=%s player=%s sig_count=%d", handler.table.Config.TableID, handler.requestKey, event.Id, playerID, len(signatures))
			signedCount++
			continue
		}
		seat, ok := seatRecordForPlayer(handler.table, playerID)
		if !ok || seat.PeerURL == "" {
			return false, fmt.Errorf("missing peer url for custody signer %s", playerID)
		}
		signed, err := handler.runtime.remoteAdvanceCustodySignerNonces(seat.PeerURL, nativeCustodySignerNoncesRequest{
			BatchID:         event.Id,
			DerivationPath:  handler.derivationPath,
			Nonces:          event.Nonces,
			PlayerID:        playerID,
			ProtocolVersion: nativeProtocolVersion,
			TableID:         handler.table.Config.TableID,
			TxID:            event.Txid,
			TransitionHash:  handler.requestKey,
		})
		if err != nil {
			return false, err
		}
		debugMeshf("custody tree nonces remote forward table=%s transition=%s batch=%s player=%s txid=%s signed=%t", handler.table.Config.TableID, handler.requestKey, event.Id, playerID, event.Txid, signed)
		if signed {
			signedCount++
		}
	}
	return signedCount == len(handler.signerPubkeys), nil
}

func (handler *custodyBatchEventsHandler) OnTreeNoncesAggregated(ctx context.Context, event arkclient.TreeNoncesAggregatedEvent) (bool, error) {
	debugMeshf("custody tree aggregated nonces event table=%s transition=%s batch=%s tx_count=%d signer_count=%d", handler.table.Config.TableID, handler.requestKey, event.Id, len(event.Nonces), len(handler.signerPubkeys))
	signedCount := 0
	for playerID := range handler.signerPubkeys {
		if playerID == handler.runtime.walletID.PlayerID {
			session := handler.signerSessions[playerID]
			session.Session.SetAggregatedNonces(event.Nonces)
			signatures, err := session.Session.Sign()
			if err != nil {
				return false, err
			}
			if err := handler.transport.SubmitTreeSignatures(ctx, event.Id, session.Session.GetPublicKey(), signatures); err != nil {
				return false, err
			}
			debugMeshf("custody tree aggregated local signatures table=%s transition=%s batch=%s player=%s sig_count=%d", handler.table.Config.TableID, handler.requestKey, event.Id, playerID, len(signatures))
			signedCount++
			continue
		}
		seat, ok := seatRecordForPlayer(handler.table, playerID)
		if !ok || seat.PeerURL == "" {
			return false, fmt.Errorf("missing peer url for custody signer %s", playerID)
		}
		signed, err := handler.runtime.remoteAdvanceCustodySignerAggregatedNonces(seat.PeerURL, nativeCustodySignerAggregatedNoncesRequest{
			BatchID:         event.Id,
			DerivationPath:  handler.derivationPath,
			Nonces:          event.Nonces,
			PlayerID:        playerID,
			ProtocolVersion: nativeProtocolVersion,
			TableID:         handler.table.Config.TableID,
			TransitionHash:  handler.requestKey,
		})
		if err != nil {
			return false, err
		}
		debugMeshf("custody tree aggregated remote forward table=%s transition=%s batch=%s player=%s tx_count=%d signed=%t", handler.table.Config.TableID, handler.requestKey, event.Id, playerID, len(event.Nonces), signed)
		if signed {
			signedCount++
		}
	}
	return signedCount == len(handler.signerPubkeys), nil
}

func (handler *custodyBatchEventsHandler) OnBatchFinalization(ctx context.Context, event arkclient.BatchFinalizationEvent, vtxoTree, connectorTree *arktree.TxTree) error {
	handler.commitmentTx = event.Tx
	if !handler.treeSigningAt.IsZero() {
		emitMeshTiming(meshTimingFields{
			Metric:         "custody_tree_nonce_signature_exchange",
			TableID:        handler.table.Config.TableID,
			CustodySeq:     handler.transition.CustodySeq,
			TransitionKind: string(handler.transition.Kind),
			Phase:          tablePhaseForTiming(handler.table),
			RequestHash:    handler.requestKey,
			InputCount:     len(handler.plan.Inputs),
			OutputCount:    handler.outputCount,
		}, time.Since(handler.treeSigningAt), nil)
	}
	debugMeshf(
		"custody batch finalization table=%s transition=%s batch=%s input_count=%d has_vtxo_tree=%t has_connector_tree=%t",
		handler.table.Config.TableID,
		handler.requestKey,
		event.Id,
		len(handler.plan.Inputs),
		vtxoTree != nil,
		connectorTree != nil,
	)
	if vtxoTree != nil {
		commitment, err := psbt.NewFromRawBytes(strings.NewReader(event.Tx), true)
		if err != nil {
			return err
		}
		forfeitPubkey, err := compressedPubkeyFromHex(handler.arkConfig.ForfeitPubkeyHex)
		if err != nil {
			return err
		}
		if err := arktree.ValidateVtxoTree(vtxoTree, commitment, forfeitPubkey, handler.batchExpiry); err != nil {
			return err
		}
	}
	handler.finalVtxoTree = vtxoTree
	handler.finalConnectorTree = connectorTree
	if len(handler.plan.Inputs) == 0 || connectorTree == nil {
		if len(handler.plan.Inputs) > 0 && connectorTree == nil {
			debugMeshf(
				"custody batch finalization missing connector tree table=%s transition=%s batch=%s input_count=%d",
				handler.table.Config.TableID,
				handler.requestKey,
				event.Id,
				len(handler.plan.Inputs),
			)
		}
		return nil
	}
	connectorLeaves := connectorTree.Leaves()
	debugMeshf(
		"custody batch finalization connectors table=%s transition=%s batch=%s connector_leaf_count=%d",
		handler.table.Config.TableID,
		handler.requestKey,
		event.Id,
		len(connectorLeaves),
	)
	if len(connectorLeaves) != len(handler.plan.Inputs) {
		return errors.New("connector tree leaf count does not match custody forfeits")
	}
	parsedForfeitAddr, err := btcutil.DecodeAddress(handler.arkConfig.ForfeitAddress, nil)
	if err != nil {
		return err
	}
	forfeitPkScript, err := txscript.PayToAddrScript(parsedForfeitAddr)
	if err != nil {
		return err
	}
	forfeitSubmitStartedAt := time.Now()
	signedForfeits := make([]string, 0, len(handler.plan.Inputs))
	for index, input := range handler.plan.Inputs {
		debugMeshf(
			"custody batch forfeit build table=%s transition=%s batch=%s input=%d ref=%s player_ids=%v",
			handler.table.Config.TableID,
			handler.requestKey,
			event.Id,
			index,
			fundingRefKey(input.Ref),
			input.SpendPath.PlayerIDs,
		)
		forfeit, err := handler.createSignedForfeit(ctx, input, connectorLeaves[index])
		if err != nil {
			return err
		}
		signedForfeits = append(signedForfeits, forfeit)
	}
	if err := validateSignedCustodyForfeits(handler.plan, connectorLeaves, signedForfeits, forfeitPkScript); err != nil {
		return err
	}
	debugMeshf(
		"custody batch forfeits submit table=%s transition=%s batch=%s signed_forfeit_count=%d",
		handler.table.Config.TableID,
		handler.requestKey,
		event.Id,
		len(signedForfeits),
	)
	err = handler.transport.SubmitSignedForfeitTxs(ctx, signedForfeits, "")
	emitMeshTiming(meshTimingFields{
		Metric:         "custody_connector_forfeit_submit",
		TableID:        handler.table.Config.TableID,
		CustodySeq:     handler.transition.CustodySeq,
		TransitionKind: string(handler.transition.Kind),
		Phase:          tablePhaseForTiming(handler.table),
		RequestHash:    handler.requestKey,
		InputCount:     len(handler.plan.Inputs),
		OutputCount:    handler.outputCount,
	}, time.Since(forfeitSubmitStartedAt), err)
	return err
}

func (handler *custodyBatchEventsHandler) createSignedForfeit(ctx context.Context, input custodyInputSpec, connectorLeaf *psbt.Packet) (string, error) {
	parsedForfeitAddr, err := btcutil.DecodeAddress(handler.arkConfig.ForfeitAddress, nil)
	if err != nil {
		return "", err
	}
	forfeitPkScript, err := txscript.PayToAddrScript(parsedForfeitAddr)
	if err != nil {
		return "", err
	}
	connector, connectorOutpoint, err := connectorOutputFromLeaf(connectorLeaf)
	if err != nil {
		return "", err
	}
	vtxoHash, err := chainhash.NewHashFromStr(input.Ref.TxID)
	if err != nil {
		return "", err
	}
	vtxoSequence := uint32(wire.MaxTxInSequenceNum)
	vtxoLocktime := uint32(0)
	if input.SpendPath.UsesCLTVLocktime {
		vtxoSequence = wire.MaxTxInSequenceNum - 1
		vtxoLocktime = uint32(input.SpendPath.Locktime)
	}
	forfeitTx, err := arktree.BuildForfeitTx(
		[]*wire.OutPoint{{Hash: *vtxoHash, Index: input.Ref.VOut}, connectorOutpoint},
		[]uint32{vtxoSequence, wire.MaxTxInSequenceNum},
		[]*wire.TxOut{{Value: int64(input.Ref.AmountSats), PkScript: input.SpendPath.PKScript}, connector},
		forfeitPkScript,
		vtxoLocktime,
	)
	if err != nil {
		return "", err
	}
	forfeitTx.Inputs[0].TaprootLeafScript = []*psbt.TaprootTapLeafScript{{
		ControlBlock: input.SpendPath.LeafProof.ControlBlock,
		Script:       input.SpendPath.Script,
		LeafVersion:  txscript.BaseLeafVersion,
	}}
	unsigned, err := forfeitTx.B64Encode()
	if err != nil {
		return "", err
	}
	return handler.runtime.fullySignCustodyPSBT(handler.table, handler.prevStateHash, handler.requestKey, "forfeit", input.SpendPath.PlayerIDs, unsigned, handler.transition, handler.authorizer, nil)
}

func mustSerializeTxTree(value *arktree.TxTree) arktree.FlatTxTree {
	if value == nil {
		return nil
	}
	serialized, err := value.Serialize()
	if err != nil {
		panic(err)
	}
	return serialized
}

func cloneFlatTxTree(tree arktree.FlatTxTree) arktree.FlatTxTree {
	if len(tree) == 0 {
		return nil
	}
	cloned := append(arktree.FlatTxTree(nil), tree...)
	for index := range cloned {
		if cloned[index].Children == nil {
			continue
		}
		children := make(map[uint32]string, len(cloned[index].Children))
		for outputIndex, txID := range cloned[index].Children {
			children[outputIndex] = txID
		}
		cloned[index].Children = children
	}
	return cloned
}

func custodySettlementWitnessFromResult(result *custodyBatchResult) *tablecustody.CustodySettlementWitness {
	if result == nil {
		return nil
	}
	return &tablecustody.CustodySettlementWitness{
		ArkIntentID:      result.IntentID,
		ArkTxID:          result.ArkTxID,
		FinalizedAt:      result.FinalizedAt,
		ProofPSBT:        result.ProofPSBT,
		CommitmentTx:     result.CommitmentTx,
		BatchExpiryType:  result.BatchExpiryType,
		BatchExpiryValue: result.BatchExpiryValue,
		VtxoTree:         cloneFlatTxTree(result.VtxoTree),
		ConnectorTree:    cloneFlatTxTree(result.ConnectorTree),
	}
}

func custodySignerDerivationPath(transitionHash string) string {
	sum := sha256.Sum256([]byte(transitionHash))
	return "parker/custody/" + hex.EncodeToString(sum[:])
}

func custodyBatchOutputFromSpec(spec custodyOutputSpec) custodyBatchOutput {
	return custodyBatchOutput{
		AmountSats:    spec.AmountSats,
		ClaimKey:      spec.ClaimKey,
		OwnerPlayerID: spec.OwnerPlayerID,
		Script:        spec.Script,
		Tapscripts:    append([]string(nil), spec.Tapscripts...),
	}
}

func custodyBatchOutputFromReceiver(claimKey, ownerPlayerID string, receiver sdktypes.Receiver, tapscripts []string) (custodyBatchOutput, error) {
	txOut, onchain, err := receiver.ToTxOut()
	if err != nil {
		return custodyBatchOutput{}, err
	}
	return custodyBatchOutput{
		AmountSats:    int(receiver.Amount),
		ClaimKey:      claimKey,
		Onchain:       onchain,
		OwnerPlayerID: ownerPlayerID,
		Script:        hex.EncodeToString(txOut.PkScript),
		Tapscripts:    append([]string(nil), tapscripts...),
	}, nil
}

func decodeBatchOutputTxOut(output custodyBatchOutput) (*wire.TxOut, error) {
	scriptBytes, err := hex.DecodeString(output.Script)
	if err != nil {
		return nil, err
	}
	return &wire.TxOut{
		Value:    int64(output.AmountSats),
		PkScript: scriptBytes,
	}, nil
}

func custodyOnchainOutputIndexes(outputs []custodyBatchOutput) []int {
	indexes := make([]int, 0)
	for index, output := range outputs {
		if output.Onchain {
			indexes = append(indexes, index)
		}
	}
	return indexes
}

func custodyOutputsRequireTreeSigning(outputs []custodyBatchOutput) bool {
	for _, output := range outputs {
		if !output.Onchain {
			return true
		}
	}
	return false
}

func (runtime *meshRuntime) prepareCustodyBatchSigners(table nativeTableState, prevStateHash, transitionHash string, transition tablecustody.CustodyTransition, authorizer *nativeTransitionAuthorizer, signerIDs []string, outputs []custodyBatchOutput) (map[string]walletpkg.CustodySignerSession, map[string]string, string, error) {
	derivationPath := custodySignerDerivationPath(transitionHash)
	sessions := map[string]walletpkg.CustodySignerSession{}
	pubkeys := map[string]string{}
	offchainOutputs := offchainCustodyBatchOutputs(outputs)
	for _, playerID := range uniqueSortedPlayerIDs(signerIDs) {
		if strings.TrimSpace(playerID) == "" {
			continue
		}
		if playerID == runtime.walletID.PlayerID {
			session, err := runtime.walletRuntime.NewCustodySignerSession(runtime.profileName, derivationPath)
			if err != nil {
				return nil, nil, "", err
			}
			sessions[playerID] = session
			pubkeys[playerID] = session.PublicKeyHex
			continue
		}
		seat, ok := seatRecordForPlayer(table, playerID)
		if !ok {
			return nil, nil, "", fmt.Errorf("missing seat for custody signer %s", playerID)
		}
		peerURL := firstNonEmptyString(seat.PeerURL, runtime.knownPeerURL(seat.PeerID))
		if peerURL == "" {
			return nil, nil, "", fmt.Errorf("missing peer url for custody signer %s", playerID)
		}
		response, err := runtime.remotePrepareCustodySigner(peerURL, nativeCustodySignerPrepareRequest{
			DerivationPath:          derivationPath,
			ExpectedPrevStateHash:   prevStateHash,
			ExpectedOffchainOutputs: append([]custodyBatchOutput(nil), offchainOutputs...),
			Authorizer:              cloneTransitionAuthorizer(authorizer),
			PlayerID:                playerID,
			ProtocolVersion:         nativeProtocolVersion,
			TableID:                 table.Config.TableID,
			Transition:              transition,
			TransitionHash:          transitionHash,
		})
		if err != nil {
			return nil, nil, "", fmt.Errorf("remote custody signer prepare for %s: %w", playerID, err)
		}
		pubkeys[playerID] = response.SignerPubkeyHex
		debugMeshf("custody batch remote prepare table=%s transition=%s player=%s pubkey=%s", table.Config.TableID, transitionHash, playerID, response.SignerPubkeyHex)
	}
	return sessions, pubkeys, derivationPath, nil
}

func sortedSignerPubkeys(pubkeys map[string]string) []string {
	values := make([]string, 0, len(pubkeys))
	for _, pubkeyHex := range pubkeys {
		if strings.TrimSpace(pubkeyHex) == "" {
			continue
		}
		values = append(values, pubkeyHex)
	}
	sort.Strings(values)
	return values
}

func custodyBatchTopics(inputs []custodyInputSpec, signerPubkeys map[string]string) []string {
	topics := make([]string, 0, len(inputs)+len(signerPubkeys))
	seen := map[string]struct{}{}
	for _, input := range inputs {
		topic := fundingRefKey(input.Ref)
		if _, ok := seen[topic]; ok {
			continue
		}
		seen[topic] = struct{}{}
		topics = append(topics, topic)
	}
	pubkeys := sortedSignerPubkeys(signerPubkeys)
	for _, pubkeyHex := range pubkeys {
		if _, ok := seen[pubkeyHex]; ok {
			continue
		}
		seen[pubkeyHex] = struct{}{}
		topics = append(topics, pubkeyHex)
	}
	return topics
}

func custodyRefExpiryISO(finalizedAt string, expiry arklib.RelativeLocktime) string {
	if finalizedAt == "" || expiry.Type != arklib.LocktimeTypeSecond || expiry.Value == 0 {
		return ""
	}
	timestamp, err := parseISOTimestamp(finalizedAt)
	if err != nil {
		return ""
	}
	return timestamp.Add(time.Duration(expiry.Value) * time.Second).UTC().Format(time.RFC3339)
}

func matchCustodyBatchOutputRefs(intentID, arkTxID, finalizedAt string, expiry arklib.RelativeLocktime, outputs []custodyBatchOutput, vtxoTree *arktree.TxTree) (map[string][]tablecustody.VTXORef, error) {
	matched := map[string][]tablecustody.VTXORef{}
	offchainOutputs := 0
	for _, output := range outputs {
		if !output.Onchain {
			offchainOutputs++
		}
	}
	if offchainOutputs == 0 {
		return matched, nil
	}
	if vtxoTree == nil {
		return nil, errors.New("ark batch did not return a vtxo tree for offchain outputs")
	}
	available := make([]tablecustody.VTXORef, 0)
	for _, leaf := range vtxoTree.Leaves() {
		for outputIndex, txOut := range leaf.UnsignedTx.TxOut {
			if bytes.Equal(txOut.PkScript, arktxutils.ANCHOR_PKSCRIPT) {
				continue
			}
			scriptHex := hex.EncodeToString(txOut.PkScript)
			available = append(available, tablecustody.VTXORef{
				AmountSats:  int(txOut.Value),
				ArkIntentID: intentID,
				ArkTxID:     arkTxID,
				ExpiresAt:   custodyRefExpiryISO(finalizedAt, expiry),
				Script:      scriptHex,
				TxID:        leaf.UnsignedTx.TxID(),
				VOut:        uint32(outputIndex),
			})
		}
	}
	used := make([]bool, len(available))
	for _, output := range outputs {
		if output.Onchain {
			continue
		}
		matchIndex := -1
		for index, ref := range available {
			if used[index] {
				continue
			}
			if ref.AmountSats != output.AmountSats || ref.Script != output.Script {
				continue
			}
			matchIndex = index
			break
		}
		if matchIndex < 0 {
			return nil, fmt.Errorf("ark batch output %s could not be matched in finalized vtxo tree", output.ClaimKey)
		}
		used[matchIndex] = true
		ref := available[matchIndex]
		ref.OwnerPlayerID = output.OwnerPlayerID
		ref.Tapscripts = append([]string(nil), output.Tapscripts...)
		matched[output.ClaimKey] = append(matched[output.ClaimKey], ref)
	}
	return matched, nil
}

func (runtime *meshRuntime) executeCustodyBatch(table nativeTableState, prevStateHash, transitionHash string, transition tablecustody.CustodyTransition, authorizer *nativeTransitionAuthorizer, inputs []custodyInputSpec, proofSignerIDs, treeSignerIDs []string, outputs []custodyBatchOutput) (*custodyBatchResult, error) {
	if runtime.custodyBatchExecute != nil {
		return runtime.custodyBatchExecute(table, prevStateHash, transitionHash, inputs, proofSignerIDs, treeSignerIDs, outputs)
	}
	if runtime.config.UseMockSettlement {
		return mockCustodyBatchResult(transitionHash, outputs), nil
	}
	if len(inputs) == 0 {
		return nil, errors.New("custody batch is missing inputs")
	}
	intentInputs, leafProofs, arkFields, locktime, err := custodyIntentInputs(inputs)
	if err != nil {
		return nil, err
	}
	txOutputs := make([]*wire.TxOut, 0, len(outputs))
	for _, output := range outputs {
		txOut, err := decodeBatchOutputTxOut(output)
		if err != nil {
			return nil, err
		}
		txOutputs = append(txOutputs, txOut)
	}

	needsTreeSigning := custodyOutputsRequireTreeSigning(outputs)
	if needsTreeSigning && len(treeSignerIDs) == 0 {
		return nil, errors.New("custody batch is missing tree signers")
	}
	proofSignerIDs = uniqueSortedPlayerIDs(proofSignerIDs)
	if len(proofSignerIDs) == 0 {
		return nil, errors.New("custody batch is missing proof signers")
	}

	prepareStartedAt := time.Now()
	signerSessions, signerPubkeys, derivationPath, err := runtime.prepareCustodyBatchSigners(table, prevStateHash, transitionHash, transition, authorizer, treeSignerIDs, outputs)
	emitMeshTiming(meshTimingFields{
		Metric:         "custody_signer_prepare",
		TableID:        table.Config.TableID,
		CustodySeq:     transition.CustodySeq,
		TransitionKind: string(transition.Kind),
		Phase:          tablePhaseForTiming(table),
		RequestHash:    transitionHash,
		Purpose:        "tree",
		InputCount:     len(inputs),
		OutputCount:    len(outputs),
	}, time.Since(prepareStartedAt), err)
	if err != nil {
		return nil, err
	}
	cosignerPubkeys := sortedSignerPubkeys(signerPubkeys)
	message, err := custodyRegisterMessage(custodyOnchainOutputIndexes(outputs), cosignerPubkeys)
	if err != nil {
		return nil, err
	}
	unsignedProof, err := custodyBuildProofPSBT(message, intentInputs, txOutputs, leafProofs, arkFields, locktime)
	if err != nil {
		return nil, err
	}
	signedProof, err := runtime.fullySignCustodyPSBT(table, prevStateHash, transitionHash, "proof", proofSignerIDs, unsignedProof, transition, authorizer, outputs)
	if err != nil {
		return nil, err
	}

	transport, err := runtime.newArkTransportClient()
	if err != nil {
		return nil, err
	}
	defer transport.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	registerStartedAt := time.Now()
	intentID, err := transport.RegisterIntent(ctx, signedProof, message)
	emitMeshTiming(meshTimingFields{
		Metric:         "custody_intent_register",
		TableID:        table.Config.TableID,
		CustodySeq:     transition.CustodySeq,
		TransitionKind: string(transition.Kind),
		Phase:          tablePhaseForTiming(table),
		RequestHash:    transitionHash,
		InputCount:     len(inputs),
		OutputCount:    len(outputs),
	}, time.Since(registerStartedAt), err)
	if err != nil {
		return nil, err
	}
	topics := custodyBatchTopics(inputs, signerPubkeys)
	eventsCh, closeStream, err := transport.GetEventStream(ctx, topics)
	if err != nil {
		return nil, err
	}
	defer closeStream()

	arkConfig, err := runtime.arkCustodyConfig()
	if err != nil {
		return nil, err
	}
	handler := &custodyBatchEventsHandler{
		runtime:            runtime,
		table:              table,
		prevStateHash:      prevStateHash,
		requestKey:         transitionHash,
		transition:         transition,
		authorizer:         cloneTransitionAuthorizer(authorizer),
		plan:               &custodySettlementPlan{Inputs: append([]custodyInputSpec(nil), inputs...)},
		transport:          transport,
		arkConfig:          arkConfig,
		intentID:           intentID,
		derivationPath:     derivationPath,
		signerSessions:     signerSessions,
		signerPubkeys:      signerPubkeys,
		outputCount:        len(outputs),
		intentRegisteredAt: time.Now(),
	}

	options := []arksdk.BatchSessionOption{}
	if !needsTreeSigning {
		options = append(options, arksdk.WithSkipVtxoTreeSigning())
	}
	arkTxID, err := arksdk.JoinBatchSession(ctx, eventsCh, handler, options...)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(handler.commitmentTx) == "" {
		return nil, errors.New("ark batch did not return a finalized commitment transaction")
	}
	commitment, err := psbt.NewFromRawBytes(strings.NewReader(handler.commitmentTx), true)
	if err != nil {
		return nil, err
	}
	if commitment.UnsignedTx.TxID() != arkTxID {
		return nil, errors.New("ark batch finalized txid does not match the returned commitment transaction")
	}
	finalizedAt := nowISO()
	outputRefs, err := matchCustodyBatchOutputRefs(intentID, arkTxID, finalizedAt, handler.batchExpiry, outputs, handler.finalVtxoTree)
	if err != nil {
		return nil, err
	}
	return &custodyBatchResult{
		ArkTxID:          arkTxID,
		BatchExpiryType:  custodyBatchExpiryType(handler.batchExpiry),
		BatchExpiryValue: handler.batchExpiry.Value,
		CommitmentTx:     handler.commitmentTx,
		ConnectorTree:    mustSerializeTxTree(handler.finalConnectorTree),
		FinalizedAt:      finalizedAt,
		IntentID:         intentID,
		OutputRefs:       outputRefs,
		ProofPSBT:        signedProof,
		VtxoTree:         mustSerializeTxTree(handler.finalVtxoTree),
	}, nil
}

func applyTransitionSettlementPlan(transition *tablecustody.CustodyTransition, plan *custodySettlementPlan, outputRefs map[string][]tablecustody.VTXORef) {
	if transition == nil {
		return
	}
	changed := map[string]struct{}{}
	if plan != nil {
		for _, input := range plan.Inputs {
			changed[input.ClaimKey] = struct{}{}
		}
		for _, output := range plan.Outputs {
			changed[output.ClaimKey] = struct{}{}
		}
	}
	for index := range transition.NextState.StackClaims {
		key := stackClaimKey(transition.NextState.StackClaims[index].PlayerID)
		if refs, ok := outputRefs[key]; ok {
			transition.NextState.StackClaims[index].VTXORefs = append([]tablecustody.VTXORef(nil), refs...)
			continue
		}
		if _, ok := changed[key]; ok {
			transition.NextState.StackClaims[index].VTXORefs = nil
		}
	}
	for index := range transition.NextState.PotSlices {
		key := potClaimKey(transition.NextState.PotSlices[index].PotID)
		if refs, ok := outputRefs[key]; ok {
			transition.NextState.PotSlices[index].VTXORefs = append([]tablecustody.VTXORef(nil), refs...)
			continue
		}
		if _, ok := changed[key]; ok {
			transition.NextState.PotSlices[index].VTXORefs = nil
		}
	}
}

func stackProofRefs(state tablecustody.CustodyState) []tablecustody.VTXORef {
	refs := make([]tablecustody.VTXORef, 0)
	for _, claim := range state.StackClaims {
		refs = append(refs, claim.VTXORefs...)
	}
	return refs
}

func (runtime *meshRuntime) finalizeNonSettlementCustodyTransition(table *nativeTableState, transition *tablecustody.CustodyTransition, authorizer *nativeTransitionAuthorizer) error {
	if table == nil || transition == nil {
		return nil
	}
	approvalTransition, _, err := runtime.normalizedCustodyApprovalTransition(*table, *transition)
	if err != nil {
		return err
	}
	approvals, err := runtime.collectCustodyApprovals(*table, approvalTransition, authorizer, runtime.requiredCustodySigners(*table, approvalTransition))
	if err != nil {
		return err
	}
	transition.Approvals = approvals
	transition.NextState.StateHash = tablecustody.HashCustodyState(transition.NextState)
	transition.NextStateHash = transition.NextState.StateHash
	transition.Proof = tablecustody.CustodyProof{
		FinalizedAt:     nowISO(),
		RequestHash:     approvalTransition.Proof.RequestHash,
		ReplayValidated: true,
		Signatures:      append([]tablecustody.CustodySignature(nil), approvals...),
		StateHash:       transition.NextStateHash,
		VTXORefs:        stackProofRefs(transition.NextState),
	}
	transition.Proof.TransitionHash = tablecustody.HashCustodyTransition(*transition)
	return tablecustody.ValidateTransition(table.LatestCustodyState, *transition)
}

func (runtime *meshRuntime) finalizeRealCustodyTransition(table *nativeTableState, transition *tablecustody.CustodyTransition, authorizer *nativeTransitionAuthorizer) (err error) {
	if table == nil || transition == nil {
		return nil
	}
	timingFields := custodyTimingFields(*table, *transition, "custody_finalize_real_total")
	timing := startMeshTiming(timingFields)
	defer func() {
		timing.EndWith(timingFields, err)
	}()
	signingTransition, plan, err := runtime.normalizedCustodyApprovalTransition(*table, *transition)
	if err != nil {
		return err
	}
	timingFields.CustodySeq = transition.CustodySeq
	timingFields.RequestHash = signingTransition.Proof.RequestHash
	timingFields.InputCount = len(plan.Inputs)
	timingFields.OutputCount = len(plan.Outputs)
	inputRefs := make([]string, 0, len(plan.Inputs))
	for _, input := range plan.Inputs {
		inputRefs = append(inputRefs, fundingRefKey(input.Ref))
	}
	debugMeshf(
		"finalize real custody transition table=%s kind=%s seq=%d request=%s input_count=%d output_count=%d proof_signers=%v tree_signers=%v inputs=%v",
		table.Config.TableID,
		transition.Kind,
		transition.CustodySeq,
		signingTransition.Proof.RequestHash,
		len(plan.Inputs),
		len(plan.Outputs),
		plan.ProofSignerIDs,
		plan.TreeSignerIDs,
		inputRefs,
	)
	if len(plan.Inputs) == 0 {
		return runtime.finalizeNonSettlementCustodyTransition(table, transition, authorizer)
	}
	approvals, err := runtime.collectCustodyApprovals(*table, signingTransition, authorizer, runtime.requiredCustodySigners(*table, signingTransition))
	if err != nil {
		return err
	}
	requestHash := signingTransition.Proof.RequestHash
	result, err := runtime.executeCustodyBatch(*table, transition.PrevStateHash, requestHash, signingTransition, authorizer, plan.Inputs, plan.ProofSignerIDs, plan.TreeSignerIDs, plan.AuthorizedOutputs)
	if err != nil {
		return err
	}
	applyTransitionSettlementPlan(transition, plan, result.OutputRefs)
	transition.ArkIntentID = result.IntentID
	transition.ArkTxID = result.ArkTxID
	transition.NextState.StateHash = tablecustody.HashCustodyState(transition.NextState)
	transition.NextStateHash = transition.NextState.StateHash
	transition.Proof = tablecustody.CustodyProof{
		ArkIntentID:       result.IntentID,
		ArkTxID:           result.ArkTxID,
		FinalizedAt:       result.FinalizedAt,
		RequestHash:       requestHash,
		ReplayValidated:   true,
		SettlementWitness: custodySettlementWitnessFromResult(result),
		Signatures:        append([]tablecustody.CustodySignature(nil), approvals...),
		StateHash:         transition.NextStateHash,
		VTXORefs:          stackProofRefs(transition.NextState),
	}
	transition.Approvals = approvals
	transition.Proof.TransitionHash = tablecustody.HashCustodyTransition(*transition)
	return tablecustody.ValidateTransition(table.LatestCustodyState, *transition)
}

func latestStackClaimForPlayer(state *tablecustody.CustodyState, playerID string) (tablecustody.StackClaim, bool) {
	if state == nil {
		return tablecustody.StackClaim{}, false
	}
	for _, claim := range state.StackClaims {
		if claim.PlayerID == playerID {
			return claim, true
		}
	}
	return tablecustody.StackClaim{}, false
}

func (runtime *meshRuntime) settleTableFundsForPlayer(table nativeTableState, transition tablecustody.CustodyTransition, authorizer *nativeTransitionAuthorizer, playerID string) (*custodyBatchResult, int, string, error) {
	claim, ok := latestStackClaimForPlayer(table.LatestCustodyState, playerID)
	if !ok {
		return nil, 0, "", errors.New("latest custody state is missing the target stack claim")
	}
	totalClaimSats := stackClaimBackedAmount(claim)
	if totalClaimSats <= 0 {
		return nil, 0, "", errors.New("latest custody state has no spendable stack to settle")
	}
	if len(claim.VTXORefs) == 0 {
		return nil, 0, "", errors.New("latest custody state stack claim is missing vtxo refs")
	}
	signingTransition, plan, err := runtime.normalizedCustodySigningTransition(table, transition)
	if err != nil {
		return nil, 0, "", err
	}
	if len(plan.Inputs) == 0 {
		return nil, 0, "", errors.New("latest custody state has no spendable stack to settle")
	}
	settledAmount := 0
	for _, output := range plan.AuthorizedOutputs {
		if output.ClaimKey == "wallet-return" {
			settledAmount = output.AmountSats
			break
		}
	}
	if settledAmount == 0 {
		return nil, 0, "", errors.New("cash-out settlement did not derive the expected wallet-return output")
	}
	if settledAmount <= 0 {
		return nil, 0, "", fmt.Errorf("custody claim is too small to cover Ark cash-out fees: have %d", totalClaimSats)
	}
	transitionHash := custodyTransitionRequestHash(signingTransition)
	result, err := runtime.executeCustodyBatch(table, table.LatestCustodyState.StateHash, transitionHash, signingTransition, authorizer, plan.Inputs, plan.ProofSignerIDs, plan.TreeSignerIDs, plan.AuthorizedOutputs)
	if err != nil {
		return nil, 0, "", err
	}
	exitProofRef := ""
	if transition.Kind == tablecustody.TransitionKindEmergencyExit {
		exitProofRef = tablecustody.BuildExitProofRef(*table.LatestCustodyState, playerID, claim.VTXORefs, nil)
	}
	return result, settledAmount, exitProofRef, nil
}

func (runtime *meshRuntime) settleCurrentTableFunds(table nativeTableState, kind string) (*custodyBatchResult, int, string, error) {
	request, err := runtime.buildSignedFundsRequest(table, kind)
	if err != nil {
		return nil, 0, "", err
	}
	transitionKind, finalStatus, err := fundsTransitionKindAndStatus(kind)
	if err != nil {
		return nil, 0, "", err
	}
	transition, err := runtime.buildFundsCustodyTransitionForPlayer(table, runtime.walletID.PlayerID, transitionKind, finalStatus)
	if err != nil {
		return nil, 0, "", err
	}
	return runtime.settleTableFundsForPlayer(table, transition, authorizerForFundsRequest(request), runtime.walletID.PlayerID)
}
