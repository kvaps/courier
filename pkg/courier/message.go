package courier

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/kvaps/courier/pkg/agent"
	"github.com/kvaps/courier/pkg/api"
	"github.com/kvaps/courier/pkg/backend"
)

// Send delivers an outbound message and records it.
//
// A question — AwaitReply — is held open until it is answered or withdrawn, and
// a conversation may only hold one at a time. That rule is what makes the reply
// unambiguous: the human answers by writing in the thread, not by quoting, so
// two open questions would mean the daemon guessing which one a bare "OK"
// belongs to. A second question is refused, naming the first and how to
// withdraw it.
func (s *Service) Send(ctx context.Context, m *api.Message) (*api.Message, error) {
	if m.Spec.Body.Empty() && len(m.Spec.Attachments) == 0 && len(m.Spec.Choices) == 0 {
		return nil, api.NewInvalid("the message is empty — there would be nothing to read and, on a question, nothing to answer")
	}
	files, err := prepareAttachments(m.Spec.Attachments)
	if err != nil {
		return nil, err
	}
	m.Spec.Attachments = files
	conv, cerr := s.conversations.Get(m.Spec.Conversation)
	if cerr != nil {
		return nil, cerr
	}
	if conv.Spec.Closed {
		return nil, api.NewInvalid("conversation %q is closed", conv.Metadata.Name)
	}
	be, berr := s.backendFor(conv.Spec.Channel)
	if berr != nil {
		return nil, berr
	}
	if m.Spec.AwaitReply {
		if pending, ok := s.openQuestion(conv); ok {
			return nil, api.NewConflictf(
				"conversation %q is already waiting on an answer to message %q (%s) — "+
					"answer it, or withdraw it first so this one is not answered by mistake",
				conv.Metadata.Name, pending.Metadata.Name, pending.Spec.Body.Summary())
		}
	}
	// A message carrying buttons is a drafted answer, and it is only ever an
	// answer to a question that is open right now. Quoting that question is what
	// lets the reader see the two together without the daemon restating either.
	var quoted backend.MessageRef
	if len(m.Spec.Choices) > 0 {
		q, derr := s.prepareDraft(conv, m)
		if derr != nil {
			return nil, derr
		}
		quoted = backend.MessageRef{Thread: backend.ThreadRef(conv.Status.Ref), ID: q.Status.Ref}
	}

	m.TypeMeta = api.TypeMeta{APIVersion: api.Version, Kind: api.KindMessage}
	m.Spec.Direction = api.Outbound
	if m.Metadata.Name == "" && m.Metadata.GenerateName == "" {
		m.Metadata.GenerateName = conv.Metadata.Name + "-"
	}
	m.Status = api.MessageStatus{Phase: api.PhasePending}

	created, err := s.messages.Create(m)
	if err != nil {
		return nil, err
	}

	text := created.Spec.Body.Render(s.replyPrompt(conv.Spec.Channel), created.Spec.AwaitReply)
	if len(created.Spec.Choices) > 0 {
		text = api.RenderDraft(created.Spec.Body, created.Spec.DraftedBy, created.Spec.Choices,
			s.choicePrompt(conv.Spec.Channel))
	}
	ref, err := be.Send(ctx, backend.ThreadRef(conv.Status.Ref), backend.Outgoing{
		Text:       text,
		Body:       created.Spec.Body,
		AwaitReply: created.Spec.AwaitReply,
		Choices:    created.Spec.Choices,
		ReplyTo:    quoted,
		Files:      created.Spec.Attachments,
	})
	if err != nil {
		created.Status.Phase = api.PhaseFailed
		created.Status.Message = err.Error()
		if _, uerr := s.messages.UpdateStatus(created); uerr != nil {
			s.log.Warn("could not record a failed send", "message", created.Metadata.Name, "err", uerr)
		}
		return nil, err
	}

	now := time.Now().UTC()
	created.Status.Phase = api.PhaseSent
	created.Status.Ref = ref.ID
	created.Status.SentAt = &now
	sent, err := s.messages.UpdateStatus(created)
	if err != nil {
		return nil, err
	}

	s.touchConversation(conv.Metadata.Name, func(c *api.Conversation) {
		c.Status.MessageCount++
		if sent.Spec.AwaitReply {
			c.Status.PendingQuestion = sent.Metadata.Name
			c.Status.PendingQuestionSince = sent.Status.SentAt
		}
	})
	return sent, nil
}

