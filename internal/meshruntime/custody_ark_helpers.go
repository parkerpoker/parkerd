package meshruntime

import (
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	arklib "github.com/arkade-os/arkd/pkg/ark-lib"
	arkscript "github.com/arkade-os/arkd/pkg/ark-lib/script"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/txscript"
	"github.com/parkerpoker/parkerd/internal/tablecustody"
	walletpkg "github.com/parkerpoker/parkerd/internal/wallet"
)

type custodySpendPath struct {
	LeafProof  *arklib.TaprootMerkleProof
	PKScript   []byte
	PlayerIDs  []string
	Script     []byte
	Tapscripts []string
}

type custodyInputSpec struct {
	ClaimKey      string
	OwnerPlayerID string
	Ref           tablecustody.VTXORef
	SpendPath     custodySpendPath
}

type custodyOutputSpec struct {
	AmountSats    int
	ClaimKey      string
	OwnerPlayerID string
	Script        string
	Tapscripts    []string
}

type custodySettlementPlan struct {
	Inputs         []custodyInputSpec
	Outputs        []custodyOutputSpec
	ProofSignerIDs []string
	TreeSignerIDs  []string
}

func (runtime *meshRuntime) arkCustodyConfig() (walletpkg.CustodyArkConfig, error) {
	return runtime.walletRuntime.ArkConfig(runtime.profileName)
}

func compressedPubkeyFromHex(value string) (*btcec.PublicKey, error) {
	raw, err := hex.DecodeString(strings.TrimSpace(value))
	if err != nil {
		return nil, err
	}
	return btcec.ParsePubKey(raw)
}

func xOnlyPubkeyHexFromCompressed(value string) (string, error) {
	pubkey, err := compressedPubkeyFromHex(value)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(schnorr.SerializePubKey(pubkey)), nil
}

func absoluteLocktimeFromISO(value string) (arklib.AbsoluteLocktime, error) {
	if strings.TrimSpace(value) == "" {
		return 0, errors.New("missing action deadline")
	}
	timestamp, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return 0, err
	}
	return arklib.AbsoluteLocktime(timestamp.Unix()), nil
}

func comparableStackClaim(claim tablecustody.StackClaim) tablecustody.StackClaim {
	claim.VTXORefs = nil
	return claim
}

func comparablePotSlice(slice tablecustody.PotSlice) tablecustody.PotSlice {
	slice.VTXORefs = nil
	return slice
}

func stackClaimKey(playerID string) string {
	return "stack:" + playerID
}

func potClaimKey(potID string) string {
	return "pot:" + potID
}

func (runtime *meshRuntime) stackOutputSpec(table nativeTableState, playerID string, amountSats int) (custodyOutputSpec, error) {
	seat, ok := seatRecordForPlayer(table, playerID)
	if !ok {
		return custodyOutputSpec{}, fmt.Errorf("missing seat for player %s", playerID)
	}
	config, err := runtime.arkCustodyConfig()
	if err != nil {
		return custodyOutputSpec{}, err
	}
	ownerPubkey, err := compressedPubkeyFromHex(seat.WalletPubkeyHex)
	if err != nil {
		return custodyOutputSpec{}, err
	}
	signerPubkey, err := compressedPubkeyFromHex(config.SignerPubkeyHex)
	if err != nil {
		return custodyOutputSpec{}, err
	}
	vtxoScript := arkscript.NewDefaultVtxoScript(ownerPubkey, signerPubkey, config.UnilateralExitDelay)
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
		AmountSats:    amountSats,
		ClaimKey:      stackClaimKey(playerID),
		OwnerPlayerID: playerID,
		Script:        hex.EncodeToString(pkScript),
		Tapscripts:    tapscripts,
	}, nil
}

func uniqueSortedPlayerIDs(values []string) []string {
	seen := map[string]struct{}{}
	ordered := make([]string, 0, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		ordered = append(ordered, value)
	}
	sort.Strings(ordered)
	return ordered
}

