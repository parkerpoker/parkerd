package meshruntime

import (
	"context"
	"path/filepath"
	"testing"

	arktree "github.com/arkade-os/arkd/pkg/ark-lib/tree"
	arkclient "github.com/arkade-os/go-sdk/client"
	cfg "github.com/parkerpoker/parkerd/internal/config"
	"github.com/parkerpoker/parkerd/internal/tablecustody"
	walletpkg "github.com/parkerpoker/parkerd/internal/wallet"
)

type fakeCustodyTransport struct {
	signatures []fakeTreeSignatureSubmission
}

type fakeTreeSignatureSubmission struct {
	batchID   string
	pubkeyHex string
}

func (f *fakeCustodyTransport) GetInfo(context.Context) (*arkclient.Info, error) {
	return nil, nil
}

func (f *fakeCustodyTransport) RegisterIntent(context.Context, string, string) (string, error) {
	return "", nil
}

func (f *fakeCustodyTransport) DeleteIntent(context.Context, string, string) error {
	return nil
}

func (f *fakeCustodyTransport) ConfirmRegistration(context.Context, string) error {
	return nil
}

func (f *fakeCustodyTransport) SubmitTreeNonces(context.Context, string, string, arktree.TreeNonces) error {
	return nil
}

func (f *fakeCustodyTransport) SubmitTreeSignatures(_ context.Context, batchID, cosignerPubkey string, _ arktree.TreePartialSigs) error {
	f.signatures = append(f.signatures, fakeTreeSignatureSubmission{
		batchID:   batchID,
		pubkeyHex: cosignerPubkey,
	})
	return nil
}

func (f *fakeCustodyTransport) SubmitSignedForfeitTxs(context.Context, []string, string) error {
	return nil
}

func (f *fakeCustodyTransport) GetEventStream(context.Context, []string) (<-chan arkclient.BatchEventChannel, func(), error) {
	return nil, func() {}, nil
}

func (f *fakeCustodyTransport) SubmitTx(context.Context, string, []string) (string, string, []string, error) {
	return "", "", nil, nil
}

func (f *fakeCustodyTransport) FinalizeTx(context.Context, string, []string) error {
	return nil
}

func (f *fakeCustodyTransport) GetTransactionsStream(context.Context) (<-chan arkclient.TransactionEvent, func(), error) {
	return nil, func() {}, nil
}

func (f *fakeCustodyTransport) Close() {}

type fakeTreeSignerSession struct {
	aggregateComplete bool
	aggregated        arktree.TreeNonces
	publicKey         string
	signCount         int
}

func (f *fakeTreeSignerSession) Init([]byte, int64, *arktree.TxTree) error {
	return nil
}

func (f *fakeTreeSignerSession) GetPublicKey() string {
	return f.publicKey
}

func (f *fakeTreeSignerSession) GetNonces() (arktree.TreeNonces, error) {
	return nil, nil
}

func (f *fakeTreeSignerSession) SetAggregatedNonces(nonces arktree.TreeNonces) {
	f.aggregated = nonces
}

func (f *fakeTreeSignerSession) AggregateNonces(string, map[string]*arktree.Musig2Nonce) (bool, error) {
	return f.aggregateComplete, nil
}

func (f *fakeTreeSignerSession) Sign() (arktree.TreePartialSigs, error) {
	f.signCount++
	return arktree.TreePartialSigs{}, nil
}

func newBootstrapOnlyMeshRuntime(t *testing.T, profileName string) *meshRuntime {
	t.Helper()

	rootDir := t.TempDir()
	runtimeConfig, err := cfg.ResolveRuntimeConfig(map[string]string{
		"cache-type":    "memory",
		"daemon-dir":    filepath.Join(rootDir, "daemons"),
		"datadir":       rootDir,
		"db-path":       filepath.Join(rootDir, "storage", "core.sqlite"),
		"event-db-path": filepath.Join(rootDir, "storage", "events.badger"),
		"mock":          "true",
		"network":       "regtest",
		"peer-port":     "0",
		"profile-dir":   filepath.Join(rootDir, "profiles"),
		"run-dir":       filepath.Join(rootDir, "runs"),
	})
	if err != nil {
		t.Fatalf("resolve runtime config: %v", err)
	}

	runtime, err := newMeshRuntime(profileName, runtimeConfig, "player")
	if err != nil {
		t.Fatalf("new mesh runtime: %v", err)
	}
	runtime.mu.Lock()
	if err := runtime.ensureBootstrapLocked(profileName, ""); err != nil {
		runtime.mu.Unlock()
		t.Fatalf("bootstrap mesh runtime %s: %v", profileName, err)
	}
	runtime.mu.Unlock()
	t.Cleanup(func() {
		_ = runtime.Close()
	})
	return runtime
}

