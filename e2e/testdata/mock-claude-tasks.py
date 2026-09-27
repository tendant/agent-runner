#!/usr/bin/env python3
"""Fake `claude` CLI for the multi-turn task e2e test (tasks_stream_e2e_test.go).

agent-runner runs with no LLM API key, so every role goes through this CLI:
the intent router, the task-message classifier, the planner (and its revise
mode) and the coding agent itself. Each call is recognised by a marker in its
prompt and answered with a stream-json result line, like the real CLI.
Every invocation's argv is appended to $MOCK_CLAUDE_LOG as a JSON line.
"""
import json
import os
import sys

args = sys.argv[1:]
if "--version" in args:
    print("2.1.0 (Claude Code)")
    sys.exit(0)

log = os.environ.get("MOCK_CLAUDE_LOG")
if log:
    with open(log, "a") as f:
        f.write(json.dumps({"cwd": os.getcwd(), "args": args}) + "\n")

prompt = args[-1] if args else ""
system = args[args.index("--system-prompt") + 1] if "--system-prompt" in args else ""
text = system + "\n" + prompt


def result(s):
    print(json.dumps({"type": "result", "subtype": "success", "is_error": False,
                      "result": s, "total_cost_usd": 0.001}))
    sys.exit(0)


def last_line_after(marker):
    lines = [l for l in text.splitlines() if l.startswith(marker)]
    return lines[-1][len(marker):].strip() if lines else ""


def is_small_talk(msg):
    return msg.lower().strip(" !.") in ("thanks", "thank you", "great", "ok")


if "You are a conversation router" in text:
    msg = last_line_after("user:")
    if is_small_talk(msg):
        result(json.dumps({"action": "ask", "message": "You're welcome!"}))
    result(json.dumps({"action": "execute", "message": "On it."}))

if "Decide whether their new message continues" in text:
    msg = last_line_after("New message:")
    kind = "chat" if is_small_talk(msg) else "continue"
    result(json.dumps({"kind": kind}))

if "revising the plan of a task" in text:
    result(json.dumps({"summary": "deploy the app", "approach": "script", "steps": [
        {"id": "1", "description": "prepare the deploy", "done": True},
        {"id": "2", "description": "deploy to the chosen environment", "done": True},
        {"id": "3", "description": "also deploy to prod", "done": False},
    ]}))

if "You are a planning agent" in text:
    result(json.dumps({"summary": "deploy the app", "approach": "script", "steps": [
        {"id": "1", "description": "prepare the deploy", "done": False},
        {"id": "2", "description": "deploy to the chosen environment", "done": False},
    ]}))

if "Summarize the following conversation" in text:
    result("A deploy task.")

# The coding agent, running in the task workspace.
def write_progress(p):
    with open("_progress.json", "w") as f:
        json.dump(p, f)

if "User's feedback on the task" in prompt:
    with open("deploy.txt", "a") as f:
        f.write("prod\n")
    write_progress({"completed_steps": ["1", "2", "3"], "status": "done",
                    "summary": "also deployed to prod"})
    result("Deployed to prod too.")

if "User's reply: staging" in prompt:
    scratch = "kept" if os.path.exists("notes.txt") else "missing"
    with open("deploy.txt", "w") as f:
        f.write("staging (scratch %s)\n" % scratch)
    write_progress({"completed_steps": ["1", "2"], "status": "done",
                    "summary": "deployed to staging"})
    result("Deployed to staging.")

# First turn: leave scratch work behind and ask a question.
with open("notes.txt", "w") as f:
    f.write("half-done deploy prep\n")
write_progress({"completed_steps": ["1"], "status": "needs_input",
                "question": "Which environment should I deploy to?",
                "summary": "prepared the deploy",
                "decisions": ["deploy with the existing script"]})
result("Prepared; need the target environment.")
