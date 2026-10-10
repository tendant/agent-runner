# Agent sandbox

agent-runner can run every agent CLI (pi, claude, codex, opencode) and all of the tools
it starts inside one isolation boundary, provided by [isobox](https://github.com/can1357/isobox)
(macOS Seatbelt, Linux gVisor). **Policy lives in agent-runner; isobox only enforces.**

> If the backend cannot enforce a required property, a `strict` run does not start.

## Modes

`AGENT_SANDBOX` = `off` (default) | `permissive` | `strict`

| Mode | Behaviour |
|---|---|
| off | agent CLIs run on the host exactly as before |
| permissive | run in the sandbox; unenforceable properties are logged (`sandbox.started` detail `unenforced: ...`) |
| strict | any required capability the backend cannot enforce rejects the run before any work |

An invalid sandbox configuration rejects every run; it never falls back to the host.

Other settings: `AGENT_SANDBOX_BACKEND` (`""`|`seatbelt`|`gvisor`), `AGENT_SANDBOX_ISOBOX`
(path to a pinned isobox build), `AGENT_SANDBOX_POLICY` (system-layer `SandboxSpec`: a path to a
JSON file, or the JSON itself when it starts with `{`; fields it leaves unset keep the default
policy's values, so `{"version":1,"network":{"egress":"none"}}` still grants the workspace,
home and tmp; a field it sets replaces the default's), `AGENT_SANDBOX_MEMORY` (memory cap for
every run, e.g. `4g`; overrides the policy's `resources.memory_bytes`), `AGENT_SANDBOX_PIDS`
(process and thread cap for every run, e.g. `1024`; overrides the policy's `resources.pids`; see
[Fork bombs](#fork-bombs-memory-and-pids-caps)), `AGENT_SANDBOX_EVIDENCE` (conformance report), `AGENT_SANDBOX_ENV_ALLOW`
(comma-separated host env var names to pass in; model credentials are never passed while the model
proxy is on), `AGENT_SANDBOX_MODEL_PROXY` (default `true`; see below).

## How a run is confined

```
Resolve policy -> Check (reject in strict) -> Thread lease -> sandbox lease
  -> run ctx carries SandboxLauncher -> planner / session / reviewer CLIs -> Finish
```

* The whole CLI process tree is the sandboxed unit (the CLIs run their own tools), so
  there is no per-tool shim and no command allowlist.
* Sandbox state: `STATE_ROOT/sandbox/{leases,home/<thread>,runs/<run>/tmp}`. Lease updates
  serialize on one `leases/.lock`; a released lease leaves no file behind.
* The runner's own files are read-denied in every run (`sandboxPrivatePaths`): its `.env`,
  `.env.<instance>` and `DATA_DIR/.env.local`, and `STATE_ROOT`, `TMP_ROOT`, `LOGS_ROOT`,
  `OUTPUTS_ROOT`, `REPO_CACHE_ROOT`, except the directories holding the run's own workspace,
  home and tmp (other sessions' workspaces, other threads' homes, leases and the session
  journal stay denied). The memory dir and uploads stay readable: prompts point the agent at
  memory, and uploaded files reach it by path.
* `HOME`/`TMPDIR` point at those private directories; the environment is an allowlist.
* Runner-side git (`internal/gitsafe`) is hardened against hooks/fsmonitor planted in the workspace.

## Claude credentials and config

The sandbox hides the host's `claude login` (`~/.claude`, and on macOS the Keychain), and copying
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
  * **OpenAI with a ChatGPT subscription** (`codex` only): set `AGENT_SANDBOX_CODEX_LOGIN` to an
    `auth.json` made for the runner alone, e.g.
    `CODEX_HOME=$DATA_DIR/state/codex-login codex login --device-auth`, then
    `AGENT_SANDBOX_CODEX_LOGIN=$DATA_DIR/state/codex-login/auth.json`. Not your own `~/.codex`
    login: refreshing rotates the refresh token, so two holders break each other. The runner
    keeps the login (`internal/sandbox/codexlogin`) and refreshes it as codex would (within 5
    minutes of expiry), and the proxy forwards to `https://chatgpt.com/backend-api/codex`
    (`/responses`, `/responses/compact`, `/models`) with the access token and `ChatGPT-Account-ID`.
    codex's config names the provider `OpenAI` (codex keys its OpenAI request features on that
    name) and turns websockets off. The login's directory is hidden from the sandbox, so it must be
    a directory of its own (not `/` or the home directory; the runner refuses those). It replaces
    `OPENAI_API_KEY` as the OpenAI channel, so pi gets no OpenAI provider then. Checked with
    the real codex CLI (0.162) against a fake backend
    (`AGENT_E2E_REAL_CODEX=1 go test ./internal/sandbox/runtime -run RealCodex`); against
    chatgpt.com it is untested, including whether Cloudflare challenges the proxy.
  * **pi** gets the run's `models.json` (the seed's, with each proxied built-in provider's `baseUrl`
    set to its proxy) and run tokens in env (`ANTHROPIC_OAUTH_TOKEN` for a setup-token).

  Proxies allow 600 requests a minute and 64 MiB a request and stop with the run. They listen on
  127.0.0.1 on macOS (Seatbelt shares the host's network) and on the host's address on Linux
  (gVisor has its own loopback). They need outbound network: with egress `none` or `restricted`
  isobox cannot open them a port, so none start and a setup-token is passed to `claude` directly
  (as with `AGENT_SANDBOX_MODEL_PROXY=false`); the model APIs are unreachable then anyway.
* **codex and pi config:** `CODEX_HOME` (`~/.codex`) and `PI_CODING_AGENT_DIR` (`~/.pi/agent`, sessions
  in `~/.pi/sessions`) are in the sandbox home too, seeded from `~/.codex` / `~/.pi/agent` (or
  `agent-home/`) with `AGENTS.md`, `prompts/`, `skills/` and pi's `settings.json`, never `auth.json`.
  `~/.pi` and opencode's `~/.local/share/opencode` are read-denied like `~/.claude`.
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
Egress modes: `none`, `restricted` (hosts, single-label `*.wildcards`, CIDRs, `@sets`), `outbound`.

Default system policy: workspace/home/tmp writable, outbound network without listeners.

## Capabilities and evidence

Each capability (`internal/sandbox/capability.go`) lists conformance tests; a backend may claim it
only if all pass. Tests not implemented yet count as unproven.

```
sandbox-conformance --isobox ./isobox --backend seatbelt --report seatbelt-macos.json --manifest m.json
AGENT_SANDBOX_EVIDENCE=seatbelt-macos.json AGENT_SANDBOX=strict ...
```

## Known gaps (be honest in strict deployments)

* **Restricted egress is not enforceable yet.** isobox has no allowlist; the egress gateway
  (`internal/sandbox/egress`) exists but no backend forces the sandbox through it. Restricted
  policies translate to no network and report `network.restricted_egress` as a gap.
* **Seatbelt** has no CPU/memory/pids limits and blocks loopback with `net=disable`.
* **No disk quota** from isobox: `WatchDisk`/`LimitedWriter` exist but are not wired into runs.
* **Reads are broad** (host files minus the user's credential directories and the runner's own
  files); `fs.read` scoping is not used, so other projects on the host are readable. Entries
  created under `TMP_ROOT` or `STATE_ROOT` after a run starts (a session started meanwhile) are
  not denied to it.
* **In-place runs:** the sandbox writes the task workspace directly. The validated-diff export with
  durable quarantine (`internal/sandbox/workspace`) is implemented and tested but not in the run path.
* **The model proxy covers Anthropic and OpenAI** for claude, codex and pi, and only with outbound
  network (isobox cannot expose one port into a sandbox without network). opencode and other
  providers (DeepSeek, ...) still need their keys through `AGENT_SANDBOX_ENV_ALLOW`. On Linux the proxy's port is on the host's address, reachable
  from the host's network for the run's lifetime, behind the per-run token.
* The fast-LLM executor fallback (analyzer, curator, and the planner outside a session, used when
  no LLM API key is set) runs the agent CLI in a sandbox run of its own, sharing one sandbox
  thread (`llm-fallback`). Before, it ran on the host: `make sandbox-smoke` caught it writing
  outside the workspace.
* No `sandbox.oom`/`fs_denied`/`egress_denied` events (isobox gives no denial signal).
* **Conformance on real hosts** (isobox `c7bbd19`): Seatbelt on macOS proves 8 capabilities
  but not `process.containment`, so `strict` cannot run there. gVisor (`runsc` 20261005.0, Linux
  arm64 in a privileged container) proves 11, including `process.containment`,
  `process.cross_run_isolation` and `resource.pids` (through the nproc shim, see below); not
  `fs.virtual_paths` (no `/workspace` remap on either backend).
* **The pids cap uses `RLIMIT_NPROC` only with `AGENT_SANDBOX_BACKEND=gvisor`.** With the
  backend left to isobox (`""`) the cap goes to isobox's `--pids`, which on gVisor caps the
  Sentry's host threads instead (see below).
* **gVisor host requirements:** root, cgroup v2, `runsc` new enough for `runsc features`
  (oci-seccomp; 20250106.0 is too old), and `ip`, `sysctl` and `iptables` (isobox builds the
  sandbox's network namespace with them).
* **On gVisor, every parent of `DATA_DIR`, `TMP_ROOT` and `STATE_ROOT` must be owned by root
  or searchable by others.** Inside, the agent is uid 0 without capabilities, so a directory not
  owned by root only gives it the "other" bits. A runner installed under a private home
  (`/home/runner`, mode 0750) cannot start a single process there (`failed to find initial
  working directory ... permission denied`). Every run is rejected with a message naming the
  directory and the fix (`chmod o+x`, which allows entering without listing). This applies whenever
  isobox resolves to gVisor, including with `AGENT_SANDBOX_BACKEND` unset on Linux.

## Fork bombs: memory and pids caps

Set both in production on gVisor: `AGENT_SANDBOX_MEMORY` (`4g` is a reasonable start: agent CLIs
plus `go build` or `npm` can need a few GB) and `AGENT_SANDBOX_PIDS` (`1024` is a reasonable
start, not measured against real workloads: the limit counts threads, and a node CLI has a dozen or
more, a parallel `go build` many more). Either one contains a fork bomb. The memory cap kills the
sandbox; the pids cap makes `fork()` fail with `EAGAIN` and the run carries on.

* **How the pids cap works on gVisor.** gVisor enforces `RLIMIT_NPROC` inside the sandbox
  (runsc 20261005.0, although google/gvisor#169 is still open), but a limit set on the host
  process does not reach the sandbox, so the adapter starts the agent CLI through the runner's
  own binary as a shim (`internal/sandbox/isobox/nproc.go`): `<runner> __agent-runner-nproc N --
  <cli> ...` sets the soft and hard limits to N and execs the CLI. The sandbox has no
  capabilities, so the agent cannot raise the hard limit. The sandbox runs as uid 0 without
  capabilities and so cannot load the runner's binary from a private directory (a 0750 home), so
  each launch runs a fresh copy in the run's tmp dir (a copy, never a hard link: the sandbox can
  write there). An agent that replaces that copy mid-run escapes only the pids cap of its later
  launches, not the sandbox.
* **isobox's `--pids` does not limit sandboxed processes**, so the adapter does not use it on
  gVisor. It sets `pids.max` on the sandbox's host cgroup, which counts the Sentry's own host
  threads (an idle run already uses ~29). A small value stops runsc from starting (8 or 16 do; 32
  works), and under a fork bomb `--pids 256` makes runsc exit with status 2 within a second;
  `fork()` never fails with `EAGAIN`. The sandbox cannot mount cgroupfs itself (`permission
  denied`).
* **The memory cap contains a fork bomb** too: the bomb uses up memory, so the sandbox is killed
  within seconds and the host is left alone. When the run's main process exits, gVisor also kills
  every process left in the sandbox.
* **Without either cap**, only the run timeout ends it, and until then it can take all of the
  host's memory.

Measured on gVisor in Docker (isobox `c7bbd19`, infinite fork bomb with the main process kept
alive, 20 s isobox timeout, host `MemAvailable` sampled every 0.5 s):

| Limit | Outcome | Host `MemAvailable` |
|---|---|---|
| none | exit 137 after ~17 s, before the timeout: the host ran out of memory | 2.9 GB → 0 |
| `--memory 1g` | exit 137 in ~2.3–3.2 s | 6.3 GB → 5.1 GB at the lowest |
| `--pids 256` | runsc exits 2 in ~0.6 s | unchanged |
| `RLIMIT_NPROC` 256 | `fork()` fails with `EAGAIN`; sandbox still running at 12 s | 3.0 GB → 2.8 GB |

`make sandbox-gvisor` checks both through the runner: a fork bomb under
`AGENT_SANDBOX_MEMORY=512m` must be killed well before the iteration timeout, and one under
`AGENT_SANDBOX_PIDS=256` (no memory cap) must hit `EAGAIN` and let the session complete, each with
the host's available memory and the runner unharmed. Seatbelt (macOS) has neither limit
(`RLIMIT_NPROC` there is per user across the whole host), so both caps are reported as unenforced.

## Operations

* **Supervisor:** run `sandbox-supervisor --leases STATE_ROOT/sandbox/leases` as a separate
  process. It kills sandbox process groups past their deadline (even if the runner is alive), and
  reaps sandboxes whose lease expired *and* whose owner process is dead. It never reaps a live
  runner because an instance ID differs. Events: `sandbox.timeout`, `sandbox.orphan_reaped`,
  `sandbox.enforcement_lost`, `sandbox.teardown_incomplete`.
* **Events logged by the runner:** `sandbox.started|finished|rejected|enforcement_lost` (ids and names only,
  never env values or secrets).
* **Thread runs:** one active run per Thread, bounded queue; a lost lease cancels the run.

## Tests

`make sandbox-smoke` and `make sandbox-gvisor` check a real host end to end (see
[sandbox-testing.md](sandbox-testing.md)). One gVisor run in Docker once failed the probe's
internet check and the next three passed; if it recurs, look at isobox's NAT setup.

See [sandbox-testing.md](sandbox-testing.md) for the full test guide (automated, smoke, real-host conformance, red-team checklist).

`go test ./internal/sandbox/...` (includes `e2e`: strict happy path, policy rejection, timeout,
crash recovery via supervisor, export conflict quarantine). `ISOBOX_BIN=... ISOBOX_BACKEND=seatbelt
go test -run Real ./internal/sandbox/isobox` prints the offline plan for a real isobox.
