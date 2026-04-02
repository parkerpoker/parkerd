//go:build integration

package meshruntime

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/parkerpoker/parkerd/internal/tablecustody"
)

const regtestRoundLockPath = "/tmp/parker-regtest-round.lock"

func lockRegtestRound(t *testing.T) func() {
	t.Helper()

	lockFile, err := os.OpenFile(regtestRoundLockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open regtest lock file: %v", err)
	}
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		_ = lockFile.Close()
		t.Fatalf("lock regtest harness: %v", err)
	}

	return func() {
		_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
		_ = lockFile.Close()
	}
}

func TestRegtestRoundUsesRealArkCustodyStandardScenario(t *testing.T) {
	result := runRegtestRoundScenario(t, "standard-4d")
	assertRegtestRoundCustodyArtifacts(t, result)
}

func TestRegtestRoundUsesRealArkCustodyHostPlayerScenario(t *testing.T) {
	result := runRegtestRoundScenario(t, "host-player-2d")
	assertRegtestRoundCustodyArtifacts(t, result)
}

func TestRegtestRoundUsesRealArkCustodyRecoveryTimeoutScenario(t *testing.T) {
	result := runRegtestRoundScenario(t, "recovery-timeout-2d")
	assertRegtestRoundRecoveryArtifacts(t, result)
}

type cliDataEnvelope[T any] struct {
	Data T `json:"data"`
}

type regtestRoundResult struct {
	Scenario           string
	Output             string
	RunRoot            string
	TableActivePath    string
	TableAfterHandPath string
	TableAfterCashout  string
	SkipCashOut        bool
}

func runRegtestRoundScenario(t *testing.T, scenario string) regtestRoundResult {
	if testing.Short() {
		t.Skip("skipping regtest Ark integration round in short mode")
	}
	defer lockRegtestRound(t)()

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	repoRoot := filepath.Clean(filepath.Join(cwd, "..", ".."))
	scriptPath := filepath.Join(repoRoot, "scripts", "run-regtest-round.sh")
	if _, err := os.Stat(scriptPath); err != nil {
		t.Fatalf("stat regtest round script: %v", err)
	}

	sharedTmpRoot := filepath.Join(repoRoot, ".tmp")
	if err := os.MkdirAll(sharedTmpRoot, 0o755); err != nil {
		t.Fatalf("create shared temp root: %v", err)
	}

	runRoot, err := os.MkdirTemp(sharedTmpRoot, "parker-regtest-round-")
	if err != nil {
		t.Fatalf("create regtest temp root: %v", err)
	}
	t.Cleanup(func() {
		if !t.Failed() {
			_ = os.RemoveAll(runRoot)
		}
	})

	baseDir := filepath.Join(runRoot, "round")
	nigiriDatadir := integrationNigiriDatadir(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, scriptPath)
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(),
		"BASE="+baseDir,
		"NIGIRI_DATADIR="+nigiriDatadir,
		"PCLI_TIMEOUT_SECONDS=90",
		"ROUND_SCENARIO="+scenario,
	)
	output, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		t.Fatalf("regtest round %s timed out after %s (artifacts at %s)\n%s", scenario, 15*time.Minute, runRoot, string(output))
	}
	if err != nil {
		t.Fatalf("regtest round %s failed: %v (artifacts at %s)\n%s", scenario, err, runRoot, string(output))
	}

	text := string(output)
	markers := []string{
		"TABLE_ID=",
		"Done. Logs are under",
	}
	if scenario == "recovery-timeout-2d" {
		markers = append(markers,
			"Forcing deterministic timeout recovery scenario...",
			"Stopping defaulting player daemon and Ark/indexer services before timeout finalization completes...",
			"Recovery transition confirmed.",
			"Skipping cash out because the recovery scenario intentionally leaves the Ark server offline.",
		)
	} else {
		markers = append(markers,
			"Playing automatic hands until funds move...",
			"Cashing out...",
			"Final wallet summaries:",
		)
	}
	for _, marker := range markers {
		if !strings.Contains(text, marker) {
			t.Fatalf("regtest round %s output missing %q (artifacts at %s)\n%s", scenario, marker, runRoot, text)
		}
	}
	return regtestRoundResult{
		Scenario:           scenario,
		Output:             text,
		RunRoot:            runRoot,
		TableActivePath:    filepath.Join(baseDir, "artifacts", "table-active.json"),
		TableAfterHandPath: filepath.Join(baseDir, "artifacts", "table-after-hand.json"),
		TableAfterCashout:  filepath.Join(baseDir, "artifacts", "table-after-cashout.json"),
		SkipCashOut:        scenario == "recovery-timeout-2d",
	}
}

