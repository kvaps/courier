package api

// Object is what every resource can do: name its own kind and hand out its
// metadata for the store to stamp. It is the minimum that lets the store treat
// all kinds alike — assign names and resource versions, check optimistic
// concurrency, persist, and fan out watch events — without knowing anything
// about what any particular kind means.
type Object interface {
	// ObjectKind returns the resource's kind, e.g. "Message".
	ObjectKind() string
	// ObjectMeta returns a pointer to the object's metadata, so the store can
	// stamp identity and versioning onto it in place.
	ObjectMeta() *ObjectMeta
}

// ObjectKind returns the resource's kind.
func (c *Channel) ObjectKind() string { return KindChannel }

// ObjectMeta returns the object's metadata.
func (c *Channel) ObjectMeta() *ObjectMeta { return &c.Metadata }

// ObjectKind returns the resource's kind.
func (c *Conversation) ObjectKind() string { return KindConversation }

// ObjectMeta returns the object's metadata.
func (c *Conversation) ObjectMeta() *ObjectMeta { return &c.Metadata }

// ObjectKind returns the resource's kind.
func (m *Message) ObjectKind() string { return KindMessage }

// ObjectMeta returns the object's metadata.
func (m *Message) ObjectMeta() *ObjectMeta { return &m.Metadata }

var (
	_ Object = (*Channel)(nil)
	_ Object = (*Conversation)(nil)
	_ Object = (*Message)(nil)
)
