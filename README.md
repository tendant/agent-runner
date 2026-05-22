# Agent Runner

An autonomous AI agent that executes tasks iteratively against Git repositories. Supports [Claude Code](https://docs.anthropic.com/en/docs/claude-code), [Codex](https://github.com/openai/codex), [opencode](https://github.com/sst/opencode), and other compatible CLI agents — with planning, review phases, conversational interfaces (Telegram, web), and a REST API.

## Prerequisites

- Go 1.25+
- At least one supported agent CLI installed and on `$PATH`:
  - [opencode](https://github.com/sst/opencode) (default — pairs with DeepSeek, Anthropic, or any OpenAI-compatible provider)
  - [Claude Code](https://docs.anthropic.com/en/docs/claude-code) (set `AGENT_CLI=claude`)
  - [Codex](https://github.com/openai/codex) (set `AGENT_CLI=codex`)
- Git configured with credentials for your remote

## Quick Start

```bash
go build -o agent-runner ./cmd/server
cp .env.example .env   # set at minimum an API key (see below)
./agent-runner
```

**Minimum config — opencode + DeepSeek** (default, recommended):
```bash
DEEPSEEK_API_KEY=sk-...
AGENT_MODEL=deepseek-v4-flash
AGENT_REASONING_MODEL=deepseek-v4-pro
```

**Minimum config — Claude Code** (if already installed and `claude login` done):
```bash
AGENT_CLI=claude
# No API key needed — claude manages its own credentials
```

For git operations against a self-hosted server:
```bash
GIT_HOST=git.example.com
GIT_ORG=myorg
GIT_TOKEN=your-personal-access-token   # or GIT_SSH_KEY=/path/to/key for SSH
```

> **Tip:** If you have a bot connected (Telegram, Stream), you can configure everything without SSH using `/set KEY VALUE` — e.g. `/set DEEPSEEK_API_KEY sk-...`. Changes persist to `.env.local` and take effect immediately.

## Docker

The image runs as a non-root `app` user (uid/gid `1000:1000` by default). Mount `/data` as a persistent volume and set `DATA_DIR=/data` so all mutable state (logs, repo-cache, memory, `.env.local`) survives image updates.

```bash
docker run -d \
  -v agent-data:/data \
  -e DATA_DIR=/data \
  -e DEEPSEEK_API_KEY=sk-... \
  -e AGENT_MODEL=deepseek-v4-flash \
  -e AGENT_REASONING_MODEL=deepseek-v4-pro \
  -p 8080:8080 \
  agent-runner
```

If your host bind-mount is owned by a different uid/gid, override at build time:

```bash
docker build --build-arg APP_UID=$(id -u) --build-arg APP_GID=$(id -g) -t agent-runner .
```

Pass additional env vars (or bind-mount a `.env` file) for full configuration — see `.env.example`.

## Configuration

All configuration is via environment variables (or `.env` file). See `.env.example` for the full list.

Key variables:

| Variable | Default | Description |
|----------|---------|-------------|
| `API_BIND` | `127.0.0.1:8080` | API listen address |
| `API_KEY` | | Authentication key (optional) |
| `AGENT_CLI` | `opencode` | Agent CLI backend (`opencode`, `claude`, or `codex`) |
| `AGENT_SYSTEM_PROMPT` | | Path to base agent prompt |
| `AGENT_PROMPT_FILE` | | Path to workflow prompt template |
| `AGENT_SHARED_REPOS` | | Comma-separated repos pre-populated in every workspace |
| `AGENT_SKILLS_DIR` | | Directory of agent skills pre-populated in every workspace |
| `AGENT_PLANNER_ENABLED` | `false` | Run planner sub-agent before iteration loop |
| `AGENT_REVIEWER_ENABLED` | `false` | Run reviewer sub-agent after iteration loop |
| `TELEGRAM_BOT_TOKEN` | | Telegram bot token |
| `STREAM_SERVER_URL` | | Agent Stream server URL |

## Connecting to Agent Stream

[Agent Stream](https://apps.apple.com/us/app/agent-stream/id6759258538) is an iOS app for conversational access to your agent. It lets you send messages, receive streaming responses, and get file attachments back from the agent.

To connect agent-runner, you need three values from the app:

1. **`STREAM_SERVER_URL`** — your Agent Stream server URL. Set it in the app via the gear icon on the login screen.

2. **`STREAM_BOT_TOKEN`** — create a bot in the app under Menu → Bots → tap `+`. The token is shown once after creation — copy it immediately.

3. **`STREAM_CONVERSATION_IDS`** — create a conversation in the app (tap `+` on the conversation list). The conversation ID starts with `c_` and is visible in the conversation detail.

```bash
STREAM_SERVER_URL=https://your-agent-stream-server
STREAM_BOT_TOKEN=your-bot-jwt
STREAM_CONVERSATION_IDS=c_your_conversation_id
```

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
