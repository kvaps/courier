// Package claude delivers courier messages into local Claude Code background
// sessions through Claude Code's own hooks.
//
// Nothing here types into a terminal or speaks an undocumented socket. A
// message is put in the session's mailbox, and the session collects it: every
// session on this machine runs an asyncRewake hook (attention_hook.py in the
// orchestrator's repository) that long-polls courier for its mailbox and,
// when something is there, exits with code 2 — which Claude Code documents as
// waking the session and handing it the hook's output, even when it is idle.
//
// A session with no hook waiting — stopped, or idle past the hook's timeout —
// is restarted with `claude respawn`, the CLI's own command for it. The restart
// keeps the session's id and history, and its SessionStart hook collects the
// mailbox. A busy session is left alone: the hook started with its current
// turn collects the message, or the one that starts when the turn ends does.
package claude

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/kvaps/courier/pkg/agent"
	"github.com/kvaps/courier/pkg/api"
)

// Kind is the registered sink name.
const Kind = "claude"

var (
	// pickupWait is how long a delivery waits for a hook already waiting in
	// the session before deciding the session has to be woken.
	pickupWait = 20 * time.Second
	// wakeWait bounds how long a respawned session gets to start and collect.
	wakeWait = 90 * time.Second
)

func init() {
	agent.Register(Kind, func() (agent.Sink, error) { return New(defaultMailboxPath()), nil })
}

// Sink delivers into Claude Code sessions by mailbox.
type Sink struct {
	path string

	mu      sync.Mutex
	changed *sync.Cond
	boxes   map[string][]agent.Letter
	seq     int64

	// claude runs the CLI; replaced in tests.
	claude func(ctx context.Context, args ...string) ([]byte, error)
}

// New builds a sink whose mailboxes persist at path ("" keeps them in memory).
func New(path string) *Sink {
	s := &Sink{path: path, boxes: map[string][]agent.Letter{}, claude: runClaude}
	s.changed = sync.NewCond(&s.mu)
	s.load()
	return s
}

// Kind returns the registered sink name.
func (s *Sink) Kind() string { return Kind }

// Resolve reports whether an address names a session Claude Code knows.
func (s *Sink) Resolve(ctx context.Context, target string) (agent.Target, error) {
	ref := strings.TrimSpace(target)
	if ref == "" {
		return agent.Target{}, api.NewInvalid("empty agent address")
	}
	sess, ok, err := s.find(ctx, ref)
	if err != nil {
		return agent.Target{}, err
	}
	if !ok {
		return agent.Target{}, api.NewNotFound("agent session", ref)
	}
	return agent.Target{Address: sess.ID, Name: sess.Name, Live: sess.Status != "", Wakeable: true}, nil
}

// Deliver puts a message in the session's mailbox and makes sure someone will
// collect it.
func (s *Sink) Deliver(ctx context.Context, target string, msg agent.Message) (agent.Receipt, error) {
	text := strings.TrimSpace(msg.Text)
	if text == "" {
		return agent.Receipt{}, api.NewInvalid("empty message")
	}
	sess, ok, err := s.find(ctx, strings.TrimSpace(target))
	if err != nil {
		return agent.Receipt{}, err
	}
	if !ok {
		return agent.Receipt{}, api.NewNotFound("agent session", target)
	}
	id := s.put(sess.ID, agent.Letter{Conversation: msg.Conversation, Text: text, At: time.Now().UTC()})

	if s.waitCollected(ctx, sess.ID, id, pickupWait) {
		return agent.Receipt{Address: sess.ID, How: "collected by the session's hook"}, nil
	}
	cur, _, err := s.find(ctx, sess.ID)
	if err == nil && cur.Status == "busy" {
		// Its hook will collect it — the one waiting through this turn, or the
		// one the end of the turn starts. Waking it would cut the turn short.
		return agent.Receipt{Address: sess.ID, How: "queued; the session is busy and collects it when it can", Queued: true}, nil
	}
	if out, rerr := s.claude(ctx, "respawn", sess.ID); rerr != nil {
		s.drop(sess.ID, id)
		return agent.Receipt{}, api.NewBackendError("claude respawn %s: %v: %s", sess.ID, rerr, strings.TrimSpace(string(out)))
	}
	if s.waitCollected(ctx, sess.ID, id, wakeWait) {
		return agent.Receipt{Address: sess.ID, How: "woken with claude respawn; collected by its hook", Woke: true}, nil
	}
	s.drop(sess.ID, id)
	return agent.Receipt{}, api.NewBackendError(
		"session %s was restarted but did not collect the message within %s — is attention_hook.py installed as its SessionStart/Stop hook?",
		sess.ID, wakeWait)
}

