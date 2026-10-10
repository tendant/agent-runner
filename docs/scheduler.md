# Scheduled tasks

agent-runner can run agent tasks in the future — once, after a delay, or on a recurring cron schedule — without any external cron daemon. This is opt-in and disabled by default; it needs a database to durably track due tasks across restarts.

**Enable it:**
```bash
SCHEDULER_ENABLED=true
SCHEDULER_DATABASE_URL=postgres://user:pass@host/db   # or sqlite:///path/to/agent-runner.db
```

See `.env.example` for tuning knobs (`SCHEDULER_LEASE_DURATION`, `SCHEDULER_POLL_CAP`, `SCHEDULER_HEARTBEAT_INTERVAL`, `SCHEDULER_MAX_ATTEMPTS`, `SCHEDULER_AGENT_ID`) — the defaults are sensible for a single instance. With `SCHEDULER_ENABLED=false` (the default), the endpoints below return `503 runner not enabled`.

**Schedule a task via the API:**
```bash
curl -X POST http://localhost:8080/schedule \
  -H "Content-Type: application/json" \
  -d '{"message": "Check for new PRs and summarize them", "cron": "0 9 * * *", "timezone": "America/Los_Angeles"}'
```

`message` is required; exactly one scheduling mode is required alongside it:
- `run_after` — an absolute RFC3339 timestamp, for a one-shot task
- `run_in_seconds` — a relative delay from now, for a one-shot task
- `cron` — a cron expression, for a recurring task (`timezone` optional, defaults to UTC)

`idempotency_key` (optional) dedupes one-shot tasks — resubmitting the same key is a no-op instead of double-scheduling.

```bash
# List active schedules
curl http://localhost:8080/schedules

# Cancel one
curl -X DELETE http://localhost:8080/schedule/{id}
```

**Scheduling from within an agent session:** a running agent can queue its own follow-up tasks by writing a `_schedule.json` file to its workspace root (same convention as `_send/` for output files) — a JSON array of objects shaped like the `POST /schedule` body above:
```json
[{"message": "Check back on this deployment", "run_in_seconds": 600}]
```
agent-runner reads this file after the session completes and submits each entry the same way `POST /schedule` does. This is how an agent sets its own reminders or recurring checks without needing network access to call the API itself.

When a schedule fires, agent-runner starts a normal agent session with `message` as the task — it goes through the same planner/iteration/reviewer pipeline and shows up in `/status`, `/sessions`, and the audit log like any other session.
