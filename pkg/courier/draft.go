package courier

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"time"

	"github.com/kvaps/courier/pkg/api"
	"github.com/kvaps/courier/pkg/backend"
)

// Limits on a drafted answer, chosen for the screen it is read on.
const (
	// maxChoices is how many buttons one draft may carry. Three is already a
	// list to read rather than a decision to take; past that, the thing being
	// asked for is not an approval but an answer, and the reader should write it.
	maxChoices = 3
	// maxLabel is how long a button's text may be. A label is a handle for a
	// decision, not the decision — the words being approved are in the message
	// above, where they can actually be read.
	maxLabel = 24
)

// Draft offers the reader an answer somebody already wrote, as buttons under
// the question, so confirming it costs one tap instead of composing a reply.
//
// This is the orchestrator's call and deliberately not an agent's. What the
// buttons do is turn a decision into a reflex, and the value of that depends
// entirely on somebody other than the asker having thought about the answer
// first. An agent able to draft its own approval would be asking the reader to
// rubber-stamp its own reasoning, which is the gate dissolving rather than the
// gate working — so this lives on the HTTP API and has no MCP tool. The MCP
// tool set is what an agent is handed; the API is what the orchestrator drives.
func (s *Service) Draft(ctx context.Context, question string, req DraftRequest) (*api.Message, error) {
	q, err := s.messages.Get(question)
	if err != nil {
		return nil, err
	}
	return s.Send(ctx, &api.Message{
		Spec: api.MessageSpec{
			Conversation: q.Spec.Conversation,
			InReplyTo:    q.Metadata.Name,
			DraftedBy:    req.DraftedBy,
			Choices:      req.Choices,
			Body:         api.Body{Context: req.Context, Text: req.Text},
		},
	})
}

// DraftRequest is what the drafter supplies: who wrote the answers, an optional
// line of framing, and the options themselves.
type DraftRequest struct {
	DraftedBy string
	Context   string
	Text      string
	Choices   []api.Choice
}

// prepareDraft validates a message carrying buttons against the question it
// claims to answer, and mints the transport handles its buttons will carry. It
// returns the question, whose own transport ref is what the draft is sent as a
// reply to — so the reader sees the two together without the daemon restating
// the question in words of its own.
func (s *Service) prepareDraft(conv *api.Conversation, m *api.Message) (*api.Message, error) {
	if m.Spec.AwaitReply {
		return nil, api.NewInvalid(
			"a drafted answer cannot itself be a question: it offers an answer to one that is already open")
	}
	if strings.TrimSpace(m.Spec.DraftedBy) == "" {
		return nil, api.NewInvalid(
			"spec.draftedBy is required on a drafted answer: approving with one tap is cheap, so the record has to say " +
				"whose words were approved rather than crediting the reader with writing them")
	}
	if strings.TrimSpace(m.Spec.InReplyTo) == "" {
		return nil, api.NewInvalid("a drafted answer must name the question it answers, in spec.inReplyTo")
	}
	q, err := s.messages.Get(m.Spec.InReplyTo)
	if err != nil {
		return nil, err
	}
	if q.Spec.Conversation != m.Spec.Conversation {
		return nil, api.NewInvalid("question %q belongs to conversation %q, not %q",
			q.Metadata.Name, q.Spec.Conversation, m.Spec.Conversation)
	}
	if !q.Open() {
		return nil, api.NewInvalid("message %q is not an open question (phase %s) — there is nothing left to answer",
			q.Metadata.Name, q.Status.Phase)
	}
	if live, ok := s.LiveDraft(q.Metadata.Name); ok {
		return nil, api.NewConflictf(
			"question %q already has a drafted answer waiting (%s) — withdraw it with cancel before offering another, "+
				"or the reader is looking at two keyboards for one decision",
			q.Metadata.Name, live.Metadata.Name)
	}
	if err := prepareChoices(m.Spec.Choices); err != nil {
		return nil, err
	}
	return q, nil
}

