package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
)

// Mode is how strictly gaps are treated.
type Mode string

const (
	ModeOff        Mode = "off"        // no sandbox (host execution)
	ModePermissive Mode = "permissive" // sandbox when possible, report gaps
	ModeStrict     Mode = "strict"     // any required gap rejects the run
)

// GapError is returned when a strict run is rejected.
type GapError struct {
	Backend string
	Gaps    []Gap
}

func (e *GapError) Error() string {
	var ids []string
	for _, g := range e.Gaps {
		ids = append(ids, string(g.Capability))
	}
	return fmt.Sprintf("sandbox backend %q cannot enforce required capabilities: %s", e.Backend, strings.Join(ids, ", "))
}

// Evaluate computes requested-policy minus backend-enforcement.
func Evaluate(backend string, spec SandboxSpec, enforced []CapabilityID, caveats []string) CheckResult {
	have := map[CapabilityID]bool{}
	for _, c := range enforced {
		have[c] = true
	}
	res := CheckResult{Backend: backend, Caveats: caveats}
	for c := range have {
		res.Enforced = append(res.Enforced, c)
	}
	sort.Slice(res.Enforced, func(i, j int) bool { return res.Enforced[i] < res.Enforced[j] })
	for _, c := range Required(spec) {
		if !have[c] {
			res.Gaps = append(res.Gaps, Gap{Capability: c, Reason: "not enforced by backend"})
		}
	}
	return res
}

// Admit applies the mode to a check result. A nil error means the run may start
// (permissive callers should still log result.Gaps).
func Admit(mode Mode, res CheckResult) error {
	if mode == ModeStrict && len(res.Gaps) > 0 {
		return &GapError{Backend: res.Backend, Gaps: res.Gaps}
	}
	return nil
}

// Manifest is a backend's declared capability set, used for offline checking
// (e.g. a macOS dev machine validating policy against the production manifest).
type Manifest struct {
	Backend         string         `json:"backend"`
	RegistryVersion int            `json:"registry_version"`
	Capabilities    []CapabilityID `json:"capabilities"`
	Caveats         []string       `json:"caveats,omitempty"`
}

// LoadManifest reads and validates a manifest file; unknown capabilities or a
// registry version mismatch fail closed.
func LoadManifest(path string) (Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, err
	}
	var m Manifest
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("manifest %s: %w", path, err)
	}
	if m.RegistryVersion != RegistryVersion {
		return Manifest{}, fmt.Errorf("manifest %s: registry version %d, want %d", path, m.RegistryVersion, RegistryVersion)
	}
	for _, c := range m.Capabilities {
		if _, ok := Registry[c]; !ok {
			return Manifest{}, fmt.Errorf("manifest %s: unknown capability %q", path, c)
		}
	}
	return m, nil
}

// ManifestBackend is a Check-only backend over a manifest.
type ManifestBackend struct{ M Manifest }

func (b ManifestBackend) Check(_ context.Context, spec SandboxSpec) (CheckResult, error) {
	return Evaluate(b.M.Backend, spec, b.M.Capabilities, b.M.Caveats), nil
}

func (b ManifestBackend) Prepare(context.Context, SandboxSpec) (PreparedSandbox, error) {
	return nil, errors.New("manifest backend is check-only")
}
