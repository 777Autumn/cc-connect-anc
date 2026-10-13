package core

import (
	"strings"
	"testing"
)

func TestBanquetContextReadRoutesOnlyToReservedCRMHost(t *testing.T) {
	command, _, err := decodeActionToolRequest(strings.NewReader(`{"command":"banquet-context","input":{"banquet_id":"recFixture"}}`))
	if err != nil || command != "banquet-context" {
		t.Fatal("read-only context tool rejected", command, err)
	}
	_, primary, _, _ := actionToolFixture(t)
	if ValidateActionHosts(CombineActionHosts(primary, &secondToolHost{}, []string{"banquet-context"})) == nil {
		t.Fatal("another host can intercept banquet context")
	}
}
