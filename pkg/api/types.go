// Package api holds courier's resource types — the wire contract shared by the
// HTTP API, the MCP tool set and the store.
//
// The shapes follow Kubernetes: every object is
// {apiVersion, kind, metadata, spec, status}, lists carry a resourceVersion,
// and the client's declared intent (spec) is kept strictly apart from what the
// daemon observed (status). Two Kubernetes ideas are deliberately absent.
// There are no namespaces — courier is one person's local daemon, and a
// namespace would be ceremony with nothing on the other side of it. And status
// is a real, writable subresource rather than a value recomputed on read,
// because the interesting half of this system is what happened out in the
// world: whether a message was actually delivered, and what the human said back.
package api

import (
	"encoding/json"
	"time"
)

// Version is the single API version this daemon serves.
const Version = "courier/v1"

// Kinds served by the API.
const (
	KindChannel      = "Channel"
	KindConversation = "Conversation"
	KindMessage      = "Message"
)

// TypeMeta identifies an object's schema. It is inlined into every resource so
// that an object read from a log, a file or an MCP result is self-describing.
type TypeMeta struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
}

// ObjectMeta is the identity and bookkeeping every resource carries.
//
// Name is the primary key and is chosen by the client or generated from
// GenerateName; UID distinguishes a deleted object from a new one that reused
// its name. ResourceVersion is the store's global monotonic counter as of this
// object's last write — it is what makes both optimistic concurrency and
// resumable watch possible, so it is set by the server and never by a client.
type ObjectMeta struct {
	Name string `json:"name"`
	// GenerateName asks the server to mint a unique name from this prefix. It
	// is a request, not an identity: it is cleared once Name is assigned.
	GenerateName string `json:"generateName,omitempty"`
	UID          string `json:"uid,omitempty"`
	// ResourceVersion is opaque to clients apart from equality: send it back on
	// an update to say "apply this only if nothing changed underneath me".
	ResourceVersion int64 `json:"resourceVersion,omitempty"`
	// Generation counts spec changes only, so a controller can tell a real
	// intent change from its own status write.
	Generation        int64             `json:"generation,omitempty"`
	CreationTimestamp time.Time         `json:"creationTimestamp"`
	DeletionTimestamp *time.Time        `json:"deletionTimestamp,omitempty"`
	Labels            map[string]string `json:"labels,omitempty"`
	Annotations       map[string]string `json:"annotations,omitempty"`
}

// ListMeta is the envelope metadata of a list response. Its ResourceVersion is
// the store version the list was taken at, so a watch started from it sees
// every change after the list and none before it.
type ListMeta struct {
	ResourceVersion int64 `json:"resourceVersion"`
}

// Phase is the coarse state of a resource, in the Kubernetes sense: a summary
// for humans and simple clients, never the authoritative state machine.
type Phase string

// Phases shared across kinds.
const (
	// PhasePending: accepted by the daemon, not yet acted on in the world.
	PhasePending Phase = "Pending"
	// PhaseReady: a Channel is connected; a Conversation exists on the backend.
	PhaseReady Phase = "Ready"
	// PhaseSent: an outbound Message reached the backend.
	PhaseSent Phase = "Sent"
	// PhaseAnswered: a human replied to a message that asked for an answer.
	PhaseAnswered Phase = "Answered"
	// PhaseCancelled: the question was withdrawn before it was answered.
	PhaseCancelled Phase = "Cancelled"
	// PhaseClosed: a Conversation was closed.
	PhaseClosed Phase = "Closed"
	// PhaseFailed: the backend refused, and retrying will not help by itself.
	PhaseFailed Phase = "Failed"
)

// Channel is one configured transport: a backend plus where it delivers.
//
// The token is not part of the object and cannot be. Spec.Config names where to
// read the credential from; the daemon resolves it at connect time and keeps it
// out of the store, the API and the logs — see the backend's config validation.
type Channel struct {
	TypeMeta `json:",inline"`
	Metadata ObjectMeta    `json:"metadata"`
	Spec     ChannelSpec   `json:"spec"`
	Status   ChannelStatus `json:"status"`
}

// ChannelSpec is what the operator asked for.
type ChannelSpec struct {
	// Backend names a registered backend implementation, e.g. "telegram".
	Backend string `json:"backend"`
	// Config is the backend's own configuration, validated by that backend.
	Config json.RawMessage `json:"config,omitempty"`
}

