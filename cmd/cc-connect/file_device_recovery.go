package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
	"github.com/chenhg5/cc-connect/actionhost/files"
	"github.com/chenhg5/cc-connect/config"
)

// Runs before runtime initialization; no platform, agent or sender is created.
func recoverFileDevices(ctx context.Context, args []string, input io.Reader) (files.DeviceRecoveryResult, error) {
	var empty files.DeviceRecoveryResult
	if len(args) != 2 || !filepath.IsAbs(args[0]) || filepath.Clean(args[0]) != args[0] {
		return empty, files.ErrInvalid
	}
	raw, err := readRecoveryConfig(args[0], args[1])
	if err != nil {
		return empty, err
	}
	var cfg config.Config
	if _, err := toml.Decode(string(raw), &cfg); err != nil {
		return empty, files.ErrInvalid
	}
	manifest, err := io.ReadAll(io.LimitReader(input, 2<<20+1))
	if err != nil {
		return empty, files.ErrInvalid
	}
	request, err := files.DecodeDeviceRecoveryRequest(manifest)
	if err != nil {
		return empty, err
	}
	ledger, snapshots := "", ""
	for _, work := range request.Works {
		matches := 0
		for _, project := range cfg.Projects {
			if project.Name != work.Project {
				continue
			}
			matches++
			fw := project.FileWork
			if !fw.Enabled || fw.Runtime != "native" || fw.Ledger == "" || fw.Snapshots == "" || (ledger != "" && (ledger != fw.Ledger || snapshots != fw.Snapshots)) {
				return empty, files.ErrScope
			}
			ledger, snapshots = fw.Ledger, fw.Snapshots
		}
		if matches != 1 {
			return empty, files.ErrScope
		}
	}
	if ledger == "" {
		return empty, files.ErrInvalid
	}
	lock, err := AcquireInstanceLock(args[0])
	if err != nil {
		return empty, files.ErrUnavailable
	}
	defer lock.Release()
	if _, err := readRecoveryConfig(args[0], args[1]); err != nil {
		return empty, files.ErrScope
	}
	return files.RecoverDeviceChange(ctx, ledger, snapshots, request)
}

func readRecoveryConfig(path, expected string) ([]byte, error) {
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Mode().Perm()&0022 != 0 || before.Size() > 1<<20 {
		return nil, files.ErrInvalid
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, files.ErrUnavailable
	}
	defer func() { _ = f.Close() }()
	raw, readErr := io.ReadAll(io.LimitReader(f, 1<<20+1))
	opened, statErr := f.Stat()
	current, pathErr := os.Lstat(path)
	digest := sha256.Sum256(raw)
	if readErr != nil || statErr != nil || pathErr != nil || !os.SameFile(before, opened) || !os.SameFile(opened, current) || current.Mode() != before.Mode() || len(raw) > 1<<20 || hex.EncodeToString(digest[:]) != expected {
		return nil, files.ErrInvalid
	}
	return raw, nil
}
