package crmfollowup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
)

func TestCRMHelperTimingProcess(t *testing.T) {
	mode := os.Getenv("CRM_TIMING_TEST_CHILD")
	if mode == "" {
		return
	}
	if os.Getenv("MYANC_CRM_TIMING") != "1" {
		os.Exit(3)
	}
	line := `MYANC_TIMING {"version":1,"total_ns":90,"phases":{"connection_cli":{"count":3,"elapsed_ns":50},"resource_cli":{"count":1,"elapsed_ns":30}}}`
	switch mode {
	case "invalid":
		line = `MYANC_TIMING {"version":1,"total_ns":90,"phases":{"private-identity":{"count":1,"elapsed_ns":30}}}`
	case "missing":
		line = `MYANC_TIMING {"version":1,"total_ns":90,"phases":{"resource_cli":{"count":1}}}`
	case "oversized":
		line = strings.Repeat("private-stderr", 1000) + line
	case "oversized-newline":
		line = strings.Repeat("private-stderr", 1000) + "\n" + line
	}
	fmt.Fprintln(os.Stderr, "private-stderr-must-not-leak")
	fmt.Fprintln(os.Stderr, line)
	if _, err := fmt.Fprint(os.Stdout, `{"status":"found"}`); err != nil {
		os.Exit(4)
	}
	if mode == "failed" {
		os.Exit(1)
	}
	os.Exit(0)
}

func TestCRMHelperTimingKeepsOutputAndFailureWithoutLoggingStderr(t *testing.T) {
	for _, mode := range []string{"valid", "invalid", "missing", "oversized", "oversized-newline", "failed"} {
		var logs bytes.Buffer
		previous := slog.Default()
		slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
		out, err := runCommand(context.Background(), os.Args[0], "-test.run=TestCRMHelperTimingProcess", nil,
			append(os.Environ(), "CRM_TIMING_TEST_CHILD="+mode))
		slog.SetDefault(previous)
		if (err != nil) != (mode == "failed") || string(out) != `{"status":"found"}` {
			t.Fatalf("mode %s: changed child result: %q, %v", mode, out, err)
		}
		var record map[string]any
		if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &record); err != nil {
			t.Fatalf("mode %s: no timing envelope: %v", mode, err)
		}
		if record["measurement_available"] != (mode == "valid" || mode == "failed") || record["process_succeeded"] != (mode != "failed") {
			t.Fatalf("mode %s: invalid timing status: %v", mode, record)
		}
		if value, ok := record["process_ns"].(float64); !ok || value < 0 {
			t.Fatal("missing process duration")
		}
		if strings.Contains(logs.String(), "private-") || strings.Contains(logs.String(), "found") {
			t.Fatal("child contents leaked into timing")
		}
	}
}

func TestCRMHelperTimingPhasesStayNumericForTextAndJSON(t *testing.T) {
	for _, format := range []string{"text", "json"} {
		t.Run(format, func(t *testing.T) {
			var logs bytes.Buffer
			var handler slog.Handler = slog.NewTextHandler(&logs, nil)
			if format == "json" {
				handler = slog.NewJSONHandler(&logs, nil)
			}
			previous := slog.Default()
			slog.SetDefault(slog.New(handler))
			defer slog.SetDefault(previous)
			out, err := runCommand(context.Background(), os.Args[0], "-test.run=TestCRMHelperTimingProcess", nil,
				append(os.Environ(), "CRM_TIMING_TEST_CHILD=valid"))
			if err != nil || string(out) != `{"status":"found"}` {
				t.Fatalf("changed child result: %q, %v", out, err)
			}
			if format == "text" {
				for _, field := range []string{"phases.connection_cli.count=3", "phases.connection_cli.elapsed_ns=50", "phases.resource_cli.count=1", "phases.resource_cli.elapsed_ns=30"} {
					if !strings.Contains(logs.String(), " "+field+" ") && !strings.HasSuffix(strings.TrimSpace(logs.String()), " "+field) {
						t.Errorf("missing numeric timing field %s in %s", field, logs.String())
					}
				}
			} else {
				var record struct {
					Phases map[string]struct {
						Count     int64 `json:"count"`
						ElapsedNS int64 `json:"elapsed_ns"`
					} `json:"phases"`
				}
				if err := json.Unmarshal(logs.Bytes(), &record); err != nil {
					t.Fatal(err)
				}
				if record.Phases["connection_cli"].Count != 3 || record.Phases["connection_cli"].ElapsedNS != 50 || record.Phases["resource_cli"].Count != 1 || record.Phases["resource_cli"].ElapsedNS != 30 {
					t.Fatalf("incorrect numeric phases: %+v", record.Phases)
				}
			}
			if strings.Contains(logs.String(), "private-") || strings.Contains(logs.String(), "found") {
				t.Fatal("child contents leaked into timing")
			}
		})
	}
}

func TestTimingStderrPipeCopyHonorsBound(t *testing.T) {
	var capture timingStderr
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := reader.Close(); err != nil {
			t.Errorf("close stderr pipe reader: %v", err)
		}
	}()
	done := make(chan error, 1)
	go func() {
		_, err := writer.Write(bytes.Repeat([]byte("x"), 16384))
		closeErr := writer.Close()
		if err == nil {
			err = closeErr
		}
		done <- err
	}()
	copied, err := io.Copy(&capture, reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if copied != 16384 {
		t.Fatalf("stderr was not fully drained: copied=%d", copied)
	}
	if capture.buffer.Len() > 8192 || !capture.overflow {
		t.Fatalf("stderr bound bypassed: stored=%d overflow=%v", capture.buffer.Len(), capture.overflow)
	}
	t.Logf("drained=%d stored=%d overflow=%v", copied, capture.buffer.Len(), capture.overflow)
}
