package store

import (
	"encoding/json"
	"sync"

	"github.com/kvaps/courier/pkg/api"
)

// historyKept bounds the ring of past events retained for watch resume. It is
// what makes "watch from resourceVersion N" answerable at all: a client that
// went away and came back within this many changes continues exactly where it
// stopped, and one that fell further behind is told to re-list rather than
// silently handed a stream with a hole in it.
const historyKept = 2048

// subBuffer is how far one watcher may fall behind before it is disconnected.
// A slow watcher must not be allowed to block every writer in the daemon, and
// a disconnected client that re-lists is correct where a stalled one is not.
const subBuffer = 256

// Event is one change to one object.
type Event struct {
	Type api.EventType
	Kind string
	Name string
	// ResourceVersion is the store version this change was assigned. A client
	// that has processed it should resume from exactly this number.
	ResourceVersion int64
	// Object is the object as stored, or as it last was for a deletion.
	Object json.RawMessage
}

// hub fans changes out to watchers and keeps enough history to resume.
type hub struct {
	mu      sync.Mutex
	subs    map[int]*sub
	nextID  int
	history []Event
}

type sub struct {
	id    int
	kinds map[string]bool
	ch    chan Event
	// closed guards against a double close when a slow subscriber is dropped
	// by the broadcaster at the same moment its owner cancels it.
	closed bool
}

func newHub() *hub { return &hub{subs: map[int]*sub{}} }

// Watch returns a channel of changes at or after sinceVersion.
//
// sinceVersion of 0 means "from now": only changes made after this call. A
// non-zero version replays retained history first, so a client that lists and
// then watches from the list's resourceVersion sees every change exactly once,
// with no gap and no duplicate. A version older than the retained history is
// refused with a Conflict — the honest answer, because the alternative is a
// stream that quietly skipped what the client missed.
//
// The returned function must be called to release the subscription.
func (s *Store) Watch(sinceVersion int64, kinds ...string) (<-chan Event, func(), error) {
	set := map[string]bool{}
	for _, k := range kinds {
		set[k] = true
	}

	// The store lock is held across subscribe so that no write can slip between
	// the history snapshot and the subscription being registered — that gap is
	// exactly how a watcher loses an event.
	s.mu.Lock()
	defer s.mu.Unlock()

	h := s.hub
	h.mu.Lock()
	defer h.mu.Unlock()

	var backlog []Event
	if sinceVersion > 0 {
		if len(h.history) > 0 && sinceVersion < h.history[0].ResourceVersion-1 {
			return nil, nil, api.NewConflict("Watch", "", s.rv, sinceVersion)
		}
		for _, e := range h.history {
			if e.ResourceVersion > sinceVersion && (len(set) == 0 || set[e.Kind]) {
				backlog = append(backlog, e)
			}
		}
	}
	if len(backlog) > subBuffer {
		return nil, nil, api.NewConflict("Watch", "", s.rv, sinceVersion)
	}

	h.nextID++
	sb := &sub{id: h.nextID, kinds: set, ch: make(chan Event, subBuffer)}
	for _, e := range backlog {
		sb.ch <- e
	}
	h.subs[sb.id] = sb

	cancel := func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if s, ok := h.subs[sb.id]; ok {
			delete(h.subs, sb.id)
			if !s.closed {
				s.closed = true
				close(s.ch)
			}
		}
	}
	return sb.ch, cancel, nil
}

// broadcast records an event in the history and delivers it to watchers.
// Callers must hold the store's write lock, which is what keeps history order
// identical to resource-version order.
func (h *hub) broadcast(e Event) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.history = append(h.history, e)
	if len(h.history) > historyKept {
		h.history = append([]Event(nil), h.history[len(h.history)-historyKept:]...)
	}

	for id, s := range h.subs {
		if len(s.kinds) > 0 && !s.kinds[e.Kind] {
			continue
		}
		select {
		case s.ch <- e:
		default:
			// Too far behind. Dropping the subscriber is deliberate: a client
			// that reconnects and re-lists is correct, whereas one fed a
			// stream with a silent gap in it is not.
			delete(h.subs, id)
			if !s.closed {
				s.closed = true
				close(s.ch)
			}
		}
	}
}