// prepareChoices checks the options and gives each one its transport handle.
//
// The rule worth stating is that every option must carry a real answer. A
// button that answered nothing would let a question be closed without deciding
// it — the reader would tap it, the agent would learn nothing, and the queue
// would look shorter than it was. Withdrawing a question that stopped mattering
// is what cancel is for, and it is the asker's call, not the reader's.
func prepareChoices(choices []api.Choice) error {
	if len(choices) == 0 {
		return api.NewInvalid("a drafted answer needs at least one option")
	}
	if len(choices) > maxChoices {
		return api.NewInvalid("%d options is more than a reader can take in on a phone; %d is the limit, "+
			"and past it the thing being asked for is an answer rather than an approval", len(choices), maxChoices)
	}
	seen := map[string]bool{}
	for i := range choices {
		c := &choices[i]
		c.Label = strings.TrimSpace(c.Label)
		c.Answer = strings.TrimSpace(c.Answer)
		c.ID = strings.TrimSpace(c.ID)
		switch {
		case c.Label == "":
			return api.NewInvalid("option %d has no label — that is the word the reader taps", i+1)
		case len([]rune(c.Label)) > maxLabel:
			return api.NewInvalid("option %q has a label longer than %d characters; the words being approved "+
				"belong in the message, where they can be read", c.Label, maxLabel)
		case c.Answer == "":
			return api.NewInvalid("option %q carries no answer — a button that answers nothing closes a question "+
				"without deciding it; to withdraw a question that stopped mattering, cancel it", c.Label)
		}
		if c.ID == "" {
			c.ID = slug(c.Label)
		}
		if seen[c.ID] {
			return api.NewInvalid("two options share the id %q; an agent branches on it, so it has to be unique", c.ID)
		}
		seen[c.ID] = true

		ref, err := newRef()
		if err != nil {
			return err
		}
		c.Ref = ref
	}
	return nil
}

