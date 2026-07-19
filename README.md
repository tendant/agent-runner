# Agent Runner

An autonomous AI agent that executes tasks iteratively against Git repositories. Supports [Claude Code](https://docs.anthropic.com/en/docs/claude-code), [Codex](https://github.com/openai/codex), [opencode](https://github.com/sst/opencode), and other compatible CLI agents — with planning, review phases, conversational interfaces (Telegram, web), and a REST API.

## Prerequisites

- Go 1.25+
- At least one supported agent CLI installed and on `$PATH`:
  - [opencode](https://github.com/sst/opencode) (default — pairs with DeepSeek, Anthropic, or any OpenAI-compatible provider)
  - [Claude Code](https://docs.anthropic.com/en/docs/claude-code) (set `AGENT_CLI=claude`)
  - [Codex](https://github.com/openai/codex) (set `AGENT_CLI=codex`)
- Git configured with credentials for your remote

### opencode on Linux (Ubuntu/Debian)

opencode is distributed as an [AppImage](https://appimage.org/) on Linux. Because it is built on Electron, it requires a display even for basic operations. On a headless server, install `xvfb` so agent-runner can run version checks (and opencode itself) without a physical display:

```bash
sudo apt install xvfb
```

Install opencode via the bot with `/install-cli opencode`, or manually:

```bash
curl -fsSL https://opencode.ai/install | sh
# or download the AppImage from https://github.com/sst/opencode/releases
# and place it at ~/bin/opencode (chmod +x)
```

Make sure `~/bin` (or wherever opencode is installed) is on your `$PATH`.

## Quick Start

```bash
go build -ldflags "-X main.buildTime=$(date -u +%Y-%m-%dT%H:%M:%SZ)" -o agent-runner ./cmd/server
cp .env.example .env   # set at minimum an API key (see below)
./agent-runner
curl -X POST http://localhost:8080/bootstrap   # installs the CLI if missing, seeds default prompts, reports readiness
```

**Minimum config — opencode + DeepSeek** (default, recommended):
```bash
DEEPSEEK_API_KEY=sk-...
```
(opencode is the default `AGENT_CLI`, and already defaults to `deepseek-v4-flash`/`deepseek-v4-pro` — only set `AGENT_MODEL`/`AGENT_REASONING_MODEL` if you want different models.)

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
  -p 8080:8080 \
  agent-runner
```

If your host bind-mount is owned by a different uid/gid, override at build time:

```bash
docker build --build-arg APP_UID=$(id -u) --build-arg APP_GID=$(id -g) -t agent-runner .
```

Pass additional env vars (or bind-mount a `.env` file) for full configuration — see `.env.example`.

## Configuration

All configuration is via environment variables (or `.env` file). `.env.example` is the full reference (grouped by category, with every var's default); the table below covers the ones most people touch first.

Key variables:

| Variable | Default | Description |
|----------|---------|-------------|
| `API_BIND` | `127.0.0.1:8080` | API listen address |
| `API_KEY` | | Authentication key (optional) |
| `DATA_DIR` | CWD | Base dir for all mutable state (logs, repo-cache, memory, `.env.local`) |
| `INSTANCE` | | Instance name — loads `.env.<instance>`, scopes the default `DATA_DIR` |
| `AGENT_CLI` | `opencode` | Agent CLI backend (`opencode`, `claude`, or `codex`) |
| `AGENT_SYSTEM_PROMPT` | | Path to base agent prompt |
| `AGENT_PROMPT_FILE` | | Path to workflow prompt template |
| `AGENT_SHARED_REPOS` | | Comma-separated repos pre-populated in every workspace |
| `AGENT_SKILLS_DIR` | | Directory of agent skills pre-populated in every workspace |
| `AGENT_PLANNER_ENABLED` | `true` | Run planner sub-agent before iteration loop |
| `AGENT_REVIEWER_ENABLED` | `false` | Run reviewer sub-agent after iteration loop |
| `GIT_TOKEN` / `GIT_SSH_KEY` | | Credentials for project repo git operations |
| `MEMORY_GIT_TOKEN` / `MEMORY_GIT_SSH_KEY` | falls back to `GIT_TOKEN` / `GIT_SSH_KEY` | Credentials for the memory repo, if it's on a different host |
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

### Error handling

`POST /agent` checks that the configured `AGENT_CLI` binary is actually installed before queueing a session — if it's missing, you get a `412` immediately instead of a session that fails minutes later after workspace setup:

```json
{"error": "codex CLI is not installed — install it (npm install -g @openai/codex ...) or run POST /bootstrap to auto-install"}
```

Missing credentials (e.g. no `ANTHROPIC_API_KEY`) don't block the request — some setups authenticate outside an API key env var — but are surfaced as a non-fatal `warnings` array on the `202` response and on the session itself:

```json
{"session_id": "agent-...", "status": "queued", "warnings": ["claude backend requires ANTHROPIC_API_KEY (or ANTHROPIC_BASE_URL for local models)"]}
```

If a session does fail on a recognized misconfiguration (bad/expired key, quota exceeded, CLI missing, unknown model), `GET /agent/{id}`'s `error` field is a short actionable message with the raw CLI/API error preserved underneath, e.g. `"authentication with the LLM provider failed — check credentials with /status, or re-run /auth\n\nDetails: ..."`. These same messages reach chat clients (Telegram, Stream, WeChat) too. Check overall readiness anytime with `/status` or `POST /bootstrap`.

## Scheduled Tasks

agent-runner can run agent tasks in the future — once, after a delay, or on a recurring cron schedule — without any external cron daemon. This is opt-in and disabled by default; it needs a database to durably track due tasks across restarts.

**Enable it:**
```bash
SCHEDULER_ENABLED=true
SCHEDULER_DATABASE_URL=postgres://user:pass@host/db   # or sqlite:///path/to/agent-runner.db
```

See `.env.example` for tuning knobs (`SCHEDULER_LEASE_DURATION`, `SCHEDULER_POLL_CAP`, `SCHEDULER_HEARTBEAT_INTERVAL`, `SCHEDULER_MAX_ATTEMPTS`, `SCHEDULER_AGENT_ID`) — the defaults are sensible for a single instance. With `SCHEDULER_ENABLED=false` (the default), the endpoints below return `503 runner not enabled`.

**Schedule a task via the API:**
```bash
curl -X POST http://localhost:8080/schedule \
  -H "Content-Type: application/json" \
  -d '{"message": "Check for new PRs and summarize them", "cron": "0 9 * * *", "timezone": "America/Los_Angeles"}'
```

`message` is required; exactly one scheduling mode is required alongside it:
- `run_after` — an absolute RFC3339 timestamp, for a one-shot task
- `run_in_seconds` — a relative delay from now, for a one-shot task
- `cron` — a cron expression, for a recurring task (`timezone` optional, defaults to UTC)

`idempotency_key` (optional) dedupes one-shot tasks — resubmitting the same key is a no-op instead of double-scheduling.

```bash
# List active schedules
curl http://localhost:8080/schedules

# Cancel one
curl -X DELETE http://localhost:8080/schedule/{id}
```

**Scheduling from within an agent session:** a running agent can queue its own follow-up tasks by writing a `_schedule.json` file to its workspace root (same convention as `_send/` for output files) — a JSON array of objects shaped like the `POST /schedule` body above:
```json
[{"message": "Check back on this deployment", "run_in_seconds": 600}]
```
agent-runner reads this file after the session completes and submits each entry the same way `POST /schedule` does. This is how an agent sets its own reminders or recurring checks without needing network access to call the API itself.

When a schedule fires, agent-runner starts a normal agent session with `message` as the task — it goes through the same planner/iteration/reviewer pipeline and shows up in `/status`, `/sessions`, and the audit log like any other session.

## Testing

```bash
go test -race ./...
```
