# Fix Tracking — End-to-End Review

Issues identified from a full architectural review. Addressed in priority order.

---

## Critical

### [C1] Orphaned workspace on `PrepareAgentWorkspace` failure
- **Files:** `internal/executor/workspace.go:104–180`, `internal/api/agent_executor.go:223–230`
- **Problem:** If `PrepareAgentWorkspace` creates `tmp/session-<id>/` then returns an error, `SetWorkspacePath` is never called. The defer sees `WorkspacePath == ""` and skips `CleanupWorkspace`. Partial directory accumulates until the stale-job cleaner runs.
- **Fix:** Have `PrepareAgentWorkspace` clean up after itself on error (deferred `os.RemoveAll` inside the function).
- **Status:** [x] done

### [C2] `cacheRepoAtomic` restore error ignored → repo permanently missing
- **Files:** `internal/executor/workspace.go:241–247`
- **Problem:** If the rename-into-place (step 3) fails and the restore rename (step 4) also fails, the error is swallowed. `dst` no longer exists; the cache entry is permanently gone.
- **Fix:** Log a fatal/error and return a descriptive error that includes both failure reasons. The caller already logs and continues — the repo is lost either way — but the error should be surfaced clearly so the operator knows.
- **Status:** [x] done

### [C3] Post-session cleanup failures are silent — user sees "completed"
- **Files:** `internal/api/agent_executor.go:130–193`
- **Problem:** `mergeAgentMemory`, `AppendDailyLog`, `CommitAndPushMemory`, `CompleteBootstrap` all fail silently (slog.Warn only). Session is always marked completed regardless. Memory may not be persisted, yet the user gets no indication.
- **Fix:** Accumulate post-session errors and surface them in the session's `Error` field (or a new `Warnings []string` field), without changing the `Status` to failed (since the agent task itself succeeded).
- **Status:** [x] done

---

## High

### [H4] `copyDir` reproduces symlinks without validation
- **Files:** `internal/executor/workspace.go:407–413`
- **Problem:** `copyFile` re-creates symlinks as-is. An agent writing `_memory/MEMORY.md -> /etc/passwd` will have that symlink merged into the persistent memory dir. `mergeAgentMemory` and `persistOutputFiles` both use `copyFile` transitively.
- **Fix:** In `copyFile`, resolve symlinks to their content (read the target, write the bytes) rather than re-creating the link. If the symlink target is outside a configurable root, skip with a warning.
- **Status:** [x] done

### [H5] No retry on memory push — transient failure = lost session memory
- **Files:** `internal/template/memory_git.go:204–209`, `internal/api/agent_executor.go:171`
- **Problem:** A single transient network error drops the entire session's memory. The agent iteration loop has exponential backoff; memory push has none.
- **Fix:** Wrap `CommitAndPushMemory` call in a small retry loop (3 attempts, 2s delay) in the defer. The retry logic can live in the call site since memory_git.go's `pushMemory` already handles the non-fast-forward case internally.
- **Status:** [x] done

### [H6] `rebase --abort` error ignored → memory repo stuck in broken state
- **Files:** `internal/template/memory_git.go:241–246`
- **Problem:** When `pull --rebase` fails, `rebase --abort` is called but its error is silently dropped. If `--abort` fails (e.g., lock file), the repo is left in an in-progress rebase state. The merge fallback then fails with "You have an unfinished rebase".
- **Fix:** Check the `--abort` error. If it fails, return an error immediately with a message directing the operator to run `git rebase --abort` manually. Do not attempt the merge fallback on a broken rebase.
- **Status:** [x] done

---

## Medium

### [M7] Missing shared repo silently skipped — not surfaced to user
- **Files:** `internal/executor/workspace.go:141–177`
- **Problem:** A repo in `AGENT_SHARED_REPOS` that isn't in cache produces only a `slog.Warn`. The agent runs with an incomplete workspace and the session result gives no indication.
- **Fix:** Collect missing-repo names during workspace prep and add them to a `Warnings` list on the session (requires adding `Warnings []string` to `Session`/snapshot, which overlaps with C3).
- **Status:** [x] done

