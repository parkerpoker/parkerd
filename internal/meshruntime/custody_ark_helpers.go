package meshruntime

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"

	arklib "github.com/arkade-os/arkd/pkg/ark-lib"
	arkscript "github.com/arkade-os/arkd/pkg/ark-lib/script"
	sdktypes "github.com/arkade-os/go-sdk/types"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/txscript"
	"github.com/parkerpoker/parkerd/internal/tablecustody"
	walletpkg "github.com/parkerpoker/parkerd/internal/wallet"
)

type custodySpendPath struct {
	LeafProof        *arklib.TaprootMerkleProof
	Locktime         arklib.AbsoluteLocktime
	PKScript         []byte
	PlayerIDs        []string
	Script           []byte
	Tapscripts       []string
	UsesCLTVLocktime bool
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
	AuthorizedOutputs []custodyBatchOutput
	Inputs            []custodyInputSpec
	Outputs           []custodyOutputSpec
	ProofSignerIDs    []string
	TreeSignerIDs     []string
}

const (
	defaultRealCustodyReserveTransitions = 8
	minimumRealCustodyReserveSats        = 10_000
)

func (runtime *meshRuntime) arkCustodyConfig() (walletpkg.CustodyArkConfig, error) {
	config, err := runtime.walletRuntime.ArkConfig(runtime.profileName)
	if err != nil {
		return walletpkg.CustodyArkConfig{}, err
	}
	if strings.TrimSpace(config.SignerPubkeyHex) == "" {
		config.SignerPubkeyHex = runtime.protocolIdentity.PublicKeyHex
	}
	if strings.TrimSpace(config.ForfeitPubkeyHex) == "" {
		config.ForfeitPubkeyHex = runtime.protocolIdentity.PublicKeyHex
	}
	if config.UnilateralExitDelay.Value == 0 {
		config.UnilateralExitDelay = arklib.RelativeLocktime{Type: arklib.LocktimeTypeBlock, Value: 512}
	}
	return config, nil
}

func (runtime *meshRuntime) estimatedCustodyBatchFee(offchainInputs, offchainOutputs, onchainInputs, onchainOutputs int) (int, error) {
	config, err := runtime.arkCustodyConfig()
	if err != nil {
		return 0, err
	}
	fee := 0
	fee += offchainInputs * maxInt(0, config.OffchainInputFeeSats)
	fee += offchainOutputs * maxInt(0, config.OffchainOutputFeeSats)
	fee += onchainInputs * maxInt(0, config.OnchainInputFeeSats)
	fee += onchainOutputs * maxInt(0, config.OnchainOutputFeeSats)
	return fee, nil
}

func (runtime *meshRuntime) initialSeatFeeReserveSats() (int, error) {
	if runtime.config.UseMockSettlement {
		return 0, nil
	}
	if override := strings.TrimSpace(os.Getenv("PARKER_CUSTODY_FEE_RESERVE_SATS")); override != "" {
		value, err := strconv.Atoi(override)
		if err != nil {
			return 0, fmt.Errorf("parse PARKER_CUSTODY_FEE_RESERVE_SATS: %w", err)
		}
		return maxInt(0, value), nil
	}
	perTransition, err := runtime.estimatedCustodyBatchFee(2, 2, 0, 0)
	if err != nil {
		return 0, err
	}
	return maxInt(minimumRealCustodyReserveSats, perTransition*defaultRealCustodyReserveTransitions), nil
}

func (runtime *meshRuntime) requiredJoinFundingSats(buyInSats int) (int, error) {
	if runtime.config.UseMockSettlement {
		return buyInSats, nil
	}
	reserve, err := runtime.initialSeatFeeReserveSats()
	if err != nil {
		return 0, err
	}
	return buyInSats + reserve, nil
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
	timestamp, err := parseISOTimestamp(value)
	if err != nil {
		return 0, err
	}
	return arklib.AbsoluteLocktime(timestamp.Unix()), nil
}

func comparableStackClaim(claim tablecustody.StackClaim) tablecustody.StackClaim {
	claim.VTXORefs = nil
	return claim
}

