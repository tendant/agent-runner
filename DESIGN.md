# Agent Runner — Design Document

## Overview

Agent Runner is a Go HTTP server that wraps Claude Code CLI for autonomous, iterative task execution against Git repositories. It supports two execution modes (one-shot jobs and agent sessions), conversational bot interfaces (Telegram and agent-stream), and optional planning/review sub-agents.

The system is local-first and mostly in-memory, with durable edges: Markdown audit logs, a session journal for restart recovery (queued sessions are re-enqueued, interrupted ones failed with a chat notification), persisted conversations, and the git-synced memory dir. Designed for trusted users with direct Git push access.

---

## Architecture

```
┌─────────────────────────────────────────────────────┐
│                   HTTP API (api/)                    │
│         POST /run  POST /agent  GET /job/...        │
├─────────────────────────────────────────────────────┤
│                                                     │
│  ┌──────────┐  ┌──────────────┐  ┌──────────────┐  │
│  │ Jobs     │  │ Agent        │  │ Conversation  │  │
│  │ Manager  │  │ Manager      │  │ Manager       │  │
│  └────┬─────┘  └──────┬───────┘  └──────┬───────┘  │
│       │               │                 │           │
│       │        ┌──────┴───────┐         │           │
│       │        │ Agent        │         │           │
│       │        │ Executor     │         │           │
│       │        │ (phases 1-3) │         │           │
│       │        └──────┬───────┘         │           │
│       │               │                 │           │
│  ┌────┴───────────────┴─────────────────┴───────┐   │
│  │          Claude Code CLI (executor/)          │   │
│  └──────────────────────────────────────────────┘   │
│                                                     │
│  ┌──────────────┐  ┌──────────────┐                 │
│  │ Stream Bot   │  │ Telegram Bot │                 │
│  │ (SSE + HTTP) │  │ (long-poll)  │                 │
│  └──────────────┘  └──────────────┘                 │
└─────────────────────────────────────────────────────┘
```

### Packages

| Package | Responsibility |
|---------|---------------|
| `api/` | HTTP server, routing, handlers, agent executor loop |
| `agent/` | Session types, state management, thread-safe snapshots |
| `config/` | Configuration loading from environment variables |
| `conversation/` | Conversational state machine (gathering → confirming → executing) |
| `executor/` | Claude Code CLI invocation and output parsing |
| `git/` | Git operations (fetch, commit, push) |
| `jobs/` | Job state, project locking (shared between jobs and agents) |
| `logging/` | Markdown audit log writer |
| `stream/` | agent-stream bot (SSE client, file upload/download) |
| `subagent/` | Planner, reviewer, and dynamic prompt builder |
| `telegram/` | Telegram bot |

---

## Execution Modes

### One-Shot Jobs (`POST /run`)

Single Claude Code invocation against a project with diff validation and Git push.

Flow: `queued → running → validating → pushing → completed/failed`

### Agent Sessions (`POST /agent`)

Iterative execution with multiple Claude invocations in a workspace. Claude handles all Git operations via the prompt.

Flow:
1. **Workspace preparation** — create isolated workspace, populate shared repos
2. **Phase 1: Planner** (optional) — sub-agent analyzes workspace state and produces a step-by-step plan
3. **Phase 2: Iteration loop** — run Claude repeatedly with dynamic prompts incorporating plan and workspace state
4. **Output file collection** — scan `_send/` directory for files to deliver to user
5. **Phase 3: Reviewer** (optional) — sub-agent evaluates completeness and quality
6. **Cleanup** — cache workspaces back, sync workspace files, write audit log, delete workspace

Session states: `queued → running → stopping → completed/failed/stopped` (`stopped` is distinct from `failed` — set when the session ends via a user-requested `/agent/{id}/stop` rather than an error)

---

## Memory & Prompt Composition

