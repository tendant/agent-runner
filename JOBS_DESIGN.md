# Session Jobs — Design

Status: draft · Scope: agent-runner (agent manager, execution engine, API, config); a deployment in agent-stream-stack (or wherever agent-runner runs)

## 1. Problem

agent-runner is one long-lived process that does two different kinds of work:

| Half | What | Footprint |
|---|---|---|
| **Control** | bots (Agent Stream SSE, Telegram, WeChat polling), thread state and the intent analyzer (one cheap LLM call), multi-turn task records, the scheduler, the session queue and journal, the REST API | a small Go process, mostly idle |
| **Execution** | an agent session: workspace and repo clones, planner, iteration loop over a CLI (claude, codex, opencode, pi), reviewer, git sync, memory write-back | Node + CLIs + repos; minutes of CPU, memory and disk per run |

Both halves live in one container sized for the heavy one, running all the
time. Sessions share that container's disk, CLI login and CPU, so
`AGENT_MAX_CONCURRENT` stays low, and a misbehaving run can take the bots
down with it.

## 2. Goals and non-goals

Goals:

- **Execution on demand:** each session runs in its own short-lived job and nothing heavy runs between sessions.
- **The control half stays always on** and small: chat replies stay instant, and schedules, Telegram and WeChat keep working.
- **One switch:** `AGENT_EXECUTION_BACKEND=local` keeps today's behavior exactly; `k8s-job` runs sessions as Kubernetes Jobs.
- **Isolation:** one session per pod; a crash, OOM or runaway CLI ends that session only.
- **Survives restarts better than today:** a dispatcher restart re-attaches to running jobs instead of failing them (§10).
- **Nothing outside agent-runner learns what a session is.** agent-stream is unchanged.

Non-goals:

- A global job manager shared with other systems (§13). Kubernetes is the global layer: scheduling, quotas, retries, logs and cleanup.
- Scaling the control half to more than one replica (bots assume one live process per bot).
- Changing what a session does: the phases in `ExecuteAgentWithContext` stay as they are.

## 3. Decision summary

- **agent-runner owns its jobs.** Only it knows sessions: threads, tasks, planner and reviewer, progress, stop, recovery, memory write-back.
- **Execution sits behind a small backend interface** (§6) with three implementations: `local` (in process, today), `subprocess` (the runner binary as a child process; for tests and single hosts), and `k8s-job`.
- **The dispatcher is today's agent-runner** minus execution. **The runner is the same binary** with a new subcommand, `agent-runner run-session`, that executes one session and exits.
- **The runner reports to the dispatcher over HTTP**, outbound only (§7). The dispatcher never connects into a job pod.

## 4. What a session touches today

From `internal/execution/engine.go` (`ExecuteAgentWithContext` and its deferred finalization):

| Step | State touched | Where it must happen with jobs |
|---|---|---|
| Memory pull (`AGENT_MEMORY_PULL_ON_START`) | `MEMORY_DIR` (git) | runner (its own clone) |
| Prompt resolution | `MEMORY_DIR`, `agent.md`/`prompt.md`, `RUNNER_URL`, API key | runner |
| Workspace prep | `TMP_ROOT/<session>` or a task's `TMP_ROOT/task-*`, `REPO_CACHE_ROOT`, skills dir, git token | runner |
| Planner, iteration loop, reviewer, git sync | workspace, CLI login (`~/.claude`, `~/.codex`, ...), LLM keys | runner |
| Live steering | `liveControls` (in-process map) | runner, fed by the control channel (§7.3) |
| Agent calls back (`RUNNER_URL`: `/lock`, `/schedule`, ...) | dispatcher API | dispatcher, reached from the job (§8.5) |
| Outputs (`_send/`), `_schedule.json` | workspace → `OUTPUTS_ROOT`, scheduler | runner collects, dispatcher stores and submits |
| Task turn result (`recordTurnResult`) | task workspace `state/` | runner |
| Repo cache write-back | `REPO_CACHE_ROOT` (per-repo lock) | runner (§8.2) |
| Audit log, metrics | `LOGS_ROOT`, Prometheus | dispatcher, from the final report |
| Daily log, curation, memory push | `MEMORY_DIR` (memory mutex) | dispatcher, single writer (§8.3) |
| Chat notification, webhook | bots, callback dispatcher | dispatcher (unchanged) |
| Lock release (`locks.ReleaseAll`) | dispatcher's lock manager | dispatcher (unchanged) |

## 5. Architecture

