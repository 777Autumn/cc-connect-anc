//go:build darwin || linux

package files

import (
	"strings"
	"testing"
)

func TestVideoFailureProtocolDoesNotLogPathsOrArbitraryText(t *testing.T) {
	code, exit := videoFailure("VIDEO_ERROR_V1 video_decode_incomplete 1\n")
	if code != "video_decode_incomplete" || exit != "1" {
		t.Fatal(code, exit)
	}
	for _, raw := range []string{"/private/member/data", "VIDEO_ERROR_V1 /private/member/data 1", "VIDEO_ERROR_V1 video_timeout /private/member/data"} {
		code, exit = videoFailure(raw)
		if strings.Contains(code+exit, "/") {
			t.Fatal("untrusted diagnostic escaped")
		}
	}
}
