# Agent Runner

A Go HTTP server that wraps Claude Code CLI for autonomous, iterative task execution against Git repositories. Supports agent mode with planning/review phases, conversational interfaces via Telegram and agent-stream, and a simple REST API for one-shot jobs.

## Prerequisites

- Go 1.25+
- [Claude Code CLI](https://docs.anthropic.com/en/docs/claude-code) installed and available on `$PATH`
- Git configured with credentials for your remote

## Quick Start

```bash
# Build
go build -o agent-runner ./cmd/server

# Configure
cp .env.example .env
# Edit .env with your settings

# Run
./agent-runner
```

## Configuration

All configuration is via environment variables (or `.env` file). See `.env.example` for the full list.

### Core Settings

| Variable | Default | Description |
|----------|---------|-------------|
| `GIT_HOST` | | Git server hostname |
| `GIT_ORG` | | Git organization |
| `BIND` | `127.0.0.1:8080` | API listen address |
| `API_KEY` | | API key for authentication (optional) |

### Agent Mode

Agent mode runs Claude iteratively in a workspace with `repos/` containing shared and task-specific repositories.

| Variable | Default | Description |
|----------|---------|-------------|
| `AGENT_SYSTEM_PROMPT` | | Path to base agent prompt (`agent.md`) |
| `AGENT_PROMPT_FILE` | | Path to workflow prompt template (`prompt.md`) |
| `AGENT_SHARED_REPOS` | | Comma-separated repos to pre-populate in every workspace |
| `AGENT_MAX_ITERATIONS` | `50` | Max iterations per session |
| `AGENT_MAX_TOTAL_SECONDS` | `3600` | Total time limit per session |
| `AGENT_MAX_ITERATION_SECONDS` | `300` | Time limit per iteration |
| `AGENT_PLANNER_ENABLED` | `false` | Run planner sub-agent before iteration loop |
| `AGENT_REVIEWER_ENABLED` | `false` | Run reviewer sub-agent after iteration loop |
| `AGENT_MODEL` | | Model name (e.g., `qwen3-coder:30b` for Ollama) |
| `AGENT_MAX_TURNS` | `50` | Max agentic turns per CLI invocation |

### Two-Layer Prompt System

Agent mode supports two prompt layers that are combined at runtime:

1. **Base prompt** (`AGENT_SYSTEM_PROMPT`) — General agent conventions: iterative loop behavior, TODO tracking, `_send/` file sending. Shared across all workflows.
2. **Workflow prompt** (`AGENT_PROMPT_FILE`) — Task-specific instructions with `{{MESSAGE}}` and `{{REPOS}}` template variables.

### Output Files (`_send/` Convention)

Agents can send files back to the user by writing them to a `_send/` directory in the workspace. Files are collected after the iteration loop and delivered via the bot interface. Limits: 20 files max, 10MB total.

### Bot Interfaces (Optional)

**Stream bot** — connects to an [agent-stream](https://github.com/tendant/agent-stream) server for web-based conversational access:

| Variable | Description |
|----------|-------------|
| `STREAM_SERVER_URL` | agent-stream server URL |
| `STREAM_BOT_TOKEN` | Pre-registered bot JWT |
| `STREAM_CONVERSATION_IDS` | Comma-separated conversation IDs to listen on |

**Telegram bot** — conversational access via Telegram:

| Variable | Description |
|----------|-------------|
| `TELEGRAM_BOT_TOKEN` | Telegram bot token |
| `TELEGRAM_CHAT_ID` | Restrict to a specific chat ID |

Both bots support a conversational flow: gathering requirements, optional plan confirmation, then agent execution with progress reporting.

## API Endpoints

### One-Shot Jobs

```bash
# Submit a job
curl -X POST http://127.0.0.1:8080/run \
  -H "Content-Type: application/json" \
  -d '{"project": "my-site", "instruction": "Add a contact page", "paths": ["pages/"]}'

# Poll for results
curl http://127.0.0.1:8080/job/{job_id}
```

### Agent Sessions

```bash
# Start an agent session
curl -X POST http://127.0.0.1:8080/agent \
  -H "Content-Type: application/json" \
  -d '{"message": "Build a landing page for a bakery"}'

# Poll agent session
curl http://127.0.0.1:8080/agent/{session_id}

# Stop a running session
curl -X POST http://127.0.0.1:8080/agent/{session_id}/stop
```

### Other

```bash
# List projects
curl http://127.0.0.1:8080/projects

# Check project lock status
curl http://127.0.0.1:8080/status/{project}
```

## Running Tests

```bash
go test -race ./...
```

## Project Structure

```
├── cmd/server/          # Entry point
├── internal/
│   ├── api/             # HTTP server, routing, handlers, agent executor
│   ├── agent/           # Agent session types and state management
│   ├── config/          # Configuration loading (env vars)
│   ├── conversation/    # Conversational state machine (gathering/confirming/executing)
│   ├── executor/        # Claude Code CLI execution
│   ├── git/             # Git operations (fetch, commit, push)
│   ├── jobs/            # Job state and project locking
│   ├── logging/         # Markdown audit log writer
│   ├── stream/          # agent-stream bot (SSE client + file upload)
│   ├── subagent/        # Planner, reviewer, and prompt builder
│   └── telegram/        # Telegram bot
├── e2e/                 # End-to-end tests with mock Claude scripts
├── repos/               # Persistent repo cache (created at runtime)
├── logs/                # Markdown audit logs (created at runtime)
└── .env.example         # Configuration template
```
