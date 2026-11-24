# courier

A local daemon that carries a question from an agent to a person, and the answer back.

An agent that reaches a decision only you can make has nowhere to put it. It stops, and you find out by opening a session list. courier gives it somewhere to put it: a post in a Telegram channel, one per question, with the replies under it as comments. Whatever you write under a post reaches the agent that owns it, waking it if it had stopped — you never pick a recipient, find the agent, or tidy up per-agent threads.

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
./courier serve --telegram-chat https://t.me/c/1234567890
claude mcp add --transport http courier http://127.0.0.1:7717/mcp
```

The bot token is read from `~/.claude/channels/telegram/.env` (`TELEGRAM_BOT_TOKEN`), or from the environment, or from `--telegram-token-file`. It is never a flag and never part of a resource — see [The token](#the-token).

The chat is a **channel with a discussion group linked** (channel settings → Edit → Discussion; neither the Bot API nor a user client can link one, so this is done by hand once). The bot must be an **administrator of both**: of the channel to post and edit, and of the discussion group to read your replies — a bot with privacy mode on sees only messages addressed to it, and administrator rights are what let it read plain comments. courier refuses to start against a chat that is not a channel, or a channel without a discussion group.

### How the feed is laid out

- **A question is a post.** One problem, one post, with a notification. The first line names the agent (the conversation's title).
- **Replies are comments under the post.** Text, voice or files. A reply goes to the conversation that owns the post — the thread it is written in decides, never the words.
- **A note that is not a question** goes as a silent comment under the agent's latest post while that post is under 12 hours old, and as a silent post of its own otherwise.
- **A drafted answer** goes as a comment under the question's post, with its buttons.
- **An answered or withdrawn question is rewritten in place**, so the feed shows at a glance what is still open.
- **A post you write in the channel yourself** — text, voice or a forward — starts a conversation of your own. It goes to the channel's `defaultConversation` (typically the orchestrator's), and so does every reply under it; the agent's answer lands in that post's comments. Without `defaultConversation` such posts are ignored.
- **Text is sent as Markdown** (Bot API rich messages), so the bold, lists, tables and code agents write arrive formatted. If Telegram refuses the markup, the same text goes out plain: a stray character costs the formatting, never the message.
- **Settled posts leave the feed.** A post with no open question and nothing new written under it for `retentionHours` (default 12) is deleted, and its comment thread with it. Open questions and the board are never swept. The record stays in courier — the channel is a working feed, not an archive. Telegram lets a bot delete only messages younger than 48 hours; a post that settles later is kept, already rewritten as settled.
- A comment written outside any post's thread is not delivered anywhere; the bot replies asking you to write under the post, or to start a post in the channel.
- Commenting as an anonymous administrator works: Telegram reports such a message as written by the group, which only an administrator can do.

Telegram links a post to its comments only through the discussion group's automatic copy of the post; courier keeps that map in its state directory (`feed.json`) so it survives a restart.

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

Drawing approval buttons is **not** in this table, and that is the point — see [Answering with one tap](#answering-with-one-tap).

`ask` takes `context`, `question` and `proposal` as separate fields rather than one string. That is the whole design: an agent handed a free-form body writes a wall of jargon about a thread you have not seen in a week, and the shape of the input is the cheapest place to prevent it. `context` re-orients you in a line, `question` is a plain either/or, `proposal` is the agent's own recommended answer so you can reply with one word, and `progress` shows the end coming.

**Files travel both ways.** `send` and `ask` take `files` — absolute paths on this machine, uploaded so the reader gets the screenshot or the log itself rather than a path they cannot open from a phone. A message may be nothing but a file. In the other direction, a file the person sends is downloaded before the agent hears about it: the message's `spec.attachments[].path` is an ordinary local file, and the envelope the agent receives names it. Telegram caps a bot at 50 MB up and 20 MB down; courier refuses anything larger up front, naming the file, rather than failing mid-upload.

A voice message is the exception, and it is not downloaded at all — see [Answering out loud](#answering-out-loud). Its bytes are not the content; what was said is, and an agent has no ears.

A received filename is a hint and never a path. Courier builds the name on disk itself — separators removed, prefixed with the file's id so two people sending `screenshot.png` do not overwrite each other — because these paths are handed to agents.

**One question at a time per conversation.** You answer by writing in the thread, not by quoting, so two open questions would mean the daemon guessing which one a bare `OK` belongs to. A second `ask` is refused, naming the first. An agent that no longer needs an answer calls `cancel`, which edits the message you are looking at so a dead question stops looking live.

## The board

A transport that can keep one standing message current gets a **board**: a pinned post, edited in place and never notifying, that says what is waiting on the reader (with links to the posts, oldest first), what the reviewer answered on their behalf in the last 24 hours, and how many questions the reviewer is looking at. It is the pull half of the feed: instead of a sound per question, one place to look. Comments under the board go to the channel's `defaultConversation`, which is how the reader disputes an answer given for them. The board is recomputed every 30 seconds and touched only when it changed.

## A reviewer before the reader

A channel can name a **reviewer** — a conversation whose agent, typically the orchestrator, sees every other agent's question before the reader does:

```json
{"spec": {"backend": "telegram", "reviewer": "mwo-orchestrator", "reviewTimeoutSeconds": 600, "config": {…}}}
```

A question is then **held**: recorded, open, and the asking agent waits on it as usual, but nothing is posted. The reviewer's session is handed the question with two calls it can make:

- `POST /api/v1/messages/{name}/answer` `{"text", "by"}` — settle it on the reader's behalf. The agent is told whose answer it is; the reader never sees the question.
- `POST /api/v1/messages/{name}/publish` `{"by", "body"}` — show it to the reader, rewritten so it reads cold, or unchanged without `body`. The agent's own words stay on the record in `status.review.original`.

A held question the reviewer does not act on within the timeout (default 10 minutes) is published as it is, with `status.review.action: timeout`: a reviewer that is down costs the reader a delay, never a question. Timeouts survive a restart. The reviewer's own questions are not held, and neither are drafted answers or notes. `answer` also works on a question already published — the reviewer answering for the reader — and rewrites the post to show who answered.

## Answering with one tap

Composing the answer is the expensive half. You read the question on a phone, between ten other agents, about a thread you have not seen in a week — and then you have to write something. So the orchestrator writes it for you, and you say yes.

An agent asks the way it always did. The orchestrator, which is watching the queue anyway, drafts the answer and posts it under the question with buttons:

```sh
curl -sX POST localhost:7717/api/v1/messages/externalip-api-7/draft -d @draft.json
```

```json
{
  "draftedBy": "orchestrator",
  "choices": [
    {"id": "send",   "label": "Send",   "answer": "Keep one honest sentence: today a catalogue app lands on the shared cluster, and ComputePlane puts it in an isolated VM."},
    {"id": "refuse", "label": "Refuse", "answer": "Drop the security framing entirely - describe what it does and let the reader draw the conclusion."}
  ]
}
```

The question's post gets a comment with the draft:

```
Draft by orchestrator:

