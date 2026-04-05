package meshruntime

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	arklib "github.com/arkade-os/arkd/pkg/ark-lib"
	arkscript "github.com/arkade-os/arkd/pkg/ark-lib/script"
	arktxutils "github.com/arkade-os/arkd/pkg/ark-lib/txutils"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/parkerpoker/parkerd/internal/game"
	"github.com/parkerpoker/parkerd/internal/tablecustody"
)

const (
	turnChallengeClaimKey   = "turn-challenge-ref"
	turnChallengeStatusOpen = "open"
)

func turnChallengeSourceRefs(state *tablecustody.CustodyState) []tablecustody.VTXORef {
	if state == nil {
		return nil
	}
	refs := make([]tablecustody.VTXORef, 0)
	for _, claim := range state.StackClaims {
		refs = append(refs, claim.VTXORefs...)
	}
	for _, slice := range state.PotSlices {
		refs = append(refs, slice.VTXORefs...)
	}
	return canonicalVTXORefs(refs)
}

func activeTurnChallengePlayerIDsForState(table nativeTableState, state *tablecustody.CustodyState) []string {
	claimStatusByPlayer := map[string]string{}
	if state == nil {
		state = table.LatestCustodyState
	}
	if state != nil {
		for _, claim := range state.StackClaims {
			claimStatusByPlayer[claim.PlayerID] = claim.Status
		}
	}
	playerIDs := make([]string, 0, len(table.Seats))
	for _, seat := range table.Seats {
		if status, ok := claimStatusByPlayer[seat.PlayerID]; ok {
			if terminalCustodySeatStatus(status) {
				continue
			}
		} else if terminalCustodySeatStatus(seat.Status) {
			continue
		}
		playerIDs = append(playerIDs, seat.PlayerID)
	}
	sort.Strings(playerIDs)
	return playerIDs
}

func activeTurnChallengePlayerIDs(table nativeTableState) []string {
	return activeTurnChallengePlayerIDsForState(table, table.LatestCustodyState)
}

func turnChallengeOpenLocktime(deadlineAt string) (arklib.AbsoluteLocktime, bool, error) {
	if strings.TrimSpace(deadlineAt) == "" {
		return 0, false, nil
	}
	locktime, err := challengeTxLocktime(deadlineAt, 0)
	if err != nil {
		return 0, false, err
	}
	return arklib.AbsoluteLocktime(locktime), true, nil
}

func (runtime *meshRuntime) turnChallengeOpenLeaf(table nativeTableState, state *tablecustody.CustodyState) (*arkscript.CLTVMultisigClosure, []string, error) {
	if turnTimeoutModeForTable(table) != turnTimeoutModeChainChallenge || state == nil {
		return nil, nil, nil
	}
	locktime, ok, err := turnChallengeOpenLocktime(state.ActionDeadlineAt)
	if err != nil || !ok {
		return nil, nil, err
	}
	playerIDs := activeTurnChallengePlayerIDsForState(table, state)
	if len(playerIDs) == 0 {
		return nil, nil, nil
	}
	playerPubkeys := make([]*btcec.PublicKey, 0, len(playerIDs))
	for _, playerID := range playerIDs {
		walletPubkeyHex, err := runtime.walletPubkeyHexForPlayer(table, playerID)
		if err != nil {
			return nil, nil, err
		}
		pubkey, err := compressedPubkeyFromHex(walletPubkeyHex)
		if err != nil {
			return nil, nil, err
		}
		playerPubkeys = append(playerPubkeys, pubkey)
	}
	return &arkscript.CLTVMultisigClosure{
		Locktime: locktime,
		MultisigClosure: arkscript.MultisigClosure{
			PubKeys: playerPubkeys,
		},
	}, playerIDs, nil
}

