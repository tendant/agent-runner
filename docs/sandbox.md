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
home and tmp; a field it sets replaces the default's), `AGENT_SANDBOX_EVIDENCE` (conformance report), `AGENT_SANDBOX_ENV_ALLOW`
(comma-separated host env var names to pass in, e.g. a model API key; interim until the model proxy is wired).

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
  `CLAUDE_CODE_OAUTH_TOKEN` in the runner's env. It is passed to the `claude` CLI only
  (`SandboxLauncher.CLIEnvAllow`), never to other CLIs. An API key works too, listed in
  `AGENT_SANDBOX_ENV_ALLOW`. With neither, a claude run fails before it starts, saying so.
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
* **Model proxy** (`internal/sandbox/proxy`) is implemented but not wired; provider keys still
  enter via `AGENT_SANDBOX_ENV_ALLOW`.
* Memory-curation and conversation-analyzer CLI fallbacks still run on the host.
* No `sandbox.oom`/`fs_denied`/`egress_denied` events (isobox gives no denial signal).
* **gVisor and Seatbelt enforcement are unverified by this repo's suite on real hosts yet.**

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

See [sandbox-testing.md](sandbox-testing.md) for the full test guide (automated, smoke, real-host conformance, red-team checklist).

`go test ./internal/sandbox/...` (includes `e2e`: strict happy path, policy rejection, timeout,
crash recovery via supervisor, export conflict quarantine). `ISOBOX_BIN=... ISOBOX_BACKEND=seatbelt
go test -run Real ./internal/sandbox/isobox` prints the offline plan for a real isobox.
