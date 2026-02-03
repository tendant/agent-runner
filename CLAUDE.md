# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

This is **Claude Code v1 - Local API Wrapper**, an HTTP API that wraps Claude Code CLI to enable programmatic, controlled automation of Git repositories with safety guardrails. The project is currently in the design phase with a comprehensive specification in DESIGN.md but no implementation yet.

**Core purpose:** A minimal, local-first, stateless API that allows trusted users to invoke Claude Code against projects with built-in safety checks, path allowlisting, and automatic Git push/commit workflows.

## Architecture

### Design Principles
- Local-first: projects exist on disk
- Stateless API: no database required
- In-memory execution state only
- One request = one async execution
- Direct push to Git (Gitea)
- Explicit guardrails over heavy isolation

### Directory Structure (Planned)
```
/claude-wrapper/
  ├── api/              # HTTP API server
  ├── projects/         # Long-lived Git working copies
  ├── runs/             # Markdown execution logs (audit trail)
  ├── tmp/              # Ephemeral per-request workspaces
  └── config.yaml       # Configuration file
```

### Execution Flow
1. `POST /run` returns job ID immediately (202 Accepted)
2. Background: workspace preparation (git fetch, reset, clean)
3. Copy to isolated `tmp/job-uuid/` workspace
4. Execute Claude Code CLI in non-interactive mode
5. Validate diff against path allowlist
6. Git commit + push (with retry logic)
7. Write markdown audit log
8. Client polls `GET /job/{id}` for result

### Key API Endpoints
- `POST /run` - Initiate execution (returns job ID)
- `GET /job/{job_id}` - Poll for status/results
- `GET /status/{project}` - Check project lock status
- `GET /projects` - List available projects

### Safety Mechanisms
- Per-project mutex prevents concurrent Git operations
- Each job gets ephemeral tmp/ workspace
- Path allowlisting validates diff against `paths` parameter
- Blocked paths: `.git/`, `.github/`, `.gitlab-ci.yml`, secrets patterns

### Job Status Flow
`queued` → `running` → `validating` → `pushing` → `completed` (or `failed`)

## Implementation Notes

The design is language-agnostic but references Go types in examples. Claude Code is invoked via:
```bash
claude --print --dangerously-skip-permissions --output-format json "instruction"
```

See DESIGN.md for complete API specifications, error codes, configuration schema, and upgrade path.