func integrationNigiriDatadir(t *testing.T) string {
	t.Helper()

	if root := strings.TrimSpace(os.Getenv("PARKER_INTEGRATION_NIGIRI_ROOT")); root != "" {
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatalf("prepare PARKER_INTEGRATION_NIGIRI_ROOT %s: %v", root, err)
		}
		path, err := os.MkdirTemp(root, "parker-regtest-round-nigiri-")
		if err != nil {
			t.Fatalf("create Nigiri datadir under %s: %v", root, err)
		}
		return path
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("locate home directory: %v", err)
	}
	for _, root := range []string{
		filepath.Join(homeDir, "Library", "Application Support", "Nigiri", "parker-integration"),
		filepath.Join(homeDir, ".nigiri", "parker-integration"),
	} {
		if err := os.MkdirAll(root, 0o755); err != nil {
			continue
		}
		path, err := os.MkdirTemp(root, "parker-regtest-round-nigiri-")
		if err != nil {
			continue
		}
		return path
	}

	t.Skip("skipping regtest Ark integration round: no writable Docker-shared Nigiri datadir root; set PARKER_INTEGRATION_NIGIRI_ROOT to a shared writable path")
	return ""
}

func loadIntegrationTableView(t *testing.T, path string) NativeMeshTableView {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read integration artifact %s: %v", path, err)
	}
	var envelope cliDataEnvelope[NativeMeshTableView]
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("decode integration artifact %s: %v", path, err)
	}
	return envelope.Data
}

func linkedTransitionBySeqHash(transitions []tablecustody.CustodyTransition, transitionHash string, custodySeq int) (tablecustody.CustodyTransition, *tablecustody.CustodyState, bool) {
	for index, transition := range transitions {
		if transition.CustodySeq != custodySeq {
			continue
		}
		if transition.Proof.TransitionHash != transitionHash {
			continue
		}
		if index == 0 {
			return transition, nil, true
		}
		previous := transitions[index-1].NextState
		return transition, &previous, true
	}
	return tablecustody.CustodyTransition{}, nil, false
}

