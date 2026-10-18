//go:build linux || darwin

package files

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
)

func TestRecoverDeviceChangeInheritedFilesKeepValidation(t *testing.T) {
	for _, scenario := range []string{"offset", "wrong_file", "writable", "directory", "pipe", "path_only", "missing", "extra", "duplicate", "replaced", "content_changed", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			if scenario == "path_only" && runtime.GOOS != "linux" {
				t.Skip("Linux O_PATH")
			}
			f := newFixture(t, nil)
			output(t, f, "docx")
			second := filepath.Join(f.context.OutputDir, "second.txt")
			if err := os.WriteFile(second, []byte("second"), 0600); err != nil {
				t.Fatal(err)
			}
			changeFixtureDevice(t, f)
			request := fixtureRecoveryRequest(t, f)
			request.Works[0].ReadFiles = map[string]*os.File{}
			open := func(path string, flag int) *os.File {
				file, err := os.OpenFile(path, flag, 0)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = file.Close() })
				return file
			}
			path := filepath.Join(f.context.OutputDir, "result.docx")
			first := open(path, os.O_RDONLY)
			request.Works[0].ReadFiles["outputs/result.docx"] = first
			request.Works[0].ReadFiles["outputs/second.txt"] = open(second, os.O_RDONLY)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch scenario {
			case "offset":
				if _, err := first.Seek(0, io.SeekEnd); err != nil {
					t.Fatal(err)
				}
			case "wrong_file":
				request.Works[0].ReadFiles["outputs/result.docx"] = open(f.database, os.O_RDONLY)
			case "writable":
				request.Works[0].ReadFiles["outputs/result.docx"] = open(path, os.O_RDWR)
			case "directory":
				request.Works[0].ReadFiles["outputs/result.docx"] = open(f.context.OutputDir, os.O_RDONLY)
			case "pipe":
				r, w, err := os.Pipe()
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = r.Close(); _ = w.Close() })
				request.Works[0].ReadFiles["outputs/result.docx"] = r
			case "path_only":
				fd, err := syscall.Open(path, 0x200000, 0)
				if err != nil {
					t.Fatal(err)
				}
				file := os.NewFile(uintptr(fd), "O_PATH")
				t.Cleanup(func() { _ = file.Close() })
				request.Works[0].ReadFiles["outputs/result.docx"] = file
			case "missing":
				delete(request.Works[0].ReadFiles, "outputs/result.docx")
			case "extra":
				request.Works[0].ReadFiles["inputs/not-authorized"] = open(path, os.O_RDONLY)
			case "duplicate":
				request.Works[0].ReadFiles["outputs/second.txt"] = first
			case "replaced":
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err = os.Rename(path, path+"-old"); err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			case "content_changed":
				if err := os.WriteFile(path, []byte("changed after freeze"), 0600); err != nil {
					t.Fatal(err)
				}
			case "cancelled":
				cancel()
			}
			var before, after []byte
			if err := f.store.db.QueryRow("SELECT payload FROM file_state WHERE id=1").Scan(&before); err != nil {
				t.Fatal(err)
			}
			result, err := RecoverDeviceChange(ctx, f.database, f.snapshots, request)
			if scenario == "offset" {
				if err != nil || result.BeforeSHA256 == result.AfterSHA256 {
					t.Fatal("two reads did not reset offset", result, err)
				}
				if _, err = first.Stat(); err != nil {
					t.Fatal("borrowed handle closed", err)
				}
				return
			}
			if err == nil {
				t.Fatal("invalid capability or changed source accepted")
			}
			if err = f.store.db.QueryRow("SELECT payload FROM file_state WHERE id=1").Scan(&after); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("failed validation committed changes")
			}
		})
	}
}
