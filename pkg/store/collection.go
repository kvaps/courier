package store

import (
	"encoding/json"
	"os"

	"github.com/kvaps/courier/pkg/api"
)

// Collection is the typed view of one kind.
//
// The double type parameter is Go's way of saying "T whose pointer is an
// api.Object": T is the value type callers hold, PT is the pointer type that
// carries the metadata accessors. Construct one with For.
type Collection[T any, PT interface {
	*T
	api.Object
}] struct {
	s    *Store
	kind string
}

// For builds the typed view of a kind:
//
//	messages := store.For[api.Message, *api.Message](s)
func For[T any, PT interface {
	*T
	api.Object
}](s *Store) *Collection[T, PT] {
	var zero T
	return &Collection[T, PT]{s: s, kind: PT(&zero).ObjectKind()}
}

// Kind is the resource kind this collection holds.
func (c *Collection[T, PT]) Kind() string { return c.kind }

// Get returns a copy of the named object.
func (c *Collection[T, PT]) Get(name string) (PT, error) {
	c.s.mu.RLock()
	defer c.s.mu.RUnlock()
	return c.getLocked(name)
}

func (c *Collection[T, PT]) getLocked(name string) (PT, error) {
	rec, ok := c.s.objects[c.kind][name]
	if !ok {
		return nil, api.NewNotFound(c.kind, name)
	}
	return c.decode(rec.raw)
}

// clone returns an independent copy, so stamping identity and versions onto an
// object never reaches through into the caller's own value. A caller that holds
// its copy for a retry, or reads it after the write, should see what it built —
// not what the store did to it.
func (c *Collection[T, PT]) clone(obj PT) (PT, error) {
	raw, err := json.Marshal(obj)
	if err != nil {
		return nil, api.NewInternalError("copy %s: %v", c.kind, err)
	}
	return c.decode(raw)
}

func (c *Collection[T, PT]) decode(raw json.RawMessage) (PT, error) {
	obj := PT(new(T))
	if err := json.Unmarshal(raw, obj); err != nil {
		return nil, api.NewInternalError("decode %s: %v", c.kind, err)
	}
	return obj, nil
}

// List returns every object of this kind, sorted by name, together with the
// store version the listing was taken at — the version a watch should resume
// from so that it sees every later change and no earlier one.
func (c *Collection[T, PT]) List() ([]T, int64, error) {
	c.s.mu.RLock()
	defer c.s.mu.RUnlock()

	names := c.s.listNames(c.kind)
	items := make([]T, 0, len(names))
	for _, n := range names {
		obj, err := c.getLocked(n)
		if err != nil {
			return nil, 0, err
		}
		items = append(items, *obj)
	}
	return items, c.s.rv, nil
}

// Create stores a new object, assigning its name (from GenerateName when the
// name is empty), uid, creation time, generation and resource version.
func (c *Collection[T, PT]) Create(in PT) (PT, error) {
	obj, err := c.clone(in)
	if err != nil {
		return nil, err
	}

	c.s.mu.Lock()
	defer c.s.mu.Unlock()

	meta := obj.ObjectMeta()
	if meta.Name == "" && meta.GenerateName != "" {
		name, err := c.s.generateName(c.kind, meta.GenerateName)
		if err != nil {
			return nil, err
		}
		meta.Name = name
	}
	if err := nameSafe(meta.Name); err != nil {
		return nil, err
	}
	if _, exists := c.s.objects[c.kind][meta.Name]; exists {
		return nil, api.NewAlreadyExists(c.kind, meta.Name)
	}

	meta.GenerateName = ""
	meta.UID = newUID()
	meta.CreationTimestamp = now()
	meta.Generation = 1
	meta.ResourceVersion = c.s.next()

	return c.writeLocked(obj, api.Added)
}

// Update replaces an object. It is the spec-changing path: it bumps the
// generation, which is how a controller tells a real change of intent from its
// own status write coming back around.
//
// A non-zero ResourceVersion on the incoming object is a precondition: the
// write is refused with a Conflict if anything changed since it was read. Zero
// means "I have not read it, overwrite" and is available on purpose — the
// daemon's own reconcilers use it — but a client that reads first should send
// what it read.
func (c *Collection[T, PT]) Update(obj PT) (PT, error) {
	return c.update(obj, true)
}

// UpdateStatus replaces an object without bumping its generation, so a status
// write is distinguishable from a change of intent. It is the write behind the
// /status subresource.
func (c *Collection[T, PT]) UpdateStatus(obj PT) (PT, error) {
	return c.update(obj, false)
}

func (c *Collection[T, PT]) update(in PT, bumpGeneration bool) (PT, error) {
	obj, err := c.clone(in)
	if err != nil {
		return nil, err
	}

	c.s.mu.Lock()
	defer c.s.mu.Unlock()

	meta := obj.ObjectMeta()
	cur, ok := c.s.objects[c.kind][meta.Name]
	if !ok {
		return nil, api.NewNotFound(c.kind, meta.Name)
	}
	if meta.ResourceVersion != 0 && meta.ResourceVersion != cur.meta.ResourceVersion {
		return nil, api.NewConflict(c.kind, meta.Name, cur.meta.ResourceVersion, meta.ResourceVersion)
	}

	// Identity is the store's, not the client's: a caller must not be able to
	// re-date an object or adopt another one's uid by sending different values.
	meta.UID = cur.meta.UID
	meta.CreationTimestamp = cur.meta.CreationTimestamp
	meta.Generation = cur.meta.Generation
	if bumpGeneration {
		meta.Generation++
	}
	meta.ResourceVersion = c.s.next()

	return c.writeLocked(obj, api.Modified)
}

// writeLocked encodes, persists and announces an object. Callers hold the lock.
func (c *Collection[T, PT]) writeLocked(obj PT, evt api.EventType) (PT, error) {
	meta := obj.ObjectMeta()
	raw, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		return nil, api.NewInternalError("encode %s %q: %v", c.kind, meta.Name, err)
	}
	if err := c.s.persist(c.kind, meta.Name, raw); err != nil {
		return nil, api.NewInternalError("persist %s %q: %v", c.kind, meta.Name, err)
	}
	c.s.put(c.kind, *meta, raw)
	c.s.hub.broadcast(Event{
		Type:            evt,
		Kind:            c.kind,
		Name:            meta.Name,
		ResourceVersion: meta.ResourceVersion,
		Object:          raw,
	})
	return c.decode(raw)
}

// Delete removes an object. The final state is announced with the DELETED
// event, so a watcher learns what went away and not merely that something did.
func (c *Collection[T, PT]) Delete(name string) error {
	c.s.mu.Lock()
	defer c.s.mu.Unlock()

	rec, ok := c.s.objects[c.kind][name]
	if !ok {
		return api.NewNotFound(c.kind, name)
	}
	if err := os.Remove(c.s.path(c.kind, name)); err != nil && !os.IsNotExist(err) {
		return api.NewInternalError("delete %s %q: %v", c.kind, name, err)
	}
	delete(c.s.objects[c.kind], name)

	rv := c.s.next()
	if err := c.s.persistCounter(); err != nil {
		return api.NewInternalError("persist counter: %v", err)
	}
	c.s.hub.broadcast(Event{
		Type:            api.Deleted,
		Kind:            c.kind,
		Name:            name,
		ResourceVersion: rv,
		Object:          rec.raw,
	})
	return nil
}
