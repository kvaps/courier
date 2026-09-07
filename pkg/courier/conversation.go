package courier

import (
	"context"
	"strings"
	"time"

	"github.com/kvaps/courier/pkg/api"
	"github.com/kvaps/courier/pkg/backend"
)

// OpenConversation creates a thread on the channel and records it.
//
// This is the orchestrator's call: it opens one conversation per agent, and the
// agent then writes into its own thread without needing to know anything about
// the transport underneath.
func (s *Service) OpenConversation(ctx context.Context, c *api.Conversation) (*api.Conversation, error) {
	if strings.TrimSpace(c.Spec.Channel) == "" {
		return nil, api.NewInvalid("spec.channel is required")
	}
	if strings.TrimSpace(c.Spec.Title) == "" {
		return nil, api.NewInvalid("spec.title is required — it is what the reader sees in the thread list")
	}
	if _, err := s.channels.Get(c.Spec.Channel); err != nil {
		return nil, err
	}
	be, err := s.backendFor(c.Spec.Channel)
	if err != nil {
		return nil, err
	}

	c.TypeMeta = api.TypeMeta{APIVersion: api.Version, Kind: api.KindConversation}
	c.Status = api.ConversationStatus{Phase: api.PhasePending}

	// Resolving the agent up front turns a typo into an error the orchestrator
	// sees while it is still holding the context, instead of a thread the human
	// writes into for a week with nothing at the other end.
	if ref := c.Spec.Agent; ref != nil {
		if strings.TrimSpace(ref.Sink) == "" || strings.TrimSpace(ref.Address) == "" {
			return nil, api.NewInvalid("spec.agent needs both sink and address")
		}
		sink, err := s.sinkFor(ref.Sink)
		if err != nil {
			return nil, err
		}
		target, err := sink.Resolve(ctx, ref.Address)
		if err != nil {
			return nil, err
		}
		c.Spec.Agent.Address = target.Address
		c.Status.Agent = &api.AgentStatus{
			Address: target.Address, Name: target.Name, Reachable: target.Reachable(),
		}
	}

	created, err := s.conversations.Create(c)
	if err != nil {
		return nil, err
	}

	ref, err := be.OpenThread(ctx, backend.ThreadSpec{
		Conversation: created.Metadata.Name,
		Title:        created.Spec.Title,
		Subject:      created.Spec.Subject,
	})
	if err != nil {
		created.Status.Phase = api.PhaseFailed
		created.Status.Message = err.Error()
		if _, uerr := s.conversations.UpdateStatus(created); uerr != nil {
			s.log.Warn("could not record a failed conversation", "conversation", created.Metadata.Name, "err", uerr)
		}
		return nil, err
	}

	now := time.Now().UTC()
	created.Status.Phase = api.PhaseReady
	created.Status.Ref = string(ref)
	created.Status.OpenedAt = &now
	return s.conversations.UpdateStatus(created)
}

// GetConversation returns one conversation.
func (s *Service) GetConversation(name string) (*api.Conversation, error) {
	return s.conversations.Get(name)
}

// ListConversations returns every conversation.
func (s *Service) ListConversations() (api.List[api.Conversation], error) {
	items, rv, err := s.conversations.List()
	if err != nil {
		return api.List[api.Conversation]{}, err
	}
	return api.NewList(api.KindConversation, rv, items), nil
}

// CloseConversation closes the thread and marks the conversation closed. The
// record and its answers stay: closing is not deleting.
func (s *Service) CloseConversation(ctx context.Context, name string) (*api.Conversation, error) {
	c, err := s.conversations.Get(name)
	if err != nil {
		return nil, err
	}
	if be, berr := s.backendFor(c.Spec.Channel); berr == nil {
		if cerr := be.CloseThread(ctx, backend.ThreadRef(c.Status.Ref)); cerr != nil {
			s.log.Warn("could not close the thread on the backend", "conversation", name, "err", cerr)
		}
	}
	c.Spec.Closed = true
	updated, err := s.conversations.Update(c)
	if err != nil {
		return nil, err
	}
	updated.Status.Phase = api.PhaseClosed
	return s.conversations.UpdateStatus(updated)
}

// DeleteConversation removes a conversation. Its messages are left in the store
// under their own names, because they are the record of what was decided.
func (s *Service) DeleteConversation(name string) error {
	return s.conversations.Delete(name)
}

// findByThread locates the conversation a backend thread belongs to.
//
// It scans rather than keeping an index. At the scale this daemon runs at — one
// thread per agent, a few dozen at most — a scan costs nothing, and an index is
// a second copy of the truth that can drift from the store after a restart.
func (s *Service) findByThread(channel string, ref backend.ThreadRef) (*api.Conversation, bool) {
	items, _, err := s.conversations.List()
	if err != nil {
		return nil, false
	}
	for i := range items {
		c := items[i]
		if c.Spec.Channel == channel && c.Status.Ref == string(ref) {
			return &c, true
		}
	}
	return nil, false
}

// touchConversation records activity on a conversation.
func (s *Service) touchConversation(name string, mutate func(*api.Conversation)) {
	c, err := s.conversations.Get(name)
	if err != nil {
		return
	}
	now := time.Now().UTC()
	c.Status.LastActivityAt = &now
	mutate(c)
	if _, err := s.conversations.UpdateStatus(c); err != nil {
		s.log.Warn("could not record conversation activity", "conversation", name, "err", err)
	}
}
