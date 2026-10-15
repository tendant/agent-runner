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

Repos to work on are added in chat, each with its own login from a `/secret`:
`/secret set GITEE_TOKEN <token>` then `/repo add https://<user>@gitee.com/org/repo.git secret=GITEE_TOKEN`
([REPOS_DESIGN.md](REPOS_DESIGN.md)). Every session gets every repo; agent-runner pushes the agent's commits.

With a chat bot connected, `/set KEY VALUE` configures a running instance (saved to `.env.local`,
applied immediately); `/config`, `/status` and `/help` show the rest.

### Sandboxed agents

To run every agent CLI in a sandbox ([agentbox](https://github.com/tendant/agentbox): gVisor on
Linux, Seatbelt on macOS, as your own user, never root):

```bash
# Linux, once, by an admin (uses sudo; agent-runner itself never does):
sudo apt install iproute2 nftables
git clone https://github.com/tendant/agentbox && (cd agentbox && make install && make check)
# macOS: nothing to install.

echo 'AGENT_SANDBOX=permissive' >> .env             # strict: refuse runs it cannot fully enforce
echo 'CLAUDE_CODE_OAUTH_TOKEN=...' >> .env          # claude: from `claude setup-token`
```

The sandbox hides your home directory and credentials; the agent reads its workspace, memory and
the CLIs' installs, and reaches the network only through a per-run gateway (any public host by
default, never private or metadata addresses). Model credentials stay in agent-runner: claude and
codex reach their models through a per-run proxy, and codex uses your `codex login` without the
sandbox ever seeing it. `/status` shows whether the sandbox is ready; `make sandbox-smoke` checks
it end to end. Details: [docs/sandbox.md](docs/sandbox.md).

## Docker

```bash
docker run -d -v agent-data:/data -e DATA_DIR=/data -e DEEPSEEK_API_KEY=sk-... \
  -e API_BIND=0.0.0.0:8080 -e API_KEY=<a long random key> -p 8080:8080 agent-runner
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
| `MEMORY_GIT_TOKEN` / `MEMORY_GIT_SSH_KEY` | | Credentials for the memory repo (project repos use `/repo` and `/secret`) |
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
| [docs/sandbox.md](docs/sandbox.md) | Agent sandbox (agentbox: gVisor on Linux, Seatbelt on macOS), egress, limits |
| [DESIGN.md](DESIGN.md) | Architecture; memory and prompt composition |
| [TASKS_DESIGN.md](TASKS_DESIGN.md), [SECRETS.md](SECRETS.md) | Task and secrets design |

## Development

```bash
go test -race ./...
make sandbox-smoke        # the sandbox on this host, end to end (as a normal user)
make sandbox-agentbox     # the Linux sandbox in Docker, from any host
```