func (runtime *meshRuntime) potOutputSpec(table nativeTableState, transition tablecustody.CustodyTransition, slice tablecustody.PotSlice, futureSignerIDs []string) (custodyOutputSpec, error) {
	config, err := runtime.arkCustodyConfig()
	if err != nil {
		return custodyOutputSpec{}, err
	}
	operatorPubkey, err := compressedPubkeyFromHex(config.SignerPubkeyHex)
	if err != nil {
		return custodyOutputSpec{}, err
	}
	playerPubkeys := make([]*btcec.PublicKey, 0, len(futureSignerIDs))
	for _, playerID := range futureSignerIDs {
		seat, ok := seatRecordForPlayer(table, playerID)
		if !ok {
			return custodyOutputSpec{}, fmt.Errorf("missing seat for player %s", playerID)
		}
		pubkey, err := compressedPubkeyFromHex(seat.WalletPubkeyHex)
		if err != nil {
			return custodyOutputSpec{}, err
		}
		playerPubkeys = append(playerPubkeys, pubkey)
	}

	closures := make([]arkscript.Closure, 0, 3)
	collaborativeKeys := append([]*btcec.PublicKey{}, playerPubkeys...)
	collaborativeKeys = append(collaborativeKeys, operatorPubkey)
	closures = append(closures, &arkscript.MultisigClosure{PubKeys: collaborativeKeys})

	if transition.NextState.ActingPlayerID != "" && transition.NextState.ActionDeadlineAt != "" {
		timeoutKeys := make([]*btcec.PublicKey, 0, len(playerPubkeys)+1)
		for _, playerID := range futureSignerIDs {
			if playerID == transition.NextState.ActingPlayerID {
				continue
			}
			seat, ok := seatRecordForPlayer(table, playerID)
			if !ok {
				return custodyOutputSpec{}, fmt.Errorf("missing seat for player %s", playerID)
			}
			pubkey, err := compressedPubkeyFromHex(seat.WalletPubkeyHex)
			if err != nil {
				return custodyOutputSpec{}, err
			}
			timeoutKeys = append(timeoutKeys, pubkey)
		}
		if len(timeoutKeys) > 0 {
			locktime, err := absoluteLocktimeFromISO(transition.NextState.ActionDeadlineAt)
			if err != nil {
				return custodyOutputSpec{}, err
			}
			timeoutKeys = append(timeoutKeys, operatorPubkey)
			closures = append(closures, &arkscript.CLTVMultisigClosure{
				Locktime: locktime,
				MultisigClosure: arkscript.MultisigClosure{
					PubKeys: timeoutKeys,
				},
			})
		}
	}

	exitKeys := make([]*btcec.PublicKey, 0, len(slice.EligiblePlayerIDs))
	for _, playerID := range uniqueSortedPlayerIDs(slice.EligiblePlayerIDs) {
		seat, ok := seatRecordForPlayer(table, playerID)
		if !ok {
			continue
		}
		pubkey, err := compressedPubkeyFromHex(seat.WalletPubkeyHex)
		if err != nil {
			return custodyOutputSpec{}, err
		}
		exitKeys = append(exitKeys, pubkey)
	}
	if len(exitKeys) > 0 {
		closures = append(closures, &arkscript.CSVMultisigClosure{
			Locktime: config.UnilateralExitDelay,
			MultisigClosure: arkscript.MultisigClosure{
				PubKeys: exitKeys,
			},
		})
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
		AmountSats: slice.TotalSats,
		ClaimKey:   potClaimKey(slice.PotID),
		Script:     hex.EncodeToString(pkScript),
		Tapscripts: tapscripts,
	}, nil
}

func (runtime *meshRuntime) playerIDByXOnlyPubkey(table nativeTableState, xOnlyHex string) (string, bool, error) {
	for _, seat := range table.Seats {
		seatXOnly, err := xOnlyPubkeyHexFromCompressed(seat.WalletPubkeyHex)
		if err != nil {
			return "", false, err
		}
		if seatXOnly == xOnlyHex {
			return seat.PlayerID, true, nil
		}
	}
	return "", false, nil
}

