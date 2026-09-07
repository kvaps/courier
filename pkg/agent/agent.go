// Package agent is courier's extension point on the machine side: one
// implementation per way of pushing a message into an agent.
//
// It is the mirror of pkg/backend. A backend reaches the human; a Sink reaches
// the machine. A conversation names one of each, and an inbound message is
// stored either way — the Sink only decides whether the agent finds out now or
// the next time it asks.
//
// Without a Sink an agent has to poll: a message written while it is thinking
// sits unread until it happens to call the daemon again. With one, the message
// arrives the moment it is sent, which is the difference between a channel a
// person can rely on and one they have to nudge.
package agent

import (
	"context"
	"sort"
	"sync"

	"github.com/kvaps/courier/pkg/api"
)

// Sink delivers a message to an agent.
type Sink interface {
	// Kind is the registered name, e.g. "claude".
	Kind() string
	// Deliver pushes a message to the agent addressed by target, returning how
	// the delivery was confirmed. An error means the agent did not get it; the
	// message stays in the store either way, so nothing is lost by failing.
	Deliver(ctx context.Context, target string, msg Message) (Receipt, error)
	// Resolve reports whether an address names a reachable agent, so a
	// conversation can be rejected at creation instead of at the first message
	// the human sends into it.
	Resolve(ctx context.Context, target string) (Target, error)
}

// Message is what the agent receives.
type Message struct {
	// Text is the fully rendered envelope, including who is writing and how to
	// answer. The Sink delivers it verbatim.
	Text string
	// Conversation is the resource name, so a delivery can be traced back.
	Conversation string
}

// Target describes an addressable agent.
type Target struct {
	// Address is the canonical form of the address, which may differ from what
	// the caller passed (a name resolved to an id).
	Address string
	// Name is a human-readable label for logs and status.
	Name string
	// Live reports whether the agent is running right now.
	Live bool
	// Wakeable reports whether a stopped agent can be brought back with its
	// history. An agent that is neither Live nor Wakeable cannot be reached.
	Wakeable bool
}

// Reachable reports whether a message can be delivered to this target at all.
func (t Target) Reachable() bool { return t.Live || t.Wakeable }

// Receipt describes a completed delivery.
type Receipt struct {
	// Address is the agent the message actually reached.
	Address string
	// How describes the evidence the delivery is based on, in words meant for
	// an operator reading a log — the daemon distinguishes "the transport
	// acknowledged it" from "it was found in the agent's own conversation".
	How string
	// Woke is true when the agent was stopped and had to be resumed first.
	Woke bool
	// Queued is true when the agent was mid-turn, so the message is waiting in
	// its input and will be consumed when the turn ends. It is a success, and
	// specifically must not be retried: a retry would deliver it twice.
	Queued bool
}

// Factory builds a sink. Sinks take no per-conversation configuration — an
// address is passed per delivery — so a factory takes nothing.
type Factory func() (Sink, error)

var (
	registryMu sync.RWMutex
	registry   = map[string]Factory{}
)

// Register makes a sink available under a name, from the package's init.
// Registering the same name twice is a programming error and panics.
func Register(kind string, f Factory) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, dup := registry[kind]; dup {
		panic("agent: duplicate registration for " + kind)
	}
	registry[kind] = f
}

// New builds a sink by name.
func New(kind string) (Sink, error) {
	registryMu.RLock()
	f, ok := registry[kind]
	registryMu.RUnlock()
	if !ok {
		return nil, api.NewInvalid("unknown agent sink %q; registered: %v", kind, Kinds())
	}
	return f()
}

// Kinds lists the registered sink names, sorted.
func Kinds() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
