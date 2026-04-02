package meshruntime

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	arklib "github.com/arkade-os/arkd/pkg/ark-lib"
	arkscript "github.com/arkade-os/arkd/pkg/ark-lib/script"
	arktxutils "github.com/arkade-os/arkd/pkg/ark-lib/txutils"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/parkerpoker/parkerd/internal/game"
	"github.com/parkerpoker/parkerd/internal/tablecustody"
)

func custodyRecoverySupportedTable(table nativeTableState) bool {
	if len(table.Seats) != 2 {
		return false
	}
	for _, seat := range table.Seats {
		if terminalCustodySeatStatus(seat.Status) {
			return false
		}
	}
	return true
}

func sourcePotRecoveryRefs(state *tablecustody.CustodyState) []tablecustody.VTXORef {
	if state == nil {
		return nil
	}
	refs := make([]tablecustody.VTXORef, 0)
	for _, slice := range state.PotSlices {
		refs = append(refs, slice.VTXORefs...)
	}
	return canonicalVTXORefs(refs)
}

func stackRecoveryDeltaOutputs(previous *tablecustody.CustodyState, next tablecustody.CustodyState) (map[string]int, bool) {
	previousAmountByPlayer := map[string]int{}
	if previous != nil {
		for _, claim := range previous.StackClaims {
			previousAmountByPlayer[claim.PlayerID] = stackClaimBackedAmount(claim)
		}
	}
	deltas := map[string]int{}
	for _, claim := range next.StackClaims {
		nextAmount := stackClaimBackedAmount(claim)
		prevAmount := previousAmountByPlayer[claim.PlayerID]
		if nextAmount < prevAmount {
			return nil, false
		}
		if nextAmount > prevAmount {
			deltas[claim.PlayerID] = nextAmount - prevAmount
		}
	}
	return deltas, true
}

func (runtime *meshRuntime) recoveryAuthorizedOutputsForTransition(table nativeTableState, previous *tablecustody.CustodyState, target tablecustody.CustodyTransition) ([]custodyBatchOutput, error) {
	if previous == nil {
		return nil, nil
	}
	if !custodyTransitionRequiresArkSettlement(previous, target) {
		return nil, nil
	}
	if recoveryRemainingPotTotal(target.NextState.PotSlices) != 0 {
		return nil, nil
	}
	deltas, ok := stackRecoveryDeltaOutputs(previous, target.NextState)
	if !ok {
		return nil, nil
	}
	outputs := make([]custodyBatchOutput, 0, len(deltas))
	for _, claim := range tablecustody.SortedStackClaims(target.NextState.StackClaims) {
		delta := deltas[claim.PlayerID]
		if delta <= 0 {
			continue
		}
		spec, err := runtime.stackOutputSpec(table, claim.PlayerID, delta)
		if err != nil {
			return nil, err
		}
		outputs = append(outputs, custodyBatchOutputFromSpec(spec))
	}
	if len(outputs) == 0 {
		return nil, nil
	}
	return outputs, nil
}

func recoveryRemainingPotTotal(slices []tablecustody.PotSlice) int {
	total := 0
	for _, slice := range slices {
		total += slice.TotalSats
	}
	return total
}

func (runtime *meshRuntime) buildActionTimeoutRecoveryTransition(table nativeTableState, hand game.HoldemState) (*tablecustody.CustodyTransition, error) {
	if !game.PhaseAllowsActions(hand.Phase) || hand.ActingSeatIndex == nil {
		return nil, nil
	}
	actingSeatIndex := *hand.ActingSeatIndex
	actingPlayerID := seatPlayerID(table, actingSeatIndex)
	legalActions := game.GetLegalActions(hand, hand.ActingSeatIndex)
	actionTypes := make([]string, 0, len(legalActions))
	for _, legalAction := range legalActions {
		actionTypes = append(actionTypes, string(legalAction.Type))
	}
	resolution := tablecustody.BuildTimeoutResolution(timeoutPolicyFromState(table.LatestCustodyState), actingPlayerID, actionTypes, []string{actingPlayerID})
	if resolution.ActionType == string(game.ActionCheck) {
		return nil, nil
	}
	nextHand, err := game.ApplyHoldemAction(hand, actingSeatIndex, game.Action{Type: game.ActionFold})
	if err != nil {
		return nil, err
	}
	transition, err := runtime.buildCustodyTransition(table, tablecustody.TransitionKindTimeout, &nextHand, &game.Action{Type: game.ActionFold}, &resolution)
	if err != nil {
		return nil, err
	}
	return &transition, nil
}