// ChannelStatus is what the daemon observed.
type ChannelStatus struct {
	Phase Phase `json:"phase,omitempty"`
	// Identity is who the backend is connected as, for the operator to confirm
	// the daemon is the bot they think it is (e.g. "@some_bot").
	Identity string `json:"identity,omitempty"`
	// Target is a human-readable description of where messages go.
	Target string `json:"target,omitempty"`
	// Message explains a non-Ready phase.
	Message     string     `json:"message,omitempty"`
	ConnectedAt *time.Time `json:"connectedAt,omitempty"`
	ObservedAt  *time.Time `json:"observedAt,omitempty"`
}

// Conversation is one thread with a human — on Telegram, a forum topic.
//
// It exists so that several agents asking about unrelated things do not
// interleave into one stream the reader has to demultiplex by hand.
type Conversation struct {
	TypeMeta `json:",inline"`
	Metadata ObjectMeta         `json:"metadata"`
	Spec     ConversationSpec   `json:"spec"`
	Status   ConversationStatus `json:"status"`
}

// ConversationSpec is what the client asked for.
type ConversationSpec struct {
	// Channel is the name of the Channel this thread lives on.
	Channel string `json:"channel"`
	// Title names the thread for the reader; on Telegram it is the topic title.
	Title string `json:"title"`
	// Subject is an optional one-line description of what this thread is about.
	Subject string `json:"subject,omitempty"`
	// Closed asks the daemon to close the thread. Closing is a spec field
	// rather than a delete so the record and its answers survive.
	Closed bool `json:"closed,omitempty"`
	// Agent, when set, is where the machine side of this conversation lives, so
	// that a message the human writes is pushed to it instead of waiting to be
	// collected. Without it the conversation still works — the agent just has
	// to ask, and a message sent while it is thinking sits unread until it does.
	Agent *AgentRef `json:"agent,omitempty"`
}

// AgentRef addresses the machine side of a conversation.
type AgentRef struct {
	// Sink names a registered delivery mechanism, e.g. "claude".
	Sink string `json:"sink"`
	// Address is the agent's address within that sink.
	Address string `json:"address"`
}

// ConversationStatus is what the daemon observed.
type ConversationStatus struct {
	Phase Phase `json:"phase,omitempty"`
	// Ref is the backend's own identifier for the thread (a topic id).
	Ref            string     `json:"ref,omitempty"`
	Message        string     `json:"message,omitempty"`
	OpenedAt       *time.Time `json:"openedAt,omitempty"`
	LastActivityAt *time.Time `json:"lastActivityAt,omitempty"`
	MessageCount   int        `json:"messageCount,omitempty"`
	// PendingQuestion names the outbound Message currently waiting on an
	// answer, so the next thing the human writes has somewhere to land — and so
	// a second question can be refused rather than quietly stealing the first
	// one's answer.
	PendingQuestion string `json:"pendingQuestion,omitempty"`
	// PendingQuestionSince is when that question was sent, so a reader can see
	// how long an agent has been standing still. It is a timestamp and not a
	// duration on purpose: a stored duration is a lie the moment it is written,
	// and the one place a duration is honest is where it is computed on read —
	// /api/healthz.
	PendingQuestionSince *time.Time `json:"pendingQuestionSince,omitempty"`
	// Agent describes the resolved machine side, as observed.
	Agent *AgentStatus `json:"agent,omitempty"`
}

// AgentStatus is what the daemon observed about the conversation's agent.
type AgentStatus struct {
	Address string `json:"address,omitempty"`
	Name    string `json:"name,omitempty"`
	// Reachable is false once a delivery has failed, with Message saying why.
	Reachable bool   `json:"reachable"`
	Message   string `json:"message,omitempty"`
	// LastDeliveryAt is when a message was last pushed to the agent.
	LastDeliveryAt *time.Time `json:"lastDeliveryAt,omitempty"`
}

// Direction says which way a message travels.
type Direction string

// Message directions.
const (
	// Outbound: written by an agent, delivered to the human.
	Outbound Direction = "Outbound"
	// Inbound: written by the human, observed by the daemon.
	Inbound Direction = "Inbound"
)

// Message is one item in a conversation.
type Message struct {
	TypeMeta `json:",inline"`
	Metadata ObjectMeta    `json:"metadata"`
	Spec     MessageSpec   `json:"spec"`
	Status   MessageStatus `json:"status"`
}