### [M8] Planner failure invisible to user
- **Files:** `internal/api/agent_executor.go:253–263`
- **Problem:** Non-permanent planner failure falls through silently. Chat notification shows only final outcome; user doesn't know planning was attempted and failed.
- **Fix:** When planner fails non-permanently, record it in a session warning (same `Warnings` field from M7/C3) and include a note in the daily log entry.
- **Status:** [x] done

### [M9] Reviewer continues corrective iterations after a failed iteration
- **Files:** `internal/api/agent_executor.go:448–469`
- **Problem:** If a corrective iteration errors out, the reviewer still runs and may trigger another corrective iteration. A broken workspace cascades into N failed corrections.
- **Fix:** After `liveSession.AddIteration(result)`, check `result.Status == agent.IterationStatusError` and break out of the reviewer loop.
- **Status:** [x] done

### [M10] `git clean -fdx` makes workspace cache state non-deterministic
- **Files:** `internal/executor/workspace.go:311`
- **Problem:** Every session load runs `git clean -fdx`, removing build artifacts. Sessions that trigger a build cache them back; sessions that don't won't. The cache alternates between clean and artifact-laden states unpredictably.
- **Fix:** Remove `git clean -fdx` from `fetchAndReset` (the workspace is already a fresh copy from cache — cleaning isn't needed). Keep `git reset --hard origin/<branch>` to ensure the working tree matches the remote.
- **Status:** [x] done

---

## Low

### [L11] Inconsistent logging: `log.Printf` / `fmt.Printf` in workspace.go
- **Files:** `internal/executor/workspace.go:85,153,156,315`
- **Problem:** workspace.go uses `log.Printf` and `fmt.Printf` while the rest of the codebase uses `slog`. These messages are invisible to structured log shippers.
- **Fix:** Replace all `log.Printf` / `fmt.Printf` calls with `slog.Info` / `slog.Warn`.
- **Status:** [x] done

### [L12] `mergeAgentMemory` not atomic
- **Files:** `internal/api/agent_executor.go:785–808`
- **Problem:** Files are copied one-by-one into the memory dir. A crash mid-merge leaves the dir in a partial write-back state, mixing new and old files.
- **Fix:** Copy to a temp subdir first, then rename each file into place (consistent with the `cacheRepoAtomic` pattern). Or: accept the partial-write risk as low-severity given the daily-commit safety net.
- **Status:** [x] done

### [L13] Three separate `Snapshot()` calls in executeAgent defer
- **Files:** `internal/api/agent_executor.go:79,87,138`
- **Problem:** `snap0` (metrics), `snap` (audit log), `snap2` (daily log) are taken at slightly different moments. Minor inconsistency in reported counts.
- **Fix:** Take a single snapshot at the start of the defer and reuse it throughout. The session is no longer mutated by the time the defer runs.
- **Status:** [x] done

---

## Found via live TESTPLAN.md validation (2026-07-17)

Found by actually running the built binary end-to-end against local bare git
remotes (not just unit tests) while manually walking TESTPLAN.md §2/§5.

### [C14] `DATA_DIR` silently ignored for state dirs unless it's a real OS env var
- **Files:** `internal/config/config.go` — `LoadFromEnv()`, `DefaultConfig()`
- **Problem:** Two compounding issues. (1) `DefaultConfig()` recomputed its own `defaultDataDir()` (hardcoded `"."`) independently of the `dataDir` resolved in `LoadFromEnv()`, so `RepoCacheRoot`/`LogsRoot`/`TmpRoot`/`MemoryDir`/etc. never actually moved with `DATA_DIR` — only `.env.local`'s path did. (2) Even after fixing that, `dataDir` was resolved from `os.Getenv("DATA_DIR")` *before* `.env` was read, so `DATA_DIR` set inside `.env` (as the README and `.env.example` both document as valid) was read too late to have any effect — it only worked when exported as a real OS env var (e.g. Docker's `-e DATA_DIR=/data`). Masked in the shipped Docker image because `WORKDIR /data` happens to equal the documented `DATA_DIR=/data`.
- **Fix:** `DefaultConfig()` now delegates to `defaultConfigForDataDir(data string)`; `LoadFromEnv()` reads `.env`/`.env.<instance>` first, resolves `dataDir` from OS env then the merged `.env` map then the default, and passes that resolved `dataDir` into `defaultConfigForDataDir`.
- **Status:** [x] done — regression test `TestLoadFromEnv_DataDirFromEnvFile_RelocatesStateDirs` in `internal/config/config_test.go`.

