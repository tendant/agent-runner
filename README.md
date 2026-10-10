# Agent Runner

Runs coding-agent CLIs ([pi](https://pi.dev), [Claude Code](https://docs.anthropic.com/en/docs/claude-code),
[Codex](https://github.com/openai/codex), [opencode](https://github.com/sst/opencode)) as autonomous agents
against Git repositories: a planner, an iteration loop and an optional reviewer, persistent memory,
chat interfaces (Telegram, Agent Stream, WeChat), scheduled tasks and a REST API.

## Prerequisites

- Go 1.25+
- Git, with credentials for your remotes
- One agent CLI on `$PATH`: `pi` (default), `claude`, `codex` or `opencode`. `POST /bootstrap`
  (or `/bootstrap` in chat) installs a missing one. On a headless Linux server, opencode also needs
  `xvfb` (see [docs/agents.md](docs/agents.md#opencode-on-linux-ubuntudebian)).

## Quick start

One directory is one agent; all its state (logs, memory, workspaces, repo cache) lives there
unless `DATA_DIR` says otherwise ([docs/agents.md](docs/agents.md)).

```bash
go build -o ~/go/bin/agent-runner ./cmd/server   # anywhere on $PATH
mkdir my-agent && cd my-agent
echo 'DEEPSEEK_API_KEY=sk-...' > .env               # a provider key; pi picks a model (AGENT_MODEL pins one)
agent-runner
curl -X POST localhost:8080/bootstrap               # installs the CLI if missing, seeds prompts, reports readiness
```

`AGENT_CLI=claude` needs no key when `claude login` is done on the host; any other provider is
in [.env.example](.env.example).

For git against a self-hosted server: `GIT_HOST`, `GIT_ORG`, and `GIT_TOKEN` (or `GIT_SSH_KEY`).

With a chat bot connected, `/set KEY VALUE` configures a running instance (saved to `.env.local`,
applied immediately); `/config`, `/status` and `/help` show the rest.

## Docker

```bash
docker run -d -v agent-data:/data -e DATA_DIR=/data -e DEEPSEEK_API_KEY=sk-... -p 8080:8080 agent-runner
```

The image runs as uid/gid `1000:1000`; for a bind mount owned by someone else, build with
`--build-arg APP_UID=$(id -u) --build-arg APP_GID=$(id -g)`.

## Configuration

Environment variables or `.env`; [.env.example](.env.example) lists every one with its default.
Upgrading from v0.0.x: see [MIGRATION.md](MIGRATION.md). The ones most people set first:

| Variable | Default | |
|---|---|---|
| `AGENT_CLI` | `pi` | `pi`, `claude`, `codex` or `opencode` |
| `AGENT_MODEL` / `AGENT_FAST_MODEL` | pi's choice (opencode: `deepseek/deepseek-v4-pro` / `-flash`) | Work model as `provider/model`, and a cheap one for planning and curation |
| `DATA_DIR` | the current directory | All mutable state |
| `API_BIND` / `API_KEY` | `127.0.0.1:8080` / none | API address and key |
| `GIT_TOKEN` / `GIT_SSH_KEY` | | Git credentials (`MEMORY_GIT_*` for a memory repo elsewhere) |
| `AGENT_SHARED_REPOS` | | Repos pre-cloned into every workspace |
| `AGENT_PLANNER_ENABLED` / `AGENT_REVIEWER_ENABLED` | `true` / `false` | Planner and reviewer sub-agents |
| `AGENT_MAX_CONCURRENT` | `1` | Sessions running at once |
| `AGENT_SANDBOX` | `off` | Run agents in a sandbox: `permissive` or `strict` (see [docs/sandbox.md](docs/sandbox.md)) |
| `TELEGRAM_BOT_TOKEN`, `STREAM_SERVER_URL` + `STREAM_BOT_TOKEN` | | Chat bots |

## API

```bash
curl -X POST localhost:8080/agent -H 'Content-Type: application/json' -d '{"message": "Build a landing page"}'
curl localhost:8080/agent/{session_id}             # status (GET .../stream: live events)
curl -X POST localhost:8080/agent/{session_id}/stop
```

## More

| Topic | |
|---|---|
| [docs/api.md](docs/api.md) | Endpoints, live event stream, webhooks, audit logs, tracing, metrics, errors |
| [docs/sessions.md](docs/sessions.md) | Parallel sessions and git conflicts, multi-turn tasks, named locks |
| [docs/agents.md](docs/agents.md) | The agent directory, isolation (`agent-home/`), memory, MCP servers |
| [docs/agent-stream.md](docs/agent-stream.md) | Connecting the Agent Stream app |
| [docs/scheduler.md](docs/scheduler.md) | One-shot and cron tasks, `_schedule.json` |
| [docs/sandbox.md](docs/sandbox.md) | Agent sandbox (Seatbelt, gVisor) |
| [DESIGN.md](DESIGN.md) | Architecture; memory and prompt composition |
| [TASKS_DESIGN.md](TASKS_DESIGN.md), [SECRETS.md](SECRETS.md) | Task and secrets design |

## Development

```bash
go test -race ./...
```