Every session's prompt is composed from markdown files in the memory dir
(`MEMORY_DIR`, default `DATA_DIR/memory`), in this order (`resolvePrompt`,
internal/api/agent_executor.go; `Compile`, internal/template/compiler.go):

1. **`agent.md`** — system instructions (identity, rules). Path overridable via `AGENT_SYSTEM_PROMPT`.
2. **`prompt.md`** — workflow overlay, appended when present. Overridable via `AGENT_PROMPT_FILE`.
3. **Well-known memory files**, each as a `## <Name>` section, in fixed order:
   `user_preferences.md`, `project_summary.md`, `decisions.md`, `workflows.md`, `lessons.md`.
4. **Other lowercase `.md` files** in the memory dir (excluding the above, `HEARTBEAT.md`, and date-prefixed daily logs).
5. **`## Recent Sessions`** — the last `AGENT_MEMORY_DAYS` days of daily logs, newest first.
6. **`## Current Request`** — the task message.

Template variables (`{{MESSAGE}}`, `{{DATE}}`, `{{REPOS}}`, `{{RUNNER_URL}}`, `{{API_KEY}}`, `{{PROJECT_DIR}}`, `{{MEMORY_DIR}}`) are substituted in all sections except Recent Sessions (log text may contain literal braces).

### Memory budget

`AGENT_MEMORY_CHAR_CAP` (default 12000, 0 = unlimited) bounds the memory content injected per prompt — files on disk are never modified. Recent Sessions gets up to a quarter of the cap (whole days dropped oldest-first); the remainder goes to memory files via `ApplyBudget` (internal/template/budget.go): oversized misc files are truncated to a third of the cap with a marker pointing at the full file, then misc files are dropped last-loaded-first, then well-known files are truncated in reverse priority (user_preferences survives longest). Every trim logs a `memory budget` warning.

### Learning loop