func (runtime *meshRuntime) buildShowdownRecoveryTransition(table nativeTableState, hand game.HoldemState) (*tablecustody.CustodyTransition, error) {
	if hand.Phase != game.StreetSettled {
		return nil, nil
	}
	timeoutResolution := latestTimeoutResolutionForHand(table)
	overrides := (*custodyBindingOverrides)(nil)
	if derived := runtime.showdownPayoutTimeoutResolution(table, timeoutResolution); derived != nil {
		timeoutResolution = derived
		overrides = &custodyBindingOverrides{
			ActionDeadlineAt: table.LatestCustodyState.ActionDeadlineAt,
		}
	}
	transition, err := runtime.buildCustodyTransitionWithOverrides(table, tablecustody.TransitionKindShowdownPayout, &hand, nil, timeoutResolution, overrides)
	if err != nil {
		return nil, err
	}
	return &transition, nil
}

func showdownRevealTimeoutResolution(table nativeTableState, seatIndex int, reason string) *tablecustody.TimeoutResolution {
	if reason == "" {
		reason = "protocol timeout during showdown-reveal"
	}
	playerID := seatPlayerID(table, seatIndex)
	return &tablecustody.TimeoutResolution{
		ActionType:               string(game.ActionFold),
		ActingPlayerID:           playerID,
		DeadPlayerIDs:            []string{playerID},
		LostEligibilityPlayerIDs: []string{playerID},
		Policy:                   timeoutPolicyFromState(table.LatestCustodyState),
		Reason:                   reason,
	}
}

func (runtime *meshRuntime) buildShowdownRevealRecoveryTransitions(table nativeTableState, hand game.HoldemState) ([]tablecustody.CustodyTransition, error) {
	if hand.Phase != game.StreetShowdownReveal {
		return nil, nil
	}
	if table.ActiveHand == nil {
		return nil, nil
	}
	transitions := make([]tablecustody.CustodyTransition, 0, len(liveShowdownSeatIndexes(table)))
	for _, seatIndex := range liveShowdownSeatIndexes(table) {
		nextHand, err := game.ForceFoldSeat(hand, seatIndex)
		if err != nil {
			return nil, err
		}
		postTable := cloneJSON(table)
		if postTable.ActiveHand != nil {
			postTable.ActiveHand.State = cloneJSON(nextHand)
		}
		publicState := runtime.publicStateFromHand(postTable, nextHand)
		postTable.PublicState = &publicState
		transition, err := runtime.buildCustodyTransition(postTable, tablecustody.TransitionKindShowdownPayout, &nextHand, nil, showdownRevealTimeoutResolution(postTable, seatIndex, "protocol timeout during showdown-reveal"))
		if err != nil {
			return nil, err
		}
		transitions = append(transitions, transition)
	}
	return transitions, nil
}

func recoveryBundleTimeoutEquivalent(left, right *tablecustody.TimeoutResolution) bool {
	if left == nil && right == nil {
		return true
	}
	if left == nil || right == nil {
		return false
	}
	normalizedLeft := tablecustody.HashValue(map[string]any{
		"actionType":               left.ActionType,
		"actingPlayerId":           left.ActingPlayerID,
		"deadPlayerIds":            append([]string(nil), left.DeadPlayerIDs...),
		"lostEligibilityPlayerIds": append([]string(nil), left.LostEligibilityPlayerIDs...),
		"policy":                   left.Policy,
		"reason":                   left.Reason,
	})
	normalizedRight := tablecustody.HashValue(map[string]any{
		"actionType":               right.ActionType,
		"actingPlayerId":           right.ActingPlayerID,
		"deadPlayerIds":            append([]string(nil), right.DeadPlayerIDs...),
		"lostEligibilityPlayerIds": append([]string(nil), right.LostEligibilityPlayerIDs...),
		"policy":                   right.Policy,
		"reason":                   right.Reason,
	})
	return normalizedLeft == normalizedRight
}

func recoveryOutputsFromBundle(bundle tablecustody.CustodyRecoveryBundle) []custodyBatchOutput {
	outputs := make([]custodyBatchOutput, 0, len(bundle.AuthorizedOutputs))
	for _, output := range bundle.AuthorizedOutputs {
		outputs = append(outputs, custodyBatchOutput{
			AmountSats:    output.AmountSats,
			OwnerPlayerID: output.OwnerPlayerID,
			Script:        output.Script,
			Tapscripts:    append([]string(nil), output.Tapscripts...),
		})
	}
	return outputs
}

func canonicalRecoveryAuthorizedOutputs(outputs []custodyBatchOutput) []custodyBatchOutput {
	canonical := append([]custodyBatchOutput(nil), outputs...)
	sort.SliceStable(canonical, func(left, right int) bool {
		switch {
		case canonical[left].OwnerPlayerID != canonical[right].OwnerPlayerID:
			return canonical[left].OwnerPlayerID < canonical[right].OwnerPlayerID
		case canonical[left].AmountSats != canonical[right].AmountSats:
			return canonical[left].AmountSats < canonical[right].AmountSats
		default:
			return canonical[left].Script < canonical[right].Script
		}
	})
	for index := range canonical {
		canonical[index].ClaimKey = ""
		canonical[index].Onchain = false
		canonical[index].Tapscripts = append([]string(nil), canonical[index].Tapscripts...)
		sort.Strings(canonical[index].Tapscripts)
	}
	return canonical
}

