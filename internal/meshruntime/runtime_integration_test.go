//go:build integration

package meshruntime

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/parkerpoker/parkerd/internal/game"
	"github.com/parkerpoker/parkerd/internal/tablecustody"
)

const regtestRoundLockPath = "/tmp/parker-regtest-round.lock"

type regtestRoundScenarioSpec struct {
	Artifacts   map[string]string
	Markers     []string
	SkipCashOut bool
}

var regtestRoundScenarioSpecs = map[string]regtestRoundScenarioSpec{
	"standard-4d": {
		Artifacts: map[string]string{
			"table-active":        "table-active.json",
			"table-after-hand":    "table-after-hand.json",
			"table-after-cashout": "table-after-cashout.json",
		},
		Markers: []string{
			"Playing automatic hands until funds move...",
			"Cashing out...",
			"Final wallet summaries:",
		},
	},
	"host-player-2d": {
		Artifacts: map[string]string{
			"table-active":        "table-active.json",
			"table-after-hand":    "table-after-hand.json",
			"table-after-cashout": "table-after-cashout.json",
		},
		Markers: []string{
			"Playing automatic hands until funds move...",
			"Cashing out...",
			"Final wallet summaries:",
		},
	},
	"recovery-timeout-2d": {
		Artifacts: map[string]string{
			"table-active":     "table-active.json",
			"table-after-hand": "table-after-hand.json",
		},
		Markers: []string{
			"Forcing deterministic timeout recovery scenario...",
			"Stopping defaulting player daemon and Ark/indexer services before timeout finalization completes...",
			"Timeout completion confirmed.",
			"Skipping cash out because the recovery scenario intentionally leaves the Ark server offline.",
		},
		SkipCashOut: true,
	},
	"aborted-hand-2d": {
		Artifacts: map[string]string{
			"table-active":        "table-active.json",
			"table-after-abort":   "table-after-abort.json",
			"table-after-hand":    "table-after-hand.json",
			"table-after-cashout": "table-after-cashout.json",
		},
		Markers: []string{
			"Forcing aborted-hand scenario...",
			"Forcing a no-blame hand abort",
			"Playing a fresh post-abort hand to verify the table continues...",
			"Cashing out...",
			"Final wallet summaries:",
		},
	},
	"all-in-side-pot-2d": {
		Artifacts: map[string]string{
			"table-active":        "table-active.json",
			"table-after-all-in":  "table-after-all-in.json",
			"table-after-hand":    "table-after-hand.json",
			"table-after-cashout": "table-after-cashout.json",
		},
		Markers: []string{
			"Forcing explicit all-in coverage...",
			"Sending all-in line via",
			"Completing all-in with",
			"Cashing out...",
			"Final wallet summaries:",
		},
	},
	"turn-challenge-2d": {
		Artifacts: map[string]string{
			"table-active":                     "table-active.json",
			"table-after-challenge-open":       "table-after-challenge-open.json",
			"table-after-challenge-resolution": "table-after-challenge-resolution.json",
			"table-after-hand":                 "table-after-hand.json",
		},
		Markers: []string{
			"Forcing on-chain turn challenge option resolution...",
			"Resolving turn challenge with option",
			"Skipping cash out because this scenario verifies challenge resolution without requiring a post-resolution cash-out.",
		},
		SkipCashOut: true,
	},
	"emergency-exit-2d": {
		Artifacts: map[string]string{
			"table-active":                "table-active.json",
			"table-before-emergency-exit": "table-before-emergency-exit.json",
			"emergency-exit-result":       "emergency-exit-result.json",
			"table-after-emergency-exit":  "table-after-emergency-exit.json",
			"table-after-hand":            "table-after-hand.json",
			"table-after-cashout":         "table-after-cashout.json",
		},
		Markers: []string{
			"Playing a live hand before emergency exit...",
			"Executing emergency exit...",
			"Cashing out the non-exiting player after emergency exit...",
			"Final wallet summaries:",
		},
	},
	"multi-hand-2d": {
		Artifacts: map[string]string{
			"table-active":        "table-active.json",
			"table-after-hand-1":  "table-after-hand-1.json",
			"table-after-hand":    "table-after-hand.json",
			"table-after-cashout": "table-after-cashout.json",
		},
		Markers: []string{
			"Playing multiple hands without cashing out...",
			"Cashing out...",
			"Final wallet summaries:",
		},
	},
	"challenge-escape-2d": {
		Artifacts: map[string]string{
			"table-active":                     "table-active.json",
			"table-after-challenge-open":       "table-after-challenge-open.json",
			"table-after-challenge-resolution": "table-after-challenge-resolution.json",
			"table-after-hand":                 "table-after-hand.json",
		},
		Markers: []string{
			"Forcing turn challenge escape after CSV maturity...",
			"Mining regtest blocks until challenge escape is eligible...",
			"Skipping cash out because this scenario verifies the CSV escape path",
		},
		SkipCashOut: true,
	},
	"recovery-showdown-2d": {
		Artifacts: map[string]string{
			"table-active":                   "table-active.json",
			"table-before-recovery-showdown": "table-before-recovery-showdown.json",
			"table-after-hand":               "table-after-hand.json",
		},
		Markers: []string{
			"Driving the hand to showdown before forcing payout recovery...",
			"taking Ark offline before payout settlement",
			"Skipping cash out because the recovery scenario intentionally leaves the Ark server offline.",
		},
		SkipCashOut: true,
	},
	"cashout-after-challenge-2d": {
		Artifacts: map[string]string{
			"table-active":                     "table-active.json",
			"table-after-challenge-open":       "table-after-challenge-open.json",
			"table-after-challenge-resolution": "table-after-challenge-resolution.json",
			"table-after-hand":                 "table-after-hand.json",
			"table-after-cashout":              "table-after-cashout.json",
		},
		Markers: []string{
			"Forcing challenged timeout resolution before cash-out...",
			"Waiting for turn challenge timeout eligibility...",
			"Cashing out...",
			"Final wallet summaries:",
		},
	},
}

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

