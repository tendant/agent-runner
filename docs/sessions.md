# Parallel sessions, multi-turn tasks and locks

## Running sessions in parallel

By default one agent session runs at a time. `AGENT_MAX_CONCURRENT=N` starts N
dispatch workers instead, so N sessions can overlap. Requires a restart — the
pool is sized at startup.

Each session already gets an isolated workspace, and the runner guards the state
they share: the memory dir (mutex in `internal/template`) and the repo cache
(per-repo lock in `internal/executor`).

**Git is handled without locks.** Two sessions pushing to the same branch is the
normal case, and it resolves the way it does for people:

1. A rejected push is rebased onto the moved remote and pushed again, up to
   `GIT_PUSH_RETRIES` times (the remote can move again in between).
2. If the rebase conflicts, the rebase is left in progress and the agent gets a
   corrective iteration naming the repo, branch and conflicted files, with the
   exact steps (resolve markers → `git add` → `git rebase --continue` → push).
   Up to two such iterations.
3. If it still can't be merged, the runner aborts the rebase and pushes the
   session's commits to `agent-rescue/<session-id>` so nothing is lost when the
   workspace is deleted, and the session carries a warning naming that branch.

The default `agent.md` tells the agent the same for pushes it does itself
mid-task. Every git repo in the workspace is handled, not just the first.

## Multi-turn tasks

By default (`AGENT_TASKS_ENABLED`, set it to `false` for one-shot runs; design in
[TASKS_DESIGN.md](../TASKS_DESIGN.md)) a chat thread's work becomes a *task* that
can span several agent runs:

- The agent can stop mid-task to ask the user a question by writing
  `"status": "needs_input"` and a `"question"` to `_progress.json`. The run ends
  at the end of that iteration and the question is posted in the thread.
- The user's next message in the thread is the answer: it skips the intent
  analyzer and starts the next turn in **the same workspace** (scratch files,
  `_progress.json` completed steps and the saved plan are kept). The turn's
  prompt carries a task context block — goal, plan progress, decisions, earlier
  turns' summaries and the reply — instead of the raw chat transcript.
- A turn that hits a limit or fails leaves the task *paused* and posts the plan
  checklist; "continue" picks it up, anything else is feedback. `/cancel`
  cancels the task and releases its workspace.
- **Feedback revises the plan.** Feedback on a paused task, or on a finished
  one whose workspace is still kept, goes to the planner in revise mode with
  the current plan: finished steps stay finished unless the feedback reopens
  them. A large revision (it reopens finished work or adds 3+ steps) is shown
  for approval first — "yes" proceeds, "no" asks what to change, anything
  else revises again. The intent analyzer decides whether a message after a
  finished task is feedback or a new request.
- **Budgets.** Each task has a budget of turns, working time and cost
  (`AGENT_TASK_MAX_*`). Reaching it pauses the task: "continue" resets the
  budget and resumes, "stop" ends it. A turn that asks a question is never
  paused for budget.
- **Telegram and WeChat run one task per chat.** A new request that arrives
  while the chat's task is unfinished is queued ("Queued — I'll start it when
  the current task finishes", at most 5) and starts as a new task when the
  current one finishes or is cancelled. In agent-stream a new request is a new
  thread, so nothing queues.
- Each turn still commits and pushes as usual. Repos are cached back only when
  the task's workspace is released.

The whole flow is covered end to end against a real agent-stream server by
`e2e/tasks_stream_e2e_test.go` (set `AGENT_STREAM_SRC` to the agent-stream
server source to run it).

| Variable | Default | |
|---|---|---|
| `AGENT_TASKS_ENABLED` | `true` | Multi-turn tasks for chat bots |
| `AGENT_TASK_RETENTION` / `AGENT_TASK_IDLE_TTL` | `24h` / `168h` | How long a finished / waiting task keeps its workspace |
| `AGENT_TASK_MAX_TURNS` / `AGENT_TASK_MAX_SECONDS` / `AGENT_TASK_MAX_COST_USD` | `10` / `4h` / unlimited | Per-task budget; reaching it pauses the task until the user says "continue" |
| `AGENT_TASK_RESUME_BACKEND` | `true` | Continue the agent CLI's own conversation across a task's turns (pi, claude, codex) |

Task records live in `STATE_ROOT/tasks/`, workspaces in `TMP_ROOT/task-*`. A
background sweep releases a finished task's workspace after
`AGENT_TASK_RETENTION` and expires a task left waiting after
`AGENT_TASK_IDLE_TTL`. A turn interrupted by a restart pauses its task.

**Backend conversation resume** (`AGENT_TASK_RESUME_BACKEND`, on by default):
the agent CLI also keeps its own conversation across turns, so it remembers
its earlier reasoning and tool results, not just the context block.

| CLI | How |
|---|---|
| `pi` | a durable session (`--session-dir <task>/state/backend/pi --session-id <id>`) instead of `--no-session`; each turn's process restores it |
| `claude` | the first prompt starts `--session-id <uuid>`, later prompts `--resume` it — within a turn too, so iterations after the first get incremental prompts as with pi |
| `codex` | the first prompt starts a thread (its ID comes from the `thread.started` event of `--json` output), later prompts `codex exec resume <id>` it; continuations don't re-inline the system prompt |
| `opencode` | not yet; context block only |

A conversation that can't be resumed is dropped with a warning and a fresh one
starts; the context block keeps the turn correct either way. Claude keeps a
conversation's first system prompt on resume.


What the runner cannot resolve is state outside git — a sequential ID derived by
listing a directory, a deploy slot, a shared config file. That's what the
optional named lock is for; use it only when a task genuinely needs exclusive
access to something like that:

```bash
# Acquire; blocks up to wait_seconds for the current holder.
curl -X POST "$RUNNER_URL/lock" -H "X-API-Key: $API_KEY" \
  -H "X-Session-ID: $SESSION_ID" -H "Content-Type: application/json" \
  -d '{"name":"sites-config","ttl_seconds":900,"wait_seconds":600}'

# Release
curl -X DELETE "$RUNNER_URL/lock/sites-config" \
  -H "X-API-Key: $API_KEY" -H "X-Session-ID: $SESSION_ID"

# Inspect
curl "$RUNNER_URL/locks" -H "X-API-Key: $API_KEY"
```

`200` acquired, `409` held by someone else (the body names the holder and when
their lease expires). Locks are released automatically when the holding session
ends and expire after their TTL regardless, so a crashed agent cannot wedge a
name. Every prompt gets `{{RUNNER_URL}}`, `{{API_KEY}}` and `{{SESSION_ID}}`
substituted in, so the agent can call this without extra configuration —
`prompt.md` shows the pattern.