func (runtime *meshRuntime) selectCustodySpendPath(table nativeTableState, ref tablecustody.VTXORef, desiredPlayerIDs []string, preferTimeout bool) (custodySpendPath, error) {
	tapscripts := append([]string(nil), ref.Tapscripts...)
	if len(tapscripts) == 0 {
		return custodySpendPath{}, fmt.Errorf("custody ref %s:%d is missing tapscripts", ref.TxID, ref.VOut)
	}
	vtxoScript, err := arkscript.ParseVtxoScript(tapscripts)
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
	desired := uniqueSortedPlayerIDs(desiredPlayerIDs)

	bestIndex := -1
	bestScript := []byte(nil)
	bestPlayers := []string(nil)
	for index, closure := range vtxoScript.ForfeitClosures() {
		keys := make([]string, 0)
		switch typed := closure.(type) {
		case *arkscript.MultisigClosure:
			for _, key := range typed.PubKeys {
				xOnly := hex.EncodeToString(schnorr.SerializePubKey(key))
				if xOnly == operatorXOnly {
					continue
				}
				playerID, ok, err := runtime.playerIDByXOnlyPubkey(table, xOnly)
				if err != nil {
					return custodySpendPath{}, err
				}
				if ok {
					keys = append(keys, playerID)
				}
			}
		case *arkscript.CLTVMultisigClosure:
			for _, key := range typed.PubKeys {
				xOnly := hex.EncodeToString(schnorr.SerializePubKey(key))
				if xOnly == operatorXOnly {
					continue
				}
				playerID, ok, err := runtime.playerIDByXOnlyPubkey(table, xOnly)
				if err != nil {
					return custodySpendPath{}, err
				}
				if ok {
					keys = append(keys, playerID)
				}
			}
		default:
			continue
		}
		keys = uniqueSortedPlayerIDs(keys)
		if !reflect.DeepEqual(keys, desired) {
			continue
		}
		if preferTimeout {
			if _, ok := closure.(*arkscript.CLTVMultisigClosure); ok {
				bestIndex = index
				bestPlayers = keys
				bestScript, err = closure.Script()
				if err != nil {
					return custodySpendPath{}, err
				}
				break
			}
			continue
		}
		bestIndex = index
		bestPlayers = keys
		bestScript, err = closure.Script()
		if err != nil {
			return custodySpendPath{}, err
		}
		break
	}
	if bestIndex < 0 {
		return custodySpendPath{}, fmt.Errorf("no custody spend path matches signers %v for %s:%d", desired, ref.TxID, ref.VOut)
	}
	tapKey, tapTree, err := vtxoScript.TapTree()
	if err != nil {
		return custodySpendPath{}, err
	}
	leafProof, err := tapTree.GetTaprootMerkleProof(txscript.NewBaseTapLeaf(bestScript).TapHash())
	if err != nil {
		return custodySpendPath{}, err
	}
	pkScript, err := arkscript.P2TRScript(tapKey)
	if err != nil {
		return custodySpendPath{}, err
	}
	return custodySpendPath{
		LeafProof:  leafProof,
		PKScript:   pkScript,
		PlayerIDs:  bestPlayers,
		Script:     bestScript,
		Tapscripts: tapscripts,
	}, nil
}