func sameRecoveryAuthorizedOutputs(left []custodyBatchOutput, right []custodyBatchOutput) bool {
	return reflect.DeepEqual(canonicalRecoveryAuthorizedOutputs(left), canonicalRecoveryAuthorizedOutputs(right))
}

func (runtime *meshRuntime) selectPotCSVExitSpendPath(table nativeTableState, ref tablecustody.VTXORef) (custodySpendPath, error) {
	if len(ref.Tapscripts) == 0 {
		return custodySpendPath{}, fmt.Errorf("custody ref %s:%d is missing tapscripts", ref.TxID, ref.VOut)
	}
	vtxoScript, err := arkscript.ParseVtxoScript(ref.Tapscripts)
	if err != nil {
		return custodySpendPath{}, err
	}
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
		signerXOnlyPubkeys = uniqueNonEmptyStrings(signerXOnlyPubkeys)
		return custodySpendPath{
			LeafProof:          leafProof,
			PKScript:           pkScript,
			PlayerIDs:          uniqueSortedPlayerIDs(playerIDs),
			SignerXOnlyPubkeys: signerXOnlyPubkeys,
			Script:             scriptBytes,
			Tapscripts:         append([]string(nil), ref.Tapscripts...),
			UsesCSVLocktime:    true,
			CSVLocktime:        csvClosure.Locktime,
		}, nil
	}
	return custodySpendPath{}, fmt.Errorf("no CSV custody exit leaf found for %s:%d", ref.TxID, ref.VOut)
}

