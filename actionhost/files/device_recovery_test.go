package files

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDecodeDeviceRecoveryRequestRejectsAmbiguousManifests(t *testing.T) {
	request := DeviceRecoveryRequest{StateSHA256: strings.Repeat("a", 64), Works: []DeviceRecoveryWork{{Project: "fixture", WorkSHA256: strings.Repeat("b", 64), Entries: map[string]RecoveryEntry{".": {Identity: "1:2:3:4", Mode: 0700, Directory: true}}}}}
	raw, err := json.MarshalIndent(request, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeDeviceRecoveryRequest(raw); err != nil {
		t.Fatal("canonical manifest rejected", err)
	}
	for _, raw := range []string{
		`{"state_sha256":"a","state_sha256":"b","works":[]}`,
		`{"State_SHA256":"a","works":[]}`,
		`{"state_sha256":"a","works":[],"unknown":true}`,
		`{"state_sha256":"a","works":[{"project":"x","work_sha256":"b","entries":{".":{"identity":"a","identity":"b","mode":448,"directory":true}}}]}`,
		`null`, `{} {}`, strings.Repeat(" ", 2<<20+1),
	} {
		if _, err := DecodeDeviceRecoveryRequest([]byte(raw)); err == nil {
			t.Fatal("ambiguous manifest accepted", raw[:min(len(raw), 100)])
		}
	}
}
