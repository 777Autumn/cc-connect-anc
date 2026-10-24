//go:build darwin || linux

package files

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
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
		if ctx.Err() == nil && errors.As(err, &rejected) && rejected.ExitCode() == 1 {
			return ErrInvalid
		}
		return ErrUnavailable
	}
	if reply.String() != "VIDEO_OK_V1\n" {
		return ErrUnavailable
	}
	return nil
}
