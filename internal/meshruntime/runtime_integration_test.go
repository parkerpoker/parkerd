//go:build integration

package meshruntime

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
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

func TestRegtestRoundUsesRealArkCustody(t *testing.T) {
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
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, scriptPath)
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(),
		"BASE="+baseDir,
		"PCLI_TIMEOUT_SECONDS=20",
	)
	output, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		t.Fatalf("regtest round timed out after %s (artifacts at %s)\n%s", 15*time.Minute, runRoot, string(output))
	}
	if err != nil {
		t.Fatalf("regtest round failed: %v (artifacts at %s)\n%s", err, runRoot, string(output))
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
			t.Fatalf("regtest round output missing %q (artifacts at %s)\n%s", marker, runRoot, text)
		}
	}
	cleanupRunRoot = true
}
