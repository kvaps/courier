// Package backend is courier's extension point on the human side: one
// implementation per messaging transport.
//
// The daemon knows nothing about Telegram. It opens conversations, sends
// rendered text, edits what it already sent, and consumes a stream of inbound
// events — and every transport that can do those four things can carry a
// courier conversation. A new backend is a package that registers itself; no
// part of the daemon changes.
package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/kvaps/courier/pkg/api"
)

// Backend is one messaging transport.
//
// Implementations are driven by exactly one goroutine for Run and may receive
// concurrent calls to everything else, so they must be safe for concurrent use.
type Backend interface {
	// Kind is the registered name, e.g. "telegram".
	Kind() string

	// Connect validates credentials and resolves the destination, returning
	// what the daemon should report in the Channel's status. It is called
	// before Run and may be called again to re-check a failed channel.
	Connect(ctx context.Context) (Identity, error)

	// Run consumes inbound traffic until ctx is cancelled, handing each event
	// to sink. It returns nil on a clean shutdown; any other return means the
	// transport gave up and the channel is Failed.
	Run(ctx context.Context, sink Sink) error

	// OpenThread creates the transport-side container for a conversation — on
	// Telegram a forum topic — and returns its reference. Implementations that
	// have no notion of threads return the channel's own reference, so every
	// conversation shares one stream.
	OpenThread(ctx context.Context, t ThreadSpec) (ThreadRef, error)

	// CloseThread closes a thread. Closing is not deleting: the history stays
	// where the human can read it.
	CloseThread(ctx context.Context, ref ThreadRef) error

	// Send delivers text to a thread and returns the transport's own id for the
	// message, which is what later makes it editable.
	Send(ctx context.Context, ref ThreadRef, out Outgoing) (MessageRef, error)

	// Edit replaces the text of a message already sent. It is how a withdrawn
	// question stops looking like a live one. A transport that cannot edit
	// returns an error satisfying api.ReasonNotSupported, and the daemon posts
	// a follow-up note instead.
	Edit(ctx context.Context, ref MessageRef, text string) error

	// React puts a short acknowledgement on a message the human sent — seen,
	// accepted. Best-effort by contract: a transport without reactions returns
	// a NotSupported error and nothing about the delivery changes.
	React(ctx context.Context, ref MessageRef, mark Mark) error
}

// Identity is what a connected backend reports about itself, for the operator
// to confirm the daemon is the bot they think it is.
type Identity struct {
	// Self is who the transport is authenticated as, e.g. "@some_bot".
	Self string
	// Target describes where messages go, e.g. a group title.
	Target string
}

// ThreadSpec is what the daemon wants a thread to be.
type ThreadSpec struct {
	// Conversation is the resource name, so a backend can make its own thread
	// title deterministic and recoverable.
	Conversation string
	Title        string
	Subject      string
}

// ThreadRef is a transport's own identifier for a thread. It is opaque to the
// daemon and stored verbatim in Conversation.status.ref.
type ThreadRef string

// MessageRef is a transport's own identifier for a delivered message, stored in
// Message.status.ref.
type MessageRef struct {
	Thread ThreadRef
	ID     string
}

// Mark is a short acknowledgement placed on a human's message.
type Mark string

// Acknowledgement marks.
const (
	// MarkSeen: the daemon has the message and is acting on it.
	MarkSeen Mark = "seen"
	// MarkAccepted: the message reached its destination.
	MarkAccepted Mark = "accepted"
	// MarkClear: remove any mark.
	MarkClear Mark = "clear"
)

// Outgoing is one message to deliver.
type Outgoing struct {
	// Text is the fully rendered body. Backends deliver it as plain text:
	// agent-authored content is full of characters a markup parser chokes on,
	// and a parse error loses the message someone was waiting for.
	Text string
	// Body is the structured original, for a transport rich enough to lay it
	// out better than plain text. Ignoring it is correct and expected.
	Body api.Body
	// AwaitReply marks a question, which some transports can present specially.
	AwaitReply bool
	// Choices, when set, are the buttons to draw under the message, in order.
	// A transport without buttons reports NotSupported rather than delivering
	// the text alone: a drafted answer whose buttons silently vanished would
	// look to the reader like an ordinary message and to the drafter like a
	// live offer nobody ever took.
	Choices []api.Choice
	// ReplyTo, when set, asks the transport to attach this message to one it
	// already delivered, so a drafted answer arrives quoting the question it
	// answers instead of needing the daemon to restate it.
	ReplyTo MessageRef
	// Files travel with the message. Path is set and readable; a transport
	// that cannot carry files reports NotSupported rather than dropping them
	// silently, so the sender learns the file did not arrive.
	Files []api.Attachment
}

