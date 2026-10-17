# API

```bash
# Start an agent session
curl -X POST http://localhost:8080/agent \
  -H "Content-Type: application/json" \
  -d '{"message": "Build a landing page for a bakery"}'

# Poll status
curl http://localhost:8080/agent/{session_id}

# Steer a running session (persistent backends like pi): inject a message
# into the live run; 409 if the backend can't steer — send a new task instead
curl -X POST http://localhost:8080/agent/{session_id}/steer \
  -H 'Content-Type: application/json' -d '{"message": "focus on the tests first"}'

# Stop
curl -X POST http://localhost:8080/agent/{session_id}/stop
```

Repos every session gets (REPOS_DESIGN.md), the API side of `/repo`:

```bash
curl http://localhost:8080/repos
curl -X POST http://localhost:8080/repos -H 'Content-Type: application/json' \
  -d '{"url": "https://lei@gitee.com/org/oam-server.git", "secret": "GITEE_TOKEN", "branch": "main"}'
curl -X DELETE http://localhost:8080/repos/oam-server
```

`POST` fetches the repo with its login before saving it (400 on a bad URL, branch or token);
`push` defaults to `true`, `false` makes it read-only.

One-shot jobs: `POST /run` → poll `GET /job/{id}` (`project` is a registered repo's name)

**Auth:** with `API_KEY` set, every request but `/health` and `/metrics` needs it in the `X-API-Key`
header. agent-runner refuses to start when `API_BIND` accepts connections from other machines
without `API_KEY`, unless `API_ALLOW_NO_KEY=true` (something in front of it authenticates).

**Notify the chats** — `POST /notify` posts a message to every chat agent-runner reports to: each
agent-stream channel its bot is in, and the Telegram chat (`TELEGRAM_CHAT_ID`). It starts no agent;
it's for cron jobs and monitoring to tell people something. WeChat is not included.

```bash
curl -X POST http://localhost:8080/notify -H "X-API-Key: $API_KEY" \
  -H 'Content-Type: application/json' -d '{"message": "nightly backup finished"}'
```

`200 {"status":"sent"}` when every chat got it, `200 {"status":"partial","failed":[...]}` when some
did, `502` when none did, `503` when no chat bot is configured. To have the agent read an alert and
decide what to do about it, post it to an agent-stream incoming webhook instead (README, Alerts).

## Observability

You don't have to poll. Three ways to see what a session is doing and how it ended:

**Live stream** — `GET /agent/{id}/stream` is Server-Sent Events. Alongside `iteration_done` frames you get an `agent_event` per tool call as the agent works (all backends, including claude — it runs `--output-format stream-json` and parses as it goes):

```
event: agent_event
data: {"session_id":"agent-…","seq":7,"kind":"tool_start","text":"Bash: go test ./...","at":"…"}

event: agent_event
data: {"session_id":"agent-…","seq":8,"kind":"tool_end","text":"Bash error: FAIL pkg …","at":"…"}
```

Kinds: `prompt_start`, `text` (assistant prose, truncated), `tool_start`, `tool_end`, `retry`, `compaction`, `warning`, `settled`. `GET /sessions` and `GET /agent/{id}` also carry `last_event` and `events` (the most recent 50), so a fleet view can show what every parallel session is doing right now without holding N SSE connections, and a polling client still sees tool errors and warnings that happened between polls.

**Webhook** — pass `callback_url` when starting a session and agent-runner POSTs the final session JSON (same shape as `GET /agent/{id}`, plus `"event": "session.completed|failed|stopped"` and `log_file`) once it reaches a terminal status. Delivery retries three times (1s/4s/16s) on 5xx or network errors; 4xx is treated as the receiver rejecting it. Sessions interrupted by a server restart are also reported this way after recovery.

```bash
curl -X POST http://localhost:8080/agent -H 'Content-Type: application/json' \
  -d '{"message": "fix the flaky test", "callback_url": "https://ci.example.com/hooks/agent"}'
```

**Audit logs over HTTP** — every session writes a markdown audit log with each iteration's full prompt, output, error and cost (plus planner/review JSON) to `LOGS_ROOT`. These survive restarts and are now reachable without SSH:

```bash
curl http://localhost:8080/logs?limit=20         # recent sessions: status, error, cost, duration
curl http://localhost:8080/logs/{session_id}     # the full log as text/markdown (unique prefix ok)
```

**Traces** — set `TRACING_ENABLED=true` plus the standard `OTEL_EXPORTER_OTLP_ENDPOINT` (and `OTEL_EXPORTER_OTLP_PROTOCOL=grpc` if your collector isn't on HTTP) and every run becomes one trace, exported over OTLP to Tempo, Jaeger, Honeycomb, Datadog, etc.:

```
agent.session            session.id, source, cli, model, status, cost_usd, iterations
├── agent.prompt.resolve
├── agent.workspace.prepare
├── agent.planner        planner.steps
├── agent.iteration      iteration.number, status, cost_usd, duration_s, commit, retry
│   ├── agent.tool       tool.name, tool.input, tool.error
│   └── agent.tool
├── agent.iteration
├── agent.review         review.score, review.issues
└── agent.finalize
```

Failed sessions, errored iterations and errored tool calls carry error status, so "show me every trace where a `Bash` span failed inside a retry iteration" is a query, not a log grep. The trace id is returned as `trace_id` on `GET /agent/{id}`, in the webhook payload, and logged as `session trace` at start so you can jump from any of them into the trace backend. Off by default; when off the span calls are the SDK's no-ops.

**Metrics** — `GET /metrics` (Prometheus): `agent_sessions_total{status,source}`, `agent_iterations_total{status,source}`, `agent_active_sessions`, `agent_queue_depth`, `agent_cost_usd_total`, `agent_iteration_duration_seconds`, and `agent_tool_calls_total{tool,outcome}` for where the time goes. `rate(agent_sessions_total{status="failed"}[15m]) > 0` is the one alert to start with.

## Error handling

`POST /agent` checks that the configured `AGENT_CLI` binary is actually installed before queueing a session — if it's missing, you get a `412` immediately instead of a session that fails minutes later after workspace setup:

```json
{"error": "codex CLI is not installed — install it (npm install -g @openai/codex ...) or run POST /bootstrap to auto-install"}
```

Missing credentials (e.g. no `ANTHROPIC_API_KEY` and no host `claude login`) don't block the request — some setups authenticate outside an API key env var — but are surfaced as a non-fatal `warnings` array on the `202` response and on the session itself:

```json
{"session_id": "agent-...", "status": "queued", "warnings": ["claude backend requires ANTHROPIC_API_KEY, ANTHROPIC_BASE_URL (local models), or a `claude login` on this host"]}
```

If a session does fail on a recognized misconfiguration (bad/expired key, quota exceeded, CLI missing, unknown model), `GET /agent/{id}`'s `error` field is a short actionable message with the raw CLI/API error preserved underneath, e.g. `"authentication with the LLM provider failed — check credentials with /status, or re-run /auth\n\nDetails: ..."`. These same messages reach chat clients (Telegram, Stream, WeChat) too. Check overall readiness anytime with `/status` or `POST /bootstrap`.