```
            agent-stream / Telegram / WeChat / REST / scheduler
                                 │
                 ┌───────────────▼────────────────┐
                 │ dispatcher (agent-runner serve)│  always on, slim image
                 │ bots · threads · tasks · queue │
                 │ journal · scheduler · locks    │
                 │ audit log · memory writer      │
                 └──────┬───────────────▲─────────┘
           Backend.Start│               │ reports, results (HTTP, from the job)
                        │               │ control: stop, steer (long poll)
                 ┌──────▼───────────────┴─────────┐
                 │ runner (agent-runner           │  one Job per session,
                 │   run-session)                 │  heavy image (CLIs)
                 │ workspace · planner · loop ·   │
                 │ reviewer · git sync · outputs  │
                 └────────────────────────────────┘
```

The dispatcher's `agent.Manager` queue and worker pool stay. A worker no
longer runs `Engine.ExecuteAgent` itself: it calls `Backend.Start` and then
`Watch`es until the session is terminal, so `AGENT_MAX_CONCURRENT` becomes
"jobs in flight".

## 6. Backend interface

```go
// internal/execution/backend (sketch)
type Spec struct {
    SessionID  string
    Session    agent.SessionSpec // message, paths, limits, source, task dir and feedback, ...
    ReportURL  string            // dispatcher base URL for §7
    Token      string            // per-session bearer token (§11)
}

type Handle struct {
    Backend string // "local", "subprocess", "k8s-job"
    Ref     string // "" / pid / job name, recorded in the journal
}

type Backend interface {
    Start(ctx context.Context, spec Spec) (Handle, error)
    // Stop ends a session's job after its grace period (§9).
    Stop(ctx context.Context, h Handle) error
    // Alive reports whether the job still exists and whether it ended in failure;
    // used by the heartbeat check and by recovery (§10).
    Alive(ctx context.Context, h Handle) (running bool, failure string, err error)
}
```

- **local** wraps today's `Engine.ExecuteAgentWithContext` in-process, with an in-process reporter. No HTTP, no behavior change.
- **subprocess** runs `agent-runner run-session` as a child process against the same `DATA_DIR`. It exercises the whole protocol without Kubernetes; the e2e tests use it.
- **k8s-job** creates a `batch/v1` Job (§12).

A future global manager would be one more implementation of this interface.

## 7. Protocol

All requests come from the runner to the dispatcher, under
`/internal/sessions/{id}/`, authenticated by the per-session token. The
dispatcher listens for them on its normal port; the k8s Service is
cluster-internal.

### 7.1 Start

`run-session` reads its spec from `GET /internal/sessions/{id}/spec`
(the job carries only the session ID, dispatcher URL and token, so secrets and
long messages stay out of the Job object).

### 7.2 Reports

- `POST .../report`: a wire snapshot of the session (§7.4), sent on every change, coalesced to at most one every 2s. The dispatcher applies it to its `agent.Session`, so `GET /agent/{id}`, the session journal and `botcommon.PollAndReport` (5s polling) work unchanged.
- `POST .../result`: the final snapshot plus everything the dispatcher finalizes (§8): output files (multipart, capped by `AGENT_JOB_MAX_OUTPUT_BYTES`), schedule entries, the daily-log inputs (changed files, review), planner prompt and per-iteration prompts for the audit log.
- Both are idempotent (snapshot carries a sequence number; result is accepted once). The runner retries with backoff for up to `AGENT_JOB_REPORT_RETRY` (default 15m) before exiting non-zero.

### 7.3 Control