func TestCustodyBatchEventsHandlerSignsAggregatedNonces(t *testing.T) {
	runtime := newBootstrapOnlyMeshRuntime(t, "signer")
	transport := &fakeCustodyTransport{}
	signer := &fakeTreeSignerSession{publicKey: "signer-pubkey"}

	handler := &custodyBatchEventsHandler{
		runtime: runtime,
		table: nativeTableState{
			Config: NativeMeshTableConfig{TableID: "table-1"},
		},
		derivationPath: "parker/custody/test",
		requestKey:     "transition-1",
		signerPubkeys: map[string]string{
			runtime.walletID.PlayerID: signer.publicKey,
		},
		signerSessions: map[string]walletpkg.CustodySignerSession{
			runtime.walletID.PlayerID: {
				DerivationPath: "parker/custody/test",
				PublicKeyHex:   signer.publicKey,
				Session:        signer,
			},
		},
		transport: transport,
	}

	signed, err := handler.OnTreeNoncesAggregated(context.Background(), arkclient.TreeNoncesAggregatedEvent{
		Id: "batch-1",
		Nonces: arktree.TreeNonces{
			"tx-1": &arktree.Musig2Nonce{},
		},
	})
	if err != nil {
		t.Fatalf("sign aggregated nonces: %v", err)
	}
	if !signed {
		t.Fatal("expected aggregated nonces handler to report signatures submitted")
	}
	if signer.signCount != 1 {
		t.Fatalf("expected one signature submission, got %d", signer.signCount)
	}
	if len(transport.signatures) != 1 {
		t.Fatalf("expected one transport signature submission, got %d", len(transport.signatures))
	}
	if transport.signatures[0].batchID != "batch-1" || transport.signatures[0].pubkeyHex != signer.publicKey {
		t.Fatalf("unexpected submitted signature metadata: %+v", transport.signatures[0])
	}
	if signer.aggregated["tx-1"] == nil {
		t.Fatal("expected aggregated tree nonces to be stored on the signer session")
	}
}