// Inbound is one message observed from the human side.
type Inbound struct {
	Thread ThreadRef
	Ref    MessageRef
	// Author is a display name for the log; it is not an identity the daemon
	// authorises against.
	Author string
	// AuthorID is the transport's stable id for the sender, which is what an
	// allow-list is checked against.
	AuthorID string
	Text     string
	At       time.Time
	// Files are what the person attached, already downloaded into InboxDir.
	// The backend does the downloading because it holds the credential; the
	// daemon only records where the bytes landed.
	Files []api.Attachment
}

// Press is one tap on a button courier drew, observed on the human side.
//
// It is not an Inbound: nobody wrote anything. The words were composed before
// the button existed, and what the transport reports is only which option was
// taken, on which message, by whom.
type Press struct {
	Thread ThreadRef
	// Message is the message the button sits on.
	Message MessageRef
	// Choice is the transport handle of the option taken — for Telegram, the
	// callback_data, which is why the answer itself was never put in it.
	Choice string
	// Author is a display name for the log.
	Author string
	// AuthorID is the transport's stable id for whoever pressed, checked
	// against the same allow-list a written message is checked against.
	AuthorID string
	At       time.Time
}

// PressResult is what to show the person who pressed, in whatever momentary
// acknowledgement the transport has — on Telegram, the toast over the button.
type PressResult struct {
	// Toast is one short line. It is the only feedback a press gets before the
	// message itself is rewritten, so it says what happened, not "ok".
	Toast string
	// Alert asks for the more insistent form, for a press that changed nothing.
	Alert bool
}

// Sink receives inbound traffic from a running backend.
type Sink interface {
	// Receive is called for each inbound message. It must not block for long:
	// a backend's receive loop is single-threaded, and a slow sink stalls
	// everything else arriving on that transport.
	Receive(ctx context.Context, channel string, in Inbound)
	// Press is called when a person takes one of the options courier offered.
	// Unlike Receive it answers: the transport has to tell the person what
	// their tap did, and only the daemon knows. It must be quick for the same
	// reason Receive must be — it runs on the backend's single receive loop.
	Press(ctx context.Context, channel string, p Press) (PressResult, error)
	// SetStatus reports a change in the transport's health, so a channel that
	// has lost its connection says so instead of silently going quiet.
	SetStatus(ctx context.Context, channel string, phase api.Phase, message string)
}

// Env is what the daemon provides a backend beyond its own configuration.
type Env struct {
	// Channel is the resource name of the channel being built.
	Channel string
	// StateDir is a directory the backend may keep private state in, such as a
	// stream cursor. It exists and is writable. Nothing in it is part of the
	// API, and the daemon never reads it.
	StateDir string
	// InboxDir is where the backend saves files it receives. Unlike StateDir
	// the daemon does read it: the paths a backend reports for inbound files
	// are handed to agents, which open them as ordinary local files.
	InboxDir string
}

// Factory builds a backend from a channel's configuration.
//
// It is given the raw config so each backend owns its own schema and its own
// validation — including the rule that matters most here: a credential must be
// named, never inlined. A config carrying a literal token is rejected, because
// the object would then be readable through the API and persisted to disk.
type Factory func(env Env, config json.RawMessage) (Backend, error)

var (
	registryMu sync.RWMutex
	registry   = map[string]Factory{}
)

// Register makes a backend available under a name. It is meant to be called
// from a package's init, so linking the package in is what enables it.
// Registering the same name twice is a programming error and panics.
func Register(kind string, f Factory) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, dup := registry[kind]; dup {
		panic("backend: duplicate registration for " + kind)
	}
	registry[kind] = f
}

// New builds a backend for a channel.
func New(kind string, env Env, config json.RawMessage) (Backend, error) {
	registryMu.RLock()
	f, ok := registry[kind]
	registryMu.RUnlock()
	if !ok {
		return nil, api.NewInvalid("unknown backend %q; registered: %v", kind, Kinds())
	}
	return f(env, config)
}

// Kinds lists the registered backend names, sorted.
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

// String renders a message reference for a log line.
func (r MessageRef) String() string { return fmt.Sprintf("%s/%s", r.Thread, r.ID) }

// Zero reports whether the reference names nothing.
func (r MessageRef) Zero() bool { return r.ID == "" }