// Cancel withdraws a question that has not been answered, or a drafted answer
// that has not been taken.
//
// Withdrawing edits the message the human is looking at, so a question that no
// longer needs an answer stops looking like one that does. A transport that
// cannot edit gets a short follow-up note instead — the point is that the
// reader is told, not how.
//
// Withdrawing a question takes its drafted answers down with it. Leaving them
// would leave a live keyboard under a question nobody is waiting on, and the
// reader tapping it would be answering into nothing.
//
// It works even when the conversation is gone. Settling the message is the
// whole point of the call, and a message whose thread was cleaned up under it
// is exactly the one with no other way left to close.
func (s *Service) Cancel(ctx context.Context, name, reason string) (*api.Message, error) {
	m, err := s.messages.Get(name)
	if err != nil {
		return nil, err
	}
	if !m.Open() && !m.DraftOpen() {
		return nil, api.NewInvalid("message %q is neither an open question nor a drafted answer still on offer (phase %s)",
			name, m.Status.Phase)
	}
	note := strings.TrimSpace(reason)
	if note == "" {
		note = "withdrawn"
	}

	// A message can outlive its conversation: threads get cleaned up while a
	// question is still standing. Withdrawing must still work then — the point
	// of cancel is to settle the message, and refusing because the thread is
	// gone would leave it open forever with no way to close it. There is simply
	// nothing left to strike on the transport. That holds for a drafted answer
	// exactly as it does for a question: its buttons went with the thread.
	conv, err := s.conversations.Get(m.Spec.Conversation)
	if err != nil {
		if !api.IsNotFound(err) {
			return nil, err
		}
		m.Status.Phase = api.PhaseCancelled
		m.Status.Message = note + " (its conversation no longer exists, so nothing was struck in the thread)"
		return s.messages.UpdateStatus(m)
	}

	if m.DraftOpen() {
		s.retire(ctx, conv, m, note)
		return s.messages.Get(name)
	}
	s.retireDrafts(ctx, conv, name, "the question it answered was withdrawn")
	if be, berr := s.backendFor(conv.Spec.Channel); berr == nil && m.Status.Ref != "" {
		ref := backend.MessageRef{Thread: backend.ThreadRef(conv.Status.Ref), ID: m.Status.Ref}
		struck := m.Spec.Body.Render(s.replyPrompt(conv.Spec.Channel), false) + "\n\n— " + note + " (no answer needed)"
		if eerr := be.Edit(ctx, ref, struck); eerr != nil {
			if _, serr := be.Send(ctx, backend.ThreadRef(conv.Status.Ref), backend.Outgoing{
				Text: "— " + note + ": the question above no longer needs an answer",
			}); serr != nil {
				s.log.Warn("could not tell the reader a question was withdrawn",
					"message", name, "edit_err", eerr, "send_err", serr)
			}
		}
	}

	m.Status.Phase = api.PhaseCancelled
	m.Status.Message = note
	cancelled, err := s.messages.UpdateStatus(m)
	if err != nil {
		return nil, err
	}
	s.touchConversation(conv.Metadata.Name, func(c *api.Conversation) {
		if c.Status.PendingQuestion == name {
			c.Status.PendingQuestion = ""
			c.Status.PendingQuestionSince = nil
		}
	})
	return cancelled, nil
}

// GetMessage returns one message.
func (s *Service) GetMessage(name string) (*api.Message, error) { return s.messages.Get(name) }

// ListMessages returns messages, optionally only those in one conversation.
func (s *Service) ListMessages(conversation string) (api.List[api.Message], error) {
	items, rv, err := s.messages.List()
	if err != nil {
		return api.List[api.Message]{}, err
	}
	if conversation != "" {
		kept := items[:0]
		for _, m := range items {
			if m.Spec.Conversation == conversation {
				kept = append(kept, m)
			}
		}
		items = kept
	}
	return api.NewList(api.KindMessage, rv, items), nil
}

// openQuestion returns the conversation's outstanding question, if it still is
// one. The status field alone is not trusted: a question can have been answered
// or withdrawn since it was recorded, and a stale name there would block every
// later question in the thread.
func (s *Service) openQuestion(c *api.Conversation) (*api.Message, bool) {
	if c.Status.PendingQuestion == "" {
		return nil, false
	}
	m, err := s.messages.Get(c.Status.PendingQuestion)
	if err != nil || !m.Open() {
		return nil, false
	}
	return m, true
}

