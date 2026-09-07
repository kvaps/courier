# courier

A local daemon that carries a question from an agent to a person, and the answer back.

An agent that reaches a decision only you can make has nowhere to put it. It stops, and you find out by opening a session list. courier gives it somewhere to put it: a thread in Telegram, one per agent, that you can also write into at any time — and what you write reaches the agent, waking it if it had stopped.

It is a message daemon with a Kubernetes-shaped API and pluggable transports. Telegram is the first one; nothing above `pkg/backend` knows it exists.

```
      agent ──MCP──►┌─────────┐──telegram──► you
                    │ courier │
      agent ◄─push──└─────────┘◄─────────────
```

## What it looks like

The orchestrator opens a thread for an agent:

```sh
curl -sX POST localhost:7717/api/v1/conversations -d '{
  "metadata": {"name": "externalip-api"},
  "spec": {
    "channel": "telegram",
    "title": "▣ externalip-api",
    "agent": {"sink": "claude", "address": "b3b059e2"}
  }
}'
```

The agent asks its question through MCP, and the thread gets:

```
3/12 — PR #17 (ComputePlane), rewording the overview

A reviewer wants the line about "closing a security gap" dropped — they
read it as convenience, not as protection. Mention security at all?

→ I'd keep one honest sentence: today a catalogue app lands on the shared
  cluster, and ComputePlane puts it in an isolated VM

OK?
```

You reply `OK`. The agent's `ask` call returns with your words. If it had stopped waiting, your message wakes it and arrives as a turn.

## Quick start

```sh
make build
./courier serve --telegram-chat https://t.me/c/4405002039/1
claude mcp add --transport http courier http://127.0.0.1:7717/mcp
```