// MessageSpec is what the client asked to be delivered.
type MessageSpec struct {
	// Conversation is the name of the Conversation this belongs to.
	Conversation string    `json:"conversation"`
	Direction    Direction `json:"direction"`
	Body         Body      `json:"body"`
	// AwaitReply marks this as a question: the daemon holds it open, routes the
	// human's next message in the thread onto it as the answer, and lets the
	// asker wait for or withdraw it.
	AwaitReply bool `json:"awaitReply,omitempty"`
	// InReplyTo names the Message this one is about: on an inbound message, the
	// question it answers, set by the daemon; on an outbound drafted answer,
	// the question whose answer is being offered, set by whoever drafted it.
	InReplyTo string `json:"inReplyTo,omitempty"`
	// Choices turn an outbound message into a drafted answer the reader can
	// confirm with one tap instead of composing a reply. They are only ever
	// set on a message that names an open question in InReplyTo — the point of
	// a draft is that somebody already wrote the answer and the reader only
	// has to agree with it.
	Choices []Choice `json:"choices,omitempty"`
	// DraftedBy names whoever wrote those answers, in the words the reader
	// sees above the buttons. It is required alongside Choices and it is not
	// decoration: an approval that takes one tap is cheap, and the record has
	// to say who actually chose the words when one later turns out to be wrong.
	DraftedBy string `json:"draftedBy,omitempty"`
	// Attachments are files travelling with the message.
	//
	// Outbound, a client supplies Path and the daemon fills in the rest.
	// Inbound, the daemon fills in everything: it downloads what the person
	// sent and records where it put it, so an agent reads an ordinary local
	// file rather than learning the transport's download protocol.
	Attachments []Attachment `json:"attachments,omitempty"`
}

// Attachment is one file travelling with a message.
//
// Everything here is a path on this machine, because courier is local: both
// sides of a conversation share a filesystem, and the daemon's job is to move
// the bytes across the transport in between, not to invent a blob store.
type Attachment struct {
	// Path is the file on this machine. It is the only field a sender sets.
	Path string `json:"path"`
	// Name is what the reader sees, defaulting to the base name of Path.
	Name string `json:"name,omitempty"`
	// Size in bytes, filled in by the daemon.
	Size int64 `json:"size,omitempty"`
	// MediaType is the guessed content type, filled in by the daemon.
	MediaType string `json:"mediaType,omitempty"`
	// Ref is the transport's own identifier for the file, recorded on an
	// inbound attachment so the same file can be recognised if it arrives again.
	Ref string `json:"ref,omitempty"`
}

// Choice is one button under a drafted answer: what the reader taps, and the
// exact words the agent is told when they do.
//
// The answer travels in the resource and never in the button. Telegram caps the
// data a button carries at 64 bytes, a fraction of any real answer, so the
// button carries Ref — a short handle the daemon mints — and the words stay
// here, where what was on offer can still be read back afterwards.
type Choice struct {
	// ID is the drafter's own name for this option, e.g. "send" or "refuse".
	// It is what an agent branches on without parsing prose.
	ID string `json:"id"`
	// Label is the button's text: two or three words, read on a phone.
	Label string `json:"label"`
	// Answer is what the agent is told when this button is tapped. It is
	// always a real answer, never an acknowledgement — a button that answered
	// nothing would close a question without deciding it, and withdrawing a
	// question that stopped mattering is what cancel is for.
	Answer string `json:"answer"`
	// Ref is the transport's handle for this button, minted by the daemon and
	// stored so a tap can still be resolved after a restart.
	Ref string `json:"ref,omitempty"`
}

// Approval records that an answer was confirmed rather than composed: somebody
// else wrote the words and the reader agreed to them with one tap.
//
// It is a separate field rather than a turn of phrase in the answer because the
// answer itself has to stay exactly what was offered. What this adds is the
// authorship — who wrote it, which option was taken, and where the rest of the
// offer can be read.
type Approval struct {
	// DraftedBy names whoever wrote the answer.
	DraftedBy string `json:"draftedBy"`
	// Choice is the id of the option that was taken.
	Choice string `json:"choice"`
	// Label is what that button said.
	Label string `json:"label,omitempty"`
	// Draft names the Message that carried the buttons, so the whole offer —
	// every option, not only the one taken — can be read back.
	Draft string `json:"draft,omitempty"`
}

