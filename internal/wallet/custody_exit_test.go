package wallet

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/wire"
	"github.com/parkerpoker/parkerd/internal/tablecustody"
)

func TestPackageBroadcastTxIDsPrefersChildBeforeParent(t *testing.T) {
	t.Parallel()

	parent := wire.NewMsgTx(2)
	parent.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{Index: 1},
	})
	parent.AddTxOut(&wire.TxOut{Value: 10, PkScript: []byte{txscript.OP_TRUE}})
	parentHex, err := encodeCustodyExitTxHex(parent)
	if err != nil {
		t.Fatalf("encode parent tx: %v", err)
	}

	child := wire.NewMsgTx(2)
	child.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  parent.TxHash(),
			Index: 0,
		},
	})
	child.AddTxOut(&wire.TxOut{Value: 9, PkScript: []byte{txscript.OP_TRUE}})
	childHex, err := encodeCustodyExitTxHex(child)
	if err != nil {
		t.Fatalf("encode child tx: %v", err)
	}

	txids, err := packageBroadcastTxIDs(childHex, parentHex)
	if err != nil {
		t.Fatalf("package broadcast tx ids: %v", err)
	}
	if len(txids) != 2 {
		t.Fatalf("expected two broadcast tx ids, got %d (%v)", len(txids), txids)
	}
	if txids[0] != child.TxHash().String() {
		t.Fatalf("expected child txid %s first, got %v", child.TxHash(), txids)
	}
	if txids[1] != parent.TxHash().String() {
		t.Fatalf("expected parent txid %s second, got %v", parent.TxHash(), txids)
	}
}

func TestPackageBroadcastTxIDsRejectsInvalidHex(t *testing.T) {
	t.Parallel()

	if _, err := packageBroadcastTxIDs("not-a-tx"); err == nil {
		t.Fatal("expected invalid tx hex to fail")
	}
}

func TestWaitForExplorerTxHexRetriesUntilVisible(t *testing.T) {
	t.Parallel()

	attempts := 0
	txHex, err := waitForExplorerTxHex(func(txid string) (string, error) {
		attempts++
		if txid != "txid" {
			t.Fatalf("expected txid lookup, got %q", txid)
		}
		if attempts < 3 {
			return "", errors.New("Transaction not found")
		}
		return "deadbeef", nil
	}, "txid", 50*time.Millisecond, time.Millisecond)
	if err != nil {
		t.Fatalf("expected explorer retry to succeed, got %v", err)
	}
	if txHex != "deadbeef" {
		t.Fatalf("expected tx hex deadbeef, got %q", txHex)
	}
	if attempts != 3 {
		t.Fatalf("expected three lookup attempts, got %d", attempts)
	}
}

func TestWaitForExplorerTxHexReturnsLastErrorAfterTimeout(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("Transaction not found")
	txHex, err := waitForExplorerTxHex(func(string) (string, error) {
		return "", sentinel
	}, "txid", 5*time.Millisecond, time.Millisecond)
	if err == nil {
		t.Fatal("expected explorer retry timeout to return an error")
	}
	if txHex != "" {
		t.Fatalf("expected empty tx hex on timeout, got %q", txHex)
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected timeout to return sentinel error, got %v", err)
	}
}

func TestVerifyUnilateralExitWitnessUsesArkTxidForSourceOutpoint(t *testing.T) {
	t.Parallel()

	ref := tablecustody.VTXORef{
		TxID:    strings.Repeat("1", 64),
		ArkTxID: strings.Repeat("2", 64),
		VOut:    0,
	}
	sourceHash, err := chainhash.NewHashFromStr(ref.ArkTxID)
	if err != nil {
		t.Fatalf("parse ark txid: %v", err)
	}

	parent := wire.NewMsgTx(2)
	parent.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  *sourceHash,
			Index: ref.VOut,
		},
	})
	parent.AddTxOut(&wire.TxOut{Value: 1, PkScript: []byte{txscript.OP_TRUE}})
	parentHex, err := encodeCustodyExitTxHex(parent)
	if err != nil {
		t.Fatalf("encode parent tx: %v", err)
	}

	summary, err := VerifyUnilateralExitWitness([]tablecustody.VTXORef{ref}, tablecustody.CustodyExitWitness{
		BroadcastTransactions: []tablecustody.CustodyExitTransaction{{
			TransactionHex: parentHex,
			TransactionID:  parent.TxHash().String(),
		}},
	})
	if err != nil {
		t.Fatalf("verify unilateral exit witness: %v", err)
	}
	if len(summary.BroadcastTxIDs) != 1 || summary.BroadcastTxIDs[0] != parent.TxHash().String() {
		t.Fatalf("expected verified broadcast txid %s, got %+v", parent.TxHash(), summary.BroadcastTxIDs)
	}
}
