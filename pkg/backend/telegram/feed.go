package telegram

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kvaps/courier/pkg/api"
	"github.com/kvaps/courier/pkg/backend"
)

// The feed is how a conversation looks on Telegram: a channel where every
// question is a post of its own, and the channel's discussion group where the
// replies to each post live as that post's comments.
//
// A conversation is the agent's address, not a place on screen. Its thread
// reference is opaque to Telegram — nothing is created when a conversation
// opens — and what the reader sees is one post per problem. That is the change
// from a topic per agent: the reader no longer picks a recipient, finds the
// agent, or tidies topics, because a reply goes to whoever owns the post it
// was written under.
//
// Telegram links a post to its comments only through the discussion group's
// automatic copy of the post, which arrives later on the update stream. So the
// backend keeps three small maps, persisted in its state directory:
//
//	post → thread       which conversation a post belongs to
//	copy → post         which post a comment thread hangs off
//	thread → last post  where a note that is not a question goes
//
// Message references are prefixed so an edit or a press can tell the two
// chats apart: "p<id>" is a channel post, "c<id>" a message in the group.

// noteWindow is how long a conversation's latest post keeps collecting its
// notes as comments. Past it, a note starts a post of its own: a status line
// about yesterday's question belongs to a new problem, not under an old one.
const noteWindow = 12 * time.Hour

// copyWait bounds how long a comment waits for its post's copy to appear in
// the discussion group. Telegram makes the copy within a second or two; the
// bound exists so a lost copy fails the send instead of hanging it.
const copyWait = 20 * time.Second

type feedPost struct {
	Thread string    `json:"thread"`
	Copy   int       `json:"copy,omitempty"`
	Header string    `json:"header,omitempty"`
	At     time.Time `json:"at"`
	// Created is when the post was made; Telegram lets a bot delete only
	// what is younger than 48 hours.
	Created time.Time `json:"created,omitempty"`
	// Kept marks a post the sweep tried and failed to delete, so it is not
	// retried every pass. It stays, rewritten as settled.
	Kept bool `json:"kept,omitempty"`
}

type feedState struct {
	// Board is the pinned summary post, if one has been made.
	Board  int               `json:"board,omitempty"`
	Posts  map[int]*feedPost `json:"posts"`
	Copies map[int]int       `json:"copies"`
	Last   map[string]int    `json:"last"`
}

type feed struct {
	path string

	mu     sync.Mutex
	copied *sync.Cond
	st     feedState
}

func newFeed(stateDir string) *feed {
	f := &feed{st: feedState{Posts: map[int]*feedPost{}, Copies: map[int]int{}, Last: map[string]int{}}}
	f.copied = sync.NewCond(&f.mu)
	if stateDir != "" {
		f.path = filepath.Join(stateDir, "feed.json")
		if raw, err := os.ReadFile(f.path); err == nil { //nolint:gosec // daemon's own state directory
			var st feedState
			if json.Unmarshal(raw, &st) == nil {
				if st.Posts != nil {
					f.st.Posts = st.Posts
				}
				if st.Copies != nil {
					f.st.Copies = st.Copies
				}
				if st.Last != nil {
					f.st.Last = st.Last
				}
				f.st.Board = st.Board
			}
		}
	}
	return f
}

// save writes the maps out. Called with mu held.
func (f *feed) save() {
	if f.path == "" {
		return
	}
	raw, err := json.Marshal(f.st)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(f.path), 0o700); err != nil {
		return
	}
	tmp := f.path + ".tmp"
	if os.WriteFile(tmp, raw, 0o600) == nil {
		_ = os.Rename(tmp, f.path)
	}
}

func (f *feed) addPost(id int, thread, header string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now().UTC()
	post := &feedPost{Thread: thread, Header: header, At: now, Created: now}
	// The group's copy can arrive before the post is recorded.
	for copyID, of := range f.st.Copies {
		if of == id {
			post.Copy = copyID
		}
	}
	f.st.Posts[id] = post
	f.st.Last[thread] = id
	f.save()
	f.copied.Broadcast()
}

// known reports whether a post is one courier recorded.
func (f *feed) known(post int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.st.Posts[post]
	return ok
}

// waitPost waits a little for a post to be recorded. A post courier is
// sending right now may have its copy arrive before Send returns and records
// it; without the wait, that copy would look like a post someone else wrote.
func (f *feed) waitPost(ctx context.Context, post int, d time.Duration) bool {
	deadline := time.Now().Add(d)
	timer := time.AfterFunc(d, func() {
		f.mu.Lock()
		f.copied.Broadcast()
		f.mu.Unlock()
	})
	defer timer.Stop()
	f.mu.Lock()
	defer f.mu.Unlock()
	for {
		if _, ok := f.st.Posts[post]; ok {
			return true
		}
		if ctx.Err() != nil || time.Now().After(deadline) {
			return false
		}
		f.copied.Wait()
	}
}

// touch makes a post the conversation's current one, so what the agent says
// next lands under the post the reader just wrote in rather than its newest.
func (f *feed) touch(post int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.st.Posts[post]
	if !ok {
		return
	}
	p.At = time.Now().UTC()
	f.st.Last[p.Thread] = post
	f.save()
}

