//go:build darwin || linux

package files

import (
	"bytes"
	"context"
	"errors"
	"github.com/chenhg5/cc-connect/core"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type videoReply struct{ bytes.Buffer }

func (b *videoReply) Write(p []byte) (int, error) {
	if len(p)+b.Len() > 64 {
		return 0, io.ErrShortBuffer
	}
	return b.Buffer.Write(p)
}

func validateVideo(ctx context.Context, validator string, data []byte) error {
	// A production root host cannot decode videos or silently change identity.
	if os.Geteuid() == 0 {
		slog.Warn("file work: video validator requires ordinary host identity")
		return ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 50*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, validator)
	cmd.Env = []string{"LANG=C.UTF-8"}
	cmd.Stdin = bytes.NewReader(data)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = time.Second
	var reply videoReply
	cmd.Stdout = &reply
	if err := cmd.Run(); err != nil {
		var rejected *exec.ExitError
		code, toolExit := videoFailure(reply.String())
		exitCode := -1
		if errors.As(err, &rejected) {
			exitCode = rejected.ExitCode()
		}
		slog.Warn("file work: video preflight rejected", "reason", code, "validator_exit", exitCode, "tool_exit", toolExit, "timed_out", ctx.Err() != nil)
		if ctx.Err() == nil && errors.As(err, &rejected) && rejected.ExitCode() == 1 {
			var key core.MsgKey
			switch code {
			case "video_truncated", "video_decode_incomplete", "video_sample_table_mismatch":
				key = core.MsgFileVideoIncomplete
			case "video_structure_budget", "video_size_budget", "video_container_unsupported", "video_external_reference",
				"video_tracks_unsupported", "video_audio_unsupported", "video_spec_unsupported", "video_variable_rate",
				"video_source_changed", "video_derived_budget":
				key = core.MsgFileVideoUnsupported
			}
			if key != "" {
				return errors.Join(ErrInvalid, core.NewFileInputError(key))
			}
		}
		return ErrUnavailable
	}
	if reply.String() != "VIDEO_OK_V1\n" {
		slog.Warn("file work: video validator returned an invalid protocol response")
		return ErrUnavailable
	}
	return nil
}

func videoFailure(reply string) (string, string) {
	fields := strings.Fields(reply)
	if len(fields) != 3 || fields[0] != "VIDEO_ERROR_V1" {
		return "video_runtime_unavailable", "unknown"
	}
	switch fields[1] {
	case "video_truncated", "video_structure_budget", "video_size_budget", "video_container_unsupported",
		"video_external_reference", "video_tracks_unsupported", "video_audio_unsupported", "video_spec_unsupported",
		"video_variable_rate", "video_sample_table_mismatch", "video_decode_incomplete", "video_source_changed",
		"video_derived_budget", "video_timeout", "video_pipe_budget", "video_accounting_unavailable",
		"video_memory_budget", "video_tool_exit", "video_runtime_unavailable":
	default:
		return "video_runtime_unavailable", "unknown"
	}
	value := "unknown"
	if n, err := strconv.Atoi(fields[2]); err == nil && n >= -128 && n <= 255 {
		value = strconv.Itoa(n)
	}
	return fields[1], value
}