// Body is a message composed the way a colleague would text it: one decision,
// re-oriented in a line, with the sender's own answer already proposed.
//
// The shape is the point. An agent handed a free-form string writes a wall of
// jargon about a thread the reader has not seen in a week; four named fields
// make the useful message the easy one to write. Context re-orients, Question
// states the choice in plain words, Proposal commits the sender to an answer so
// the reader can say "OK" instead of composing one, and Progress shows the end
// coming. Text is the escape hatch for anything that is not a decision — a
// status note, a heads-up, a finished result.
type Body struct {
	// Context re-orients the reader in one line: which PR, doc, or thread, who
	// is involved, what is on the table.
	Context string `json:"context,omitempty"`
	// Question is the choice itself, as a human either/or, without jargon.
	Question string `json:"question,omitempty"`
	// Proposal is the sender's own recommended answer. A question that arrives
	// without one makes the reader do the work the sender should have done.
	Proposal string `json:"proposal,omitempty"`
	// Progress shows where this sits in a batch, so a long run is visibly finite.
	Progress *Progress `json:"progress,omitempty"`
	// Text is free-form content, used on its own for anything that is not a
	// decision, and for the human's words on an inbound message.
	Text string `json:"text,omitempty"`
}

// Progress is the "3/12" prefix of a batched run.
type Progress struct {
	Index int `json:"index"`
	Total int `json:"total"`
}

// MessageStatus is what became of the message.
type MessageStatus struct {
	Phase Phase `json:"phase,omitempty"`
	// Ref is the backend's identifier for the delivered message, which is what
	// lets a question be edited when it is withdrawn.
	Ref        string     `json:"ref,omitempty"`
	SentAt     *time.Time `json:"sentAt,omitempty"`
	Answer     string     `json:"answer,omitempty"`
	AnsweredAt *time.Time `json:"answeredAt,omitempty"`
	// AnsweredBy is who replied, for the log — a display name, not an identity
	// the daemon authorises against.
	AnsweredBy string `json:"answeredBy,omitempty"`
	// Approval is set when the answer was a draft the reader confirmed rather
	// than words they wrote. Absent means they typed it themselves.
	Approval *Approval `json:"approval,omitempty"`
	// Message explains a Failed or Cancelled phase.
	Message string `json:"message,omitempty"`
}

// Answered reports whether a question has its answer.
func (m *Message) Answered() bool { return m.Status.Phase == PhaseAnswered }

// Open reports whether a message is a question still waiting on the human.
// A cancelled or failed question is not open, and neither is one already
// answered — so the next inbound message does not land on a dead question.
func (m *Message) Open() bool {
	if !m.Spec.AwaitReply || m.Spec.Direction != Outbound {
		return false
	}
	switch m.Status.Phase {
	case PhasePending, PhaseSent:
		return true
	default:
		return false
	}
}

// DraftOpen reports whether this is a drafted answer whose buttons are still
// live — drawn, not yet taken, not withdrawn.
func (m *Message) DraftOpen() bool {
	if m.Spec.Direction != Outbound || len(m.Spec.Choices) == 0 {
		return false
	}
	switch m.Status.Phase {
	case PhasePending, PhaseSent:
		return true
	default:
		return false
	}
}

// ChoiceByRef finds the option a transport handle stands for.
func (m *Message) ChoiceByRef(ref string) (Choice, bool) {
	if ref == "" {
		return Choice{}, false
	}
	for _, c := range m.Spec.Choices {
		if c.Ref == ref {
			return c, true
		}
	}
	return Choice{}, false
}

// List is the envelope of any list response.
type List[T any] struct {
	TypeMeta `json:",inline"`
	Metadata ListMeta `json:"metadata"`
	Items    []T      `json:"items"`
}

// NewList builds a list envelope for a kind. The kind is the item's kind with
// "List" appended, as in Kubernetes.
func NewList[T any](kind string, rv int64, items []T) List[T] {
	if items == nil {
		items = []T{}
	}
	return List[T]{
		TypeMeta: TypeMeta{APIVersion: Version, Kind: kind + "List"},
		Metadata: ListMeta{ResourceVersion: rv},
		Items:    items,
	}
}

// EventType is the kind of change a watch reports.
type EventType string

// Watch event types, mirroring Kubernetes.
const (
	// Added: the object came into existence, or into the watched set.
	Added EventType = "ADDED"
	// Modified: the object changed.
	Modified EventType = "MODIFIED"
	// Deleted: the object went away.
	Deleted EventType = "DELETED"
	// Error: the watch cannot continue; Object carries a Status.
	Error EventType = "ERROR"
)

// WatchEvent is one frame of a watch stream.
type WatchEvent struct {
	Type   EventType       `json:"type"`
	Object json.RawMessage `json:"object"`
}