// feedKey is what a conversation's posts are filed under: its name when the
// daemon supplies one, so a post the reader started and a post the agent
// started belong to the same conversation regardless of how its thread was
// first opened.
func feedKey(ref backend.ThreadRef, conversation string) string {
	if conversation != "" {
		return "conv:" + conversation
	}
	return string(ref)
}

// sawCopy records the discussion group's copy of a post and wakes whoever is
// waiting to comment under it.
func (f *feed) sawCopy(post, copyID int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.st.Copies[copyID] = post
	if p, ok := f.st.Posts[post]; ok {
		p.Copy = copyID
	}
	f.save()
	f.copied.Broadcast()
}

// copyOf waits for a post's copy to appear and returns its id in the group.
func (f *feed) copyOf(ctx context.Context, post int) (int, bool) {
	deadline := time.Now().Add(copyWait)
	stop := context.AfterFunc(ctx, func() {
		f.mu.Lock()
		f.copied.Broadcast()
		f.mu.Unlock()
	})
	defer stop()
	timer := time.AfterFunc(copyWait, func() {
		f.mu.Lock()
		f.copied.Broadcast()
		f.mu.Unlock()
	})
	defer timer.Stop()

	f.mu.Lock()
	defer f.mu.Unlock()
	for {
		if p, ok := f.st.Posts[post]; ok && p.Copy > 0 {
			return p.Copy, true
		}
		if ctx.Err() != nil || time.Now().After(deadline) {
			return 0, false
		}
		f.copied.Wait()
	}
}

// threadOfCopy resolves a comment thread to the conversation that owns it.
func (f *feed) threadOfCopy(copyID int) (thread string, post int, ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	post, ok = f.st.Copies[copyID]
	if !ok {
		return "", 0, false
	}
	p, ok := f.st.Posts[post]
	if !ok {
		return "", 0, false
	}
	return p.Thread, post, true
}

func (f *feed) threadOfPost(post int) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.st.Posts[post]
	if !ok {
		return "", false
	}
	return p.Thread, true
}

func (f *feed) header(post int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p, ok := f.st.Posts[post]; ok {
		return p.Header
	}
	return ""
}

// recentPost is the conversation's latest post, if it is recent enough to take
// a note as a comment.
func (f *feed) recentPost(thread string) (int, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.st.Last[thread]
	if !ok {
		return 0, false
	}
	p, ok := f.st.Posts[id]
	if !ok || time.Since(p.At) > noteWindow {
		return 0, false
	}
	return id, true
}

func postRef(thread backend.ThreadRef, id int) backend.MessageRef {
	return backend.MessageRef{Thread: thread, ID: "p" + strconv.Itoa(id)}
}

func commentRef(thread backend.ThreadRef, id int) backend.MessageRef {
	return backend.MessageRef{Thread: thread, ID: "c" + strconv.Itoa(id)}
}

// parseRef splits a message reference into which chat it lives in and its id.
func parseRef(id string) (post bool, n int, err error) {
	if len(id) < 2 || (id[0] != 'p' && id[0] != 'c') {
		return false, 0, api.NewInvalid("telegram message ref %q is not a feed reference", id)
	}
	n, err = strconv.Atoi(id[1:])
	if err != nil {
		return false, 0, api.NewInvalid("telegram message ref %q is not a feed reference", id)
	}
	return id[0] == 'p', n, nil
}

// postHeader is the first line of a post: whose problem this is. The title is
// the conversation's, so it carries the agent's zone mark and name.
func postHeader(title string) string {
	title = strings.TrimSpace(title)
	if title == "" {
		return ""
	}
	return title
}

func withHeader(header, text string) string {
	if header == "" {
		return text
	}
	return header + "\n\n" + text
}

func (f *feed) boardPost() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.st.Board
}

// setBoard records the summary post. It belongs to the given thread so that
// comments under it reach someone, but it never becomes that thread's latest
// post: an agent's next note must not land under the summary.
func (f *feed) setBoard(id int, thread string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.st.Board = id
	if thread != "" {
		f.st.Posts[id] = &feedPost{Thread: thread, At: time.Now().UTC()}
	}
	f.save()
	f.copied.Broadcast()
}

// stale lists posts idle for longer than d, excluding the board and kept ones.
func (f *feed) stale(d time.Duration) []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []int
	for id, p := range f.st.Posts {
		if id == f.st.Board || p.Kept || time.Since(p.At) < d {
			continue
		}
		out = append(out, id)
	}
	return out
}

// forget drops a deleted post from every map.
func (f *feed) forget(id int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p, ok := f.st.Posts[id]; ok {
		delete(f.st.Copies, p.Copy)
		if f.st.Last[p.Thread] == id {
			delete(f.st.Last, p.Thread)
		}
	}
	delete(f.st.Posts, id)
	f.save()
}

func (f *feed) keep(id int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p, ok := f.st.Posts[id]; ok {
		p.Kept = true
	}
	f.save()
}
