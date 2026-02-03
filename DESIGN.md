# Claude Code v1 — Local API Wrapper Design

## Overview

This document describes **v1** of a minimal, local-first wrapper around Claude Code, exposed via a simple HTTP API.

The goal of v1 is **speed, clarity, and safety**, not scale or multi-tenancy. The system is designed for **trusted users**, **direct Git pushes**, and **no persistent database**. State is kept in memory, with optional Markdown files used as human-readable run logs.

This design intentionally avoids queues, workers, PR workflows, OAuth, or complex sandboxing. All of those can be layered on later without breaking the core model.

---

## Design Principles

- Local-first: projects already exist on disk
- Stateless API: no database
- In-memory execution state only
- One request = one execution
- Direct push to Git (Gitea)
- Explicit guardrails instead of heavy isolation
- Easy to debug (files + logs)

---

## High-Level Flow

```
POST /run
    ↓
Return job ID immediately
    ↓
Background: Claude Code (CLI)
    ↓
Modify repo workspace
    ↓
Validate diff against allowlist
    ↓
Git commit + push
    ↓
Poll GET /job/{id} for result
```

Execution is **asynchronous**. The API returns a job ID immediately; clients poll for completion. Requests are serialized per project using in-memory locks.

---

## Directory Layout

```
/claude-wrapper/
  ├── api/
  │   └── server.go (or app.py)
  ├── projects/
  │   ├── site-a/
  │   ├── site-b/
  │   └── site-c/
  ├── runs/
  │   └── 2026-02-02_21-04-33_site-a.md
  ├── tmp/
  │   └── job-<uuid>/
  └── config.yaml
```

### Directory Semantics

| Directory | Purpose | Lifecycle |
|-----------|---------|-----------|
| `projects/` | Long-lived Git working copies | Persistent |
| `tmp/` | Ephemeral per-request workspaces | Cleaned after each job |
| `runs/` | Markdown execution logs (audit trail) | Persistent |

---

## API Surface (v1)

### POST /run

Initiates a Claude Code execution against a project. Returns immediately with a job ID.

#### Request Body

```json
{
  "project": "site-a",
  "instruction": "Add a contact page and link it from the homepage",
  "paths": ["content/", "pages/"],
  "commit_message": "Add contact page",
  "author": "claude-bot"
}
```

#### Field Reference

| Field | Required | Description |
|-------|----------|-------------|
| `project` | Yes | Must exist under `projects/` and be in `allowed_projects` |
| `instruction` | Yes | Natural language task for Claude Code |
| `paths` | Yes | Allowlist of editable directories/files |
| `commit_message` | No | If omitted, auto-generated from instruction + diff summary |
| `author` | No | Git author name (default: `claude-bot`) |

#### Response (202 Accepted)

```json
{
  "job_id": "a1b2c3d4-5678-90ab-cdef-1234567890ab",
  "status": "queued",
  "project": "site-a"
}
```

#### Error Responses

| Status | Condition |
|--------|-----------|
| 400 | Invalid request body or unknown project |
| 409 | Project is currently locked by another job |
| 503 | System at capacity |

---

### GET /job/{job_id}

Poll for job status and results.

#### Response (Running)

```json
{
  "job_id": "a1b2c3d4-...",
  "status": "running",
  "project": "site-a",
  "started_at": "2026-02-02T21:04:33Z",
  "elapsed_seconds": 45
}
```

#### Response (Completed)

```json
{
  "job_id": "a1b2c3d4-...",
  "status": "completed",
  "project": "site-a",
  "commit": "a3f9c12",
  "changed_files": [
    "pages/contact.md",
    "pages/index.md"
  ],
  "diff_summary": {
    "insertions": 120,
    "deletions": 0
  },
  "log_file": "runs/2026-02-02_21-04-33_site-a.md",
  "duration_seconds": 87
}
```

#### Response (Failed)

```json
{
  "job_id": "a1b2c3d4-...",
  "status": "failed",
  "project": "site-a",
  "error": "Push failed: remote contains commits not present locally",
  "error_code": "GIT_PUSH_CONFLICT",
  "log_file": "runs/2026-02-02_21-04-33_site-a.md"
}
```

#### Status Values

| Status | Description |
|--------|-------------|
| `queued` | Waiting for project lock |
| `running` | Claude Code executing |
| `validating` | Checking diff against policy |
| `pushing` | Git commit and push in progress |
| `completed` | Success |
| `failed` | Error occurred (see `error_code`) |

---

### GET /status/{project}

Check if a project is available or locked.

#### Response