func (runtime *meshRuntime) buildCustodySettlementPlan(table nativeTableState, transition tablecustody.CustodyTransition) (*custodySettlementPlan, error) {
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

	treeSignerIDs := runtime.requiredCustodySigners(table, transition)
	plan := &custodySettlementPlan{
		Inputs:        []custodyInputSpec{},
		Outputs:       []custodyOutputSpec{},
		TreeSignerIDs: append([]string(nil), treeSignerIDs...),
	}
	proofSignerSet := map[string]struct{}{}
	potSpendSignerIDs := append([]string(nil), treeSignerIDs...)
	if len(potSpendSignerIDs) == 0 {
		potSpendSignerIDs = playerIDsFromSeats(table.Seats)
	}
	preferTimeout := transition.TimeoutResolution != nil

	for index := range transition.NextState.StackClaims {
		nextClaim := transition.NextState.StackClaims[index]
		prevClaim, hadPrev := prevStacks[nextClaim.PlayerID]
		if hadPrev && reflect.DeepEqual(comparableStackClaim(prevClaim), comparableStackClaim(nextClaim)) {
			transition.NextState.StackClaims[index].VTXORefs = append([]tablecustody.VTXORef(nil), prevClaim.VTXORefs...)
			continue
		}
		inputRefs := append([]tablecustody.VTXORef(nil), nextClaim.VTXORefs...)
		if hadPrev {
			inputRefs = append([]tablecustody.VTXORef(nil), prevClaim.VTXORefs...)
		}
		transition.NextState.StackClaims[index].VTXORefs = nil
		if len(inputRefs) > 0 {
			for _, ref := range inputRefs {
				spendPath, err := runtime.selectCustodySpendPath(table, ref, []string{nextClaim.PlayerID}, false)
				if err != nil {
					return nil, err
				}
				plan.Inputs = append(plan.Inputs, custodyInputSpec{
					ClaimKey:      stackClaimKey(nextClaim.PlayerID),
					OwnerPlayerID: nextClaim.PlayerID,
					Ref:           ref,
					SpendPath:     spendPath,
				})
				for _, playerID := range spendPath.PlayerIDs {
					proofSignerSet[playerID] = struct{}{}
				}
			}
		}
		if nextClaim.AmountSats > 0 {
			output, err := runtime.stackOutputSpec(table, nextClaim.PlayerID, nextClaim.AmountSats)
			if err != nil {
				return nil, err
			}
			plan.Outputs = append(plan.Outputs, output)
		}
	}

	for index := range transition.NextState.PotSlices {
		nextSlice := transition.NextState.PotSlices[index]
		prevSlice, hadPrev := prevPots[nextSlice.PotID]
		if hadPrev && reflect.DeepEqual(comparablePotSlice(prevSlice), comparablePotSlice(nextSlice)) {
			transition.NextState.PotSlices[index].VTXORefs = append([]tablecustody.VTXORef(nil), prevSlice.VTXORefs...)
			continue
		}
		transition.NextState.PotSlices[index].VTXORefs = nil
		if hadPrev {
			for _, ref := range prevSlice.VTXORefs {
				spendPath, err := runtime.selectCustodySpendPath(table, ref, potSpendSignerIDs, preferTimeout)
				if err != nil {
					return nil, err
				}
				plan.Inputs = append(plan.Inputs, custodyInputSpec{
					ClaimKey:  potClaimKey(nextSlice.PotID),
					Ref:       ref,
					SpendPath: spendPath,
				})
				for _, playerID := range spendPath.PlayerIDs {
					proofSignerSet[playerID] = struct{}{}
				}
			}
		}
		if nextSlice.TotalSats > 0 {
			output, err := runtime.potOutputSpec(table, transition, nextSlice, potSpendSignerIDs)
			if err != nil {
				return nil, err
			}
			plan.Outputs = append(plan.Outputs, output)
		}
	}

	if len(plan.TreeSignerIDs) == 0 {
		for _, playerID := range playerIDsFromSeats(table.Seats) {
			plan.TreeSignerIDs = append(plan.TreeSignerIDs, playerID)
		}
	}
	if len(plan.TreeSignerIDs) == 0 {
		for _, claim := range transition.NextState.StackClaims {
			plan.TreeSignerIDs = append(plan.TreeSignerIDs, claim.PlayerID)
		}
	}
	plan.TreeSignerIDs = uniqueSortedPlayerIDs(plan.TreeSignerIDs)
	for playerID := range proofSignerSet {
		plan.ProofSignerIDs = append(plan.ProofSignerIDs, playerID)
	}
	sort.Strings(plan.ProofSignerIDs)
	return plan, nil
}