// Press implements backend.Sink: it takes the option a person tapped and turns
// it into the answer the waiting agent gets.
//
// Every refusal path here returns a result rather than an error where the press
// was simply late, because "late" is the normal case: a question can be
// answered by hand, or withdrawn by its asker, while the draft is still on the
// reader's screen. What must never happen is a stale draft answering a question
// it was not written for, so the question is re-checked at the moment of the
// tap and not trusted from when the buttons were drawn.
func (s *Service) Press(ctx context.Context, channel string, p backend.Press) (backend.PressResult, error) {
	conv, ok := s.findByThread(channel, p.Thread)
	if !ok {
		s.log.Warn("a button press in an unmapped thread", "channel", channel, "thread", p.Thread, "from", p.Author)
		return backend.PressResult{Toast: "This thread is not bound to an agent.", Alert: true}, nil
	}
	draft, found := s.messageByRef(conv.Metadata.Name, p.Message.ID)
	if !found {
		s.log.Warn("a button press on a message this daemon does not know",
			"conversation", conv.Metadata.Name, "ref", p.Message.ID, "from", p.Author)
		return backend.PressResult{Toast: "These buttons are no longer live.", Alert: true}, nil
	}
	if !draft.DraftOpen() {
		s.log.Info("a button press on a draft that is no longer on offer",
			"conversation", conv.Metadata.Name, "message", draft.Metadata.Name,
			"phase", draft.Status.Phase, "from", p.Author)
		if draft.Status.Phase == api.PhaseAnswered {
			return backend.PressResult{Toast: "Already decided.", Alert: true}, nil
		}
		return backend.PressResult{Toast: "That draft was withdrawn — nothing was sent.", Alert: true}, nil
	}
	choice, ok := draft.ChoiceByRef(p.Choice)
	if !ok {
		s.log.Warn("a button press naming an option this draft does not carry",
			"conversation", conv.Metadata.Name, "message", draft.Metadata.Name, "from", p.Author)
		return backend.PressResult{Toast: "That option is not on this draft any more.", Alert: true}, nil
	}

	q, live := s.answerable(conv, draft.Spec.InReplyTo)
	if !live {
		s.log.Info("a button press arrived after its question was settled",
			"conversation", conv.Metadata.Name, "message", draft.Metadata.Name,
			"question", draft.Spec.InReplyTo, "choice", choice.ID, "from", p.Author)
		s.retire(ctx, conv, draft, "the question it answered was settled another way")
		return backend.PressResult{
			Toast: "Too late — that question is no longer open, so nothing was sent.",
			Alert: true,
		}, nil
	}

	now := p.At
	if now.IsZero() {
		now = time.Now().UTC()
	}
	approval := &api.Approval{
		DraftedBy: draft.Spec.DraftedBy,
		Choice:    choice.ID,
		Label:     choice.Label,
		Draft:     draft.Metadata.Name,
	}

	q.Status.Phase = api.PhaseAnswered
	q.Status.Answer = choice.Answer
	q.Status.AnsweredAt = &now
	q.Status.AnsweredBy = p.Author
	q.Status.Approval = approval
	if _, err := s.messages.UpdateStatus(q); err != nil {
		return backend.PressResult{}, err
	}

	draft.Status.Phase = api.PhaseAnswered
	draft.Status.Answer = choice.Answer
	draft.Status.AnsweredAt = &now
	draft.Status.AnsweredBy = p.Author
	draft.Status.Approval = approval
	settled, err := s.messages.UpdateStatus(draft)
	if err != nil {
		return backend.PressResult{}, err
	}

	// The press is recorded as an inbound message like any other, because from
	// the conversation's side that is what it is: something that arrived from
	// the human. What it carries beyond the words is the approval — who wrote
	// them and who agreed to them.
	stored, err := s.messages.Create(&api.Message{
		TypeMeta: api.TypeMeta{APIVersion: api.Version, Kind: api.KindMessage},
		Metadata: api.ObjectMeta{GenerateName: conv.Metadata.Name + "-in-"},
		Spec: api.MessageSpec{
			Conversation: conv.Metadata.Name,
			Direction:    api.Inbound,
			Body:         api.Body{Text: choice.Answer},
			InReplyTo:    q.Metadata.Name,
		},
		Status: api.MessageStatus{
			Phase:      api.PhaseSent,
			SentAt:     &now,
			AnsweredBy: p.Author,
			Approval:   approval,
		},
	})
	if err != nil {
		return backend.PressResult{}, err
	}
	s.touchConversation(conv.Metadata.Name, func(c *api.Conversation) {
		c.Status.MessageCount++
		c.Status.PendingQuestion = ""
		c.Status.PendingQuestionSince = nil
	})

	// Rewrite the draft where the reader is looking, so a decision that has been
	// taken stops looking like one still on offer.
	edited := s.editMessage(ctx, conv, settled,
		api.RenderChosen(settled.Spec.Body, settled.Spec.DraftedBy, settled.Spec.Choices, choice, p.Author))

	s.log.Info("a drafted answer was taken",
		"conversation", conv.Metadata.Name, "question", q.Metadata.Name, "draft", draft.Metadata.Name,
		"choice", choice.ID, "label", choice.Label, "drafted_by", draft.Spec.DraftedBy,
		"by", p.Author, "rewritten", edited)

	if conv.Spec.Agent != nil {
		s.background(func() {
			s.push(conv, stored, backend.Inbound{Author: p.Author, Text: choice.Answer, At: now})
		})
	}
	// Alert rather than the quieter toast, because a press that seems to do
	// nothing gets pressed again. Editing the message raises no notification,
	// so this is the only thing that happens at the moment of the tap, and what
	// it confirms is a decision already on its way to an agent.
	return backend.PressResult{Toast: "✓ \"" + choice.Label + "\" — sent to the agent as your answer.", Alert: true}, nil
}

// answerable reports whether a question is still the one this conversation is
// waiting on. Both halves matter: the question must be open, and it must be the
// conversation's current one — a draft written for a question that has since
// been withdrawn and replaced must not answer its successor.
func (s *Service) answerable(conv *api.Conversation, question string) (*api.Message, bool) {
	if question == "" || conv.Status.PendingQuestion != question {
		return nil, false
	}
	q, err := s.messages.Get(question)
	if err != nil || !q.Open() {
		return nil, false
	}
	return q, true
}

