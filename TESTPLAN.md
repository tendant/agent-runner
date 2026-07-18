# Local Test Plan — Stateless Agent with Git Memory

Tests the full command surface needed to set up, run, and maintain a stateless
agent whose only persistent state lives in git-backed memory.

Run automated tests with:
```
go test -race ./...
```

Manual steps below use `agent-cli` against a running server.

---

## 1. Initial Setup (`/config`, `/status`, `/bootstrap`, `/set`)

### 1.1 Readiness check before configuration
```
/status         → agent: idle, cli: ✓ or ✗, ready: ✗ (no API key)
/config         → shows cli, no API keys set, agent.md: missing, prompt.md: missing
```

### 1.2 Set API key
```
/set ANTHROPIC_API_KEY sk-ant-xxx    → ok ANTHROPIC_API_KEY  (value hidden)
/set DEEPSEEK_API_KEY sk-xxx         → ok DEEPSEEK_API_KEY
/set AGENT_CLI claude                → ok AGENT_CLI=claude
/set AGENT_MODEL claude-sonnet-4-5   → ok AGENT_MODEL=claude-sonnet-4-5
/config         → shows ANTHROPIC_API_KEY: set, cli: claude, ready: ✓
```

### 1.3 Create prompt files
```
/bootstrap              → created agent.md, created prompt.md, ready=true
/bootstrap              → skipped agent.md, skipped prompt.md (idempotent)
/bootstrap force        → created agent.md, created prompt.md (overwrites)
```

### 1.4 Override prompt content
```
/set-agent You are a backend engineer. Work iteratively.
            → ok wrote agent.md (N bytes)

/set-prompt 1. Read the task. 2. Make a plan. 3. Implement.
            → ok wrote prompt.md (N bytes)
```

### 1.5 Persist across restart
```
# Stop and restart agent-runner — check .env.local was written
cat .env.local          → contains ANTHROPIC_API_KEY, AGENT_CLI, etc.
/config                 → same values after restart
```

---

## 2. Memory Git — HTTPS Token Auth

### 2.1 Init
```
/set MEMORY_GIT_TOKEN <gitea-token>
/set MEMORY_GIT_USER  agent-git           → optional; defaults to oauth2
/memory git https://git.example.com/user/memory.git
    → initialised git in ./memory
    → remote set: https://git.example.com/user/memory.git
    → run /memory push to sync
```

Verify no token in stored remote:
```
git -C memory remote get-url origin
    → https://git.example.com/user/memory.git   (no oauth2:TOKEN@ prefix)
git -C memory config --local credential.helper
    → ...MEMORY_GIT_TOKEN...                    (reads from env at runtime)
```

### 2.2 Push
```
/memory push            → memory pushed to https://git.example.com/...
# Run again with no changes:
/memory push            → memory pushed (no error, no-op commit)
```

### 2.3 Pull
```
# Make a change on the remote (e.g. via Gitea web UI or another clone)
/memory pull            → memory pulled from https://git.example.com/...
```

### 2.4 Status
```
/memory status
    → Memory
    → dir: ./memory
    → git: initialised
    → remote: https://git.example.com/...
    → last commit: [memory] YYYY-MM-DD
/memory             → same as /memory status
```

### 2.5 Update remote URL
```
/memory git https://git.example.com/user/memory2.git
    → remote updated: https://...memory.git → https://...memory2.git
```

### 2.6 Same remote is idempotent
```
/memory git https://git.example.com/user/memory2.git
    → already configured: https://git.example.com/user/memory2.git
```

### 2.7 Error cases
```
/memory git             → error: usage: /memory git <remote-url>
/memory pull            (before /memory git) → error: memory dir is not a git repo
/memory push            (before /memory git) → error: memory dir is not a git repo
/memory bogus           → unknown subcommand: /memory bogus
```

---

## 3. Memory Git — SSH Key Auth

### 3.1 Generate key
```
/memory keygen
    → generated SSH key: ./memory_key
    → MEMORY_GIT_SSH_KEY saved.
    → Add this public key to GitHub/GitLab → Deploy Keys:
    → ssh-ed25519 AAAA...

# Running again must NOT regenerate:
/memory keygen
    → key already exists at ./memory_key
    → ssh-ed25519 AAAA...
```

### 3.2 Show existing public key
```
/memory pubkey          → ssh-ed25519 AAAA...
# Before keygen:
/memory pubkey          → no public key found at ./memory_key — run /memory keygen first
```

### 3.3 SSH-based push/pull
```
# Add public key to the git host as a deploy key
/memory git git@git.example.com:user/memory.git
/memory push            → memory pushed to git@git.example.com:...
/memory pull            → memory pulled from git@git.example.com:...
```

---

## 4. Diverged History Recovery

### 4.1 Local-only history vs existing remote
```
# Simulate: memory dir has commits, remote also has unrelated commits
/memory push            → should succeed (rebase or merge fallback, no error)
/memory pull            → memory pulled
```

