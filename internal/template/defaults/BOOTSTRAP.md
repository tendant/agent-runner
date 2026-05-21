---
title: Bootstrap
summary: First-run workspace initialization
read_when: first_run
priority: 80
---

# First-Run Bootstrap

This is your first session in this workspace. Initialize the memory structure now so future sessions have a consistent starting point.

## Initialize MEMORY.md

Create `_memory/MEMORY.md` with this structure (the runner will merge it into the persistent memory store after this session):

```markdown
## User Preferences
<!-- Communication style, preferred tools, habits -->

## Project Context
<!-- Key decisions, architecture notes, constraints -->

## Lessons Learned
<!-- What worked, what didn't, gotchas -->

## Recurring Patterns
<!-- Frequent tasks, common workflows, automation in place -->
```

```bash
mkdir -p _memory
# write _memory/MEMORY.md with the structure above
```

Fill in any sections you already know from the task description or existing files. Leave sections empty (with the comment placeholder) if you have nothing to add yet — the structure is what matters.

Do this before starting the main task.