// LiveDraft returns the question's outstanding drafted answer, if it has one —
// what the reader currently has on their screen for this decision.
func (s *Service) LiveDraft(question string) (*api.Message, bool) {
	drafts := s.liveDrafts(question)
	if len(drafts) == 0 {
		return nil, false
	}
	return &drafts[0], true
}

// liveDrafts returns every drafted answer still on offer for a question. There
// is normally at most one — a second is refused when it is offered — but the
// plural is what the retirement path wants, and a leftover from an earlier
// version of the daemon should be retired rather than ignored.
func (s *Service) liveDrafts(question string) []api.Message {
	if question == "" {
		return nil
	}
	items, _, err := s.messages.List()
	if err != nil {
		return nil
	}
	var out []api.Message
	for i := range items {
		if items[i].Spec.InReplyTo == question && items[i].DraftOpen() {
			out = append(out, items[i])
		}
	}
	return out
}

// messageByRef finds a conversation's message by the transport's own id for it.
func (s *Service) messageByRef(conversation, ref string) (*api.Message, bool) {
	if ref == "" {
		return nil, false
	}
	items, _, err := s.messages.List()
	if err != nil {
		return nil, false
	}
	for i := range items {
		m := items[i]
		if m.Spec.Conversation == conversation && m.Spec.Direction == api.Outbound && m.Status.Ref == ref {
			return &m, true
		}
	}
	return nil, false
}

// retireDrafts withdraws whatever was on offer for a question that has just
// been settled some other way — answered by hand, or withdrawn by its asker.
//
// Leaving them would leave a live keyboard on a decided question: the reader
// would tap it, and the press would either be refused after the fact or, worse,
// land on whatever question came next.
func (s *Service) retireDrafts(ctx context.Context, conv *api.Conversation, question, note string) {
	drafts := s.liveDrafts(question)
	for i := range drafts {
		s.retire(ctx, conv, &drafts[i], note)
	}
}

// retire takes a drafted answer off the table, on the transport and in the store.
func (s *Service) retire(ctx context.Context, conv *api.Conversation, draft *api.Message, note string) {
	s.log.Info("taking a drafted answer off the table",
		"conversation", conv.Metadata.Name, "message", draft.Metadata.Name,
		"question", draft.Spec.InReplyTo, "why", note)
	_ = s.editMessage(ctx, conv, draft,
		api.RenderRetired(draft.Spec.Body, draft.Spec.DraftedBy, draft.Spec.Choices, note))
	draft.Status.Phase = api.PhaseCancelled
	draft.Status.Message = note
	if _, err := s.messages.UpdateStatus(draft); err != nil {
		s.log.Warn("could not record a retired draft", "message", draft.Metadata.Name, "err", err)
	}
}

// editMessage rewrites a message already delivered, reporting whether the
// reader's screen actually changed. Every edit courier makes is a message
// becoming final, so the transport clears any buttons with it.
//
// The outcome is returned rather than only logged because it is the difference
// between a decision the reader can see and one they have to take on faith: if
// this fails, the buttons are still on their screen under a question that has
// already been answered.
func (s *Service) editMessage(ctx context.Context, conv *api.Conversation, m *api.Message, text string) bool {
	be, err := s.backendFor(conv.Spec.Channel)
	if err != nil || m.Status.Ref == "" {
		return false
	}
	ref := backend.MessageRef{Thread: backend.ThreadRef(conv.Status.Ref), ID: m.Status.Ref}
	if eerr := be.Edit(ctx, ref, text); eerr != nil {
		s.log.Warn("could not rewrite a message the reader is looking at",
			"conversation", conv.Metadata.Name, "message", m.Metadata.Name, "err", eerr)
		return false
	}
	return true
}

// newRef mints the handle a button carries. It is short because Telegram allows
// a button 64 bytes and no more, which is a fraction of any real answer — the
// words stay in the message resource and only this travels.
func newRef() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", api.NewInternalError("mint a button handle: %v", err)
	}
	return "c" + hex.EncodeToString(b[:]), nil
}

// slug turns a label into a default id an agent can branch on.
func slug(label string) string {
	out := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		default:
			return '-'
		}
	}, label)
	out = strings.Trim(out, "-")
	if out == "" {
		return "choice"
	}
	return out
}