func assertRegtestRoundCustodyArtifacts(t *testing.T, result regtestRoundResult) {
	t.Helper()

	for _, path := range []string{result.TableActivePath, result.TableAfterHandPath, result.TableAfterCashout} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("missing integration artifact %s: %v", path, err)
		}
	}

	afterHand := loadIntegrationTableView(t, result.TableAfterHandPath)
	afterCashout := loadIntegrationTableView(t, result.TableAfterCashout)

	assertRegtestRoundMovedFunds(t, afterHand)

	handResultCount := 0
	for index, event := range afterHand.Events {
		if stringValue(event.Body["type"]) != "HandResult" {
			continue
		}
		handResultCount++
		if strings.TrimSpace(stringValue(event.Body["latestCustodyStateHash"])) == "" {
			t.Fatalf("hand result event %d is missing latest custody state hash", index)
		}
		custodySeq, err := eventCustodySeq(event)
		if err != nil {
			t.Fatalf("decode hand result custody seq for event %d: %v", index, err)
		}
		transitionHash := eventTransitionHash(event)
		transition, _, ok := linkedTransitionBySeqHash(afterHand.CustodyTransitions, transitionHash, custodySeq)
		if !ok {
			t.Fatalf("hand result event %d does not link to custody history", index)
		}
		if transition.NextStateHash != strings.TrimSpace(stringValue(event.Body["latestCustodyStateHash"])) {
			t.Fatalf("hand result event %d latest custody hash %q does not match linked transition next hash %q", index, stringValue(event.Body["latestCustodyStateHash"]), transition.NextStateHash)
		}
	}
	if handResultCount == 0 {
		t.Fatal("expected at least one HandResult event in regtest round")
	}

	playerActionCount := 0
	signedActionCount := 0
	for index, event := range afterHand.Events {
		if stringValue(event.Body["type"]) != "PlayerAction" {
			continue
		}
		playerActionCount++
		request, hasRequest, err := actionRequestFromEvent(event)
		if err != nil {
			t.Fatalf("decode action request for event %d: %v", index, err)
		}
		if !hasRequest || request == nil {
			t.Fatalf("player action event %d is missing canonical actionRequest payload", index)
		}
		signedActionCount++
		if strings.TrimSpace(request.SignatureHex) == "" || strings.TrimSpace(request.SignedAt) == "" {
			t.Fatalf("player action event %d request is missing its signature material", index)
		}
		if strings.TrimSpace(request.PrevCustodyStateHash) == "" {
			t.Fatalf("player action event %d request is missing prev custody hash", index)
		}
		if strings.TrimSpace(request.ChallengeAnchor) == "" {
			t.Fatalf("player action event %d request is missing challenge anchor", index)
		}
		if strings.TrimSpace(request.TranscriptRoot) == "" {
			t.Fatalf("player action event %d request is missing transcript root", index)
		}
		custodySeq, err := eventCustodySeq(event)
		if err != nil {
			t.Fatalf("decode action custody seq for event %d: %v", index, err)
		}
		transitionHash := eventTransitionHash(event)
		transition, previous, ok := linkedTransitionBySeqHash(afterHand.CustodyTransitions, transitionHash, custodySeq)
		if !ok {
			t.Fatalf("player action event %d does not link to custody history", index)
		}
		if transition.Kind != tablecustody.TransitionKindAction {
			t.Fatalf("player action event %d linked transition kind %s, want %s", index, transition.Kind, tablecustody.TransitionKindAction)
		}
		if request.PrevCustodyStateHash != transition.PrevStateHash {
			t.Fatalf("player action event %d request prev hash %q does not match transition prev hash %q", index, request.PrevCustodyStateHash, transition.PrevStateHash)
		}
			expectedActor := ""
			if previous != nil {
				expectedActor = previous.ActingPlayerID
			}
			if strings.TrimSpace(expectedActor) != "" && request.PlayerID != expectedActor {
				t.Fatalf("player action event %d request player %q does not match transition actor %q", index, request.PlayerID, expectedActor)
			}
		if transition.NextState.ChallengeAnchor != request.ChallengeAnchor {
			t.Fatalf("player action event %d request challenge anchor %q does not match transition binding %q", index, request.ChallengeAnchor, transition.NextState.ChallengeAnchor)
		}
		if transition.NextState.TranscriptRoot != request.TranscriptRoot {
			t.Fatalf("player action event %d request transcript root %q does not match transition binding %q", index, request.TranscriptRoot, transition.NextState.TranscriptRoot)
		}
		if previous != nil && request.DecisionIndex != previous.DecisionIndex {
			t.Fatalf("player action event %d request decision index %d does not match previous custody decision %d", index, request.DecisionIndex, previous.DecisionIndex)
		}
	}
	if playerActionCount == 0 {
		t.Fatal("expected at least one PlayerAction event in regtest round")
	}
	if signedActionCount == 0 {
		t.Fatal("expected signed action requests in regtest round")
	}

	cashOutCount := 0
	for index, event := range afterCashout.Events {
		if stringValue(event.Body["type"]) != "CashOut" {
			continue
		}
		cashOutCount++
		request, hasRequest, err := fundsRequestFromEvent(event)
		if err != nil {
			t.Fatalf("decode funds request for event %d: %v", index, err)
		}
		if !hasRequest || request == nil {
			t.Fatalf("cash-out event %d is missing canonical fundsRequest payload", index)
		}
		if strings.TrimSpace(request.SignatureHex) == "" || strings.TrimSpace(request.SignedAt) == "" {
			t.Fatalf("cash-out event %d request is missing its signature material", index)
		}
		if strings.TrimSpace(request.PrevCustodyStateHash) == "" {
			t.Fatalf("cash-out event %d request is missing prev custody hash", index)
		}
		custodySeq, err := eventCustodySeq(event)
		if err != nil {
			t.Fatalf("decode funds custody seq for event %d: %v", index, err)
		}
		transitionHash := eventTransitionHash(event)
		transition, _, ok := linkedTransitionBySeqHash(afterCashout.CustodyTransitions, transitionHash, custodySeq)
		if !ok {
			t.Fatalf("cash-out event %d does not link to custody history", index)
		}
		if transition.Kind != tablecustody.TransitionKindCashOut {
			t.Fatalf("cash-out event %d linked transition kind %s, want %s", index, transition.Kind, tablecustody.TransitionKindCashOut)
		}
		if request.PrevCustodyStateHash != transition.PrevStateHash {
			t.Fatalf("cash-out event %d request prev hash %q does not match transition prev hash %q", index, request.PrevCustodyStateHash, transition.PrevStateHash)
		}
		if request.PlayerID != transition.ActingPlayerID {
			t.Fatalf("cash-out event %d request player %q does not match transition actor %q", index, request.PlayerID, transition.ActingPlayerID)
		}
	}
	if cashOutCount < 2 {
		t.Fatalf("expected at least two CashOut events in regtest round, got %d", cashOutCount)
	}
}

