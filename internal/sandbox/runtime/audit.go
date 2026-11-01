package runtime

import (
	"log/slog"

	"github.com/agent-runner/agent-runner/internal/sandbox"
	"github.com/tendant/agentbox/pkg/agentbox/audit"
)

// The audit record (Config.Audit) is the runs' security evidence, for
// agent-gate: each run's policy snapshot, its lifecycle, and every egress and
// model-channel decision. The slog events stay as they are; the record is
// the complete one, keyed by the run's RunID.

// auditRun is a run the record is following.
type auditRun struct {
	thread  string
	project string
	policy  string // the snapshot digest its events carry
	started bool
}

// auditPolicy records the run's policy snapshot: the resolved spec and what
// the backend said it enforces. Called once the check has an answer, so a
// rejected run is on record with the policy it was refused under.
func (r *Runtime) auditPolicy(req BeginReq, spec sandbox.SandboxSpec, res sandbox.CheckResult) {
	if r.cfg.Audit == nil {
		return
	}
	snap, err := audit.NewPolicySnapshot(res.Backend, string(r.cfg.Mode), sandbox.RegistryVersion, spec, res)
	if len(r.cfg.Models) > 0 {
		// How each provider's calls will be metered, declared before the
		// run: agent-gate judges every model_forwarded against it.
		snap.Metering = map[string]string{}
		for name, ch := range r.cfg.Models {
			snap.Metering[name] = ch.MeteringMode()
		}
	}
	var id string
	if err == nil {
		id, err = r.cfg.Audit.RecordPolicy(req.RunID, snap)
	}
	if err != nil {
		slog.Error("sandbox audit: policy snapshot not recorded", "run", req.RunID, "error", err)
	}
	r.auditMu.Lock()
	r.auditRuns[req.RunID] = &auditRun{thread: req.ThreadID, project: req.Project, policy: id}
	r.auditMu.Unlock()
}

// audit records one event of run runID. A failed write is logged; the
// record's sequence gap shows it to readers.
func (r *Runtime) audit(runID, typ string, detail any) {
	if r.cfg.Audit == nil || runID == "" {
		return
	}
	var policy string
	r.auditMu.Lock()
	if a := r.auditRuns[runID]; a != nil {
		policy = a.policy
	}
	r.auditMu.Unlock()
	if _, err := r.cfg.Audit.Record(audit.Event{RunID: runID, Type: typ, PolicyID: policy}, detail); err != nil {
		slog.Error("sandbox audit: event not recorded", "run", runID, "type", typ, "error", err)
	}
}

// auditLifecycle records the lifecycle events among the runtime's events.
func (r *Runtime) auditLifecycle(e Event) {
	if r.cfg.Audit == nil {
		return
	}
	var typ string
	switch e.Kind {
	case "sandbox.started":
		typ = audit.TypeSandboxCreated
	case "sandbox.finished":
		typ = audit.TypeSandboxClosed
	case "sandbox.rejected":
		typ = audit.TypeSandboxRejected
	case "sandbox.enforcement_lost":
		typ = audit.TypeEnforcementLost
	default:
		return
	}
	runs := []string{e.RunID}
	r.auditMu.Lock()
	if e.RunID == "" {
		// A thread lease: it was guarding the thread's started runs.
		runs = nil
		for id, a := range r.auditRuns {
			if a.thread == e.ThreadID && a.started {
				runs = append(runs, id)
			}
		}
	}
	if a := r.auditRuns[e.RunID]; a != nil && typ == audit.TypeSandboxCreated {
		a.started = true
	}
	// Whose run it is, where the record starts: created, or refused.
	var project string
	if a := r.auditRuns[e.RunID]; a != nil {
		project = a.project
	}
	r.auditMu.Unlock()
	for _, id := range runs {
		d := audit.LifecycleDetail{Backend: e.Backend, ThreadID: e.ThreadID, Reason: e.Detail}
		if typ == audit.TypeSandboxCreated || typ == audit.TypeSandboxRejected {
			d.Agent, d.Project = r.cfg.Agent, project
		}
		r.audit(id, typ, d)
	}
	r.auditMu.Lock()
	// A run's record ends when it finishes, or is refused before starting
	// (a refusal at a later launch leaves the run going).
	if a := r.auditRuns[e.RunID]; a != nil && (typ == audit.TypeSandboxClosed || typ == audit.TypeSandboxRejected && !a.started) {
		delete(r.auditRuns, e.RunID)
	}
	r.auditMu.Unlock()
}

// auditEgress records one decision of the run's egress gateway.
func (r *Runtime) auditEgress(runID string, allowed bool, host string) {
	typ := audit.TypeNetworkBlocked
	if allowed {
		typ = audit.TypeNetworkAllowed
	}
	r.audit(runID, typ, audit.NetworkDetail{Destination: host})
}

// auditModel records one decision of a run's model channel that refused a
// request (broker kinds: denied, limit, credential_error). A forwarded one
// is recorded by the channel's OnForward, with its metering and request id.
func (r *Runtime) auditModel(runID, provider, kind string) {
	typ := audit.TypeModelDenied
	switch kind {
	case "forwarded":
		return
	case "limit":
		typ = audit.TypeModelLimitHit
	}
	r.audit(runID, typ, audit.ModelDetail{Provider: provider, Reason: kind})
}
