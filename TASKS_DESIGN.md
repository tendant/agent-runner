# Multi-Turn Tasks — Design

Status: draft, decisions recorded in §15 · Scope: agent-runner (engine, thread state, execution, stream bot); one small agent-stream change (§9)

## 1. Problem

Inside one run, agent-runner already has a plan → work → iterate loop
(`internal/execution/engine.go`):

- **Planner** (`AGENT_PLANNER_ENABLED`, default on) writes numbered steps.
- **Iteration loop**: up to `AGENT_MAX_ITERATIONS` (20) or `AGENT_MAX_TOTAL_SECONDS` (3600). The agent records finished steps in `_progress.json`. The loop stops on `TASK: DONE`, on a `blocked_steps` entry, when all steps are done, or when no progress is made for 5 iterations.
- **Reviewer** (optional): corrective iterations.

**Across turns it is one-shot.** When a run ends, and the user replies in the thread:

| What | Today | Where |
|---|---|---|
| Workspace | deleted after repos are cached back | `engine.go` top-level defer, `CleanupWorkspace` |
| Agent conversation | gone; `pi` runs `--mode rpc --no-session` for one run; claude, codex and opencode run one prompt per call | `executor/pi_backend.go`, `executor/executor.go` |
| Plan and progress | gone with the workspace; the next run re-plans from scratch | `subagent/planner.go`, `_progress.json` |
| Context | the thread's chat transcript as text, compacted past 20 messages | `botcommon/engine.go` `HandleConfirmation` |
| Asking the user | not possible: `blocked_steps` ends the run, and the answer later starts an unrelated run | `runIterationLoop` |

So a task that needs a question answered, a review, or more than one hour of
work loses its state at each turn boundary.

## 2. Goals and non-goals

Goals:

- **A Task spans many runs ("turns") in one thread**, keeping its plan, progress, workspace and decisions between them.
- **The agent can ask a question and wait.** The user's answer resumes the same task where it stopped.
- **Feedback after "done" continues the task** instead of starting over.
- **The agent's own conversation is resumed** where the backend supports it, with a structured fallback where it doesn't.
- **Bounded:** per-turn limits as today, plus per-task limits on turns, time and cost.
- **Survives restarts** of agent-runner.
- **Works on every transport.** On Telegram and WeChat, a chat is one task at a time.

Non-goals:

- Several tasks in parallel inside one thread (use separate threads).
- Changing the per-run iteration logic beyond the new signals (§5).
- A UI beyond what the thread already shows. A live-updating plan message is optional (§9).

## 3. Concepts

- **Task:** one unit of user work, bound 1:1 to a thread key (an agent-stream thread, or a Telegram/WeChat chat). It is created when a thread's first actionable message is routed to `execute` or `plan`.
- **Turn:** one agent run on a task. This is today's session, with its planner, iteration loop and reviewer phases. A task has many turns, and at most one is running at a time.
- **Task record:** the durable state between turns (§4).
- **Task workspace:** the working directory that persists across turns (§6).

## 4. Task record

Stored as `STATE_DIR/tasks/<thread-key>.json` and written atomically
(write-then-rename, as `thread.Manager` already does). It sits next to the
thread file, keyed the same way.

```json
{
  "id": "task-<uuid>",
  "thread_key": "m_...",
  "status": "planning|awaiting_approval|working|awaiting_input|reviewing|paused|done|failed|cancelled",
  "goal": "one-paragraph statement of what the user wants",
  "plan": [ { "id": "1", "text": "…", "status": "todo|doing|done|blocked" } ],
  "decisions": [ "user chose Postgres over SQLite (turn 2)" ],
  "open_question": "Which environment should I deploy to?",
  "turns": [
    { "session_id": "agent-…", "started_at": "…", "ended_at": "…",
      "stop_reason": "needs_input", "summary": "…", "cost_usd": 0.41 }
  ],
  "workspace": "STATE_DIR/tasks/<thread-key>/",
  "backend": { "cli": "pi", "session_ref": "…" },
  "budget": { "turns_used": 3, "seconds_used": 2140, "cost_usd": 1.12 },
  "artifacts": [ "file ids / _send names from earlier turns" ],
  "created_at": "…", "updated_at": "…"
}
```

