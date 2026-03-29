package meshruntime

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"reflect"
	"strings"
	"time"

	arkscript "github.com/arkade-os/arkd/pkg/ark-lib/script"
	arkindexer "github.com/arkade-os/go-sdk/indexer"
	arkgrpcindexer "github.com/arkade-os/go-sdk/indexer/grpc"
	sdktypes "github.com/arkade-os/go-sdk/types"
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
			return fmt.Errorf("custody ref %s Ark tx mismatch", key)
		}
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
		if strings.TrimSpace(ref.ArkTxID) == "" || ref.ArkTxID != transition.ArkTxID {
			return fmt.Errorf("custody ref %s is not anchored to the transition Ark tx", key)
		}
	}
	return nil
}
