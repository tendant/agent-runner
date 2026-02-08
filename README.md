# Agent Runner

An HTTP API that wraps Claude Code CLI to enable programmatic, controlled automation of Git repositories with safety guardrails.

Agent Runner lets you send natural language instructions to Claude Code via a simple REST API. It handles workspace isolation, diff validation, and Git commit/push automatically.

## Prerequisites

- Go 1.25+
- [Claude Code CLI](https://docs.anthropic.com/en/docs/claude-code) installed and available on `$PATH`
- Git configured with credentials for your remote (SSH keys or credential helper)

## Quick Start

```bash
# Build
go build -o agent-runner ./cmd/server

# Create a project directory with a cloned repo
mkdir -p projects
git clone git@your-git-server:org/my-site.git projects/my-site

# Run with defaults (listens on 127.0.0.1:8080)
./agent-runner

# Or with a config file
./agent-runner -config config.yaml
```

## Configuration

Create a `config.yaml` file (all fields are optional; defaults shown below):

```yaml
# Directory paths
projects_root: ./projects
runs_root: ./runs
tmp_root: ./tmp

# Only allow specific projects (empty = allow all)
allowed_projects:
  - my-site

# Execution limits
max_runtime_seconds: 300
max_concurrent_jobs: 5

# Git push retry settings
git_push_retries: 3
git_push_retry_delay_seconds: 5

# Diff validation
validation:
  block_binary_files: false
  blocked_paths:
    - ".git/"
    - ".github/"
    - ".gitlab-ci.yml"
    - "secrets/"
    - "*.env"

# API settings
api:
  bind: "127.0.0.1:8080"
  api_key: ""  # If set, all requests must include X-API-Key header

# Cleanup
job_retention_seconds: 3600
startup_cleanup_stale_jobs: true
```

If no config file is found, the server starts with these defaults.

## API Usage

### Submit a job

```bash
curl -X POST http://127.0.0.1:8080/run \
  -H "Content-Type: application/json" \
  -d '{
    "project": "my-site",
    "instruction": "Add a contact page and link it from the homepage",
    "paths": ["content/", "pages/"],
    "commit_message": "Add contact page",
    "author": "claude-bot"
  }'
```

**Response** (202 Accepted):

```json
{
  "job_id": "a1b2c3d4-5678-90ab-cdef-1234567890ab",
  "status": "queued",
  "project": "my-site"
}
```

| Field | Required | Description |
|-------|----------|-------------|
| `project` | Yes | Directory name under `projects/`, must be a Git repo |
| `instruction` | Yes | Natural language task for Claude Code |
| `paths` | Yes | Allowlist of directories/files Claude is permitted to change |
| `commit_message` | No | Custom commit message (auto-generated if omitted) |
| `author` | No | Git author name (default: `claude-bot`) |

### Poll for results

```bash
curl http://127.0.0.1:8080/job/a1b2c3d4-5678-90ab-cdef-1234567890ab
```

**Response** (completed):

```json
{
  "job_id": "a1b2c3d4-...",
  "status": "completed",
  "project": "my-site",
  "commit": "a3f9c12",
  "changed_files": ["pages/contact.md", "pages/index.md"],
  "diff_summary": { "insertions": 120, "deletions": 0 },
  "log_file": "runs/2026-02-02_21-04-33_my-site.md",
  "duration_seconds": 87
}
```

**Response** (failed):

```json
{
  "job_id": "a1b2c3d4-...",
  "status": "failed",
  "error": "Changes outside allowed paths",
  "error_code": "PATH_VIOLATION"
}
```

Job statuses: `queued` &rarr; `running` &rarr; `validating` &rarr; `pushing` &rarr; `completed` (or `failed`).

### Check project lock status

```bash
curl http://127.0.0.1:8080/status/my-site
```

```json
{
  "project": "my-site",
  "locked": true,
  "current_job_id": "a1b2c3d4-...",
  "locked_since": "2026-02-02T21:04:33Z"
}
```

### List projects

```bash
curl http://127.0.0.1:8080/projects
```

```json
{
  "projects": [
    { "name": "my-site", "locked": false }
  ]
}
```

## Authentication

If `api.api_key` is set in the config, all requests must include the `X-API-Key` header:

```bash
curl -H "X-API-Key: your-secret-key" http://127.0.0.1:8080/projects
```

## How It Works

1. **POST /run** queues the job and returns a job ID immediately
2. The project is locked (one job per project at a time)
3. The source repo is fetched and reset to match the remote
4. The repo is copied to an isolated temp workspace (`tmp/job-uuid/`)
5. Claude Code CLI runs in non-interactive mode against the workspace
6. The diff is validated against the `paths` allowlist and blocked path rules
7. Changes are committed and pushed to the remote
8. A Markdown audit log is written to `runs/`
9. The temp workspace is cleaned up

## Safety Guardrails

- **Path allowlisting**: Only files matching the `paths` parameter can be changed
- **Blocked paths**: `.git/`, `.github/`, CI configs, secrets, and `.env` files are always blocked
- **Per-project locking**: Only one job runs per project at a time (returns `409 Conflict` otherwise)
- **Workspace isolation**: Each job runs in its own temp directory; the source repo is never modified directly
- **Execution timeout**: Jobs are killed after `max_runtime_seconds` (default: 300s)
- **Audit trail**: Every run produces a Markdown log in `runs/`

## Error Codes

| Code | Description |
|------|-------------|
| `PATH_VIOLATION` | Changes outside allowed paths |
| `GIT_DIR_VIOLATION` | Attempted to modify `.git/` |
| `CI_CONFIG_VIOLATION` | Attempted to modify CI configuration |
| `SECRETS_VIOLATION` | Secrets patterns detected in changes |
| `HOOKS_VIOLATION` | Attempted to modify Git hooks |
| `GIT_PUSH_CONFLICT` | Remote has new commits (no auto-rebase) |
| `GIT_AUTH_FAILURE` | Git authentication failed |
| `GIT_NETWORK_ERROR` | Network error during Git operations |
| `TIMEOUT` | Execution exceeded time limit |
| `CLAUDE_ERROR` | Claude Code returned an error |

## Running Tests

```bash
go test ./...
```

## Project Structure

```
├── cmd/server/          # Entry point
├── internal/
│   ├── api/             # HTTP server, routing, middleware, handlers
│   ├── config/          # YAML configuration loading and validation
│   ├── executor/        # Claude Code CLI execution, workspace management, diff validation
│   ├── git/             # Git operations (fetch, reset, commit, push)
│   ├── jobs/            # In-memory job state and project locking
│   └── logging/         # Markdown run log writer
├── e2e/                 # End-to-end tests
├── projects/            # Git working copies (created at runtime)
├── runs/                # Markdown audit logs (created at runtime)
├── tmp/                 # Ephemeral job workspaces (created at runtime)
├── config.yaml          # Configuration file
└── DESIGN.md            # Full design specification
```