func comparableStackClaimSansReserve(claim tablecustody.StackClaim) tablecustody.StackClaim {
	claim = comparableStackClaim(claim)
	claim.ReservedFeeSats = 0
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

func stackClaimRefAmount(claim tablecustody.StackClaim) int {
	return claim.AmountSats + claim.ReservedFeeSats
}

func (runtime *meshRuntime) walletPubkeyHexForPlayer(table nativeTableState, playerID string) (string, error) {
	if seat, ok := seatRecordForPlayer(table, playerID); ok && strings.TrimSpace(seat.WalletPubkeyHex) != "" {
		return seat.WalletPubkeyHex, nil
	}
	if playerID == runtime.walletID.PlayerID && strings.TrimSpace(runtime.walletID.PublicKeyHex) != "" {
		return runtime.walletID.PublicKeyHex, nil
	}
	return "", fmt.Errorf("missing seat for player %s", playerID)
}

func (runtime *meshRuntime) stackOutputSpec(table nativeTableState, playerID string, amountSats int) (custodyOutputSpec, error) {
	config, err := runtime.arkCustodyConfig()
	if err != nil {
		return custodyOutputSpec{}, err
	}
	walletPubkeyHex, err := runtime.walletPubkeyHexForPlayer(table, playerID)
	if err != nil {
		return custodyOutputSpec{}, err
	}
	ownerPubkey, err := compressedPubkeyFromHex(walletPubkeyHex)
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

func timeoutSignerSets(transition tablecustody.CustodyTransition, futureSignerIDs []string) [][]string {
	signers := uniqueSortedPlayerIDs(futureSignerIDs)
	if len(signers) == 0 {
		return nil
	}
	actingPlayerID := transition.NextState.ActingPlayerID
	if transition.Kind == tablecustody.TransitionKindShowdownPayout {
		actingPlayerID = ""
	}
	if strings.TrimSpace(actingPlayerID) != "" {
		timeoutSigners := make([]string, 0, len(signers))
		for _, playerID := range signers {
			if playerID == actingPlayerID {
				continue
			}
			timeoutSigners = append(timeoutSigners, playerID)
		}
		if len(timeoutSigners) == 0 {
			return nil
		}
		return [][]string{timeoutSigners}
	}
	if len(signers) < 2 {
		return nil
	}
	sets := make([][]string, 0, len(signers))
	for _, missingPlayerID := range signers {
		timeoutSigners := make([]string, 0, len(signers)-1)
		for _, playerID := range signers {
			if playerID == missingPlayerID {
				continue
			}
			timeoutSigners = append(timeoutSigners, playerID)
		}
		if len(timeoutSigners) == 0 {
			continue
		}
		sets = append(sets, timeoutSigners)
	}
	return sets
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

	closures := make([]arkscript.Closure, 0, 3)
	collaborativeKeys := append([]*btcec.PublicKey{}, playerPubkeys...)
	collaborativeKeys = append(collaborativeKeys, operatorPubkey)
	closures = append(closures, &arkscript.MultisigClosure{PubKeys: collaborativeKeys})

	if transition.NextState.ActionDeadlineAt != "" {
		locktime, err := absoluteLocktimeFromISO(transition.NextState.ActionDeadlineAt)
		if err != nil {
			return custodyOutputSpec{}, err
		}
		seenTimeoutSets := map[string]struct{}{}
		for _, timeoutSignerIDs := range timeoutSignerSets(transition, futureSignerIDs) {
			setKey := strings.Join(timeoutSignerIDs, ",")
			if _, ok := seenTimeoutSets[setKey]; ok {
				continue
			}
			seenTimeoutSets[setKey] = struct{}{}
			timeoutKeys := make([]*btcec.PublicKey, 0, len(timeoutSignerIDs)+1)
			for _, playerID := range timeoutSignerIDs {
				walletPubkeyHex, err := runtime.walletPubkeyHexForPlayer(table, playerID)
				if err != nil {
					return custodyOutputSpec{}, err
				}
				pubkey, err := compressedPubkeyFromHex(walletPubkeyHex)
				if err != nil {
					return custodyOutputSpec{}, err
				}
				timeoutKeys = append(timeoutKeys, pubkey)
			}
			if len(timeoutKeys) == 0 {
				continue
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
	if strings.TrimSpace(runtime.walletID.PublicKeyHex) != "" && strings.TrimSpace(runtime.walletID.PlayerID) != "" {
		localXOnly, err := xOnlyPubkeyHexFromCompressed(runtime.walletID.PublicKeyHex)
		if err != nil {
			return "", false, err
		}
		if localXOnly == xOnlyHex {
			return runtime.walletID.PlayerID, true, nil
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
	bestLocktime := arklib.AbsoluteLocktime(0)
	bestScript := []byte(nil)
	bestPlayers := []string(nil)
	for index, closure := range vtxoScript.ForfeitClosures() {
		keys := make([]string, 0)
		unmappedNonOperatorKeys := 0
		appendClosureKeys := func(pubkeys []*btcec.PublicKey) error {
			for _, key := range pubkeys {
				xOnly := hex.EncodeToString(schnorr.SerializePubKey(key))
				if xOnly == operatorXOnly {
					continue
				}
				playerID, ok, err := runtime.playerIDByXOnlyPubkey(table, xOnly)
				if err != nil {
					return err
				}
				if ok {
					keys = append(keys, playerID)
					continue
				}
				unmappedNonOperatorKeys++
			}
			return nil
		}
		switch typed := closure.(type) {
		case *arkscript.MultisigClosure:
			if err := appendClosureKeys(typed.PubKeys); err != nil {
				return custodySpendPath{}, err
			}
		case *arkscript.CLTVMultisigClosure:
			if err := appendClosureKeys(typed.PubKeys); err != nil {
				return custodySpendPath{}, err
			}
		default:
			continue
		}
		if len(keys) == 0 && unmappedNonOperatorKeys == 1 && strings.TrimSpace(ref.OwnerPlayerID) != "" {
			keys = append(keys, ref.OwnerPlayerID)
		}
		keys = uniqueSortedPlayerIDs(keys)
		if !reflect.DeepEqual(keys, desired) {
			continue
		}
		if preferTimeout {
			if cltv, ok := closure.(*arkscript.CLTVMultisigClosure); ok {
				bestIndex = index
				bestLocktime = cltv.Locktime
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
		bestLocktime = 0
		if cltv, ok := closure.(*arkscript.CLTVMultisigClosure); ok {
			bestLocktime = cltv.Locktime
		}
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
		LeafProof:        leafProof,
		Locktime:         bestLocktime,
		PKScript:         pkScript,
		PlayerIDs:        bestPlayers,
		Script:           bestScript,
		Tapscripts:       tapscripts,
		UsesCLTVLocktime: bestLocktime != 0,
	}, nil
}

func (runtime *meshRuntime) buildCustodySettlementPlan(table nativeTableState, transition tablecustody.CustodyTransition) (*custodySettlementPlan, error) {
	transition = cloneJSON(transition)
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
	nextPotIDs := map[string]struct{}{}

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
		} else if transition.Kind == tablecustody.TransitionKindBuyInLock {
			if seat, ok := seatRecordForPlayer(table, nextClaim.PlayerID); ok && len(seat.FundingRefs) > 0 {
				inputRefs = append([]tablecustody.VTXORef(nil), seat.FundingRefs...)
			}
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
			output, err := runtime.stackOutputSpec(table, nextClaim.PlayerID, stackClaimRefAmount(nextClaim))
			if err != nil {
				return nil, err
			}
			plan.Outputs = append(plan.Outputs, output)
		} else if nextClaim.ReservedFeeSats > 0 {
			output, err := runtime.stackOutputSpec(table, nextClaim.PlayerID, stackClaimRefAmount(nextClaim))
			if err != nil {
				return nil, err
			}
			plan.Outputs = append(plan.Outputs, output)
		}
	}

	for index := range transition.NextState.PotSlices {
		nextSlice := transition.NextState.PotSlices[index]
		nextPotIDs[nextSlice.PotID] = struct{}{}
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
	for potID, prevSlice := range prevPots {
		if _, ok := nextPotIDs[potID]; ok {
			continue
		}
		for _, ref := range prevSlice.VTXORefs {
			spendPath, err := runtime.selectCustodySpendPath(table, ref, potSpendSignerIDs, preferTimeout)
			if err != nil {
				return nil, err
			}
			plan.Inputs = append(plan.Inputs, custodyInputSpec{
				ClaimKey:  potClaimKey(potID),
				Ref:       ref,
				SpendPath: spendPath,
			})
			for _, playerID := range spendPath.PlayerIDs {
				proofSignerSet[playerID] = struct{}{}
			}
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
	authorizedOutputs, err := runtime.authorizedCustodyBatchOutputs(table, transition, plan)
	if err != nil {
		return nil, err
	}
	plan.AuthorizedOutputs = authorizedOutputs
	return plan, nil
}

func (runtime *meshRuntime) authorizedCustodyBatchOutputs(table nativeTableState, transition tablecustody.CustodyTransition, plan *custodySettlementPlan) ([]custodyBatchOutput, error) {
	if plan == nil {
		return nil, errors.New("missing custody settlement plan")
	}
	outputs := make([]custodyBatchOutput, 0, len(plan.Outputs)+1)
	for _, output := range plan.Outputs {
		outputs = append(outputs, custodyBatchOutputFromSpec(output))
	}
	if transition.Kind == tablecustody.TransitionKindCashOut {
		claim, ok := latestStackClaimForPlayer(table.LatestCustodyState, transition.ActingPlayerID)
		if !ok {
			return nil, errors.New("latest custody state is missing the target stack claim")
		}
		totalClaimSats := stackClaimBackedAmount(claim)
		if totalClaimSats <= 0 {
			return nil, errors.New("latest custody state has no spendable stack to settle")
		}
		seat, ok := seatRecordForPlayer(table, transition.ActingPlayerID)
		if !ok {
			return nil, fmt.Errorf("missing seat for cash-out player %s", transition.ActingPlayerID)
		}
		if strings.TrimSpace(seat.ArkAddress) == "" {
			return nil, fmt.Errorf("seat %s is missing an Ark address", transition.ActingPlayerID)
		}
		feeSats, err := runtime.estimatedCustodyBatchFee(len(plan.Inputs), len(outputs)+1, 0, 0)
		if err != nil {
			return nil, err
		}
		settledAmount := totalClaimSats - feeSats
		if settledAmount <= 0 {
			return nil, fmt.Errorf("custody claim is too small to cover Ark cash-out fees: have %d need %d", totalClaimSats, feeSats)
		}
		output, err := custodyBatchOutputFromReceiver("wallet-return", transition.ActingPlayerID, sdktypes.Receiver{
			To:     seat.ArkAddress,
			Amount: uint64(settledAmount),
		}, nil)
		if err != nil {
			return nil, err
		}
		outputs = append(outputs, output)
		return outputs, nil
	}
	return outputs, nil
}

func (runtime *meshRuntime) custodyFeePayerIDs(table nativeTableState, transition tablecustody.CustodyTransition) []string {
	if transition.Kind == tablecustody.TransitionKindCashOut || transition.Kind == tablecustody.TransitionKindEmergencyExit {
		if strings.TrimSpace(transition.ActingPlayerID) != "" {
			return []string{transition.ActingPlayerID}
		}
	}

	previousStacks := map[string]tablecustody.StackClaim{}
	previousPots := map[string]tablecustody.PotSlice{}
	if table.LatestCustodyState != nil {
		for _, claim := range table.LatestCustodyState.StackClaims {
			previousStacks[claim.PlayerID] = claim
		}
		for _, slice := range table.LatestCustodyState.PotSlices {
			previousPots[slice.PotID] = slice
		}
	}
	changed := make([]string, 0)
	for _, claim := range transition.NextState.StackClaims {
		prevClaim, hadPrev := previousStacks[claim.PlayerID]
		if !hadPrev || !reflect.DeepEqual(comparableStackClaimSansReserve(prevClaim), comparableStackClaimSansReserve(claim)) {
			changed = append(changed, claim.PlayerID)
		}
	}
	changed = uniqueSortedPlayerIDs(changed)
	if len(changed) > 0 {
		return changed
	}
	potsChanged := false
	for _, slice := range transition.NextState.PotSlices {
		prevSlice, hadPrev := previousPots[slice.PotID]
		if !hadPrev || !reflect.DeepEqual(comparablePotSlice(prevSlice), comparablePotSlice(slice)) {
			potsChanged = true
			break
		}
	}
	if !potsChanged {
		return nil
	}
	if strings.TrimSpace(transition.ActingPlayerID) != "" {
		return []string{transition.ActingPlayerID}
	}
	return nil
}

func previewFeeChargedTransition(previous *tablecustody.CustodyState, transition tablecustody.CustodyTransition, payerIDs []string) tablecustody.CustodyTransition {
	preview := cloneJSON(transition)
	if previous == nil || len(payerIDs) == 0 {
		return preview
	}
	previousByPlayer := map[string]tablecustody.StackClaim{}
	for _, claim := range previous.StackClaims {
		previousByPlayer[claim.PlayerID] = claim
	}
	payers := map[string]struct{}{}
	for _, playerID := range payerIDs {
		payers[playerID] = struct{}{}
	}
	for index := range preview.NextState.StackClaims {
		claim := preview.NextState.StackClaims[index]
		if _, ok := payers[claim.PlayerID]; !ok {
			continue
		}
		prevClaim, hadPrev := previousByPlayer[claim.PlayerID]
		if hadPrev && reflect.DeepEqual(comparableStackClaim(prevClaim), comparableStackClaim(claim)) {
			preview.NextState.StackClaims[index].ReservedFeeSats++
		}
	}
	return preview
}

func (runtime *meshRuntime) applyRealCustodyFeeReserve(table nativeTableState, transition *tablecustody.CustodyTransition) error {
	if transition == nil || runtime.config.UseMockSettlement {
		return nil
	}
	payerIDs := runtime.custodyFeePayerIDs(table, *transition)
	if len(payerIDs) == 0 {
		return nil
	}
	preview := previewFeeChargedTransition(table.LatestCustodyState, *transition, payerIDs)
	plan, err := runtime.buildCustodySettlementPlan(table, preview)
	if err != nil {
		return err
	}
	feeSats, err := runtime.estimatedCustodyBatchFee(len(plan.Inputs), len(plan.Outputs), 0, 0)
	if err != nil {
		return err
	}
	if feeSats <= 0 {
		return nil
	}
	return allocateCustodyFeeReserve(transition, payerIDs, feeSats)
}

func allocateCustodyFeeReserve(transition *tablecustody.CustodyTransition, payerIDs []string, feeSats int) error {
	if transition == nil || feeSats <= 0 {
		return nil
	}
	payerSet := map[string]struct{}{}
	for _, playerID := range payerIDs {
		payerSet[playerID] = struct{}{}
	}
	type payerReserve struct {
		Index        int
		PlayerID     string
		ReservedSats int
		SeatIndex    int
	}
	reserves := make([]payerReserve, 0, len(payerSet))
	totalReserve := 0
	for index, claim := range transition.NextState.StackClaims {
		if _, ok := payerSet[claim.PlayerID]; !ok {
			continue
		}
		reserves = append(reserves, payerReserve{
			Index:        index,
			PlayerID:     claim.PlayerID,
			ReservedSats: claim.ReservedFeeSats,
			SeatIndex:    claim.SeatIndex,
		})
		totalReserve += claim.ReservedFeeSats
	}
	if totalReserve < feeSats {
		return fmt.Errorf("insufficient custody fee reserve: need %d have %d", feeSats, totalReserve)
	}
	sort.SliceStable(reserves, func(left, right int) bool {
		if reserves[left].ReservedSats != reserves[right].ReservedSats {
			return reserves[left].ReservedSats > reserves[right].ReservedSats
		}
		if reserves[left].SeatIndex != reserves[right].SeatIndex {
			return reserves[left].SeatIndex < reserves[right].SeatIndex
		}
		return reserves[left].PlayerID < reserves[right].PlayerID
	})
	remaining := feeSats
	for _, reserve := range reserves {
		if remaining == 0 {
			break
		}
		deduction := minInt(reserve.ReservedSats, remaining)
		transition.NextState.StackClaims[reserve.Index].ReservedFeeSats -= deduction
		remaining -= deduction
	}
	if remaining > 0 {
		return fmt.Errorf("custody fee reserve allocation left %d sats unpaid", remaining)
	}
	return nil
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}