func recoveryUnsignedPSBT(sourceRefs []tablecustody.VTXORef, spendPaths []custodySpendPath, outputs []custodyBatchOutput) (string, error) {
	if len(sourceRefs) == 0 || len(sourceRefs) != len(spendPaths) {
		return "", errors.New("custody recovery inputs are incomplete")
	}
	ins := make([]*wire.OutPoint, 0, len(sourceRefs))
	sequences := make([]uint32, 0, len(sourceRefs))
	txOuts := make([]*wire.TxOut, 0, len(outputs)+1)
	for _, output := range outputs {
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
		sequence, err := arklib.BIP68Sequence(spendPaths[index].CSVLocktime)
		if err != nil {
			return "", err
		}
		sequences = append(sequences, sequence)
	}
	packet, err := psbt.New(ins, txOuts, 3, 0, sequences)
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

func recoveryBundleEarliestExecuteAt(finalizedAt string, spendPaths []custodySpendPath) string {
	if strings.TrimSpace(finalizedAt) == "" || len(spendPaths) == 0 {
		return ""
	}
	maxDelaySeconds := int64(0)
	for _, spendPath := range spendPaths {
		if !spendPath.UsesCSVLocktime || spendPath.CSVLocktime.Type != arklib.LocktimeTypeSecond {
			return ""
		}
		if spendPath.CSVLocktime.Seconds() > maxDelaySeconds {
			maxDelaySeconds = spendPath.CSVLocktime.Seconds()
		}
	}
	if maxDelaySeconds == 0 {
		return finalizedAt
	}
	return addMillis(finalizedAt, int(maxDelaySeconds*1000))
}

func recoveryBundleHash(bundle tablecustody.CustodyRecoveryBundle) string {
	return tablecustody.HashCustodyRecoveryBundle(bundle)
}

func recoveryOutputsToProof(outputs []custodyBatchOutput) []tablecustody.CustodyRecoveryOutput {
	recoveryOutputs := make([]tablecustody.CustodyRecoveryOutput, 0, len(outputs))
	for _, output := range outputs {
		recoveryOutputs = append(recoveryOutputs, tablecustody.CustodyRecoveryOutput{
			AmountSats:    output.AmountSats,
			OwnerPlayerID: output.OwnerPlayerID,
			Script:        output.Script,
			Tapscripts:    append([]string(nil), output.Tapscripts...),
		})
	}
	return recoveryOutputs
}

func (runtime *meshRuntime) buildRecoveryBundle(table nativeTableState, sourceTransition tablecustody.CustodyTransition, target tablecustody.CustodyTransition, sourceAuthorizer *nativeTransitionAuthorizer, outputs []custodyBatchOutput) (*tablecustody.CustodyRecoveryBundle, error) {
	sourceRefs := sourcePotRecoveryRefs(&sourceTransition.NextState)
	if len(sourceRefs) == 0 || len(outputs) == 0 {
		return nil, nil
	}
	spendPaths := make([]custodySpendPath, 0, len(sourceRefs))
	recoverySignerSet := map[string]struct{}{}
	for _, ref := range sourceRefs {
		spendPath, err := runtime.selectPotCSVExitSpendPath(table, ref)
		if err != nil {
			return nil, err
		}
		spendPaths = append(spendPaths, spendPath)
		for _, playerID := range spendPath.PlayerIDs {
			recoverySignerSet[playerID] = struct{}{}
		}
	}
	unsigned, err := recoveryUnsignedPSBT(sourceRefs, spendPaths, outputs)
	if err != nil {
		return nil, err
	}
	recoverySigners := make([]string, 0, len(recoverySignerSet))
	for playerID := range recoverySignerSet {
		recoverySigners = append(recoverySigners, playerID)
	}
	sort.Strings(recoverySigners)
	requestHash := strings.TrimSpace(sourceTransition.Proof.RequestHash)
	if requestHash == "" {
		requestHash = custodyTransitionRequestHash(sourceTransition)
	}
	signedPSBT, err := runtime.fullySignCustodyPSBT(table, sourceTransition.PrevStateHash, requestHash, "recovery", recoverySigners, unsigned, sourceTransition, sourceAuthorizer, outputs)
	if err != nil {
		return nil, err
	}
	bundle := &tablecustody.CustodyRecoveryBundle{
		AuthorizedOutputs: recoveryOutputsToProof(outputs),
		EarliestExecuteAt: recoveryBundleEarliestExecuteAt(sourceTransition.Proof.FinalizedAt, spendPaths),
		Kind:              target.Kind,
		SignedPSBT:        signedPSBT,
		SourcePotRefs:     append([]tablecustody.VTXORef(nil), sourceRefs...),
		TimeoutResolution: cloneTimeoutResolution(target.TimeoutResolution),
	}
	bundle.BundleHash = recoveryBundleHash(*bundle)
	return bundle, nil
}

func (runtime *meshRuntime) deterministicRecoveryTargetsForTransition(table nativeTableState, transition tablecustody.CustodyTransition, postHand *game.HoldemState) ([]tablecustody.CustodyTransition, error) {
	if !custodyRecoverySupportedTable(table) || postHand == nil {
		return nil, nil
	}
	postTable := cloneJSON(table)
	nextState := cloneJSON(transition.NextState)
	postTable.LatestCustodyState = &nextState
	if postTable.ActiveHand != nil {
		postTable.ActiveHand.State = cloneJSON(*postHand)
	}
	publicState := runtime.publicStateFromHand(postTable, *postHand)
	postTable.PublicState = &publicState

	targets := make([]tablecustody.CustodyTransition, 0, 2)
	if timeoutTransition, err := runtime.buildActionTimeoutRecoveryTransition(postTable, *postHand); err != nil {
		return nil, err
	} else if timeoutTransition != nil {
		targets = append(targets, *timeoutTransition)
	}
	if showdownRevealTransitions, err := runtime.buildShowdownRevealRecoveryTransitions(postTable, *postHand); err != nil {
		return nil, err
	} else {
		targets = append(targets, showdownRevealTransitions...)
	}
	if showdownTransition, err := runtime.buildShowdownRecoveryTransition(postTable, *postHand); err != nil {
		return nil, err
	} else if showdownTransition != nil {
		targets = append(targets, *showdownTransition)
	}
	return targets, nil
}

func (runtime *meshRuntime) attachDeterministicRecoveryBundles(table nativeTableState, transition *tablecustody.CustodyTransition, sourceAuthorizer *nativeTransitionAuthorizer, postHand *game.HoldemState) error {
	if runtime.config.UseMockSettlement || transition == nil {
		return nil
	}
	targets, err := runtime.deterministicRecoveryTargetsForTransition(table, *transition, postHand)
	if err != nil {
		return err
	}
	bundles := make([]tablecustody.CustodyRecoveryBundle, 0, len(targets))
	for _, target := range targets {
		outputs, err := runtime.recoveryAuthorizedOutputsForTransition(table, &transition.NextState, target)
		if err != nil {
			return err
		}
		if len(outputs) == 0 {
			continue
		}
		bundle, err := runtime.buildRecoveryBundle(table, *transition, target, sourceAuthorizer, outputs)
		if err != nil {
			return err
		}
		if bundle == nil {
			continue
		}
		bundles = append(bundles, *bundle)
	}
	if len(bundles) == 0 {
		return nil
	}
	transition.Proof.RecoveryBundles = append([]tablecustody.CustodyRecoveryBundle(nil), bundles...)
	transition.Proof.TransitionHash = tablecustody.HashCustodyTransition(*transition)
	return tablecustody.ValidateTransition(table.LatestCustodyState, *transition)
}

func validateCustodyRecoveryPSBT(packet *psbt.Packet, sourceRefs []tablecustody.VTXORef, spendPaths []custodySpendPath, authorizedOutputs []custodyBatchOutput) error {
	if packet == nil {
		return errors.New("custody recovery psbt is missing")
	}
	if len(sourceRefs) == 0 || len(sourceRefs) != len(spendPaths) {
		return errors.New("custody recovery inputs are incomplete")
	}
	if len(packet.UnsignedTx.TxIn) != len(sourceRefs) || len(packet.Inputs) != len(sourceRefs) {
		return errors.New("custody recovery psbt input set does not match the authorized source refs")
	}
	expectedInputs := map[string]int{}
	for index, ref := range sourceRefs {
		expectedInputs[custodyInputAuthorizationKey(custodyInputSpec{Ref: ref, SpendPath: spendPaths[index]})]++
	}
	for index, txIn := range packet.UnsignedTx.TxIn {
		input := packet.Inputs[index]
		if input.WitnessUtxo == nil {
			return errors.New("custody recovery psbt is missing witness utxo metadata")
		}
		actualKey := fmt.Sprintf(
			"%s:%d|%d|%s",
			txIn.PreviousOutPoint.Hash.String(),
			txIn.PreviousOutPoint.Index,
			input.WitnessUtxo.Value,
			hex.EncodeToString(input.WitnessUtxo.PkScript),
		)
		if expectedInputs[actualKey] == 0 {
			return fmt.Errorf("custody recovery psbt input %s is not authorized by the source pots", actualKey)
		}
		expectedInputs[actualKey]--
		if len(input.TaprootLeafScript) != 1 {
			return fmt.Errorf("custody recovery psbt input %d is missing its csv leaf proof", index)
		}
		if !bytes.Equal(input.TaprootLeafScript[0].Script, spendPaths[index].Script) {
			return fmt.Errorf("custody recovery psbt input %d does not use the authorized csv leaf", index)
		}
		if sequence, err := arklib.BIP68Sequence(spendPaths[index].CSVLocktime); err != nil {
			return err
		} else if packet.UnsignedTx.TxIn[index].Sequence != sequence {
			return fmt.Errorf("custody recovery psbt input %d sequence does not match the authorized csv delay", index)
		}
	}
	for key, count := range expectedInputs {
		if count != 0 {
			return fmt.Errorf("custody recovery psbt is missing authorized input %s", key)
		}
	}
	if packet.UnsignedTx.LockTime != 0 {
		return errors.New("custody recovery psbt has an unexpected absolute locktime")
	}
	expectedOutputs := map[string]int{}
	for _, output := range authorizedOutputs {
		txOut, err := decodeBatchOutputTxOut(output)
		if err != nil {
			return err
		}
		expectedOutputs[custodyOutputAuthorizationKey(txOut.Value, txOut.PkScript)]++
	}
	anchorCount := 0
	if len(packet.UnsignedTx.TxOut) != len(authorizedOutputs)+1 {
		return errors.New("custody recovery psbt output set does not match the authorized recovery outputs")
	}
	for _, txOut := range packet.UnsignedTx.TxOut {
		if bytes.Equal(txOut.PkScript, arktxutils.ANCHOR_PKSCRIPT) && txOut.Value == arktxutils.ANCHOR_VALUE {
			anchorCount++
			continue
		}
		key := custodyOutputAuthorizationKey(txOut.Value, txOut.PkScript)
		if expectedOutputs[key] == 0 {
			return fmt.Errorf("custody recovery psbt output %s is not authorized", key)
		}
		expectedOutputs[key]--
	}
	if anchorCount != 1 {
		return errors.New("custody recovery psbt is missing the anchor output")
	}
	for key, count := range expectedOutputs {
		if count != 0 {
			return fmt.Errorf("custody recovery psbt is missing authorized output %s", key)
		}
	}
	return nil
}

func signedRecoveryPSBTInputsComplete(packet *psbt.Packet, spendPaths []custodySpendPath) bool {
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

func finalizedRecoveryTxFromPacket(packet *psbt.Packet) (*wire.MsgTx, error) {
	if packet == nil {
		return nil, errors.New("custody recovery psbt is missing")
	}
	recoveryTx := packet.UnsignedTx.Copy()
	for inputIndex, input := range packet.Inputs {
		if len(input.TaprootLeafScript) != 1 {
			return nil, fmt.Errorf("custody recovery psbt input %d is missing its csv leaf proof", inputIndex)
		}
		leafScript := input.TaprootLeafScript[0]
		closure, err := arkscript.DecodeClosure(leafScript.Script)
		if err != nil {
			return nil, err
		}
		csvClosure, ok := closure.(*arkscript.CSVMultisigClosure)
		if !ok {
			return nil, fmt.Errorf("custody recovery psbt input %d does not use a csv multisig leaf", inputIndex)
		}
		leafHash := txscript.NewTapLeaf(leafScript.LeafVersion, leafScript.Script).TapHash()
		signatures := make(map[string][]byte, len(input.TaprootScriptSpendSig))
		for _, signature := range input.TaprootScriptSpendSig {
			if signature == nil || !bytes.Equal(signature.LeafHash, leafHash[:]) {
				return nil, fmt.Errorf("custody recovery psbt input %d contains a signature for the wrong leaf", inputIndex)
			}
			rawSignature := append([]byte(nil), signature.Signature...)
			if signature.SigHash != txscript.SigHashDefault {
				rawSignature = append(rawSignature, byte(signature.SigHash))
			}
			signatures[hex.EncodeToString(signature.XOnlyPubKey)] = rawSignature
		}
		witness, err := csvClosure.Witness(leafScript.ControlBlock, signatures)
		if err != nil {
			return nil, err
		}
		recoveryTx.TxIn[inputIndex].Witness = witness
	}
	return recoveryTx, nil
}

func validateSignedRecoveryPSBT(packet *psbt.Packet, sourceRefs []tablecustody.VTXORef, spendPaths []custodySpendPath) (*wire.MsgTx, error) {
	if packet == nil {
		return nil, errors.New("custody recovery psbt is missing")
	}
	if len(packet.Inputs) != len(sourceRefs) || len(packet.Inputs) != len(spendPaths) {
		return nil, errors.New("custody recovery psbt input set is incomplete")
	}
	recoveryTx, err := finalizedRecoveryTxFromPacket(packet)
	if err != nil {
		return nil, err
	}
	prevOuts := txscript.NewMultiPrevOutFetcher(nil)
	for index, txIn := range recoveryTx.TxIn {
		prevOut := packet.Inputs[index].WitnessUtxo
		if prevOut == nil {
			return nil, fmt.Errorf("custody recovery psbt input %d is missing witness utxo metadata", index)
		}
		prevOuts.AddPrevOut(txIn.PreviousOutPoint, &wire.TxOut{
			PkScript: append([]byte(nil), prevOut.PkScript...),
			Value:    prevOut.Value,
		})
	}
	sigHashes := txscript.NewTxSigHashes(recoveryTx, prevOuts)
	for index := range recoveryTx.TxIn {
		prevOut := prevOuts.FetchPrevOutput(recoveryTx.TxIn[index].PreviousOutPoint)
		if prevOut == nil {
			return nil, fmt.Errorf("custody recovery psbt input %d prevout is unavailable", index)
		}
		vm, err := txscript.NewEngine(
			spendPaths[index].PKScript,
			recoveryTx,
			index,
			txscript.StandardVerifyFlags,
			nil,
			sigHashes,
			prevOut.Value,
			prevOuts,
		)
		if err != nil {
			return nil, fmt.Errorf("custody recovery psbt input %d witness setup failed: %w", index, err)
		}
		if err := vm.Execute(); err != nil {
			return nil, fmt.Errorf("custody recovery psbt input %d witness does not satisfy the shared csv leaf: %w", index, err)
		}
	}
	return recoveryTx, nil
}

func recoveryOutputRefsFromBundle(bundle tablecustody.CustodyRecoveryBundle) (map[string][]tablecustody.VTXORef, string, error) {
	packet, err := psbt.NewFromRawBytes(strings.NewReader(bundle.SignedPSBT), true)
	if err != nil {
		return nil, "", err
	}
	recoveryTx, err := finalizedRecoveryTxFromPacket(packet)
	if err != nil {
		return nil, "", err
	}
	txid := recoveryTx.TxHash().String()
	available := make([]tablecustody.VTXORef, 0, len(recoveryTx.TxOut))
	for outputIndex, txOut := range recoveryTx.TxOut {
		if bytes.Equal(txOut.PkScript, arktxutils.ANCHOR_PKSCRIPT) && txOut.Value == arktxutils.ANCHOR_VALUE {
			continue
		}
		available = append(available, tablecustody.VTXORef{
			AmountSats: int(txOut.Value),
			Script:     hex.EncodeToString(txOut.PkScript),
			TxID:       txid,
			VOut:       uint32(outputIndex),
		})
	}
	used := make([]bool, len(available))
	matched := map[string][]tablecustody.VTXORef{}
	for _, output := range bundle.AuthorizedOutputs {
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
			return nil, "", fmt.Errorf("custody recovery bundle output for %s could not be matched in the signed psbt", output.OwnerPlayerID)
		}
		used[matchIndex] = true
		ref := available[matchIndex]
		ref.OwnerPlayerID = output.OwnerPlayerID
		ref.Tapscripts = append([]string(nil), output.Tapscripts...)
		matched[stackClaimKey(output.OwnerPlayerID)] = append(matched[stackClaimKey(output.OwnerPlayerID)], ref)
	}
	return matched, txid, nil
}

func applyRecoveryBundleToTransition(previous *tablecustody.CustodyState, transition *tablecustody.CustodyTransition, outputRefs map[string][]tablecustody.VTXORef) {
	if transition == nil {
		return
	}
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
	for index := range transition.NextState.StackClaims {
		claim := transition.NextState.StackClaims[index]
		prevClaim, hasPrev := prevStacks[claim.PlayerID]
		extraRefs := outputRefs[stackClaimKey(claim.PlayerID)]
		switch {
		case len(extraRefs) > 0 && hasPrev:
			transition.NextState.StackClaims[index].VTXORefs = append(append([]tablecustody.VTXORef(nil), prevClaim.VTXORefs...), extraRefs...)
		case len(extraRefs) > 0:
			transition.NextState.StackClaims[index].VTXORefs = append([]tablecustody.VTXORef(nil), extraRefs...)
		case hasPrev:
			transition.NextState.StackClaims[index].VTXORefs = append([]tablecustody.VTXORef(nil), prevClaim.VTXORefs...)
		default:
			transition.NextState.StackClaims[index].VTXORefs = nil
		}
	}
	for index := range transition.NextState.PotSlices {
		slice := transition.NextState.PotSlices[index]
		prevSlice, hasPrev := prevPots[slice.PotID]
		if hasPrev && reflect.DeepEqual(comparablePotSlice(prevSlice), comparablePotSlice(slice)) {
			transition.NextState.PotSlices[index].VTXORefs = append([]tablecustody.VTXORef(nil), prevSlice.VTXORefs...)
			continue
		}
		transition.NextState.PotSlices[index].VTXORefs = nil
	}
	transition.NextState.StateHash = tablecustody.HashCustodyState(transition.NextState)
	transition.NextStateHash = transition.NextState.StateHash
}

func (runtime *meshRuntime) matchingStoredRecoveryBundle(table nativeTableState, target tablecustody.CustodyTransition) (*tablecustody.CustodyRecoveryBundle, string, error) {
	if len(table.CustodyTransitions) == 0 || table.LatestCustodyState == nil {
		return nil, "", nil
	}
	sourceTransition := table.CustodyTransitions[len(table.CustodyTransitions)-1]
	sourceTransitionHash := strings.TrimSpace(sourceTransition.Proof.TransitionHash)
	if sourceTransitionHash == "" {
		sourceTransitionHash = tablecustody.HashCustodyTransition(sourceTransition)
	}
	outputs, err := runtime.recoveryAuthorizedOutputsForTransition(table, table.LatestCustodyState, target)
	if err != nil {
		return nil, "", err
	}
	if len(outputs) == 0 {
		return nil, "", nil
	}
	sourceRefs := sourcePotRecoveryRefs(table.LatestCustodyState)
	for _, bundle := range sourceTransition.Proof.RecoveryBundles {
		if bundle.Kind != target.Kind {
			continue
		}
		if !recoveryBundleTimeoutEquivalent(bundle.TimeoutResolution, target.TimeoutResolution) {
			continue
		}
		if !sameCanonicalVTXORefs(bundle.SourcePotRefs, sourceRefs) {
			continue
		}
		if !sameRecoveryAuthorizedOutputs(recoveryOutputsFromBundle(bundle), outputs) {
			continue
		}
		candidate := bundle
		return &candidate, sourceTransitionHash, nil
	}
	return nil, "", nil
}

func (runtime *meshRuntime) recoveryBundleForTransition(table nativeTableState, target tablecustody.CustodyTransition) (*tablecustody.CustodyRecoveryBundle, string, error) {
	if bundle, sourceTransitionHash, err := runtime.matchingStoredRecoveryBundle(table, target); err != nil {
		return nil, "", err
	} else if bundle != nil {
		return bundle, sourceTransitionHash, nil
	}
	return nil, "", nil
}

func (runtime *meshRuntime) validateStoredRecoveryBundle(table nativeTableState, bundle tablecustody.CustodyRecoveryBundle) error {
	spendPaths := make([]custodySpendPath, 0, len(bundle.SourcePotRefs))
	for _, ref := range bundle.SourcePotRefs {
		spendPath, err := runtime.selectPotCSVExitSpendPath(table, ref)
		if err != nil {
			return err
		}
		spendPaths = append(spendPaths, spendPath)
	}
	packet, err := psbt.NewFromRawBytes(strings.NewReader(bundle.SignedPSBT), true)
	if err != nil {
		return err
	}
	if err := validateCustodyRecoveryPSBT(packet, bundle.SourcePotRefs, spendPaths, recoveryOutputsFromBundle(bundle)); err != nil {
		return err
	}
	if !signedRecoveryPSBTInputsComplete(packet, spendPaths) {
		return errors.New("custody recovery bundle is not fully signed")
	}
	if _, err := validateSignedRecoveryPSBT(packet, bundle.SourcePotRefs, spendPaths); err != nil {
		return err
	}
	if expected := recoveryBundleHash(bundle); bundle.BundleHash != "" && bundle.BundleHash != expected {
		return errors.New("custody recovery bundle hash mismatch")
	}
	return nil
}

func recoveryBundleReady(bundle *tablecustody.CustodyRecoveryBundle) bool {
	if bundle == nil || strings.TrimSpace(bundle.EarliestExecuteAt) == "" {
		return bundle != nil
	}
	return elapsedMillis(bundle.EarliestExecuteAt) >= 0
}

func uniqueNonEmptyStrings(values []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		if _, ok := seen[trimmed]; ok {
			continue
		}
		seen[trimmed] = struct{}{}
		out = append(out, trimmed)
	}
	sort.Strings(out)
	return out
}