// Receive implements backend.Sink: it records what a person wrote and, when the
// conversation has an agent, pushes it to them.
//
// It returns quickly. A backend's receive loop is single-threaded, so anything
// slow — waking a stopped agent takes tens of seconds — happens on its own
// goroutine, after the message is safely in the store.
func (s *Service) Receive(ctx context.Context, channel string, in backend.Inbound) {
	conv, ok := s.findByThread(channel, in.Thread)
	if !ok {
		// Not an error: a thread nobody bound to a conversation. Logged rather
		// than delivered somewhere arbitrary, because a message put in front of
		// the wrong agent is worse than one that went nowhere.
		s.log.Warn("message in an unmapped thread", "channel", channel, "thread", in.Thread, "from", in.Author)
		return
	}
	s.mark(ctx, conv, in.Ref, backend.MarkSeen)

	answered := s.recordAnswer(ctx, conv, in)

	msg := &api.Message{
		TypeMeta: api.TypeMeta{APIVersion: api.Version, Kind: api.KindMessage},
		Metadata: api.ObjectMeta{GenerateName: conv.Metadata.Name + "-in-"},
		Spec: api.MessageSpec{
			Conversation: conv.Metadata.Name,
			Direction:    api.Inbound,
			Body:         api.Body{Text: in.Text},
			InReplyTo:    answered,
			Attachments:  in.Files,
		},
		Status: api.MessageStatus{
			Phase:      api.PhaseSent,
			Ref:        in.Ref.ID,
			SentAt:     &in.At,
			AnsweredBy: in.Author,
		},
	}
	stored, err := s.messages.Create(msg)
	if err != nil {
		s.log.Error("could not record an inbound message", "conversation", conv.Metadata.Name, "err", err)
		return
	}
	s.touchConversation(conv.Metadata.Name, func(c *api.Conversation) { c.Status.MessageCount++ })

	if conv.Spec.Agent == nil {
		s.mark(ctx, conv, in.Ref, backend.MarkAccepted)
		return
	}
	s.background(func() { s.push(conv, stored, in) })
}

// recordAnswer lands an inbound message on the conversation's open question, if
// there is one, and returns that question's name.
//
// Words the reader typed settle the question exactly as a tapped button would,
// which is the point: buttons are an offer laid over the ordinary way of
// answering, never a replacement for it. Because they are only an offer, one
// still standing when the reader writes instead has to come down — otherwise a
// decided question keeps a live keyboard under it.
func (s *Service) recordAnswer(ctx context.Context, conv *api.Conversation, in backend.Inbound) string {
	q, ok := s.openQuestion(conv)
	if !ok {
		return ""
	}
	now := in.At
	if now.IsZero() {
		now = time.Now().UTC()
	}
	q.Status.Phase = api.PhaseAnswered
	q.Status.Answer = in.Text
	q.Status.AnsweredAt = &now
	q.Status.AnsweredBy = in.Author
	if _, err := s.messages.UpdateStatus(q); err != nil {
		s.log.Error("could not record an answer", "message", q.Metadata.Name, "err", err)
		return ""
	}
	s.touchConversation(conv.Metadata.Name, func(c *api.Conversation) {
		c.Status.PendingQuestion = ""
		c.Status.PendingQuestionSince = nil
	})
	// After the answer is recorded, not before: the waiting agent should not be
	// held up by a round trip to the transport. A button pressed in the window
	// between the two is refused on the question's own state, which is already
	// settled — the edit is what the reader sees, not what makes it true.
	s.retireDrafts(ctx, conv, q.Metadata.Name, "answered in the reader's own words")
	return q.Metadata.Name
}