The bot token is read from `~/.claude/channels/telegram/.env` (`TELEGRAM_BOT_TOKEN`), or from the environment, or from `--telegram-token-file`. It is never a flag and never part of a resource — see [The token](#the-token).

The bot must be an **administrator** of the group, with topics enabled. Administrator is not about creating topics: a bot with privacy mode on sees only messages addressed to it, and administrator rights are what let it read your plain replies. (Verified rather than assumed: with `can_read_all_group_messages: false`, an admin bot still receives every message in the group.)

## The MCP tools

| tool | who calls it |
|---|---|
| `open_conversation` | the orchestrator, once per agent |
| `ask` | an agent with a decision to put to you — sends and waits |
| `wait_for_answer` | an agent picking up a question `ask` left open |
| `send` | an agent with something to say that needs no answer |
| `receive` | an agent collecting what you wrote unprompted |
| `cancel` | withdrawing a question that stopped mattering |
| `list_conversations`, `get_conversation`, `close_conversation`, `list_messages`, `get_message`, `list_channels` | reading the record |

`ask` takes `context`, `question` and `proposal` as separate fields rather than one string. That is the whole design: an agent handed a free-form body writes a wall of jargon about a thread you have not seen in a week, and the shape of the input is the cheapest place to prevent it. `context` re-orients you in a line, `question` is a plain either/or, `proposal` is the agent's own recommended answer so you can reply with one word, and `progress` shows the end coming.

**One question at a time per conversation.** You answer by writing in the thread, not by quoting, so two open questions would mean the daemon guessing which one a bare `OK` belongs to. A second `ask` is refused, naming the first. An agent that no longer needs an answer calls `cancel`, which edits the message you are looking at so a dead question stops looking live.

## The API

Kubernetes-shaped: `{apiVersion, kind, metadata, spec, status}`, lists carrying a `resourceVersion`, `Status` error bodies with machine-readable reasons, optimistic concurrency on update, and a watch that resumes from a version.

```
GET  /api/v1                          a prose index of every route
GET  /api/healthz                     health, and which channels are degraded
     /api/v1/channels                 transports
     /api/v1/conversations            threads          + /{name}/close
     /api/v1/messages                 messages         + /{name}/cancel, /{name}/answer
GET  /api/v1/watch                    every kind, ?resourceVersion=N&kind=Message
POST /mcp                             the tool set, over streamable HTTP
```

Three kinds. A **Channel** is a configured transport. A **Conversation** is one thread — with a person on one side (`spec.channel`) and, optionally, a machine on the other (`spec.agent`). A **Message** is one item in it; a question is a message with `spec.awaitReply`, and your reply lands in its `status.answer`.

Watch is chunked JSON, one event per line — the format that needs no dependency and no protocol upgrade, so `curl -N` follows it:

```sh
curl -sN 'localhost:7717/api/v1/watch?kind=Message'
```

List first, then watch from the list's `resourceVersion`, and you see every later change exactly once. A version the store no longer retains is refused with a `Conflict` rather than served a stream with a silent hole in it.

There are no namespaces. courier is one person's local daemon, and a namespace would be ceremony with nothing on the other side of it.

## Two extension points

They are deliberately symmetric: one reaches the human, one reaches the machine, and a conversation names one of each.

**`pkg/backend` — transports.** Open a thread, send, edit, react, and hand inbound messages to a sink. A new transport is a package that registers itself; nothing in the daemon changes. `pkg/backend/telegram` is the first.

**`pkg/agent` — delivery into an agent.** Push a message to the machine side of a conversation. Without one, an agent only sees your message the next time it calls `receive`, which for an agent in the middle of a long turn can be a while.

`pkg/agent/claude` speaks the local Claude Code daemon's control socket — the same socket and ops the `claude` CLI uses to hand text to a background session. Three things make it the right channel:

- **It needs no approval.** The daemon's reply handler either delivers or returns `ENOJOB` / `ERESPAWNING` / `ENOREPLY`; it raises no dialog. The approval prompt you may have seen — `approve message from uds:/tmp/cc-socks/…` — belongs to Claude Code's peer-to-peer session inbox, which gates unknown writers on purpose. courier does not speak that protocol: a channel you have to approve message by message is not a channel.
- **It reaches sessions that are not running.** A stopped-but-resumable session is woken in place, keeping its history, before the message is delivered.
- **It confirms delivery against the session's own transcript.** An acknowledgement only means the text reached an input box; a long message lands there as an unsubmitted paste. courier looks for the message in the session's conversation, presses Enter if it is not there, and refuses to press Enter at all when a dialog holds the keyboard — a blind Enter on the resume dialog would accept "compact this conversation" and discard the message with it.

courier does not impersonate a session. It could — the sender identity Claude Code exposes is just environment variables — but the return address would be a lie, and the agent would answer into something nobody is listening on. It writes a plain envelope instead, naming you and the tool to answer with.

## The token

It is read from the environment or a `KEY=VALUE` file, and from nowhere else. It is never a command-line flag, so it cannot appear in `ps`; a channel config carrying a literal `token` is **refused**, because the object is served by the API and written to disk. The token sits in the Bot API's request path, so `net/http` quotes it in its own errors — every string this daemon returns is scrubbed of both the whole token and its secret half before it can reach a log.

## One poller per token

**Telegram allows exactly one `getUpdates` consumer per bot token.** Everything else here is forgiving; this is not, and it is the failure that looks like an outage and is not.

The Claude Code Telegram channel plugin polls with the token in `~/.claude/channels/telegram/.env` whenever a session has that channel enabled. If courier reads the same file, the two fight: whoever polls second gets `409 Conflict`, and inbound messages are split arbitrarily between them. The log names this case explicitly. The way out is to give courier its own bot — create one in @BotFather, add it to the group as an administrator, and start courier with `TELEGRAM_BOT_TOKEN` set in its environment.

Sending is unaffected either way; several bots may post into the same group.

## Restarts

Telegram holds undelivered updates for 24 hours, so a **first** start skips the backlog: replaying a day of group chatter into live agents would deliver yesterday's conversation as today's answers. Later starts resume from the saved offset, so a message sent while the daemon was down is still delivered. The offset advances only after a message has been handled.

Everything else lives in `~/.courier` as JSON, one file per object, written by rename. Conversations, questions and answers survive a restart; so does the resource version, so a resumed watch is never handed a number that was already used.

## Layout

```
pkg/api        resource types, the Status error object, the message renderer
pkg/store      JSON files + an index, resourceVersion, optimistic concurrency, watch
pkg/backend    the transport interface and registry   → telegram
pkg/agent      the delivery interface and registry    → claude
pkg/courier    the rules: threads, questions, answers, withdrawal, pushing
pkg/mcpserver  the MCP tool set, in-process over pkg/courier
internal/server the HTTP API, in-process over pkg/courier
cmd/courier    flags and wiring
```

`pkg/mcpserver` and `internal/server` are siblings, not layers: both construct a `courier.Service` and call it in-process, so a tool call and a REST call take the same path, write the same store and reach the same watchers.

MCP is served by the daemon rather than by a separate `courier mcp` process, and that is not an accident. The store is a single-writer index over a directory; a second process opening it would be a second truth.

```sh
make all     # tidy, lint, test, build
```

## License

Apache-2.0. See [LICENSE](LICENSE).
