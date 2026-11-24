package courier

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/kvaps/courier/pkg/api"
	"github.com/kvaps/courier/pkg/backend"
)

// The board is the pull half of the feed. Posts arrive silently; instead of a
// notification per question, the reader looks at one pinned message that says
// what is waiting on them and what was settled without them.

const (
	boardEvery   = 30 * time.Second
	settledSince = 24 * time.Hour
)

// runBoards keeps every channel's board current for the daemon's lifetime.
func (s *Service) runBoards() {
	last := map[string]string{}
	tick := time.NewTicker(boardEvery)
	defer tick.Stop()
	for {
		s.updateBoards(last)
		select {
		case <-s.baseCtx.Done():
			return
		case <-tick.C:
		}
	}
}

func (s *Service) updateBoards(last map[string]string) {
	s.mu.Lock()
	running := make(map[string]backend.Backend, len(s.running))
	for name, rt := range s.running {
		running[name] = rt.backend
	}
	s.mu.Unlock()
	for name, be := range running {
		b, ok := be.(backend.Boarder)
		if !ok {
			continue
		}
		board := s.board(name, time.Now())
		raw, _ := json.Marshal(board)
		if last[name] == string(raw) {
			continue
		}
		ctx, cancel := context.WithTimeout(s.baseCtx, 30*time.Second)
		err := b.Board(ctx, board)
		cancel()
		if err != nil {
			s.log.Warn("could not update the board", "channel", name, "err", err)
			continue
		}
		last[name] = string(raw)
	}
}

// board computes what a channel's reader needs at a glance.
func (s *Service) board(channel string, now time.Time) backend.Board {
	var out backend.Board
	convs, _, err := s.conversations.List()
	if err != nil {
		return out
	}
	byName := map[string]*api.Conversation{}
	for i := range convs {
		if convs[i].Spec.Channel == channel {
			byName[convs[i].Metadata.Name] = &convs[i]
		}
	}
	msgs, _, err := s.messages.List()
	if err != nil {
		return out
	}
	for i := range msgs {
		m := &msgs[i]
		c, ok := byName[m.Spec.Conversation]
		if !ok || m.Spec.Direction != api.Outbound || !m.Spec.AwaitReply {
			continue
		}
		switch {
		case m.Status.Phase == api.PhaseHeld:
			out.Held++
		case m.Status.Phase == api.PhaseSent && !c.Spec.Closed:
			since := m.Metadata.CreationTimestamp
			if m.Status.SentAt != nil {
				since = *m.Status.SentAt
			}
			out.Waiting = append(out.Waiting, backend.BoardItem{
				Title: c.Spec.Title, Summary: m.Spec.Body.Summary(), Since: since,
				Ref: backend.MessageRef{Thread: backend.ThreadRef(c.Status.Ref), ID: m.Status.Ref},
			})
		case m.Status.Phase == api.PhaseAnswered && m.Status.Review != nil && m.Status.Review.Action == "answered" &&
			m.Status.AnsweredAt != nil && now.Sub(*m.Status.AnsweredAt) < settledSince:
			out.Settled = append(out.Settled, backend.BoardItem{
				Title: c.Spec.Title, Summary: m.Spec.Body.Summary(), Answer: m.Status.Answer, Since: *m.Status.AnsweredAt,
			})
		}
	}
	sort.Slice(out.Waiting, func(i, j int) bool { return out.Waiting[i].Since.Before(out.Waiting[j].Since) })
	sort.Slice(out.Settled, func(i, j int) bool { return out.Settled[i].Since.After(out.Settled[j].Since) })
	return out
}