// push delivers an inbound message to the conversation's agent.
//
// It runs on the daemon's lifetime, not the caller's: the human has already
// been told the message was received, and a delivery that is abandoned halfway
// because a poll loop moved on would be a message silently lost.
func (s *Service) push(conv *api.Conversation, stored *api.Message, in backend.Inbound) {
	ctx, cancel := context.WithTimeout(s.baseCtx, 2*time.Minute)
	defer cancel()

	sink, err := s.sinkFor(conv.Spec.Agent.Sink)
	if err == nil {
		var receipt agent.Receipt
		receipt, err = sink.Deliver(ctx, conv.Spec.Agent.Address, agent.Message{
			Text:         s.envelope(conv, stored, in),
			Conversation: conv.Metadata.Name,
		})
		if err == nil {
			s.log.Info("delivered to agent", "conversation", conv.Metadata.Name,
				"agent", receipt.Address, "how", receipt.How, "woke", receipt.Woke)
			s.noteAgent(conv.Metadata.Name, true, "")
			s.mark(ctx, conv, in.Ref, backend.MarkAccepted)
			return
		}
	}

	s.log.Error("could not deliver to agent", "conversation", conv.Metadata.Name, "err", err)
	s.noteAgent(conv.Metadata.Name, false, err.Error())
	// Tell the person. A message the agent never got must not look delivered.
	if be, berr := s.backendFor(conv.Spec.Channel); berr == nil {
		if _, serr := be.Send(ctx, backend.ThreadRef(conv.Status.Ref), backend.Outgoing{
			Text: "⚠ not delivered to the agent — " + err.Error() + "\n\nIt is recorded as " + stored.Metadata.Name + " and can be re-sent.",
		}); serr != nil {
			s.log.Warn("could not report a failed agent delivery", "conversation", conv.Metadata.Name, "err", serr)
		}
	}
}

// envelope is what the agent reads. It says who is writing and how to answer,
// so the message is recognisably from a person via courier — rather than
// courier pretending to be a session, which would hand the agent a return
// address nobody is listening on.
//
// The last paragraph is not boilerplate. Without it an agent that has no
// courier tools goes looking for another way to reply — a peer message to
// something named after the conversation, typically — and reports the channel
// as broken when the real answer is that nobody configured it. Naming the tools
// and the conversation, and saying plainly what to do when they are absent,
// turns that into one accurate sentence back to the operator.
func (s *Service) envelope(conv *api.Conversation, stored *api.Message, in backend.Inbound) string {
	from := in.Author
	if from == "" {
		from = "the operator"
	}
	body := in.Text
	if len(in.Files) > 0 {
		// The files are already on this machine, so the agent is told where
		// they are rather than how to fetch them — it opens ordinary paths.
		var b strings.Builder
		if body != "" {
			b.WriteString(body)
			b.WriteString("\n\n")
		}
		b.WriteString("Attached, already saved on this machine — read them as ordinary files:")
		for _, f := range in.Files {
			fmt.Fprintf(&b, "\n  %s  (%s, %s)", f.Path, f.Name, humanSize(f.Size))
		}
		body = b.String()
	}
	lead := fmt.Sprintf("%s wrote to you in %q:", from, conv.Spec.Title)
	if a := stored.Status.Approval; a != nil {
		// Said plainly, and not only in a field beside the text, because this is
		// the sentence that stops a confirmed draft from being read as the
		// operator's own reasoning. One tap is a cheap thing to have obtained,
		// and an answer that turns out to be wrong has to be traceable to
		// whoever actually composed it.
		lead = fmt.Sprintf(
			"%s approved an answer in %q. These are not their own words: %s drafted them, and %s confirmed the draft "+
				"by choosing %q — one tap, not a reply they composed. Weigh it accordingly.",
			from, conv.Spec.Title, a.DraftedBy, from, a.Label)
	}
	return fmt.Sprintf(
		"[courier] %s\n\n%s\n\n"+
			"To answer, call the courier MCP tool `send` (or `ask`, if you need a decision back) with conversation=%q. "+
			"If you have no courier tools, courier is not registered as an MCP server for this session — say that plainly rather than "+
			"looking for another channel; there is no other way back.",
		lead, body, conv.Metadata.Name)
}

func (s *Service) noteAgent(conversation string, reachable bool, message string) {
	s.touchConversation(conversation, func(c *api.Conversation) {
		if c.Status.Agent == nil {
			c.Status.Agent = &api.AgentStatus{}
		}
		now := time.Now().UTC()
		c.Status.Agent.Reachable = reachable
		c.Status.Agent.Message = message
		c.Status.Agent.LastDeliveryAt = &now
	})
}

// mark places a best-effort acknowledgement on a person's message.
func (s *Service) mark(ctx context.Context, conv *api.Conversation, ref backend.MessageRef, m backend.Mark) {
	be, err := s.backendFor(conv.Spec.Channel)
	if err != nil || ref.Zero() {
		return
	}
	if err := be.React(ctx, ref, m); err != nil && api.ReasonOf(err) != api.ReasonNotSupported {
		s.log.Debug("could not set a reaction", "conversation", conv.Metadata.Name, "err", err)
	}
}
