# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

**Agent Runner** — a Go HTTP server that wraps coding-agent CLIs (opencode, Claude Code, Codex) for autonomous, iterative task execution against Git repositories. Supports planning/review sub-agents, a persistent file-based memory system with post-session curation, conversational interfaces (Telegram, Agent Stream, WeChat), scheduled tasks, and a REST API.

## Architecture

### Packages (internal/)

| Package | Responsibility |
|---------|---------------|
| `agent/` | Session types, state management, serialized dispatch queue, thread-safe snapshots |
| `api/` | HTTP server, routing, handlers, chat command dispatch, agent executor loop |
| `botcommon/` | Shared bot helpers (status formatting, confirmation parsing, poll-and-report) |
| `config/` | Configuration loading from env files + environment (see Load priority below) |
| `conversation/` | Conversational state machine and intent analyzer (ask/plan/execute) |
| `curator/` | Post-session memory curation via a cheap LLM call (allowlisted writes only) |
| `executor/` | Agent CLI invocation (opencode/claude/codex), workspace management, validation |
| `git/` | Git operations (fetch, commit, push with retries) |
| `jobs/` | One-shot job state, project locking (shared between jobs and agents) |
| `llm/` | Minimal LLM client (anthropic/openai/deepseek + executor-CLI fallback) |
| `logging/` | Markdown audit log writer |
| `metrics/` | Prometheus metrics |
| `runner/` | DB-backed workflow scheduler (cron/delayed tasks via simple-workflow) |
| `stream/` | Agent Stream bot (SSE client, file upload/download) |
| `subagent/` | Planner and reviewer sub-agents, per-iteration prompt builder |
| `telegram/` | Telegram bot |
| `template/` | Memory system: prompt composition, retrieval, budget, daily logs, git sync |
| `wechat/` | WeChat bot (iLink API) |

### State & Persistence

- **In-memory (lost on restart):** active/queued agent sessions, one-shot jobs, conversation state.
- **Survives restart:** scheduled tasks (`SCHEDULER_DATABASE_URL` DB), the memory dir (git-synced), `.env.local` (written by `/set`), audit logs (`logs/`), outputs/uploads, bot event cursors (`tmp/`).
- All mutable state roots under `DATA_DIR`.

### Key Patterns
- Sessions are strictly serialized — `agent.Manager.dispatchLoop` runs one at a time; memory writes never race.
- Deep copies (snapshots) returned from `Session.Snapshot()` for thread safety.
- Config load priority: OS env > `DATA_DIR/.env.local` > `.env.<instance>` > `.env`.
- Agent workspace uses `workspace/` subdirectory as the agent's CWD; shared repos cached in `REPO_CACHE_ROOT` between sessions.
- Memory & prompt composition: see DESIGN.md "Memory & Prompt Composition" — `agent.md` + `prompt.md` + well-known memory files + Recent Sessions digest, bounded by `AGENT_MEMORY_CHAR_CAP`.

### Agent Execution Flow
1. Pull memory git remote (optional), resolve prompt from the memory system
2. Prepare workspace (clone/copy shared repos, skills)
3. **Planner** sub-agent (default on) — step-by-step plan
4. **Iteration loop** — run the agent CLI repeatedly with dynamic prompts
5. Collect output files from `_send/`; read `_schedule.json` for self-scheduled follow-ups
6. **Reviewer** sub-agent (optional) — structured completeness review, corrective iterations
7. Post-session: daily memory log → curation (opt-in) → memory git push → cleanup

### API Endpoints
- `POST /run` / `GET /job/{id}` — one-shot jobs
- `POST /agent` / `GET /agent/{id}` / `POST /agent/{id}/stop` — agent sessions
- `POST /bootstrap` — install CLI, seed default prompts, report readiness
- `POST /schedule` / `GET /schedules` / `DELETE /schedule/{id}` — scheduled tasks
- `GET /sessions`, `GET /projects`, `GET /status/{project}`

## Development

### Build and Run
```bash
go build -o agent-runner ./cmd/server
cp .env.example .env  # minimal: DEEPSEEK_API_KEY (opencode default) or AGENT_CLI=claude
./agent-runner
curl -X POST localhost:8080/bootstrap
```

### Testing
```bash
go test -race ./...
```
- E2E tests in `e2e/` use mock CLI bash scripts prepended to PATH
- Always run with `-race`; CI also gates on `gofmt -l` and `go vet`

### Configuration
All via environment variables — `.env.example` is the authoritative reference. Chat commands (`/set`, `/config`, `/status`, `/bootstrap`) configure a running instance without SSH.

See DESIGN.md for the full architecture document.
