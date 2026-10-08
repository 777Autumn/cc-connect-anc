//go:build anc_capacity_canary

package crmfollowup

import "context"

// NewCapacityFixture is available only in the opt-in test build. It substitutes
// the external helper transport, keeping the production adapter and envelope.
func NewCapacityFixture(secret string, run func(context.Context, string, string, []byte, []string) ([]byte, error)) *Adapter {
	return &Adapter{command: markerCommand, hostSecret: secret, toolsEnabled: true, run: run}
}