▸ Send — Keep one honest sentence: today a catalogue app lands on the shared
  cluster, and ComputePlane puts it in an isolated VM.

▸ Refuse — Drop the security framing entirely — describe what it does and let
  the reader draw the conclusion.

Tap one — or write your own answer.

           ┌──────────┐
           │   Send   │
           ├──────────┤
           │  Refuse  │
           └──────────┘
```

You tap `Send`. The agent's `ask` returns with the whole drafted sentence as its answer, the message is rewritten in place to say what you chose, and the buttons go away.

**Editing a message raises no notification**, which is worth knowing because it shaped two things. A pop-up over the button confirms the tap immediately — the insistent kind you dismiss, not the banner that fades, because a press that seems to do nothing gets pressed again. And the outcome is written at the *top* of the rewritten message, not appended after the options, so it is still the first thing you read when you come back to the thread an hour later. Both were learned the first time somebody used this and said "I don't see any reaction".

**Every option's words are on screen, never only its label.** A button says "Send"; a reader who taps it without having seen what gets sent has approved nothing. Those words are also why the button cannot carry them: Telegram allows a button 64 bytes of data, so it carries a handle courier mints and the answer stays in the Message resource — which is what lets the whole offer, and not only the option taken, be read back afterwards.

**The answer says whose words it is.** `status.approval` names the drafter, the option taken and the draft it came from, and the text delivered into the agent's session says it in a sentence: *these are not their own words, the orchestrator drafted them and the operator confirmed the draft by choosing "Send".* That is not politeness. A one-tap approval is cheap, and a cheap approval becomes reflexive; when a decision turns out to have been wrong, the record has to lead back to whoever actually composed it rather than crediting you with authorship you never had.

**Refusing is answering, and it is not `cancel`.** `Refuse` settles the question with real instructions the agent can act on — *don't do that, do this instead*. `cancel` is the asker withdrawing a question that stopped mattering, and it leaves nothing behind. So every option must carry an answer: a button that answered nothing would let a question be closed with no decision behind it, and the queue would look shorter than it was.

**Silence is never consent.** Nothing expires, and no timeout answers on your behalf. A question you have not tapped stays open however long that takes, because asleep, busy and unconvinced all look identical from here. What the daemon does instead is say how long it has been waiting: `status.pendingQuestionSince` on the conversation, and `/api/healthz` computing the durations, longest first, so the orchestrator can re-raise a stale question or withdraw it.

```sh
curl -s localhost:7717/api/healthz | jq .waiting
[{"conversation":"externalip-api","message":"externalip-api-7","summary":"Mention security at all?","seconds":8140,"draft":"externalip-api-9"}]
```

**Writing still works, and takes the draft down.** Buttons are an offer laid over the ordinary way of answering, never a replacement for it. Type a reply and it settles the question exactly as before — recorded as your own words, with no approval on it — and the draft is struck where it stands, so a decided question never keeps a live keyboard under it. The same happens when the asker withdraws the question.

**Only the orchestrator draws buttons.** Drafting is an HTTP route and deliberately has no MCP tool, because the MCP tool set is what an agent is handed. Keep it that way: put it in the tool set and the gate is gone.

What that buys is narrower than it first looks, and worth stating exactly. It is not a rule that the drafter must be someone other than the asker — an asker proposing its own answer is the design already, and `ask` has carried `proposal` from the start. A button does not change who proposes; it changes how cheap accepting is. The line is *which* askers: the workers, of which there are twenty and none of them supervised, are the ones whose work an answer authorises, and an agent supplying both the question and the approval of its own reasoning leaves your judgement with nothing to do. The orchestrator is one party, watched, and already the instrument you drive the fleet with — so it may put a question it asked itself on a button, and does. The record says `draftedBy` either way, which is what makes a reflexive approval traceable afterwards.

**Pressing is driving an agent, so `allowFrom` covers it.** A tap arrives as a `callback_query` with its own sender, not as a message, and it is checked against the same allow-list. Without that, anyone who can see the group could close another person's question with a thumb. The group is two people today, which is exactly the kind of fact that quietly stops being true.

**Buttons are optional.** A question asked without a draft is sent, rendered and answered exactly as it was before any of this existed.

## Answering out loud

Talking is faster than typing, especially on a phone, so courier turns a voice message into words. The audio is never downloaded: an agent cannot open an `.oga`, and a copy on disk would be a file nothing ever reads.

The bot cannot do this itself. Telegram transcribes for a *user account*, and courier is a bot — so it asks something that already has that account's session, over MCP:

```sh
courier serve --telegram-chat … --telegram-transcribe http://127.0.0.1:8787/mcp
```

That endpoint is [mcp-tg](https://github.com/lexfrei/mcp-tg), whose `tg_messages_transcribe_audio` takes a chat and a message id. Point courier at a client that is already running rather than starting one: two clients on one Telegram account is how you earn `AUTH_KEY_DUPLICATED`. The account needs Telegram Premium for transcription, and it has to be in the group — otherwise the message id resolves to nothing.

**A transcript never settles a question by itself.** Recognition of technical speech is wrong in exactly the places that matter — identifiers, version numbers, and the difference between "send it" and "don't send it", which is one short word. So when the thread is waiting on an answer, what you said comes back as a draft with one button:

```
Draft by speech recognition:

