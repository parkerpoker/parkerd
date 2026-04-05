package meshruntime

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

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
	LeafProof          *arklib.TaprootMerkleProof
	CSVLocktime        arklib.RelativeLocktime
	Locktime           arklib.AbsoluteLocktime
	PKScript           []byte
	PlayerIDs          []string
	SignerXOnlyPubkeys []string
	Script             []byte
	Tapscripts         []string
	UsesCSVLocktime    bool
	UsesCLTVLocktime   bool
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
	// Ark/Bitcoin evaluates CLTV against chain time, which can lag local wall
	// clock. Encode timeout leaves slightly before the visible protocol deadline
	// so the timeout path is mature once the runtime decides the deadline elapsed.
	custodyTimeoutLocktimeSlack = 2 * time.Minute
)

var (
	compressedPubkeyCache sync.Map
	custodyTapscriptCache sync.Map
	xOnlyPubkeyHexCache   sync.Map
)

type cachedCustodySpendCandidate struct {
	Kind               string
	LeafProof          *arklib.TaprootMerkleProof
	Locktime           arklib.AbsoluteLocktime
	Script             []byte
	SignerXOnlyPubkeys []string
	UsesCLTVLocktime   bool
}

type cachedCustodySpendPaths struct {
	Candidates []cachedCustodySpendCandidate
	PKScript   []byte
	Tapscripts []string
}

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
	if !runtime.config.UseMockSettlement &&
		runtime.custodyBatchExecute != nil &&
		strings.TrimSpace(runtime.candidateIntentAckSigningKeyHex(config.SignerPubkeyHex)) == "" {
		_, mockSignerPubkeyHex := mockOperatorSigningKeyHex("parker-mock-ark-signer")
		config.SignerPubkeyHex = mockSignerPubkeyHex
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
	normalized := strings.TrimSpace(value)
	if cached, ok := compressedPubkeyCache.Load(normalized); ok {
		return cached.(*btcec.PublicKey), nil
	}
	raw, err := hex.DecodeString(normalized)
	if err != nil {
		return nil, err
	}
	pubkey, err := btcec.ParsePubKey(raw)
	if err != nil {
		return nil, err
	}
	actual, _ := compressedPubkeyCache.LoadOrStore(normalized, pubkey)
	return actual.(*btcec.PublicKey), nil
}

func xOnlyPubkeyHexFromCompressed(value string) (string, error) {
	normalized := strings.TrimSpace(value)
	if cached, ok := xOnlyPubkeyHexCache.Load(normalized); ok {
		return cached.(string), nil
	}
	pubkey, err := compressedPubkeyFromHex(normalized)
	if err != nil {
		return "", err
	}
	encoded := hex.EncodeToString(schnorr.SerializePubKey(pubkey))
	actual, _ := xOnlyPubkeyHexCache.LoadOrStore(normalized, encoded)
	return actual.(string), nil
}

func custodyTapscriptsCacheKey(tapscripts []string) string {
	sum := sha256.New()
	for _, tapscript := range tapscripts {
		_, _ = sum.Write([]byte(strconv.Itoa(len(tapscript))))
		_, _ = sum.Write([]byte{':'})
		_, _ = sum.Write([]byte(tapscript))
		_, _ = sum.Write([]byte{0})
	}
	return hex.EncodeToString(sum.Sum(nil))
}

func cloneTaprootMerkleProof(proof *arklib.TaprootMerkleProof) *arklib.TaprootMerkleProof {
	if proof == nil {
		return nil
	}
	cloned := *proof
	cloned.ControlBlock = append([]byte(nil), proof.ControlBlock...)
	cloned.Script = append([]byte(nil), proof.Script...)
	return &cloned
}

func cloneCachedCustodySpendCandidate(candidate cachedCustodySpendCandidate) cachedCustodySpendCandidate {
	cloned := candidate
	cloned.LeafProof = cloneTaprootMerkleProof(candidate.LeafProof)
	cloned.Script = append([]byte(nil), candidate.Script...)
	cloned.SignerXOnlyPubkeys = append([]string(nil), candidate.SignerXOnlyPubkeys...)
	return cloned
}