func (runtime *meshRuntime) finalizeCustodyRecoveryTransition(table *nativeTableState, transition *tablecustody.CustodyTransition, authorizer *nativeTransitionAuthorizer) (bool, error) {
	if runtime.config.UseMockSettlement || table == nil || transition == nil {
		return false, nil
	}

	bundle, sourceTransitionHash, err := runtime.recoveryBundleForTransition(*table, *transition)
	if err != nil || bundle == nil {
		return false, err
	}
	if err := runtime.validateStoredRecoveryBundle(*table, *bundle); err != nil {
		return false, err
	}
	if !recoveryBundleReady(bundle) {
		return false, nil
	}

	approvalTransition, _, err := runtime.normalizedCustodyApprovalTransition(*table, *transition)
	if err != nil {
		return false, err
	}
	approvals, err := runtime.collectCustodyApprovals(*table, approvalTransition, authorizer, runtime.requiredCustodySigners(*table, approvalTransition))
	if err != nil {
		return false, err
	}

	outputRefs, recoveryTxID, err := recoveryOutputRefsFromBundle(*bundle)
	if err != nil {
		return false, err
	}
	execution, err := runtime.executeLocalCustodyRecovery(bundle.SignedPSBT)
	if err != nil {
		return false, err
	}
	if execution.RecoveryTxID != "" && execution.RecoveryTxID != recoveryTxID {
		return false, errors.New("custody recovery execution txid mismatch")
	}

	applyRecoveryBundleToTransition(table.LatestCustodyState, transition, outputRefs)
	finalizedAt := nowISO()
	broadcastTxIDs := uniqueNonEmptyStrings(append(append([]string(nil), execution.BroadcastTxIDs...), recoveryTxID))
	transition.Approvals = append([]tablecustody.CustodySignature(nil), approvals...)
	transition.Proof = tablecustody.CustodyProof{
		FinalizedAt:     finalizedAt,
		RequestHash:     approvalTransition.Proof.RequestHash,
		ReplayValidated: true,
		RecoveryWitness: &tablecustody.CustodyRecoveryWitness{
			BroadcastTxIDs:       broadcastTxIDs,
			BundleHash:           bundle.BundleHash,
			ExecutedAt:           finalizedAt,
			RecoveryTxID:         recoveryTxID,
			SourceTransitionHash: sourceTransitionHash,
		},
		Signatures: append([]tablecustody.CustodySignature(nil), approvals...),
		StateHash:  transition.NextStateHash,
		VTXORefs:   stackProofRefs(transition.NextState),
	}
	transition.Proof.TransitionHash = tablecustody.HashCustodyTransition(*transition)
	return true, tablecustody.ValidateTransition(table.LatestCustodyState, *transition)
}