func challengeBundleOutputsToBatchOutputs(outputs []tablecustody.CustodyChallengeOutput) []custodyBatchOutput {
	converted := make([]custodyBatchOutput, 0, len(outputs))
	for _, output := range outputs {
		converted = append(converted, custodyBatchOutput{
			AmountSats:    output.AmountSats,
			ClaimKey:      output.ClaimKey,
			OwnerPlayerID: output.OwnerPlayerID,
			Script:        output.Script,
			Tapscripts:    append([]string(nil), output.Tapscripts...),
		})
	}
	return converted
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func challengeOutputsFromBatchOutputs(outputs []custodyBatchOutput) []tablecustody.CustodyChallengeOutput {
	converted := make([]tablecustody.CustodyChallengeOutput, 0, len(outputs))
	for _, output := range outputs {
		converted = append(converted, tablecustody.CustodyChallengeOutput{
			AmountSats:    output.AmountSats,
			ClaimKey:      output.ClaimKey,
			OwnerPlayerID: output.OwnerPlayerID,
			Script:        output.Script,
			Tapscripts:    append([]string(nil), output.Tapscripts...),
		})
	}
	return converted
}

func turnChallengeMatchesTable(table nativeTableState, challenge *NativePendingTurnChallenge) bool {
	if challenge == nil || !tableHasActionableTurn(table) {
		return false
	}
	expectedEpoch := table.CurrentEpoch
	if table.PendingTurnMenu != nil &&
		table.PendingTurnMenu.Epoch > 0 &&
		table.PendingTurnMenu.HandID == table.ActiveHand.State.HandID &&
		table.PendingTurnMenu.DecisionIndex == custodyDecisionIndex(&table.ActiveHand.State) &&
		table.PendingTurnMenu.ActingPlayerID == seatPlayerID(table, *table.ActiveHand.State.ActingSeatIndex) {
		expectedEpoch = pendingTurnEpoch(table, table.PendingTurnMenu)
	}
	return challenge.TableID == table.Config.TableID &&
		challenge.Epoch == expectedEpoch &&
		challenge.HandID == table.ActiveHand.State.HandID &&
		challenge.DecisionIndex == custodyDecisionIndex(&table.ActiveHand.State) &&
		challenge.ActingPlayerID == seatPlayerID(table, *table.ActiveHand.State.ActingSeatIndex)
}

func turnChallengeTimeoutEligibleAt(menu *NativePendingTurnMenu, windowMS int) string {
	if menu == nil || strings.TrimSpace(menu.ActionDeadlineAt) == "" {
		return ""
	}
	return addMillis(menu.ActionDeadlineAt, windowMS)
}

func turnChallengeOpenBundleTxLocktime(menu *NativePendingTurnMenu) uint32 {
	if menu == nil {
		return 0
	}
	if menu.ChallengeEnvelope != nil && menu.ChallengeEnvelope.OpenBundle.TxLocktime != 0 {
		return menu.ChallengeEnvelope.OpenBundle.TxLocktime
	}
	locktime, err := challengeTxLocktime(menu.ActionDeadlineAt, 0)
	if err != nil {
		return 0
	}
	return locktime
}

func turnChallengeOpenReady(menu *NativePendingTurnMenu) bool {
	txLocktime := turnChallengeOpenBundleTxLocktime(menu)
	if txLocktime == 0 {
		return true
	}
	return uint32(time.Now().Unix()) >= txLocktime
}

func turnChallengeEscapeEligibleAt(openedAt string, spendPath custodySpendPath) string {
	if strings.TrimSpace(openedAt) == "" || !spendPath.UsesCSVLocktime {
		return ""
	}
	switch spendPath.CSVLocktime.Type {
	case arklib.LocktimeTypeSecond:
		delaySeconds := spendPath.CSVLocktime.Seconds()
		if delaySeconds <= 0 {
			return openedAt
		}
		return addMillis(openedAt, int(delaySeconds*1000))
	case arklib.LocktimeTypeBlock:
		return ""
	default:
		return ""
	}
}

func turnChallengeEscapeEligibleHeight(openConfirmedHeight int64, spendPath custodySpendPath) (int64, bool) {
	if !spendPath.UsesCSVLocktime || spendPath.CSVLocktime.Type != arklib.LocktimeTypeBlock {
		return 0, false
	}
	return openConfirmedHeight + int64(spendPath.CSVLocktime.Value), true
}

func turnChallengeEscapeReady(challenge *NativePendingTurnChallenge) bool {
	if challenge == nil || strings.TrimSpace(challenge.EscapeEligibleAt) == "" {
		return true
	}
	return elapsedMillis(challenge.EscapeEligibleAt) >= 0
}

func (runtime *meshRuntime) pendingTurnChallengeOpenContext(table nativeTableState) (*tablecustody.CustodyTransition, nativeTableState, custodySpendPath, error) {
	if !turnChallengeMatchesTable(table, table.PendingTurnChallenge) || table.PendingTurnChallenge == nil {
		return nil, nativeTableState{}, custodySpendPath{}, errors.New("turn challenge is not open for the current table state")
	}
	if len(table.CustodyTransitions) == 0 || table.CustodyTransitions[len(table.CustodyTransitions)-1].Kind != tablecustody.TransitionKindTurnChallengeOpen {
		return nil, nativeTableState{}, custodySpendPath{}, errors.New("turn challenge is missing its accepted open transition")
	}
	openTransition := cloneJSON(table.CustodyTransitions[len(table.CustodyTransitions)-1])
	if openTransition.Proof.ChallengeWitness == nil {
		return nil, nativeTableState{}, custodySpendPath{}, errors.New("turn challenge open transition is missing its witness")
	}
	openTable := cloneJSON(table)
	openState := cloneJSON(openTransition.NextState)
	openTable.LatestCustodyState = &openState
	spendPath, err := runtime.selectTurnChallengeExitSpendPath(openTable, table.PendingTurnChallenge.ChallengeRef)
	if err != nil {
		return nil, nativeTableState{}, custodySpendPath{}, err
	}
	return &openTransition, openTable, spendPath, nil
}

func (runtime *meshRuntime) turnChallengeChainStatus(table nativeTableState) (*NativeTurnChallengeChainStatus, error) {
	openTransition, _, spendPath, err := runtime.pendingTurnChallengeOpenContext(table)
	if err != nil {
		return nil, err
	}
	if spendPath.CSVLocktime.Type != arklib.LocktimeTypeBlock {
		return nil, nil
	}
	openTxID := strings.TrimSpace(openTransition.Proof.ChallengeWitness.TransactionID)
	if openTxID == "" {
		return nil, errors.New("turn challenge open transition is missing its transaction id")
	}
	openStatus, err := runtime.transactionChainStatus(openTxID)
	if err != nil {
		return nil, fmt.Errorf("unable to verify turn challenge open tx status: %w", err)
	}
	status := &NativeTurnChallengeChainStatus{
		OpenConfirmed:       openStatus.Confirmed,
		OpenConfirmedHeight: openStatus.BlockHeight,
		OpenTxID:            openTxID,
	}
	if !openStatus.Confirmed {
		return status, nil
	}
	eligibleHeight, ok := turnChallengeEscapeEligibleHeight(openStatus.BlockHeight, spendPath)
	if !ok {
		return nil, nil
	}
	status.EscapeEligibleHeight = eligibleHeight
	tip, err := runtime.currentChainTip()
	if err != nil {
		return nil, fmt.Errorf("unable to verify chain tip height: %w", err)
	}
	status.ChainTipHeight = tip.Height
	status.ChainTipObservedAt = tip.ObservedAt
	status.EscapeReady = tip.Height >= eligibleHeight
	return status, nil
}

func (runtime *meshRuntime) validateTurnChallengeEscapeReadiness(table nativeTableState) error {
	_, _, spendPath, err := runtime.pendingTurnChallengeOpenContext(table)
	if err != nil {
		return err
	}
	switch spendPath.CSVLocktime.Type {
	case arklib.LocktimeTypeSecond:
		if table.PendingTurnChallenge == nil || strings.TrimSpace(table.PendingTurnChallenge.EscapeEligibleAt) == "" {
			return errors.New("turn challenge escape is missing its eligibility timestamp")
		}
		if !turnChallengeEscapeReady(table.PendingTurnChallenge) {
			return errors.New("turn challenge escape is not yet eligible")
		}
		return nil
	case arklib.LocktimeTypeBlock:
		status, err := runtime.turnChallengeChainStatus(table)
		if err != nil {
			return err
		}
		if status == nil {
			return errors.New("turn challenge escape is missing chain status")
		}
		if !status.OpenConfirmed {
			return errors.New("turn challenge escape is not yet eligible: open tx is unconfirmed")
		}
		if !status.EscapeReady {
			return fmt.Errorf("turn challenge escape is not yet eligible: chain tip height %d is below required height %d", status.ChainTipHeight, status.EscapeEligibleHeight)
		}
		return nil
	default:
		return nil
	}
}

func clearTurnChallengeRefs(transition *tablecustody.CustodyTransition) {
	if transition == nil {
		return
	}
	for index := range transition.NextState.StackClaims {
		transition.NextState.StackClaims[index].VTXORefs = nil
	}
	for index := range transition.NextState.PotSlices {
		transition.NextState.PotSlices[index].VTXORefs = nil
	}
	transition.NextState.StateHash = tablecustody.HashCustodyState(transition.NextState)
	transition.NextStateHash = transition.NextState.StateHash
	transition.Proof.StateHash = transition.NextStateHash
	transition.Proof.VTXORefs = nil
	transition.Proof.TransitionHash = ""
}

func (runtime *meshRuntime) buildTurnChallengeOpenTransition(table nativeTableState, menu *NativePendingTurnMenu) (tablecustody.CustodyTransition, error) {
	if menu == nil || !turnMenuMatchesTable(table, menu) {
		return tablecustody.CustodyTransition{}, errors.New("turn challenge open requires a valid pending turn menu")
	}
	table = tableWithEpoch(table, menu.Epoch)
	hand := cloneJSON(table.ActiveHand.State)
	transition, err := runtime.buildCustodyTransitionWithOverrides(
		table,
		tablecustody.TransitionKindTurnChallengeOpen,
		&hand,
		nil,
		nil,
		&custodyBindingOverrides{ActionDeadlineAt: menu.ActionDeadlineAt},
	)
	if err != nil {
		return tablecustody.CustodyTransition{}, err
	}
	clearTurnChallengeRefs(&transition)
	transition.Proof.RequestHash = custodyTransitionRequestHash(transition)
	return transition, tablecustody.ValidateTransition(table.LatestCustodyState, transition)
}

func (runtime *meshRuntime) buildTurnChallengeEscapeTransition(table nativeTableState, turnDeadlineAt string) (tablecustody.CustodyTransition, error) {
	if table.ActiveHand == nil {
		return tablecustody.CustodyTransition{}, errors.New("turn challenge escape requires an active hand")
	}
	table = tableWithEpoch(table, pendingTurnEpoch(table, table.PendingTurnMenu))
	hand := cloneJSON(table.ActiveHand.State)
	transition, err := runtime.buildCustodyTransitionWithOverrides(
		table,
		tablecustody.TransitionKindTurnChallengeEscape,
		&hand,
		nil,
		nil,
		&custodyBindingOverrides{ActionDeadlineAt: turnDeadlineAt},
	)
	if err != nil {
		return tablecustody.CustodyTransition{}, err
	}
	clearTurnChallengeRefs(&transition)
	transition.Proof.RequestHash = custodyTransitionRequestHash(transition)
	return transition, tablecustody.ValidateTransition(table.LatestCustodyState, transition)
}

func (runtime *meshRuntime) challengeRefOutputSpec(table nativeTableState, amountSats int) (custodyOutputSpec, error) {
	config, err := runtime.arkCustodyConfig()
	if err != nil {
		return custodyOutputSpec{}, err
	}
	playerPubkeys := make([]*btcec.PublicKey, 0, len(table.Seats))
	for _, playerID := range activeTurnChallengePlayerIDs(table) {
		walletPubkeyHex, err := runtime.walletPubkeyHexForPlayer(table, playerID)
		if err != nil {
			return custodyOutputSpec{}, err
		}
		pubkey, err := compressedPubkeyFromHex(walletPubkeyHex)
		if err != nil {
			return custodyOutputSpec{}, err
		}
		playerPubkeys = append(playerPubkeys, pubkey)
	}
	if len(playerPubkeys) == 0 {
		return custodyOutputSpec{}, errors.New("turn challenge ref requires active player keys")
	}
	closures := []arkscript.Closure{
		&arkscript.MultisigClosure{
			PubKeys: append([]*btcec.PublicKey{}, playerPubkeys...),
		},
		&arkscript.CSVMultisigClosure{
			Locktime: config.UnilateralExitDelay,
			MultisigClosure: arkscript.MultisigClosure{
				PubKeys: append([]*btcec.PublicKey{}, playerPubkeys...),
			},
		},
	}
	vtxoScript := &arkscript.TapscriptsVtxoScript{Closures: closures}
	tapscripts, err := vtxoScript.Encode()
	if err != nil {
		return custodyOutputSpec{}, err
	}
	tapKey, _, err := vtxoScript.TapTree()
	if err != nil {
		return custodyOutputSpec{}, err
	}
	pkScript, err := arkscript.P2TRScript(tapKey)
	if err != nil {
		return custodyOutputSpec{}, err
	}
	return custodyOutputSpec{
		AmountSats: amountSats,
		ClaimKey:   turnChallengeClaimKey,
		Script:     hex.EncodeToString(pkScript),
		Tapscripts: tapscripts,
	}, nil
}

func challengeTxLocktime(deadlineAt string, extraMS int) (uint32, error) {
	target := strings.TrimSpace(deadlineAt)
	if target == "" {
		return 0, errors.New("missing challenge locktime deadline")
	}
	if extraMS != 0 {
		target = addMillis(target, extraMS)
	}
	locktime, err := timeoutLocktimeFromISO(target)
	if err != nil {
		return 0, err
	}
	return uint32(locktime), nil
}

func challengeOpenBundleLocktime(turnDeadlineAt string, spendPaths []custodySpendPath) (uint32, error) {
	txLocktime, err := challengeTxLocktime(turnDeadlineAt, 0)
	if err != nil {
		return 0, err
	}
	for _, spendPath := range spendPaths {
		if !spendPath.UsesCLTVLocktime {
			continue
		}
		candidate := uint32(spendPath.Locktime)
		if candidate > txLocktime {
			txLocktime = candidate
		}
	}
	return txLocktime, nil
}

func challengeSpendSequence(spendPath custodySpendPath, txLocktime uint32) (uint32, error) {
	switch {
	case spendPath.UsesCSVLocktime:
		return arklib.BIP68Sequence(spendPath.CSVLocktime)
	case txLocktime != 0:
		return wire.MaxTxInSequenceNum - 1, nil
	default:
		return wire.MaxTxInSequenceNum, nil
	}
}

func challengeUnsignedPSBT(sourceRefs []tablecustody.VTXORef, spendPaths []custodySpendPath, outputs []tablecustody.CustodyChallengeOutput, txLocktime uint32) (string, error) {
	if len(sourceRefs) == 0 || len(sourceRefs) != len(spendPaths) {
		return "", errors.New("custody challenge inputs are incomplete")
	}
	ins := make([]*wire.OutPoint, 0, len(sourceRefs))
	sequences := make([]uint32, 0, len(sourceRefs))
	txOuts := make([]*wire.TxOut, 0, len(outputs)+1)
	for _, output := range challengeBundleOutputsToBatchOutputs(outputs) {
		txOut, err := decodeBatchOutputTxOut(output)
		if err != nil {
			return "", err
		}
		txOuts = append(txOuts, txOut)
	}
	txOuts = append(txOuts, arktxutils.AnchorOutput())
	for index, ref := range sourceRefs {
		hash, err := chainhash.NewHashFromStr(ref.TxID)
		if err != nil {
			return "", err
		}
		ins = append(ins, &wire.OutPoint{Hash: *hash, Index: ref.VOut})
		sequence, err := challengeSpendSequence(spendPaths[index], txLocktime)
		if err != nil {
			return "", err
		}
		sequences = append(sequences, sequence)
	}
	packet, err := psbt.New(ins, txOuts, 3, txLocktime, sequences)
	if err != nil {
		return "", err
	}
	for index := range packet.Inputs {
		packet.Inputs[index].WitnessUtxo = &wire.TxOut{
			Value:    int64(sourceRefs[index].AmountSats),
			PkScript: spendPaths[index].PKScript,
		}
		packet.Inputs[index].TaprootLeafScript = []*psbt.TaprootTapLeafScript{{
			ControlBlock: spendPaths[index].LeafProof.ControlBlock,
			Script:       spendPaths[index].Script,
			LeafVersion:  txscript.BaseLeafVersion,
		}}
	}
	return packet.B64Encode()
}

func validateCustodyChallengePSBT(packet *psbt.Packet, sourceRefs []tablecustody.VTXORef, spendPaths []custodySpendPath, authorizedOutputs []tablecustody.CustodyChallengeOutput, txLocktime uint32) error {
	if packet == nil {
		return errors.New("custody challenge psbt is missing")
	}
	if len(sourceRefs) == 0 || len(sourceRefs) != len(spendPaths) {
		return errors.New("custody challenge inputs are incomplete")
	}
	if len(packet.UnsignedTx.TxIn) != len(sourceRefs) || len(packet.Inputs) != len(sourceRefs) {
		return errors.New("custody challenge psbt input set does not match the authorized source refs")
	}
	for index, txIn := range packet.UnsignedTx.TxIn {
		input := packet.Inputs[index]
		ref := sourceRefs[index]
		if input.WitnessUtxo == nil {
			return errors.New("custody challenge psbt is missing witness utxo metadata")
		}
		if txIn.PreviousOutPoint.Hash.String() != ref.TxID || txIn.PreviousOutPoint.Index != ref.VOut {
			return fmt.Errorf("custody challenge psbt input %d is not authorized", index)
		}
		if input.WitnessUtxo.Value != int64(ref.AmountSats) {
			return fmt.Errorf("custody challenge psbt input %d amount mismatch", index)
		}
		if !bytes.Equal(input.WitnessUtxo.PkScript, spendPaths[index].PKScript) {
			return fmt.Errorf("custody challenge psbt input %d script mismatch", index)
		}
		if len(input.TaprootLeafScript) != 1 {
			return fmt.Errorf("custody challenge psbt input %d is missing its authorized leaf proof", index)
		}
		if !bytes.Equal(input.TaprootLeafScript[0].Script, spendPaths[index].Script) {
			return fmt.Errorf("custody challenge psbt input %d does not use the authorized leaf", index)
		}
		sequence, err := challengeSpendSequence(spendPaths[index], txLocktime)
		if err != nil {
			return err
		}
		if txIn.Sequence != sequence {
			return fmt.Errorf("custody challenge psbt input %d sequence mismatch", index)
		}
	}
	if packet.UnsignedTx.LockTime != txLocktime {
		return errors.New("custody challenge psbt locktime mismatch")
	}
	expectedOutputs := challengeBundleOutputsToBatchOutputs(authorizedOutputs)
	if len(packet.UnsignedTx.TxOut) != len(expectedOutputs)+1 {
		return errors.New("custody challenge psbt output set does not match the authorized outputs")
	}
	for index, output := range expectedOutputs {
		txOut, err := decodeBatchOutputTxOut(output)
		if err != nil {
			return err
		}
		actual := packet.UnsignedTx.TxOut[index]
		if actual.Value != txOut.Value || !bytes.Equal(actual.PkScript, txOut.PkScript) {
			return fmt.Errorf("custody challenge psbt output %d mismatch", index)
		}
	}
	anchor := packet.UnsignedTx.TxOut[len(packet.UnsignedTx.TxOut)-1]
	if anchor.Value != arktxutils.ANCHOR_VALUE || !bytes.Equal(anchor.PkScript, arktxutils.ANCHOR_PKSCRIPT) {
		return errors.New("custody challenge psbt is missing the anchor output")
	}
	return nil
}

func signedChallengePSBTInputsComplete(packet *psbt.Packet, spendPaths []custodySpendPath) bool {
	if packet == nil || len(packet.Inputs) != len(spendPaths) {
		return false
	}
	for index, input := range packet.Inputs {
		requiredSigners := uniqueNonEmptyStrings(spendPaths[index].SignerXOnlyPubkeys)
		if len(requiredSigners) == 0 {
			return false
		}
		required := make(map[string]struct{}, len(requiredSigners))
		for _, xOnly := range requiredSigners {
			required[xOnly] = struct{}{}
		}
		leafHash := txscript.NewBaseTapLeaf(spendPaths[index].Script).TapHash()
		seen := map[string]struct{}{}
		for _, signature := range input.TaprootScriptSpendSig {
			if signature == nil || !bytes.Equal(signature.LeafHash, leafHash[:]) {
				return false
			}
			xOnly := hex.EncodeToString(signature.XOnlyPubKey)
			if _, ok := required[xOnly]; !ok {
				return false
			}
			seen[xOnly] = struct{}{}
		}
		if len(seen) != len(required) {
			return false
		}
	}
	return true
}

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

func validateSignedChallengePSBT(packet *psbt.Packet, sourceRefs []tablecustody.VTXORef, spendPaths []custodySpendPath) (*wire.MsgTx, error) {
	if packet == nil {
		return nil, errors.New("custody challenge psbt is missing")
	}
	if len(packet.Inputs) != len(sourceRefs) || len(packet.Inputs) != len(spendPaths) {
		return nil, errors.New("custody challenge psbt input set is incomplete")
	}
	finalTx, err := finalizedSignedCustodyTxFromPacket(packet)
	if err != nil {
		return nil, err
	}
	prevOuts := txscript.NewMultiPrevOutFetcher(nil)
	for index, txIn := range finalTx.TxIn {
		prevOut := packet.Inputs[index].WitnessUtxo
		if prevOut == nil {
			return nil, fmt.Errorf("custody challenge psbt input %d is missing witness utxo metadata", index)
		}
		prevOuts.AddPrevOut(txIn.PreviousOutPoint, &wire.TxOut{
			PkScript: append([]byte(nil), prevOut.PkScript...),
			Value:    prevOut.Value,
		})
	}
	sigHashes := txscript.NewTxSigHashes(finalTx, prevOuts)
	for index := range finalTx.TxIn {
		prevOut := prevOuts.FetchPrevOutput(finalTx.TxIn[index].PreviousOutPoint)
		if prevOut == nil {
			return nil, fmt.Errorf("custody challenge psbt input %d prevout is unavailable", index)
		}
		vm, err := txscript.NewEngine(
			spendPaths[index].PKScript,
			finalTx,
			index,
			txscript.StandardVerifyFlags,
			nil,
			sigHashes,
			prevOut.Value,
			prevOuts,
		)
		if err != nil {
			return nil, err
		}
		if err := vm.Execute(); err != nil {
			return nil, fmt.Errorf("custody challenge psbt input %d witness does not satisfy the authorized leaf: %w", index, err)
		}
	}
	return finalTx, nil
}

func challengeOutputRefsFromBundle(bundle tablecustody.CustodyChallengeBundle) (map[string][]tablecustody.VTXORef, string, error) {
	packet, err := psbt.NewFromRawBytes(strings.NewReader(bundle.SignedPSBT), true)
	if err != nil {
		return nil, "", err
	}
	finalTx, err := finalizedSignedCustodyTxFromPacket(packet)
	if err != nil {
		return nil, "", err
	}
	outputRefs := map[string][]tablecustody.VTXORef{}
	for index, output := range bundle.AuthorizedOutputs {
		if index >= len(finalTx.TxOut) {
			return nil, "", errors.New("custody challenge bundle output index is out of range")
		}
		txOut := finalTx.TxOut[index]
		ref := tablecustody.VTXORef{
			AmountSats:    int(txOut.Value),
			OwnerPlayerID: output.OwnerPlayerID,
			Script:        output.Script,
			Tapscripts:    append([]string(nil), output.Tapscripts...),
			TxID:          finalTx.TxHash().String(),
			VOut:          uint32(index),
		}
		outputRefs[output.ClaimKey] = append(outputRefs[output.ClaimKey], ref)
	}
	return outputRefs, finalTx.TxHash().String(), nil
}

func applyChallengeOutputRefsToTransition(transition *tablecustody.CustodyTransition, outputRefs map[string][]tablecustody.VTXORef) {
	if transition == nil {
		return
	}
	for index := range transition.NextState.StackClaims {
		claimKey := stackClaimKey(transition.NextState.StackClaims[index].PlayerID)
		transition.NextState.StackClaims[index].VTXORefs = append([]tablecustody.VTXORef(nil), outputRefs[claimKey]...)
	}
	for index := range transition.NextState.PotSlices {
		claimKey := potClaimKey(transition.NextState.PotSlices[index].PotID)
		transition.NextState.PotSlices[index].VTXORefs = append([]tablecustody.VTXORef(nil), outputRefs[claimKey]...)
	}
	transition.NextState.StateHash = tablecustody.HashCustodyState(transition.NextState)
	transition.NextStateHash = transition.NextState.StateHash
	transition.Proof.StateHash = transition.NextStateHash
	transition.Proof.VTXORefs = stackProofRefs(transition.NextState)
	transition.Proof.TransitionHash = tablecustody.HashCustodyTransition(*transition)
}

func (runtime *meshRuntime) fullChallengeOutputsForTransition(table nativeTableState, transition tablecustody.CustodyTransition) ([]tablecustody.CustodyChallengeOutput, error) {
	outputs := make([]tablecustody.CustodyChallengeOutput, 0)
	for _, claim := range tablecustody.SortedStackClaims(transition.NextState.StackClaims) {
		backedAmount := stackClaimBackedAmount(claim)
		if backedAmount <= 0 {
			continue
		}
		spec, err := runtime.stackOutputSpecForTransition(table, &transition, claim.PlayerID, backedAmount)
		if err != nil {
			return nil, err
		}
		outputs = append(outputs, challengeOutputsFromBatchOutputs([]custodyBatchOutput{custodyBatchOutputFromSpec(spec)})...)
	}
	for _, slice := range canonicalCustodyMoneyPots(&transition.NextState) {
		if slice.TotalSats <= 0 {
			continue
		}
		spec, err := runtime.potOutputSpec(table, transition, slice, runtime.potSpendSignerIDsForTransition(table, transition))
		if err != nil {
			return nil, err
		}
		outputs = append(outputs, challengeOutputsFromBatchOutputs([]custodyBatchOutput{custodyBatchOutputFromSpec(spec)})...)
	}
	return outputs, nil
}

func (runtime *meshRuntime) selectTurnChallengeOpenSpendPath(table nativeTableState, ref tablecustody.VTXORef, turnDeadlineAt string) (custodySpendPath, error) {
	if len(ref.Tapscripts) == 0 {
		return custodySpendPath{}, fmt.Errorf("custody ref %s:%d is missing tapscripts", ref.TxID, ref.VOut)
	}
	expectedPlayers := activeTurnChallengePlayerIDs(table)
	if len(expectedPlayers) == 0 {
		return custodySpendPath{}, fmt.Errorf("no active turn challenge players for %s:%d", ref.TxID, ref.VOut)
	}
	expectedLocktime, hasExpectedLocktime, err := turnChallengeOpenLocktime(turnDeadlineAt)
	if err != nil {
		return custodySpendPath{}, err
	}
	vtxoScript, err := arkscript.ParseVtxoScript(ref.Tapscripts)
	if err != nil {
		return custodySpendPath{}, err
	}
	config, err := runtime.arkCustodyConfig()
	if err != nil {
		return custodySpendPath{}, err
	}
	operatorXOnly, err := xOnlyPubkeyHexFromCompressed(config.SignerPubkeyHex)
	if err != nil {
		return custodySpendPath{}, err
	}
	var fallback *custodySpendPath
	for _, closure := range vtxoScript.ForfeitClosures() {
		cltvClosure, ok := closure.(*arkscript.CLTVMultisigClosure)
		if !ok {
			continue
		}
		scriptBytes, err := cltvClosure.Script()
		if err != nil {
			return custodySpendPath{}, err
		}
		tapKey, tapTree, err := vtxoScript.TapTree()
		if err != nil {
			return custodySpendPath{}, err
		}
		leafProof, err := tapTree.GetTaprootMerkleProof(txscript.NewBaseTapLeaf(scriptBytes).TapHash())
		if err != nil {
			return custodySpendPath{}, err
		}
		pkScript, err := arkscript.P2TRScript(tapKey)
		if err != nil {
			return custodySpendPath{}, err
		}
		playerIDs := make([]string, 0, len(cltvClosure.PubKeys))
		signerXOnlyPubkeys := make([]string, 0, len(cltvClosure.PubKeys))
		hasOperator := false
		unmappedNonOperatorKeys := 0
		for _, pubkey := range cltvClosure.PubKeys {
			xOnly := hex.EncodeToString(schnorr.SerializePubKey(pubkey))
			signerXOnlyPubkeys = append(signerXOnlyPubkeys, xOnly)
			if xOnly == operatorXOnly {
				hasOperator = true
				break
			}
			playerID, ok, err := runtime.playerIDByXOnlyPubkey(table, xOnly)
			if err != nil {
				return custodySpendPath{}, err
			}
			if ok {
				playerIDs = append(playerIDs, playerID)
			} else {
				unmappedNonOperatorKeys++
			}
		}
		if hasOperator || unmappedNonOperatorKeys != 0 {
			continue
		}
		playerIDs = uniqueSortedPlayerIDs(playerIDs)
		if !reflect.DeepEqual(playerIDs, expectedPlayers) {
			continue
		}
		candidate := custodySpendPath{
			LeafProof:          leafProof,
			Locktime:           cltvClosure.Locktime,
			PKScript:           pkScript,
			PlayerIDs:          playerIDs,
			SignerXOnlyPubkeys: uniqueNonEmptyStrings(signerXOnlyPubkeys),
			Script:             scriptBytes,
			Tapscripts:         append([]string(nil), ref.Tapscripts...),
			UsesCLTVLocktime:   true,
		}
		if hasExpectedLocktime && cltvClosure.Locktime == expectedLocktime {
			return candidate, nil
		}
		if fallback == nil {
			copy := candidate
			fallback = &copy
		}
	}
	if fallback != nil {
		return *fallback, nil
	}
	return custodySpendPath{}, fmt.Errorf("no turn challenge open leaf found for %s:%d", ref.TxID, ref.VOut)
}

func (runtime *meshRuntime) selectTurnChallengeExitSpendPath(table nativeTableState, ref tablecustody.VTXORef) (custodySpendPath, error) {
	if len(ref.Tapscripts) == 0 {
		return custodySpendPath{}, fmt.Errorf("custody ref %s:%d is missing tapscripts", ref.TxID, ref.VOut)
	}
	vtxoScript, err := arkscript.ParseVtxoScript(ref.Tapscripts)
	if err != nil {
		return custodySpendPath{}, err
	}
	expectedPlayers := activeTurnChallengePlayerIDs(table)
	for _, closure := range vtxoScript.ExitClosures() {
		csvClosure, ok := closure.(*arkscript.CSVMultisigClosure)
		if !ok {
			continue
		}
		scriptBytes, err := csvClosure.Script()
		if err != nil {
			return custodySpendPath{}, err
		}
		tapKey, tapTree, err := vtxoScript.TapTree()
		if err != nil {
			return custodySpendPath{}, err
		}
		leafProof, err := tapTree.GetTaprootMerkleProof(txscript.NewBaseTapLeaf(scriptBytes).TapHash())
		if err != nil {
			return custodySpendPath{}, err
		}
		pkScript, err := arkscript.P2TRScript(tapKey)
		if err != nil {
			return custodySpendPath{}, err
		}
		playerIDs := make([]string, 0, len(csvClosure.PubKeys))
		signerXOnlyPubkeys := make([]string, 0, len(csvClosure.PubKeys))
		for _, pubkey := range csvClosure.PubKeys {
			xOnly := hex.EncodeToString(schnorr.SerializePubKey(pubkey))
			signerXOnlyPubkeys = append(signerXOnlyPubkeys, xOnly)
			playerID, ok, err := runtime.playerIDByXOnlyPubkey(table, xOnly)
			if err != nil {
				return custodySpendPath{}, err
			}
			if ok {
				playerIDs = append(playerIDs, playerID)
			}
		}
		playerIDs = uniqueSortedPlayerIDs(playerIDs)
		if !reflect.DeepEqual(playerIDs, expectedPlayers) {
			continue
		}
		return custodySpendPath{
			LeafProof:          leafProof,
			PKScript:           pkScript,
			PlayerIDs:          playerIDs,
			SignerXOnlyPubkeys: uniqueNonEmptyStrings(signerXOnlyPubkeys),
			Script:             scriptBytes,
			Tapscripts:         append([]string(nil), ref.Tapscripts...),
			UsesCSVLocktime:    true,
			CSVLocktime:        csvClosure.Locktime,
		}, nil
	}
	return custodySpendPath{}, fmt.Errorf("no turn challenge CSV escape leaf found for %s:%d", ref.TxID, ref.VOut)
}

func challengeBundleRequestHash(transition tablecustody.CustodyTransition) string {
	return firstNonEmptyString(strings.TrimSpace(transition.Proof.RequestHash), custodyTransitionRequestHash(transition))
}

func (runtime *meshRuntime) challengeOpenSpendPaths(table nativeTableState, sourceRefs []tablecustody.VTXORef, turnDeadlineAt string) ([]custodySpendPath, []string, error) {
	signerSet := map[string]struct{}{}
	spendPaths := make([]custodySpendPath, 0, len(sourceRefs))
	for _, ref := range sourceRefs {
		spendPath, err := runtime.selectTurnChallengeOpenSpendPath(table, ref, turnDeadlineAt)
		if err != nil {
			return nil, nil, err
		}
		spendPaths = append(spendPaths, spendPath)
		for _, playerID := range spendPath.PlayerIDs {
			signerSet[playerID] = struct{}{}
		}
	}
	signerIDs := make([]string, 0, len(signerSet))
	for playerID := range signerSet {
		signerIDs = append(signerIDs, playerID)
	}
	sort.Strings(signerIDs)
	return spendPaths, signerIDs, nil
}

func (runtime *meshRuntime) buildTurnChallengeOpenBundle(table nativeTableState, menu *NativePendingTurnMenu, transition tablecustody.CustodyTransition) (tablecustody.CustodyTransition, *tablecustody.CustodyChallengeBundle, tablecustody.VTXORef, error) {
	sourceRefs := turnChallengeSourceRefs(table.LatestCustodyState)
	if len(sourceRefs) == 0 {
		return tablecustody.CustodyTransition{}, nil, tablecustody.VTXORef{}, errors.New("turn challenge open is missing source refs")
	}
	authorizer := &nativeTransitionAuthorizer{TurnDeadlineAt: menu.ActionDeadlineAt}
	transition.Proof.RequestHash = challengeBundleRequestHash(transition)
	approvals, err := runtime.collectCustodyApprovals(table, transition, authorizer, runtime.requiredCustodySigners(table, transition))
	if err != nil {
		return tablecustody.CustodyTransition{}, nil, tablecustody.VTXORef{}, err
	}
	transition.Approvals = append([]tablecustody.CustodySignature(nil), approvals...)
	transition.Proof.Signatures = append([]tablecustody.CustodySignature(nil), approvals...)
	transition.Proof.ReplayValidated = true
	transition.Proof.TransitionHash = tablecustody.HashCustodyTransition(transition)

	spendPaths, signerIDs, err := runtime.challengeOpenSpendPaths(table, sourceRefs, menu.ActionDeadlineAt)
	if err != nil {
		return tablecustody.CustodyTransition{}, nil, tablecustody.VTXORef{}, err
	}
	spec, err := runtime.challengeRefOutputSpec(table, sumVTXORefs(sourceRefs))
	if err != nil {
		return tablecustody.CustodyTransition{}, nil, tablecustody.VTXORef{}, err
	}
	outputs := challengeOutputsFromBatchOutputs([]custodyBatchOutput{custodyBatchOutputFromSpec(spec)})
	txLocktime, err := challengeOpenBundleLocktime(menu.ActionDeadlineAt, spendPaths)
	if err != nil {
		return tablecustody.CustodyTransition{}, nil, tablecustody.VTXORef{}, err
	}
	unsignedPSBT, err := challengeUnsignedPSBT(sourceRefs, spendPaths, outputs, txLocktime)
	if err != nil {
		return tablecustody.CustodyTransition{}, nil, tablecustody.VTXORef{}, err
	}
	signedPSBT, err := runtime.fullySignCustodyPSBT(table, transition.PrevStateHash, transition.Proof.RequestHash, "challenge-open", signerIDs, unsignedPSBT, transition, authorizer, challengeBundleOutputsToBatchOutputs(outputs))
	if err != nil {
		return tablecustody.CustodyTransition{}, nil, tablecustody.VTXORef{}, err
	}
	bundle := &tablecustody.CustodyChallengeBundle{
		AuthorizedOutputs: outputs,
		Kind:              tablecustody.TransitionKindTurnChallengeOpen,
		SignedPSBT:        signedPSBT,
		SourceRefs:        append([]tablecustody.VTXORef(nil), sourceRefs...),
		TxLocktime:        txLocktime,
	}
	bundle.BundleHash = tablecustody.HashCustodyChallengeBundle(*bundle)
	outputRefs, _, err := challengeOutputRefsFromBundle(*bundle)
	if err != nil {
		return tablecustody.CustodyTransition{}, nil, tablecustody.VTXORef{}, err
	}
	challengeRefs := outputRefs[turnChallengeClaimKey]
	if len(challengeRefs) != 1 {
		return tablecustody.CustodyTransition{}, nil, tablecustody.VTXORef{}, errors.New("turn challenge open bundle did not produce exactly one challenge ref")
	}
	return transition, bundle, challengeRefs[0], nil
}

func challengeBundleAuthorizer(base *nativeTransitionAuthorizer, openBundle *tablecustody.CustodyChallengeBundle) *nativeTransitionAuthorizer {
	authorizer := cloneTransitionAuthorizer(base)
	if authorizer == nil {
		authorizer = &nativeTransitionAuthorizer{}
	}
	if openBundle != nil {
		cloned := cloneJSON(*openBundle)
		authorizer.ChallengeOpenBundle = &cloned
	}
	return authorizer
}

func (runtime *meshRuntime) buildTurnChallengeResolutionBundle(table nativeTableState, menu *NativePendingTurnMenu, openBundle *tablecustody.CustodyChallengeBundle, challengeRef tablecustody.VTXORef, transition tablecustody.CustodyTransition, baseAuthorizer *nativeTransitionAuthorizer, optionID string) (*tablecustody.CustodyChallengeBundle, error) {
	outputs, err := runtime.fullChallengeOutputsForTransition(table, transition)
	if err != nil {
		return nil, err
	}
	if len(outputs) == 0 {
		return nil, errors.New("turn challenge resolution is missing successor outputs")
	}
	sourceRefs := []tablecustody.VTXORef{challengeRef}
	spendPath, err := runtime.selectCustodySpendPath(table, challengeRef, activeTurnChallengePlayerIDs(table), false)
	if err != nil {
		return nil, err
	}
	txLocktime := uint32(0)
	purpose := "challenge-option"
	if transition.Kind == tablecustody.TransitionKindTimeout {
		purpose = "challenge-timeout"
		txLocktime, err = challengeTxLocktime(menu.ActionDeadlineAt, turnChallengeWindowMSForTable(table))
		if err != nil {
			return nil, err
		}
	}
	unsignedPSBT, err := challengeUnsignedPSBT(sourceRefs, []custodySpendPath{spendPath}, outputs, txLocktime)
	if err != nil {
		return nil, err
	}
	authorizerBase := cloneTransitionAuthorizer(baseAuthorizer)
	if authorizerBase == nil {
		authorizerBase = &nativeTransitionAuthorizer{}
	}
	authorizerBase.TurnDeadlineAt = menu.ActionDeadlineAt
	authorizer := challengeBundleAuthorizer(authorizerBase, openBundle)
	signedPSBT, err := runtime.fullySignCustodyPSBT(table, transition.PrevStateHash, challengeBundleRequestHash(transition), purpose, spendPath.PlayerIDs, unsignedPSBT, transition, authorizer, challengeBundleOutputsToBatchOutputs(outputs))
	if err != nil {
		return nil, err
	}
	bundle := &tablecustody.CustodyChallengeBundle{
		AuthorizedOutputs: outputs,
		Kind:              transition.Kind,
		OptionID:          optionID,
		SignedPSBT:        signedPSBT,
		SourceRefs:        append([]tablecustody.VTXORef(nil), sourceRefs...),
		TimeoutResolution: cloneTimeoutResolution(transition.TimeoutResolution),
		TxLocktime:        txLocktime,
	}
	bundle.BundleHash = tablecustody.HashCustodyChallengeBundle(*bundle)
	return bundle, nil
}

func (runtime *meshRuntime) buildTurnChallengeEscapeBundle(table nativeTableState, menu *NativePendingTurnMenu, openBundle *tablecustody.CustodyChallengeBundle, challengeRef tablecustody.VTXORef, transition tablecustody.CustodyTransition) (*tablecustody.CustodyChallengeBundle, error) {
	outputs, err := runtime.fullChallengeOutputsForTransition(table, transition)
	if err != nil {
		return nil, err
	}
	if len(outputs) == 0 {
		return nil, errors.New("turn challenge escape is missing successor outputs")
	}
	spendPath, err := runtime.selectTurnChallengeExitSpendPath(table, challengeRef)
	if err != nil {
		return nil, err
	}
	unsignedPSBT, err := challengeUnsignedPSBT([]tablecustody.VTXORef{challengeRef}, []custodySpendPath{spendPath}, outputs, 0)
	if err != nil {
		return nil, err
	}
	authorizer := challengeBundleAuthorizer(&nativeTransitionAuthorizer{TurnDeadlineAt: menu.ActionDeadlineAt}, openBundle)
	signedPSBT, err := runtime.fullySignCustodyPSBT(table, transition.PrevStateHash, challengeBundleRequestHash(transition), "challenge-escape", spendPath.PlayerIDs, unsignedPSBT, transition, authorizer, challengeBundleOutputsToBatchOutputs(outputs))
	if err != nil {
		return nil, err
	}
	bundle := &tablecustody.CustodyChallengeBundle{
		AuthorizedOutputs: outputs,
		Kind:              tablecustody.TransitionKindTurnChallengeEscape,
		SignedPSBT:        signedPSBT,
		SourceRefs:        []tablecustody.VTXORef{challengeRef},
		TxLocktime:        0,
	}
	bundle.BundleHash = tablecustody.HashCustodyChallengeBundle(*bundle)
	return bundle, nil
}

func (runtime *meshRuntime) buildChallengeEnvelope(table nativeTableState, menu *NativePendingTurnMenu) (*NativeChallengeEnvelope, error) {
	if menu == nil {
		return nil, nil
	}
	openTransition, err := runtime.buildTurnChallengeOpenTransition(table, menu)
	if err != nil {
		return nil, err
	}
	openTransition, openBundle, challengeRef, err := runtime.buildTurnChallengeOpenBundle(table, menu, openTransition)
	if err != nil {
		return nil, err
	}
	envelope := &NativeChallengeEnvelope{
		OpenBundle:              cloneJSON(*openBundle),
		OpenTransition:          cloneJSON(openTransition),
		OptionResolutionBundles: make([]tablecustody.CustodyChallengeBundle, 0, len(menu.Candidates)),
	}
	for _, candidate := range menu.Candidates {
		bundle, err := runtime.buildTurnChallengeResolutionBundle(
			table,
			menu,
			openBundle,
			challengeRef,
			candidate.Transition,
			authorizerForActionRequest(*candidate.ActionRequest),
			candidate.OptionID,
		)
		if err != nil {
			return nil, err
		}
		envelope.OptionResolutionBundles = append(envelope.OptionResolutionBundles, *bundle)
	}
	timeoutAuthorizer := &nativeTransitionAuthorizer{TurnDeadlineAt: menu.ActionDeadlineAt}
	if menu.TimeoutCandidate == nil {
		return nil, errors.New("pending turn menu is missing its timeout candidate")
	}
	timeoutBundle, err := runtime.buildTurnChallengeResolutionBundle(table, menu, openBundle, challengeRef, menu.TimeoutCandidate.Transition, timeoutAuthorizer, "")
	if err != nil {
		return nil, err
	}
	envelope.TimeoutResolutionBundle = *timeoutBundle
	escapeTransition, err := runtime.buildTurnChallengeEscapeTransition(table, menu.ActionDeadlineAt)
	if err != nil {
		return nil, err
	}
	escapeBundle, err := runtime.buildTurnChallengeEscapeBundle(table, menu, openBundle, challengeRef, escapeTransition)
	if err != nil {
		return nil, err
	}
	envelope.EscapeBundle = *escapeBundle
	return envelope, nil
}

func (runtime *meshRuntime) validateChallengeBundleWithContext(bundle tablecustody.CustodyChallengeBundle, sourceRefs []tablecustody.VTXORef, spendPaths []custodySpendPath, outputs []tablecustody.CustodyChallengeOutput, txLocktime uint32) error {
	packet, err := psbt.NewFromRawBytes(strings.NewReader(bundle.SignedPSBT), true)
	if err != nil {
		return err
	}
	if err := validateChallengeSigningRequestWithContext(packet, bundle.SourceRefs, bundle.AuthorizedOutputs, bundle.TxLocktime, sourceRefs, spendPaths, outputs, txLocktime); err != nil {
		return err
	}
	if !signedChallengePSBTInputsComplete(packet, spendPaths) {
		return errors.New("custody challenge bundle is missing tapscript signatures")
	}
	if _, err := validateSignedChallengePSBT(packet, sourceRefs, spendPaths); err != nil {
		return err
	}
	if expectedHash := tablecustody.HashCustodyChallengeBundle(bundle); bundle.BundleHash != expectedHash {
		return errors.New("custody challenge bundle hash mismatch")
	}
	return nil
}

func validateChallengeSigningRequestWithContext(packet *psbt.Packet, actualSourceRefs []tablecustody.VTXORef, actualOutputs []tablecustody.CustodyChallengeOutput, actualLocktime uint32, sourceRefs []tablecustody.VTXORef, spendPaths []custodySpendPath, outputs []tablecustody.CustodyChallengeOutput, txLocktime uint32) error {
	if !sameCanonicalVTXORefs(actualSourceRefs, sourceRefs) {
		return errors.New("custody challenge bundle source refs mismatch")
	}
	if !reflect.DeepEqual(challengeOutputsFromBatchOutputs(challengeBundleOutputsToBatchOutputs(actualOutputs)), outputs) {
		return errors.New("custody challenge bundle outputs mismatch")
	}
	if actualLocktime != txLocktime {
		return errors.New("custody challenge bundle locktime mismatch")
	}
	if packet == nil {
		return errors.New("custody challenge psbt is missing")
	}
	return validateCustodyChallengePSBT(packet, sourceRefs, spendPaths, outputs, txLocktime)
}

func (runtime *meshRuntime) validateTurnChallengeOpenBundle(table nativeTableState, menu *NativePendingTurnMenu, bundle tablecustody.CustodyChallengeBundle) (tablecustody.VTXORef, error) {
	if bundle.Kind != tablecustody.TransitionKindTurnChallengeOpen {
		return tablecustody.VTXORef{}, errors.New("turn challenge open bundle kind mismatch")
	}
	sourceRefs := turnChallengeSourceRefs(table.LatestCustodyState)
	spendPaths, _, err := runtime.challengeOpenSpendPaths(table, sourceRefs, menu.ActionDeadlineAt)
	if err != nil {
		return tablecustody.VTXORef{}, err
	}
	spec, err := runtime.challengeRefOutputSpec(table, sumVTXORefs(sourceRefs))
	if err != nil {
		return tablecustody.VTXORef{}, err
	}
	outputs := challengeOutputsFromBatchOutputs([]custodyBatchOutput{custodyBatchOutputFromSpec(spec)})
	txLocktime, err := challengeOpenBundleLocktime(menu.ActionDeadlineAt, spendPaths)
	if err != nil {
		return tablecustody.VTXORef{}, err
	}
	if err := runtime.validateChallengeBundleWithContext(bundle, sourceRefs, spendPaths, outputs, txLocktime); err != nil {
		return tablecustody.VTXORef{}, err
	}
	outputRefs, _, err := challengeOutputRefsFromBundle(bundle)
	if err != nil {
		return tablecustody.VTXORef{}, err
	}
	refs := outputRefs[turnChallengeClaimKey]
	if len(refs) != 1 {
		return tablecustody.VTXORef{}, errors.New("turn challenge open bundle did not produce exactly one challenge ref")
	}
	return refs[0], nil
}

func (runtime *meshRuntime) validateTurnChallengeResolutionBundle(table nativeTableState, menu *NativePendingTurnMenu, openBundle tablecustody.CustodyChallengeBundle, transition tablecustody.CustodyTransition, optionID string, bundle tablecustody.CustodyChallengeBundle) error {
	if bundle.Kind != transition.Kind {
		return errors.New("turn challenge resolution bundle kind mismatch")
	}
	challengeRef, err := runtime.validateTurnChallengeOpenBundle(table, menu, openBundle)
	if err != nil {
		return err
	}
	spendPath, err := runtime.selectCustodySpendPath(table, challengeRef, activeTurnChallengePlayerIDs(table), false)
	if err != nil {
		return err
	}
	outputs, err := runtime.fullChallengeOutputsForTransition(table, transition)
	if err != nil {
		return err
	}
	txLocktime := uint32(0)
	if transition.Kind == tablecustody.TransitionKindTimeout {
		txLocktime, err = challengeTxLocktime(menu.ActionDeadlineAt, turnChallengeWindowMSForTable(table))
		if err != nil {
			return err
		}
		if !recoveryBundleTimeoutEquivalent(bundle.TimeoutResolution, transition.TimeoutResolution) {
			return errors.New("turn challenge timeout resolution mismatch")
		}
	} else {
		if bundle.OptionID != optionID {
			return errors.New("turn challenge option id mismatch")
		}
	}
	return runtime.validateChallengeBundleWithContext(bundle, []tablecustody.VTXORef{challengeRef}, []custodySpendPath{spendPath}, outputs, txLocktime)
}

func (runtime *meshRuntime) validateTurnChallengeResolutionSigningRequest(table nativeTableState, menu *NativePendingTurnMenu, openBundle tablecustody.CustodyChallengeBundle, transition tablecustody.CustodyTransition, optionID string, packet *psbt.Packet, actualOutputs []custodyBatchOutput) error {
	if transition.Kind != tablecustody.TransitionKindAction && transition.Kind != tablecustody.TransitionKindTimeout {
		return errors.New("turn challenge resolution transition kind mismatch")
	}
	challengeRef, err := runtime.validateTurnChallengeOpenBundle(table, menu, openBundle)
	if err != nil {
		return err
	}
	spendPath, err := runtime.selectCustodySpendPath(table, challengeRef, activeTurnChallengePlayerIDs(table), false)
	if err != nil {
		return err
	}
	outputs, err := runtime.fullChallengeOutputsForTransition(table, transition)
	if err != nil {
		return err
	}
	txLocktime := uint32(0)
	if transition.Kind == tablecustody.TransitionKindTimeout {
		txLocktime, err = challengeTxLocktime(menu.ActionDeadlineAt, turnChallengeWindowMSForTable(table))
		if err != nil {
			return err
		}
	} else if optionID == "" {
		return errors.New("turn challenge option id mismatch")
	}
	actualLocktime := uint32(0)
	if packet != nil && packet.UnsignedTx != nil {
		actualLocktime = packet.UnsignedTx.LockTime
	}
	return validateChallengeSigningRequestWithContext(
		packet,
		[]tablecustody.VTXORef{challengeRef},
		challengeOutputsFromBatchOutputs(actualOutputs),
		actualLocktime,
		[]tablecustody.VTXORef{challengeRef},
		[]custodySpendPath{spendPath},
		outputs,
		txLocktime,
	)
}

func (runtime *meshRuntime) validateTurnChallengeEscapeBundle(table nativeTableState, menu *NativePendingTurnMenu, openBundle tablecustody.CustodyChallengeBundle, bundle tablecustody.CustodyChallengeBundle) error {
	if bundle.Kind != tablecustody.TransitionKindTurnChallengeEscape {
		return errors.New("turn challenge escape bundle kind mismatch")
	}
	challengeRef, err := runtime.validateTurnChallengeOpenBundle(table, menu, openBundle)
	if err != nil {
		return err
	}
	openTable := cloneJSON(table)
	openState := cloneJSON(menu.ChallengeEnvelope.OpenTransition.NextState)
	openTable.LatestCustodyState = &openState
	transition, err := runtime.buildTurnChallengeEscapeTransition(openTable, menu.ActionDeadlineAt)
	if err != nil {
		return err
	}
	spendPath, err := runtime.selectTurnChallengeExitSpendPath(openTable, challengeRef)
	if err != nil {
		return err
	}
	outputs, err := runtime.fullChallengeOutputsForTransition(openTable, transition)
	if err != nil {
		return err
	}
	return runtime.validateChallengeBundleWithContext(bundle, []tablecustody.VTXORef{challengeRef}, []custodySpendPath{spendPath}, outputs, 0)
}

func challengeEnvelopeBundleByOption(envelope *NativeChallengeEnvelope, optionID string) (tablecustody.CustodyChallengeBundle, bool) {
	if envelope == nil {
		return tablecustody.CustodyChallengeBundle{}, false
	}
	for _, bundle := range envelope.OptionResolutionBundles {
		if bundle.OptionID == optionID {
			return cloneJSON(bundle), true
		}
	}
	return tablecustody.CustodyChallengeBundle{}, false
}

func (runtime *meshRuntime) validateChallengeEnvelope(table nativeTableState, menu *NativePendingTurnMenu) error {
	if turnTimeoutModeForTable(table) != turnTimeoutModeChainChallenge {
		if menu != nil && menu.ChallengeEnvelope != nil {
			return errors.New("direct timeout mode should not carry a challenge envelope")
		}
		return nil
	}
	if menu == nil || menu.ChallengeEnvelope == nil {
		return errors.New("chain-challenge mode requires a challenge envelope")
	}
	expectedOpenTransition, err := runtime.buildTurnChallengeOpenTransition(table, menu)
	if err != nil {
		return err
	}
	if err := runtime.validateCustodyTransitionSemanticsWithOptions(table, menu.ChallengeEnvelope.OpenTransition, &nativeTransitionAuthorizer{TurnDeadlineAt: menu.ActionDeadlineAt}, true); err != nil {
		return fmt.Errorf("pending turn challenge open transition is invalid: %w", err)
	}
	actualOpenComparable, err := runtime.semanticComparableCustodyTransition(table, menu.ChallengeEnvelope.OpenTransition)
	if err != nil {
		return fmt.Errorf("pending turn challenge open transition cannot be normalized: %w", err)
	}
	expectedOpenComparable, err := runtime.semanticComparableCustodyTransition(table, expectedOpenTransition)
	if err != nil {
		return fmt.Errorf("pending turn challenge open transition cannot be derived: %w", err)
	}
	if !reflect.DeepEqual(actualOpenComparable, expectedOpenComparable) {
		return errors.New("pending turn challenge open transition does not match the locally derived successor")
	}
	actualRequestHash := custodyTransitionRequestHash(menu.ChallengeEnvelope.OpenTransition)
	if menu.ChallengeEnvelope.OpenTransition.Proof.RequestHash != actualRequestHash {
		return fmt.Errorf("pending turn challenge open transition request hash mismatch: got %s want %s", menu.ChallengeEnvelope.OpenTransition.Proof.RequestHash, actualRequestHash)
	}
	if err := runtime.validateCustodyApprovals(table, menu.ChallengeEnvelope.OpenTransition, runtime.requiredCustodySigners(table, menu.ChallengeEnvelope.OpenTransition)); err != nil {
		return fmt.Errorf("pending turn challenge open approvals are invalid: %w", err)
	}
	challengeRef, err := runtime.validateTurnChallengeOpenBundle(table, menu, menu.ChallengeEnvelope.OpenBundle)
	if err != nil {
		return fmt.Errorf("pending turn challenge open bundle is invalid: %w", err)
	}
	_ = challengeRef
	for _, option := range menu.Options {
		candidate, ok := findTurnCandidateByOption(menu, option.OptionID)
		if !ok {
			return fmt.Errorf("pending turn challenge envelope is missing option %s", option.OptionID)
		}
		bundle, ok := challengeEnvelopeBundleByOption(menu.ChallengeEnvelope, option.OptionID)
		if !ok {
			return fmt.Errorf("pending turn challenge envelope is missing bundle %s", option.OptionID)
		}
		if err := runtime.validateTurnChallengeResolutionBundle(table, menu, menu.ChallengeEnvelope.OpenBundle, candidate.Transition, option.OptionID, bundle); err != nil {
			return fmt.Errorf("pending turn challenge option bundle %s is invalid: %w", option.OptionID, err)
		}
	}
	if menu.TimeoutCandidate == nil {
		return errors.New("pending turn menu is missing its timeout candidate")
	}
	if err := runtime.validateTurnChallengeResolutionBundle(table, menu, menu.ChallengeEnvelope.OpenBundle, menu.TimeoutCandidate.Transition, "", menu.ChallengeEnvelope.TimeoutResolutionBundle); err != nil {
		return fmt.Errorf("pending turn challenge timeout bundle is invalid: %w", err)
	}
	if err := runtime.validateTurnChallengeEscapeBundle(table, menu, menu.ChallengeEnvelope.OpenBundle, menu.ChallengeEnvelope.EscapeBundle); err != nil {
		return fmt.Errorf("pending turn challenge escape bundle is invalid: %w", err)
	}
	return nil
}

func (runtime *meshRuntime) pendingTurnChallengeFromEnvelope(table nativeTableState, menu *NativePendingTurnMenu, openBundle tablecustody.CustodyChallengeBundle) (*NativePendingTurnChallenge, error) {
	challengeRef, err := runtime.validateTurnChallengeOpenBundle(table, menu, openBundle)
	if err != nil {
		return nil, err
	}
	optionIDs := make([]string, 0, len(menu.Options))
	for _, option := range menu.Options {
		optionIDs = append(optionIDs, option.OptionID)
	}
	sort.Strings(optionIDs)
	return &NativePendingTurnChallenge{
		ActingPlayerID:    menu.ActingPlayerID,
		ChallengeRef:      challengeRef,
		DecisionIndex:     menu.DecisionIndex,
		Epoch:             menu.Epoch,
		EscapeEligibleAt:  "",
		HandID:            menu.HandID,
		OpenBundleHash:    openBundle.BundleHash,
		OptionIDs:         optionIDs,
		SourceStateHash:   latestCustodyStateHash(table),
		Status:            turnChallengeStatusOpen,
		TableID:           table.Config.TableID,
		TimeoutEligibleAt: turnChallengeTimeoutEligibleAt(menu, turnChallengeWindowMSForTable(table)),
		TimeoutResolution: cloneTimeoutResolution(menu.TimeoutCandidate.TimeoutResolution),
	}, nil
}

func (runtime *meshRuntime) challengeBundleForSelectedCandidate(table nativeTableState) (*NativeTurnCandidateBundle, tablecustody.CustodyChallengeBundle, bool) {
	menu, err := runtime.pendingTurnMenuWithLocalBundles(table)
	if err != nil || menu == nil || menu.ChallengeEnvelope == nil {
		return nil, tablecustody.CustodyChallengeBundle{}, false
	}
	selected := strings.TrimSpace(menu.SelectedCandidateHash)
	if selected == "" {
		return nil, tablecustody.CustodyChallengeBundle{}, false
	}
	candidate, ok := findTurnCandidateByHash(menu, selected)
	if !ok || candidate.OptionID == "timeout" {
		return nil, tablecustody.CustodyChallengeBundle{}, false
	}
	bundle, ok := challengeEnvelopeBundleByOption(menu.ChallengeEnvelope, candidate.OptionID)
	if !ok {
		return nil, tablecustody.CustodyChallengeBundle{}, false
	}
	return &candidate, bundle, true
}

func (runtime *meshRuntime) challengeBundleForOptionID(table nativeTableState, optionID string) (*NativeTurnCandidateBundle, tablecustody.CustodyChallengeBundle, bool) {
	menu, err := runtime.pendingTurnMenuWithLocalBundles(table)
	if err != nil || menu == nil || menu.ChallengeEnvelope == nil {
		return nil, tablecustody.CustodyChallengeBundle{}, false
	}
	trimmedOptionID := strings.TrimSpace(optionID)
	if trimmedOptionID == "" {
		return nil, tablecustody.CustodyChallengeBundle{}, false
	}
	candidate, ok := findTurnCandidateByOption(menu, trimmedOptionID)
	if !ok {
		return nil, tablecustody.CustodyChallengeBundle{}, false
	}
	bundle, ok := challengeEnvelopeBundleByOption(menu.ChallengeEnvelope, candidate.OptionID)
	if !ok {
		return nil, tablecustody.CustodyChallengeBundle{}, false
	}
	return &candidate, bundle, true
}

func challengeTimeoutBundle(envelope *NativeChallengeEnvelope) *tablecustody.CustodyChallengeBundle {
	if envelope == nil {
		return nil
	}
	bundle := cloneJSON(envelope.TimeoutResolutionBundle)
	return &bundle
}

func challengeEscapeBundle(envelope *NativeChallengeEnvelope) *tablecustody.CustodyChallengeBundle {
	if envelope == nil {
		return nil
	}
	bundle := cloneJSON(envelope.EscapeBundle)
	return &bundle
}

func challengeBundleFromSigningRequest(packet *psbt.Packet, transition tablecustody.CustodyTransition, outputs []custodyBatchOutput, optionID string, openBundle *tablecustody.CustodyChallengeBundle) tablecustody.CustodyChallengeBundle {
	bundle := tablecustody.CustodyChallengeBundle{
		AuthorizedOutputs: challengeOutputsFromBatchOutputs(outputs),
		Kind:              transition.Kind,
		OptionID:          optionID,
		SignedPSBT:        "",
		TimeoutResolution: cloneTimeoutResolution(transition.TimeoutResolution),
	}
	if packet != nil && packet.UnsignedTx != nil {
		bundle.TxLocktime = packet.UnsignedTx.LockTime
	}
	if openBundle != nil {
		if outputRefs, _, err := challengeOutputRefsFromBundle(*openBundle); err == nil {
			bundle.SourceRefs = canonicalVTXORefs(outputRefs[turnChallengeClaimKey])
		}
	}
	return bundle
}

func comparableChallengeResolutionTransition(transition tablecustody.CustodyTransition) tablecustody.CustodyTransition {
	comparable := comparableSemanticCustodyTransition(transition)
	comparable.CustodySeq = 0
	comparable.PrevStateHash = ""
	comparable.NextState.CustodySeq = 0
	comparable.NextState.PrevStateHash = ""
	return comparable
}

func rebaseChallengeResolutionTransition(table nativeTableState, transition *tablecustody.CustodyTransition) error {
	if transition == nil {
		return errors.New("challenge resolution transition is missing")
	}
	if table.LatestCustodyState == nil {
		return errors.New("challenge resolution requires a prior custody state")
	}
	transition.CustodySeq = table.LatestCustodyState.CustodySeq + 1
	transition.PrevStateHash = table.LatestCustodyState.StateHash
	transition.NextState.CustodySeq = transition.CustodySeq
	transition.NextState.PrevStateHash = transition.PrevStateHash
	transition.NextState.StateHash = tablecustody.HashCustodyState(transition.NextState)
	transition.NextStateHash = transition.NextState.StateHash
	transition.Proof.StateHash = transition.NextStateHash
	transition.Proof.TransitionHash = tablecustody.HashCustodyTransition(*transition)
	return nil
}

func (runtime *meshRuntime) validateChallengeResolutionSemanticMatch(currentTable, sourceTable nativeTableState, actual, expected tablecustody.CustodyTransition) error {
	actualComparable, err := runtime.semanticComparableCustodyTransition(currentTable, actual)
	if err != nil {
		return err
	}
	expectedComparable, err := runtime.semanticComparableCustodyTransition(sourceTable, expected)
	if err != nil {
		return err
	}
	actualComparable = comparableChallengeResolutionTransition(actualComparable)
	expectedComparable = comparableChallengeResolutionTransition(expectedComparable)
	if !reflect.DeepEqual(actualComparable, expectedComparable) {
		return errors.New("turn challenge resolution does not match the prebuilt finite-menu successor")
	}
	return nil
}

func (runtime *meshRuntime) deriveChallengeResolutionTransition(table nativeTableState, bundle NativeTurnCandidateBundle) (tablecustody.CustodyTransition, *nativeTransitionAuthorizer, error) {
	switch {
	case bundle.ActionRequest != nil, bundle.TimeoutResolution != nil:
		transition := cloneJSON(bundle.Transition)
		if err := rebaseChallengeResolutionTransition(table, &transition); err != nil {
			return tablecustody.CustodyTransition{}, nil, err
		}
		return transition, nil, nil
	default:
		return tablecustody.CustodyTransition{}, nil, errors.New("challenge resolution bundle is missing its semantic transition")
	}
}

func (runtime *meshRuntime) executeTurnChallengeBundle(bundle tablecustody.CustodyChallengeBundle) (*tablecustody.CustodyChallengeWitness, error) {
	execution, err := runtime.executeLocalCustodyRecovery(bundle.SignedPSBT)
	if err != nil {
		return nil, err
	}
	_, txid, err := challengeOutputRefsFromBundle(bundle)
	if err != nil {
		return nil, err
	}
	if execution.RecoveryTxID != "" && execution.RecoveryTxID != txid {
		return nil, errors.New("custody challenge execution txid mismatch")
	}
	broadcastTxIDs := uniqueNonEmptyStrings(append(append([]string(nil), execution.BroadcastTxIDs...), txid))
	return &tablecustody.CustodyChallengeWitness{
		BroadcastTxIDs: broadcastTxIDs,
		BundleHash:     bundle.BundleHash,
		ExecutedAt:     nowISO(),
		TransactionID:  txid,
	}, nil
}

func (runtime *meshRuntime) finalizeTurnChallengeResolutionLocked(table *nativeTableState, candidateBundle NativeTurnCandidateBundle, challengeBundle tablecustody.CustodyChallengeBundle) (bool, error) {
	if table == nil || !turnChallengeMatchesTable(*table, table.PendingTurnChallenge) || !turnMenuMatchesTable(*table, table.PendingTurnMenu) {
		return false, nil
	}
	actingSeatIndex := -1
	if table.ActiveHand != nil && table.ActiveHand.State.ActingSeatIndex != nil {
		actingSeatIndex = *table.ActiveHand.State.ActingSeatIndex
	}
	transition, authorizer, err := runtime.deriveChallengeResolutionTransition(*table, candidateBundle)
	if err != nil {
		return false, err
	}
	if authorizer != nil {
		if err := runtime.validateCustodyTransitionSemanticsWithOptions(*table, transition, authorizer, true); err != nil {
			return false, err
		}
	}
	if err := runtime.validateCustodyTransitionSemanticsWithOptions(*table, transition, nil, true); err != nil && transition.Kind == tablecustody.TransitionKindTimeout {
		return false, err
	}
	transition.Approvals = append([]tablecustody.CustodySignature(nil), candidateBundle.Transition.Approvals...)
	transition.Proof.RequestHash = strings.TrimSpace(candidateBundle.Transition.Proof.RequestHash)
	transition.Proof.Signatures = append([]tablecustody.CustodySignature(nil), candidateBundle.Transition.Proof.Signatures...)
	if transition.Proof.RequestHash == "" {
		return false, errors.New("turn challenge resolution is missing its prebuilt request hash")
	}
	witness, err := runtime.executeTurnChallengeBundle(challengeBundle)
	if err != nil {
		return false, err
	}
	outputRefs, _, err := challengeOutputRefsFromBundle(challengeBundle)
	if err != nil {
		return false, err
	}
	transition.Proof.ChallengeBundle = &challengeBundle
	transition.Proof.ChallengeWitness = witness
	transition.Proof.FinalizedAt = witness.ExecutedAt
	transition.Proof.ReplayValidated = true
	applyChallengeOutputRefsToTransition(&transition, outputRefs)
	if err := tablecustody.ValidateTransition(table.LatestCustodyState, transition); err != nil {
		return false, err
	}
	nextHand, err := nextHandStateForCandidate(*table, candidateBundle)
	if err != nil {
		return false, err
	}
	if err := runtime.attachDeterministicRecoveryBundles(*table, &transition, authorizer, &nextHand); err != nil {
		return false, err
	}
	table.ActiveHand.State = nextHand
	runtime.applyCustodyTransition(table, transition)
	switch {
	case candidateBundle.ActionRequest != nil:
		seat, _ := seatRecordForPlayer(*table, candidateBundle.ActionRequest.PlayerID)
		if err := runtime.appendEvent(table, map[string]any{
			"actionRequest":  rawJSONMap(*candidateBundle.ActionRequest),
			"custodySeq":     transition.CustodySeq,
			"playerId":       candidateBundle.ActionRequest.PlayerID,
			"seatIndex":      seat.SeatIndex,
			"transitionHash": transition.Proof.TransitionHash,
			"type":           "PlayerAction",
		}); err != nil {
			return false, err
		}
	default:
		if err := runtime.appendEvent(table, map[string]any{
			"custodySeq":        transition.CustodySeq,
			"playerId":          candidateBundle.TimeoutResolution.ActingPlayerID,
			"seatIndex":         actingSeatIndex,
			"timeoutResolution": rawJSONMap(*candidateBundle.TimeoutResolution),
			"transitionHash":    transition.Proof.TransitionHash,
			"type":              "PlayerAction",
		}); err != nil {
			return false, err
		}
	}
	if err := runtime.advanceHandProtocolLocked(table); err != nil {
		return false, err
	}
	return true, nil
}

func (runtime *meshRuntime) finalizeTurnChallengeEscapeLocked(table *nativeTableState, challengeBundle tablecustody.CustodyChallengeBundle) (bool, error) {
	if table == nil || !turnChallengeMatchesTable(*table, table.PendingTurnChallenge) || !turnMenuMatchesTable(*table, table.PendingTurnMenu) {
		return false, nil
	}
	if !turnChallengeEscapeReady(table.PendingTurnChallenge) {
		return false, errors.New("turn challenge escape is not yet eligible")
	}
	turnDeadlineAt := ""
	if table.PendingTurnMenu != nil {
		turnDeadlineAt = table.PendingTurnMenu.ActionDeadlineAt
	}
	if strings.TrimSpace(turnDeadlineAt) == "" && table.LatestCustodyState != nil {
		turnDeadlineAt = table.LatestCustodyState.ActionDeadlineAt
	}
	transition, err := runtime.buildTurnChallengeEscapeTransition(*table, turnDeadlineAt)
	if err != nil {
		return false, err
	}
	witness, err := runtime.executeTurnChallengeBundle(challengeBundle)
	if err != nil {
		return false, err
	}
	outputRefs, _, err := challengeOutputRefsFromBundle(challengeBundle)
	if err != nil {
		return false, err
	}
	transition.Proof.ChallengeBundle = &challengeBundle
	transition.Proof.ChallengeWitness = witness
	transition.Proof.FinalizedAt = witness.ExecutedAt
	transition.Proof.ReplayValidated = true
	applyChallengeOutputRefsToTransition(&transition, outputRefs)
	if err := tablecustody.ValidateTransition(table.LatestCustodyState, transition); err != nil {
		return false, err
	}
	runtime.applyCustodyTransition(table, transition)
	if err := runtime.abortActiveHandLocked(table, "turn challenge escaped after CSV delay", nil); err != nil {
		return false, err
	}
	return true, nil
}

func (runtime *meshRuntime) openTurnChallengeLocked(table *nativeTableState) (bool, error) {
	if table == nil ||
		turnTimeoutModeForTable(*table) != turnTimeoutModeChainChallenge ||
		table.PendingTurnChallenge != nil ||
		!turnMenuMatchesTable(*table, table.PendingTurnMenu) {
		return false, nil
	}
	if !pendingTurnAllowsUnlockedResolution(*table) {
		return false, nil
	}
	menu, err := runtime.pendingTurnMenuWithLocalBundles(*table)
	if err != nil {
		return false, err
	}
	if menu == nil || menu.ChallengeEnvelope == nil {
		return false, nil
	}
	if elapsedMillis(menu.ActionDeadlineAt) < 0 || !turnChallengeOpenReady(menu) {
		return false, nil
	}
	pending, err := runtime.pendingTurnChallengeFromEnvelope(*table, menu, menu.ChallengeEnvelope.OpenBundle)
	if err != nil {
		return false, err
	}
	transition := cloneJSON(menu.ChallengeEnvelope.OpenTransition)
	witness, err := runtime.executeTurnChallengeBundle(menu.ChallengeEnvelope.OpenBundle)
	if err != nil {
		return false, err
	}
	pending.OpenedAt = witness.ExecutedAt
	escapeSpendPath, err := runtime.selectTurnChallengeExitSpendPath(*table, pending.ChallengeRef)
	if err != nil {
		return false, err
	}
	pending.EscapeEligibleAt = turnChallengeEscapeEligibleAt(pending.OpenedAt, escapeSpendPath)
	transition.Proof.ChallengeBundle = cloneJSON(&menu.ChallengeEnvelope.OpenBundle)
	transition.Proof.ChallengeWitness = witness
	transition.Proof.FinalizedAt = witness.ExecutedAt
	transition.Proof.ReplayValidated = true
	transition.Proof.TransitionHash = tablecustody.HashCustodyTransition(transition)
	if err := tablecustody.ValidateTransition(table.LatestCustodyState, transition); err != nil {
		return false, err
	}
	runtime.applyCustodyTransition(table, transition)
	table.PendingTurnChallenge = pending
	if err := runtime.appendEvent(table, map[string]any{
		"challengeRef":      rawJSONMap(pending.ChallengeRef),
		"custodySeq":        transition.CustodySeq,
		"escapeEligibleAt":  pending.EscapeEligibleAt,
		"openBundleHash":    pending.OpenBundleHash,
		"openedAt":          pending.OpenedAt,
		"timeoutEligibleAt": pending.TimeoutEligibleAt,
		"transitionHash":    transition.Proof.TransitionHash,
		"type":              "TurnChallengeOpened",
	}); err != nil {
		return false, err
	}
	return true, nil
}

func (runtime *meshRuntime) handlePendingTurnChallengeLocked(table *nativeTableState) (bool, error) {
	if table == nil || !turnChallengeMatchesTable(*table, table.PendingTurnChallenge) || !turnMenuMatchesTable(*table, table.PendingTurnMenu) {
		return false, nil
	}
	if candidate, bundle, ok := runtime.challengeBundleForSelectedCandidate(*table); ok {
		return runtime.finalizeTurnChallengeResolutionLocked(table, *candidate, bundle)
	}
	menu, err := runtime.pendingTurnMenuWithLocalBundles(*table)
	if err != nil {
		return false, err
	}
	if menu == nil {
		return false, nil
	}
	timeoutBundle := challengeTimeoutBundle(menu.ChallengeEnvelope)
	if timeoutBundle == nil || strings.TrimSpace(table.PendingTurnChallenge.TimeoutEligibleAt) == "" || elapsedMillis(table.PendingTurnChallenge.TimeoutEligibleAt) < 0 {
		return false, nil
	}
	if menu.TimeoutCandidate == nil {
		return false, errors.New("pending turn menu is missing its timeout candidate")
	}
	return runtime.finalizeTurnChallengeResolutionLocked(table, *menu.TimeoutCandidate, *timeoutBundle)
}

func (runtime *meshRuntime) validateAcceptedPendingTurnChallenge(table nativeTableState, challenge *NativePendingTurnChallenge) error {
	if challenge == nil {
		if len(table.CustodyTransitions) > 0 && table.CustodyTransitions[len(table.CustodyTransitions)-1].Kind == tablecustody.TransitionKindTurnChallengeOpen {
			return errors.New("accepted turn challenge open transition is missing pending turn challenge state")
		}
		return nil
	}
	if !turnChallengeMatchesTable(table, challenge) {
		return errors.New("pending turn challenge does not match the current turn")
	}
	if !turnMenuMatchesTable(table, table.PendingTurnMenu) || table.PendingTurnMenu == nil {
		return errors.New("pending turn challenge requires its matching pending turn menu")
	}
	optionIDs := make([]string, 0, len(table.PendingTurnMenu.Options))
	for _, option := range table.PendingTurnMenu.Options {
		optionIDs = append(optionIDs, option.OptionID)
	}
	sort.Strings(optionIDs)
	challengeOptionIDs := append([]string(nil), challenge.OptionIDs...)
	sort.Strings(challengeOptionIDs)
	if !reflect.DeepEqual(optionIDs, challengeOptionIDs) {
		return errors.New("pending turn challenge option ids mismatch")
	}
	if challenge.Status != turnChallengeStatusOpen {
		return errors.New("pending turn challenge status mismatch")
	}
	if len(table.CustodyTransitions) == 0 || table.CustodyTransitions[len(table.CustodyTransitions)-1].Kind != tablecustody.TransitionKindTurnChallengeOpen {
		return errors.New("pending turn challenge requires a latest turn-challenge-open transition")
	}
	openTransition := table.CustodyTransitions[len(table.CustodyTransitions)-1]
	if openTransition.Proof.ChallengeWitness == nil {
		return errors.New("latest turn challenge open transition is missing its witness")
	}
	if openTransition.Proof.ChallengeBundle == nil {
		return errors.New("latest turn challenge open transition is missing its challenge bundle")
	}
	if challenge.OpenBundleHash != openTransition.Proof.ChallengeBundle.BundleHash {
		return errors.New("pending turn challenge open bundle hash mismatch")
	}
	openSourceTable := cloneJSON(table)
	if len(table.CustodyTransitions) > 1 {
		previous := cloneJSON(table.CustodyTransitions[len(table.CustodyTransitions)-2].NextState)
		openSourceTable.LatestCustodyState = &previous
	} else {
		openSourceTable.LatestCustodyState = nil
	}
	openSourceTable = tableWithEpoch(openSourceTable, pendingTurnEpoch(table, table.PendingTurnMenu))
	if latestCustodyStateHash(openSourceTable) != challenge.SourceStateHash {
		return errors.New("pending turn challenge source state hash mismatch")
	}
	legalActions := game.GetLegalActions(openSourceTable.ActiveHand.State, openSourceTable.ActiveHand.State.ActingSeatIndex)
	actionTypes := make([]string, 0, len(legalActions))
	for _, legal := range legalActions {
		actionTypes = append(actionTypes, string(legal.Type))
	}
	expectedTimeoutResolution := tablecustody.BuildTimeoutResolution(timeoutPolicyFromState(openSourceTable.LatestCustodyState), challenge.ActingPlayerID, actionTypes, []string{challenge.ActingPlayerID})
	if !reflect.DeepEqual(cloneTimeoutResolution(&expectedTimeoutResolution), cloneTimeoutResolution(challenge.TimeoutResolution)) {
		return errors.New("pending turn challenge timeout resolution mismatch")
	}
	outputRefs, _, err := challengeOutputRefsFromBundle(*openTransition.Proof.ChallengeBundle)
	if err != nil {
		return err
	}
	expectedChallengeRef, ok := outputRefs[turnChallengeClaimKey]
	if !ok || len(expectedChallengeRef) != 1 {
		return errors.New("pending turn challenge open bundle is missing its challenge ref")
	}
	if !sameCanonicalVTXORefs([]tablecustody.VTXORef{expectedChallengeRef[0]}, []tablecustody.VTXORef{challenge.ChallengeRef}) {
		return errors.New("pending turn challenge ref mismatch")
	}
	if turnChallengeTimeoutEligibleAt(table.PendingTurnMenu, turnChallengeWindowMSForTable(table)) != challenge.TimeoutEligibleAt {
		return errors.New("pending turn challenge timeout window mismatch")
	}
	expectedOpenedAt := firstNonEmptyString(openTransition.Proof.ChallengeWitness.ExecutedAt, openTransition.Proof.FinalizedAt)
	if challenge.OpenedAt != expectedOpenedAt {
		return errors.New("pending turn challenge opened-at mismatch")
	}
	openState := cloneJSON(openTransition.NextState)
	openTable := cloneJSON(table)
	openTable.LatestCustodyState = &openState
	escapeSpendPath, err := runtime.selectTurnChallengeExitSpendPath(openTable, challenge.ChallengeRef)
	if err != nil {
		return err
	}
	expectedEscapeEligibleAt := turnChallengeEscapeEligibleAt(expectedOpenedAt, escapeSpendPath)
	if challenge.EscapeEligibleAt != expectedEscapeEligibleAt {
		return errors.New("pending turn challenge escape window mismatch")
	}
	return nil
}

func acceptedChallengeOpenContextForTransition(table nativeTableState, transition tablecustody.CustodyTransition) (*tablecustody.CustodyTransition, *tablecustody.CustodyState, *tablecustody.CustodyChallengeBundle, error) {
	for index := len(table.CustodyTransitions) - 1; index >= 0; index-- {
		candidate := table.CustodyTransitions[index]
		if candidate.Kind != tablecustody.TransitionKindTurnChallengeOpen {
			continue
		}
		if candidate.NextStateHash != transition.PrevStateHash {
			continue
		}
		if candidate.Proof.ChallengeBundle == nil {
			return nil, nil, nil, errors.New("turn challenge source transition is missing its challenge bundle")
		}
		if candidate.Proof.ChallengeWitness == nil {
			return nil, nil, nil, errors.New("turn challenge source transition is missing its challenge witness")
		}
		var previous *tablecustody.CustodyState
		if index > 0 {
			state := cloneJSON(table.CustodyTransitions[index-1].NextState)
			previous = &state
		}
		cloned := cloneJSON(*candidate.Proof.ChallengeBundle)
		openTransition := cloneJSON(candidate)
		return &openTransition, previous, &cloned, nil
	}
	return nil, nil, nil, errors.New("turn challenge source transition is missing")
}

func validateChallengeWitnessMetadata(witness *tablecustody.CustodyChallengeWitness, bundle *tablecustody.CustodyChallengeBundle) error {
	if witness == nil {
		return errors.New("custody challenge witness is missing")
	}
	if bundle == nil {
		return errors.New("custody challenge bundle is missing")
	}
	if witness.BundleHash != bundle.BundleHash {
		return errors.New("custody challenge witness bundle hash mismatch")
	}
	return nil
}

func participantProtocolPubkeyForPeer(table nativeTableState, peerID string) (string, bool) {
	trimmedPeerID := strings.TrimSpace(peerID)
	if trimmedPeerID == "" {
		return "", false
	}
	if table.CurrentHost.Peer.PeerID == trimmedPeerID && strings.TrimSpace(table.CurrentHost.Peer.ProtocolPubkeyHex) != "" {
		return table.CurrentHost.Peer.ProtocolPubkeyHex, true
	}
	for _, seat := range table.Seats {
		if seat.PeerID == trimmedPeerID && strings.TrimSpace(seat.ProtocolPubkeyHex) != "" {
			return seat.ProtocolPubkeyHex, true
		}
	}
	for _, witness := range table.Witnesses {
		if witness.Peer.PeerID == trimmedPeerID && strings.TrimSpace(witness.Peer.ProtocolPubkeyHex) != "" {
			return witness.Peer.ProtocolPubkeyHex, true
		}
	}
	return "", false
}

func isSeatedPeer(table nativeTableState, peerID string) bool {
	trimmedPeerID := strings.TrimSpace(peerID)
	if trimmedPeerID == "" {
		return false
	}
	for _, seat := range table.Seats {
		if seat.PeerID == trimmedPeerID {
			return true
		}
	}
	return false
}

func challengeSyncTransitionKindsOnly(transitions []tablecustody.CustodyTransition) bool {
	if len(transitions) == 0 {
		return false
	}
	for _, transition := range transitions {
		if transition.Proof.ChallengeWitness == nil {
			return false
		}
		switch transition.Kind {
		case tablecustody.TransitionKindTurnChallengeOpen, tablecustody.TransitionKindAction, tablecustody.TransitionKindTimeout, tablecustody.TransitionKindTurnChallengeEscape:
		default:
			return false
		}
	}
	return true
}

func challengeSyncEventKindsOnly(events []NativeSignedTableEvent) bool {
	for _, event := range events {
		eventType := strings.TrimSpace(stringValue(event.Body["type"]))
		switch eventType {
		case "TurnChallengeOpened", "PlayerAction", "HandAbort":
		default:
			return false
		}
	}
	return true
}

func (runtime *meshRuntime) allowNonHostChallengeSync(existing *nativeTableState, incoming nativeTableState, senderPeerID string) bool {
	if existing == nil || strings.TrimSpace(senderPeerID) == "" {
		return false
	}
	if senderPeerID == existing.CurrentHost.Peer.PeerID || !isSeatedPeer(incoming, senderPeerID) {
		return false
	}
	if incoming.CurrentEpoch != existing.CurrentEpoch || incoming.CurrentHost.Peer.PeerID != existing.CurrentHost.Peer.PeerID {
		return false
	}
	if len(incoming.CustodyTransitions) <= len(existing.CustodyTransitions) || len(incoming.Events) < len(existing.Events) {
		return false
	}
	if !challengeSyncTransitionKindsOnly(incoming.CustodyTransitions[len(existing.CustodyTransitions):]) {
		return false
	}
	return challengeSyncEventKindsOnly(incoming.Events[len(existing.Events):])
}

func (runtime *meshRuntime) validateAcceptedCustodyChallengeWitness(table nativeTableState, previous *tablecustody.CustodyState, transition tablecustody.CustodyTransition) error {
	witness := transition.Proof.ChallengeWitness
	bundle := transition.Proof.ChallengeBundle
	if err := validateChallengeWitnessMetadata(witness, bundle); err != nil {
		return err
	}
	baseTable := cloneJSON(table)
	baseTable.LatestCustodyState = previous

	switch transition.Kind {
	case tablecustody.TransitionKindTurnChallengeOpen:
		sourceRefs := turnChallengeSourceRefs(previous)
		spendPaths, _, err := runtime.challengeOpenSpendPaths(baseTable, sourceRefs, transition.NextState.ActionDeadlineAt)
		if err != nil {
			return err
		}
		spec, err := runtime.challengeRefOutputSpec(baseTable, sumVTXORefs(sourceRefs))
		if err != nil {
			return err
		}
		outputs := challengeOutputsFromBatchOutputs([]custodyBatchOutput{custodyBatchOutputFromSpec(spec)})
		txLocktime, err := challengeOpenBundleLocktime(transition.NextState.ActionDeadlineAt, spendPaths)
		if err != nil {
			return err
		}
		if err := runtime.validateChallengeBundleWithContext(*bundle, sourceRefs, spendPaths, outputs, txLocktime); err != nil {
			return err
		}
		_, txid, err := challengeOutputRefsFromBundle(*bundle)
		if err != nil {
			return err
		}
		if witness.TransactionID != txid {
			return errors.New("custody challenge witness txid mismatch")
		}
		if !containsString(witness.BroadcastTxIDs, txid) {
			return errors.New("custody challenge witness broadcast metadata is missing the challenge transaction")
		}
		if len(transition.Proof.VTXORefs) != 0 {
			return errors.New("turn challenge open transition should not expose stack proof refs")
		}
		return nil
	case tablecustody.TransitionKindAction, tablecustody.TransitionKindTimeout:
		openTransition, openPrevious, openBundle, err := acceptedChallengeOpenContextForTransition(table, transition)
		if err != nil {
			return err
		}
		openSourceTable := cloneJSON(table)
		openSourceTable.LatestCustodyState = openPrevious
		challengeRef, err := runtime.validateTurnChallengeOpenBundle(openSourceTable, &NativePendingTurnMenu{
			ActionDeadlineAt: openTransition.NextState.ActionDeadlineAt,
		}, *openBundle)
		if err != nil {
			return err
		}
		if !sameCanonicalVTXORefs(bundle.SourceRefs, []tablecustody.VTXORef{challengeRef}) {
			return errors.New("custody challenge bundle source does not match the accepted challenge ref")
		}
		spendPath, err := runtime.selectCustodySpendPath(baseTable, challengeRef, activeTurnChallengePlayerIDs(baseTable), false)
		if err != nil {
			return err
		}
		expected := cloneJSON(transition)
		clearTurnChallengeRefs(&expected)
		outputs, err := runtime.fullChallengeOutputsForTransition(baseTable, expected)
		if err != nil {
			return err
		}
		txLocktime := uint32(0)
		if transition.Kind == tablecustody.TransitionKindTimeout {
			txLocktime, err = challengeTxLocktime(transition.NextState.ActionDeadlineAt, turnChallengeWindowMSForTable(baseTable))
			if err != nil {
				return err
			}
		}
		if err := runtime.validateChallengeBundleWithContext(*bundle, []tablecustody.VTXORef{challengeRef}, []custodySpendPath{spendPath}, outputs, txLocktime); err != nil {
			return err
		}
		outputRefs, txid, err := challengeOutputRefsFromBundle(*bundle)
		if err != nil {
			return err
		}
		if witness.TransactionID != txid {
			return errors.New("custody challenge witness txid mismatch")
		}
		if !containsString(witness.BroadcastTxIDs, txid) {
			return errors.New("custody challenge witness broadcast metadata is missing the challenge transaction")
		}
		applyChallengeOutputRefsToTransition(&expected, outputRefs)
		if !reflect.DeepEqual(canonicalCustodyMoneyStacks(&expected.NextState), canonicalCustodyMoneyStacks(&transition.NextState)) {
			return errors.New("challenge-derived stack refs do not match the accepted next state")
		}
		if !reflect.DeepEqual(canonicalCustodyMoneyPots(&expected.NextState), canonicalCustodyMoneyPots(&transition.NextState)) {
			return errors.New("challenge-derived pot refs do not match the accepted next state")
		}
		if !sameCanonicalVTXORefs(stackProofRefs(expected.NextState), transition.Proof.VTXORefs) {
			return errors.New("challenge-derived proof refs do not match the accepted transition")
		}
		return nil
	case tablecustody.TransitionKindTurnChallengeEscape:
		openTransition, openPrevious, openBundle, err := acceptedChallengeOpenContextForTransition(table, transition)
		if err != nil {
			return err
		}
		openSourceTable := cloneJSON(table)
		openSourceTable.LatestCustodyState = openPrevious
		challengeRef, err := runtime.validateTurnChallengeOpenBundle(openSourceTable, &NativePendingTurnMenu{
			ActionDeadlineAt: openTransition.NextState.ActionDeadlineAt,
		}, *openBundle)
		if err != nil {
			return err
		}
		if !sameCanonicalVTXORefs(bundle.SourceRefs, []tablecustody.VTXORef{challengeRef}) {
			return errors.New("custody challenge escape bundle source does not match the accepted challenge ref")
		}
		openTable := cloneJSON(table)
		openState := cloneJSON(openTransition.NextState)
		openTable.LatestCustodyState = &openState
		spendPath, err := runtime.selectTurnChallengeExitSpendPath(openTable, challengeRef)
		if err != nil {
			return err
		}
		openTxID := strings.TrimSpace(openTransition.Proof.ChallengeWitness.TransactionID)
		if openTxID == "" {
			return errors.New("turn challenge open witness txid is missing")
		}
		expected := cloneJSON(transition)
		clearTurnChallengeRefs(&expected)
		outputs, err := runtime.fullChallengeOutputsForTransition(openTable, expected)
		if err != nil {
			return err
		}
		if err := runtime.validateChallengeBundleWithContext(*bundle, []tablecustody.VTXORef{challengeRef}, []custodySpendPath{spendPath}, outputs, 0); err != nil {
			return err
		}
		outputRefs, txid, err := challengeOutputRefsFromBundle(*bundle)
		if err != nil {
			return err
		}
		if witness.TransactionID != txid {
			return errors.New("custody challenge witness txid mismatch")
		}
		if !containsString(witness.BroadcastTxIDs, txid) {
			return errors.New("custody challenge witness broadcast metadata is missing the challenge transaction")
		}
		switch spendPath.CSVLocktime.Type {
		case arklib.LocktimeTypeSecond:
			eligibleAt := turnChallengeEscapeEligibleAt(firstNonEmptyString(openTransition.Proof.ChallengeWitness.ExecutedAt, openTransition.Proof.FinalizedAt), spendPath)
			if strings.TrimSpace(eligibleAt) == "" {
				return errors.New("turn challenge escape eligibility timestamp is unavailable")
			}
			eligibleTime, err := parseISOTimestamp(eligibleAt)
			if err != nil {
				return err
			}
			executedAt, err := parseISOTimestamp(firstNonEmptyString(witness.ExecutedAt, transition.Proof.FinalizedAt))
			if err != nil {
				return err
			}
			if executedAt.Before(eligibleTime) {
				return errors.New("custody challenge escape witness executed before the CSV delay matured")
			}
		case arklib.LocktimeTypeBlock:
			openStatus, err := runtime.transactionChainStatus(openTxID)
			if err != nil {
				return fmt.Errorf("unable to verify turn challenge open tx status: %w", err)
			}
			if !openStatus.Confirmed {
				return errors.New("turn challenge open tx is unconfirmed")
			}
			eligibleHeight, ok := turnChallengeEscapeEligibleHeight(openStatus.BlockHeight, spendPath)
			if !ok {
				return errors.New("turn challenge escape eligible height is unavailable")
			}
			escapeStatus, err := runtime.transactionChainStatus(txid)
			if err != nil {
				return fmt.Errorf("unable to verify turn challenge escape tx status: %w", err)
			}
			if escapeStatus.Confirmed {
				if escapeStatus.BlockHeight < eligibleHeight {
					return errors.New("custody challenge escape witness confirmed before the CSV block delay matured")
				}
				break
			}
			tip, err := runtime.currentChainTip()
			if err != nil {
				return fmt.Errorf("unable to verify chain tip height for turn challenge escape: %w", err)
			}
			if tip.Height < eligibleHeight {
				return errors.New("turn challenge escape tx is unconfirmed before the CSV block delay matured")
			}
		}
		applyChallengeOutputRefsToTransition(&expected, outputRefs)
		if !reflect.DeepEqual(canonicalCustodyMoneyStacks(&expected.NextState), canonicalCustodyMoneyStacks(&transition.NextState)) {
			return errors.New("challenge escape-derived stack refs do not match the accepted next state")
		}
		if !reflect.DeepEqual(canonicalCustodyMoneyPots(&expected.NextState), canonicalCustodyMoneyPots(&transition.NextState)) {
			return errors.New("challenge escape-derived pot refs do not match the accepted next state")
		}
		if !sameCanonicalVTXORefs(stackProofRefs(expected.NextState), transition.Proof.VTXORefs) {
			return errors.New("challenge escape-derived proof refs do not match the accepted transition")
		}
		return nil
	default:
		return errors.New("custody challenge witness is not supported for this transition kind")
	}
}