func cloneCachedCustodySpendPaths(paths *cachedCustodySpendPaths) *cachedCustodySpendPaths {
	if paths == nil {
		return nil
	}
	cloned := &cachedCustodySpendPaths{
		Candidates: make([]cachedCustodySpendCandidate, len(paths.Candidates)),
		PKScript:   append([]byte(nil), paths.PKScript...),
		Tapscripts: append([]string(nil), paths.Tapscripts...),
	}
	for index, candidate := range paths.Candidates {
		cloned.Candidates[index] = cloneCachedCustodySpendCandidate(candidate)
	}
	return cloned
}

func cachedCustodySpendPathsForTapscripts(tapscripts []string) (*cachedCustodySpendPaths, error) {
	key := custodyTapscriptsCacheKey(tapscripts)
	if cached, ok := custodyTapscriptCache.Load(key); ok {
		return cloneCachedCustodySpendPaths(cached.(*cachedCustodySpendPaths)), nil
	}
	vtxoScript, err := arkscript.ParseVtxoScript(tapscripts)
	if err != nil {
		return nil, err
	}
	tapKey, tapTree, err := vtxoScript.TapTree()
	if err != nil {
		return nil, err
	}
	pkScript, err := arkscript.P2TRScript(tapKey)
	if err != nil {
		return nil, err
	}
	paths := &cachedCustodySpendPaths{
		Candidates: []cachedCustodySpendCandidate{},
		PKScript:   pkScript,
		Tapscripts: append([]string(nil), tapscripts...),
	}
	for _, closure := range vtxoScript.ForfeitClosures() {
		candidate := cachedCustodySpendCandidate{
			SignerXOnlyPubkeys: []string{},
		}
		appendPubkeys := func(pubkeys []*btcec.PublicKey) {
			for _, key := range pubkeys {
				candidate.SignerXOnlyPubkeys = append(candidate.SignerXOnlyPubkeys, hex.EncodeToString(schnorr.SerializePubKey(key)))
			}
		}
		switch typed := closure.(type) {
		case *arkscript.MultisigClosure:
			candidate.Kind = "*arkscript.MultisigClosure"
			appendPubkeys(typed.PubKeys)
		case *arkscript.CLTVMultisigClosure:
			candidate.Kind = "*arkscript.CLTVMultisigClosure"
			candidate.Locktime = typed.Locktime
			candidate.UsesCLTVLocktime = true
			appendPubkeys(typed.PubKeys)
		default:
			continue
		}
		candidate.Script, err = closure.Script()
		if err != nil {
			return nil, err
		}
		candidate.LeafProof, err = tapTree.GetTaprootMerkleProof(txscript.NewBaseTapLeaf(candidate.Script).TapHash())
		if err != nil {
			return nil, err
		}
		paths.Candidates = append(paths.Candidates, candidate)
	}
	actual, _ := custodyTapscriptCache.LoadOrStore(key, cloneCachedCustodySpendPaths(paths))
	return cloneCachedCustodySpendPaths(actual.(*cachedCustodySpendPaths)), nil
}

func timeoutLocktimeFromISO(value string) (arklib.AbsoluteLocktime, error) {
	if strings.TrimSpace(value) == "" {
		return 0, errors.New("missing action deadline")
	}
	timestamp, err := parseISOTimestamp(value)
	if err != nil {
		return 0, err
	}
	timestamp = timestamp.Add(-custodyTimeoutLocktimeSlack)
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
	return runtime.stackOutputSpecForTransition(table, nil, playerID, amountSats)
}

