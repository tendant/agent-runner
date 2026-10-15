# Memory and Prompt Management — Design

Status: phase 1 implemented (runtime layer, slim `agent.md` with upgrade of unedited defaults,
`/prompt show|check|preview`); the rest is a proposal. Current behaviour: DESIGN.md "Memory & Prompt Composition".

Decided:
- **Several people** talk to one agent and share its memory. There is no per-person memory;
  the agent is told that what it saves is shared with everyone.
- **Each agent has its own memory repo.** oam and ubuntu-max share nothing: each repo has one
  set of instructions and its own projects.

## 1. Problems today

1. **Instructions go stale.** `agent.md` mixes the operator's instructions (persona, rules) with
   facts about the runtime (how git works, where credentials are, the sandbox). It is seeded once
   and never updated, so a runtime change (the sandbox, "the runner pushes") leaves every
   existing instance with wrong instructions until someone edits the file by hand.
2. **No way to see or change prompts from chat.** Editing means SSH or git. Nobody can see the
   prompt a session actually got, or what the memory budget cut from it.
3. **One flat memory for everything.** Notes about oam-server, the user's preferences and
   lessons from unrelated work share one directory and one budget. Every session gets all of
   it, trimmed by a character cap, not by relevance.
4. **Agent writes are uncontrolled outside the sandbox.** Sandboxed runs merge through a checked
   copy (allowlist, append rules, no instruction files). Unsandboxed runs write the memory dir
   directly, including `agent.md` and `prompt.md`, and concurrent sessions can overwrite each
   other.
5. **No provenance or undo per change.** The runner commits whatever is in the dir with one
   generic message. There is no "which session wrote this" or "undo that session's notes".

## 2. Goals

* Instructions about the runtime always match the code running them.
* The operator owns instructions; agents own notes; the runner enforces the line between them.
* A session gets the memory relevant to it.
* Every change is attributable and reversible, and the composed prompt is inspectable.
* Git stays the source of truth and the sync mechanism; everything stays plain Markdown.

Non-goals: a vector database, embeddings, or a hosted service. Search can come later (§9).

## 3. Three layers of instructions

| Layer | Owner | Where | Changes when |
|---|---|---|---|
| **Runtime** | the code | built in, composed per session | the runner is upgraded; differs by mode (sandboxed or not, CLI, task or one-shot) |
| **Instructions** | operator | `memory/instructions/agent.md`, `workflow.md` | the operator edits them (chat, API, git) |
| **Memory** | agents, curated | `memory/global/`, `memory/projects/<name>/` | sessions save notes; the curator compacts |

The **runtime layer** takes over everything that describes how the runner works:
- git: who pushes, conflicts;
- memory: where the copy is and what is saved;
- credentials;
- the sandbox;
- `_send/`, `_progress.json` and `_schedule.json`;
- `POST /lock`.

It is generated per session, like today's sandbox notes, so it can never go stale. `agent.md`
shrinks to what only the operator knows: who the agent is, how to talk to the user, house rules.

**Migration:** the runner keeps hashes of every default `agent.md` and `prompt.md` it has ever
shipped.
- An instance whose file still matches one is upgraded automatically to the new slim default.
  The old version stays in git history.
- A customized file is kept, and `/prompt check` lists the lines that duplicate or contradict
  the runtime layer.

## 4. Memory layout and scopes

```
memory/
  instructions/   agent.md, workflow.md            operator-owned
  global/         user_preferences.md, lessons.md  every session
  projects/<p>/   summary.md, decisions.md, *.md   sessions working on <p>
  logs/YYYY-MM/   DD-<host>.md                     runner-written daily logs
```

**Which projects a session gets:** the repos in its workspace (shared repos, task repos), plus
any named with `/repo`. `<p>` is the repo name (`oam-server`). A session with no repo gets
`global/` only. `Recent Sessions` is filtered the same way: entries from sessions on the same
projects, or from the same task or chat thread.

**Budget:** applied per scope, in this order:
1. instructions, never cut;
2. global;
3. the session's projects, split evenly;
4. recent sessions.

Each cut is reported in the prompt preview (§7).