```json
{
  "project": "site-a",
  "locked": true,
  "current_job_id": "a1b2c3d4-...",
  "locked_since": "2026-02-02T21:04:33Z"
}
```

---

### GET /projects

List available projects.

#### Response

```json
{
  "projects": [
    { "name": "site-a", "locked": false },
    { "name": "site-b", "locked": true, "current_job_id": "..." }
  ]
}
```

---

## Execution Model

### 1. Project Locking

Each project has an in-memory mutex to prevent concurrent Git operations:

```
site-a: locked (job a1b2c3d4)
site-b: free
site-c: free
```

Behavior:
- Lock acquired at job start
- Released on completion, failure, or timeout
- If locked, `/run` returns `409 Conflict`

---

### 2. Workspace Preparation

```bash
# Ensure clean state
cd projects/site-a
git fetch origin
git reset --hard origin/main
git clean -fdx

# Copy to isolated workspace
cp -R projects/site-a tmp/job-uuid
cd tmp/job-uuid
```

Rationale:
- Avoid corrupting long-lived working copies
- Start from known-good state (matches remote)
- Enable clean diffs and validation
- Easy cleanup on failure

---

### 3. Claude Code Execution

Claude Code is invoked in non-interactive mode inside the temp workspace.

```bash
claude --print \
  --dangerously-skip-permissions \
  --output-format json \
  "Add a contact page and link it from the homepage" \
  2>&1 | tee execution.log
```

#### CLI Flags Reference

| Flag | Purpose |
|------|---------|
| `--print` | Non-interactive mode, outputs result and exits |
| `--dangerously-skip-permissions` | Skip interactive permission prompts |
| `--output-format json` | Structured output for parsing |
| `--allowedTools` | (Optional) Restrict to file editing tools only |

**Note:** Path allowlisting is enforced via post-execution diff validation, not CLI flags.

---

### 4. Diff Validation

Before committing, validate the diff against policy:

```bash
git diff --name-only
```

#### Validation Rules

| Check | Action on Violation |
|-------|---------------------|
| Files outside `paths` allowlist | Abort, error `PATH_VIOLATION` |
| `.git/` directory modified | Abort, error `GIT_DIR_VIOLATION` |
| CI configs (`.github/`, `.gitlab-ci.yml`) | Abort, error `CI_CONFIG_VIOLATION` |
| Secrets patterns detected | Abort, error `SECRETS_VIOLATION` |
| Git hooks modified | Abort, error `HOOKS_VIOLATION` |
| Binary files added | Warn (configurable to abort) |

Violations abort the run and record details in the log file.

---

### 5. Commit and Push

```bash
# Generate commit message if not provided
COMMIT_MSG="${provided_message:-$(generate_commit_message)}"

git add .
git commit -m "$COMMIT_MSG" \
  --author="claude-bot <bot@local>" \
  --trailer "Instruction: ${instruction}"
git push origin HEAD
```

#### Commit Message Generation

If `commit_message` is omitted, auto-generate from:
1. First line: summarize changed files
2. Body: include the original instruction for traceability

Example:
```
Add contact page (pages/contact.md, pages/index.md)

Instruction: Add a contact page and link it from the homepage
```

#### Push Failure Handling

| Failure | Recovery |
|---------|----------|
| Network error | Retry up to 3 times with backoff |
| Remote has new commits | Abort with `GIT_PUSH_CONFLICT` (no auto-rebase in v1) |
| Auth failure | Abort with `GIT_AUTH_FAILURE` |

On push failure:
- Workspace is preserved for debugging
- Error details logged
- Lock is released
- Caller can retry after resolving

---

### 6. Cleanup

```bash
rm -rf tmp/job-uuid
```

Cleanup occurs:
- On successful completion
- On validation failure
- On push failure (after logging)
- On timeout

**Crash recovery:** A startup routine scans `tmp/` and removes stale job directories older than `max_runtime_seconds`.

---

### 7. Run Log (Markdown)

Each execution writes a Markdown log for audit purposes:

```markdown
# Claude Run — 2026-02-02 21:04:33

**Job ID:** a1b2c3d4-5678-90ab-cdef-1234567890ab  
**Project:** site-a  
**Status:** completed  
**Duration:** 87s

## Instruction

> Add a contact page and link it from the homepage

## Changed Files

- `pages/contact.md` (+95)
- `pages/index.md` (+25)

## Diff Summary

- Insertions: 120
- Deletions: 0

## Commit

`a3f9c12` pushed to `origin/main`

## Validation

✓ All changes within allowed paths  
✓ No CI config modifications  
✓ No secrets detected

## Execution Log

<details>
<summary>Claude Code output</summary>

```
[Claude Code execution output here]
```

</details>
```

