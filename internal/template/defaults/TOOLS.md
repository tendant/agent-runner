---
title: Tools
summary: Available tools and file conventions
read_when: always
priority: 50
---

# Tools and Conventions

- Place files you want to deliver to the user in the `_send/` directory
- Track completed plan steps by updating `_progress.json` with: `{"completed_steps": ["1", "2"]}`
- Schedule future tasks via `POST {{RUNNER_URL}}/schedule` (see AGENTS.md for details)
  - IMPORTANT: This is the ONLY way to create scheduled/recurring jobs. Do NOT use CronCreate, cron tools, or any other mechanism — they are session-scoped and will be lost when your session ends.
- Use `TODO.md` to track your progress within the workspace
