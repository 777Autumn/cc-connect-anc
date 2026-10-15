package crmfollowup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

type helperPhaseTiming struct {
	Count     *int64 `json:"count"`
	ElapsedNS *int64 `json:"elapsed_ns"`
}

type helperTiming struct {
	Version int                          `json:"version"`
	TotalNS *int64                       `json:"total_ns"`
	Phases  map[string]helperPhaseTiming `json:"phases"`
}

// Discard unbounded/unstructured stderr, including native CLI error content.
// A named buffer prevents io.Copy from bypassing Write via Buffer.ReadFrom.
type timingStderr struct {
	buffer   bytes.Buffer
	overflow bool
}

func (b *timingStderr) Write(p []byte) (int, error) {
	if len(p) > 8192-b.buffer.Len() {
		b.overflow = true
	} else if !b.overflow {
		_, _ = b.buffer.Write(p)
	}
	return len(p), nil
}

func logHelperTiming(ctx context.Context, subcommand string, stderr *timingStderr, elapsed time.Duration, succeeded bool) {
	var report helperTiming
	valid := false
	if !stderr.overflow {
		lines := bytes.Split(bytes.TrimSpace(stderr.buffer.Bytes()), []byte("\n"))
		var candidate []byte
		for _, line := range lines {
			if bytes.HasPrefix(line, []byte("MYANC_TIMING ")) {
				if candidate != nil {
					candidate = nil
					break
				}
				candidate = bytes.TrimPrefix(line, []byte("MYANC_TIMING "))
			}
		}
		decoder := json.NewDecoder(bytes.NewReader(candidate))
		decoder.DisallowUnknownFields()
		valid = decoder.Decode(&report) == nil && decoder.Decode(new(any)) == io.EOF &&
			report.Version == 1 && report.TotalNS != nil && *report.TotalNS >= 0 &&
			*report.TotalNS <= int64(10*time.Minute) && report.Phases != nil
		for name, phase := range report.Phases {
			switch name {
			case "connection_cli", "resource_cli", "connection_db_setup", "connection_lock_wait":
			default:
				valid = false
			}
			if phase.Count == nil || phase.ElapsedNS == nil || *phase.Count <= 0 || *phase.Count > 100000 || *phase.ElapsedNS < 0 ||
				report.TotalNS == nil || *phase.ElapsedNS > *report.TotalNS {
				valid = false
			}
		}
	}
	attrs := []any{"trace_id", core.ActionToolTimingID(ctx), "process_ns", elapsed.Nanoseconds(),
		"subcommand_sha256", fmt.Sprintf("%x", sha256.Sum256([]byte(subcommand))),
		"process_succeeded", succeeded, "measurement_available", valid}
	if valid {
		// Structured numeric attrs keep TextHandler and JSONHandler equally readable.
		phases := make([]any, 0, len(report.Phases))
		for _, name := range []string{"connection_cli", "resource_cli", "connection_db_setup", "connection_lock_wait"} {
			if phase, ok := report.Phases[name]; ok {
				phases = append(phases, slog.Group(name, slog.Int64("count", *phase.Count), slog.Int64("elapsed_ns", *phase.ElapsedNS)))
			}
		}
		attrs = append(attrs, "python_total_ns", *report.TotalNS, slog.Group("phases", phases...))
	}
	slog.Info("CRM helper timing", attrs...)
}