▸ Send as my answer — keep one honest sentence about isolation

Tap one — or write your own answer.

           ┌────────────────────┐
           │ Send as my answer  │
           └────────────────────┘
```

Tap it and it becomes the answer, recorded as an approval: `draftedBy: speech recognition`, confirmed by you. Say it wrong and you fix it the way you fix any draft — type the correction, which answers the question and takes the offer down. There is no second button for that, because typing already does it.

**Which agent hears it is decided by the post it is spoken under, never by the words.** A misheard sentence can be wrong about anything except where it goes.

When nothing is waiting on an answer there is nothing to confirm, and the words travel as they are — marked as heard rather than typed, with the envelope telling the agent to read an odd word as an odd word rather than as an instruction.

**A voice message that cannot be transcribed is not delivered, and you are told so in the thread.** Premium missing, Telegram still working on it, or nothing configured at all — each says which, where you spoke. Silence would be the worst of the available outcomes: you would believe you had answered.

Transcription happens on the receive loop, so its wait is time no other message is being read. Twenty seconds by default, and `--telegram-transcribe-wait` moves it; a note that has not come back by then is better reported than waited on.

## The API

Kubernetes-shaped: `{apiVersion, kind, metadata, spec, status}`, lists carrying a `resourceVersion`, `Status` error bodies with machine-readable reasons, optimistic concurrency on update, and a watch that resumes from a version.

```
GET  /api/v1                          a prose index of every route
GET  /api/healthz                     health, and which channels are degraded
     /api/v1/channels                 transports
     /api/v1/conversations            threads          + /{name}/close
     /api/v1/messages                 messages         + /{name}/cancel, /{name}/answer, /{name}/draft
