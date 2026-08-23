package main

import (
	"strings"
	"testing"
)

// The control-plane gate is the difference between a host that alerts and a
// host that lies: without CORE_ADDR the agent used to start, report healthy and
// converge nothing forever. Development keeps that mode for driver work.
func TestRequireControlPlane(t *testing.T) {
	tests := []struct {
		name     string
		coreAddr string
		dev      bool
		wantErr  bool
	}{
		{name: "production with an address", coreAddr: "core:9090", wantErr: false},
		{name: "production without an address", coreAddr: "", wantErr: true},
		{name: "development without an address is runtime-only", coreAddr: "", dev: true, wantErr: false},
		{name: "development with an address", coreAddr: "core:9090", dev: true, wantErr: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := requireControlPlane(tt.coreAddr, tt.dev)
			if tt.wantErr != (err != nil) {
				t.Fatalf("err = %v, wantErr = %v", err, tt.wantErr)
			}
			if err == nil {
				return
			}
			// The usual cause is a typo'd variable in the unit's
			// EnvironmentFile, so the refusal has to name it.
			if !strings.Contains(err.Error(), "CORE_ADDR") {
				t.Errorf("error %q does not name CORE_ADDR", err)
			}
		})
	}
}

// TestRequireAgentIdentity closes the second-order form of the same failure.
//
// AGENT_UUID is checked during first enrollment, but EnsureEnrolled returns as
// soon as a client certificate exists on disk — so on every boot after the
// first, that check is unreachable. An agent that lost the variable came up
// holding a valid certificate and sent anonymous Register/Heartbeat/PullWork
// forever, converging nothing while reporting healthy.
//
// The gate is deliberately keyed on CORE_ADDR being set rather than on
// enrollment state: the UUID is what Core matches an RPC to a row by, so it is
// required whenever this agent will speak to Core at all, whether it enrolls
// this boot or presents a certificate it already had.
func TestRequireAgentIdentity(t *testing.T) {
	tests := []struct {
		name      string
		coreAddr  string
		agentUUID string
		dev       bool
		wantErr   bool
	}{
		{name: "production with both", coreAddr: "core:9090", agentUUID: "a-uuid", wantErr: false},
		{name: "production with a control plane and no uuid", coreAddr: "core:9090", agentUUID: "", wantErr: true},
		// No control plane means no RPC to be anonymous on. requireControlPlane
		// owns that case, and stacking a second refusal on it would report the
		// wrong variable for a host that is only missing CORE_ADDR.
		{name: "production runtime-only needs no uuid", coreAddr: "", agentUUID: "", wantErr: false},
		{name: "development is exempt, like the other relaxations", coreAddr: "core:9090", agentUUID: "", dev: true, wantErr: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := requireAgentIdentity(tt.coreAddr, tt.agentUUID, tt.dev)
			if tt.wantErr != (err != nil) {
				t.Fatalf("err = %v, wantErr = %v", err, tt.wantErr)
			}
			if err == nil {
				return
			}
			if !strings.Contains(err.Error(), "AGENT_UUID") {
				t.Errorf("error %q does not name AGENT_UUID", err)
			}
		})
	}
}
