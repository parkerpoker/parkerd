package meshruntime

import (
	"encoding/json"
	"log"
	"os"
	"strings"
	"time"
)

const meshTimingLogPrefix = "[mesh-timing]"

type meshTimingFields struct {
	Metric         string
	TableID        string
	Epoch          int
	TurnAnchorHash string
	CandidateHash  string
	Reason         string
	CustodySeq     int
	TransitionKind string
	Phase          string
	RequestHash    string
	Purpose        string
	PlayerID       string
	InputCount     int
	OutputCount    int
	BundleCount    int
	BundleKind     string
}

type meshTimingEntry struct {
	Metric         string  `json:"metric"`
	TableID        string  `json:"tableId,omitempty"`
	Epoch          int     `json:"epoch,omitempty"`
	TurnAnchorHash string  `json:"turnAnchorHash,omitempty"`
	CandidateHash  string  `json:"candidateHash,omitempty"`
	Reason         string  `json:"reason,omitempty"`
	CustodySeq     int     `json:"custodySeq,omitempty"`
	TransitionKind string  `json:"transitionKind,omitempty"`
	Phase          string  `json:"phase,omitempty"`
	RequestHash    string  `json:"requestHash,omitempty"`
	Purpose        string  `json:"purpose,omitempty"`
	PlayerID       string  `json:"playerId,omitempty"`
	InputCount     int     `json:"inputCount"`
	OutputCount    int     `json:"outputCount"`
	BundleCount    int     `json:"bundleCount"`
	BundleKind     string  `json:"bundleKind,omitempty"`
	DurationMs     float64 `json:"durationMs"`
	FinishedAt     string  `json:"finishedAt,omitempty"`
	OK             bool    `json:"ok"`
	Err            string  `json:"err,omitempty"`
}

type meshTimingLogger struct {
	enabled bool
	logf    func(format string, args ...any)
}

type meshTimingSpan struct {
	fields    meshTimingFields
	logger    meshTimingLogger
	startedAt time.Time
}

func newMeshTimingLogger(enabled bool, logf func(format string, args ...any)) meshTimingLogger {
	if logf == nil {
		logf = log.Printf
	}
	return meshTimingLogger{
		enabled: enabled,
		logf:    logf,
	}
}

func meshTimingLoggerFromEnv() meshTimingLogger {
	return newMeshTimingLogger(meshTimingMetricsEnabled(), log.Printf)
}

func meshTimingMetricsEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("PARKER_TIMING_METRICS"))) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

func (logger meshTimingLogger) Start(fields meshTimingFields) meshTimingSpan {
	return meshTimingSpan{
		fields:    fields,
		logger:    logger,
		startedAt: time.Now(),
	}
}

func startMeshTiming(fields meshTimingFields) meshTimingSpan {
	return meshTimingLoggerFromEnv().Start(fields)
}

func (span meshTimingSpan) End(err error) {
	if !span.logger.enabled {
		return
	}
	span.logger.emit(span.fields, time.Since(span.startedAt), err)
}

func (span meshTimingSpan) EndWith(fields meshTimingFields, err error) {
	if !span.logger.enabled {
		return
	}
	span.logger.emit(fields, time.Since(span.startedAt), err)
}

func (logger meshTimingLogger) emit(fields meshTimingFields, duration time.Duration, err error) {
	if !logger.enabled {
		return
	}
	entry := meshTimingEntry{
		Metric:         fields.Metric,
		TableID:        strings.TrimSpace(fields.TableID),
		Epoch:          fields.Epoch,
		TurnAnchorHash: strings.TrimSpace(fields.TurnAnchorHash),
		CandidateHash:  strings.TrimSpace(fields.CandidateHash),
		Reason:         strings.TrimSpace(fields.Reason),
		CustodySeq:     fields.CustodySeq,
		TransitionKind: strings.TrimSpace(fields.TransitionKind),
		Phase:          strings.TrimSpace(fields.Phase),
		RequestHash:    strings.TrimSpace(fields.RequestHash),
		Purpose:        strings.TrimSpace(fields.Purpose),
		PlayerID:       strings.TrimSpace(fields.PlayerID),
		InputCount:     fields.InputCount,
		OutputCount:    fields.OutputCount,
		BundleCount:    fields.BundleCount,
		BundleKind:     strings.TrimSpace(fields.BundleKind),
		DurationMs:     float64(duration) / float64(time.Millisecond),
		FinishedAt:     time.Now().UTC().Format(time.RFC3339Nano),
		OK:             err == nil,
	}
	if err != nil {
		entry.Err = err.Error()
	}
	body, marshalErr := json.Marshal(entry)
	if marshalErr != nil {
		logger.logf("%s %q", meshTimingLogPrefix, marshalErr.Error())
		return
	}
	logger.logf("%s %s", meshTimingLogPrefix, string(body))
}

func emitMeshTiming(fields meshTimingFields, duration time.Duration, err error) {
	meshTimingLoggerFromEnv().emit(fields, duration, err)
}
