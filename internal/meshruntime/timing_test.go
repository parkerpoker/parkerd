package meshruntime

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestMeshTimingLoggerDisabledSkipsEmission(t *testing.T) {
	called := 0
	logger := newMeshTimingLogger(false, func(format string, args ...any) {
		called++
	})

	logger.Start(meshTimingFields{Metric: "disabled"}).End(nil)

	if called != 0 {
		t.Fatalf("expected disabled timing logger to skip emission, got %d calls", called)
	}
}

func TestMeshTimingLoggerEmitsStructuredFields(t *testing.T) {
	lines := []string{}
	logger := newMeshTimingLogger(true, func(format string, args ...any) {
		lines = append(lines, strings.TrimSpace(fmt.Sprintf(format, args...)))
	})

	fields := meshTimingFields{
		Metric:         "custody_finalize_real_total",
		TableID:        "table-1",
		CustodySeq:     7,
		TransitionKind: "action",
		Phase:          "preflop",
		RequestHash:    "req-123",
		Purpose:        "proof",
		PlayerID:       "player-1",
		InputCount:     2,
		OutputCount:    3,
		BundleCount:    1,
		BundleKind:     "timeout",
	}

	logger.Start(fields).End(errors.New("boom"))

	if len(lines) != 1 {
		t.Fatalf("expected one timing line, got %d", len(lines))
	}
	if !strings.HasPrefix(lines[0], meshTimingLogPrefix+" ") {
		t.Fatalf("expected timing prefix, got %q", lines[0])
	}

	var entry meshTimingEntry
	if err := json.Unmarshal([]byte(strings.TrimPrefix(lines[0], meshTimingLogPrefix+" ")), &entry); err != nil {
		t.Fatalf("unmarshal timing entry: %v", err)
	}
	if entry.Metric != fields.Metric || entry.TableID != fields.TableID || entry.RequestHash != fields.RequestHash {
		t.Fatalf("expected propagated fields, got %+v", entry)
	}
	if entry.CustodySeq != fields.CustodySeq || entry.InputCount != fields.InputCount || entry.OutputCount != fields.OutputCount {
		t.Fatalf("expected numeric fields to propagate, got %+v", entry)
	}
	if entry.BundleCount != fields.BundleCount || entry.BundleKind != fields.BundleKind {
		t.Fatalf("expected bundle fields to propagate, got %+v", entry)
	}
	if entry.OK {
		t.Fatalf("expected error timing entry to mark ok=false, got %+v", entry)
	}
	if entry.Err != "boom" {
		t.Fatalf("expected err field to propagate, got %+v", entry)
	}
	if entry.DurationMs < 0 {
		t.Fatalf("expected non-negative duration, got %+v", entry)
	}
}
