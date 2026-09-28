package codex

import (
	"context"
	"crypto/md5" // LibreOffice's IPC naming protocol, not a security digest.
	"encoding/binary"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
)

func macOfficeSocket(root, runAsUser string) (string, error) {
	if runAsUser == "" || strings.HasPrefix(runAsUser, "-") {
		return "", errors.New("invalid Office runtime identity")
	}
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve work root: %w", err)
	}
	// Native id works with the candidate's CGO_ENABLED=0 build and macOS accounts;
	// do not guess the model UID from the gateway or a writable output's owner.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/bin/id", "-u", runAsUser).Output()
	if err != nil {
		return "", fmt.Errorf("resolve Office runtime UID: %w", err)
	}
	uid, err := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 32)
	if err != nil || uid == 0 {
		return "", errors.New("office requires a known ordinary UID")
	}
	return officeSocketName(filepath.Join(canonical, "outputs", ".office-profile"), uint32(uid)), nil
}

// Match LibreOffice 26.8 desktop/source/deployment/misc/dp_misc.cxx and
// Python Path.as_uri(): UTF-16LE URI digest, each digest byte WITHOUT zero pad.
func officeSocketName(profile string, uid uint32) string {
	var uri strings.Builder
	uri.WriteString("file://")
	for _, b := range []byte(filepath.ToSlash(profile)) {
		if b == '/' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || strings.ContainsRune("-._~", rune(b)) {
			uri.WriteByte(b)
		} else {
			fmt.Fprintf(&uri, "%%%02X", b)
		}
	}
	units := utf16.Encode([]rune(uri.String()))
	data := make([]byte, 2*len(units))
	for i, unit := range units {
		binary.LittleEndian.PutUint16(data[2*i:], unit)
	}
	digest := md5.Sum(data)
	var suffix strings.Builder
	for _, b := range digest {
		fmt.Fprintf(&suffix, "%x", b)
	}
	return fmt.Sprintf("/private/tmp/OSL_PIPE_%d_SingleOfficeIPC_%s", uid, suffix.String())
}
