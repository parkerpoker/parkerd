package meshruntime

import (
	"encoding/hex"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	arklib "github.com/arkade-os/arkd/pkg/ark-lib"
	"github.com/btcsuite/btcd/btcutil/psbt"
	"github.com/parkerpoker/parkerd/internal/game"
	"github.com/parkerpoker/parkerd/internal/tablecustody"
	walletpkg "github.com/parkerpoker/parkerd/internal/wallet"
)

type turnChallengeChainOracle struct {
	mu     sync.Mutex
	tip    walletpkg.ChainTipStatus
	tipErr error
	tx     map[string]walletpkg.ChainTransactionStatus
	txErr  map[string]error
}

func newTurnChallengeChainOracle() *turnChallengeChainOracle {
	return &turnChallengeChainOracle{
		tip: walletpkg.ChainTipStatus{
			ObservedAt: nowISO(),
		},
		tx:    map[string]walletpkg.ChainTransactionStatus{},
		txErr: map[string]error{},
	}
}

func (oracle *turnChallengeChainOracle) chainTip(profileName string) (walletpkg.ChainTipStatus, error) {
	oracle.mu.Lock()
	defer oracle.mu.Unlock()
	if oracle.tipErr != nil {
		return walletpkg.ChainTipStatus{}, oracle.tipErr
	}
	return oracle.tip, nil
}

func (oracle *turnChallengeChainOracle) chainTx(profileName, txid string) (walletpkg.ChainTransactionStatus, error) {
	oracle.mu.Lock()
	defer oracle.mu.Unlock()
	if err, ok := oracle.txErr[txid]; ok && err != nil {
		return walletpkg.ChainTransactionStatus{}, err
	}
	status, ok := oracle.tx[txid]
	if !ok {
		return walletpkg.ChainTransactionStatus{}, fmt.Errorf("missing tx status for %s", txid)
	}
	return status, nil
}

func (oracle *turnChallengeChainOracle) setTip(height int64) {
	oracle.mu.Lock()
	defer oracle.mu.Unlock()
	oracle.tip = walletpkg.ChainTipStatus{
		Height:     height,
		ObservedAt: nowISO(),
	}
	oracle.tipErr = nil
}

func (oracle *turnChallengeChainOracle) setTipError(err error) {
	oracle.mu.Lock()
	defer oracle.mu.Unlock()
	oracle.tipErr = err
}

func (oracle *turnChallengeChainOracle) setTxStatus(status walletpkg.ChainTransactionStatus) {
	oracle.mu.Lock()
	defer oracle.mu.Unlock()
	if oracle.tx == nil {
		oracle.tx = map[string]walletpkg.ChainTransactionStatus{}
	}
	status.ObservedAt = nowISO()
	oracle.tx[status.TxID] = status
	delete(oracle.txErr, status.TxID)
}

func (oracle *turnChallengeChainOracle) setTxError(txid string, err error) {
	oracle.mu.Lock()
	defer oracle.mu.Unlock()
	if oracle.txErr == nil {
		oracle.txErr = map[string]error{}
	}
	oracle.txErr[txid] = err
}

func installDirectTableSync(t *testing.T, runtimes ...*meshRuntime) {
	t.Helper()

	byURL := map[string]*meshRuntime{}
	for _, runtime := range runtimes {
		if runtime == nil {
			continue
		}
		byURL[runtime.selfPeerURL()] = runtime
	}
	for _, runtime := range runtimes {
		if runtime == nil {
			continue
		}
		runtime.tableSyncSender = func(peerURL string, input nativeTableSyncRequest) error {
			target := byURL[peerURL]
			if target == nil {
				return nil
			}
			err := target.acceptTableSync(input)
			if err != nil {
				t.Errorf("table sync %s -> %s failed: %v", input.SenderPeerID, peerURL, err)
			}
			return err
		}
	}
}

func bridgeHostToGuestSync(host, guest *meshRuntime) {
	if host == nil || guest == nil {
		return
	}
	host.tableSyncSender = func(peerURL string, input nativeTableSyncRequest) error {
		if peerURL == guest.selfPeerURL() {
			return guest.acceptTableSync(input)
		}
		return nil
	}
}

