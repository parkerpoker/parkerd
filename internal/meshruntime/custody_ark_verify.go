package meshruntime

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	arkscript "github.com/arkade-os/arkd/pkg/ark-lib/script"
	arktree "github.com/arkade-os/arkd/pkg/ark-lib/tree"
	arkindexer "github.com/arkade-os/go-sdk/indexer"
	arkgrpcindexer "github.com/arkade-os/go-sdk/indexer/grpc"
	sdktypes "github.com/arkade-os/go-sdk/types"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/parkerpoker/parkerd/internal/tablecustody"
)

func previousCustodyRefSet(state *tablecustody.CustodyState) map[string]tablecustody.VTXORef {
	refs := map[string]tablecustody.VTXORef{}
	if state == nil {
		return refs
	}
	for _, claim := range state.StackClaims {
		for _, ref := range claim.VTXORefs {
			refs[fundingRefKey(ref)] = ref
		}
	}
	for _, slice := range state.PotSlices {
		for _, ref := range slice.VTXORefs {
			refs[fundingRefKey(ref)] = ref
		}
	}
	return refs
}

func nextCustodyRefs(transition tablecustody.CustodyTransition) []tablecustody.VTXORef {
	refs := make([]tablecustody.VTXORef, 0)
	for _, claim := range transition.NextState.StackClaims {
		refs = append(refs, claim.VTXORefs...)
	}
	for _, slice := range transition.NextState.PotSlices {
		refs = append(refs, slice.VTXORefs...)
	}
	return refs
}

func uniqueCustodyRefs(refs ...[]tablecustody.VTXORef) []tablecustody.VTXORef {
	ordered := make([]tablecustody.VTXORef, 0)
	seen := map[string]tablecustody.VTXORef{}
	for _, group := range refs {
		for _, ref := range group {
			key := fundingRefKey(ref)
			if existing, ok := seen[key]; ok {
				if !reflect.DeepEqual(existing, ref) {
					// Let the structural validator surface the inconsistent duplicate.
					continue
				}
				continue
			}
			seen[key] = ref
			ordered = append(ordered, ref)
		}
	}
	return ordered
}

func indexedOutpointKey(outpoint sdktypes.Outpoint) string {
	return fmt.Sprintf("%s:%d", outpoint.Txid, outpoint.VOut)
}

func decodeCustodyScriptHex(value string) ([]byte, error) {
	decoded, err := hex.DecodeString(strings.TrimSpace(value))
	if err != nil {
		return nil, err
	}
	return decoded, nil
}

func verifyCustodyTaprootBinding(ref tablecustody.VTXORef, indexedScript string) error {
	if len(ref.Tapscripts) == 0 {
		return nil
	}

	vtxoScript, err := arkscript.ParseVtxoScript(ref.Tapscripts)
	if err != nil {
		return fmt.Errorf("custody ref %s tapscripts are invalid: %w", fundingRefKey(ref), err)
	}
	tapKey, _, err := vtxoScript.TapTree()
	if err != nil {
		return fmt.Errorf("custody ref %s tapscript tree is invalid: %w", fundingRefKey(ref), err)
	}
	expectedScript, err := arkscript.P2TRScript(tapKey)
	if err != nil {
		return fmt.Errorf("custody ref %s taproot script derivation failed: %w", fundingRefKey(ref), err)
	}

	if strings.TrimSpace(ref.Script) != "" {
		declaredScript, err := decodeCustodyScriptHex(ref.Script)
		if err != nil {
			return fmt.Errorf("custody ref %s script is not valid hex: %w", fundingRefKey(ref), err)
		}
		if !bytes.Equal(declaredScript, expectedScript) {
			return fmt.Errorf("custody ref %s tapscripts do not match the declared output script", fundingRefKey(ref))
		}
	}
	if strings.TrimSpace(indexedScript) != "" {
		onArkScript, err := decodeCustodyScriptHex(indexedScript)
		if err != nil {
			return fmt.Errorf("custody ref %s indexed script is not valid hex: %w", fundingRefKey(ref), err)
		}
		if !bytes.Equal(onArkScript, expectedScript) {
			return fmt.Errorf("custody ref %s tapscripts do not match the indexed Ark output script", fundingRefKey(ref))
		}
	}
	return nil
}