---

## In-Memory State

```go
type Job struct {
    ID        string
    Project   string
    Status    string    // queued, running, validating, pushing, completed, failed
    StartedAt time.Time
    Error     string
    ErrorCode string
    Result    *JobResult
}

type JobResult struct {
    Commit       string
    ChangedFiles []string
    DiffSummary  DiffSummary
    LogFile      string
    Duration     time.Duration
}

var (
    jobs     = make(map[string]*Job)       // job_id -> Job
    locks    = make(map[string]string)     // project -> job_id
    jobsMu   sync.RWMutex
)
```

- State lost on process restart (acceptable for v1)
- Job records kept in memory for 1 hour after completion (configurable)

---

## Configuration (config.yaml)

```yaml
# Directory paths
projects_root: ./projects
runs_root: ./runs
tmp_root: ./tmp

# Project allowlist
allowed_projects:
  - site-a
  - site-b

# Execution limits
max_runtime_seconds: 300
max_concurrent_jobs: 5

# Git settings
git_push_retries: 3
git_push_retry_delay_seconds: 5

# Validation settings
validation:
  block_binary_files: false
  blocked_paths:
    - ".git/"
    - ".github/"
    - ".gitlab-ci.yml"
    - "secrets/"
    - "*.env"

# API settings
api:
  bind: "127.0.0.1:8080"
  api_key: ""  # Optional; if set, required in X-API-Key header

# Cleanup
job_retention_seconds: 3600
startup_cleanup_stale_jobs: true
```

---

## Security Model (v1)

| Layer | Mechanism |
|-------|-----------|
| Network | API bound to localhost or private network |
| Authentication | Optional static API key (`X-API-Key` header) |
| Authorization | Trusted users only; no user impersonation |
| Isolation | Temp workspace per job; diff validation |
| Git | Credentials managed externally (SSH keys or credential helper) |

**Threat model:** This design assumes trusted callers. It protects against accidental misconfiguration (path allowlist), not malicious actors with API access.

---

## Error Codes Reference

| Code | Description |
|------|-------------|
| `PATH_VIOLATION` | Changes outside allowed paths |
| `GIT_DIR_VIOLATION` | Attempted to modify `.git/` |
| `CI_CONFIG_VIOLATION` | Attempted to modify CI configuration |
| `SECRETS_VIOLATION` | Secrets patterns detected in changes |
| `HOOKS_VIOLATION` | Attempted to modify Git hooks |
| `GIT_PUSH_CONFLICT` | Remote has commits not present locally |
| `GIT_AUTH_FAILURE` | Git authentication failed |
| `GIT_NETWORK_ERROR` | Network error during Git operations |
| `TIMEOUT` | Execution exceeded `max_runtime_seconds` |
| `CLAUDE_ERROR` | Claude Code returned an error |

---

## Explicit Non-Goals (v1)

The following are intentionally excluded:

| Feature | Rationale |
|---------|-----------|
| Database / persistent job storage | In-memory + log files sufficient |
| Job queues / background workers | Per-project locks sufficient |
| Pull request workflows | Direct push is simpler for v1 |
| Web UI | API-first; UI can be added later |
| Multi-tenant isolation | Trusted users only |
| Streaming logs | Poll-based approach sufficient |
| Artifact storage | Git is the artifact store |
| Auto-rebase on conflict | Too risky for v1; fail-fast instead |

---

## Upgrade Path (Future Versions)

This design supports clean evolution:

| Version | Feature |
|---------|---------|
| v1.1 | Plan-only mode (diff preview without commit) |
| v1.2 | PR-based workflow option |
| v2 | Persistent job storage (SQLite or Postgres) |
| v2 | Replace in-memory locks with proper queue |
| v2 | Streaming logs via SSE |
| v3 | Container-based sandboxing |
| v3 | Web UI |
| v3 | Git App authentication (installation tokens) |

No breaking changes required to reach these stages.

---

## Summary

Claude Code v1 is a **controlled local automation tool**, exposed via HTTP, that behaves like a safe, scriptable pair-programmer with Git write access.

**Key properties:**

- Async job execution with polling
- Per-project locking prevents conflicts
- Workspace isolation protects working copies
- Diff validation enforces path allowlists
- Markdown logs provide zero-cost audit trail
- Explicit error codes for programmatic handling

This makes it ideal for early-stage internal usage and rapid iteration.

