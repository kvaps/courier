package courier

import (
	"context"
	"encoding/json"
	"time"

	"github.com/kvaps/courier/pkg/api"
	"github.com/kvaps/courier/pkg/store"
)

// maxWait bounds any blocking call. An agent asking to wait forever would hold
// an MCP call open past every timeout between it and the daemon, and come back
// with a transport error instead of an answer; a bounded wait that returns
// "still open" is something it can act on.
const maxWait = 30 * time.Minute

// WaitForAnswer blocks until a question is answered, withdrawn or fails, and
// returns it in whatever state it reached. A timeout is not an error: the
// message comes back still open, which is a true and useful answer to "has
// anyone replied yet".
func (s *Service) WaitForAnswer(ctx context.Context, name string, timeout time.Duration) (*api.Message, error) {
	m, err := s.messages.Get(name)
	if err != nil {
		return nil, err
	}
	if !m.Open() {
		return m, nil
	}
	// Subscribing from the version we just read at is what closes the race: an
	// answer landing between the read and the watch is replayed, not missed.
	events, cancel, err := s.store.Watch(m.Metadata.ResourceVersion, api.KindMessage)
	if err != nil {
		return nil, err
	}
	defer cancel()

	// Re-read once after subscribing, in case it was answered in the instant
	// before the watch existed and after the version we quoted.
	if cur, gerr := s.messages.Get(name); gerr == nil && !cur.Open() {
		return cur, nil
	}

	deadline := time.NewTimer(clampWait(timeout))
	defer deadline.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, api.NewTimeout("waiting for an answer to %q was cancelled", name)
		case <-deadline.C:
			return s.messages.Get(name)
		case ev, ok := <-events:
			if !ok {
				return s.messages.Get(name) // watch dropped us; report the current state
			}
			if ev.Name != name {
				continue
			}
			msg, derr := decodeMessage(ev)
			if derr != nil || msg.Open() {
				continue
			}
			return msg, nil
		}
	}
}

// WaitForInbound blocks until someone writes in the conversation after
// sinceVersion, and returns everything they wrote.
//
// This is how an agent hears a message that answers no question of its own —
// the operator writing unprompted. Passing the resourceVersion from the last
// listing makes the call exactly-once: nothing written in between is skipped,
// and nothing already seen comes back.
func (s *Service) WaitForInbound(ctx context.Context, conversation string, sinceVersion int64, timeout time.Duration) ([]api.Message, int64, error) {
	if _, err := s.conversations.Get(conversation); err != nil {
		return nil, 0, err
	}
	events, cancel, err := s.store.Watch(sinceVersion, api.KindMessage)
	if err != nil {
		return nil, 0, err
	}
	defer cancel()

	// Anything already written past sinceVersion is returned at once: an agent
	// that was busy should not have to wait for one more message to learn about
	// the ones it already missed.
	if have, rv := s.inboundSince(conversation, sinceVersion); len(have) > 0 {
		return have, rv, nil
	}

	deadline := time.NewTimer(clampWait(timeout))
	defer deadline.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, sinceVersion, api.NewTimeout("waiting for a message in %q was cancelled", conversation)
		case <-deadline.C:
			return nil, s.store.Version(), nil
		case ev, ok := <-events:
			if !ok {
				have, rv := s.inboundSince(conversation, sinceVersion)
				return have, rv, nil
			}
			msg, derr := decodeMessage(ev)
			if derr != nil {
				continue
			}
			if msg.Spec.Conversation != conversation || msg.Spec.Direction != api.Inbound {
				continue
			}
			return []api.Message{*msg}, ev.ResourceVersion, nil
		}
	}
}

// inboundSince collects the conversation's inbound messages newer than a
// version, with the store version to resume from next time.
func (s *Service) inboundSince(conversation string, since int64) ([]api.Message, int64) {
	items, rv, err := s.messages.List()
	if err != nil {
		return nil, since
	}
	var out []api.Message
	for _, m := range items {
		if m.Spec.Conversation == conversation &&
			m.Spec.Direction == api.Inbound &&
			m.Metadata.ResourceVersion > since {
			out = append(out, m)
		}
	}
	return out, rv
}

func decodeMessage(ev store.Event) (*api.Message, error) {
	var m api.Message
	if err := json.Unmarshal(ev.Object, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func clampWait(d time.Duration) time.Duration {
	switch {
	case d <= 0:
		return time.Second // "check and tell me now"
	case d > maxWait:
		return maxWait
	default:
		return d
	}
}
