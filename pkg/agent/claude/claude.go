package claude

import (
	"context"
	"strings"
	"time"

	"github.com/kvaps/courier/pkg/agent"
	"github.com/kvaps/courier/pkg/api"
)

// Kind is the registered sink name.
const Kind = "claude"

func init() {
	agent.Register(Kind, func() (agent.Sink, error) { return &Sink{}, nil })
}

// Sink delivers courier messages into local Claude Code background sessions.
type Sink struct{ c control }

// Kind returns the registered sink name.
func (s *Sink) Kind() string { return Kind }

// Resolve reports whether an address names a reachable session. It accepts a
// short id, a full session id or a display name, and is what lets a
// conversation be rejected when it is created rather than when the operator
// first writes into it and hears nothing back.
func (s *Sink) Resolve(_ context.Context, target string) (agent.Target, error) {
	ref := strings.TrimSpace(target)
	if ref == "" {
		return agent.Target{}, api.NewInvalid("empty agent address")
	}
	if sess, ok := s.c.find(ref); ok {
		return agent.Target{Address: sess.Short, Name: sess.Name, Live: true, Wakeable: true}, nil
	}
	// Not running. It is still reachable if it can be woken in place, which is
	// decided by its on-disk job state, not by the roster.
	if wakeable(ref) {
		name := ref
		if js, err := readJobState(ref); err == nil && js.Name != "" {
			name = js.Name
		}
		return agent.Target{Address: ref, Name: name, Live: false, Wakeable: true}, nil
	}
	return agent.Target{}, api.NewNotFound("agent session", ref)
}

// Deliver pushes a message into a session and confirms it arrived.
//
// The confirmation is the point. The daemon's reply op acknowledges that text
// reached the session's REPL, which is not the same as a turn having started:
// a multi-line body lands as an unsubmitted paste. So the delivery is checked
// against the session's own transcript, and only what is found there is
// reported as confirmed.
func (s *Sink) Deliver(ctx context.Context, target string, msg agent.Message) (agent.Receipt, error) {
	text := strings.TrimSpace(msg.Text)
	if text == "" {
		return agent.Receipt{}, api.NewInvalid("empty message")
	}
	t, err := s.Resolve(ctx, target)
	if err != nil {
		return agent.Receipt{}, err
	}

	receipt := agent.Receipt{Address: t.Address}
	if !t.Live {
		if err := s.c.wake(t.Address); err != nil {
			return agent.Receipt{}, err
		}
		receipt.Woke = true
	}

	sess, _ := s.c.find(t.Address)
	m := markTranscripts(sess.SessionID)

	if err := s.c.reply(t.Address, text); err != nil {
		return receipt, err
	}
	if how, ok := s.confirm(ctx, t.Address, m, text, 4*time.Second); ok {
		receipt.How = how
		return receipt, nil
	}

	// Not in the transcript yet. Three states look alike here and want opposite
	// responses: a dialog owns the keyboard (do not touch it), a turn is
	// already running so the message is queued (leave it, it will be consumed),
	// or the text is genuinely sitting unsubmitted (press Enter).
	for attempt := 0; attempt < 2; attempt++ {
		screen, _ := s.c.snapshot(t.Address, 60)
		if dialogUp(screen) {
			return receipt, api.NewBackendError(
				"session %s is holding a dialog, so it cannot take typed input until that is answered; "+
					"the message was not delivered and no key was pressed (a blind Enter there would answer the dialog)", t.Address)
		}
		if cur, ok := s.c.find(t.Address); ok && cur.busy() || m.grew() {
			receipt.Queued = true
			receipt.How = "queued behind the turn the session is already running; it will be consumed when that turn ends"
			return receipt, nil
		}
		if err := s.c.pressEnter(t.Address); err != nil {
			return receipt, err
		}
		if how, ok := s.confirm(ctx, t.Address, m, text, 6*time.Second); ok {
			receipt.How = how
			return receipt, nil
		}
	}
	return receipt, api.NewBackendError(
		"session %s took the message but never started a turn; Enter was sent twice without effect, "+
			"so the text is sitting unsubmitted in its input box", t.Address)
}

// confirm polls for evidence that the message landed, preferring ground truth.
func (s *Sink) confirm(ctx context.Context, short string, m mark, text string, timeout time.Duration) (string, bool) {
	deadline := time.Now().Add(timeout)
	for {
		if m.landed(text) {
			return "found as a user message in the session's own transcript", true
		}
		if !m.available() {
			// No transcript to check against — a session that has never been
			// prompted. Fall back to the roster having moved, which says the
			// session started doing something, not that it was our text.
			if cur, ok := s.c.find(short); ok && cur.busy() {
				return "the session started a turn (roster only; not confirmed against its transcript)", true
			}
		}
		if time.Now().After(deadline) {
			return "", false
		}
		select {
		case <-ctx.Done():
			return "", false
		case <-time.After(150 * time.Millisecond):
		}
	}
}
