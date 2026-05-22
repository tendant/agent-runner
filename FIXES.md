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
