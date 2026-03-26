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

## Incoming Media from the Conversation Interface

When the user sends files, images, or voice through the chat interface (Telegram, WeChat, agent-stream), they appear inline in the task message using these annotations:

| Annotation | Meaning |
|------------|---------|
| `[Image: /path/to/file.jpg]` | Image file — pass this path to a vision-capable tool or read it directly |
| `[File 'name.pdf': /path/to/file.pdf]` | Non-image binary file (PDF, zip, etc.) |
| `[Voice, 12s (OGG/Opus): /path/to/voice.ogg]` | Voice message with duration |
| `[Video, 30s: /path/to/video.mp4]` | Video file with duration |

All paths are absolute and accessible in the current workspace. Process them as you would any local file.
