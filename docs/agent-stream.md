# Connecting to Agent Stream

[Agent Stream](https://apps.apple.com/us/app/agent-stream/id6759258538) is an iOS app for conversational access to your agent. It lets you send messages, receive streaming responses, and get file attachments back from the agent.

To connect agent-runner, you need three values from the app:

1. **`STREAM_SERVER_URL`** — your Agent Stream server URL. Set it in the app via the gear icon on the login screen.

2. **`STREAM_BOT_TOKEN`** — create a bot in the app under Menu → Bots → tap `+`. The token is shown once after creation — copy it immediately.

3. **Add the bot to a channel** in the app. By default the bot follows its memberships: it listens on every channel it's a member of and notices being added to (or removed from) a channel within `STREAM_CHANNEL_DISCOVERY_INTERVAL` (default `30s`), with no restart. To pin it to specific channels instead, set **`STREAM_CHANNEL_IDS`** (IDs start with `c_`; `STREAM_CONVERSATION_IDS` is still read as a fallback).

**One agent-runner per bot.** Each process names itself to agent-stream (`X-Bot-Instance`), which allows one live process per bot: if you start a second agent-runner with the same bot token, it takes over and the first one logs "another process is now running this bot" and stops its stream bot, so nothing is answered twice. To run two agents in a channel, give each its own bot.

**Who the bot answers.** agent-stream decides who each message is for and lists those bots in the message's `addressees`: a mentioned bot, else the thread's assignee (for replies), else the channel's default bot (for new threads) — see agent-stream's `BOT_ADDRESSING_DESIGN.md`. The bot acts only on messages that list it, so bots sharing a channel answer only what is meant for them, and bot-to-bot exchanges happen only by mention (the server stops long chains). A run ends with its result posted as a message carrying the run's `run_id`.

Each top-level message in a channel starts a Thread, and the bot keeps separate context, plan and agent session for every thread. Replies inside a thread continue that thread's work, so several tasks can run side by side in one channel.

Threads are handled in parallel: messages within one thread are processed in order, while a slow step in one thread (an analyzer call, a file download) doesn't hold up another; at most 8 threads are handled at once. Agent sessions themselves are limited by `AGENT_MAX_CONCURRENT` (default `1`), so with the default a second thread's task still queues behind the first. Set it to the number of tasks you want running at the same time, e.g. `AGENT_MAX_CONCURRENT=3` (see [Running sessions in parallel](sessions.md#running-sessions-in-parallel)).

```bash
STREAM_SERVER_URL=https://your-agent-stream-server
STREAM_BOT_TOKEN=your-bot-jwt
# STREAM_CHANNEL_IDS=c_your_channel_id   # optional: pin to these channels
```