- `goal`, `plan` and `decisions` are written by the runner from planner output and turn results. The agent never edits the record directly. It reports through `_progress.json` (§5), and the runner merges that in.
- `turns[].summary` is the agent's own end-of-turn summary (§5). When a turn has no summary, the runner falls back to the curator's summary.
- `thread.Thread` gains `TaskID`, and its `State` gains the task states. The existing states map to them: `gathering` means "no task or task done", `confirming` becomes `awaiting_approval`, and `executing` becomes `working`.

## 5. Turn protocol (agent → runner)

`_progress.json` is extended, staying backward compatible: the existing fields keep their meaning.

```json
{
  "completed_steps": ["1", "2"],
  "blocked_steps": [ { "step": "3", "reason": "…" } ],
  "status": "working|needs_input|done",
  "question": "Which environment should I deploy to?",
  "summary": "Implemented the migration and tests; waiting for the target env.",
  "decisions": [ "used the existing users table" ]
}
```

- **`status: needs_input`** plus a `question` ends the turn at the end of the iteration. The task moves to `awaiting_input`, and the runner posts the question in the thread. This replaces "blocked" as the way to ask the user. `blocked_steps` keeps its current meaning (stuck, cannot continue) for compatibility.
- **`status: done`**, or the existing `TASK: DONE` marker, ends the turn as complete. The reviewer runs if it is enabled.
- **`summary` and `decisions`** are appended to the task record at the end of every turn. The prompt builder asks for them in the final iteration, and when a turn stops for a limit.
- **Instructions** are added to `subagent/promptbuilder.go` next to today's `doneInstruction` and progress instructions.

## 6. Task workspace

- **Path:** `STATE_DIR/tasks/<thread-key>/workspace/` instead of `session-<id>/workspace/`. The layout (`workspace/`, `state/`, `_send/`, `_progress.json`) is unchanged.
- **Created on the first turn** by `PrepareAgentWorkspace`, exactly as today. Later turns reuse the directory as-is. They do not copy the repos in again or wipe untracked files.
- **Repos (decided: push per turn):** each turn keeps today's end-of-run git handling. It commits and pushes through `gitsync` (rebase, agent-assisted conflict resolution, rescue branch), so finished work lands every turn, as it does now, and is never stranded in a workspace. What persistence adds is uncommitted scratch work, generated files, `_send/` history and the progress file.
- **Cache-back:** `CacheReposBack` runs only when the task ends (`done`, `failed` or `cancelled`), not after every turn. That keeps half-finished work out of the shared cache that other tasks copy from.
- **Lock:** each workspace has a lock, taken for the length of a turn. The thread state machine already allows only one turn per task; the lock is a guard against races around restart recovery.
- **Clean-up:**
  - `done`: the workspace is deleted after `AGENT_TASK_RETENTION` (default 24h), which leaves room for feedback.
  - `awaiting_input` or `paused`: deleted after `AGENT_TASK_IDLE_TTL` (default 7d). The task is then marked `failed` with the reason "expired", and the record is kept.
  - A background sweep runs this, in the same way the session clean-up loop does.

## 7. Conversation continuity

Every turn gets a **task context block** in its prompt, whatever the backend.
The prompt builder assembles it from the task record:

```
## Task
Goal: …
Plan: 1 [done] … / 2 [done] … / 3 [doing] … / 4 [todo] …
Decisions so far: …
Previous turns: turn 1 — …; turn 2 — … (asked: "Which environment?")
User's reply: "staging"
```

This replaces passing the raw chat transcript (§1) as the default context. It
is shorter, it is structured, and it doesn't degrade under compaction.

**Backend resume, on top of the context block:**

| Backend | Resume mechanism | Stored as `backend.session_ref` |
|---|---|---|
| `pi` (default) | drop `--no-session`; start with a session file in the task dir so the process restores its conversation | session file path |
| `claude` | `--resume <session_id>`; the ID comes from the `system/init` event of `stream-json` output | Claude session ID |
| `codex` | `codex exec resume <id>` | Codex session ID |
| `opencode` | none assumed; context block only | — |

