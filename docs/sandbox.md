# Agent sandbox

agent-runner can run every agent CLI (pi, claude, codex, opencode) and all of the tools it
starts inside one isolation boundary, provided by [agentbox](https://github.com/tendant/agentbox):
**gVisor on Linux, Seatbelt on macOS**, as the runner's own user (never root). **Policy lives in
agent-runner; agentbox only enforces.**

> If the backend cannot enforce a required property, a `strict` run does not start.

## Setup

* **Linux** (production): once, by an admin. agentbox's installer puts gVisor's `runsc` and, on
  Ubuntu 23.10+, the AppArmor profiles rootless gVisor needs in place (it uses sudo; agent-runner
  never does):

  ```bash
  sudo apt install iproute2 nftables         # transparent egress (below)
  git clone https://github.com/tendant/agentbox && cd agentbox
  make install                               # runsc, AppArmor profiles, agentbox-unshare
  make check                                 # as the runner's user: what this host supports
  ```

* **macOS** (development): nothing to install; `sandbox-exec` is part of the system. Build
  agent-runner with cgo (the default) so closing a sandbox can find every process in it.

## Modes

`AGENT_SANDBOX` = `off` (default) | `permissive` | `strict`

| Mode | Behaviour |
|---|---|
| off | agent CLIs run on the host exactly as before |
| permissive | run in the sandbox; unenforceable properties are logged (`sandbox.started` detail `unenforced: ...`) |
| strict | any required capability the backend cannot enforce rejects the run before any work |

An invalid sandbox configuration rejects every run; it never falls back to the host.

Other settings:

* `AGENT_SANDBOX_POLICY`: the system-layer `SandboxSpec`, a path to a JSON file or the JSON
  itself when it starts with `{`. Fields it leaves unset keep the default policy's values, so
  `{"version":1,"network":{"egress":"none"}}` still grants the workspace, home and tmp; a field
  it sets replaces the default's.
* `AGENT_SANDBOX_MEMORY` (e.g. `4g`) and `AGENT_SANDBOX_PIDS` (e.g. `1024`): caps for every run,
  overriding the policy's (Linux only, see [Fork bombs](#fork-bombs-memory-and-pids-caps)).
* `AGENT_SANDBOX_EGRESS_ALLOW` and `AGENT_SANDBOX_EGRESS_MODE`: see [Network](#network).
* `AGENT_SANDBOX_READ_ALLOW`: see [What the sandbox can read](#what-the-sandbox-can-read).
* `AGENT_SANDBOX_ENV_ALLOW`: host env var names to pass in (comma-separated); model credentials
  are never passed while the model proxy is on.
* `AGENT_SANDBOX_MODEL_PROXY` (default `true`) and `AGENT_SANDBOX_CODEX_LOGIN`: see
  [Claude credentials and config](#claude-credentials-and-config).
* `AGENT_SANDBOX_EVIDENCE`: a conformance report; only the capabilities it proves are claimed.
* `AGENT_SANDBOX_BACKEND`: unset or `agentbox`. The isobox values (`gvisor`, `seatbelt`) are
  refused: isobox was removed. `AGENT_SANDBOX_READ` accepts only `scoped`, the default.

## Linux and macOS

| | Linux: gVisor | macOS: Seatbelt |
|---|---|---|
| Kernel | gVisor's own (`kernel.isolation`) | the host's |
| Files | grants mounted at their own host paths, nothing else of the host | a `sandbox-exec` profile: the same paths readable and writable |
| Network | its own stack: loopback only | the host's stack: no sockets but the run's own ports |
| Egress | `transparent` (every connection) or `proxy` | `proxy` |
| Model proxies | Unix sockets mounted in, a forwarder inside | ports on the host's loopback |
| Limits | memory, CPU (systemd user scope), processes (`RLIMIT_NPROC`) | none |
| Containment | closing the sandbox kills everything in it | process groups, and every process still under the run's profile |

Both run as the runner's user: files the agent writes belong to it, root-owned files stay
unreadable, and a private (0700) home is no obstacle. Each proves the capabilities in its
baseline, `.github/sandbox-baselines/agentbox-gvisor.json` and `agentbox-seatbelt.json`
(`make sandbox-smoke`).

## Network

The sandbox has no direct network. Its ways out are the run's model proxies and its egress
gateway (`internal/sandbox/egress`), both in the runner.

**Egress.** Policy `outbound` becomes `restricted` egress through the gateway. The gateway
resolves names itself and allows ports 443 and 80. By default it allows **any public host**;
`AGENT_SANDBOX_EGRESS_ALLOW=github.com,*.npmjs.org,...` narrows that to listed hosts. Private,
loopback, link-local and metadata addresses are refused either way, also when a name resolves to
one. git over SSH (port 22) is refused: use HTTPS remotes. How connections reach the gateway,
`AGENT_SANDBOX_EGRESS_MODE`:

* **`transparent`** (Linux; what `auto`, the default, picks where the host supports it):
  agentbox's gateway network. The sandbox gets a private network namespace in which **every** TCP
  connection, whether or not the program honours proxy settings, is handed to the gateway for the
  host it was made for. DNS is answered inside with stand-in addresses (198.18.0.0/15: nothing is
  resolved in the sandbox or leaks over DNS); other UDP, ICMP and IPv6 are dropped. Needs `ip` and
  `nft` and, where AppArmor restricts user namespaces, `agentbox-unshare` (both from Setup).
  `transparent` set explicitly refuses to run without them.
* **`proxy`** (macOS always; Linux when `auto` falls back): commands get `HTTPS_PROXY`/`HTTP_PROXY`
  (and `NO_PROXY=127.0.0.1,localhost`) pointing at the gateway, which git over HTTPS, npm, pip, go
  and curl honour. On Linux the gateway is a socket mounted into the sandbox and served at
  `127.0.0.1:3128` by the forwarder; on macOS it is a port on the host's loopback that the
  sandbox may connect to. A program that ignores the proxy settings gets no connection at all.

**What it did.** Each run tallies the gateway's decisions by host. The first time a run reaches
or is refused a host, the runner logs `sandbox.egress_allowed` or `sandbox.egress_denied` with
the host (names only, never payloads), and the session's audit log (`LOGS_ROOT`) gets a
**Network** table: every host, refused first, with how many connections were allowed and
refused (at most 500 hosts a run).

**Nothing else.** On macOS the profile denies every socket except connections to the run's own
loopback ports, and Mach lookups of the keychain (`com.apple.SecurityServer`) and DNS
(`com.apple.mDNSResponder`) services; a sandboxed process cannot listen or signal anything
outside its sandbox.

## What the sandbox can read

An allowlist, on both systems: the run's workspace, sandbox home and tmp, the memory dir and
uploads, the system paths programs need (Linux: `/usr`, `/bin`, `/sbin`, `/lib*`, `/etc`, `/opt`;
macOS: `/usr`, `/bin`, `/sbin`, `/System`, `/Library`, `/opt`, `/private/etc`, `/private/var/db`),
the agent CLIs' installs (each of `claude`, `codex`, `pi`, `opencode` and `node` found on `PATH`:
its directory, a `bin/` directory's prefix unless that is directly in the home directory, an npm
`node_modules` tree; never the home directory itself), and `AGENT_SANDBOX_READ_ALLOW`
(comma-separated host paths, e.g. a toolchain under your home: `~/go,~/.cargo`). The home
directory, its credentials (`~/.ssh`, `~/.codex`, `~/.local/share`, ...) and other projects are
not readable.

The runner's own files are denied even where a readable path covers them: its `.env` files, and
`STATE_ROOT`, `TMP_ROOT`, `LOGS_ROOT`, `OUTPUTS_ROOT`, `REPO_CACHE_ROOT` except the run's own
workspace, home and tmp (other sessions, other threads' homes, leases and the session journal
stay denied).

A tool that fails reading something outside the allowlist (a cache, a toolchain in your home)
needs that path in `AGENT_SANDBOX_READ_ALLOW`.

## How a run is confined

```
Resolve policy -> Check (reject in strict) -> Thread lease -> sandbox lease
  -> run ctx carries SandboxLauncher -> planner / session / reviewer CLIs -> Finish
```

* The whole CLI process tree is the sandboxed unit (the CLIs run their own tools), so there is
  no per-tool shim and no command allowlist.
* Sandbox state: `STATE_ROOT/sandbox/{leases,home/<thread>,runs/<run>/tmp,agentbox}`. Lease
  updates serialize on one `leases/.lock`; a released lease leaves no file behind.
* `HOME`/`TMPDIR` point at the run's private directories; the environment is an allowlist.
* Runner-side git (`internal/gitsafe`) is hardened against hooks/fsmonitor planted in the workspace.

## Claude credentials and config

The sandbox hides the host's `claude login` (`~/.claude`, and on macOS the keychain), and copying
its `.credentials.json` would break the host login when a copy refreshes the rotating token. So:

* **Credentials:** run `claude setup-token` (a long-lived subscription token, no refresh) and set
  `CLAUDE_CODE_OAUTH_TOKEN` in the runner's env, or set `ANTHROPIC_API_KEY`. With neither, a claude
  run fails before it starts, saying so.
* **Model proxy** (`AGENT_SANDBOX_MODEL_PROXY`, default on): credentials stay in the runner. Each
  sandbox run starts a proxy (`internal/sandbox/proxy`) per provider that attaches the credential
  and forwards; the CLIs get only proxy URLs and per-run tokens, and the credential variables are
  stripped from the sandbox whatever `AGENT_SANDBOX_ENV_ALLOW` lists.
  * **Anthropic** (`claude`, `pi`): `CLAUDE_CODE_OAUTH_TOKEN` (sent as `Authorization: Bearer`) or
    `ANTHROPIC_API_KEY` (`x-api-key`), to `ANTHROPIC_BASE_URL` or the Anthropic API; paths
    `/v1/messages`, `/api/hello`. `claude` gets `ANTHROPIC_BASE_URL`/`ANTHROPIC_AUTH_TOKEN`.
  * **OpenAI** (`codex`, `pi`): `OPENAI_API_KEY`, to `OPENAI_BASE_URL` or the OpenAI API; paths
    `/v1/responses`, `/v1/chat/completions`, `/v1/models`. codex ignores `OPENAI_BASE_URL` for its
    built-in provider, so the run's `CODEX_HOME/config.toml` defines a provider pointing at the
    proxy.
  * **OpenAI with a ChatGPT subscription** (`codex` only). With `AGENT_CLI=codex` and no
    `OPENAI_API_KEY`, sandboxed codex uses the ChatGPT login codex itself uses on the host:
    `$CODEX_HOME/auth.json` or `~/.codex/auth.json` (`agent-home/codex/auth.json` with
    `AGENT_ISOLATED=true`), if that is a ChatGPT login (`codex login`). `AGENT_SANDBOX_CODEX_LOGIN`
    names another `auth.json` instead (it then also applies with an API key set or another
    `AGENT_CLI`), for example one made for the runner alone
    (`CODEX_HOME=$DATA_DIR/state/codex-login codex login --device-auth`, then
    `AGENT_SANDBOX_CODEX_LOGIN=$DATA_DIR/state/codex-login/auth.json`); `off` turns it off. The
    runner keeps the login (`internal/sandbox/codexlogin`), and the proxy forwards to
    `https://chatgpt.com/backend-api/codex` (`/responses`, `/responses/compact`, `/models`) with
    the access token and `ChatGPT-Account-ID`. codex's config names the provider `OpenAI` (codex
    keys its OpenAI request features on that name) and turns websockets off.
    * **Sharing `~/.codex/auth.json` with codex on the host** works the way codex shares it
      between its own processes. Each refresh rotates the refresh token, and there is no file lock:
      codex re-reads the file before refreshing and adopts tokens another process wrote. The runner
      does the same: it re-reads the file on every request and writes it atomically, keeping every
      field. It refreshes 10 minutes before expiry (codex waits until 5), so when both are active
      the runner refreshes first and codex finds fresh tokens. After a failed refresh it adopts
      tokens codex rotated meanwhile. Both refreshing in the same instant still fails one side
      with "sign in again" (`refresh_token_reused`); whether OpenAI then revokes the whole login is
      untested. A login made for the runner alone avoids the race.
    * The login's directory is hidden from the sandbox (`~/.codex` already is), so a runner login
      must be in a directory of its own (not `/` or the home directory; the runner refuses those).
    * It replaces `OPENAI_API_KEY` as the OpenAI channel, so pi gets no OpenAI provider then.
    * `AGENT_SANDBOX_CHATGPT_URL` sends it to another endpoint than `https://chatgpt.com` (the
      smoke test's fake backend).
    * Checked with the real codex CLI (0.162) against a fake backend on both systems
      (`make sandbox-smoke`). Against chatgpt.com it is untested, including whether Cloudflare
      challenges the proxy.
  * **pi** gets the run's `models.json` (the seed's, with each proxied built-in provider's `baseUrl`
    set to its proxy) and run tokens in env (`ANTHROPIC_OAUTH_TOKEN` for a setup-token).

  Proxies allow 600 requests a minute and 64 MiB a request and stop with the run. They need no
  network: on Linux the sandbox reaches them over Unix sockets mounted into it, served at
  `http://127.0.0.1:4801` (Anthropic) and `:4802` (OpenAI) by a forwarder inside (agent-runner's
  own binary, `__agent-runner-fwd`); on macOS at their ports on the host's loopback. So they work
  with egress `none` too.
* **codex and pi config:** `CODEX_HOME` (`~/.codex`) and `PI_CODING_AGENT_DIR` (`~/.pi/agent`, sessions
  in `~/.pi/sessions`) are in the sandbox home too, seeded from `~/.codex` / `~/.pi/agent` (or
  `agent-home/`) with `AGENTS.md`, `prompts/`, `skills/` and pi's `settings.json`, never `auth.json`.
* **Config:** `CLAUDE_CONFIG_DIR` is `/home/agent/.claude` (the thread's sandbox home: writable,
  kept across a task's turns so `--resume` works). Before each run it is refreshed from
  `~/.claude` (or `agent-home/claude` with `AGENT_ISOLATED=true`), copying only `settings.json`,
  `CLAUDE.md`, `agents/`, `commands/`, `skills/` (symlinks resolved) and the `mcpServers` and
  `hasCompletedOnboarding` keys of `.claude.json`. Never credentials, other projects'
  transcripts, history or todos.

## Policy

`SandboxSpec` (version 1; unknown fields or versions fail closed). Layers
`system -> bot -> agent -> workspace -> run` may only tighten: unset inherits, widening is an
error naming the layer, deny lists and requirements only grow, secrets are granted only by the
first layer. Filesystem uses logical roots `/workspace`, `/home/agent`, `/tmp`.
Egress modes: `none`, `restricted` (hosts, single-label `*.wildcards`, CIDRs, `@sets`), `outbound`
(which becomes `restricted` to any public host).

Default system policy: workspace/home/tmp writable, outbound network without listeners.

## Capabilities and evidence

Each capability (`internal/sandbox/capability.go`) lists conformance tests; a backend may claim it
only if all pass. Tests not implemented yet count as unproven.

```
sandbox-conformance --report agentbox-host.json --manifest m.json
AGENT_SANDBOX_EVIDENCE=agentbox-host.json AGENT_SANDBOX=strict ...
```

## Known gaps (be honest in strict deployments)

* **macOS is weaker:** the host's kernel and network stack, no memory/CPU/process limits, and
  other host programs' processes are visible. Closing a sandbox finds its processes by their
  profile, which needs a cgo build; without cgo, a process that left its process group
  (`setsid`) outlives it.
* **No disk quota:** `WatchDisk`/`LimitedWriter` exist but are not wired into runs.
* **In-place runs:** the sandbox writes the task workspace directly. The validated-diff export with
  durable quarantine (`internal/sandbox/workspace`) is implemented and tested but not in the run path.
* **The model proxy covers Anthropic and OpenAI** for claude, codex and pi. opencode and other
  providers (DeepSeek, ...) still need their keys through `AGENT_SANDBOX_ENV_ALLOW`, and egress
  to their APIs.
* The fast-LLM executor fallback (analyzer, curator, and the planner outside a session, used when
  no LLM API key is set) runs the agent CLI in a sandbox run of its own, sharing one sandbox
  thread (`llm-fallback`).
* Conformance does not prove `fs.virtual_paths` on either system (no `/workspace` remap: paths
  stay the host's) or `network.outbound` (no direct network by design).

## Fork bombs: memory and pids caps

On Linux set both in production: `AGENT_SANDBOX_MEMORY` (`4g` is a reasonable start: agent CLIs
plus `go build` or `npm` can need a few GB) and `AGENT_SANDBOX_PIDS` (`1024` is a reasonable
start, not measured against real workloads: the limit counts threads, and a node CLI has a dozen or
more, a parallel `go build` many more). Either one contains a fork bomb:

* **Pids:** agentbox sets `RLIMIT_NPROC` in the sandbox's OCI spec, which gVisor enforces inside:
  `fork()` fails with `EAGAIN` and the run carries on.
* **Memory:** agentbox runs the sandbox in a systemd user scope (`MemoryMax`); past the cap the
  sandbox is killed within seconds and the host is left alone. Without a user session there is
  no scope, and a memory cap refuses to run rather than run uncapped.
* **Without either cap**, only the run timeout ends it, and until then it can take all of the
  host's memory.

macOS has neither limit (`RLIMIT_NPROC` there counts all of the user's processes), so both caps
are reported as unenforced there. `make sandbox-smoke` checks both caps through the runner on
Linux: a fork bomb under `AGENT_SANDBOX_MEMORY=512m` must be killed well before the iteration
timeout, and one under `AGENT_SANDBOX_PIDS=256` (no memory cap) must hit `EAGAIN` and let the
session complete, each with the host's available memory and the runner unharmed.

## Operations

* **Supervisor:** run `sandbox-supervisor --leases STATE_ROOT/sandbox/leases` as a separate
  process. It kills sandbox process groups past their deadline (even if the runner is alive), and
  reaps sandboxes whose lease expired *and* whose owner process is dead. It never reaps a live
  runner because an instance ID differs. Events: `sandbox.timeout`, `sandbox.orphan_reaped`,
  `sandbox.enforcement_lost`, `sandbox.teardown_incomplete`.
* **Events logged by the runner:** `sandbox.started|finished|rejected|enforcement_lost` and
  `sandbox.egress_allowed|egress_denied` (ids and names only, never env values, payloads or
  secrets).
* **Thread runs:** one active run per Thread, bounded queue; a lost lease cancels the run.

## Tests

* `make sandbox-smoke`: this host end to end, as a normal user: the conformance suite gated on
  its baseline, then the real runner with a fake `claude` probing reads, writes, egress (both
  modes on Linux), the model proxy, strict mode, the fork bombs (Linux) and codex on a ChatGPT
  login against a fake backend. CI runs it on macOS and Ubuntu 24.04 (`.github/workflows/sandbox.yml`).
* `make sandbox-agentbox`: the Linux run in an unprivileged Docker container, from any host.
* `go test ./internal/sandbox/...` (includes `e2e`: strict happy path, policy rejection, timeout,
  crash recovery via supervisor, export conflict quarantine, over the real adapter with a fake
  agentbox backend).

See [sandbox-testing.md](sandbox-testing.md) for the full test guide.
