# Repos and Their Credentials — Design

Status: steps 1 and 2 implemented (`internal/repos`, `/repo`, `/repos`); SSH keys as secrets (step 3) are not. Replaces `GIT_HOST`, `GIT_ORG`, `GIT_TOKEN`, `GIT_SSH_KEY`,
`AGENT_SHARED_REPOS` and `ALLOWED_PROJECTS`. No compatibility with them is kept.

## 1. Principle

**`.env` configures agent-runner itself. The work comes from the people using it.**

| Belongs to | Examples | Set in |
|---|---|---|
| agent-runner | model keys, bot tokens, sandbox, limits, the memory repo (`MEMORY_GIT_*`) | `.env` |
| the agent's users | which repos to work on, the credentials for them, other task credentials | chat (`/repo`, `/secret`) or the API |

Today project repos are agent-runner config:
- one host and organization for every repo (`GIT_HOST`, `GIT_ORG`);
- one token for all of them, logging in as `oauth2` (`GIT_TOKEN`);
- a fixed list of repos in every workspace (`AGENT_SHARED_REPOS`).

That can't express "oam-server on Gitee as user X with this token, plus a public GitHub repo".
Changing anything means editing `.env` and restarting.

## 2. The repo registry

`DATA_DIR/repos.json`, managed with `/repo` (and `GET/POST/DELETE /repos`). Each entry:

| Field | Meaning |
|---|---|
| `name` | short name, unique; the folder in every workspace (`oam-server`) |
| `url` | clone URL, as the user gave it, without a password: `https://user@gitee.com/org/oam-server.git`, `https://github.com/org/x.git`, `git@host:org/x.git` |
| `secret` | the `/secret` holding its password or token; empty for a public repo |
| `branch` | the branch sessions start on; default: the remote's default branch |
| `push` | `true` (default): agent-runner pushes the session's commits. `false`: read-only, never pushed |

```
/repo add https://lei@gitee.com/org/oam-server.git secret=GITEE_TOKEN
/repo add https://github.com/org/docs.git push=false
/repo list                      name, url, branch, secret name, push, last fetch
/repo set oam-server branch=develop
/repo remove oam-server
```

`/repo add` clones the repo right away, so a wrong URL or token fails at once, in the reply,
not in the next session.

## 3. Credentials

* **Source:** the named `/secret`. It is the user's, like any agent secret: it is also in the
  agent's environment and named in its prompt. There is one place for user credentials, and no
  separate repo-token store.
* **Login name:** from the URL (`https://lei@…`). Without one, `oauth2`, which GitLab, Gitea and
  GitHub tokens accept. Gitee and others that need the real user name get it in the URL.
* **Use on the host:** for each git command agent-runner runs in that repo (clone, fetch, push),
  a credential helper reads the token from that command's own environment. It is never written to
  git config, a URL, a log or the registry. `GIT_TERMINAL_PROMPT=0`, so a bad token fails fast.
  Each repo gets its own token; nothing is shared across hosts.
* **SSH** (`git@…` URLs): the secret holds a private key. Agent secrets become allowed to span
  lines (a key is several), and agent-runner writes the key to a 0600 file outside the sandbox
  for the one command and removes it after.

## 4. Workspaces

* **Every registered repo is in every session's workspace,** copied from the repo cache and fast
  forwarded to its branch before the session.
  - Most agents have a few repos. A per-task selection (the planner choosing, or a task naming
    repos) can come later if workspaces get too big.
* **The repo cache:** one clone per registered repo, in `REPO_CACHE_ROOT/<name>`. It is
  refreshed by fetching, never written back from a workspace. That ends today's
  "cache-back", which copied whatever a session left (agent-written config, half-finished work)
  into the next session's start.
* **Git config:** written by agent-runner from the registry when the workspace is made, saved,
  and put back before every host-side git step (as now). A repo that is not in the registry,
  cloned or made by the agent, is never pushed or cached.
* **What the agent is told:** the runtime section lists each repo:
  - its folder and URL;
  - its branch;
  - whether agent-runner pushes it;
  - "to work on another repo, ask the user to `/repo add` it".

  Agents cannot add repos themselves: what agent-runner pushes, and with which credentials, is
  the user's call.

## 5. Pushing

Unchanged in principle: the agent commits, and agent-runner pushes at session end to the branch
the agent is on. It rebases on a moved remote, gives the agent one turn to resolve conflicts, and
parks unmergeable work on `agent-rescue/<session>`. The differences:
- it uses the repo's own credentials;
- it skips `push=false` repos.

An authentication failure becomes a session warning naming the repo and its secret
(`oam-server: push refused, check /secret GITEE_TOKEN`).

The agent could push itself, since the token is in its environment and egress allows the host.
The runtime section tells it not to, so pushes stay in one place, with conflict handling and the
saved config. This is a convention, not enforcement, which fits secrets the operator decided not
to hide.

## 6. What goes away

* `GIT_HOST`, `GIT_ORG`, `GIT_TOKEN`, `GIT_SSH_KEY`, `AGENT_SHARED_REPOS`, `ALLOWED_PROJECTS`
  are no longer read. A startup warning names any that are still set.
* Cache-back after sessions and tasks.
* The one-shot job API's (`POST /run`) project resolution moves to the registry: `project` is a
  registered repo's name.
* `/repo update`, which becomes a fetch that `/repo add` and every session already do.

## 7. Plan

1. **Registry and credentials:**
   - `repos.json`, `/repo add|list|set|remove`, `/repos` API;
   - per-repo credentials from secrets over HTTPS;
   - the old settings stop being read.
2. **Workspaces and pushing:** every registered repo in every workspace from a fetched cache, no
   cache-back, per-repo push credentials and `push=false`, the runtime section listing repos.
3. **SSH keys as secrets.**

Steps 1 and 2 ship together (2 is what makes 1 useful). Step 3 is needed only for a host with no
HTTPS tokens.
