package meshruntime

import (
	"encoding/hex"
	"strings"
	"testing"

	arklib "github.com/arkade-os/arkd/pkg/ark-lib"
	arkscript "github.com/arkade-os/arkd/pkg/ark-lib/script"
	"github.com/parkerpoker/parkerd/internal/tablecustody"
)

func TestVerifyCustodyRefsOnArkRejectsMismatchedTapscripts(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	host.config.UseMockSettlement = false
	host.custodyArkVerify = func(refs []tablecustody.VTXORef, requireSpendable bool) error {
		return nil
	}

	hostOwner, err := compressedPubkeyFromHex(host.walletID.PublicKeyHex)
	if err != nil {
		t.Fatalf("decode host owner pubkey: %v", err)
	}
	guestOwner, err := compressedPubkeyFromHex(guest.walletID.PublicKeyHex)
	if err != nil {
		t.Fatalf("decode guest owner pubkey: %v", err)
	}
	operator, err := compressedPubkeyFromHex(host.protocolIdentity.PublicKeyHex)
	if err != nil {
		t.Fatalf("decode operator pubkey: %v", err)
	}
	exitDelay := arklib.RelativeLocktime{Type: arklib.LocktimeTypeBlock, Value: 512}

	validScript := arkscript.NewDefaultVtxoScript(hostOwner, operator, exitDelay)
	validTapscripts, err := validScript.Encode()
	if err != nil {
		t.Fatalf("encode valid tapscripts: %v", err)
	}
	tapKey, _, err := validScript.TapTree()
	if err != nil {
		t.Fatalf("build valid tap tree: %v", err)
	}
	validPkScript, err := arkscript.P2TRScript(tapKey)
	if err != nil {
		t.Fatalf("derive valid p2tr script: %v", err)
	}

	wrongScript := arkscript.NewDefaultVtxoScript(guestOwner, operator, exitDelay)
	wrongTapscripts, err := wrongScript.Encode()
	if err != nil {
		t.Fatalf("encode mismatched tapscripts: %v", err)
	}

	validRef := tablecustody.VTXORef{
		AmountSats:  4_000,
		ArkIntentID: "ark-intent-1",
		ArkTxID:     "ark-tx-1",
		Script:      hex.EncodeToString(validPkScript),
		Tapscripts:  validTapscripts,
		TxID:        strings.Repeat("01", 32),
		VOut:        0,
	}
	if err := host.verifyCustodyRefsOnArk([]tablecustody.VTXORef{validRef}, true); err != nil {
		t.Fatalf("expected valid tapscripts to verify, got %v", err)
	}

	mismatchedRef := validRef
	mismatchedRef.Tapscripts = wrongTapscripts
	if err := host.verifyCustodyRefsOnArk([]tablecustody.VTXORef{mismatchedRef}, true); err == nil || !strings.Contains(err.Error(), "tapscripts do not match") {
		t.Fatalf("expected mismatched tapscripts to be rejected, got %v", err)
	}
}

func TestValidateCustodyTransitionArkProofAllowsIndexedRefsWithoutPerRefArkTxID(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	host.config.UseMockSettlement = false
	host.custodyArkVerify = func(refs []tablecustody.VTXORef, requireSpendable bool) error {
		return nil
	}

	hostOwner, err := compressedPubkeyFromHex(host.walletID.PublicKeyHex)
	if err != nil {
		t.Fatalf("decode host owner pubkey: %v", err)
	}
	operator, err := compressedPubkeyFromHex(host.protocolIdentity.PublicKeyHex)
	if err != nil {
		t.Fatalf("decode operator pubkey: %v", err)
	}
	exitDelay := arklib.RelativeLocktime{Type: arklib.LocktimeTypeBlock, Value: 512}
	script := arkscript.NewDefaultVtxoScript(hostOwner, operator, exitDelay)
	tapscripts, err := script.Encode()
	if err != nil {
		t.Fatalf("encode tapscripts: %v", err)
	}
	tapKey, _, err := script.TapTree()
	if err != nil {
		t.Fatalf("build tap tree: %v", err)
	}
	pkScript, err := arkscript.P2TRScript(tapKey)
	if err != nil {
		t.Fatalf("derive p2tr script: %v", err)
	}

	transition := tablecustody.CustodyTransition{
		Kind:        tablecustody.TransitionKindBuyInLock,
		ArkIntentID: "ark-intent-1",
		ArkTxID:     "batch-tx-1",
		NextState: tablecustody.CustodyState{
			StackClaims: []tablecustody.StackClaim{
				{
					PlayerID:   "player-1",
					AmountSats: 4_000,
					VTXORefs: []tablecustody.VTXORef{
						{
							AmountSats:  4_000,
							ArkIntentID: "ark-intent-1",
							ArkTxID:     "",
							Script:      hex.EncodeToString(pkScript),
							Tapscripts:  tapscripts,
							TxID:        strings.Repeat("01", 32),
							VOut:        0,
						},
					},
				},
			},
		},
		Proof: tablecustody.CustodyProof{
			VTXORefs: []tablecustody.VTXORef{
				{
					AmountSats:  4_000,
					ArkIntentID: "ark-intent-1",
					ArkTxID:     "",
					Script:      hex.EncodeToString(pkScript),
					Tapscripts:  tapscripts,
					TxID:        strings.Repeat("01", 32),
					VOut:        0,
				},
			},
		},
	}

	if err := validateAcceptedCustodyRefs(nil, transition, true); err != nil {
		t.Fatalf("expected structural validation to accept blank per-ref Ark tx id, got %v", err)
	}
	if err := host.validateCustodyTransitionArkProof(nil, transition, true); err != nil {
		t.Fatalf("expected Ark proof validation to accept blank per-ref Ark tx id, got %v", err)
	}
}

