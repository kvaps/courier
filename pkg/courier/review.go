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

// Review puts one agent — typically the orchestrator — between every other
// agent's questions and the reader.
//
// The reader's attention is the scarce thing here. An agent asks the moment it
// is stuck, in its own words, about a thread the reader has not seen in a week;
// half of those questions are already answered somewhere, a third are worded
// so the reader cannot tell what is being asked. A held question goes to the
// reviewer first, which answers what it can and rewrites what it publishes.
//
// The reviewer is a filter, never a wall: a question it does not act on within
// the timeout is published exactly as the agent wrote it.

const defaultReviewTimeout = 10 * time.Minute

// reviewerFor decides whether a message is held, and for whom.
func (s *Service) reviewerFor(conv *api.Conversation, m *api.Message) (*api.Conversation, bool) {
	if !m.Spec.AwaitReply || len(m.Spec.Choices) > 0 {
		return nil, false
	}
	ch, err := s.channels.Get(conv.Spec.Channel)
	if err != nil || ch.Spec.Reviewer == "" || ch.Spec.Reviewer == conv.Metadata.Name {
		return nil, false
	}
	rev, err := s.conversations.Get(ch.Spec.Reviewer)
	if err != nil || rev.Spec.Closed || rev.Spec.Agent == nil {
		// A reviewer that cannot be reached is no reason to hold anything.
		return nil, false
	}
	return rev, true
}

func (s *Service) reviewTimeout(channel string) time.Duration {
	ch, err := s.channels.Get(channel)
	if err != nil || ch.Spec.ReviewTimeoutSeconds <= 0 {
		return defaultReviewTimeout
	}
	return time.Duration(ch.Spec.ReviewTimeoutSeconds) * time.Second
}

// hold records a question as the conversation's pending one without showing it
// to the reader, tells the reviewer, and arms the timeout.
func (s *Service) hold(conv *api.Conversation, m *api.Message, reviewer *api.Conversation) (*api.Message, error) {
	now := time.Now().UTC()
	s.touchConversation(conv.Metadata.Name, func(c *api.Conversation) {
		c.Status.MessageCount++
		c.Status.PendingQuestion = m.Metadata.Name
		c.Status.PendingQuestionSince = &now
	})
	s.log.Info("question held for review", "conversation", conv.Metadata.Name, "message", m.Metadata.Name,
		"reviewer", reviewer.Metadata.Name)
	s.background(func() { s.notifyReviewer(reviewer, conv, m) })
	s.armReview(m.Metadata.Name, s.reviewTimeout(conv.Spec.Channel))
	return m, nil
}

// armReview publishes a held question as it is once its time is up.
func (s *Service) armReview(name string, after time.Duration) {
	time.AfterFunc(after, func() {
		m, err := s.messages.Get(name)
		if err != nil || m.Status.Phase != api.PhaseHeld {
			return
		}
		s.log.Warn("the reviewer did not act on a held question in time; publishing it as it is", "message", name)
		if _, err := s.Publish(s.baseCtx, name, nil, "timeout"); err != nil {
			s.log.Error("could not publish a held question after its review timed out", "message", name, "err", err)
		}
	})
}

// resumeReviews re-arms the timeouts of questions held when the daemon
// stopped, so a restart cannot strand one.
func (s *Service) resumeReviews() {
	items, _, err := s.messages.List()
	if err != nil {
		return
	}
	for i := range items {
		m := items[i]
		if m.Status.Phase != api.PhaseHeld {
			continue
		}
		after := s.reviewTimeout(s.channelOf(m.Spec.Conversation)) - time.Since(m.Metadata.CreationTimestamp)
		if after < time.Second {
			after = time.Second
		}
		s.armReview(m.Metadata.Name, after)
	}
}

func (s *Service) channelOf(conversation string) string {
	c, err := s.conversations.Get(conversation)
	if err != nil {
		return ""
	}
	return c.Spec.Channel
}

// Publish shows a held question to the reader, optionally rewritten. The
// agent's own words are kept on the record beside the rewrite.
func (s *Service) Publish(ctx context.Context, name string, body *api.Body, by string) (*api.Message, error) {
	m, err := s.messages.Get(name)
	if err != nil {
		return nil, err
	}
	if m.Status.Phase != api.PhaseHeld {
		return nil, api.NewConflictf("message %q is %s, not held — there is nothing to publish", name, m.Status.Phase)
	}
	conv, err := s.conversations.Get(m.Spec.Conversation)
	if err != nil {
		return nil, err
	}
	be, err := s.backendFor(conv.Spec.Channel)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	action := "published"
	if by == "timeout" {
		action, by = "timeout", ""
	}
	review := &api.Review{Action: action, By: by, At: &now}
	if body != nil && !body.Empty() {
		original := m.Spec.Body
		review.Original = &original
		m.Spec.Body = *body
		if m, err = s.messages.Update(m); err != nil {
			return nil, err
		}
	}
	m.Status.Review = review
	if m, err = s.messages.UpdateStatus(m); err != nil {
		return nil, err
	}
	return s.deliver(ctx, conv, be, m, backend.MessageRef{})
}

