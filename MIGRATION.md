# Migrating to v0.1.0

v0.1.0 is a systematic redesign. **Every legacy `.env` keeps working** — old
variable names are read as aliases and old semantics are auto-detected, with a
one-time startup warning naming the replacement. This guide covers what
changed, who needs to act, and how to move to the new names at your own pace.

**TL;DR for most deployments:** upgrade the binary, restart, read the startup
warnings, act on them when convenient. Nothing breaks on day one.

---

## 1. Data directory default moved (`DATA_DIR`)

**Before:** all mutable state (repo-cache/, runs/, workspaces/, logs/, memory/,
.env.local) defaulted to the **process working directory**.

**After:** the default is **`~/.agent-runner`** (or `~/.agent-runner/<instance>`
when `INSTANCE` is set).

**Auto-migration:** if `DATA_DIR` is unset and the working directory contains
state from the old layout (a non-empty `repo-cache/`, `runs/`, or
`workspaces/`), that layout keeps being used with a startup warning:

```
WARN config: found legacy data layout in the working directory; using it for
this run — set DATA_DIR=. to keep state here permanently, or move
repo-cache/, runs/, workspaces/ etc. to ~/.agent-runner
```

**Action (pick one):**

- Keep state where it is: add `DATA_DIR=.` to `.env`. The warning stops.
- Adopt the new default: stop the server, move the state dirs, restart:

  ```bash
  mkdir -p ~/.agent-runner
  mv repo-cache runs workspaces logs memory tmp outputs uploads .env.local ~/.agent-runner/ 2>/dev/null
  ```

**Docker deployments:** no action — `DATA_DIR=/data` is explicit.

---

## 2. Model tiers renamed and flipped (`AGENT_MODEL`)

**Before:** `AGENT_MODEL` was the *fast* tier (planning, chat responses) and
`AGENT_REASONING_MODEL` was the model that actually did the work at the agent
CLI — inverted from what most people expect.

**After:** `AGENT_MODEL` is **the model doing real work**. The optional cheap
tier is `AGENT_FAST_MODEL` (defaults to `AGENT_MODEL` when unset). Both accept
the combined `provider/model` form:

```bash
AGENT_MODEL=deepseek/deepseek-v4-pro
AGENT_FAST_MODEL=deepseek/deepseek-v4-flash   # optional
```

**Legacy mode:** when `AGENT_REASONING_MODEL` or `AGENT_REASONING_PROVIDER` is
set, the **old meaning is preserved exactly** (`REASONING_*` = work model,
`AGENT_MODEL` = fast tier) and a deprecation warning is logged. An old `.env`
behaves identically to before.

**Action:** replace the pair when convenient:

| Old | New |
|---|---|
| `AGENT_REASONING_MODEL=deepseek-v4-pro` | `AGENT_MODEL=deepseek/deepseek-v4-pro` |
| `AGENT_MODEL=deepseek-v4-flash` (as fast tier) | `AGENT_FAST_MODEL=deepseek/deepseek-v4-flash` |
| `AGENT_REASONING_PROVIDER=` / `AGENT_PROVIDER=` | usually unnecessary — the `provider/` prefix covers it |

> **Caution:** if your `.env` sets **only** `AGENT_MODEL` (no `REASONING_*`
> vars), its meaning changes from "fast tier" to "work model". For the
> opencode default setup this is what you want (your chosen model now actually
> reaches the CLI); if you relied on `AGENT_MODEL` being planning-only, add an
> explicit `AGENT_FAST_MODEL`.

---

## 3. Fast-LLM slot renamed (`ANALYZER_*`, `PLANNER_*` → `FAST_LLM_*`)

One config slot now backs the conversation analyzer, the planner, and the
memory curator:

| Old (still read, warns) | New |
|---|---|
| `ANALYZER_PROVIDER` | `FAST_LLM_PROVIDER` (or `provider/` prefix in the model) |
| `ANALYZER_MODEL` | `FAST_LLM_MODEL` |
| `ANALYZER_API_KEY`, `PLANNER_API_KEY` | `FAST_LLM_API_KEY` |
| `ANALYZER_BASE_URL`, `PLANNER_BASE_URL` | `FAST_LLM_BASE_URL` |
| `ANALYZER_TIMEOUT_SECONDS` | `FAST_LLM_TIMEOUT_SECONDS` |

When unset, the slot follows `AGENT_FAST_MODEL` → `AGENT_MODEL`, so most
deployments need none of these.

---

## 4. Previously-dead memory knobs are now live

These variables existed before but did nothing. They now work — check any
values you may have set long ago:

- `AGENT_MEMORY_DAYS` (default 7) — days of daily session logs folded into
  every prompt as a `## Recent Sessions` section. Was write-only before.
- `AGENT_MEMORY_CHAR_CAP` (default 12000) — memory content per prompt is now
  actually bounded (prompt-side only; files on disk are never modified). A
  forgotten low value will now truncate visibly — trims log a
  `memory budget` warning.

New, opt-in:

- `AGENT_MEMORY_CURATION_ENABLED=true` — post-session LLM pass that distills
  lessons into `lessons.md` and compacts over-budget memory files (uses the
  fast-LLM slot; one small LLM call per session).
- `MEMORY_GIT_TOKEN` / `MEMORY_GIT_SSH_KEY` now fall back to
  `GIT_TOKEN` / `GIT_SSH_KEY` — single-host setups can delete the duplicates.

---

## 5. `/set` now applies everything immediately

Previously only `AGENT_CLI`, `AGENT_PROVIDER`, `AGENT_MODEL`, and
`AGENT_MAX_TURNS` took effect live; everything else silently waited for a
restart. Now every `/set KEY VALUE` reloads the full config (with the same
parsing as startup) and rebuilds the executor and LLM clients.

**Action:** none — but workflows that assumed "set it, restart later" can drop
the restart. Directory paths and bot tokens still need a restart (components
built at startup keep their original roots/connections).

---

## 6. Removed: the `/migrate` chat command

`/migrate` converted pre-2026 memory files (`MEMORY.md`, `USER.md`,
`IDENTITY.md`, `SOUL.md`) to the per-topic layout. If you still have those
files, run `/migrate` once on v0.0.x **before** upgrading, or split them by
hand into `user_preferences.md` / `project_summary.md` / `decisions.md` /
`agent.md`.

---

## 7. No action needed (renames without env changes)

- **Scheduler:** internal names consolidated (`internal/runner` →
  `internal/scheduler`, `RunnerConfig` → `SchedulerConfig`). The env vars were
  already `SCHEDULER_*` and are unchanged.
- **Package layout:** `internal/api` split into `api` / `chatcmd` /
  `execution` / `clisetup`. Only relevant if you import agent-runner as a
  library — update import paths accordingly.
- **HTTP API and chat commands:** unchanged (minus `/migrate`).

---

## Verifying an upgrade

1. Start the server and read the log: each legacy variable in use logs one
   `deprecated env var in use` warning naming its replacement; a legacy data
   layout logs the `DATA_DIR` warning. **No warnings = fully migrated.**
2. `agent configured cli=... model=... fast_model=...` in the startup log
   shows which model reaches the CLI — confirm `model=` is your work model.
3. Run `/status` (chat or `POST /agent {"message": "/status"}`) — should show
   the CLI installed and ready.
4. Run a small real session and check `memory/` afterwards for the daily log
   entry (and `lessons.md` if curation is enabled).