GET  /api/v1/watch                    every kind, ?resourceVersion=N&kind=Message
POST /mcp                             the tool set, over streamable HTTP
```

Attachments are paths, not uploads: both sides of a conversation share a filesystem, so the API moves file names and the daemon moves the bytes across the transport in between. There is no blob store to run.

Three kinds. A **Channel** is a configured transport. A **Conversation** is one thread — with a person on one side (`spec.channel`) and, optionally, a machine on the other (`spec.agent`). A **Message** is one item in it; a question is a message with `spec.awaitReply`, and your reply lands in its `status.answer`. A message with `spec.choices` is a drafted answer to one of those questions, and `status.approval` on the answer records that you confirmed somebody else's words rather than writing your own.

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

`pkg/agent/claude` delivers through Claude Code's own hooks — nothing types into a terminal and nothing speaks an undocumented socket:

- **A message goes into the session's mailbox** (`~/.courier/mailbox.json`, so it survives a restart).
- **The session collects it.** Every session runs an `asyncRewake` hook that long-polls `POST /api/v1/sessions/{session}/collect` and, when a letter is there, exits with code 2 — which Claude Code documents as waking the session, even an idle one, and handing it the hook's output. The hook is started on `SessionStart`, `UserPromptSubmit` and `Stop`, one per session, so a busy session collects mid-turn and an idle one right away. The hook lives in the orchestrator's repository (`hooks/attention_hook.py`) and is installed in the user's `settings.json`.
- **A session nobody collects for within 20 seconds** is looked up with `claude agents --json`. A busy one is left alone — its next hook collects. An idle or stopped one is restarted with `claude respawn`, which keeps its id and history, and its `SessionStart` hook collects. If even that does not collect within 90 seconds, the delivery fails and you are told in the thread.

courier does not impersonate a session. It could — the sender identity Claude Code exposes is just environment variables — but the return address would be a lie, and the agent would answer into something nobody is listening on. It writes a plain envelope instead, naming you and the tool to answer with.

## The token

It is read from the environment or a `KEY=VALUE` file, and from nowhere else. It is never a command-line flag, so it cannot appear in `ps`; a channel config carrying a literal `token` is **refused**, because the object is served by the API and written to disk. The token sits in the Bot API's request path, so `net/http` quotes it in its own errors — every string this daemon returns is scrubbed of both the whole token and its secret half before it can reach a log.

## One poller per token

**Telegram allows exactly one `getUpdates` consumer per bot token.** Everything else here is forgiving; this is not, and it is the failure that looks like an outage and is not.

This is not theoretical — it happened. The Claude Code Telegram channel plugin polls with the token in `~/.claude/channels/telegram/.env`, and it starts **on every session start**, not once. So each new Claude Code session took the token back, courier's poll began returning `409 Conflict`, and every message written in the group from then on went to the plugin instead. Sending kept working, which is what makes it disorienting: the daemon looks alive, the posts keep appearing, and replies simply never arrive.

The cure was to disable the plugin (`telegram@claude-plugins-official`). The alternative, if you want both, is to give courier its own bot: create one in @BotFather, add it as an administrator of the channel and of its discussion group, and start courier with `TELEGRAM_BOT_TOKEN` set in its own environment. Sending is unaffected either way — several bots may post into the same group.

**Restarting produces one of these, and it is not the same thing.** The process being replaced still holds a long poll open, and Telegram takes a second or two to notice the connection is gone, so a new daemon started immediately gets a conflict or two and then settles. Those are logged and nothing else; only from the third in a row does the channel go `Failed`, about six seconds in. The distinction is the point — calling a handover a failed channel is how an operator learns to scroll past the message that matters. To avoid them entirely, let the old process exit before starting the new one:

```sh
kill "$(pgrep -f 'courier serve')" && while pgrep -f 'courier serve' >/dev/null; do sleep 0.5; done
courier serve --telegram-chat …
```

**`/api/healthz` is what finds this.** A channel that loses its poll goes to `Failed`, health flips to `degraded`, and the channel's `status.message` names the conflict. Check it before assuming the group is quiet:

```sh
curl -s localhost:7717/api/healthz
{"status":"degraded","channels":1,"degraded":["telegram: Failed"], ...}
```

## What a deletion does not do

Deleting a message in Telegram does not retract it from an agent that already received it. The message disappears from the thread, and the agent still has it in its conversation, because the Bot API delivers no deletion event a bot could act on — courier never learns it happened.

So a message is committed the moment it is sent. If you wrote something you did not mean, say the next thing rather than deleting the last one: the agent read the first version and will not see it vanish.

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
