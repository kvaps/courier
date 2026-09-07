package mcpserver

import (
	"context"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kvaps/courier/pkg/api"
)

// bodyInput is the decide-shaped composition shared by send and ask. The
// per-field descriptions are the actual guidance an agent gets, so they say what
// each field is for rather than what type it is.
type bodyInput struct {
	Context  string `json:"context,omitempty" jsonschema:"one line that re-orients the reader: which PR, doc or thread, who is involved, what is on the table. They have not seen this in a week and are juggling other agents"`
	Question string `json:"question,omitempty" jsonschema:"the choice itself, as a plain either/or a person would say out loud. No undefined jargon; if a term is unavoidable, gloss it in two or three words"`
	Proposal string `json:"proposal,omitempty" jsonschema:"the answer YOU recommend, so they can reply with one word. Never leave this empty on a question"`
	Text     string `json:"text,omitempty" jsonschema:"free-form content, for anything that is not a decision — a note, a heads-up, a result"`
	Index    int    `json:"progress_index,omitempty" jsonschema:"position in a batch, e.g. 3 of 12, so a long run is visibly finite"`
	Total    int    `json:"progress_total,omitempty" jsonschema:"how many items the batch has in total"`
}

func (b bodyInput) body() api.Body {
	out := api.Body{Context: b.Context, Question: b.Question, Proposal: b.Proposal, Text: b.Text}
	if b.Total > 0 {
		out.Progress = &api.Progress{Index: b.Index, Total: b.Total}
	}
	return out
}

type openConversationInput struct {
	Channel      string `json:"channel" jsonschema:"the channel to open the thread on; list_channels shows them"`
	Title        string `json:"title" jsonschema:"what the reader sees in the thread list — name the agent or its task, not its id"`
	Name         string `json:"name,omitempty" jsonschema:"resource name for the conversation; generated from the title when omitted"`
	Subject      string `json:"subject,omitempty" jsonschema:"one line describing what this thread is about"`
	AgentSink    string `json:"agent_sink,omitempty" jsonschema:"how to reach the agent, e.g. claude; omit for a conversation the agent polls itself"`
	AgentAddress string `json:"agent_address,omitempty" jsonschema:"the agent's address within that sink, e.g. a session short id or name"`
}

func (h *server) openConversation(ctx context.Context, _ *mcp.CallToolRequest, in openConversationInput) (*mcp.CallToolResult, *api.Conversation, error) {
	c := &api.Conversation{
		Metadata: api.ObjectMeta{Name: in.Name},
		Spec: api.ConversationSpec{
			Channel: in.Channel,
			Title:   in.Title,
			Subject: in.Subject,
		},
	}
	if in.Name == "" {
		c.Metadata.GenerateName = "conv-"
	}
	if in.AgentSink != "" || in.AgentAddress != "" {
		c.Spec.Agent = &api.AgentRef{Sink: in.AgentSink, Address: in.AgentAddress}
	}
	out, err := h.cfg.Service.OpenConversation(ctx, c)
	return nil, out, err
}

type emptyInput struct{}

func (h *server) listConversations(_ context.Context, _ *mcp.CallToolRequest, _ emptyInput) (*mcp.CallToolResult, api.List[api.Conversation], error) {
	out, err := h.cfg.Service.ListConversations()
	return nil, out, err
}

type nameInput struct {
	Name string `json:"name" jsonschema:"the resource name"`
}

func (h *server) getConversation(_ context.Context, _ *mcp.CallToolRequest, in nameInput) (*mcp.CallToolResult, *api.Conversation, error) {
	out, err := h.cfg.Service.GetConversation(in.Name)
	return nil, out, err
}

func (h *server) closeConversation(ctx context.Context, _ *mcp.CallToolRequest, in nameInput) (*mcp.CallToolResult, *api.Conversation, error) {
	out, err := h.cfg.Service.CloseConversation(ctx, in.Name)
	return nil, out, err
}

type sendInput struct {
	Conversation string `json:"conversation" jsonschema:"the conversation to write in"`
	bodyInput
}

func (h *server) send(ctx context.Context, _ *mcp.CallToolRequest, in sendInput) (*mcp.CallToolResult, *api.Message, error) {
	out, err := h.cfg.Service.Send(ctx, &api.Message{
		Spec: api.MessageSpec{Conversation: in.Conversation, Body: in.body()},
	})
	return nil, out, err
}

type askInput struct {
	Conversation string `json:"conversation" jsonschema:"the conversation to ask in"`
	bodyInput
	WaitSeconds int `json:"wait_seconds,omitempty" jsonschema:"how long to wait for the answer before returning the question still open; defaults to 300, capped at 1800"`
}

// askResult is what an agent gets back: the message, plus the two things it
// would otherwise have to work out from the phase — did anyone answer, and what
// did they say.
type askResult struct {
	Message  *api.Message `json:"message"`
	Answered bool         `json:"answered"`
	Answer   string       `json:"answer,omitempty"`
	// Note explains a result that is not an answer, so the agent does not have
	// to guess what to do next.
	Note string `json:"note,omitempty"`
}

