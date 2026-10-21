# Agent Runner

Runs coding-agent CLIs ([pi](https://pi.dev), [Claude Code](https://docs.anthropic.com/en/docs/claude-code),
[Codex](https://github.com/openai/codex), [opencode](https://github.com/sst/opencode)) as autonomous agents
against Git repositories: a planner, an iteration loop and an optional reviewer, persistent memory,
chat interfaces (Telegram, Agent Stream, WeChat), scheduled tasks and a REST API.

## Prerequisites

- Go 1.25+
- Git (repo credentials are given in chat with `/env secret`, not on the host)
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

### In chat

Once a bot is connected (Telegram needs `TELEGRAM_CHAT_ID`: the one chat that controls the agent;
WeChat answers `WECHAT_ALLOWED_USERS`, or the first user to message it; on agent-stream anyone in a
channel can talk to the agent, but only the bot's owner and `STREAM_ADMIN_USERS` run commands, and
only the owner can add the bot to a channel), `/help` shows how to start:

```
/env secret GITEE_TOKEN <token> Gitee API and git      a credential for the agent's tasks
/repo add https://<user>@gitee.com/org/repo.git secret=GITEE_TOKEN
then describe the task in a message
```

### agent-runner's settings, and the agent's environment

Two commands, by who a value is for: **agent-runner itself** (`/config`), or **the code the agent
runs** (`/env`).

| | `/config set KEY VALUE` | `/env set NAME VALUE [description]` | `/env secret NAME VALUE [description]` |
|---|---|---|---|
| For | agent-runner: its model and key, its CLI, limits, the sandbox | the agent's tasks: a plain setting | the agent's tasks: a credential |
| Example | `/config set DEEPSEEK_API_KEY sk-...` · `/config set AGENT_CLI claude` | `/env set APP_ENV staging deploy target` | `/env secret STRIPE_API_KEY sk_test_... Stripe` |
| Saved in | `DATA_DIR/.env.local` (above `.env`, below the OS environment) | `DATA_DIR/env.json` | `DATA_DIR/secrets.json` |
| Applies | at once; the reply says when one waits for a restart | from the next session | from the next session |
| The agent | never gets it (except the model key its CLI needs, by its own route) | gets `$NAME`; its prompt shows `NAME=value` | gets `$NAME`; its prompt shows the name and description only |
| Shown | `/config` (keys masked) | `/env`, the prompt, logs | never: hidden in chat, logs and memory |
| Remove | `/config unset KEY` (`.env` or the default applies again) | `/env unset NAME` | `/env unset NAME` |

- **`/config set` takes only settings agent-runner reads** ([.env.example](.env.example) lists them):
  anything else would be saved and never read, so it is refused with a pointer to `/env`. It also
  refuses names that change how processes run (`PATH`, `GIT_*`, `LD_*`, ...). A few settings are
  read only at start (the chat connection, the listen address, the sandbox, the worker pool): those
  are saved and apply after a restart, and the reply says so.
- **`/env` is everything the agent's tasks get**, in one list. `/env set` refuses a value shaped like
  a credential (`ghp_…`, `sk-…`, a private key, a URL with a password) and points to `/env secret`, so
  a token is never shown by mistake. A name is either plain or secret: setting it as the other kind
  moves it. Names agent-runner reads (`AGENT_*`, `STREAM_*`, ...) are refused, with where they go.
- **Model keys for the apps the agent builds:** `DEEPSEEK_API_KEY`, `OPENAI_API_KEY` and
  `ANTHROPIC_API_KEY` can be in `/env` too, apart from agent-runner's own in `/config`: each agent
  CLI gets agent-runner's key by its own route (pi `--api-key` or the sandbox's `auth.json`, codex a
  `-c` provider, opencode its config), so the two never mix. Except `ANTHROPIC_API_KEY` while the
  agent CLI is claude, which reads only its environment: give the agent that key under another name.
- agent-runner's own keys never reach the agent: bot tokens, `API_KEY` and the memory token are left
  out of its environment even without the sandbox (the model provider key its CLI needs is kept).
- **Sealed on agent-stream:** `/env secret` and `/config set` (and the old `/secret` and `/set`,
  typed from habit, and misspelled forms) are kept and shown masked (`/env secret NAME ••••••`), push
  nobody, and reach this bot once; a plain `/env set` stays readable unless its value looks like a
  credential.

### More in chat

- **Repos** (`/repo`, `/help repo`): every session gets every registered repo, logging in with its
  `/env secret`; the agent commits, agent-runner pushes ([REPOS_DESIGN.md](REPOS_DESIGN.md)).
- **Memory and instructions** (`/help memory`): `/memory log` lists what each session saved,
  `/memory undo <session-id>` takes it back; `/prompt show|check|preview` shows `agent.md`,
  `prompt.md` and the prompt a session gets ([MEMORY_DESIGN.md](MEMORY_DESIGN.md)).
- **Alerts** (`/alerts`): CI, monitoring or GitHub post to an agent-stream channel through an
  incoming webhook (`asctl webhooks create <channel-id> ci`); the bot triages each alert: ignores
  noise, summarizes, or proposes a fix and waits for a yes in the thread. `/alerts set <policy>`
  says what it may fix without asking. Alert text never runs a command.
- **No lost work:** before a session's or task's workspace is removed, anything not on a remote is
  kept on the host in `DATA_DIR/rescued/<workspace>/` for 30 days (`AGENT_RESCUE_RETENTION`):
  unpushed commits as a git bundle, uncommitted files and files outside any repo as they were, with
  a README on restoring them. The thread is told. Only registered repos (`/repo add`) are pushed.
- **Status:** `/config` shows the settings and readiness, `/status` what is running.

### The agent's environment and repos as files

`/env` and `/repo add` write three files in `DATA_DIR`, which you can also write yourself, keep in
git and deploy, so a bot's setup is repeatable. All are read when used: changes apply at the next
session, without a restart.

`env.json`, the agent's plain settings (no secrets in it: commit it as is):

```json
{
  "APP_ENV": { "value": "staging", "description": "where the app deploys" },
  "API_BASE_URL": { "value": "https://api.staging.example.com" }
}
```

`secrets.json`, the agent's secrets (multi-line values such as SSH keys are ordinary strings):

```json
{
  "GITEE_TOKEN": { "value": "1a2b3c…", "description": "Gitee API and git for oam-server" },
  "GITEE_SSH_KEY": { "value": "-----BEGIN OPENSSH PRIVATE KEY-----\n…\n-----END OPENSSH PRIVATE KEY-----\n",
                     "description": "deploy key for oam-ops" }
}
```

`repos.json`, the repos every session gets:

```json
[
  { "name": "oam-server", "url": "https://lei@gitee.com/org/oam-server.git", "secret": "GITEE_TOKEN", "branch": "develop", "push": true },
  { "name": "docs", "url": "https://github.com/org/docs.git", "push": false }
]
```

Encrypt only the secrets' values with [sops](https://github.com/getsops/sops), so a diff still shows
which secret changed and what it is for, and decrypt on the bot's host when deploying:

```sh
sops --encrypt --age <recipient> --encrypted-regex '^value$' secrets.json > agent-secrets.sops.json   # commit this
sops --decrypt agent-secrets.sops.json > "$DATA_DIR/secrets.json" && chmod 600 "$DATA_DIR/secrets.json"
cp env.json repos.json "$DATA_DIR/"                                                                     # no secrets in them
```

- Prefer this to `/env secret` in chat for anything long-lived. agent-stream never stores a value
  typed there (it seals the command), but Telegram and WeChat keep it in the chat, and the sender's
  own app may show what was typed until it reloads.
- A name in both `env.json` and `secrets.json` is a secret. Names agent-runner reads itself are
  skipped even in a hand-written file: agent-runner's own keys stay in `.env` and never reach the
  agent.
- `/repo add` fetches a repo to check its URL, branch and token; a hand-written entry is first
  fetched at the next session, so a mistake shows up there (and in `/repo list`).

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
| `API_BIND` / `API_KEY` | `127.0.0.1:8080` / none | API address and key (browser requests from other sites are refused either way) |
| `MEMORY_GIT_TOKEN` / `MEMORY_GIT_SSH_KEY` | | Credentials for the memory repo (project repos use `/repo` and `/env secret`) |
| `AGENT_PLANNER_ENABLED` / `AGENT_REVIEWER_ENABLED` | `true` / `false` | Planner and reviewer sub-agents |
| `AGENT_MAX_CONCURRENT` | `1` | Sessions running at once |
| `AGENT_SANDBOX` | `off` | Run agents in a sandbox: `permissive` or `strict` (see [docs/sandbox.md](docs/sandbox.md)) |
| `TELEGRAM_BOT_TOKEN` + `TELEGRAM_CHAT_ID` | | Telegram bot; it answers no one until the chat id is set (the log names it) |
| `WECHAT_TOKEN`, `WECHAT_ALLOWED_USERS` | | WeChat bot; without the list, the first user to message it |
| `STREAM_SERVER_URL` + `STREAM_BOT_TOKEN` | | Agent Stream bot |

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
| [MEMORY_DESIGN.md](MEMORY_DESIGN.md), [REPOS_DESIGN.md](REPOS_DESIGN.md), [SECRETS_DESIGN.md](SECRETS_DESIGN.md) | Memory and prompts, repos and their logins, agent secrets |
| [TASKS_DESIGN.md](TASKS_DESIGN.md) | Multi-turn tasks |

## Development

```bash
go test -race ./...
make sandbox-smoke        # the sandbox on this host, end to end (as a normal user)
make sandbox-agentbox     # the Linux sandbox in Docker, from any host
```