func waitForPendingTurnChallenge(t *testing.T, runtimes []*meshRuntime, reader *meshRuntime, tableID string) nativeTableState {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		table := mustReadNativeTable(t, reader, tableID)
		if turnChallengeMatchesTable(table, table.PendingTurnChallenge) {
			return table
		}
		for _, runtime := range runtimes {
			if runtime != nil {
				runtime.Tick()
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	_ = mustReadNativeTable(t, reader, tableID)
	t.Fatalf("timed out waiting for pending turn challenge on table %s", tableID)
	return nativeTableState{}
}

func waitForPendingTurnMenuWithChallengeEnvelope(t *testing.T, runtimes []*meshRuntime, reader *meshRuntime, tableID string) nativeTableState {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		table := mustReadNativeTable(t, reader, tableID)
		if turnMenuMatchesTable(table, table.PendingTurnMenu) && table.PendingTurnMenu.ChallengeEnvelope != nil {
			return table
		}
		for _, runtime := range runtimes {
			if runtime != nil {
				runtime.Tick()
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	_ = mustReadNativeTable(t, reader, tableID)
	t.Fatalf("timed out waiting for pending turn menu challenge envelope on table %s", tableID)
	return nativeTableState{}
}

func preferredChallengeResolutionOption(menu *NativePendingTurnMenu) NativeActionMenuOption {
	if menu == nil || len(menu.Options) == 0 {
		return NativeActionMenuOption{}
	}
	for _, option := range menu.Options {
		if option.Action.Type == game.ActionCheck || option.Action.Type == game.ActionCall {
			return option
		}
	}
	for _, option := range menu.Options {
		if option.Action.Type != game.ActionFold {
			return option
		}
	}
	return menu.Options[0]
}

func challengeBundleUsesSignerXOnly(t *testing.T, bundle tablecustody.CustodyChallengeBundle, xOnlyHex string) bool {
	t.Helper()
	packet, err := psbt.NewFromRawBytes(strings.NewReader(bundle.SignedPSBT), true)
	if err != nil {
		t.Fatalf("parse challenge bundle psbt: %v", err)
	}
	for _, input := range packet.Inputs {
		for _, signature := range input.TaprootScriptSpendSig {
			if signature != nil && strings.EqualFold(hex.EncodeToString(signature.XOnlyPubKey), xOnlyHex) {
				return true
			}
		}
	}
	return false
}

type preparedTurnChallengeEscape struct {
	csvBlocks   int64
	escapeTxID  string
	openTxID    string
	sourceState tablecustody.CustodyState
	table       nativeTableState
	tableID     string
}

func prepareBlockBasedTurnChallengeEscape(t *testing.T) (*turnChallengeChainOracle, *meshRuntime, *meshRuntime, preparedTurnChallengeEscape) {
	t.Helper()

	oracle := newTurnChallengeChainOracle()
	host := newMeshTestRuntime(
		t,
		"host",
		withMeshTestChainTipStatus(oracle.chainTip),
		withMeshTestChainTxStatus(oracle.chainTx),
	)
	guest := newMeshTestRuntime(
		t,
		"guest",
		withMeshTestChainTipStatus(oracle.chainTip),
		withMeshTestChainTxStatus(oracle.chainTx),
	)
	host.custodyRecoveryExecute = func(profileName, signedPSBT string) (walletpkg.CustodyRecoveryResult, error) {
		return syntheticExecuteCustodyRecoveryForTest(signedPSBT)
	}
	guest.custodyRecoveryExecute = func(profileName, signedPSBT string) (walletpkg.CustodyRecoveryResult, error) {
		return syntheticExecuteCustodyRecoveryForTest(signedPSBT)
	}
	bridgeHostToGuestSync(host, guest)

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)
	table := waitForLocalCanAct(t, []*meshRuntime{host, guest}, host, tableID)
	sourceState := cloneJSON(*table.LatestCustodyState)

	waitForTurnActionDeadlineToExpire(t, []*meshRuntime{host, guest}, host, tableID)
	if _, err := host.OpenTurnChallenge(tableID); err != nil {
		t.Fatalf("open turn challenge: %v", err)
	}
	table = waitForPendingTurnChallenge(t, []*meshRuntime{host, guest}, host, tableID)
	if table.PendingTurnChallenge == nil {
		t.Fatal("expected pending turn challenge after open")
	}
	if strings.TrimSpace(table.PendingTurnChallenge.EscapeEligibleAt) != "" {
		t.Fatalf("expected block-based escape eligibility timestamp to stay empty, got %+v", table.PendingTurnChallenge)
	}
	if table.PendingTurnMenu == nil || table.PendingTurnMenu.ChallengeEnvelope == nil {
		t.Fatal("expected pending challenge envelope after open")
	}
	openTransition := table.CustodyTransitions[len(table.CustodyTransitions)-1]
	if openTransition.Kind != tablecustody.TransitionKindTurnChallengeOpen {
		t.Fatalf("expected latest custody transition %q, got %q", tablecustody.TransitionKindTurnChallengeOpen, openTransition.Kind)
	}
	openTxID := openTransition.Proof.ChallengeWitness.TransactionID
	if strings.TrimSpace(openTxID) == "" {
		t.Fatal("expected turn challenge open txid")
	}
	_, escapeTxID, err := challengeOutputRefsFromBundle(table.PendingTurnMenu.ChallengeEnvelope.EscapeBundle)
	if err != nil {
		t.Fatalf("derive turn challenge escape txid: %v", err)
	}
	openTable := cloneJSON(table)
	openState := cloneJSON(openTransition.NextState)
	openTable.LatestCustodyState = &openState
	spendPath, err := host.selectTurnChallengeExitSpendPath(openTable, table.PendingTurnChallenge.ChallengeRef)
	if err != nil {
		t.Fatalf("select turn challenge escape spend path: %v", err)
	}
	if spendPath.CSVLocktime.Type != arklib.LocktimeTypeBlock {
		t.Fatalf("expected block-based turn challenge escape locktime, got %+v", spendPath.CSVLocktime)
	}

	return oracle, host, guest, preparedTurnChallengeEscape{
		csvBlocks:   int64(spendPath.CSVLocktime.Value),
		escapeTxID:  escapeTxID,
		openTxID:    openTxID,
		sourceState: sourceState,
		table:       table,
		tableID:     tableID,
	}
}

func TestTurnChallengeEscapeEligibleAtOnlyUsesSecondBasedCSV(t *testing.T) {
	openedAt := "2026-04-03T12:00:00Z"
	spendPath := custodySpendPath{
		UsesCSVLocktime: true,
		CSVLocktime:     arklib.RelativeLocktime{Type: arklib.LocktimeTypeSecond, Value: 30},
	}
	if got, want := turnChallengeEscapeEligibleAt(openedAt, spendPath), addMillis(openedAt, 30_000); got != want {
		t.Fatalf("expected second-based challenge escape eligibility %q, got %q", want, got)
	}
	spendPath.CSVLocktime = arklib.RelativeLocktime{Type: arklib.LocktimeTypeBlock, Value: 30}
	if got := turnChallengeEscapeEligibleAt(openedAt, spendPath); got != "" {
		t.Fatalf("expected block-based challenge escape eligibility timestamp to stay empty, got %q", got)
	}
}

func TestTurnChallengeEscapeEligibleHeightUsesOpenConfirmationHeight(t *testing.T) {
	spendPath := custodySpendPath{
		UsesCSVLocktime: true,
		CSVLocktime:     arklib.RelativeLocktime{Type: arklib.LocktimeTypeBlock, Value: 144},
	}
	height, ok := turnChallengeEscapeEligibleHeight(700_000, spendPath)
	if !ok {
		t.Fatal("expected block-based CSV locktime to produce an eligible height")
	}
	if height != 700_144 {
		t.Fatalf("expected eligible height 700144, got %d", height)
	}
}

func waitForTurnActionDeadlineToExpire(t *testing.T, runtimes []*meshRuntime, reader *meshRuntime, tableID string) nativeTableState {
	t.Helper()

	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		table := mustReadNativeTable(t, reader, tableID)
		if turnMenuMatchesTable(table, table.PendingTurnMenu) &&
			table.PendingTurnChallenge == nil &&
			elapsedMillis(table.PendingTurnMenu.ActionDeadlineAt) >= 0 &&
			turnChallengeOpenReady(table.PendingTurnMenu) {
			return table
		}
		for _, runtime := range runtimes {
			if runtime != nil {
				runtime.Tick()
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	_ = mustReadNativeTable(t, reader, tableID)
	t.Fatalf("timed out waiting for turn action deadline to expire on table %s", tableID)
	return nativeTableState{}
}

func waitForLatestCustodyTransitionKind(t *testing.T, runtimes []*meshRuntime, reader *meshRuntime, tableID string, kind tablecustody.TransitionKind) nativeTableState {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		table := mustReadNativeTable(t, reader, tableID)
		if len(table.CustodyTransitions) > 0 && table.CustodyTransitions[len(table.CustodyTransitions)-1].Kind == kind {
			return table
		}
		for _, runtime := range runtimes {
			if runtime != nil {
				runtime.Tick()
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	_ = mustReadNativeTable(t, reader, tableID)
	t.Fatalf("timed out waiting for latest custody transition %s on table %s", kind, tableID)
	return nativeTableState{}
}

func waitForCustodyTransitionKindPresent(t *testing.T, runtimes []*meshRuntime, reader *meshRuntime, tableID string, kind tablecustody.TransitionKind) nativeTableState {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		table := mustReadNativeTable(t, reader, tableID)
		for _, transition := range table.CustodyTransitions {
			if transition.Kind == kind {
				return table
			}
		}
		for _, runtime := range runtimes {
			if runtime != nil {
				runtime.Tick()
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	_ = mustReadNativeTable(t, reader, tableID)
	t.Fatalf("timed out waiting for custody transition %s on table %s", kind, tableID)
	return nativeTableState{}
}

func TestTurnChallengeTimeoutModeDefaults(t *testing.T) {
	if got := turnTimeoutModeFromCreateInput(map[string]any{}); got != turnTimeoutModeChainChallenge {
		t.Fatalf("expected new tables to default to %q, got %q", turnTimeoutModeChainChallenge, got)
	}
	if got := turnTimeoutModeFromCreateInput(map[string]any{"turnTimeoutMode": "invalid"}); got != turnTimeoutModeChainChallenge {
		t.Fatalf("expected invalid create input timeout mode to default to %q, got %q", turnTimeoutModeChainChallenge, got)
	}
	if got := turnTimeoutModeForTable(nativeTableState{}); got != turnTimeoutModeDirect {
		t.Fatalf("expected legacy tables without timeout mode to be treated as %q, got %q", turnTimeoutModeDirect, got)
	}
	if got := turnChallengeWindowMSForTable(nativeTableState{}); got != defaultTurnChallengeWindowMS {
		t.Fatalf("expected default challenge window %dms, got %d", defaultTurnChallengeWindowMS, got)
	}
}

func TestPendingTurnMenuCarriesDeterministicChallengeEnvelope(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")
	bridgeHostToGuestSync(host, guest)

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)

	hostTable := waitForLocalCanAct(t, []*meshRuntime{host, guest}, host, tableID)
	guestTable := waitForPendingTurnMenuWithChallengeEnvelope(t, []*meshRuntime{host, guest}, guest, tableID)

	if turnTimeoutModeForTable(hostTable) != turnTimeoutModeChainChallenge {
		t.Fatalf("expected started tables to default to %q", turnTimeoutModeChainChallenge)
	}
	if !turnMenuMatchesTable(hostTable, hostTable.PendingTurnMenu) || hostTable.PendingTurnMenu.ChallengeEnvelope == nil {
		t.Fatal("expected host pending turn menu to carry a challenge envelope")
	}
	if !turnMenuMatchesTable(guestTable, guestTable.PendingTurnMenu) || guestTable.PendingTurnMenu.ChallengeEnvelope == nil {
		t.Fatal("expected guest pending turn menu to carry a challenge envelope")
	}

	hostEnvelope := hostTable.PendingTurnMenu.ChallengeEnvelope
	guestEnvelope := guestTable.PendingTurnMenu.ChallengeEnvelope
	if !reflect.DeepEqual(*hostEnvelope, *guestEnvelope) {
		t.Fatalf("expected peers to converge on the same challenge envelope, host=%+v guest=%+v", *hostEnvelope, *guestEnvelope)
	}
	if hostEnvelope.OpenBundle.Kind != tablecustody.TransitionKindTurnChallengeOpen {
		t.Fatalf("expected open bundle kind %q, got %q", tablecustody.TransitionKindTurnChallengeOpen, hostEnvelope.OpenBundle.Kind)
	}
	if len(hostEnvelope.OpenBundle.SourceRefs) == 0 {
		t.Fatal("expected open bundle to consume the live turn bankroll refs")
	}
	if len(hostEnvelope.OptionResolutionBundles) != len(hostTable.PendingTurnMenu.Candidates) {
		t.Fatalf("expected %d option bundles, got %d", len(hostTable.PendingTurnMenu.Candidates), len(hostEnvelope.OptionResolutionBundles))
	}
	if hostEnvelope.TimeoutResolutionBundle.Kind != tablecustody.TransitionKindTimeout {
		t.Fatalf("expected timeout resolution bundle kind %q, got %q", tablecustody.TransitionKindTimeout, hostEnvelope.TimeoutResolutionBundle.Kind)
	}
	if hostEnvelope.TimeoutResolutionBundle.TxLocktime <= hostEnvelope.OpenBundle.TxLocktime {
		t.Fatalf("expected timeout resolution locktime %d to trail open locktime %d", hostEnvelope.TimeoutResolutionBundle.TxLocktime, hostEnvelope.OpenBundle.TxLocktime)
	}
	if hostEnvelope.EscapeBundle.Kind != tablecustody.TransitionKindTurnChallengeEscape {
		t.Fatalf("expected escape bundle kind %q, got %q", tablecustody.TransitionKindTurnChallengeEscape, hostEnvelope.EscapeBundle.Kind)
	}
	if len(hostEnvelope.EscapeBundle.SourceRefs) != 1 {
		t.Fatalf("expected escape bundle to spend the single challenge ref, got %+v", hostEnvelope.EscapeBundle.SourceRefs)
	}
}

func TestTurnChallengeBundlesStayPlayerOnlyWhileOrdinaryPotPathKeepsOperator(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")
	bridgeHostToGuestSync(host, guest)

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)
	table := waitForLocalCanAct(t, []*meshRuntime{host, guest}, host, tableID)
	if !turnMenuMatchesTable(table, table.PendingTurnMenu) || table.PendingTurnMenu.ChallengeEnvelope == nil {
		t.Fatal("expected actionable turn menu with challenge envelope")
	}
	config, err := host.arkCustodyConfig()
	if err != nil {
		t.Fatalf("ark custody config: %v", err)
	}
	operatorXOnly, err := xOnlyPubkeyHexFromCompressed(config.SignerPubkeyHex)
	if err != nil {
		t.Fatalf("operator xonly: %v", err)
	}
	envelope := table.PendingTurnMenu.ChallengeEnvelope
	if challengeBundleUsesSignerXOnly(t, envelope.OpenBundle, operatorXOnly) {
		t.Fatal("expected challenge-open bundle to avoid operator signatures")
	}
	for index, bundle := range envelope.OptionResolutionBundles {
		if challengeBundleUsesSignerXOnly(t, bundle, operatorXOnly) {
			t.Fatalf("expected challenge option bundle %d to avoid operator signatures", index)
		}
	}
	if challengeBundleUsesSignerXOnly(t, envelope.TimeoutResolutionBundle, operatorXOnly) {
		t.Fatal("expected challenge-timeout bundle to avoid operator signatures")
	}
	if challengeBundleUsesSignerXOnly(t, envelope.EscapeBundle, operatorXOnly) {
		t.Fatal("expected challenge-escape bundle to avoid operator signatures")
	}

	var potRef tablecustody.VTXORef
	foundPotRef := false
	for _, slice := range table.LatestCustodyState.PotSlices {
		if len(slice.VTXORefs) == 0 {
			continue
		}
		potRef = slice.VTXORefs[0]
		foundPotRef = true
		break
	}
	if !foundPotRef {
		t.Fatal("expected actionable turn state to carry a pot ref")
	}
	challengePath, err := host.selectTurnChallengeOpenSpendPath(table, potRef, table.PendingTurnMenu.ActionDeadlineAt)
	if err != nil {
		t.Fatalf("select turn challenge open spend path: %v", err)
	}
	if containsString(challengePath.SignerXOnlyPubkeys, operatorXOnly) {
		t.Fatalf("expected challenge-open spend path to exclude operator signer, got %+v", challengePath.SignerXOnlyPubkeys)
	}
	ordinaryPath, err := host.selectCustodySpendPath(table, potRef, activeTurnChallengePlayerIDs(table), false)
	if err != nil {
		t.Fatalf("select ordinary custody spend path: %v", err)
	}
	if !containsString(ordinaryPath.SignerXOnlyPubkeys, operatorXOnly) {
		t.Fatalf("expected ordinary pot spend path to keep operator signer, got %+v", ordinaryPath.SignerXOnlyPubkeys)
	}
}

func TestAcceptRemoteTableRejectsTamperedChallengeEnvelope(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")
	bridgeHostToGuestSync(host, guest)

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)
	table := waitForLocalCanAct(t, []*meshRuntime{host, guest}, host, tableID)

	if !turnMenuMatchesTable(table, table.PendingTurnMenu) || table.PendingTurnMenu.ChallengeEnvelope == nil {
		t.Fatal("expected actionable turn menu with challenge envelope")
	}

	tampered := cloneJSON(table)
	tampered.PendingTurnMenu.ChallengeEnvelope.OpenBundle.AuthorizedOutputs[0].AmountSats++
	tampered.PendingTurnMenu.ChallengeEnvelope.OpenBundle.BundleHash = tablecustody.HashCustodyChallengeBundle(tampered.PendingTurnMenu.ChallengeEnvelope.OpenBundle)

	if err := guest.acceptRemoteTable(tampered); err == nil || !strings.Contains(err.Error(), "challenge") {
		t.Fatalf("expected tampered challenge envelope to be rejected, got %v", err)
	}
}

func TestAcceptRemoteTableRejectsTamperedChallengeBundleKind(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")
	bridgeHostToGuestSync(host, guest)

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)
	table := waitForLocalCanAct(t, []*meshRuntime{host, guest}, host, tableID)

	if !turnMenuMatchesTable(table, table.PendingTurnMenu) || table.PendingTurnMenu.ChallengeEnvelope == nil {
		t.Fatal("expected actionable turn menu with challenge envelope")
	}

	tampered := cloneJSON(table)
	tampered.PendingTurnMenu.ChallengeEnvelope.OpenBundle.Kind = tablecustody.TransitionKindAction
	tampered.PendingTurnMenu.ChallengeEnvelope.OpenBundle.BundleHash = tablecustody.HashCustodyChallengeBundle(tampered.PendingTurnMenu.ChallengeEnvelope.OpenBundle)

	if err := guest.acceptRemoteTable(tampered); err == nil || !strings.Contains(err.Error(), "kind") {
		t.Fatalf("expected tampered challenge bundle kind to be rejected, got %v", err)
	}
}

func TestTurnChallengeOpenBlocksOrdinarySendAction(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")
	host.custodyRecoveryExecute = func(profileName, signedPSBT string) (walletpkg.CustodyRecoveryResult, error) {
		return syntheticExecuteCustodyRecoveryForTest(signedPSBT)
	}
	guest.custodyRecoveryExecute = func(profileName, signedPSBT string) (walletpkg.CustodyRecoveryResult, error) {
		return syntheticExecuteCustodyRecoveryForTest(signedPSBT)
	}

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)
	table := waitForLocalCanAct(t, []*meshRuntime{host, guest}, host, tableID)
	originalMenu := cloneJSON(*table.PendingTurnMenu)
	bridgeHostToGuestSync(host, guest)

	if _, err := host.OpenTurnChallenge(tableID); err == nil || !strings.Contains(err.Error(), "cannot be opened") {
		t.Fatalf("expected challenge-open to reject before D, got %v", err)
	}

	waitForTurnActionDeadlineToExpire(t, []*meshRuntime{host, guest}, host, tableID)
	if _, err := host.OpenTurnChallenge(tableID); err != nil {
		t.Fatalf("open turn challenge: %v", err)
	}
	table = waitForPendingTurnChallenge(t, []*meshRuntime{host, guest}, host, tableID)
	if len(table.CustodyTransitions) == 0 || table.CustodyTransitions[len(table.CustodyTransitions)-1].Kind != tablecustody.TransitionKindTurnChallengeOpen {
		t.Fatalf("expected latest custody transition to be %q, got %+v", tablecustody.TransitionKindTurnChallengeOpen, table.CustodyTransitions)
	}
	if table.CurrentHost.Peer.PeerID != host.selfPeerID() {
		t.Fatalf("expected current host to remain %s, got %s", host.selfPeerID(), table.CurrentHost.Peer.PeerID)
	}
	if table.PendingTurnChallenge == nil || table.PendingTurnChallenge.Status != turnChallengeStatusOpen {
		t.Fatalf("expected open pending turn challenge, got %+v", table.PendingTurnChallenge)
	}
	if table.PendingTurnChallenge.OpenedAt == "" || table.PendingTurnChallenge.TimeoutEligibleAt == "" {
		t.Fatalf("expected pending turn challenge timestamps, got %+v", table.PendingTurnChallenge)
	}
	if strings.TrimSpace(table.PendingTurnChallenge.EscapeEligibleAt) != "" {
		t.Fatalf("expected block-based pending turn challenge escape eligibility timestamp to stay empty, got %+v", table.PendingTurnChallenge)
	}
	if table.PendingTurnChallenge.DecisionIndex != originalMenu.DecisionIndex || table.PendingTurnChallenge.ActingPlayerID != originalMenu.ActingPlayerID {
		t.Fatalf("expected turn challenge to preserve turn identity, got %+v want decision=%d acting=%s", table.PendingTurnChallenge, originalMenu.DecisionIndex, originalMenu.ActingPlayerID)
	}
	if !turnMenuMatchesTable(table, table.PendingTurnMenu) {
		t.Fatal("expected pending turn menu to remain attached while challenge is open")
	}
	if !reflect.DeepEqual(table.PendingTurnMenu.Options, originalMenu.Options) {
		t.Fatalf("expected challenge-open to preserve the legal finite menu, got %+v want %+v", table.PendingTurnMenu.Options, originalMenu.Options)
	}
	if err := host.validateAcceptedPendingTurnChallenge(table, table.PendingTurnChallenge); err != nil {
		t.Fatalf("expected accepted pending turn challenge to validate, got %v", err)
	}
	if _, err := host.SendAction(tableID, originalMenu.Options[0].Action); err == nil || !strings.Contains(err.Error(), "ordinary SendAction is disabled") {
		t.Fatalf("expected ordinary SendAction to be disabled during an open challenge, got %v", err)
	}
}

func TestTurnChallengeOptionResolutionExecutesFromParticipant(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")
	host.custodyRecoveryExecute = func(profileName, signedPSBT string) (walletpkg.CustodyRecoveryResult, error) {
		return syntheticExecuteCustodyRecoveryForTest(signedPSBT)
	}
	guest.custodyRecoveryExecute = func(profileName, signedPSBT string) (walletpkg.CustodyRecoveryResult, error) {
		return syntheticExecuteCustodyRecoveryForTest(signedPSBT)
	}

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)
	table := waitForLocalCanAct(t, []*meshRuntime{host, guest}, host, tableID)
	originalMenu := cloneJSON(*table.PendingTurnMenu)
	optionID := preferredChallengeResolutionOption(table.PendingTurnMenu).OptionID
	expectedBundle, ok := findTurnCandidateByOption(&originalMenu, optionID)
	if !ok {
		t.Fatalf("expected option bundle %s", optionID)
	}
	bridgeHostToGuestSync(host, guest)

	waitForTurnActionDeadlineToExpire(t, []*meshRuntime{host, guest}, host, tableID)
	if _, err := host.OpenTurnChallenge(tableID); err != nil {
		t.Fatalf("open turn challenge: %v", err)
	}
	waitForPendingTurnChallenge(t, []*meshRuntime{host, guest}, host, tableID)

	if _, err := host.ResolveTurnChallenge(tableID, optionID); err != nil {
		t.Fatalf("resolve turn challenge option: %v", err)
	}

	table = waitForLatestCustodyTransitionKind(t, []*meshRuntime{host, guest}, host, tableID, tablecustody.TransitionKindAction)
	last := table.CustodyTransitions[len(table.CustodyTransitions)-1]
	if last.Proof.ChallengeBundle == nil || last.Proof.ChallengeWitness == nil {
		t.Fatalf("expected option resolution to carry challenge proof material, got %+v", last.Proof)
	}
	if last.Proof.SettlementWitness != nil {
		t.Fatalf("expected challenge option resolution to avoid settlement witness, got %+v", last.Proof.SettlementWitness)
	}
	if last.Proof.RequestHash != expectedBundle.Transition.Proof.RequestHash {
		t.Fatalf("expected challenge option request hash %q, got %q", expectedBundle.Transition.Proof.RequestHash, last.Proof.RequestHash)
	}
	if !reflect.DeepEqual(last.Approvals, expectedBundle.Transition.Approvals) {
		t.Fatalf("expected challenge option approvals to reuse the prebuilt candidate approvals")
	}
	if table.PendingTurnChallenge != nil {
		t.Fatalf("expected pending turn challenge to clear after option resolution, got %+v", table.PendingTurnChallenge)
	}
	guestTable := waitForLatestCustodyTransitionKind(t, []*meshRuntime{host, guest}, guest, tableID, tablecustody.TransitionKindAction)
	guestLast := guestTable.CustodyTransitions[len(guestTable.CustodyTransitions)-1]
	if !reflect.DeepEqual(guestLast.Approvals, expectedBundle.Transition.Approvals) {
		t.Fatalf("expected guest to accept the reused challenge option approvals")
	}
}

func TestTurnChallengeOptionResolutionExecutesInSyntheticRealMode(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")
	enableSyntheticRealMode(host, guest)

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)
	bridgeHostToGuestSync(host, guest)
	table := waitForLocalCanAct(t, []*meshRuntime{host, guest}, host, tableID)
	if !turnMenuMatchesTable(table, table.PendingTurnMenu) || table.PendingTurnMenu.ChallengeEnvelope == nil {
		t.Fatal("expected actionable turn menu with challenge envelope")
	}
	option := preferredChallengeResolutionOption(table.PendingTurnMenu)

	waitForTurnActionDeadlineToExpire(t, []*meshRuntime{host, guest}, host, tableID)
	if _, err := host.OpenTurnChallenge(tableID); err != nil {
		t.Fatalf("open turn challenge: %v", err)
	}
	waitForPendingTurnChallenge(t, []*meshRuntime{host, guest}, guest, tableID)

	if _, err := guest.ResolveTurnChallenge(tableID, option.OptionID); err != nil {
		t.Fatalf("resolve turn challenge option in synthetic-real mode: %v", err)
	}
	table = waitForLatestCustodyTransitionKind(t, []*meshRuntime{host, guest}, host, tableID, tablecustody.TransitionKindAction)
	last := table.CustodyTransitions[len(table.CustodyTransitions)-1]
	if last.Proof.ChallengeBundle == nil || last.Proof.ChallengeWitness == nil {
		t.Fatalf("expected synthetic-real challenge resolution to carry challenge proof material, got %+v", last.Proof)
	}
}

func TestTurnChallengeTimeoutResolutionExecutesAfterWindow(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")
	host.custodyRecoveryExecute = func(profileName, signedPSBT string) (walletpkg.CustodyRecoveryResult, error) {
		return syntheticExecuteCustodyRecoveryForTest(signedPSBT)
	}
	guest.custodyRecoveryExecute = func(profileName, signedPSBT string) (walletpkg.CustodyRecoveryResult, error) {
		return syntheticExecuteCustodyRecoveryForTest(signedPSBT)
	}

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)
	table := waitForLocalCanAct(t, []*meshRuntime{host, guest}, host, tableID)
	originalMenu := cloneJSON(*table.PendingTurnMenu)
	bridgeHostToGuestSync(host, guest)

	waitForTurnActionDeadlineToExpire(t, []*meshRuntime{host, guest}, host, tableID)
	if _, err := host.OpenTurnChallenge(tableID); err != nil {
		t.Fatalf("open turn challenge: %v", err)
	}

	waitForPendingTurnChallenge(t, []*meshRuntime{host, guest}, host, tableID)

	if err := host.store.withTableLock(tableID, func() error {
		locked, err := host.store.readTable(tableID)
		if err != nil || locked == nil {
			return err
		}
		if locked.PendingTurnChallenge == nil {
			t.Fatal("expected pending turn challenge before forcing timeout eligibility")
		}
		challenge := cloneJSON(*locked.PendingTurnChallenge)
		challenge.TimeoutEligibleAt = addMillis(nowISO(), -1)
		locked.PendingTurnChallenge = &challenge
		return host.persistLocalTable(locked, false)
	}); err != nil {
		t.Fatalf("force timeout eligibility: %v", err)
	}

	if _, err := host.ResolveTurnChallenge(tableID, "timeout"); err != nil {
		t.Fatalf("resolve turn challenge timeout: %v", err)
	}

	table = waitForLatestCustodyTransitionKind(t, []*meshRuntime{host, guest}, host, tableID, tablecustody.TransitionKindTimeout)
	last := table.CustodyTransitions[len(table.CustodyTransitions)-1]
	if last.Proof.ChallengeBundle == nil || last.Proof.ChallengeWitness == nil {
		t.Fatalf("expected timeout resolution to carry challenge proof material, got %+v", last.Proof)
	}
	if last.Proof.SettlementWitness != nil {
		t.Fatalf("expected timeout challenge resolution to avoid settlement witness, got %+v", last.Proof.SettlementWitness)
	}
	if last.Proof.RequestHash != originalMenu.TimeoutCandidate.Transition.Proof.RequestHash {
		t.Fatalf("expected challenge timeout request hash %q, got %q", originalMenu.TimeoutCandidate.Transition.Proof.RequestHash, last.Proof.RequestHash)
	}
	if !reflect.DeepEqual(last.Approvals, originalMenu.TimeoutCandidate.Transition.Approvals) {
		t.Fatalf("expected challenge timeout approvals to reuse the prebuilt candidate approvals")
	}
	if table.PendingTurnChallenge != nil {
		t.Fatalf("expected pending turn challenge to clear after timeout resolution, got %+v", table.PendingTurnChallenge)
	}
	if turnMenuMatchesTable(table, table.PendingTurnMenu) {
		t.Fatalf("expected pending turn menu to clear after timeout resolution, got %+v", table.PendingTurnMenu)
	}
	guestTable := waitForLatestCustodyTransitionKind(t, []*meshRuntime{host, guest}, guest, tableID, tablecustody.TransitionKindTimeout)
	guestLast := guestTable.CustodyTransitions[len(guestTable.CustodyTransitions)-1]
	if !reflect.DeepEqual(guestLast.Approvals, originalMenu.TimeoutCandidate.Transition.Approvals) {
		t.Fatalf("expected guest to accept the reused challenge timeout approvals")
	}
}

func TestTurnChallengeEscapeRejectsUnconfirmedOpenTx(t *testing.T) {
	oracle, host, _, prepared := prepareBlockBasedTurnChallengeEscape(t)

	oracle.setTxStatus(walletpkg.ChainTransactionStatus{
		Confirmed: false,
		TxID:      prepared.openTxID,
	})
	oracle.setTip(prepared.csvBlocks + 100)

	if _, err := host.ResolveTurnChallenge(prepared.tableID, "escape"); err == nil || !strings.Contains(err.Error(), "open tx is unconfirmed") {
		t.Fatalf("expected escape to reject an unconfirmed open tx, got %v", err)
	}
}

func TestTurnChallengeEscapeRejectsTipBelowEligibleHeight(t *testing.T) {
	oracle, host, _, prepared := prepareBlockBasedTurnChallengeEscape(t)

	openConfirmedHeight := int64(250)
	eligibleHeight := openConfirmedHeight + prepared.csvBlocks
	oracle.setTxStatus(walletpkg.ChainTransactionStatus{
		BlockHeight: openConfirmedHeight,
		Confirmed:   true,
		TxID:        prepared.openTxID,
	})
	oracle.setTip(eligibleHeight - 1)

	if _, err := host.ResolveTurnChallenge(prepared.tableID, "escape"); err == nil || !strings.Contains(err.Error(), "not yet eligible") {
		t.Fatalf("expected escape to reject a chain tip below the eligible height, got %v", err)
	}
}

func TestTurnChallengeEscapeRestoresSplitAndAbortsHand(t *testing.T) {
	oracle, host, guest, prepared := prepareBlockBasedTurnChallengeEscape(t)

	openConfirmedHeight := int64(100)
	eligibleHeight := openConfirmedHeight + prepared.csvBlocks
	oracle.setTxStatus(walletpkg.ChainTransactionStatus{
		BlockHeight: openConfirmedHeight,
		Confirmed:   true,
		TxID:        prepared.openTxID,
	})
	oracle.setTxStatus(walletpkg.ChainTransactionStatus{
		BlockHeight: eligibleHeight,
		Confirmed:   true,
		TxID:        prepared.escapeTxID,
	})
	oracle.setTip(eligibleHeight)

	view := host.localTableView(prepared.table)
	if view.Local.TurnChallengeChain == nil {
		t.Fatal("expected local view to expose turn challenge chain status")
	}
	if view.Local.TurnChallengeChain.OpenTxID != prepared.openTxID {
		t.Fatalf("expected open txid %q, got %+v", prepared.openTxID, view.Local.TurnChallengeChain)
	}
	if view.Local.TurnChallengeChain.OpenConfirmedHeight != openConfirmedHeight {
		t.Fatalf("expected open confirmed height %d, got %+v", openConfirmedHeight, view.Local.TurnChallengeChain)
	}
	if view.Local.TurnChallengeChain.EscapeEligibleHeight != eligibleHeight {
		t.Fatalf("expected escape eligible height %d, got %+v", eligibleHeight, view.Local.TurnChallengeChain)
	}
	if view.Local.TurnChallengeChain.ChainTipHeight != eligibleHeight {
		t.Fatalf("expected chain tip height %d, got %+v", eligibleHeight, view.Local.TurnChallengeChain)
	}
	if !view.Local.TurnChallengeChain.EscapeReady {
		t.Fatalf("expected local turn challenge chain status to report readiness, got %+v", view.Local.TurnChallengeChain)
	}

	if _, err := host.ResolveTurnChallenge(prepared.tableID, "escape"); err != nil {
		t.Fatalf("resolve turn challenge escape: %v", err)
	}
	table := mustReadNativeTable(t, host, prepared.tableID)
	if len(table.CustodyTransitions) == 0 {
		t.Fatal("expected challenge escape to append a custody transition")
	}
	if table.CustodyTransitions[len(table.CustodyTransitions)-1].Kind != tablecustody.TransitionKindTurnChallengeEscape {
		t.Fatalf("expected immediate local latest transition %q, got %+v", tablecustody.TransitionKindTurnChallengeEscape, table.CustodyTransitions[len(table.CustodyTransitions)-1])
	}
	table = waitForCustodyTransitionKindPresent(t, []*meshRuntime{host, guest}, host, prepared.tableID, tablecustody.TransitionKindTurnChallengeEscape)
	var last tablecustody.CustodyTransition
	foundEscape := false
	for _, transition := range table.CustodyTransitions {
		if transition.Kind != tablecustody.TransitionKindTurnChallengeEscape {
			continue
		}
		last = transition
		foundEscape = true
	}
	if !foundEscape {
		t.Fatal("expected accepted custody history to contain a turn-challenge-escape transition")
	}
	if last.Proof.ChallengeBundle == nil || last.Proof.ChallengeWitness == nil {
		t.Fatalf("expected challenge escape to carry challenge proof material, got %+v", last.Proof)
	}
	if last.Proof.ChallengeBundle.Kind != tablecustody.TransitionKindTurnChallengeEscape {
		t.Fatalf("expected stored challenge escape bundle kind %q, got %q", tablecustody.TransitionKindTurnChallengeEscape, last.Proof.ChallengeBundle.Kind)
	}
	if len(last.Approvals) != 0 {
		t.Fatalf("expected challenge escape to avoid custody approvals, got %+v", last.Approvals)
	}
	if !reflect.DeepEqual(semanticComparableCustodyStacks(&last.NextState), semanticComparableCustodyStacks(&prepared.sourceState)) {
		t.Fatalf("expected challenge escape to restore the pre-open stack split, got %+v want %+v", last.NextState.StackClaims, prepared.sourceState.StackClaims)
	}
	if !reflect.DeepEqual(semanticComparableCustodyPots(&last.NextState), semanticComparableCustodyPots(&prepared.sourceState)) {
		t.Fatalf("expected challenge escape to restore the pre-open pot split, got %+v want %+v", last.NextState.PotSlices, prepared.sourceState.PotSlices)
	}
	if !sameCanonicalVTXORefs(stackProofRefs(last.NextState), last.Proof.VTXORefs) {
		t.Fatalf("expected challenge escape proof refs to match restored stack refs")
	}
	if table.PendingTurnChallenge != nil {
		t.Fatalf("expected pending turn challenge to clear after escape, got %+v", table.PendingTurnChallenge)
	}
	if turnMenuMatchesTable(table, table.PendingTurnMenu) {
		t.Fatalf("expected pending turn menu to clear after escape, got %+v", table.PendingTurnMenu)
	}
	if table.ActiveHand != nil {
		t.Fatalf("expected challenge escape to abort the active hand, got %+v", table.ActiveHand)
	}
	if table.Config.Status != "ready" {
		t.Fatalf("expected challenge escape to return the table to ready status, got %q", table.Config.Status)
	}
	if got := stringValue(table.Events[len(table.Events)-1].Body["type"]); got != "HandAbort" && got != "HostRotated" {
		t.Fatalf("expected challenge escape to preserve a HandAbort tail or a later failover marker, got %q", got)
	}
	if err := host.validateAcceptedCustodyHistory(nil, table); err != nil {
		t.Fatalf("expected accepted replay to validate challenge escape offline, got %v", err)
	}
}

func TestAcceptedTurnChallengeEscapeReplayAllowsVisibleUnconfirmedEscape(t *testing.T) {
	oracle, host, _, prepared := prepareBlockBasedTurnChallengeEscape(t)

	openConfirmedHeight := int64(300)
	eligibleHeight := openConfirmedHeight + prepared.csvBlocks
	oracle.setTxStatus(walletpkg.ChainTransactionStatus{
		BlockHeight: openConfirmedHeight,
		Confirmed:   true,
		TxID:        prepared.openTxID,
	})
	oracle.setTxStatus(walletpkg.ChainTransactionStatus{
		Confirmed: false,
		TxID:      prepared.escapeTxID,
	})
	oracle.setTip(eligibleHeight)

	if _, err := host.ResolveTurnChallenge(prepared.tableID, "escape"); err != nil {
		t.Fatalf("resolve turn challenge escape: %v", err)
	}
	table := waitForCustodyTransitionKindPresent(t, []*meshRuntime{host}, host, prepared.tableID, tablecustody.TransitionKindTurnChallengeEscape)

	if err := host.validateAcceptedCustodyHistory(nil, table); err != nil {
		t.Fatalf("expected accepted replay to allow a visible unconfirmed escape after maturity, got %v", err)
	}
}

func TestAcceptedTurnChallengeEscapeReplayRejectsEscapeConfirmedBelowEligibleHeight(t *testing.T) {
	oracle, host, _, prepared := prepareBlockBasedTurnChallengeEscape(t)

	openConfirmedHeight := int64(400)
	eligibleHeight := openConfirmedHeight + prepared.csvBlocks
	oracle.setTxStatus(walletpkg.ChainTransactionStatus{
		BlockHeight: openConfirmedHeight,
		Confirmed:   true,
		TxID:        prepared.openTxID,
	})
	oracle.setTxStatus(walletpkg.ChainTransactionStatus{
		BlockHeight: eligibleHeight,
		Confirmed:   true,
		TxID:        prepared.escapeTxID,
	})
	oracle.setTip(eligibleHeight)

	if _, err := host.ResolveTurnChallenge(prepared.tableID, "escape"); err != nil {
		t.Fatalf("resolve turn challenge escape: %v", err)
	}
	table := waitForCustodyTransitionKindPresent(t, []*meshRuntime{host}, host, prepared.tableID, tablecustody.TransitionKindTurnChallengeEscape)

	oracle.setTxStatus(walletpkg.ChainTransactionStatus{
		BlockHeight: eligibleHeight - 1,
		Confirmed:   true,
		TxID:        prepared.escapeTxID,
	})
	if err := host.validateAcceptedCustodyHistory(nil, table); err == nil || !strings.Contains(err.Error(), "confirmed before the CSV block delay matured") {
		t.Fatalf("expected accepted replay to reject an early escape confirmation, got %v", err)
	}
}

func TestAcceptedTurnChallengeEscapeReplayRejectsMissingChainStatus(t *testing.T) {
	oracle, host, _, prepared := prepareBlockBasedTurnChallengeEscape(t)

	openConfirmedHeight := int64(500)
	eligibleHeight := openConfirmedHeight + prepared.csvBlocks
	oracle.setTxStatus(walletpkg.ChainTransactionStatus{
		BlockHeight: openConfirmedHeight,
		Confirmed:   true,
		TxID:        prepared.openTxID,
	})
	oracle.setTxStatus(walletpkg.ChainTransactionStatus{
		BlockHeight: eligibleHeight,
		Confirmed:   true,
		TxID:        prepared.escapeTxID,
	})
	oracle.setTip(eligibleHeight)

	if _, err := host.ResolveTurnChallenge(prepared.tableID, "escape"); err != nil {
		t.Fatalf("resolve turn challenge escape: %v", err)
	}
	table := waitForCustodyTransitionKindPresent(t, []*meshRuntime{host}, host, prepared.tableID, tablecustody.TransitionKindTurnChallengeEscape)

	oracle.setTxError(prepared.escapeTxID, fmt.Errorf("missing chain status"))
	if err := host.validateAcceptedCustodyHistory(nil, table); err == nil || !strings.Contains(err.Error(), "unable to verify turn challenge escape tx status") {
		t.Fatalf("expected accepted replay to fail closed when chain status is unavailable, got %v", err)
	}
}

func TestAcceptedCustodyHistoryAllowsChallengeWitnessTimingDrift(t *testing.T) {
	_, host, _, prepared := prepareBlockBasedTurnChallengeEscape(t)

	existing := cloneJSON(prepared.table)
	incoming := cloneJSON(prepared.table)
	last := len(incoming.CustodyTransitions) - 1
	witness := incoming.CustodyTransitions[last].Proof.ChallengeWitness
	if witness == nil {
		t.Fatal("expected turn challenge open witness")
	}
	witness.ExecutedAt = addMillis(witness.ExecutedAt, 1500)
	witness.BroadcastTxIDs = []string{witness.TransactionID, witness.TransactionID}
	incoming.CustodyTransitions[last].Proof.ChallengeWitness = witness
	incoming.CustodyTransitions[last].Proof.FinalizedAt = witness.ExecutedAt
	incoming.CustodyTransitions[last].Proof.TransitionHash = tablecustody.HashCustodyTransition(incoming.CustodyTransitions[last])
	if incoming.PendingTurnChallenge == nil {
		t.Fatal("expected pending turn challenge")
	}
	incoming.PendingTurnChallenge.OpenedAt = witness.ExecutedAt

	if err := host.validateAcceptedCustodyHistory(&existing, incoming); err != nil {
		t.Fatalf("expected accepted custody history to tolerate challenge witness timing drift, got %v", err)
	}
}

func TestAcceptedHistoricalLedgerAllowsTurnChallengeOpenAuthorDrift(t *testing.T) {
	_, host, guest, prepared := prepareBlockBasedTurnChallengeEscape(t)

	existing := cloneJSON(prepared.table)
	incoming := cloneJSON(prepared.table)
	last := len(existing.Events) - 1
	if got := stringValue(existing.Events[last].Body["type"]); got != "TurnChallengeOpened" {
		t.Fatalf("expected last event type TurnChallengeOpened, got %q", got)
	}

	existing.Events[last].SenderPeerID = guest.selfPeerID()
	existing.Events[last].SenderProtocolPubkeyHex = guest.protocolIdentity.PublicKeyHex
	existing.Events[last].SenderRole = "player"
	existing.Events[last].Timestamp = addMillis(existing.Events[last].Timestamp, -14_000)
	existing.Events[last].Body["openedAt"] = addMillis(stringValue(existing.Events[last].Body["openedAt"]), -14_000)
	if existing.PendingTurnChallenge == nil {
		t.Fatal("expected pending turn challenge")
	}
	existing.PendingTurnChallenge.OpenedAt = stringValue(existing.Events[last].Body["openedAt"])
	if escapeEligibleAt := strings.TrimSpace(stringValue(existing.Events[last].Body["escapeEligibleAt"])); escapeEligibleAt != "" {
		existing.Events[last].Body["escapeEligibleAt"] = addMillis(escapeEligibleAt, -14_000)
		existing.PendingTurnChallenge.EscapeEligibleAt = stringValue(existing.Events[last].Body["escapeEligibleAt"])
	}
	existing.Events[last].Body["transitionHash"] = strings.Repeat("0", 64)

	var lastEventHash string
	existing.Events, lastEventHash = resignHistoricalEventsForTest(t, existing.Events, host, guest)
	existing.LastEventHash = lastEventHash
	if existing.PublicState != nil {
		existing.PublicState.LatestEventHash = lastEventHash
	}

	if err := host.validateAcceptedHistoricalLedger(&existing, incoming); err != nil {
		t.Fatalf("expected accepted event history to tolerate turn challenge open author drift, got %v", err)
	}
}

func TestTurnChallengeOpenBundleBecomesStaleAfterOrdinaryCandidateFinalizesFirst(t *testing.T) {
	host := newMeshTestRuntime(t, "host")
	guest := newMeshTestRuntime(t, "guest")
	bridgeHostToGuestSync(host, guest)

	tableID, _ := createStartedTwoPlayerTable(t, host, guest)
	table := waitForLocalCanAct(t, []*meshRuntime{host, guest}, host, tableID)
	originalMenu := cloneJSON(*table.PendingTurnMenu)

	if _, err := host.SendAction(tableID, originalMenu.Options[0].Action); err != nil {
		t.Fatalf("send ordinary action: %v", err)
	}

	table = waitForLatestCustodyTransitionKind(t, []*meshRuntime{host, guest}, host, tableID, tablecustody.TransitionKindAction)
	if _, err := host.validateTurnChallengeOpenBundle(table, &originalMenu, originalMenu.ChallengeEnvelope.OpenBundle); err == nil {
		t.Fatal("expected original challenge-open bundle to go stale once the ordinary candidate settled first")
	}
}