func (h *server) ask(ctx context.Context, _ *mcp.CallToolRequest, in askInput) (*mcp.CallToolResult, askResult, error) {
	sent, err := h.cfg.Service.Send(ctx, &api.Message{
		Spec: api.MessageSpec{
			Conversation: in.Conversation,
			Body:         in.body(),
			AwaitReply:   true,
		},
	})
	if err != nil {
		return nil, askResult{}, err
	}
	return h.await(ctx, sent.Metadata.Name, in.WaitSeconds)
}

type waitInput struct {
	Name        string `json:"name" jsonschema:"the message name that ask returned"`
	WaitSeconds int    `json:"wait_seconds,omitempty" jsonschema:"how long to wait before returning it still open; defaults to 300, capped at 1800"`
}

func (h *server) waitForAnswer(ctx context.Context, _ *mcp.CallToolRequest, in waitInput) (*mcp.CallToolResult, askResult, error) {
	return h.await(ctx, in.Name, in.WaitSeconds)
}

func (h *server) await(ctx context.Context, name string, waitSeconds int) (*mcp.CallToolResult, askResult, error) {
	m, err := h.cfg.Service.WaitForAnswer(ctx, name, waitFor(waitSeconds))
	if err != nil {
		return nil, askResult{}, err
	}
	res := askResult{Message: m}
	switch m.Status.Phase {
	case api.PhaseAnswered:
		res.Answered, res.Answer = true, m.Status.Answer
	case api.PhaseCancelled:
		res.Note = "this question was withdrawn (" + m.Status.Message + "); it will not be answered"
	default:
		res.Note = "no answer yet — the question is still standing. Call wait_for_answer with this name to keep waiting, " +
			"or get on with something else and come back to it"
	}
	return nil, res, nil
}

type cancelInput struct {
	Name   string `json:"name" jsonschema:"the question to withdraw"`
	Reason string `json:"reason,omitempty" jsonschema:"why, in the words the operator will see, e.g. 'answered elsewhere' or 'no longer relevant'"`
}

func (h *server) cancel(ctx context.Context, _ *mcp.CallToolRequest, in cancelInput) (*mcp.CallToolResult, *api.Message, error) {
	out, err := h.cfg.Service.Cancel(ctx, in.Name, in.Reason)
	return nil, out, err
}

type receiveInput struct {
	Conversation    string `json:"conversation" jsonschema:"the conversation to collect from"`
	ResourceVersion int64  `json:"resource_version,omitempty" jsonschema:"the resource_version from your previous receive; 0 starts from now"`
	WaitSeconds     int    `json:"wait_seconds,omitempty" jsonschema:"how long to wait if nothing has been written yet; defaults to 300, capped at 1800"`
}

// receiveResult carries the messages and the cursor to pass back next time.
type receiveResult struct {
	Messages []api.Message `json:"messages"`
	// ResourceVersion is the cursor for the next call. Passing it back is what
	// makes collection exactly-once.
	ResourceVersion int64 `json:"resource_version"`
}

func (h *server) receive(ctx context.Context, _ *mcp.CallToolRequest, in receiveInput) (*mcp.CallToolResult, receiveResult, error) {
	since := in.ResourceVersion
	if since == 0 {
		// Starting "from now" rather than from the beginning: an agent's first
		// call should not replay every message the conversation ever had.
		since = h.cfg.Service.Store().Version()
	}
	msgs, rv, err := h.cfg.Service.WaitForInbound(ctx, in.Conversation, since, waitFor(in.WaitSeconds))
	if err != nil {
		return nil, receiveResult{}, err
	}
	if msgs == nil {
		msgs = []api.Message{}
	}
	return nil, receiveResult{Messages: msgs, ResourceVersion: rv}, nil
}

type listMessagesInput struct {
	Conversation string `json:"conversation,omitempty" jsonschema:"limit to one conversation; omit for every message"`
}

func (h *server) listMessages(_ context.Context, _ *mcp.CallToolRequest, in listMessagesInput) (*mcp.CallToolResult, api.List[api.Message], error) {
	out, err := h.cfg.Service.ListMessages(in.Conversation)
	return nil, out, err
}

func (h *server) getMessage(_ context.Context, _ *mcp.CallToolRequest, in nameInput) (*mcp.CallToolResult, *api.Message, error) {
	out, err := h.cfg.Service.GetMessage(in.Name)
	return nil, out, err
}

func (h *server) listChannels(_ context.Context, _ *mcp.CallToolRequest, _ emptyInput) (*mcp.CallToolResult, api.List[api.Channel], error) {
	out, err := h.cfg.Service.ListChannels()
	return nil, out, err
}

// waitFor turns the tool's seconds into a duration, with a default that is long
// enough for a person to look at their phone and a cap that keeps the call
// inside any sane transport timeout.
func waitFor(seconds int) time.Duration {
	switch {
	case seconds <= 0:
		return 5 * time.Minute
	case seconds > 1800:
		return 30 * time.Minute
	default:
		return time.Duration(seconds) * time.Second
	}
}