func TestRegtestRoundUsesRealArkCustodyAbortedHandScenario(t *testing.T) {
	result := runRegtestRoundScenario(t, "aborted-hand-2d")
	assertRegtestRoundAbortedHandArtifacts(t, result)
}

func TestRegtestRoundUsesRealArkCustodyAllInScenario(t *testing.T) {
	result := runRegtestRoundScenario(t, "all-in-side-pot-2d")
	assertRegtestRoundAllInArtifacts(t, result)
}

func TestRegtestRoundUsesRealArkCustodyTurnChallengeScenario(t *testing.T) {
	result := runRegtestRoundScenario(t, "turn-challenge-2d")
	assertRegtestRoundTurnChallengeOptionArtifacts(t, result)
}

func TestRegtestRoundUsesRealArkCustodyEmergencyExitScenario(t *testing.T) {
	result := runRegtestRoundScenario(t, "emergency-exit-2d")
	assertRegtestRoundEmergencyExitArtifacts(t, result)
}

func TestRegtestRoundUsesRealArkCustodyMultiHandScenario(t *testing.T) {
	result := runRegtestRoundScenario(t, "multi-hand-2d")
	assertRegtestRoundMultiHandArtifacts(t, result)
}

func TestRegtestRoundUsesRealArkCustodyChallengeEscapeScenario(t *testing.T) {
	result := runRegtestRoundScenario(t, "challenge-escape-2d")
	assertRegtestRoundChallengeEscapeArtifacts(t, result)
}

func TestRegtestRoundUsesRealArkCustodyRecoveryShowdownScenario(t *testing.T) {
	result := runRegtestRoundScenario(t, "recovery-showdown-2d")
	assertRegtestRoundRecoveryShowdownArtifacts(t, result)
}

func TestRegtestRoundUsesRealArkCustodyCashoutAfterChallengeScenario(t *testing.T) {
	result := runRegtestRoundScenario(t, "cashout-after-challenge-2d")
	assertRegtestRoundCashoutAfterChallengeArtifacts(t, result)
}

type cliDataEnvelope[T any] struct {
	Data T `json:"data"`
}

type regtestRoundResult struct {
	Artifacts   map[string]string
	Output      string
	RunRoot     string
	Scenario    string
	SkipCashOut bool
}

func (result regtestRoundResult) Artifact(name string) string {
	return result.Artifacts[name]
}

