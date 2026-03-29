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