// Collect hands a session everything in its mailbox, waiting up to wait for
// something to arrive. It is what a session's hook long-polls.
func (s *Sink) Collect(ctx context.Context, session string, wait time.Duration) []agent.Letter {
	short := shortID(session)
	deadline := time.Now().Add(wait)
	stop := context.AfterFunc(ctx, s.broadcast)
	defer stop()
	timer := time.AfterFunc(wait, s.broadcast)
	defer timer.Stop()

	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		if box := s.boxes[short]; len(box) > 0 {
			delete(s.boxes, short)
			s.saveLocked()
			s.changed.Broadcast()
			return box
		}
		if ctx.Err() != nil || !time.Now().Before(deadline) {
			return nil
		}
		s.changed.Wait()
	}
}

func (s *Sink) broadcast() {
	s.mu.Lock()
	s.changed.Broadcast()
	s.mu.Unlock()
}

func (s *Sink) put(short string, l agent.Letter) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	l.ID = s.seq
	s.boxes[short] = append(s.boxes[short], l)
	s.saveLocked()
	s.changed.Broadcast()
	return l.ID
}

func (s *Sink) drop(short string, id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	box := s.boxes[short][:0]
	for _, l := range s.boxes[short] {
		if l.ID != id {
			box = append(box, l)
		}
	}
	if len(box) == 0 {
		delete(s.boxes, short)
	} else {
		s.boxes[short] = box
	}
	s.saveLocked()
}

// waitCollected waits for one letter to leave the mailbox.
func (s *Sink) waitCollected(ctx context.Context, short string, id int64, wait time.Duration) bool {
	deadline := time.Now().Add(wait)
	stop := context.AfterFunc(ctx, s.broadcast)
	defer stop()
	timer := time.AfterFunc(wait, s.broadcast)
	defer timer.Stop()

	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		waiting := false
		for _, l := range s.boxes[short] {
			if l.ID == id {
				waiting = true
				break
			}
		}
		if !waiting {
			return true
		}
		if ctx.Err() != nil || !time.Now().Before(deadline) {
			return false
		}
		s.changed.Wait()
	}
}

type mailboxFile struct {
	Seq   int64                     `json:"seq"`
	Boxes map[string][]agent.Letter `json:"boxes"`
}

func (s *Sink) load() {
	if s.path == "" {
		return
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var f mailboxFile
	if json.Unmarshal(raw, &f) == nil && f.Boxes != nil {
		s.boxes, s.seq = f.Boxes, f.Seq
	}
}

// saveLocked persists the mailboxes, so a message queued for a busy session
// survives a courier restart. Called with mu held.
func (s *Sink) saveLocked() {
	if s.path == "" {
		return
	}
	raw, err := json.Marshal(mailboxFile{Seq: s.seq, Boxes: s.boxes})
	if err != nil {
		return
	}
	if os.MkdirAll(filepath.Dir(s.path), 0o700) != nil {
		return
	}
	tmp := s.path + ".tmp"
	if os.WriteFile(tmp, raw, 0o600) == nil {
		_ = os.Rename(tmp, s.path)
	}
}

// session is one row of `claude agents --json --all`.
type session struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionId"`
	Name      string `json:"name"`
	Status    string `json:"status"` // idle, busy; empty when not running
	State     string `json:"state"`
}

// find looks a session up by short id, session id or name in the CLI's own
// list of background sessions.
func (s *Sink) find(ctx context.Context, ref string) (session, bool, error) {
	out, err := s.claude(ctx, "agents", "--json", "--all")
	if err != nil {
		return session{}, false, api.NewBackendError("claude agents --json: %v", err)
	}
	var list []session
	if err := json.Unmarshal(out, &list); err != nil {
		return session{}, false, api.NewBackendError("claude agents --json: %v", err)
	}
	short := shortID(ref)
	for _, sess := range list {
		if sess.ID == short || sess.SessionID == ref || sess.Name == ref {
			return sess, true, nil
		}
	}
	return session{}, false, nil
}

func shortID(ref string) string {
	ref = strings.TrimSpace(ref)
	if len(ref) > 8 && strings.Count(ref, "-") >= 4 {
		return ref[:8]
	}
	return ref
}

func defaultMailboxPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".courier", "mailbox.json")
}

// claudeBinary finds the CLI. A LaunchAgent's PATH rarely includes
// ~/.local/bin, where the native installer puts it, so that is tried too.
func claudeBinary() string {
	if p := os.Getenv("COURIER_CLAUDE"); p != "" {
		return p
	}
	if p, err := exec.LookPath("claude"); err == nil {
		return p
	}
	if home, err := os.UserHomeDir(); err == nil {
		if p := filepath.Join(home, ".local", "bin", "claude"); fileExists(p) {
			return p
		}
	}
	return "claude"
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func runClaude(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, claudeBinary(), args...) //nolint:gosec // fixed binary, fixed verbs
	return cmd.Output()
}