func (runtime *meshRuntime) stackOutputSpecForTransition(table nativeTableState, transition *tablecustody.CustodyTransition, playerID string, amountSats int) (custodyOutputSpec, error) {
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
	var vtxoScript *arkscript.TapscriptsVtxoScript
	challengeOpenLeaf := arkscript.Closure(nil)
	if transition != nil {
		leaf, _, err := runtime.turnChallengeOpenLeaf(table, &transition.NextState)
		if err != nil {
			return custodyOutputSpec{}, err
		}
		if leaf != nil {
			challengeOpenLeaf = leaf
		}
	}
	if challengeOpenLeaf == nil {
		vtxoScript = arkscript.NewDefaultVtxoScript(ownerPubkey, signerPubkey, config.UnilateralExitDelay)
	} else {
		vtxoScript = &arkscript.TapscriptsVtxoScript{
			Closures: []arkscript.Closure{
				&arkscript.MultisigClosure{PubKeys: []*btcec.PublicKey{ownerPubkey, signerPubkey}},
				challengeOpenLeaf,
				&arkscript.CSVMultisigClosure{
					Locktime: config.UnilateralExitDelay,
					MultisigClosure: arkscript.MultisigClosure{
						PubKeys: []*btcec.PublicKey{ownerPubkey},
					},
				},
			},
		}
	}
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
	if challengeOpenLeaf, _, err := runtime.turnChallengeOpenLeaf(table, &transition.NextState); err != nil {
		return custodyOutputSpec{}, err
	} else if challengeOpenLeaf != nil {
		closures = append(closures, challengeOpenLeaf)
	}

	if transition.NextState.ActionDeadlineAt != "" {
		locktime, err := timeoutLocktimeFromISO(transition.NextState.ActionDeadlineAt)
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

func canonicalStrings(values []string) []string {
	canonical := append([]string(nil), values...)
	sort.Strings(canonical)
	return canonical
}

func sameCanonicalStrings(left, right []string) bool {
	return reflect.DeepEqual(canonicalStrings(left), canonicalStrings(right))
}

func (runtime *meshRuntime) potSpendSignerIDsForTransition(table nativeTableState, transition tablecustody.CustodyTransition) []string {
	signerIDs := append([]string(nil), runtime.requiredCustodySigners(table, transition)...)
	if len(signerIDs) == 0 {
		signerIDs = playerIDsFromSeats(table.Seats)
	}
	return uniqueSortedPlayerIDs(signerIDs)
}

func (runtime *meshRuntime) potSliceRefsReusableForTransition(table nativeTableState, transition tablecustody.CustodyTransition, previous, next tablecustody.PotSlice) bool {
	// Player-action transitions always mint fresh custody outputs. Historical
	// replay may happen after cash-out clears the current ActiveHand pointer, so
	// reuse cannot depend on the live table view.
	if transition.Kind == tablecustody.TransitionKindAction {
		return false
	}
	if !reflect.DeepEqual(comparablePotSlice(previous), comparablePotSlice(next)) {
		return false
	}
	if len(previous.VTXORefs) != 1 {
		return false
	}
	spec, err := runtime.potOutputSpec(table, transition, next, runtime.potSpendSignerIDsForTransition(table, transition))
	if err != nil {
		return false
	}
	ref := previous.VTXORefs[0]
	return ref.AmountSats == spec.AmountSats &&
		ref.Script == spec.Script &&
		sameCanonicalStrings(ref.Tapscripts, spec.Tapscripts)
}

func (runtime *meshRuntime) stackClaimRefsReusableForTransition(table nativeTableState, transition tablecustody.CustodyTransition, previous, next tablecustody.StackClaim) bool {
	// Player-action transitions always mint fresh custody outputs. Historical
	// replay may happen after cash-out clears the current ActiveHand pointer, so
	// reuse cannot depend on the live table view.
	if transition.Kind == tablecustody.TransitionKindAction {
		return false
	}
	if len(previous.VTXORefs) != 1 {
		return false
	}
	if previous.PlayerID != next.PlayerID || previous.SeatIndex != next.SeatIndex {
		return false
	}
	if stackClaimRefAmount(previous) != stackClaimRefAmount(next) {
		return false
	}
	spec, err := runtime.stackOutputSpecForTransition(table, &transition, next.PlayerID, stackClaimRefAmount(next))
	if err != nil {
		return false
	}
	ref := previous.VTXORefs[0]
	return ref.AmountSats == spec.AmountSats &&
		ref.Script == spec.Script &&
		sameCanonicalStrings(ref.Tapscripts, spec.Tapscripts)
}

func (runtime *meshRuntime) playerIDByXOnlyPubkey(table nativeTableState, xOnlyHex string) (string, bool, error) {
	lookup, err := runtime.playerIDsByXOnlyPubkey(table)
	if err != nil {
		return "", false, err
	}
	playerID, ok := lookup[xOnlyHex]
	return playerID, ok, nil
}

func (runtime *meshRuntime) playerIDsByXOnlyPubkey(table nativeTableState) (map[string]string, error) {
	lookup := map[string]string{}
	for _, seat := range table.Seats {
		seatXOnly, err := xOnlyPubkeyHexFromCompressed(seat.WalletPubkeyHex)
		if err != nil {
			return nil, err
		}
		lookup[seatXOnly] = seat.PlayerID
	}
	if strings.TrimSpace(runtime.walletID.PublicKeyHex) != "" && strings.TrimSpace(runtime.walletID.PlayerID) != "" {
		localXOnly, err := xOnlyPubkeyHexFromCompressed(runtime.walletID.PublicKeyHex)
		if err != nil {
			return nil, err
		}
		lookup[localXOnly] = runtime.walletID.PlayerID
	}
	return lookup, nil
}

func (runtime *meshRuntime) selectCustodySpendPath(table nativeTableState, ref tablecustody.VTXORef, desiredPlayerIDs []string, preferTimeout bool) (custodySpendPath, error) {
	tapscripts := append([]string(nil), ref.Tapscripts...)
	if len(tapscripts) == 0 {
		return custodySpendPath{}, fmt.Errorf("custody ref %s:%d is missing tapscripts", ref.TxID, ref.VOut)
	}
	paths, err := cachedCustodySpendPathsForTapscripts(tapscripts)
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
	playerIDsByXOnly, err := runtime.playerIDsByXOnlyPubkey(table)
	if err != nil {
		return custodySpendPath{}, err
	}

	bestCandidateIndex := -1
	bestPlayers := []string(nil)
	triedClosures := make([]string, 0, len(paths.Candidates))
	for index, candidate := range paths.Candidates {
		keys := make([]string, 0)
		unmappedNonOperatorKeys := 0
		for _, xOnly := range candidate.SignerXOnlyPubkeys {
			if xOnly == operatorXOnly {
				continue
			}
			playerID, ok := playerIDsByXOnly[xOnly]
			if ok {
				keys = append(keys, playerID)
				continue
			}
			unmappedNonOperatorKeys++
		}
		if len(keys) == 0 && unmappedNonOperatorKeys == 1 && strings.TrimSpace(ref.OwnerPlayerID) != "" {
			keys = append(keys, ref.OwnerPlayerID)
		}
		keys = uniqueSortedPlayerIDs(keys)
		triedClosures = append(triedClosures, fmt.Sprintf("%s:%v:unmapped=%d", candidate.Kind, keys, unmappedNonOperatorKeys))
		if !reflect.DeepEqual(keys, desired) {
			continue
		}
		if preferTimeout {
			if candidate.UsesCLTVLocktime {
				bestCandidateIndex = index
				bestPlayers = keys
				break
			}
			continue
		}
		bestCandidateIndex = index
		bestPlayers = keys
		break
	}
	if bestCandidateIndex < 0 {
		return custodySpendPath{}, fmt.Errorf("no custody spend path matches signers %v for %s:%d (owner=%s closures=%v)", desired, ref.TxID, ref.VOut, ref.OwnerPlayerID, triedClosures)
	}
	bestCandidate := paths.Candidates[bestCandidateIndex]
	return custodySpendPath{
		LeafProof:          cloneTaprootMerkleProof(bestCandidate.LeafProof),
		Locktime:           bestCandidate.Locktime,
		PKScript:           append([]byte(nil), paths.PKScript...),
		PlayerIDs:          bestPlayers,
		SignerXOnlyPubkeys: uniqueNonEmptyStrings(bestCandidate.SignerXOnlyPubkeys),
		Script:             append([]byte(nil), bestCandidate.Script...),
		Tapscripts:         append([]string(nil), paths.Tapscripts...),
		UsesCLTVLocktime:   bestCandidate.UsesCLTVLocktime,
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
	potSpendSignerIDs := runtime.potSpendSignerIDsForTransition(table, transition)
	preferTimeout := transition.TimeoutResolution != nil
	nextPotIDs := map[string]struct{}{}

	for index := range transition.NextState.StackClaims {
		nextClaim := transition.NextState.StackClaims[index]
		prevClaim, hadPrev := prevStacks[nextClaim.PlayerID]
		if hadPrev && runtime.stackClaimRefsReusableForTransition(table, transition, prevClaim, nextClaim) {
			transition.NextState.StackClaims[index].VTXORefs = append([]tablecustody.VTXORef(nil), prevClaim.VTXORefs...)
			continue
		}
		inputRefs := []tablecustody.VTXORef(nil)
		if hadPrev {
			inputRefs = append([]tablecustody.VTXORef(nil), prevClaim.VTXORefs...)
		} else if transition.Kind == tablecustody.TransitionKindBuyInLock {
			if seat, ok := seatRecordForPlayer(table, nextClaim.PlayerID); ok && len(seat.FundingRefs) > 0 {
				inputRefs = append([]tablecustody.VTXORef(nil), seat.FundingRefs...)
			}
		} else if len(nextClaim.VTXORefs) > 0 {
			inputRefs = append([]tablecustody.VTXORef(nil), nextClaim.VTXORefs...)
		}
		transition.NextState.StackClaims[index].VTXORefs = nil
		if len(inputRefs) > 0 {
			for _, ref := range inputRefs {
				spendPath, err := runtime.selectCustodySpendPath(table, ref, []string{nextClaim.PlayerID}, false)
				if err != nil {
					return nil, fmt.Errorf("select stack spend path for %s: %w", nextClaim.PlayerID, err)
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
			output, err := runtime.stackOutputSpecForTransition(table, &transition, nextClaim.PlayerID, stackClaimRefAmount(nextClaim))
			if err != nil {
				return nil, err
			}
			plan.Outputs = append(plan.Outputs, output)
		} else if nextClaim.ReservedFeeSats > 0 {
			output, err := runtime.stackOutputSpecForTransition(table, &transition, nextClaim.PlayerID, stackClaimRefAmount(nextClaim))
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
		if hadPrev && runtime.potSliceRefsReusableForTransition(table, transition, prevSlice, nextSlice) {
			transition.NextState.PotSlices[index].VTXORefs = append([]tablecustody.VTXORef(nil), prevSlice.VTXORefs...)
			continue
		}
		transition.NextState.PotSlices[index].VTXORefs = nil
		if hadPrev {
			for _, ref := range prevSlice.VTXORefs {
				spendPath, err := runtime.selectCustodySpendPath(table, ref, potSpendSignerIDs, preferTimeout)
				if err != nil {
					return nil, fmt.Errorf("select pot spend path for %s: %w", nextSlice.PotID, err)
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
				return nil, fmt.Errorf("select removed-pot spend path for %s: %w", potID, err)
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
	sort.SliceStable(plan.Inputs, func(left, right int) bool {
		return fundingRefKey(plan.Inputs[left].Ref) < fundingRefKey(plan.Inputs[right].Ref)
	})
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
		if !hadPrev || !runtime.potSliceRefsReusableForTransition(table, transition, prevSlice, slice) {
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
