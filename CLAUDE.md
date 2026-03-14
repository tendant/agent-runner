# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

**Agent Runner** — a Go HTTP server that wraps Claude Code CLI for autonomous, iterative task execution against Git repositories. Supports agent mode with planning/review phases, conversational interfaces via Telegram and agent-stream, and a REST API for one-shot jobs.

## Architecture

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

### Key Patterns
- All state in-memory; Markdown audit logs in `logs/` for persistence
- Deep copies (snapshots) returned from `Session.Snapshot()` for thread safety
- Background goroutines for async execution; capture fields to locals before spawning
- Project locking shared between jobs and agents via `jobs.Manager`
- Agent workspace uses `repos/` subdirectory within each session; shared repos cached in `workspaces/` (WORKSPACES_ROOT) between sessions
- Template-based prompt system: embedded defaults in `template/defaults/` merged with user overrides in memory dir; `AGENT_SYSTEM_PROMPT` / `AGENT_PROMPT_FILE` are seeded into the memory dir at startup

### Agent Execution Flow
1. Resolve prompt (compose from template system: embedded defaults + memory dir overrides)
2. Prepare workspace (clone/copy shared repos, populate project files)
3. **Phase 1**: Planner sub-agent (optional) — produces step-by-step plan
4. **Phase 2**: Iteration loop — run Claude repeatedly with dynamic prompts
5. Collect output files from `_send/` directory
6. **Phase 3**: Reviewer sub-agent (optional) — evaluates completeness
7. Cache repos back, sync files, write audit log, cleanup workspace

### API Endpoints
- `POST /run` — one-shot job (returns job ID)
- `GET /job/{id}` — poll job status
- `POST /agent` — start agent session
- `GET /agent/{id}` — poll session status
- `POST /agent/{id}/stop` — request graceful stop
- `GET /projects` — list projects
- `GET /status/{project}` — check lock status

## Development

### Build and Run
```bash
go build -o agent-runner ./cmd/server
cp .env.example .env  # configure
./agent-runner
```

### Testing
```bash
go test -race ./...
```

- E2E tests in `e2e/` use mock `claude` bash scripts prepended to PATH
- Mock scripts use external counter files for iteration tracking
- Always run with `-race` flag

### Configuration
All via environment variables. See `.env.example` for the full list. Key vars:
- `AGENT_SYSTEM_PROMPT` / `AGENT_PROMPT_FILE` — prompt files seeded into the template system at startup
- `AGENT_SHARED_REPOS` — repos pre-populated in every workspace
- `STREAM_*` / `TELEGRAM_*` — bot configuration

See DESIGN.md for the full architecture document.
