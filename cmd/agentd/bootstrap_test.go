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