func (runtime *meshRuntime) verifyCustodyRefsOnArk(refs []tablecustody.VTXORef, requireSpendable bool) error {
	if runtime.config.UseMockSettlement || len(refs) == 0 {
		return nil
	}
	for _, ref := range refs {
		if err := verifyCustodyTaprootBinding(ref, ""); err != nil {
			return err
		}
	}
	if runtime.custodyArkVerify != nil {
		return runtime.custodyArkVerify(refs, requireSpendable)
	}

	indexerClient, err := arkgrpcindexer.NewClient(runtime.config.ArkServerURL)
	if err != nil {
		return err
	}
	defer indexerClient.Close()

	outpoints := make([]sdktypes.Outpoint, 0, len(refs))
	for _, ref := range refs {
		outpoints = append(outpoints, sdktypes.Outpoint{Txid: ref.TxID, VOut: ref.VOut})
	}
	options := arkindexer.GetVtxosRequestOption{}
	if err := options.WithOutpoints(outpoints); err != nil {
		return err
	}
	if requireSpendable {
		options.WithSpendableOnly()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	response, err := indexerClient.GetVtxos(ctx, options)
	if err != nil {
		return err
	}
	indexed := map[string]sdktypes.Vtxo{}
	if response != nil {
		for _, vtxo := range response.Vtxos {
			indexed[indexedOutpointKey(vtxo.Outpoint)] = vtxo
		}
	}
	for _, ref := range refs {
		key := fundingRefKey(ref)
		vtxo, ok := indexed[key]
		if !ok {
			return fmt.Errorf("custody ref %s is not indexed on Ark", key)
		}
		if requireSpendable && vtxo.Spent {
			return fmt.Errorf("custody ref %s is no longer spendable on Ark", key)
		}
		if int(vtxo.Amount) != ref.AmountSats {
			return fmt.Errorf("custody ref %s amount mismatch", key)
		}
		if err := verifyCustodyTaprootBinding(ref, vtxo.Script); err != nil {
			return err
		}
		if strings.TrimSpace(ref.ArkTxID) != "" && vtxo.ArkTxid != ref.ArkTxID {
			return fmt.Errorf("custody ref %s Ark tx mismatch: ref=%s indexed=%s", key, ref.ArkTxID, vtxo.ArkTxid)
		}
	}
	return nil
}

func settlementProofRefs(nextState tablecustody.CustodyState, outputRefs map[string][]tablecustody.VTXORef) []tablecustody.VTXORef {
	refs := stackProofRefs(nextState)
	extraKeys := make([]string, 0)
	for claimKey := range outputRefs {
		if strings.HasPrefix(claimKey, "stack:") || strings.HasPrefix(claimKey, "pot:") {
			continue
		}
		extraKeys = append(extraKeys, claimKey)
	}
	sort.Strings(extraKeys)
	for _, claimKey := range extraKeys {
		refs = append(refs, outputRefs[claimKey]...)
	}
	return refs
}

func validateCustodySettlementWitnessSummary(transition tablecustody.CustodyTransition, witness *tablecustody.CustodySettlementWitness) error {
	if witness == nil {
		return errors.New("custody settlement witness is missing")
	}
	if strings.TrimSpace(witness.ArkIntentID) == "" {
		return errors.New("custody settlement witness is missing Ark intent id")
	}
	if strings.TrimSpace(witness.ArkTxID) == "" {
		return errors.New("custody settlement witness is missing Ark txid")
	}
	if strings.TrimSpace(witness.FinalizedAt) == "" {
		return errors.New("custody settlement witness is missing finalized timestamp")
	}
	if transition.ArkIntentID != "" && witness.ArkIntentID != transition.ArkIntentID {
		return errors.New("custody settlement witness intent id mismatch")
	}
	if transition.Proof.ArkIntentID != "" && witness.ArkIntentID != transition.Proof.ArkIntentID {
		return errors.New("custody proof witness intent id mismatch")
	}
	if transition.ArkTxID != "" && witness.ArkTxID != transition.ArkTxID {
		return errors.New("custody settlement witness txid mismatch")
	}
	if transition.Proof.ArkTxID != "" && witness.ArkTxID != transition.Proof.ArkTxID {
		return errors.New("custody proof witness txid mismatch")
	}
	if transition.Proof.FinalizedAt != "" && witness.FinalizedAt != transition.Proof.FinalizedAt {
		return errors.New("custody proof witness finalized timestamp mismatch")
	}
	return nil
}

func validateConnectorTreeWitness(commitmentTxID string, plan *custodySettlementPlan, connectorTree *arktree.TxTree) error {
	if connectorTree == nil {
		return nil
	}
	if err := connectorTree.Validate(); err != nil {
		return err
	}
	rootInput := connectorTree.Root.UnsignedTx.TxIn[0].PreviousOutPoint
	if rootInput.Hash.String() != commitmentTxID {
		return errors.New("custody connector tree root does not spend the commitment transaction")
	}
	if len(plan.Inputs) > 0 && len(connectorTree.Leaves()) != len(plan.Inputs) {
		return fmt.Errorf("custody connector tree leaf count mismatch: expected %d got %d", len(plan.Inputs), len(connectorTree.Leaves()))
	}
	return nil
}

func (runtime *meshRuntime) validateAcceptedCustodySettlementWitness(table nativeTableState, previous *tablecustody.CustodyState, transition tablecustody.CustodyTransition) error {
	witness := transition.Proof.SettlementWitness
	if err := validateCustodySettlementWitnessSummary(transition, witness); err != nil {
		return err
	}
	if strings.TrimSpace(witness.ProofPSBT) == "" {
		return errors.New("custody settlement witness is missing proof psbt")
	}
	if strings.TrimSpace(witness.CommitmentTx) == "" {
		return errors.New("custody settlement witness is missing commitment transaction")
	}
	expiry, err := parseCustodyBatchExpiry(witness.BatchExpiryType, witness.BatchExpiryValue)
	if err != nil {
		return err
	}

	baseTable := cloneJSON(table)
	baseTable.LatestCustodyState = previous
	plan, err := runtime.buildCustodySettlementPlan(baseTable, transition)
	if err != nil {
		return err
	}
	if len(plan.Inputs) == 0 {
		return errors.New("custody settlement witness is not expected for a no-op transition")
	}

	proofPSBT, err := psbt.NewFromRawBytes(strings.NewReader(witness.ProofPSBT), true)
	if err != nil {
		return err
	}
	if err := validateCustodyProofPSBT(proofPSBT, plan, plan.AuthorizedOutputs); err != nil {
		return err
	}

	commitment, err := psbt.NewFromRawBytes(strings.NewReader(witness.CommitmentTx), true)
	if err != nil {
		return err
	}
	if commitment.UnsignedTx.TxID() != witness.ArkTxID {
		return errors.New("custody settlement witness commitment txid mismatch")
	}

	config, err := runtime.arkCustodyConfig()
	if err != nil {
		return err
	}
	forfeitPubkey, err := compressedPubkeyFromHex(config.ForfeitPubkeyHex)
	if err != nil {
		return err
	}

	vtxoTree, err := arktree.NewTxTree(cloneFlatTxTree(witness.VtxoTree))
	if err != nil {
		return err
	}
	if err := arktree.ValidateVtxoTree(vtxoTree, commitment, forfeitPubkey, expiry); err != nil {
		return err
	}

	if len(witness.ConnectorTree) > 0 {
		connectorTree, err := arktree.NewTxTree(cloneFlatTxTree(witness.ConnectorTree))
		if err != nil {
			return err
		}
		if err := validateConnectorTreeWitness(commitment.UnsignedTx.TxID(), plan, connectorTree); err != nil {
			return err
		}
	}

	outputRefs, err := matchCustodyBatchOutputRefs(witness.ArkIntentID, witness.ArkTxID, witness.FinalizedAt, expiry, plan.AuthorizedOutputs, vtxoTree)
	if err != nil {
		return err
	}
	expected := cloneJSON(transition)
	applyTransitionSettlementPlan(&expected, plan, outputRefs)
	if !reflect.DeepEqual(canonicalCustodyMoneyStacks(&expected.NextState), canonicalCustodyMoneyStacks(&transition.NextState)) {
		return errors.New("witness-derived stack refs do not match the accepted next state")
	}
	if !reflect.DeepEqual(canonicalCustodyMoneyPots(&expected.NextState), canonicalCustodyMoneyPots(&transition.NextState)) {
		return errors.New("witness-derived pot refs do not match the accepted next state")
	}
	if !sameCanonicalVTXORefs(settlementProofRefs(expected.NextState, outputRefs), transition.Proof.VTXORefs) {
		return errors.New("witness-derived proof refs do not match the accepted transition")
	}
	return nil
}

func (runtime *meshRuntime) validateCustodyTransitionArkProof(previous *tablecustody.CustodyState, transition tablecustody.CustodyTransition, requireSpendable bool) error {
	if runtime.config.UseMockSettlement {
		return nil
	}

	transitionRefs := uniqueCustodyRefs(nextCustodyRefs(transition), transition.Proof.VTXORefs)
	if err := runtime.verifyCustodyRefsOnArk(transitionRefs, requireSpendable); err != nil {
		return err
	}

	previousRefs := previousCustodyRefSet(previous)
	for _, ref := range nextCustodyRefs(transition) {
		key := fundingRefKey(ref)
		if previousRef, carried := previousRefs[key]; carried && reflect.DeepEqual(previousRef, ref) {
			continue
		}
		if transition.Kind == tablecustody.TransitionKindEmergencyExit {
			continue
		}
		if strings.TrimSpace(ref.ArkIntentID) == "" || ref.ArkIntentID != transition.ArkIntentID {
			return fmt.Errorf("custody ref %s is not anchored to the transition Ark intent", key)
		}
	}
	return nil
}
