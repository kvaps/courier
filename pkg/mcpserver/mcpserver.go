// Package mcpserver exposes courier to an agent as MCP tools.
//
// It is a sibling of the HTTP API, not a client of it: both construct a
// courier.Service and call it in-process, so a tool call and a REST call take
// the same path, write the same store and reach the same watchers.
//
// The tool set is shaped around the rhythm the messages are meant to have — one
// decision per message, re-oriented in a line, with the sender's own answer
// already proposed. That is why send takes four named fields instead of a
// string: an agent handed a free-form body writes a wall of jargon about a
// thread the reader has not seen in a week, and the shape of the input is the
// cheapest place to prevent it.
package mcpserver

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kvaps/courier/pkg/courier"
)

// Config configures the MCP server.
type Config struct {
	// Service is what every tool call runs against.
	Service *courier.Service
	Version string
}

type server struct{ cfg Config }

// New builds the MCP server.
func New(cfg Config) *mcp.Server {
	h := &server{cfg: cfg}
	s := mcp.NewServer(&mcp.Implementation{Name: "courier", Version: cfg.Version}, nil)

	mcp.AddTool(s, &mcp.Tool{
		Name: "open_conversation",
		Description: "Open a thread with the operator and, optionally, bind it to an agent. " +
			"This is the orchestrator's call: one conversation per agent, made once, before that agent has anything to say. " +
			"On a forum-style channel it creates a topic, so two agents asking about unrelated things do not interleave into one stream. " +
			"Set agent_sink and agent_address to have whatever the operator writes here pushed straight to that agent — without it the agent " +
			"only sees a message the next time it calls receive, which for an agent in the middle of a long turn can be a while.",
	}, h.openConversation)

	mcp.AddTool(s, &mcp.Tool{
		Name: "list_conversations",
		Description: "List threads, with what each is bound to and whether one is currently waiting on an answer. " +
			"Use it to find the conversation name for a given agent before sending. " +
			"status.pendingQuestion names the question a thread is waiting on and status.pendingQuestionSince says since when, " +
			"so a question that has been standing too long can be re-raised or withdrawn.",
	}, h.listConversations)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_conversation",
		Description: "One thread in full, including the name of the question it is waiting on, if any.",
	}, h.getConversation)

	mcp.AddTool(s, &mcp.Tool{
		Name: "close_conversation",
		Description: "Close a thread when its agent is finished. The history and every answer stay readable — " +
			"closing is not deleting.",
	}, h.closeConversation)

	mcp.AddTool(s, &mcp.Tool{
		Name: "send",
		Description: "Send a message to the operator without asking for an answer: a status note, a heads-up, a finished result. " +
			"For anything you need a decision on, use ask instead — it holds the question open and routes the reply back to you. " +
			"Keep it short: this arrives as a phone notification, not a document.\n\n" +
			"Attach files with `files` — absolute paths on this machine. They are uploaded, so the operator gets the screenshot or " +
			"the log itself rather than a path they cannot open from their phone. A message may be nothing but files.",
	}, h.send)

	mcp.AddTool(s, &mcp.Tool{
		Name: "ask",
		Description: "Ask the operator one decision and wait for the answer. This is the main tool.\n\n" +
			"Compose it the way a colleague would text: `context` re-orients them in one line (which PR, doc or thread, who is involved) " +
			"because they are juggling other agents and have not seen yours in a week; `question` is the choice as a plain either/or, " +
			"without jargon; `proposal` is the answer YOU recommend, so they can reply \"OK\" instead of composing one — a question " +
			"arriving without a proposal makes them do the work you should have done; `progress` shows the end coming when you have a batch.\n\n" +
			"One question at a time per conversation: a second is refused while the first is open, because the operator answers by writing " +
			"in the thread rather than by quoting, and two open questions would mean guessing which one a bare \"OK\" belongs to. " +
			"If the question stops mattering, withdraw it with cancel rather than leaving it standing.\n\n" +
			"Attach files with `files` (absolute paths): a screenshot or a diff is often what turns a question the operator has to " +
			"go and investigate into one they can answer at a glance.\n\n" +
			"Returns when the operator answers, or when wait_seconds runs out — a timeout is not a failure, it comes back still open " +
			"and you can wait again with wait_for_answer. There is no timeout that answers for them: an unanswered question stays open.\n\n" +
			"An answer may come back with `approval` set. That means somebody else drafted the words and the operator confirmed them " +
			"with one tap rather than composing a reply. Act on it — it is a real answer — but do not read it as reasoning they worked " +
			"through themselves, and say whose draft it was if you later report how the decision was made.",
	}, h.ask)

	mcp.AddTool(s, &mcp.Tool{
		Name: "wait_for_answer",
		Description: "Keep waiting on a question that ask returned still open. Returns as soon as it is answered or withdrawn, " +
			"or again unanswered when the time runs out.",
	}, h.waitForAnswer)

	mcp.AddTool(s, &mcp.Tool{
		Name: "receive",
		Description: "Collect what the operator has written in a conversation since you last looked, waiting for it if there is nothing yet. " +
			"This is for messages that answer no question of yours — the operator writing unprompted. " +
			"Pass the resource_version returned by your previous call to get exactly what you have not seen: nothing skipped, nothing repeated. " +
			"On the first call leave it at 0 to start from now.\n\n" +
			"Files the operator sends are downloaded for you: each message's `spec.attachments[].path` is an ordinary local file you can read.",
	}, h.receive)

	mcp.AddTool(s, &mcp.Tool{
		Name: "cancel",
		Description: "Withdraw a question that no longer needs an answer, striking it where the operator can see it so they do not " +
			"answer something that stopped mattering. Use it before asking a replacement question in the same conversation.\n\n" +
			"Withdrawing is not refusing: it says the question stopped mattering and no answer is wanted. An answer of \"no, do not do " +
			"that\" is an answer, and it comes back through ask like any other.",
	}, h.cancel)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_messages",
		Description: "The record of a conversation: what was asked, what was answered, and what is still open.",
	}, h.listMessages)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_message",
		Description: "One message, including status.answer once the operator has replied.",
	}, h.getMessage)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_channels",
		Description: "The configured transports and whether each is connected — where to look first when nothing is arriving.",
	}, h.listChannels)

	return s
}

// Serve runs the MCP server over stdio.
func Serve(ctx context.Context, s *mcp.Server) error {
	return s.Run(ctx, &mcp.StdioTransport{})
}