func runRegtestRoundScenario(t *testing.T, scenario string) regtestRoundResult {
	if testing.Short() {
		t.Skip("skipping regtest Ark integration round in short mode")
	}
	defer lockRegtestRound(t)()

	spec, ok := regtestRoundScenarioSpecs[scenario]
	if !ok {
		t.Fatalf("unknown regtest round scenario %q", scenario)
	}

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
	markers = append(markers, spec.Markers...)
	for _, marker := range markers {
		if !strings.Contains(text, marker) {
			t.Fatalf("regtest round %s output missing %q (artifacts at %s)\n%s", scenario, marker, runRoot, text)
		}
	}
	artifacts := map[string]string{}
	for name, filename := range spec.Artifacts {
		artifacts[name] = filepath.Join(baseDir, "artifacts", filename)
	}
	return regtestRoundResult{
		Artifacts:   artifacts,
		Output:      text,
		RunRoot:     runRoot,
		Scenario:    scenario,
		SkipCashOut: spec.SkipCashOut,
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

func loadIntegrationArtifactTableView(t *testing.T, result regtestRoundResult, name string) NativeMeshTableView {
	t.Helper()

	path := result.Artifact(name)
	if strings.TrimSpace(path) == "" {
		t.Fatalf("scenario %s is missing artifact mapping for %s", result.Scenario, name)
	}
	return loadIntegrationTableView(t, path)
}

func loadIntegrationArtifactJSONMap(t *testing.T, result regtestRoundResult, name string) map[string]any {
	t.Helper()

	path := result.Artifact(name)
	if strings.TrimSpace(path) == "" {
		t.Fatalf("scenario %s is missing artifact mapping for %s", result.Scenario, name)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read integration artifact %s: %v", path, err)
	}
	var envelope map[string]any
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("decode integration artifact %s: %v", path, err)
	}
	if data, ok := envelope["data"].(map[string]any); ok {
		return data
	}
	return envelope
}

func assertIntegrationArtifactsExist(t *testing.T, result regtestRoundResult, names ...string) {
	t.Helper()

	for _, name := range names {
		path := result.Artifact(name)
		if strings.TrimSpace(path) == "" {
			t.Fatalf("scenario %s is missing artifact mapping for %s", result.Scenario, name)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("missing integration artifact %s (%s): %v", name, path, err)
		}
	}
}

func latestIntegrationTransition(t *testing.T, table NativeMeshTableView) tablecustody.CustodyTransition {
	t.Helper()

	if len(table.CustodyTransitions) == 0 {
		t.Fatal("expected custody transitions in integration artifact")
	}
	return table.CustodyTransitions[len(table.CustodyTransitions)-1]
}

func countIntegrationEvents(table NativeMeshTableView, eventType string) int {
	count := 0
	for _, event := range table.Events {
		if stringValue(event.Body["type"]) == eventType {
			count++
		}
	}
	return count
}

func integrationTableHasEventType(table NativeMeshTableView, eventType string) bool {
	return countIntegrationEvents(table, eventType) > 0
}

func assertIntegrationCashOutEvents(t *testing.T, table NativeMeshTableView, minimum int) {
	t.Helper()

	cashOutCount := 0
	for index, event := range table.Events {
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
		transition, _, ok := linkedTransitionBySeqHash(table.CustodyTransitions, transitionHash, custodySeq)
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
	if cashOutCount < minimum {
		t.Fatalf("expected at least %d CashOut events in regtest artifact, got %d", minimum, cashOutCount)
	}
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

	assertRegtestRoundCustodyArtifactsWithBalanceCheck(t, result, true)
}

func assertRegtestRoundCustodyArtifactsWithBalanceCheck(t *testing.T, result regtestRoundResult, requireMovedFunds bool) {
	t.Helper()

	assertIntegrationArtifactsExist(t, result, "table-active", "table-after-hand", "table-after-cashout")

	afterHand := loadIntegrationArtifactTableView(t, result, "table-after-hand")
	afterCashout := loadIntegrationArtifactTableView(t, result, "table-after-cashout")

	if requireMovedFunds {
		assertRegtestRoundMovedFunds(t, afterHand)
	}

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
	assertIntegrationCashOutEvents(t, afterCashout, 2)
}

func assertRegtestRoundRecoveryArtifacts(t *testing.T, result regtestRoundResult) {
	t.Helper()

	assertIntegrationArtifactsExist(t, result, "table-active", "table-after-hand")
	if path := result.Artifact("table-after-cashout"); strings.TrimSpace(path) != "" {
		if _, err := os.Stat(path); err == nil {
			t.Fatalf("recovery scenario unexpectedly produced a cash-out artifact at %s", path)
		}
	}

	afterHand := loadIntegrationArtifactTableView(t, result, "table-after-hand")
	assertRegtestRoundMovedFunds(t, afterHand)

	var recovered tablecustody.CustodyTransition
	var recoveredIndex int
	foundRecovered := false
	for index, transition := range afterHand.CustodyTransitions {
		if transition.Kind != tablecustody.TransitionKindTimeout {
			continue
		}
		if transition.Proof.RecoveryWitness == nil && transition.Proof.ChallengeWitness == nil {
			continue
		}
		if transition.Proof.SettlementWitness != nil {
			t.Fatalf("recovery timeout transition %d unexpectedly includes a settlement witness", index)
		}
		if strings.TrimSpace(transition.Proof.ArkIntentID) != "" || strings.TrimSpace(transition.Proof.ArkTxID) != "" {
			t.Fatalf("recovery timeout transition %d unexpectedly carries live Ark settlement ids", index)
		}
		recovered = transition
		recoveredIndex = index
		foundRecovered = true
	}
	if !foundRecovered {
		t.Fatal("expected a timeout transition finalized from recovery or challenge proof material")
	}

	switch {
	case recovered.Proof.RecoveryWitness != nil:
		if strings.TrimSpace(recovered.Proof.RecoveryWitness.SourceTransitionHash) == "" {
			t.Fatalf("recovery timeout transition %d is missing its source transition hash", recoveredIndex)
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
	case recovered.Proof.ChallengeWitness != nil:
		if recovered.Proof.ChallengeBundle == nil {
			t.Fatal("expected the challenge-based timeout completion to carry its bundle")
		}
		if recovered.Proof.ChallengeBundle.Kind != tablecustody.TransitionKindTimeout {
			t.Fatalf("expected challenge-based timeout completion to use bundle kind %q, got %q", tablecustody.TransitionKindTimeout, recovered.Proof.ChallengeBundle.Kind)
		}
		if strings.TrimSpace(recovered.Proof.ChallengeWitness.TransactionID) == "" {
			t.Fatal("expected the challenge witness to include a transaction id")
		}
		foundChallengeTxID := false
		for _, txid := range recovered.Proof.ChallengeWitness.BroadcastTxIDs {
			if txid == recovered.Proof.ChallengeWitness.TransactionID {
				foundChallengeTxID = true
				break
			}
		}
		if !foundChallengeTxID {
			t.Fatal("expected the challenge witness broadcast metadata to include the timeout transaction id")
		}
		if recoveredIndex == 0 {
			t.Fatal("expected the challenge-based timeout completion to follow a source transition")
		}
		openTransition := afterHand.CustodyTransitions[recoveredIndex-1]
		if openTransition.Kind != tablecustody.TransitionKindTurnChallengeOpen {
			t.Fatalf("expected the challenge-based timeout completion to follow %q, got %q", tablecustody.TransitionKindTurnChallengeOpen, openTransition.Kind)
		}
		if openTransition.Proof.ChallengeWitness == nil {
			t.Fatal("expected the source turn-challenge-open transition to carry challenge witness metadata")
		}
	default:
		t.Fatal("expected the timeout completion to carry either recovery or challenge witness metadata")
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

func assertRegtestRoundAbortedHandArtifacts(t *testing.T, result regtestRoundResult) {
	t.Helper()

	assertIntegrationArtifactsExist(t, result, "table-after-abort")
	afterAbort := loadIntegrationArtifactTableView(t, result, "table-after-abort")
	if !integrationTableHasEventType(afterAbort, "HandAbort") {
		t.Fatal("expected HandAbort event in aborted-hand artifact")
	}
	if afterAbort.LatestCustodyState == nil {
		t.Fatal("expected latest custody state in aborted-hand artifact")
	}

	afterHand := loadIntegrationArtifactTableView(t, result, "table-after-hand")
	abortHandNumber := -1
	if afterAbort.PublicState != nil {
		abortHandNumber = afterAbort.PublicState.HandNumber
	} else if afterAbort.LatestSnapshot != nil {
		abortHandNumber = afterAbort.LatestSnapshot.HandNumber
	}
	afterHandNumber := -1
	if afterHand.PublicState != nil {
		afterHandNumber = afterHand.PublicState.HandNumber
	} else if afterHand.LatestSnapshot != nil {
		afterHandNumber = afterHand.LatestSnapshot.HandNumber
	}
	if afterHandNumber <= abortHandNumber {
		t.Fatal("expected a fresh hand to start after the aborted-hand artifact")
	}
	assertRegtestRoundCustodyArtifacts(t, result)
}

func assertRegtestRoundAllInArtifacts(t *testing.T, result regtestRoundResult) {
	t.Helper()

	assertIntegrationArtifactsExist(t, result, "table-after-all-in")
	afterAllIn := loadIntegrationArtifactTableView(t, result, "table-after-all-in")
	foundAllIn := false
	if afterAllIn.PublicState != nil {
		for _, seat := range afterAllIn.PublicState.SeatedPlayers {
			if seat.Status == string(game.PlayerStatusAllIn) {
				foundAllIn = true
				break
			}
		}
	}
	if !foundAllIn && afterAllIn.LatestCustodyState != nil {
		for _, claim := range afterAllIn.LatestCustodyState.StackClaims {
			if claim.AllIn {
				foundAllIn = true
				break
			}
		}
	}
	if !foundAllIn {
		t.Fatal("expected explicit all-in status in the all-in artifact")
	}

	afterHand := loadIntegrationArtifactTableView(t, result, "table-after-hand")
	latest := latestIntegrationTransition(t, afterHand)
	if latest.Kind != tablecustody.TransitionKindShowdownPayout {
		t.Fatalf("expected all-in hand to settle via %q, got %q", tablecustody.TransitionKindShowdownPayout, latest.Kind)
	}
	assertRegtestRoundCustodyArtifactsWithBalanceCheck(t, result, false)
}

func assertTurnChallengeOpenArtifact(t *testing.T, table NativeMeshTableView) {
	t.Helper()

	latest := latestIntegrationTransition(t, table)
	if latest.Kind != tablecustody.TransitionKindTurnChallengeOpen {
		t.Fatalf("expected latest transition kind %q, got %q", tablecustody.TransitionKindTurnChallengeOpen, latest.Kind)
	}
	if latest.Proof.ChallengeWitness == nil {
		t.Fatal("expected turn challenge open transition to carry a challenge witness")
	}
	if latest.Proof.ChallengeBundle == nil || latest.Proof.ChallengeBundle.Kind != tablecustody.TransitionKindTurnChallengeOpen {
		t.Fatalf("expected turn challenge open bundle kind %q, got %+v", tablecustody.TransitionKindTurnChallengeOpen, latest.Proof.ChallengeBundle)
	}
	if table.PendingTurnChallenge == nil || table.PendingTurnChallenge.Status == "" {
		t.Fatalf("expected pending turn challenge state, got %+v", table.PendingTurnChallenge)
	}
}

func assertRegtestRoundTurnChallengeOptionArtifacts(t *testing.T, result regtestRoundResult) {
	t.Helper()

	assertIntegrationArtifactsExist(t, result, "table-after-challenge-open", "table-after-challenge-resolution", "table-after-hand")
	openTable := loadIntegrationArtifactTableView(t, result, "table-after-challenge-open")
	assertTurnChallengeOpenArtifact(t, openTable)

	resolvedTable := loadIntegrationArtifactTableView(t, result, "table-after-challenge-resolution")
	if resolvedTable.PendingTurnChallenge != nil {
		t.Fatalf("expected challenge resolution artifact to clear the pending turn challenge, got %+v", resolvedTable.PendingTurnChallenge)
	}
	latest := latestIntegrationTransition(t, resolvedTable)
	if latest.Kind != tablecustody.TransitionKindAction {
		t.Fatalf("expected challenge option resolution to land as %q, got %q", tablecustody.TransitionKindAction, latest.Kind)
	}
	if latest.Proof.ChallengeWitness == nil {
		t.Fatal("expected challenge option resolution to carry a challenge witness")
	}
	if latest.Proof.ChallengeBundle == nil || latest.Proof.ChallengeBundle.Kind != tablecustody.TransitionKindAction {
		t.Fatalf("expected challenge option bundle kind %q, got %+v", tablecustody.TransitionKindAction, latest.Proof.ChallengeBundle)
	}
	afterHand := loadIntegrationArtifactTableView(t, result, "table-after-hand")
	if afterHand.PendingTurnChallenge != nil {
		t.Fatalf("expected final turn-challenge artifact to keep the pending turn challenge cleared, got %+v", afterHand.PendingTurnChallenge)
	}
	afterHandLatest := latestIntegrationTransition(t, afterHand)
	if afterHandLatest.Kind != tablecustody.TransitionKindAction {
		t.Fatalf("expected final turn-challenge artifact latest transition kind %q, got %q", tablecustody.TransitionKindAction, afterHandLatest.Kind)
	}
	if afterHandLatest.Proof.ChallengeWitness == nil {
		t.Fatal("expected final turn-challenge artifact to preserve challenge witness metadata")
	}
}

func assertRegtestRoundCashoutAfterChallengeArtifacts(t *testing.T, result regtestRoundResult) {
	t.Helper()

	assertIntegrationArtifactsExist(t, result, "table-after-challenge-open", "table-after-challenge-resolution")
	openTable := loadIntegrationArtifactTableView(t, result, "table-after-challenge-open")
	assertTurnChallengeOpenArtifact(t, openTable)

	resolvedTable := loadIntegrationArtifactTableView(t, result, "table-after-challenge-resolution")
	if resolvedTable.PendingTurnChallenge != nil {
		t.Fatalf("expected timeout challenge resolution artifact to clear the pending turn challenge, got %+v", resolvedTable.PendingTurnChallenge)
	}
	latest := latestIntegrationTransition(t, resolvedTable)
	if latest.Kind != tablecustody.TransitionKindTimeout {
		t.Fatalf("expected challenged timeout resolution to land as %q, got %q", tablecustody.TransitionKindTimeout, latest.Kind)
	}
	if latest.Proof.ChallengeWitness == nil {
		t.Fatal("expected challenged timeout resolution to carry a challenge witness")
	}
	if latest.Proof.ChallengeBundle == nil || latest.Proof.ChallengeBundle.Kind != tablecustody.TransitionKindTimeout {
		t.Fatalf("expected challenged timeout bundle kind %q, got %+v", tablecustody.TransitionKindTimeout, latest.Proof.ChallengeBundle)
	}
	assertRegtestRoundCustodyArtifacts(t, result)
}

func assertRegtestRoundChallengeEscapeArtifacts(t *testing.T, result regtestRoundResult) {
	t.Helper()

	assertIntegrationArtifactsExist(t, result, "table-after-challenge-open", "table-after-challenge-resolution", "table-after-hand")
	openTable := loadIntegrationArtifactTableView(t, result, "table-after-challenge-open")
	assertTurnChallengeOpenArtifact(t, openTable)

	escapedTable := loadIntegrationArtifactTableView(t, result, "table-after-challenge-resolution")
	if escapedTable.PendingTurnChallenge != nil {
		t.Fatalf("expected challenge escape artifact to clear the pending turn challenge, got %+v", escapedTable.PendingTurnChallenge)
	}
	latest := latestIntegrationTransition(t, escapedTable)
	if latest.Kind != tablecustody.TransitionKindTurnChallengeEscape {
		t.Fatalf("expected challenge escape transition kind %q, got %q", tablecustody.TransitionKindTurnChallengeEscape, latest.Kind)
	}
	if latest.Proof.ChallengeWitness == nil {
		t.Fatal("expected challenge escape transition to carry a challenge witness")
	}
	if latest.Proof.ChallengeBundle == nil || latest.Proof.ChallengeBundle.Kind != tablecustody.TransitionKindTurnChallengeEscape {
		t.Fatalf("expected challenge escape bundle kind %q, got %+v", tablecustody.TransitionKindTurnChallengeEscape, latest.Proof.ChallengeBundle)
	}
	if !integrationTableHasEventType(escapedTable, "HandAbort") {
		t.Fatal("expected challenge escape to append HandAbort")
	}
	afterHand := loadIntegrationArtifactTableView(t, result, "table-after-hand")
	if afterHand.PendingTurnChallenge != nil {
		t.Fatalf("expected post-escape final artifact to keep the pending turn challenge cleared, got %+v", afterHand.PendingTurnChallenge)
	}
	afterHandLatest := latestIntegrationTransition(t, afterHand)
	if afterHandLatest.Kind != tablecustody.TransitionKindTurnChallengeEscape {
		t.Fatalf("expected post-escape final artifact latest transition kind %q, got %q", tablecustody.TransitionKindTurnChallengeEscape, afterHandLatest.Kind)
	}
	if afterHandLatest.Proof.ChallengeWitness == nil {
		t.Fatal("expected post-escape final artifact to preserve challenge witness metadata")
	}
}

func assertRegtestRoundEmergencyExitArtifacts(t *testing.T, result regtestRoundResult) {
	t.Helper()

	assertIntegrationArtifactsExist(t, result, "table-before-emergency-exit", "emergency-exit-result", "table-after-emergency-exit", "table-after-cashout")
	beforeExit := loadIntegrationArtifactTableView(t, result, "table-before-emergency-exit")
	assertRegtestRoundMovedFunds(t, beforeExit)

	exitResult := loadIntegrationArtifactJSONMap(t, result, "emergency-exit-result")
	exitStatus := strings.TrimSpace(stringValue(exitResult["status"]))
	if exitStatus != "pending-exit" && exitStatus != "exited" {
		t.Fatalf("expected emergency exit result status pending-exit or exited, got %+v", exitResult)
	}
	settledArkTx := strings.TrimSpace(stringValue(exitResult["settledArkTx"]))
	if settledArkTx == "" {
		t.Fatalf("expected emergency exit result to include a settled Ark tx id, got %+v", exitResult)
	}
	receipt, ok := exitResult["receipt"].(map[string]any)
	if !ok {
		t.Fatalf("expected emergency exit result receipt payload, got %+v", exitResult["receipt"])
	}
	if kind := strings.TrimSpace(stringValue(receipt["kind"])); kind != string(tablecustody.TransitionKindEmergencyExit) {
		t.Fatalf("expected emergency exit receipt kind %q, got %+v", tablecustody.TransitionKindEmergencyExit, receipt["kind"])
	}
	if arkTxID := strings.TrimSpace(stringValue(receipt["arkTxid"])); arkTxID != settledArkTx {
		t.Fatalf("expected emergency exit receipt ark tx %q, got %q", settledArkTx, arkTxID)
	}
	fundsRequest, ok := receipt["fundsRequest"].(map[string]any)
	if !ok {
		t.Fatalf("expected emergency exit receipt funds request, got %+v", receipt["fundsRequest"])
	}
	exitExecution, ok := fundsRequest["exitExecution"].(map[string]any)
	if !ok {
		t.Fatalf("expected emergency exit receipt execution payload, got %+v", fundsRequest["exitExecution"])
	}
	broadcastTxIDs, err := decodeJSONValue[[]string](exitExecution["broadcastTxIds"])
	if err != nil {
		t.Fatalf("decode emergency exit broadcast tx ids: %v", err)
	}
	if len(broadcastTxIDs) == 0 {
		t.Fatalf("expected emergency exit receipt broadcast tx ids, got %+v", exitExecution)
	}
	if !slices.Contains(broadcastTxIDs, settledArkTx) {
		t.Fatalf("expected emergency exit settled tx %q inside broadcast tx ids %v", settledArkTx, broadcastTxIDs)
	}
	sourceRefs, err := decodeJSONValue[[]tablecustody.VTXORef](exitExecution["sourceRefs"])
	if err != nil {
		t.Fatalf("decode emergency exit source refs: %v", err)
	}
	if len(sourceRefs) == 0 {
		t.Fatalf("expected emergency exit receipt source refs, got %+v", exitExecution)
	}

	exitTable := loadIntegrationArtifactTableView(t, result, "table-after-emergency-exit")
	if exitTable.PublicState == nil {
		t.Fatal("expected emergency exit artifact to retain public state")
	}
	if got := strings.TrimSpace(exitTable.PublicState.Status); got != "seating" {
		t.Fatalf("expected emergency exit to leave the remaining player in seating status, got %q", got)
	}
	if integrationTableHasEventType(exitTable, "EmergencyExit") {
		latest := latestIntegrationTransition(t, exitTable)
		if latest.Kind != tablecustody.TransitionKindEmergencyExit {
			t.Fatalf("expected latest transition kind %q, got %q", tablecustody.TransitionKindEmergencyExit, latest.Kind)
		}
		if latest.Proof.ExitWitness == nil {
			t.Fatal("expected emergency exit transition to persist an exit witness")
		}
		if latest.Proof.SettlementWitness != nil {
			t.Fatal("did not expect emergency exit transition to carry an Ark settlement witness")
		}
	}

	afterCashout := loadIntegrationArtifactTableView(t, result, "table-after-cashout")
	assertIntegrationCashOutEvents(t, afterCashout, 1)
}

func assertRegtestRoundMultiHandArtifacts(t *testing.T, result regtestRoundResult) {
	t.Helper()

	assertIntegrationArtifactsExist(t, result, "table-after-hand-1", "table-after-hand", "table-after-cashout")
	firstHand := loadIntegrationArtifactTableView(t, result, "table-after-hand-1")
	secondHand := loadIntegrationArtifactTableView(t, result, "table-after-hand")
	if firstHand.PublicState == nil || secondHand.PublicState == nil {
		t.Fatal("expected public state in multi-hand artifacts")
	}
	if secondHand.PublicState.HandNumber <= firstHand.PublicState.HandNumber {
		t.Fatalf("expected later hand number after multi-hand run, got first=%d second=%d", firstHand.PublicState.HandNumber, secondHand.PublicState.HandNumber)
	}
	if countIntegrationEvents(secondHand, "HandResult") < 2 {
		t.Fatalf("expected at least two HandResult events after multi-hand run, got %d", countIntegrationEvents(secondHand, "HandResult"))
	}
	if firstHand.PublicState.DealerSeatIndex == secondHand.PublicState.DealerSeatIndex {
		t.Fatalf("expected dealer rotation across multi-hand run, got first=%v second=%v", firstHand.PublicState.DealerSeatIndex, secondHand.PublicState.DealerSeatIndex)
	}
	if firstHand.LatestCustodyState == nil || strings.TrimSpace(firstHand.LatestCustodyState.StateHash) == "" {
		t.Fatal("expected first multi-hand artifact to include the settled custody state hash")
	}
	continuedBlindPostFound := false
	blindPostCount := 0
	for _, transition := range secondHand.CustodyTransitions {
		if transition.Kind != tablecustody.TransitionKindBlindPost {
			continue
		}
		blindPostCount++
		if transition.PrevStateHash == firstHand.LatestCustodyState.StateHash {
			continuedBlindPostFound = true
		}
	}
	if blindPostCount < 2 {
		t.Fatalf("expected at least two blind-post transitions after multi-hand run, got %d", blindPostCount)
	}
	if !continuedBlindPostFound {
		t.Fatalf("expected a later blind-post transition to continue from settled custody state %q", firstHand.LatestCustodyState.StateHash)
	}
	afterCashout := loadIntegrationArtifactTableView(t, result, "table-after-cashout")
	assertIntegrationCashOutEvents(t, afterCashout, 2)
}

func assertRegtestRoundRecoveryShowdownArtifacts(t *testing.T, result regtestRoundResult) {
	t.Helper()

	assertIntegrationArtifactsExist(t, result, "table-before-recovery-showdown", "table-after-hand")
	beforeRecovery := loadIntegrationArtifactTableView(t, result, "table-before-recovery-showdown")
	if beforeRecovery.PublicState == nil || beforeRecovery.PublicState.Phase != string(game.StreetShowdownReveal) {
		t.Fatalf("expected showdown-reveal phase before recovery showdown, got %+v", beforeRecovery.PublicState)
	}

	afterHand := loadIntegrationArtifactTableView(t, result, "table-after-hand")
	assertRegtestRoundMovedFunds(t, afterHand)
	latest := latestIntegrationTransition(t, afterHand)
	if latest.Kind != tablecustody.TransitionKindShowdownPayout {
		t.Fatalf("expected recovered showdown payout transition kind %q, got %q", tablecustody.TransitionKindShowdownPayout, latest.Kind)
	}
	if latest.Proof.RecoveryWitness == nil {
		t.Fatal("expected recovered showdown payout to carry a recovery witness")
	}
	if latest.Proof.SettlementWitness != nil {
		t.Fatal("did not expect recovered showdown payout to carry a settlement witness")
	}
	if !integrationTableHasEventType(afterHand, "HandResult") {
		t.Fatal("expected HandResult event after recovered showdown payout")
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