// AnswerFor answers a question on the reader's behalf — the reviewer settling
// what it could settle itself. The agent is told the answer came from the
// reviewer, not from the reader, so it is weighed accordingly.
func (s *Service) AnswerFor(ctx context.Context, name, text, by string) (*api.Message, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, api.NewInvalid("an answer needs words")
	}
	if by == "" {
		by = "reviewer"
	}
	q, err := s.messages.Get(name)
	if err != nil {
		return nil, err
	}
	if !q.Open() {
		return nil, api.NewConflictf("message %q is %s, not an open question", name, q.Status.Phase)
	}
	conv, err := s.conversations.Get(q.Spec.Conversation)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	wasHeld := q.Status.Phase == api.PhaseHeld
	q.Status.Phase = api.PhaseAnswered
	q.Status.Answer = text
	q.Status.AnsweredAt = &now
	q.Status.AnsweredBy = by
	if wasHeld {
		q.Status.Review = &api.Review{Action: "answered", By: by, At: &now}
	}
	if q, err = s.messages.UpdateStatus(q); err != nil {
		return nil, err
	}
	stored, err := s.messages.Create(&api.Message{
		TypeMeta: api.TypeMeta{APIVersion: api.Version, Kind: api.KindMessage},
		Metadata: api.ObjectMeta{GenerateName: conv.Metadata.Name + "-in-"},
		Spec: api.MessageSpec{
			Conversation: conv.Metadata.Name,
			Direction:    api.Inbound,
			Body:         api.Body{Text: text},
			InReplyTo:    q.Metadata.Name,
		},
		Status: api.MessageStatus{Phase: api.PhaseSent, SentAt: &now, AnsweredBy: by},
	})
	if err != nil {
		return nil, err
	}
	s.touchConversation(conv.Metadata.Name, func(c *api.Conversation) {
		c.Status.MessageCount++
		c.Status.PendingQuestion = ""
		c.Status.PendingQuestionSince = nil
	})
	if !wasHeld {
		// The reader already sees it; show that it was settled, and by whom.
		s.retireDrafts(ctx, conv, q.Metadata.Name, "answered by "+by)
		s.editMessage(ctx, conv, q, answeredText(q.Spec.Body, text+" ("+by+")"))
	}
	s.log.Info("question answered on the reader's behalf", "conversation", conv.Metadata.Name,
		"message", q.Metadata.Name, "by", by, "held", wasHeld)
	if conv.Spec.Agent != nil {
		s.background(func() {
			s.push(conv, stored, backend.Inbound{Author: by + " (on the operator's behalf)", Text: text, At: now})
		})
	}
	return q, nil
}

// notifyReviewer hands a held question to the reviewer's session, with
// everything it needs to act on it.
func (s *Service) notifyReviewer(reviewer, conv *api.Conversation, m *api.Message) {
	ctx, cancel := context.WithTimeout(s.baseCtx, 2*time.Minute)
	defer cancel()
	sink, err := s.sinkFor(reviewer.Spec.Agent.Sink)
	if err != nil {
		s.log.Error("cannot reach the reviewer", "reviewer", reviewer.Metadata.Name, "err", err)
		return
	}
	timeout := s.reviewTimeout(conv.Spec.Channel)
	text := fmt.Sprintf(`[courier review] %q (conversation %s) wants to ask the operator. It is held: the operator has not seen it.

%s

Decide within %s, or it is published as it is:
- answer it yourself, if the answer is already settled (decisions.md, context/*.md, the card) or the agent should first check something:
  curl -sX POST localhost:7717/api/v1/messages/%s/answer -d '{"text":"<answer>","by":"orchestrator"}'
- publish it to the operator, rewritten so it reads cold (what this is, what happened, what is needed, the agent's proposal and the cost of being wrong; facts only with a source):
  curl -sX POST localhost:7717/api/v1/messages/%s/publish -d '{"by":"orchestrator","body":{"context":"…","question":"…","proposal":"…"}}'
  (omit "body" to publish it unchanged)`,
		conv.Spec.Title, conv.Metadata.Name, m.Spec.Body.Render("", false), timeout, m.Metadata.Name, m.Metadata.Name)
	if len(m.Spec.Attachments) > 0 {
		var b strings.Builder
		b.WriteString("\n\nAttached by the agent:")
		for _, f := range m.Spec.Attachments {
			fmt.Fprintf(&b, "\n  %s", f.Path)
		}
		text += b.String()
	}
	if _, err := sink.Deliver(ctx, reviewer.Spec.Agent.Address, agent.Message{Text: text, Conversation: reviewer.Metadata.Name}); err != nil {
		// The timeout still stands: an unreachable reviewer costs a delay only.
		s.log.Error("could not hand a held question to the reviewer", "reviewer", reviewer.Metadata.Name,
			"message", m.Metadata.Name, "err", err)
	}
}
