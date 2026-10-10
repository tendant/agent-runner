# Agent directory, isolation, memory and MCP

## Creating an agent

One directory is one agent. From an empty directory:

```bash
mkdir my-agent && cd my-agent
agent-runner            # start; then from chat:
```

All runner state — logs, memory, workspaces, repo cache, `agent-home/` —
lives in the agent directory (the current folder) unless `DATA_DIR` is set.

`/bootstrap` creates the agent's identity: `agent.md`, `prompt.md`,
`mcp.json.example` (rename to `mcp.json` and declare the agent's MCP
servers), and `.env` with `AGENT_ISOLATED=true`. `/config` always shows the
agent's directory, isolation state, and declared servers; `/install-mcp`
(or a restart) materializes declarations into `agent-home/`. `/set
AGENT_ISOLATED true|false` applies live — no restart needed.

## Agent isolation

Set `AGENT_ISOLATED=true` and spawned agents run inside `agent-home/` — a
self-contained config universe in the runner directory. The runner
provisions it at startup from what's already there: `mcp.json` (the agent's
MCP servers), `skills/` (synced into the claude skills dir), and copies of
the host CLIs' credentials. Executors are redirected into it via
`CLAUDE_CONFIG_DIR` / `CODEX_HOME` / `XDG_CONFIG_HOME`, so agents see
exactly the declared tools — nothing inherited from the host user's own CLI
configs.

One runner = one agent = one universe. For multiple agents (e.g. different
mail accounts), run multiple runner directories or containers — no profile
management. Isolation covers the tool surface, not the filesystem; for
enforcement run the runner in a container (agent-home lives in the runner
directory, so the volume contract is unchanged).

## Memory and learning loop

The agent evolves across sessions through markdown files in `MEMORY_DIR` (git-synced, human-editable). Each prompt is composed from `agent.md` + `prompt.md` + curated memory files (`user_preferences.md`, `decisions.md`, `lessons.md`, ...) + a **Recent Sessions** digest of the last `AGENT_MEMORY_DAYS` days of session logs, all bounded by `AGENT_MEMORY_CHAR_CAP`. The agent writes to its own memory files during sessions; after each session the runner appends an outcome log (including reviewer findings), and — with `AGENT_MEMORY_CURATION_ENABLED=true` — a cheap LLM pass distills durable lessons into `lessons.md` and compacts files that outgrow their budget. See [DESIGN.md](../DESIGN.md) ("Memory & Prompt Composition") for the full pipeline and safety rails.

New chat conversations get a one-time welcome message explaining what the agent does and pointing at `/help` (`WELCOME_ENABLED`, default on; customize via `MEMORY_DIR/WELCOME.md`).

## MCP servers for spawned agents

Declare MCP servers the agent CLIs should have in `mcp.json` next to the
runner (operator-owned; chat users can install declared servers but never
supply commands):

```json
{
  "servers": {
    "maildirx": {
      "command": "~/go/bin/maildirx",
      "args": ["mcp"],
      "env": { "MAIL_ROOT": "~/Mail", "MAILDIRX_MCP_MODE": "read-only" }
    }
  }
}
```

On startup the runner reconciles the declaration into the active CLI's own
config (via `claude mcp add`, `~/.codex/config.toml`, or
`~/.config/opencode/opencode.json`) — idempotently, so a fresh host or
container converges on boot. `/install-mcp [name]` triggers the same
reconciliation from chat.

## opencode on Linux (Ubuntu/Debian)

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