### 4.2 Remote ahead (non-fast-forward)
```
# Push a commit to remote from another clone
/memory push            → pull-rebase then push, no error
```

---

## 5. Repo Cache (`/repo`)

### 5.1 Add a repo
```
/repo add https://git.example.com/org/myrepo.git
    → added myrepo from https://git.example.com/org/myrepo.git; added to AGENT_SHARED_REPOS

# Verify token stripped:
git -C repo-cache/myrepo remote get-url origin
    → https://git.example.com/org/myrepo.git   (no token in URL)
git -C repo-cache/myrepo config --local credential.helper
    → ...GIT_TOKEN...
```

### 5.2 Add same repo twice
```
/repo add https://git.example.com/org/myrepo.git
    → myrepo is already in cache (./repo-cache/myrepo)
```

### 5.3 List repos
```
/repo list
    → **myrepo** ✓ shared — https://git.example.com/org/myrepo.git
    →   <hash> <last commit message>
# Empty cache:
/repo list              → no repos in cache
```

### 5.4 Update a repo
```
# Push a new commit to the remote
/repo update myrepo     → updated myrepo to origin/main
# Check new file exists in cache
```

### 5.5 Remove a repo
```
/repo remove myrepo
    → removed myrepo; removed from AGENT_SHARED_REPOS
# Directory must be gone:
ls repo-cache/          → (empty or other repos only)
```

### 5.6 Typo suggestion
```
/repo remove my_repo    → error: repo not found: my_repo (did you mean myrepo?)
/repo update my_repo    → error: repo not found: my_repo (did you mean myrepo?)
```

### 5.7 Error cases
```
/repo add               → error: usage: /repo add <url>
/repo remove            → error: usage: /repo remove <name>
/repo update            → error: usage: /repo update <name>
/repo bogus             → unknown subcommand: /repo bogus
```

---

## 6. Agent Execution

### 6.1 Basic task (stateless: no repo required)
```
# POST via agent-cli or curl:
fix the typo in README.md
    → [queued] session <id>
    → [running] iteration 1...
    → [completed] N iterations, Xs
/status
    → last: completed ✓ Xs ago — "fix the typo..."
```

### 6.2 Task against a cached repo
```
# AGENT_SHARED_REPOS=myrepo must be set
add error handling to src/handler.go
    → agent runs with myrepo in workspace
```

### 6.3 Memory loaded into agent
```
# After /memory push — next agent run should reference memory dir
# Verify by checking that agent.md content appears in the generated prompt
```

### 6.4 Stop a running session
```
# While agent is running, send Ctrl+C in agent-cli
    → [stopping] sending stop signal...
    → [stopped] stopped by user
/status → last: stopped ⏹ Xs ago — stopped by user
```

### 6.5 Queue behaviour
```
# Send two tasks back to back
    → first session: running
    → second session: queued
/status → agent: running, queued: 1
```

---

## 7. Authentication (`/auth`)

### 7.1 Claude auth (chat channel only)
```
/auth claude            → Starting claude auth — open the URL when it appears...
# URL printed — open in browser, complete auth
/auth cancel            → auth flow cancelled
```

### 7.2 Auth cancel with no flow
```
/auth cancel            → no auth flow is running
```

### 7.3 Auth not available via REST
```
# POST /agent with message="/auth claude" (no send callback)
    → error: /auth is only available via chat (Telegram, WeChat, stream)
```

---

## 8. Full Workflow (End-to-End)

A complete run from cold start to agent execution with persistent memory:

```bash
# 1. Start agent-runner in a new directory
mkdir my-agent && cd my-agent
agent-runner &

# 2. Connect
agent-cli

# 3. Configure
> /set ANTHROPIC_API_KEY sk-ant-xxx
> /set AGENT_CLI claude
> /bootstrap
> /config   # verify ready: ✓

# 4. Set up memory
> /set MEMORY_GIT_TOKEN <token>
> /memory git https://git.example.com/user/agent-memory.git
> /memory push

# 5. Add a workspace repo
> /set GIT_TOKEN <token>
> /repo add https://git.example.com/org/myproject.git
> /repo list

# 6. Run a task
> fix the null pointer bug in main.go

# 7. After task completes, memory auto-pushed
> /memory status   # verify last commit timestamp updated
> /repo update myproject   # pull any remote changes
```

---

## 9. Regression Checklist

| Scenario | Expected |
|----------|----------|
| `/memory push` with no changes | No error, no spurious commit |
| `/memory push` on non-git dir | `error: memory dir is not a git repo` |
| `/repo add` with `GIT_TOKEN` set | Token injected for clone, stripped from stored remote |
| `agent-cli` paste multi-line | Entire pasted block sent as one message |
| Restart agent-runner | `.env.local` values restored, memory dir intact |
| Two agents queue | Second waits; `/status` shows `queued: 1` |
| Network down during push | Push fails with descriptive error, no crash |
| Empty `REPO_CACHE_ROOT` dir | `/repo list` → `no repos in cache` |
