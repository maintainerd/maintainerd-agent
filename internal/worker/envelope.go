package worker

import (
	"encoding/json"
	"fmt"
	"strings"

	kitruntime "github.com/maintainerd/kit/runtime"
)

// envelope is the parsed shape of a WorkItem's spec_json:
//
//	{"workload": <kit WorkloadSpec JSON>, "tier": "system"|"", "teardown": bool}
//
// The legacy bare shape {image,name,cmd,env} (what today's core still emits)
// is also accepted and mapped into a WorkloadSpec so existing deployments keep
// working; core adopts the envelope in the next phase, after which the legacy
// path can be dropped.
type envelope struct {
	Workload kitruntime.WorkloadSpec
	Tier     string
	Teardown bool
}

// wireEnvelope is the raw JSON carrier for the new shape.
type wireEnvelope struct {
	Workload json.RawMessage `json:"workload"`
	Tier     string          `json:"tier"`
	Teardown bool            `json:"teardown"`
}

// legacySpec is the pre-envelope bare container shape.
type legacySpec struct {
	Image string            `json:"image"`
	Name  string            `json:"name"`
	Cmd   []string          `json:"cmd"`
	Env   map[string]string `json:"env"`
}

// parseEnvelope decodes a work item's spec JSON, envelope first, legacy shape
// second. Anything that fits neither is an error — the worker reports it as
// failed rather than guessing at desired state, because a half-understood
// spec converged wrong is worse than one rejected loudly.
func parseEnvelope(specJSON string) (envelope, error) {
	trimmed := strings.TrimSpace(specJSON)
	if trimmed == "" {
		return envelope{}, fmt.Errorf("empty spec")
	}

	var wire wireEnvelope
	if err := json.Unmarshal([]byte(trimmed), &wire); err != nil {
		return envelope{}, fmt.Errorf("spec is not valid JSON: %w", err)
	}

	if len(wire.Workload) > 0 && string(wire.Workload) != "null" {
		var spec kitruntime.WorkloadSpec
		if err := json.Unmarshal(wire.Workload, &spec); err != nil {
			return envelope{}, fmt.Errorf("workload does not parse as a WorkloadSpec: %w", err)
		}
		return envelope{Workload: spec, Tier: wire.Tier, Teardown: wire.Teardown}, nil
	}

	// A teardown needs no workload body; honor the envelope fields alone.
	if wire.Teardown {
		return envelope{Tier: wire.Tier, Teardown: true}, nil
	}

	var legacy legacySpec
	if err := json.Unmarshal([]byte(trimmed), &legacy); err != nil {
		return envelope{}, fmt.Errorf("spec fits neither the envelope nor the legacy shape: %w", err)
	}
	if legacy.Image == "" {
		return envelope{}, fmt.Errorf("spec has no workload and no legacy image")
	}
	return envelope{Workload: kitruntime.WorkloadSpec{
		Image: legacy.Image,
		Name:  legacy.Name,
		Cmd:   legacy.Cmd,
		Env:   legacy.Env,
	}}, nil
}