func TestSelectCustodySpendPathFallsBackToRefOwnerBeforeSeatReplication(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	config, err := host.arkCustodyConfig()
	if err != nil {
		t.Fatalf("ark custody config: %v", err)
	}
	guestOwner, err := compressedPubkeyFromHex(guest.walletID.PublicKeyHex)
	if err != nil {
		t.Fatalf("decode guest owner pubkey: %v", err)
	}
	operator, err := compressedPubkeyFromHex(config.SignerPubkeyHex)
	if err != nil {
		t.Fatalf("decode operator pubkey: %v", err)
	}
	script := arkscript.NewDefaultVtxoScript(guestOwner, operator, config.UnilateralExitDelay)
	tapscripts, err := script.Encode()
	if err != nil {
		t.Fatalf("encode tapscripts: %v", err)
	}
	tapKey, _, err := script.TapTree()
	if err != nil {
		t.Fatalf("build tap tree: %v", err)
	}
	pkScript, err := arkscript.P2TRScript(tapKey)
	if err != nil {
		t.Fatalf("derive p2tr script: %v", err)
	}

	ref := tablecustody.VTXORef{
		AmountSats:    4_000,
		OwnerPlayerID: guest.walletID.PlayerID,
		Script:        hex.EncodeToString(pkScript),
		Tapscripts:    tapscripts,
		TxID:          strings.Repeat("02", 32),
		VOut:          0,
	}
	path, err := host.selectCustodySpendPath(nativeTableState{}, ref, []string{guest.walletID.PlayerID}, false)
	if err != nil {
		t.Fatalf("expected spend path selection to fall back to ref owner, got %v", err)
	}
	if len(path.PlayerIDs) != 1 || path.PlayerIDs[0] != guest.walletID.PlayerID {
		t.Fatalf("expected fallback player ownership, got %+v", path.PlayerIDs)
	}
}

func TestSelectCustodySpendPathReturnsIndependentCachedResults(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")

	config, err := host.arkCustodyConfig()
	if err != nil {
		t.Fatalf("ark custody config: %v", err)
	}
	guestOwner, err := compressedPubkeyFromHex(guest.walletID.PublicKeyHex)
	if err != nil {
		t.Fatalf("decode guest owner pubkey: %v", err)
	}
	operator, err := compressedPubkeyFromHex(config.SignerPubkeyHex)
	if err != nil {
		t.Fatalf("decode operator pubkey: %v", err)
	}
	script := arkscript.NewDefaultVtxoScript(guestOwner, operator, config.UnilateralExitDelay)
	tapscripts, err := script.Encode()
	if err != nil {
		t.Fatalf("encode tapscripts: %v", err)
	}
	tapKey, _, err := script.TapTree()
	if err != nil {
		t.Fatalf("build tap tree: %v", err)
	}
	pkScript, err := arkscript.P2TRScript(tapKey)
	if err != nil {
		t.Fatalf("derive p2tr script: %v", err)
	}

	ref := tablecustody.VTXORef{
		AmountSats:    4_000,
		OwnerPlayerID: guest.walletID.PlayerID,
		Script:        hex.EncodeToString(pkScript),
		Tapscripts:    tapscripts,
		TxID:          strings.Repeat("03", 32),
		VOut:          0,
	}
	first, err := host.selectCustodySpendPath(nativeTableState{}, ref, []string{guest.walletID.PlayerID}, false)
	if err != nil {
		t.Fatalf("select initial spend path: %v", err)
	}

	originalPKScript := hex.EncodeToString(first.PKScript)
	originalScript := hex.EncodeToString(first.Script)
	originalControlBlock := hex.EncodeToString(first.LeafProof.ControlBlock)
	originalLeafScript := hex.EncodeToString(first.LeafProof.Script)
	originalTapscript := first.Tapscripts[0]
	originalSigner := first.SignerXOnlyPubkeys[0]

	first.PKScript[0] ^= 0xff
	first.Script[0] ^= 0xff
	first.LeafProof.ControlBlock[0] ^= 0xff
	first.LeafProof.Script[0] ^= 0xff
	first.Tapscripts[0] = "mutated"
	first.SignerXOnlyPubkeys[0] = "mutated"

	second, err := host.selectCustodySpendPath(nativeTableState{}, ref, []string{guest.walletID.PlayerID}, false)
	if err != nil {
		t.Fatalf("select cached spend path: %v", err)
	}

	if got := hex.EncodeToString(second.PKScript); got != originalPKScript {
		t.Fatalf("expected cached pkScript %s, got %s", originalPKScript, got)
	}
	if got := hex.EncodeToString(second.Script); got != originalScript {
		t.Fatalf("expected cached witness script %s, got %s", originalScript, got)
	}
	if got := hex.EncodeToString(second.LeafProof.ControlBlock); got != originalControlBlock {
		t.Fatalf("expected cached control block %s, got %s", originalControlBlock, got)
	}
	if got := hex.EncodeToString(second.LeafProof.Script); got != originalLeafScript {
		t.Fatalf("expected cached leaf script %s, got %s", originalLeafScript, got)
	}
	if got := second.Tapscripts[0]; got != originalTapscript {
		t.Fatalf("expected cached tapscript %q, got %q", originalTapscript, got)
	}
	if got := second.SignerXOnlyPubkeys[0]; got != originalSigner {
		t.Fatalf("expected cached signer %q, got %q", originalSigner, got)
	}
}