### [H15] Fresh `/memory git <remote>` → `/memory push` fails with a confusing compound error
- **Files:** `internal/template/memory_git.go` — `pushMemory`
- **Problem:** If the memory dir has zero commits (e.g. `/memory push` run before `/bootstrap` or any agent session has written files), `git add -A` has nothing to stage, so no commit is ever made, and `git push origin HEAD` fails against a branch with no commits. The rebase-fallback then also fails against the empty remote (`couldn't find remote ref HEAD`), and `rebase --abort` fails too (`no rebase in progress`), surfacing a compound low-level git error with no indication the real problem is "nothing to commit yet."
- **Fix:** `pushMemory` now checks `git rev-parse HEAD` up front and returns a clear `no commits yet ... run /bootstrap or an agent session first` error before attempting push/rebase.
- **Status:** [x] done — regression test `TestPushMemory_NoCommitsYet_ReturnsClearError` in `internal/template/memory_git_test.go`.

### [H16] User-stopped session reported as "completed" — indistinguishable from success
- **Files:** `internal/agent/types.go`, `internal/agent/manager.go`, `internal/api/agent_executor.go` (`determineFinalStatus`), `internal/api/agent_handlers.go` (SSE done-event terminal check), `internal/api/command.go`, `internal/api/runner_bridge.go`, `internal/botcommon/{poll,botcommon}.go`, `cmd/cli/main.go`
- **Problem:** `determineFinalStatus` mapped a user-requested stop (`StopRequested()`) to the same `CompleteSession` call used for genuine success, with no distinct status or recorded reason. Live-confirmed: stopping a session mid-call (0 iterations run) still showed `status: "completed"` and `/status` showed `last: completed ✓ ...`, directly contradicting TESTPLAN.md §6.4's documented `[failed] stopped by user`. Traced via `git log -S StopRequested` to commit `f446945` (2026-05-24) — intentional since agent mode's stop handling was introduced, not a regression, but still wrong for the zero-iteration case and inconsistent with the project's own docs. Also confirmed live: `/agent/{id}/stop` only takes effect between iterations/phases, not mid in-flight CLI call — expected, not changed here.
- **Fix:** Added a new terminal `SessionStatusStopped` ("stopped"), distinct from both completed and failed, with `Session.Stop(reason)` / `Manager.MarkSessionStopped(id, reason)` recording `"stopped by user"` in the session's `Error` field. Updated every terminal-status check and status-rendering path (SSE `done` event, `/status`, `/sessions`, `/logs` icons, bot polling, `FormatStatusLine`, the runner bridge, and the `agent-cli` SSE handler) to recognize the new status instead of falling through to "completed" or being silently unreachable.
- **Status:** [x] done — regression cases added to `TestDetermineFinalStatus` in `internal/api/agent_executor_test.go`; `TestE2E_AgentGracefulStop` in `e2e/agent_e2e_test.go` updated to assert `stopped` instead of `completed`. TESTPLAN.md §6.4 updated to match. Live-confirmed against a real server with a slow mock CLI: stop now yields `status: "stopped"`, `error: "stopped by user"`, and `/status` shows `last: stopped ⏹ ...`.
