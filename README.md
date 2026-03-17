# Agent Runner

A Go HTTP server that wraps Claude Code CLI for autonomous, iterative task execution against Git repositories.

## Prerequisites

- Go 1.25+
- [Claude Code CLI](https://docs.anthropic.com/en/docs/claude-code) installed and on `$PATH`
- Git configured with credentials for your remote

## Quick Start

```bash
go build -o agent-runner ./cmd/server
cp .env.example .env   # edit with your settings
./agent-runner
```

## Configuration

All configuration is via environment variables (or `.env` file). See `.env.example` for the full list.

Key variables:

| Variable | Default | Description |
|----------|---------|-------------|
| `BIND` | `127.0.0.1:8080` | API listen address |
| `API_KEY` | | Authentication key (optional) |
| `AGENT_SYSTEM_PROMPT` | | Path to base agent prompt |
| `AGENT_PROMPT_FILE` | | Path to workflow prompt template |
| `AGENT_SHARED_REPOS` | | Comma-separated repos pre-populated in every workspace |
| `AGENT_PLANNER_ENABLED` | `false` | Run planner sub-agent before iteration loop |
| `AGENT_REVIEWER_ENABLED` | `false` | Run reviewer sub-agent after iteration loop |
| `TELEGRAM_BOT_TOKEN` | | Telegram bot token |
| `STREAM_SERVER_URL` | | [agent-stream](https://github.com/tendant/agent-stream) server URL |

## API

```bash
# Start an agent session
curl -X POST http://localhost:8080/agent \
  -H "Content-Type: application/json" \
  -d '{"message": "Build a landing page for a bakery"}'

# Poll status
curl http://localhost:8080/agent/{session_id}

# Stop
curl -X POST http://localhost:8080/agent/{session_id}/stop
```

One-shot jobs: `POST /run` → poll `GET /job/{id}`

## Testing

```bash
go test -race ./...
```
