//go:build !darwin && !linux

package files

import "context"

func validateVideo(context.Context, string, []byte) error { return ErrUnavailable }