func TestHandleCustodySignerAggregatedNoncesFromPeerSubmitsSignatures(t *testing.T) {
	runtime := newBootstrapOnlyMeshRuntime(t, "guest")
	tableID := "table-aggregated-nonces"
	table := nativeTableState{
		Config: NativeMeshTableConfig{TableID: tableID},
		LatestCustodyState: &tablecustody.CustodyState{
			StateHash: "state-1",
		},
	}
	if err := runtime.store.writeTable(&table); err != nil {
		t.Fatalf("write local table: %v", err)
	}

	transport := &fakeCustodyTransport{}
	runtime.arkTransportFactory = func() (arkclient.TransportClient, error) {
		return transport, nil
	}

	transitionHash := "transition-aggregated"
	derivationPath := custodySignerDerivationPath(transitionHash)
	signer := &fakeTreeSignerSession{publicKey: "guest-signer-pubkey"}
	key := custodySignerSessionKey(tableID, transitionHash, runtime.walletID.PlayerID, derivationPath)
	runtime.storeCustodySignerSession(key, walletpkg.CustodySignerSession{
		DerivationPath: derivationPath,
		PublicKeyHex:   signer.publicKey,
		Session:        signer,
	})
	runtime.storeCustodySignerAuthorization(key, custodySignerAuthorization{
		ExpectedPrevStateHash: "state-1",
		TransitionHash:        transitionHash,
	})

	response, err := runtime.handleCustodySignerAggregatedNoncesFromPeer(nativeCustodySignerAggregatedNoncesRequest{
		BatchID:        "batch-2",
		DerivationPath: derivationPath,
		Nonces: arktree.TreeNonces{
			"tx-2": &arktree.Musig2Nonce{},
		},
		PlayerID:       runtime.walletID.PlayerID,
		TableID:        tableID,
		TransitionHash: transitionHash,
	})
	if err != nil {
		t.Fatalf("handle aggregated nonces from peer: %v", err)
	}
	if !response.OK {
		t.Fatal("expected aggregated nonces request to be acknowledged")
	}
	if signer.signCount != 1 {
		t.Fatalf("expected one signature submission, got %d", signer.signCount)
	}
	if len(transport.signatures) != 1 {
		t.Fatalf("expected one transport signature submission, got %d", len(transport.signatures))
	}
	if transport.signatures[0].batchID != "batch-2" || transport.signatures[0].pubkeyHex != signer.publicKey {
		t.Fatalf("unexpected submitted signature metadata: %+v", transport.signatures[0])
	}
	if signer.aggregated["tx-2"] == nil {
		t.Fatal("expected aggregated tree nonces to be stored on the signer session")
	}
	if _, ok := runtime.loadCustodySignerSession(key); ok {
		t.Fatal("expected custody signer session to be cleared after signature submission")
	}
	if _, ok := runtime.loadCustodySignerAuthorization(key); ok {
		t.Fatal("expected custody signer authorization to be cleared after signature submission")
	}
}

func TestHandleCustodySignerNoncesFromPeerReportsIncompleteAggregation(t *testing.T) {
	runtime := newBootstrapOnlyMeshRuntime(t, "guest")
	tableID := "table-nonces"
	table := nativeTableState{
		Config: NativeMeshTableConfig{TableID: tableID},
		LatestCustodyState: &tablecustody.CustodyState{
			StateHash: "state-1",
		},
	}
	if err := runtime.store.writeTable(&table); err != nil {
		t.Fatalf("write local table: %v", err)
	}

	transport := &fakeCustodyTransport{}
	runtime.arkTransportFactory = func() (arkclient.TransportClient, error) {
		return transport, nil
	}

	transitionHash := "transition-nonces"
	derivationPath := custodySignerDerivationPath(transitionHash)
	signer := &fakeTreeSignerSession{
		aggregateComplete: false,
		publicKey:         "guest-signer-pubkey",
	}
	key := custodySignerSessionKey(tableID, transitionHash, runtime.walletID.PlayerID, derivationPath)
	runtime.storeCustodySignerSession(key, walletpkg.CustodySignerSession{
		DerivationPath: derivationPath,
		PublicKeyHex:   signer.publicKey,
		Session:        signer,
	})
	runtime.storeCustodySignerAuthorization(key, custodySignerAuthorization{
		ExpectedPrevStateHash: "state-1",
		TransitionHash:        transitionHash,
	})

	response, err := runtime.handleCustodySignerNoncesFromPeer(nativeCustodySignerNoncesRequest{
		BatchID:        "batch-3",
		DerivationPath: derivationPath,
		Nonces: map[string]*arktree.Musig2Nonce{
			"nonce-1": &arktree.Musig2Nonce{},
		},
		PlayerID:       runtime.walletID.PlayerID,
		TableID:        tableID,
		TxID:           "tx-3",
		TransitionHash: transitionHash,
	})
	if err != nil {
		t.Fatalf("handle nonces from peer: %v", err)
	}
	if !response.OK {
		t.Fatal("expected nonce request to be acknowledged")
	}
	if response.Signed {
		t.Fatal("expected incomplete nonce aggregation to report Signed=false")
	}
	if signer.signCount != 0 {
		t.Fatalf("expected no signatures to be submitted before aggregation completes, got %d", signer.signCount)
	}
	if len(transport.signatures) != 0 {
		t.Fatalf("expected no transport signature submissions, got %d", len(transport.signatures))
	}
	if _, ok := runtime.loadCustodySignerSession(key); !ok {
		t.Fatal("expected custody signer session to remain available while aggregation is incomplete")
	}
}