After every session (in order, inside the executor's completion defer):

1. **Daily log append** — status, iterations, cost, task preview, reviewer score/issues, changed files, error → `memory/YYYY-MM-DD-<host>.md`. Append-only audit trail; folded back into prompts via Recent Sessions.
2. **Curation** (opt-in: `AGENT_MEMORY_CURATION_ENABLED`, default false) — a cheap LLM call (uses the fast-LLM slot (`FAST_LLM_*`)) distills the session outcome into at most 2 durable lessons appended to `lessons.md`, and compacts allowlisted memory files that exceed their per-file budget. Safety rails (internal/curator): may only write `lessons.md` and rewrite the well-known files; rewrites only when over budget and only if strictly smaller; never touches `agent.md`, `prompt.md`, or daily logs; all failures are non-fatal (`AGENT_MEMORY_CURATION_TIMEOUT_SECONDS`, default 60, bounds the call).
3. **Git push** — `CommitAndPushMemory` (3 retries) persists everything above in one commit; git history is the undo mechanism for curation.

The agent also self-edits memory directly during sessions — the default `agent.md` instructs it to write preferences/decisions/summaries to the well-known files and workflow changes to `prompt.md`.

Memory writes made by the runner (daily log, curation, pull/commit/push) are serialized by a package-level mutex in `internal/template`, and `WriteMemoryFile` writes via temp+rename so a reader never sees a truncated file. The mutex covers this process only: the agent CLI edits memory files from a subprocess, which no in-process lock can cover. With `AGENT_MAX_CONCURRENT > 1` two agents can therefore still overwrite each other's in-session memory edits; the git-backed memory history is the undo mechanism.

---

## Agent Workspace

Each agent session gets an isolated workspace:

```
session-{id}/
├── workspace/              # Agent's CWD (cmd.Dir points here)
│   ├── shared-repo-a/      # Pre-populated from AGENT_SHARED_REPOS
│   ├── shared-repo-b/      # Pre-populated from AGENT_SHARED_REPOS
│   ├── _send/              # Agent writes output files here
│   └── _progress.json      # Agent writes plan step completion
└── state/                  # Runner-managed (not visible to agent)
    └── TODO.md             # Progress tracker, injected into prompts
```

- **Shared repos** (`AGENT_SHARED_REPOS`) are cached in `repo-cache/` (configured via `REPO_CACHE_ROOT`) and copied into each workspace's `workspace/` directory
- Agent opens its eyes in `workspace/` — no `cd` needed; `_send/` and `_progress.json` are relative to CWD
- `state/` is the runner's bookkeeping, invisible to the agent
- After completion, repos are cached back from `workspace/` for future sessions (underscore-prefixed entries skipped)

### Output Files (`_send/` Convention)

Agents can send files to the user by writing them to `_send/` in their working directory. After the iteration loop completes (before cleanup), the executor:

1. Scans `_send/` for files (skips subdirectories)
2. Reads each file with content type detection
3. Stores files on the session as `OutputFiles` (limits: 20 files, 10MB total)
4. Bot interfaces upload and deliver the files

---

## Bot Interfaces

Both bots provide conversational access to agent sessions through a shared state machine.

### Conversation Flow

```
User message → Analyzer (Claude) → Action decision
                                      ├── ask: request more info
                                      ├── plan: present plan for confirmation
                                      └── execute: start agent session
```

States: `gathering → confirming → executing → completed`

- **Gathering**: collecting requirements, analyzer decides next action
- **Confirming**: plan presented, waiting for yes/no
- **Executing**: agent session running, user messages queued
- **Completed**: session done, conversation reset

### Stream Bot (`stream/`)

Connects to an [agent-stream](https://github.com/tendant/agent-stream) server via SSE for web-based access.

- Listens on configured conversation IDs
- Emits events: `status.thinking`, `assistant.delta`, `assistant.final`
- Uploads output files via `POST /v1/conversations/{id}/files`
- Sends file attachments via `POST /v1/conversations/{id}/messages` with `file_ids`
- Downloads user-attached files (text inlined, binary saved to temp)

### Telegram Bot (`telegram/`)

Long-polling Telegram bot with incremental progress reporting.

- Restricted to a configured chat ID
- Reports each iteration as it completes
- Mentions generated output files in the summary (no file upload)

---

## Sub-Agents (`subagent/`)

### Planner (Phase 1)

Runs before the iteration loop. Analyzes workspace state (file tree, git log, TODO.md, available skills under `.claude/skills/`/`.agents/skills/`) and produces a structured plan with steps. The plan is injected into iteration prompts.

### Prompt Builder (Phase 2)

Builds dynamic per-iteration prompts incorporating:
- Base preamble (combined system + workflow prompt)
- Current plan and step progress
- Workspace state (file tree, recent commits)
- Iteration number and user message

### Reviewer (Phase 3)

Runs after the iteration loop. Evaluates the agent's work against the original request, producing a score and completeness assessment.

---

## Locking

### Project locks (one-shot jobs)

Per-project mutex prevents concurrent Git operations on the same project directory, via `jobs.Manager` (`internal/jobs/manager.go`).

- Lock acquired in `CreateJob`, keyed by the `project` field
- Released when the job reaches a terminal status
- Concurrent requests return `409 Conflict`

This applies to `POST /run` only. Agent sessions have no project field and do not take it — each gets a private workspace instead.

### Advisory locks (agent sessions)

`internal/locks` provides named leases that sessions take out on each other over HTTP: `POST /lock`, `DELETE /lock/{name}`, `GET /locks`.

The runner cannot tell which two tasks conflict — that depends on what the task touches, not on anything the runner knows — so the **agent chooses the name**. The canonical case is new-site creation: the next site ID is derived by listing `base/sites/` in the agent's own copy of the shared config repo, so two concurrent sessions allocate the same ID unless one waits (see `prompt.md`).

- Advisory: nothing stops a session that never asks
- `wait_seconds` blocks for the current holder; `0` makes it a try-lock returning `409`
- `ttl_seconds` bounds how long a name can be wedged (capped at `locks.MaxTTL`)
- Released automatically when the holding session ends, however it ends
- In-memory and process-local; a restart clears them

### Shared-resource locks (internal)

- Memory dir — package mutex in `internal/template`, shared with the `/memory` chat commands
- Repo cache write-back — per-repo-name mutex in `internal/executor`, plus randomised temp paths so a concurrent swap cannot clobber an in-flight copy

---

## State Management

- Agent sessions are dispatched by a worker pool of `AGENT_MAX_CONCURRENT` goroutines (default 1 = serial)
- All state is in-memory (lost on restart)
- `Session` uses `sync.RWMutex` for thread-safe field updates
- `Snapshot()` returns deep copies for safe concurrent reading
- Markdown audit logs in `logs/` provide persistent record of all executions

---

## API Surface

### Jobs

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/run` | POST | Submit a one-shot job |
| `/job/{id}` | GET | Poll job status |

### Agent Sessions

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/agent` | POST | Start an agent session |
| `/agent/{id}` | GET | Poll session status |
| `/agent/{id}/stop` | POST | Request graceful stop |

### Advisory Locks

Called by the running agent, not by operators — see "Locking" above.

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/lock` | POST | Acquire a named lease (`name`, `ttl_seconds`, `wait_seconds`) |
| `/lock/{name}` | DELETE | Release a lease |
| `/locks` | GET | List currently held leases |

### Management

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/projects` | GET | List available projects |
| `/status/{project}` | GET | Check project lock status |

---

## Configuration

All configuration via environment variables. See `.env.example` for the full list.

Key groups:
- **Git**: `GIT_HOST`, `GIT_ORG`
- **Agent**: `AGENT_SYSTEM_PROMPT`, `AGENT_PROMPT_FILE` (seeded into template system at startup), `AGENT_SHARED_REPOS`, `AGENT_MAX_CONCURRENT`, iteration/time limits, planner/reviewer toggles
- **API**: `API_BIND`, `API_KEY`
- **Stream bot**: `STREAM_SERVER_URL`, `STREAM_BOT_TOKEN`, `STREAM_CONVERSATION_IDS`
- **Telegram**: `TELEGRAM_BOT_TOKEN`, `TELEGRAM_CHAT_ID`
- **Ollama**: `ANTHROPIC_BASE_URL`, `ANTHROPIC_AUTH_TOKEN`, `AGENT_MODEL`

---

## Security Model

| Layer | Mechanism |
|-------|-----------|
| Network | API bound to localhost or private network |
| Authentication | Optional static API key (`X-API-Key` header) |
| Authorization | Trusted users only |
| Isolation | Per-session workspace; repos cached and cleaned up |
| Git | Credentials managed externally |
| Bots | Telegram restricted by chat ID; stream bot uses JWT auth |

**Threat model:** Assumes trusted callers. Protects against accidental misconfiguration, not malicious actors with API access.

---

## Directory Layout

```
agent-runner/
├── cmd/server/          # Entry point
├── internal/
│   ├── api/             # HTTP server, routing, handlers, agent executor
│   ├── agent/           # Session types and state management
│   ├── config/          # Configuration loading (env vars)
│   ├── conversation/    # Conversational state machine
│   ├── executor/        # Claude Code CLI execution
│   ├── git/             # Git operations
│   ├── jobs/            # Job state and project locking
│   ├── logging/         # Markdown audit log writer
│   ├── stream/          # agent-stream bot
│   ├── subagent/        # Planner, reviewer, prompt builder
│   └── telegram/        # Telegram bot
├── e2e/                 # End-to-end tests with mock Claude scripts
├── repo-cache/          # Persistent repo cache (runtime, gitignored)
├── logs/                # Markdown audit logs (runtime)
└── .env.example         # Configuration template
```