`GET .../control?wait=30s` is a long poll returning `{"stop": bool, "steer": [text...]}`
with steer messages queued since the last poll. The runner's
control loop feeds stop into the session context and steer into its
`agentSession` (today's `Engine.Steer`). `Engine.Steer` on the dispatcher
becomes "queue for the session's next control poll"; it still fails for
one-shot backends so `botcommon` keeps falling back to a follow-up run.

### 7.4 Wire snapshot

`agent.Session` hides much of its state from JSON (`json:"-"`: plan, review,
completed steps, turn status/question/summary/decisions, exec events,
warnings' context). A separate `agent.SessionWire` type carries every field the
dispatcher needs, with `Snapshot()` ↔ `SessionWire` conversions tested for
round-trip equality. Exec events are sent as a delta after the last
acknowledged `Seq`.

## 8. State

### 8.1 Workspaces

- A one-shot session's workspace lives on the job's own disk (`emptyDir`) and dies with it.
- A **task workspace** (`TMP_ROOT/task-*`, TASKS_DESIGN.md §6) must outlive the job: the next turn resumes it. It goes on a shared volume mounted by the dispatcher and every job at the same path, so `TaskWorkspacePath` and `FinishTaskWorkspace` work unchanged.

### 8.2 Repo cache

`REPO_CACHE_ROOT` moves to the same shared volume. Copying from it and the
per-repo write-back lock must then work across pods: the in-process lock in
`internal/executor` becomes a lock file (`flock`) next to each cached repo.
Without a shared volume (`AGENT_JOB_SHARED_VOLUME` unset), jobs clone fresh
and skip write-back, which costs time, not correctness.

The shared volume needs `ReadWriteMany`, or `ReadWriteOnce` with the dispatcher and
jobs pinned to one node (an RWO volume can be mounted by several pods on the
same node). Which storage class the cluster offers is an open question (§16).

### 8.3 Memory

The agent is told `MEMORY_DIR` and may edit memory itself during a run, and
finalization appends to it (daily log, curation, push). With jobs:

- **The runner works on its own clone** of the memory remote: pull at start (today's `MemoryPullOnStart`, now always on for jobs), and at the end commit and push the agent's own edits with `pushMemory`'s existing rebase-and-retry.
- **The dispatcher stays the single writer** of what finalization adds: it writes the daily log and runs curation from the `result` report, under the existing memory mutex, then pushes.
- The k8s-job backend therefore **requires the memory dir to have a git remote** (set up with `/memory`, `tmpl.InitMemoryGit`). The dispatcher passes the remote URL in the spec, and `run-session` refuses to start without one rather than silently losing memory edits.

### 8.4 Outputs and schedules

The runner sends `_send/` files and `_schedule.json` entries in `result`; the
dispatcher persists outputs to `OUTPUTS_ROOT` and submits schedules exactly as
`finalizeAgentOutputs` does today.

### 8.5 `RUNNER_URL`

Prompts tell the agent where agent-runner's API is (`RUNNER_URL`, today
`http://` + `API_BIND`, so loopback). In a job it is the dispatcher's
Service URL (`AGENT_JOB_DISPATCHER_URL`), and `API_KEY` in the prompt becomes the
per-session token, scoped to the agent-facing endpoints (`/lock`, `/schedule`,
`/sessions`) and to its own session.

## 9. Lifecycle

1. A worker takes the session from the queue, mints its token, `Start`s the backend, and journals `{status: running, backend, ref}`.
2. The runner fetches its spec and runs `ExecuteAgentWithContext` with a reporter instead of the local finalization steps.
3. **Stop** (`POST /agent/{id}/stop`, chat `/stop`): the session goes `stopping`, the next control poll tells the runner, which stops like today (context cancel, then finalize). After `AGENT_JOB_STOP_GRACE` (60s) without a result the dispatcher calls `Backend.Stop` (Job deletion) and marks the session stopped.
4. **Time limit:** the Job's `activeDeadlineSeconds` is `MaxTotalSeconds` + finalization margin (10m), a backstop under the runner's own deadline.
5. **Heartbeat:** no report for `AGENT_JOB_HEARTBEAT` (2m) → `Alive`. A gone or failed job fails the session with the pod's reason (OOMKilled, Evicted, image pull error) instead of a generic timeout.
6. **Result** → the dispatcher finalizes (§4), sets the terminal status, removes the journal entry; the Job deletes itself (`ttlSecondsAfterFinished`).

## 10. Recovery

Today a restart fails every running session (`internal/api/recovery.go`),
because the work died with the process. With jobs the work survives the dispatcher:

- On startup, each journaled running session with a job `Handle` is checked with `Alive`. A running job is **re-attached**: the session is restored from the journal, its watcher (`ResumeSession`) re-attached, and its next report brings it up to date. A finished job's result arrives through the runner's retries (§7.2).
- A job that is gone without a result fails the session with the targeted chat notice, as today.
- Queued sessions are re-enqueued as today.

This needs the journal to carry the handle and enough of the session to rebuild it, and the session token to be re-derivable (HMAC of the session ID with a dispatcher secret) rather than kept in memory.

## 11. Security

- Per-session bearer token for `/internal/sessions/{id}/*` and the agent-facing API; it dies with the session.
- The dispatcher's ServiceAccount may `create/get/list/delete` Jobs and `get` Pods (for failure reasons) in its own namespace only.
- Job pods run with `automountServiceAccountToken: false`, non-root, no access to the dispatcher's own secrets beyond what a session needs (LLM keys, git token, CLI login).
- A NetworkPolicy allows jobs to egress to the dispatcher Service, the LLM APIs and git hosts.

## 12. Kubernetes backend

Job template (built in Go, overridable fields from config):

- image `AGENT_JOB_IMAGE` (the full image with CLIs); command `agent-runner run-session --id <id>`
- env: `AGENT_SESSION_ID`, `AGENT_JOB_DISPATCHER_URL`, `AGENT_SESSION_TOKEN`; `envFrom` the runner's Secret (`AGENT_JOB_SECRET`) for LLM keys, git and memory tokens
- CLI login (`~/.claude`, `~/.codex`) from a Secret or the shared volume (§16)
- volumes: `emptyDir` for the one-shot workspace; the shared volume (`AGENT_JOB_SHARED_VOLUME`) for task workspaces and the repo cache
- resources `AGENT_JOB_CPU` / `AGENT_JOB_MEMORY`; `backoffLimit: 0` (the engine already retries iterations; a re-run job would repeat side effects such as pushes); `activeDeadlineSeconds` (§9); `ttlSecondsAfterFinished: 600`
- labels `agent-runner/session=<id>`, `agent-runner/instance=<name>` so a dispatcher only ever lists its own jobs

A namespace `ResourceQuota` (`count/jobs.batch`, CPU, memory) is the cluster-wide cap across agent-runner instances.

## 13. Alternatives considered

- **Wake the whole agent-runner on demand** (agent-stream calls a wake URL; agent-runner exits when idle). Cold start on every chat reply, and schedules, Telegram and WeChat need it awake anyway.
- **agent-stream starts jobs.** Couples a chat server to an agent runtime and to Kubernetes, and leaves Telegram, WeChat, schedules and the API out.
- **A global job manager for all agent systems.** Worth it when a second system needs sandboxed agent jobs, or several instances must share a scarce resource (one CLI login, one budget) or one dashboard. None applies today; the backend interface (§6) keeps the door open.

## 14. Configuration

| Variable | Default | Meaning |
|---|---|---|
| `AGENT_EXECUTION_BACKEND` | `local` | `local`, `subprocess` or `k8s-job` |
| `AGENT_MAX_CONCURRENT` | 1 | sessions (jobs) in flight per dispatcher |
| `AGENT_JOB_IMAGE` | | runner image (k8s-job) |
| `AGENT_JOB_NAMESPACE` | pod's own | where jobs run |
| `AGENT_JOB_DISPATCHER_URL` | | how jobs reach the dispatcher; also `RUNNER_URL` in prompts |
| `AGENT_JOB_SECRET` | | Secret whose keys become the job's env |
| `AGENT_JOB_SHARED_VOLUME` | | PVC for task workspaces and the repo cache; unset = fresh clones, one-turn tasks only (§16) |
| `AGENT_JOB_CPU`, `AGENT_JOB_MEMORY` | `1`, `2Gi` | job requests (limits = 2× requests) |
| `AGENT_JOB_STOP_GRACE` | `60s` | stop → job deletion |
| `AGENT_JOB_HEARTBEAT` | `2m` | silence before checking the job |
| `AGENT_JOB_REPORT_RETRY` | `15m` | how long a runner retries reaching the dispatcher |
| `AGENT_JOB_MAX_OUTPUT_BYTES` | `50MiB` | cap on `_send/` uploaded in a result |

## 15. Plan

1. **Split the engine (no behavior change).** Separate `ExecuteAgentWithContext` into the run (prompt → workspace → planner → loop → reviewer → git sync → outputs) and finalization (audit log, daily log, curation, memory push, notify, webhook, cleanup), with a `Reporter` between them. `local` = both in process. Existing tests and e2e must pass unchanged.
2. **Wire protocol and `run-session`.** `SessionWire`, `/internal/sessions/*`, the control long poll, tokens, and the `subprocess` backend. E2E: the existing mock-CLI suites run again with `AGENT_EXECUTION_BACKEND=subprocess`, plus stop, steer, a killed runner, and a dispatcher restart mid-session.
3. **Cross-pod state.** `flock` repo-cache lock, memory clone in the runner and single-writer finalization in the dispatcher, journal handles and re-attach (§10).
4. **`k8s-job` backend.** Job template, `Alive` from Job/Pod status, RBAC; tested against a fake clientset, then on a kind cluster in CI.
5. **Images and deploy.** A slim dispatcher image (no Node, no CLIs) and the runner image; manifests (Deployment, Service, ServiceAccount/Role, PVC, NetworkPolicy, ResourceQuota); move the bot onto it.

Steps 1–2 are useful without Kubernetes (a crashed CLI no longer takes the bots down with `subprocess`).

## 16. Open questions

- **Where it runs.** The production agent-runner behind the agent-stream bots isn't in the weilabs or xchangeai clusters today. The weilabs cluster (Flux, `reg.memochat.ai`, PikoCI builds) is the natural home.
- **Shared volume.** Does the cluster have a `ReadWriteMany` storage class? If not: RWO with node affinity, or tasks keep their workspace only on hosts using `local`/`subprocess`.
- **CLI logins.** A subscription login (`~/.claude`) shared by concurrent pods: does the provider allow concurrent use, and how is a refreshed token written back? An API key avoids both and fits jobs better.
- **Pod start latency.** Image size decides cold start; pre-pulling (a DaemonSet or a warm node) may be needed for the heavy image.
- **Persistent backends (pi).** Steering latency is bounded by the control long poll; acceptable, or does steer need a push channel?