- **Pre-check:** the exact `pi` flags and the Codex resume subcommand must be checked against the CLI versions installed by `clisetup` before phase 2. The context block alone must be enough for correctness, and resume is an optimisation.
- **Failure handling:** if resume fails (session missing, format changed), the runner logs a warning, clears `session_ref` and continues with the context block. It never fails the turn over this.

## 8. State machine and routing

```
                 execute/plan
 (no task) ──────────────────► planning ──► awaiting_approval ──yes──► working
                                                 │ no                 ▲   │
                                                 ▼                    │   │ needs_input
                                             (no task)                │   ▼
                                                               reply  │ awaiting_input
                                                                      │   │
   working ── done ──► reviewing ──► done ── feedback ────────────────┘   │
      │                                                                    │
      ├── turn limit (iterations/time) ──► paused ── "continue" ──► working
      └── task budget exhausted ──► paused (ask to raise limits or stop)
```

Routing rules in `handleMessage`, per thread. They replace today's three-state switch.

| Task status | Incoming message | Action |
|---|---|---|
| none / done-expired | any | today's analyzer (`ask`, `plan`, `execute`) |
| `awaiting_approval` | yes / no / edit | run the plan / drop it / re-plan with the edit |
| `working` | any | steer the live session if possible, else queue (today's `HandleExecuting`) |
| `awaiting_input` | any | **resume directly**: the reply becomes the answer, and the analyzer is skipped |
| `paused` | "continue" / other | resume / treat as feedback and resume |
| `done` (within retention) | any | the analyzer chooses **feedback** (resume the task with the message) or **new** (suggest a new thread, or start a new task in chat transports) |
| any | `/cancel` | cancel the task, clean up the workspace, reset the thread (as today, extended) |

**New requests while a task is active (chat transports).** On Telegram and
WeChat a chat is one task at a time. When a message arrives for a task that is
not finished, and the analyzer classifies it as a *new request* rather than an
answer or feedback, it is **queued** on the chat and the user is told so
("Queued — I'll start it when the current task finishes"). Queued requests
start in order, each as a new task, once the current task reaches `done`,
`failed` or `cancelled`. `/cancel` also releases the queue's next request. In
agent-stream this never arises: a new request is a new top-level message,
which is a new thread and so a new task.

**Plan revision:** when a task resumes after feedback, the planner runs in
revise mode. It receives the current plan and statuses plus the new message,
and returns an edited plan. Finished steps stay finished unless the feedback
reopens them. For large changes the result goes to `awaiting_approval`, as
today's `plan` action does.

## 9. User-visible behaviour in agent-stream

- Each turn keeps posting status and results as today (`run.status`, `reply.*`) in the task's thread.
- A question (`needs_input`) is posted as a normal bot message ending with the question.
- A final or paused turn posts a short checklist: plan steps with their status, plus the next action.
- **Optional live plan message:** a single bot message holding the checklist, edited as steps finish.
  - This needs an agent-stream change. Editing is currently limited to the author and a 5-minute window (`ErrEditWindowExpired` in the store).
  - Proposal: let bots edit their own messages without a time limit, or add a "status message" kind that bots can update.
  - Until then, a checklist is posted per turn.

## 10. Limits and budgets

| Limit | Default | Scope | On hit |
|---|---|---|---|
| `AGENT_MAX_ITERATIONS` / `AGENT_MAX_TOTAL_SECONDS` | 20 / 3600 | per turn (unchanged) | turn ends → task `paused` with "reply continue" |
| `AGENT_TASK_MAX_TURNS` | 10 | per task | `paused`; user can continue (resets the counter) or stop |
| `AGENT_TASK_MAX_SECONDS` | 4h | per task, working time only | same |
| `AGENT_TASK_MAX_COST_USD` | unset | per task, from iteration cost | same |

Time spent in `awaiting_input` or `paused` doesn't count as working time.

## 11. Restart recovery

- **Task records are on disk**, so an `awaiting_input`, `paused` or `done` task survives restarts with nothing extra to do.
- **A turn running at restart time** is recovered by the existing session journal and `ResumeSession` path. The journal entry gains `task_id`.
  - When the session can be re-attached, the watcher updates the task when the turn finishes.
  - When it can't, the task becomes `paused` with the reason "interrupted by restart". Its workspace is intact, so "continue" resumes it.

## 12. Changes by package

| Package | Change |
|---|---|
| `thread` | `TaskID`; task states; routing helpers |
| new `task` | record type, load/save, sweep, budget accounting |
| `botcommon` (engine) | routing table (§8); `HandleConfirmation` starts or resumes a task turn; `watchSession` merges results into the task and sets the status |
| `execution` | a task-scoped workspace in place of the per-session one; cache-back and clean-up at task end; stop on `needs_input`; context block; summaries |
| `subagent` | `_progress.json` extensions; instructions; planner revise mode |
| `executor` | resume per backend (§7): `pi` session file, claude `--resume`, codex resume |
| `sessionjournal` | `task_id` on entries |
| `stream`, `telegram`, `wechat` | posting questions and checklists; `/cancel` semantics; commands posted in the thread |
| agent-stream (optional) | bot message edits without the 5-minute window (§9) |

## 13. Testing

- **State machine:** every row of the routing table; `needs_input` → reply → resume in the same workspace with the answer in context; feedback after done; pause at turn limit → continue; `/cancel` cleans up.
- **Workspace:** a file written in turn 1 is visible in turn 2; cache-back happens only at task end; the clean-up sweep respects the TTLs.
- **Protocol:** `_progress.json` with and without the new fields, so old prompts still work; `TASK: DONE` still ends the turn.
- **Context block:** assembled from the record, and trimmed to a budget as turns accumulate.
- **Resume:** per backend, with a fake CLI that records its arguments; the fallback when a resume fails.
- **Recovery:** a restart during a turn, with and without re-attachment.
- **End to end** (stream bot, with a fake starter as in `stream/thread_test.go`): two turns plus a question in one thread, while another thread runs its own task independently.

## 14. Rollout

| Phase | Scope |
|---|---|
| 1 | Task record, task workspace (persist, lock, clean-up), `_progress.json` extensions, `needs_input` → `awaiting_input` → resume routing, context block, per-turn summaries. Gated behind `AGENT_TASKS_ENABLED`, default off. |
| 2 | Backend resume (`pi` session file first, as the default CLI; then claude and codex) |
| 3 | Planner revise mode, per-task budgets and `paused`, checklists; the optional live plan message with the agent-stream change |
| 4 | Default `AGENT_TASKS_ENABLED` on; retire the transcript-as-context path |

**Phase 1 status (implemented).** Differences from the text above:

- Workspaces live at `TMP_ROOT/task-<task-id>` (one per task, so a new task in the same thread never inherits a finished one's files); records at `STATE_ROOT/tasks/<thread-key>.json`. The `task-` prefix keeps them out of the startup stale-workspace sweep.
- No separate workspace lock: the thread state machine already allows one turn per thread, and a new task in a thread releases the previous task's workspace before starting.
- Routing: `awaiting_input` and `paused` tasks resume on the next message; `done` tasks (within retention) start a new task rather than taking feedback (feedback resume and plan revise mode are phase 3).
- While a task is waiting, every message in the thread is its answer. Queueing new requests on Telegram/WeChat (decision 3) is not in phase 1.
- A turn stopped by `needs_input` completes (it is not reviewed), and each reused turn starts with an empty `_send/` and no `_schedule.json` so earlier outputs aren't delivered twice.

## 15. Decisions and open questions

Decided:

1. **Push per turn.** Each turn commits and pushes through `gitsync`, as today. There are no task branches and no merge step at task end.
2. **Retention:** 24h after `done` (`AGENT_TASK_RETENTION`), 7 days idle in `awaiting_input` or `paused` (`AGENT_TASK_IDLE_TTL`).
3. **New requests during an active task (Telegram/WeChat) queue** behind the current task and start in order as new tasks (§8). The analyzer still separates an answer from a new request, so a reply to an open question resumes the task.

Open:

1. **Several repos:** should one task be able to change several shared repos in one turn? Today's `gitsync` handles them per repo; confirm that stays enough.
2. **Question limits:** cap the number of questions a task can ask (e.g. 5), to avoid ping-pong?
3. **Cost reporting:** show the per-task cost in the final checklist?
4. **Queue bound:** how many queued requests per chat before new ones are refused (proposal: 5)?