func (runtime *meshRuntime) validateAcceptedCustodyRecoveryWitness(table nativeTableState, previous *tablecustody.CustodyState, transition tablecustody.CustodyTransition) error {
	witness := transition.Proof.RecoveryWitness
	if witness == nil {
		return errors.New("custody recovery witness is missing")
	}
	var bundle *tablecustody.CustodyRecoveryBundle
	if strings.TrimSpace(witness.SourceTransitionHash) != "" {
		for _, candidate := range table.CustodyTransitions {
			if candidate.Proof.TransitionHash != witness.SourceTransitionHash {
				continue
			}
			for _, storedBundle := range candidate.Proof.RecoveryBundles {
				if storedBundle.BundleHash != witness.BundleHash {
					continue
				}
				cloned := storedBundle
				bundle = &cloned
				break
			}
			break
		}
	} else {
		for _, storedBundle := range transition.Proof.RecoveryBundles {
			if storedBundle.BundleHash != witness.BundleHash {
				continue
			}
			cloned := storedBundle
			bundle = &cloned
			break
		}
	}
	if bundle == nil {
		return errors.New("custody recovery witness bundle is missing")
	}
	if err := runtime.validateStoredRecoveryBundle(table, *bundle); err != nil {
		return err
	}
	outputRefs, recoveryTxID, err := recoveryOutputRefsFromBundle(*bundle)
	if err != nil {
		return err
	}
	if witness.RecoveryTxID != recoveryTxID {
		return errors.New("custody recovery witness txid mismatch")
	}
	foundRecoveryTx := false
	for _, txid := range witness.BroadcastTxIDs {
		if txid == recoveryTxID {
			foundRecoveryTx = true
			break
		}
	}
	if !foundRecoveryTx {
		return errors.New("custody recovery witness broadcast metadata is missing the recovery transaction")
	}
	expected := cloneJSON(transition)
	applyRecoveryBundleToTransition(previous, &expected, outputRefs)
	if !reflect.DeepEqual(canonicalCustodyMoneyStacks(&expected.NextState), canonicalCustodyMoneyStacks(&transition.NextState)) {
		return errors.New("recovery-derived stack refs do not match the accepted next state")
	}
	if !reflect.DeepEqual(canonicalCustodyMoneyPots(&expected.NextState), canonicalCustodyMoneyPots(&transition.NextState)) {
		return errors.New("recovery-derived pot refs do not match the accepted next state")
	}
	if !sameCanonicalVTXORefs(stackProofRefs(expected.NextState), transition.Proof.VTXORefs) {
		return errors.New("recovery-derived proof refs do not match the accepted transition")
	}
	return nil
}
