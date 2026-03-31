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

type cliDataEnvelope[T any] struct {
	Data T `json:"data"`
}

type regtestRoundResult struct {
	Output             string
	RunRoot            string
	TableActivePath    string
	TableAfterHandPath string
	TableAfterCashout  string
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
	cleanupRunRoot := false
	defer func() {
		if cleanupRunRoot {
			_ = os.RemoveAll(runRoot)
		}
	}()

	baseDir := filepath.Join(runRoot, "round")
	nigiriDatadir := integrationNigiriDatadir(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, scriptPath)
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(),
		"BASE="+baseDir,
		"NIGIRI_DATADIR="+nigiriDatadir,
		"PCLI_TIMEOUT_SECONDS=20",
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
	for _, marker := range []string{
		"TABLE_ID=",
		"Playing one hand automatically...",
		"Cashing out...",
		"Final wallet summaries:",
		"Done. Logs are under",
	} {
		if !strings.Contains(text, marker) {
			t.Fatalf("regtest round %s output missing %q (artifacts at %s)\n%s", scenario, marker, runRoot, text)
		}
	}
	cleanupRunRoot = true
	return regtestRoundResult{
		Output:             text,
		RunRoot:            runRoot,
		TableActivePath:    filepath.Join(baseDir, "artifacts", "table-active.json"),
		TableAfterHandPath: filepath.Join(baseDir, "artifacts", "table-after-hand.json"),
		TableAfterCashout:  filepath.Join(baseDir, "artifacts", "table-after-cashout.json"),
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
		if request.PlayerID != transition.ActingPlayerID {
			t.Fatalf("player action event %d request player %q does not match transition actor %q", index, request.PlayerID, transition.ActingPlayerID)
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
