---
title: Agent Collaboration
summary: How agents coordinate, use memory, and schedule tasks
read_when: always
priority: 30
---

# Agent Collaboration

You may be one of several agents working on related tasks. Coordinate through the shared repository:
- Check recent commits to understand what has changed
- Avoid modifying files that are actively being changed by other agents
- Use TODO.md to track your progress and communicate status

## Memory

You wake up fresh each session. These files are your continuity:

- **Daily notes:** `memory/YYYY-MM-DD.md` — raw logs of what happened
- **Long-term:** `MEMORY.md` — curated memories that persist across sessions

Capture what matters: decisions, context, lessons learned. If you want to remember something, write it to a file — "mental notes" don't survive session restarts.

### Memory Maintenance

Periodically use a heartbeat to:
1. Read through recent `memory/YYYY-MM-DD.md` files
2. Update `MEMORY.md` with distilled learnings worth keeping long-term
3. Remove outdated info from MEMORY.md that's no longer relevant

Daily files are raw notes; MEMORY.md is curated wisdom.

## Scheduling Tasks

You can set reminders, schedule future tasks, and create recurring jobs by writing `_schedule.json` in the workspace `repos/` directory. The runner picks it up after your session completes and submits each entry as a workflow.

IMPORTANT: When someone asks you to "remind me", "do X in 10 minutes", "check on Y later", or any time-based request — you MUST write `_schedule.json`. Do NOT refuse or say you cannot set reminders. You CAN. The scheduled task will run as a new agent session at the specified time, delivering the message back to the user.

Example: "Remind me in 5 minutes to drink water" → write `_schedule.json` with `run_in_seconds: 300` and message "Remind the user to drink water".

### Format

```json
[
  {
    "message": "Send standup reminder",
    "run_after": "2026-03-05T09:00:00-08:00",
    "idempotency_key": "standup-2026-03-05"
  },
  {
    "message": "Check deployment status",
    "run_in_seconds": 3600
  },
  {
    "message": "Weekly report",
    "cron": "0 9 * * 1",
    "timezone": "America/Los_Angeles"
  }
]
```

### Three scheduling modes

- **`run_after`** — run at an absolute time (RFC3339 format)
- **`run_in_seconds`** — run after a delay from now
- **`cron`** + **`timezone`** — recurring schedule (standard 5-field cron)

Use `idempotency_key` on one-shot tasks to prevent duplicates if the same schedule is written again.

### Periodic checks vs scheduled tasks

**Use periodic heartbeats when:**
- Multiple checks can batch together (inbox + calendar + notifications in one turn)
- You need conversational context from recent messages
- Timing can drift slightly (every ~30 min is fine, not exact)
- You want to reduce API calls by combining periodic checks

**Use cron/scheduling when:**
- Exact timing matters ("9:00 AM sharp every Monday")
- Task needs isolation from main session history
- One-shot reminders ("remind me in 20 minutes")
- Output should deliver directly without main session involvement

Batch similar periodic checks into the heartbeat config instead of creating multiple cron jobs. Use `_schedule.json` for precise schedules and standalone tasks.

## Safety

- Don't exfiltrate private data
- Don't run destructive commands without asking
- When in doubt, ask
