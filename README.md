# Agent Runner

An autonomous AI agent that executes tasks iteratively against Git repositories. Supports [Claude Code](https://docs.anthropic.com/en/docs/claude-code), [Codex](https://github.com/openai/codex), and other compatible CLI agents — with planning, review phases, conversational interfaces (Telegram, web), and a REST API.

## Prerequisites

- Go 1.25+
- At least one supported agent CLI installed and on `$PATH`:
  - [Claude Code](https://docs.anthropic.com/en/docs/claude-code) (default)
  - [Codex](https://github.com/openai/codex) (set `AGENT_CLI=codex`)
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
| `AGENT_CLI` | `claude` | Agent CLI backend (`claude` or `codex`) |
| `AGENT_SYSTEM_PROMPT` | | Path to base agent prompt |
| `AGENT_PROMPT_FILE` | | Path to workflow prompt template |
| `AGENT_SHARED_REPOS` | | Comma-separated repos pre-populated in every workspace |
| `AGENT_PLANNER_ENABLED` | `false` | Run planner sub-agent before iteration loop |
| `AGENT_REVIEWER_ENABLED` | `false` | Run reviewer sub-agent after iteration loop |
| `TELEGRAM_BOT_TOKEN` | | Telegram bot token |
| `STREAM_SERVER_URL` | | Agent Stream server URL |

## Connecting to Agent Stream

[Agent Stream](https://apps.apple.com/us/app/agent-stream/id6759258538) is an iOS app for conversational access to your agent. Once you have access to an agent-stream server, connect agent-runner to it:

```bash
STREAM_SERVER_URL=https://your-agent-stream-server
STREAM_BOT_TOKEN=your-bot-jwt
STREAM_CONVERSATION_IDS=conv_id1,conv_id2
```

The app lets you send messages, receive streaming responses, and get file attachments back from the agent.

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
