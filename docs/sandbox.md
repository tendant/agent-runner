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
* `AGENT_SANDBOX_MEMORY` (e.g. `4g`), `AGENT_SANDBOX_CPUS` (cores, e.g. `2`) and
  `AGENT_SANDBOX_PIDS` (e.g. `1024`): caps for every run, overriding the policy's (Linux only, see
  [Fork bombs](#fork-bombs-memory-and-pids-caps)). Memory and CPU are set on a systemd user scope
  around each sandbox, so the runner's user needs a user manager that outlives logins
  (`loginctl enable-linger <user>`); without one, a run that asks for either is refused.
* `AGENT_SANDBOX_DISK` (e.g. `20g`): a cap on what each run's workspace, sandbox home and tmp
  hold together, overriding the policy's `disk_bytes` (Linux and macOS, see [Disk](#disk)).
* Measured use: on Linux, whenever a user scope works, each sandbox runs in one even without caps,
  and the run's `sandbox_closed` event records its highest peak memory, CPU time and OOM kills
  (`usage`) over its sandboxes, one per agent CLI launch. A launch that lost processes to the memory
  cap is also recorded as `resource_limit_hit` (`memory`, reason `oom_kill`). Measure first, then
  set caps from what runs actually use.
* `AGENT_SANDBOX_EGRESS_ALLOW`, `AGENT_SANDBOX_EGRESS_MODE`, `AGENT_SANDBOX_SERVICES` and `AGENT_SANDBOX_INJECT`: see [Network](#network).
* `AGENT_SANDBOX_READ_ALLOW`: see [What the sandbox can read](#what-the-sandbox-can-read).
* `AGENT_SANDBOX_ENV_ALLOW`: host env var names to pass in (comma-separated), for plain
  settings; model credentials are never passed while the model proxy is on. Credentials an
  agent needs for its tasks go in with `/env` instead (see [Secrets](#secrets)).
* `AGENT_SANDBOX_MODEL_PROXY` (default `true`) and `AGENT_SANDBOX_CODEX_LOGIN`: see
  [Claude credentials and config](#claude-credentials-and-config).
* `AGENT_SANDBOX_EVIDENCE`: a conformance report; only the capabilities it proves are claimed.
* `AGENT_SANDBOX_AUDIT_DIR`: a directory recording every run for agent-gate (agentbox's
  `audit` schema, JSON Lines): its policy snapshot (the resolved spec and what the backend
  enforces), lifecycle, and every egress and model-channel decision. Names and ids only, never
  credentials or payloads. Unset: `DATA_DIR/audit`; `off`: no record. With the sandbox on, a
  directory that cannot be opened rejects every run. One agent-runner per directory; it is
  read-denied to runs.
  With the sandbox off (`AGENT_SANDBOX=off`) the record is kept too: each run is on it as
  unsandboxed, as a policy snapshot with backend `none` and mode `off` (nothing enforced), then
  its start (agent, project, trace) and end. agent-gate lists the run and judges the sandbox's
  controls failed, instead of never hearing of it. A directory that cannot be opened is logged,
  and such runs go unrecorded.
  The record is kept as numbered segments (`00000000`, `00000001`, ...), as Prometheus keeps
  its write-ahead log: appended to the newest, never renamed or rewritten. agent-gate pulls it
  from the API, read-only and behind `API_KEY`: `GET /audit/segments` lists the segments,
  `GET /audit/segments/{name}` serves one (Range requests resume from an offset). Without
  `API_KEY` the API, and so the record, is reachable on loopback only.
  A session run by a runner process (`AGENT_EXECUTION_BACKEND=subprocess`, JOBS_DESIGN.md)
  records its runs in a directory of its own, `<AGENT_SANDBOX_AUDIT_DIR>-runners/<session>`,
  since a record directory has one writer: `GET /audit/runners` lists them, and each is served
  the same way under `/audit/runners/{session}/`. agent-gate does not pull these yet.
* `AGENT_NAME` (default `INSTANCE`): the agent's name, and for a job its project (the registered
  repo it runs on), are on each run's record (its `sandbox_created` or `sandbox_rejected` event),
  so agent-gate can filter and group runs by them. Sessions work across every registered repo
  and have no project.
* `AGENT_SANDBOX_AUDIT_SEGMENT`: one segment's size (`10m` when unset); a full segment is
  closed and the next started.
* `AGENT_SANDBOX_AUDIT_KEEP`: how many segments are kept, the newest (`10` when unset, `0` for
  all): about 100 MiB by default. agent-gate must read a segment before it is deleted; one it
  missed shows there as a gap in the record.
* `AGENT_SANDBOX_LEDGER_URL`: ledger-gate's base URL. Model channels whose upstream has the same
  scheme and host (`ANTHROPIC_BASE_URL`, `OPENAI_BASE_URL`, ...) tag every request with
  `Agentbox-Run-Id: <run id>`, so ledger-gate can link its cost records to runs, and
  `Agentbox-Request-Id: <the call's id>`, the id on the call's `model_forwarded` event. The proxy
  always drops either header the sandbox sends; other channels never carry them.
* `AGENT_SANDBOX_METERING`: how each model channel's calls are metered on the record
  (`provider=ledger|broker|none`, comma-separated). By default a channel to
  `AGENT_SANDBOX_LEDGER_URL` is `ledger` (ledger-gate's row is the evidence) and every other is
  `broker`: the proxy reads each response's token usage as it passes and records a `model_usage`
  event under the call's id, billed `subscription` for a Claude setup-token or ChatGPT login and
  `api` otherwise. `none` records the call only. Each run's policy snapshot declares the modes, and
  agent-gate expects the evidence each names (agent-gate's `metering-design.md`). A Chat
  Completions stream reports usage only when asked to, so the proxy sets
  `stream_options.include_usage` on each streamed request; the stream then ends with a usage
  chunk that has no choices.
* `AGENT_SANDBOX_MODEL_BUDGET_USD`: per run, what broker-metered calls may use, priced at list
  price by agentbox's price table (`pkg/agentbox/pricing`), subscription channels included, as a
  cap on runaway runs rather than a bill. A model the table lacks is charged at its provider's
  highest rates. Once the run has used it, the proxy refuses further calls (`model_limit_hit`,
  reason `usage`); calls already sent complete, so a run can end somewhat over. The policy
  snapshot records the budget and the table version. Ledger-metered calls are left to
  ledger-gate's budgets.
* `AGENT_SANDBOX_EGRESS_INSPECT=on`: the egress gateway reads every HTTP(S) request and records it
  (`http_request`: method, host, path, status, bytes, duration; never headers, query string or
  body). HTTPS is read through a certificate authority made for each run: its certificate, and a
  bundle of the system roots with it, are written to the run's tmp, and the sandbox's tools are
  pointed at them (`SSL_CERT_FILE`, `CURL_CA_BUNDLE`, `GIT_SSL_CAINFO`, `REQUESTS_CA_BUNDLE`,
  `PIP_CERT`, `CARGO_HTTP_CAINFO`, `NODE_EXTRA_CA_CERTS`). A tool that refuses the certificate fails
  that request; the host then passes through for the rest of the run (`egress_inspection`,
  `client_refused`). `AGENT_SANDBOX_EGRESS_INSPECT_EXCEPT` lists hosts never inspected. Off by
  default; restricted egress only. Design: agent-gate's `api-visibility-design.md`.
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
resolves names itself and allows ports 443 and 80 (`AGENT_SANDBOX_EGRESS_PORTS=443,80,...` to
change them). By default `permissive` allows **any public host**, and `strict` only a short list:
`api.anthropic.com`, `api.openai.com`, `api.deepseek.com`, `registry.npmjs.org`, `*.npmjs.org`,
`pypi.org`, `*.pypi.org`, `files.pythonhosted.org`, `github.com`, `api.github.com`,
`raw.githubusercontent.com` and `gitlab.com` (`strictModeApprovedHosts` in
`internal/sandbox/runtime`). `AGENT_SANDBOX_EGRESS_ALLOW=github.com,*.npmjs.org,...` replaces
either default with the listed hosts. A
`*.example.com` wildcard grants one label under the domain (`a.example.com`, not `example.com`
itself nor `a.b.example.com`). Named sets keep a list short: `@github`, `@npm`, `@pypi`, `@go`,
`@gitlab`, `@cloudflare`, `@anthropic`, `@openai`, `@deepseek` stand for their services' hosts
(`internal/sandbox/egresssets.go`, the one place to change them), e.g.
`AGENT_SANDBOX_EGRESS_ALLOW=@github,@npm,*.variablename.com`. An unknown set is refused at startup.
Each run's policy snapshot records what the sets it used meant (`egress_sets`), so the audit
record says which hosts were granted. agent-gate's outbound hosts page lists what each agent
actually reached, to start a list from. `AGENT_SANDBOX_EGRESS_ENFORCE=report` tries a list before
enforcing it: a host the list does not grant is reached anyway and recorded as outside it
(`network_allowed`, reason `outside_allowlist`; the session log's Network table gains an
"Outside the allowlist" column), so what enforcing would refuse is on record without breaking
runs. Ports and internal addresses are refused all the same. `enforce`, the default, refuses
them; any other value rejects every run. Private, loopback, link-local and metadata addresses are
refused either way, also when a name resolves to one. git over SSH (port 22) is refused unless
`AGENT_SANDBOX_EGRESS_PORTS` lists 22: use HTTPS remotes. How connections reach the gateway,
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

**Services.** A database, a cache or an internal API is not a host to open in the allowlist:
`AGENT_SANDBOX_SERVICES=pg=10.0.0.5:5432,redis=127.0.0.1:6379` names fixed upstreams a run may
reach, by any TCP protocol, outside the egress gateway. Internal and loopback addresses are
included, since each grant is one host:port and opens nothing else. Each run gets a listener per
service and relays each connection to that one upstream:
* **Linux (gVisor):** a Unix socket mounted into the sandbox, served at `127.0.0.1:<the upstream's
  port>` inside it by the forwarder (a port from 15000 when that one is taken).
* **macOS (Seatbelt):** a port on the host's loopback that the sandbox may connect to.

The agent connects at `$AGENT_SERVICE_<NAME>_ADDR` (also `_HOST` and `_PORT`), which its prompt
names. Unsandboxed, these hold the upstream's own address. A login the service needs goes in the
agent's environment (`/env secret`), as with any other credential.

The services are in each run's policy (`network.services`, so on its `policy_snapshot`). A lower
policy layer may drop some but never add or repoint one, and a run with no network (egress `none`)
has none; an eval replay has none either. Each connection is a `network_allowed` event with
`service`, `destination`, `port` and `protocol: tcp`. The upstream must still be reachable from
the runner's host. A malformed `AGENT_SANDBOX_SERVICES` rejects every sandboxed run.

**Credentials added for you.** The agent can call an API without ever holding its token. The
token is agent-runner's own, like the model keys, not the agent's:
```
/config secret GITHUB_API_TOKEN ghp_... GitHub API, added for the agent
AGENT_SANDBOX_INJECT=api.github.com:Authorization=Bearer $GITHUB_API_TOKEN
```
Rules are separated by `;`, each `host:Header=value`. The host is a name or `*.wildcard`, and the
value names agent-runner's secrets as `$NAME` or `${NAME}`. Each name is looked up in
`/config secret`'s file (`STATE_ROOT/runner-secrets.json`, which the sandbox cannot read; read at
each run, so a new one applies from the next session), else in agent-runner's own environment
(a deployment's, e.g. sops). The agent's `/env` is never consulted. What a sandboxed run gets:
* The run's egress gateway sets that header on each HTTPS request to the host, over any the agent
  sends. The agent's prompt lists the host as signed in for it, without naming the credential.
* The agent's own secrets are unaffected: every `/env secret` is in its environment, as always.
  To give the agent the token as well, add it with `/env secret` too.
* An injected value is redacted from what the agent says, as its own secrets are, since a service
  can echo it back.
* A credential never goes out over plain HTTP, and never to a host the gateway does not inspect.
* Injection turns inspection on for just those hosts when `AGENT_SANDBOX_EGRESS_INSPECT` is off.
  A host in `AGENT_SANDBOX_EGRESS_INSPECT_EXCEPT` never gets the credential, and neither does one
  whose client refuses the run's certificate.
* The hosts must be in the egress allowlist; agent-runner warns at start-up when one is not.

On the record, the run's policy lists each rule (`network.inject`: host, header and credential
name), and each `http_request` the gateway authenticated names its `credentials`. Values are
never recorded.

A rule whose secret agent-runner lacks is skipped (`sandbox.inject_unavailable`), and so is every
rule when inspection is unavailable on the host: the agent's requests go out unauthenticated.
Unsandboxed runs have no gateway, so no injection; they have only the agent's `/env`. An eval
replay and agent-runner's own model fallback get no credentials. A malformed
`AGENT_SANDBOX_INJECT` rejects every sandboxed run. `/config secrets` lists agent-runner's
secrets by name, and `/config unset NAME` removes one.

Limit: a service that echoes request headers back (an echo or debug endpoint) shows the agent
the credential. Its output is still redacted on record.

**What it did.** Each run tallies the gateway's decisions by host. The first time a run reaches
or is refused a host, the runner logs `sandbox.egress_allowed` or `sandbox.egress_denied` with
the host (names only, never payloads), and the session's audit log (`LOGS_ROOT`) gets a
**Network** table: every host, refused first, with how many connections were allowed and
refused (at most 500 hosts a run).

**Nothing else.** On macOS the profile denies every socket except connections to the run's own
loopback ports, and Mach lookups of the keychain (`com.apple.SecurityServer`) and DNS
(`com.apple.mDNSResponder`) services; a sandboxed process cannot listen or signal anything
outside its sandbox.

## Git

Repos come from the registry (`/repo`, REPOS_DESIGN.md), each logging in with the user's
`/env secret` it names. The runner fetches them into its cache and checks them out into the
workspace before a session; the agent works and commits inside; after the session the runner
pushes every registered repo the agent committed to (`syncWorkspaceRepos`), except read-only ones
(`push=false`). When another session moved the branch it rebases the commits itself, and
only when that rebase conflicts does it hand the agent an iteration to resolve the markers (it
pushes afterwards); work that still cannot be merged goes to `agent-rescue/<session>`. Every
session is told this in its prompt, so it commits and does not push (the token is in its
environment, as an `/env secret`, but the runner is the one place pushes happen). The runner's own
git commands ignore the hooks and config an agent could plant (`internal/gitsafe`), and never
use a `.git/config` the agent wrote: each repo's config is saved after the runner sets the
workspace up, out of the sandbox's view, and put back before every host-side git step (push,
rebase). Nothing goes back from a workspace to the cache, which only fetches. A repo the runner
did not set up (one the agent cloned or made) is never pushed; the session says so in a warning.

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

## Memory

The memory dir is read-only in the sandbox (its `.git` hidden). A sandboxed agent is pointed at a
copy instead, in its sandbox home (`{{MEMORY_DIR}}` is the copy), and when the session ends the
runner merges back, under the memory lock and before the daily log, curation and push:

* **Saved:** changes to top-level lowercase `.md` files, new ones included (at most 10 per
  session, each growing by at most 64 KiB); `lessons.md` only takes appends.
* **Not saved:** `agent.md`, `prompt.md`, upper-case files (`HEARTBEAT.md`, `SOUL.md`, ...),
  daily logs, deletions, subdirectories, links and binary files. A hijacked agent cannot rewrite
  the instructions every later session starts from, nor pull a host file into memory (which is
  pushed to its remote) through a link.
* **Concurrent writers:** a file another session or the curator changed meanwhile takes this
  session's change only when it is a pure append (added on top); a rewrite is refused, not lost.

Whatever is refused shows as a session warning (`memory changes not saved: ...`) and in the
session's audit log. Unsandboxed sessions work the same way, on a copy next to their workspace.

## Secrets

Two kinds, kept apart:

* **agent-runner's own** (`.env`, `/config set`): model keys, `MEMORY_GIT_TOKEN`, bot tokens,
  `API_KEY`. Never in the sandbox: the model proxy uses the model keys for the agent. (Repos
  log in with the `/env secret` each names, not with these.)
* **The agent user's** (`/env secret NAME VALUE [description]`, kept in `DATA_DIR/secrets.json`; plain settings, `/env set`, in `DATA_DIR/env.json`,
  0600): what agents need for their tasks, such as `GITEE_TOKEN`. In every session's environment,
  sandboxed or not, and named with their descriptions (never values) in the prompt. They are not
  hidden from the agent: give each the least access its task needs.

`/env` refuses any name agent-runner reads itself (its settings, `GIT_*`, `AGENT_*`, model
provider variables, `PATH`, proxies, ...), so one kind cannot become the other; use the names
tools expect (`GITEE_TOKEN`, `GH_TOKEN`, `STRIPE_API_KEY`). The file is denied to the sandbox like
`.env`; `/env` shows secrets by name and description only. A value may span lines (an SSH private
key: `/env secret NAME [description]`, the key on the lines after); a repo with an `ssh://` or
`git@host:` URL logs in with it, written to a 0600 file next to `secrets.json` (also hidden from
the sandbox) and given to ssh with none of the host's keys, agent or ssh config.

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
  * **DeepSeek** (`pi`): `DEEPSEEK_API_KEY` (`Authorization: Bearer`), to `DEEPSEEK_BASE_URL` or
    `https://api.deepseek.com`; paths `/chat/completions`, `/models` (with or without `/v1`).
    pi's built-in `deepseek` provider gets the proxy as its `baseUrl` and a run token in
    `DEEPSEEK_API_KEY`. More OpenAI-compatible providers are a row each in `CompatProviders`
    (`internal/sandbox/runtime/model.go`).
  * **pi** gets the run's `models.json` (the seed's, with each proxied built-in provider's `baseUrl`
    set to its proxy) and run tokens in env (`ANTHROPIC_OAUTH_TOKEN` for a setup-token).

  Proxies allow 600 requests a minute and 64 MiB a request and stop with the run. They need no
  network: on Linux the sandbox reaches them over Unix sockets mounted into it, served at
  `http://127.0.0.1:4801` (Anthropic) and `:4802` (OpenAI) by a forwarder inside (agent-runner's
  own binary, `__agent-runner-fwd`); on macOS at their ports on the host's loopback. So they work
  with egress `none` too.
* **codex and pi config:** `CODEX_HOME` (`~/.codex`) and `PI_CODING_AGENT_DIR` (`~/.pi/agent`, sessions
  in `~/.pi/sessions`) are in the sandbox home too, seeded from `~/.codex` / `~/.pi/agent` (or
  `agent-home/`) with `AGENTS.md`, `prompts/`, `skills/` and pi's `settings.json` and `mcp.json` (its
  MCP servers, as claude gets its `mcpServers`), never `auth.json` or pi's `mcp-auth.json`.
* **Config:** `CLAUDE_CONFIG_DIR` is `/home/agent/.claude` (the thread's sandbox home: writable,
  kept across a task's turns so `--resume` works). Before each run it is refreshed from
  `~/.claude` (or `agent-home/claude` with `AGENT_ISOLATED=true`), copying only `settings.json`,
  `CLAUDE.md`, `agents/`, `commands/`, `skills/` (symlinks resolved) and the `mcpServers` and
  `hasCompletedOnboarding` keys of `.claude.json`. Never credentials, other projects'
  transcripts, history or todos.

## Policy

`SandboxSpec` (version 1; unknown fields or versions fail closed). Layers
`system -> bot -> agent -> workspace -> run` may only tighten: unset inherits, widening is an
error naming the layer, deny lists and requirements only grow. There is no secrets field: the
agent user's secrets are `/env secret`'s ([Secrets](#secrets)). Filesystem uses logical roots `/workspace`, `/home/agent`, `/tmp`.
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
* **The disk cap is polled** (see [Disk](#disk)): a burst between polls can overshoot it, and
  it does not count what an agent writes outside its workspace, home and tmp (nothing else is
  writable to it).
* **In-place runs:** the sandbox writes the task workspace directly. The validated-diff export with
  durable quarantine (`internal/sandbox/workspace`) is implemented and tested but not in the run path.
* **The model proxy covers Anthropic, OpenAI and DeepSeek** for claude, codex and pi (DeepSeek:
  pi). opencode, and other providers (OpenRouter, Gemini, ...), still need their keys through
  `AGENT_SANDBOX_ENV_ALLOW`, which puts the real key in the sandbox, and egress to their APIs.
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

## Disk

`AGENT_SANDBOX_DISK` (or the policy's `disk_bytes`) caps the disk a run's writable dirs take
together: its workspace (the repo checkouts included, and for a task the workspace its turns
share), its sandbox home (per thread) and its tmp. No backend enforces `resource.disk`; the
runner does, on Linux and macOS alike, so a strict run with a cap starts and its check records
`resource.disk` as enforced, with a caveat that it is polled.

The runner walks the dirs every 5 seconds at most often, and at least ten times as long as the
last walk took (a large tree is walked at most once a minute). It counts allocated blocks, so a
sparse file counts what it holds, and a hard-linked file once. A run past the cap is ended like a
lost lease: its sandboxes are closed, the record gets `resource_limit_hit` (`disk`), the runner
logs `sandbox.disk_limit`, and the session fails with "sandbox disk limit exceeded: the run's
files take …, over its …" (a one-shot job with `SANDBOX_LIMIT`).

Size it for the largest checkout plus build output: `20g` is a reasonable start for repos with
`node_modules` or Go module caches in the home.

## Operations

* **Supervisor:** run `sandbox-supervisor --leases STATE_ROOT/sandbox/leases` as a separate
  process. It kills sandbox process groups past their deadline (even if the runner is alive), and
  reaps sandboxes whose lease expired *and* whose owner process is dead. It never reaps a live
  runner because an instance ID differs. Events: `sandbox.timeout`, `sandbox.orphan_reaped`,
  `sandbox.enforcement_lost`, `sandbox.teardown_incomplete`.
* **Events logged by the runner:** `sandbox.started|finished|rejected|enforcement_lost|disk_limit` and
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