**Migration:** today's top-level files move to `global/` (`decisions.md` and
`project_summary.md` too, since we can't tell which project they belong to; the curator can
later move items it can place, §8). `agent.md` and
`prompt.md` move to `instructions/`. Old paths are read as fallbacks for one release.

## 5. Writing memory

Every session, sandboxed or not, writes through a staged copy. It extends today's sandboxed
merge, which becomes the only path.

1. **Stage.** Copy the session's scopes (global plus its projects) into a per-run directory and point `{{MEMORY_DIR}}` at it. Instructions and logs are copied read-only, or not copied.
2. **Merge** at session end, under the memory lock:
   - **Allowed:** lowercase `.md` files in the staged scopes.
   - **Append-only:** `lessons.md`.
   - **Refused:** instructions, logs, deletions, links and binaries.
   - **Limits:** size and new-file caps.
   - **Three-way:** a file someone else changed meanwhile takes only a pure append.
   - A new file goes to the scope it was written in. The runtime layer tells the agent where
     things belong (facts about a repo in `projects/<p>/`, the rest in `global/`) and that
     several people share this memory.
3. **Optional review queue** (`AGENT_MEMORY_REVIEW=off|on`, default `off`): the merge writes a
   pending change instead of applying it, the chat gets the diff, and `/memory approve <id>` or
   `/memory reject <id>` decides.
4. **Provenance.** Each merge is its own commit:
   ```
   memory: session <id> (<source>): saved projects/oam-server/decisions.md, global/lessons.md
   ```
   Daily log and curation get their own commits. `/memory undo <session-id>` reverts that
   session's commit.

## 6. Managing instructions

| Chat | API | Does |
|---|---|---|
| `/prompt show [agent\|workflow]` | `GET /prompt/files/{name}` | show the file |
| `/prompt edit agent` + text | `PUT /prompt/files/{name}` | replace it, committed as the operator |
| `/prompt diff` | `GET /prompt/diff` | compare with the current default |
| `/prompt reset agent` | `POST /prompt/reset` | back to the default (old version in git) |
| `/prompt check` | `GET /prompt/check` | lines that contradict the runtime layer |
| `/prompt preview [message]` | `GET /prompt/preview` | the composed prompt for a session like this: layers, sections, sizes, what the budget cut |

Edits from chat follow the same rules as `/set`. Instructions are never writable by an
agent session, sandboxed or not; a session that wants a workflow change says so in its reply.

## 7. Seeing what a session got

* `/prompt preview` (above).
* The audit log records each session's prompt manifest:
  - layers and sections, with each one's size and hash;
  - budget cuts;
  - the memory scopes used;
  - which memory files the merge saved or refused.

  It records sizes and hashes, not the full text.
* `/memory log [scope]` lists recent memory commits, with their session and source.

## 8. Curation

The curator keeps its role (lessons; compaction over budget) and gains scopes:
- it compacts per scope;
- it may move a note from `global/` to the project it names;
- it never touches instructions or logs.

Its writes are commits like any session's, so they can be undone the same way.

## 9. Later

* **Search instead of dumping:** the runtime layer tells agents memory is a directory they can
  read and `grep`, and the prompt carries only instructions, global memory and a
  table of contents per project. Possibly a small `memory search` helper.
* **agent-stream panel:** browse and edit memory and instructions, approve pending changes, see
  a session's prompt manifest. Built on the §6 API.
* **Expiry:** notes with a date stamp the curator may retire.

## 10. Phases

1. **Runtime layer and slim `agent.md`:** auto-upgrade of untouched defaults, `/prompt show`,
   `/prompt preview`, `/prompt check`. Fixes problem 1 for every instance without hand edits.
2. **One write path:** staged merge for all sessions, commits per session, `/memory undo`,
   `/memory log`, the prompt manifest in the audit log.
3. **Scopes:** the new layout and migration, choosing scopes per session, a per-scope budget,
   filtered Recent Sessions.
4. **Review queue and API:** then the agent-stream panel.

Each phase ships on its own; phase 1 is the smallest and removes the most confusion.

## 11. Open questions

1. **Project key:** the repo name, or a project name the operator assigns, so that one project
   can span oam-server and oam-ops? Proposed: the repo name by default, with optional groups in
   `instructions/projects.md` (`oam: oam-server, oam-ops`).
