# Agent Runner — Design Document

## Overview

Agent Runner is a Go HTTP server that wraps Claude Code CLI for autonomous, iterative task execution against Git repositories. It supports two execution modes (one-shot jobs and agent sessions), conversational bot interfaces (Telegram and agent-stream), and optional planning/review sub-agents.

The system is local-first, stateless (in-memory state, Markdown audit logs), and designed for trusted users with direct Git push access.

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
6. **Cleanup** — cache repos back, sync workspace files, write audit log, delete workspace

Session states: `running → stopping → completed/failed`

---

## Template-Based Prompt System

Agent mode composes the system prompt from a template pipeline:

1. **Embedded defaults** (`internal/template/defaults/*.md`) — Well-known templates (IDENTITY, SOUL, AGENTS, USER, TOOLS, BOOT, HEARTBEAT) with assigned priorities and read phases.

2. **User overrides** (memory directory) — Any `.md` file in the memory dir is loaded and merged by filename. Files without frontmatter get `priority: 100` and `read_when: always`.

3. **Legacy prompt files** — `AGENT_SYSTEM_PROMPT` and `AGENT_PROMPT_FILE` are seeded into the memory directory at startup as `system-prompt.md` and `prompt.md` respectively, so they flow through the same pipeline.

Template variables are substituted in all templates:
- `{{MESSAGE}}` — the user's task message
- `{{REPOS}}` — comma-separated list of shared repos
- `{{DATE}}` — current date
- `{{ITERATION}}` — current iteration number

The composed prompt is passed as the system prompt to Claude Code CLI. If no templates produce output, the user message is used directly.

---

## Agent Workspace

Each agent session gets an isolated workspace:

```
workspace-{session-id}/
├── repos/              # Claude's working directory
│   ├── shared-repo/    # Pre-populated from AGENT_SHARED_REPOS
│   ├── another-repo/   # Pre-populated from AGENT_SHARED_REPOS
│   └── _send/          # Output files for user delivery
├── TODO.md             # Agent's progress tracker (synced back to project dir)
└── ...                 # Other project files (synced from ProjectDir)
```

- **Shared repos** (`AGENT_SHARED_REPOS`) are cached in `repos/` and copied into each workspace
- Claude runs in the `repos/` subdirectory
- After completion, repos are cached back for future sessions
- Non-repo files are synced back to the project directory

### Output Files (`_send/` Convention)

Agents can send files to the user by writing them to `repos/_send/`. After the iteration loop completes (before cleanup), the executor:

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

Runs before the iteration loop. Analyzes workspace state (file tree, git log, TODO.md) and produces a structured plan with steps. The plan is injected into iteration prompts.

### Prompt Builder (Phase 2)

Builds dynamic per-iteration prompts incorporating:
- Base preamble (combined system + workflow prompt)
- Current plan and step progress
- Workspace state (file tree, recent commits)
- Iteration number and user message

### Reviewer (Phase 3)

Runs after the iteration loop. Evaluates the agent's work against the original request, producing a score and completeness assessment.

---

## Project Locking

Per-project mutex prevents concurrent Git operations. Shared between jobs and agent sessions via `jobs.Manager.AcquireProjectLock/ReleaseProjectLock`.

- Lock acquired at session/job start
- Released on completion, failure, or timeout
- Concurrent requests return `409 Conflict`

---

## State Management

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
- **Agent**: `AGENT_SYSTEM_PROMPT`, `AGENT_PROMPT_FILE` (seeded into template system at startup), `AGENT_SHARED_REPOS`, iteration/time limits, planner/reviewer toggles
- **API**: `BIND`, `API_KEY`
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
├── repos/               # Persistent repo cache (runtime)
├── logs/                # Markdown audit logs (runtime)
└── .env.example         # Configuration template
```