func assertRegtestRoundRecoveryArtifacts(t *testing.T, result regtestRoundResult) {
	t.Helper()

	for _, path := range []string{result.TableActivePath, result.TableAfterHandPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("missing integration artifact %s: %v", path, err)
		}
	}
	if _, err := os.Stat(result.TableAfterCashout); err == nil {
		t.Fatalf("recovery scenario unexpectedly produced a cash-out artifact at %s", result.TableAfterCashout)
	}

	afterHand := loadIntegrationTableView(t, result.TableAfterHandPath)
	assertRegtestRoundMovedFunds(t, afterHand)

	var recovered tablecustody.CustodyTransition
	var recoveredPrev *tablecustody.CustodyState
	foundRecovered := false
	for index, transition := range afterHand.CustodyTransitions {
		if transition.Kind != tablecustody.TransitionKindTimeout {
			continue
		}
		if transition.Proof.RecoveryWitness == nil {
			continue
		}
		if transition.Proof.SettlementWitness != nil {
			t.Fatalf("recovery timeout transition %d unexpectedly includes a settlement witness", index)
		}
		if strings.TrimSpace(transition.Proof.ArkIntentID) != "" || strings.TrimSpace(transition.Proof.ArkTxID) != "" {
			t.Fatalf("recovery timeout transition %d unexpectedly carries live Ark settlement ids", index)
		}
		if strings.TrimSpace(transition.Proof.RecoveryWitness.SourceTransitionHash) == "" {
			t.Fatalf("recovery timeout transition %d is missing its source transition hash", index)
		}
		recovered = transition
		if index > 0 {
			previous := afterHand.CustodyTransitions[index-1].NextState
			recoveredPrev = &previous
		}
		foundRecovered = true
	}
	if !foundRecovered {
		t.Fatal("expected a timeout transition finalized from a recovery witness")
	}
	if recoveredPrev == nil {
		t.Fatal("expected the recovered timeout transition to have a prior source state")
	}

	sourceFound := false
	sourceBundleFound := false
	for _, transition := range afterHand.CustodyTransitions {
		if transition.Proof.TransitionHash != recovered.Proof.RecoveryWitness.SourceTransitionHash {
			continue
		}
		sourceFound = true
		for _, bundle := range transition.Proof.RecoveryBundles {
			if bundle.BundleHash == recovered.Proof.RecoveryWitness.BundleHash {
				sourceBundleFound = true
				break
			}
		}
		break
	}
	if !sourceFound {
		t.Fatalf("recovery source transition %s not found in custody history", recovered.Proof.RecoveryWitness.SourceTransitionHash)
	}
	if !sourceBundleFound {
		t.Fatalf("recovery bundle %s not found on its source transition", recovered.Proof.RecoveryWitness.BundleHash)
	}
	if strings.TrimSpace(recovered.Proof.RecoveryWitness.RecoveryTxID) == "" {
		t.Fatal("expected the recovery witness to include a recovery transaction id")
	}
	foundRecoveryTxID := false
	for _, txid := range recovered.Proof.RecoveryWitness.BroadcastTxIDs {
		if txid == recovered.Proof.RecoveryWitness.RecoveryTxID {
			foundRecoveryTxID = true
			break
		}
	}
	if !foundRecoveryTxID {
		t.Fatal("expected the recovery witness broadcast metadata to include the recovery transaction id")
	}

	latestTransition := afterHand.CustodyTransitions[len(afterHand.CustodyTransitions)-1]
	if latestTransition.Proof.TransitionHash != recovered.Proof.TransitionHash {
		t.Fatalf("expected the recovery timeout transition to be the latest accepted transition, got %s want %s", latestTransition.Proof.TransitionHash, recovered.Proof.TransitionHash)
	}
	if afterHand.LatestCustodyState == nil {
		t.Fatal("expected latest custody state after recovery")
	}
	if afterHand.LatestCustodyState.StateHash != recovered.NextStateHash {
		t.Fatalf("latest custody state hash %q does not match recovered timeout next state hash %q", afterHand.LatestCustodyState.StateHash, recovered.NextStateHash)
	}

	handResultCount := 0
	for index, event := range afterHand.Events {
		if stringValue(event.Body["type"]) != "HandResult" {
			continue
		}
		handResultCount++
		if strings.TrimSpace(stringValue(event.Body["latestCustodyStateHash"])) != recovered.NextStateHash {
			t.Fatalf("hand result event %d latest custody hash %q does not match recovered timeout next hash %q", index, stringValue(event.Body["latestCustodyStateHash"]), recovered.NextStateHash)
		}
		if eventTransitionHash(event) != recovered.Proof.TransitionHash {
			t.Fatalf("hand result event %d transition hash %q does not match recovered timeout transition %q", index, eventTransitionHash(event), recovered.Proof.TransitionHash)
		}
	}
	if handResultCount == 0 {
		t.Fatal("expected at least one HandResult event in recovery scenario")
	}
}

func assertRegtestRoundMovedFunds(t *testing.T, table NativeMeshTableView) {
	t.Helper()

	if table.PublicState == nil {
		t.Fatal("regtest round is missing public state")
	}
	if len(table.PublicState.SeatedPlayers) != 2 {
		t.Fatalf("expected exactly two seated players in regtest round, got %d", len(table.PublicState.SeatedPlayers))
	}

	unchangedBalances := 0
	for _, seated := range table.PublicState.SeatedPlayers {
		balance, ok := table.PublicState.ChipBalances[seated.PlayerID]
		if !ok {
			t.Fatalf("missing chip balance for seated player %s", seated.PlayerID)
		}
		if balance == seated.BuyInSats {
			unchangedBalances++
		}
	}
	if unchangedBalances == len(table.PublicState.SeatedPlayers) {
		t.Fatalf("regtest round ended without net chip transfer: balances=%v", table.PublicState.ChipBalances)
	}
}
