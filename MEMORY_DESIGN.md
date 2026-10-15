# Memory and Prompt Management — Design

Status: phases 1 to 3 implemented (runtime layer, slim `agent.md` with upgrade of unedited
defaults, `/prompt show|check|preview`; one write path through a checked copy, a commit per
step, `/memory log|undo`, prompt and memory in the audit log; topic files indexed, Recent Sessions
bounded and same-thread first); phase 4 is a proposal. Current behaviour: DESIGN.md "Memory & Prompt Composition".

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
| **Memory** | agents, curated | `memory/*.md` (flat) | sessions save notes; the curator compacts |

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

## 4. What a session loads

The memory dir stays flat: no scopes, no migration. Each agent has its own memory, usually for
one area of work, so splitting it again by project adds more than it fixes. What changes is
what goes into the prompt:

* **In full:** the well-known files (`user_preferences.md`, `project_summary.md`,
  `decisions.md`, `workflows.md`, `lessons.md`) and upper-case files (the operator's, such as
  `SOUL.md`). The curator keeps the well-known ones under budget.
* **As an index:** every other lowercase `.md` file, the agent's topic files (`oam_server.md`),
  is one line of a `## Memory Files` section: path, size and first line. The runtime layer tells
  the agent to start a topic file with a one-line summary and to read it when a task touches
  the topic. Memory can grow without crowding the prompt.
* **Recent Sessions:** at most 10 entries from the last `AGENT_MEMORY_DAYS`, those of the same
  thread first (the task, else the chat; each daily-log entry records it as a hashed
  `**Thread:**` line), then the newest others. With several people on one agent, other threads'
  sessions are mostly noise.
* **Budget:** as before for full files (cut with a note pointing at the file). Index lines are
  never cut. `/prompt preview` shows what is in full and what is indexed.

What it gives up: an indexed file is read only if the agent chooses to. The core files and the
first lines cover the common case; a topic every session needs can be named in `agent.md`.

## 5. Writing memory

Every session, sandboxed or not, writes through a staged copy. It extends today's sandboxed
merge, which becomes the only path.

1. **Stage.** Copy the memory dir's top-level files into a per-run directory and point
   `{{MEMORY_DIR}}` at it.
2. **Merge** at session end, under the memory lock:
   - **Allowed:** top-level lowercase `.md` files.
   - **Append-only:** `lessons.md`.
   - **Refused:** instructions, logs, deletions, links and binaries.
   - **Limits:** size and new-file caps.
   - **Three-way:** a file someone else changed meanwhile takes only a pure append.
   - The runtime layer tells the agent which files to use, that topic files go in their own
     file with a one-line summary first, and that several people share this memory.
3. **Optional review queue** (`AGENT_MEMORY_REVIEW=off|on`, default `off`): the merge writes a
   pending change instead of applying it, the chat gets the diff, and `/memory approve <id>` or
   `/memory reject <id>` decides.
4. **Provenance.** Each merge is its own commit:
   ```
   memory: session <id> (<source>): saved decisions.md, oam_server.md
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
  - which memory files the merge saved or refused.

  It records sizes and hashes, not the full text.
* `/memory log` lists recent memory commits, with their session and source.

## 8. Curation

The curator keeps its role (lessons; compaction of well-known files over budget) and never
touches instructions or logs. Its writes are commits like any session's, so they can be undone
the same way.

## 9. Later

* **Search:** if the topic index grows past what a line each can carry, a small `memory search`
  helper, or the curator merging related topic files.
* **agent-stream panel:** browse and edit memory and instructions, approve pending changes, see
  a session's prompt manifest. Built on the §6 API.
* **Expiry:** notes with a date stamp the curator may retire.

## 10. Phases

1. **Runtime layer and slim `agent.md`:** auto-upgrade of untouched defaults, `/prompt show`,
   `/prompt preview`, `/prompt check`. Fixes problem 1 for every instance without hand edits.
2. **One write path:** staged merge for all sessions, commits per session, `/memory undo`,
   `/memory log`, the prompt manifest in the audit log.
3. **Load less, read on demand:** core files in full, topic files as an index, Recent Sessions
   bounded and same-thread first (§4). Replaces the earlier plan of per-project scopes.
4. **Review queue and API:** then the agent-stream panel.

Each phase ships on its own; phase 1 is the smallest and removes the most confusion.
