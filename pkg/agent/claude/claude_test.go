package claude

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kvaps/courier/pkg/agent"
)

// fakeCLI stands in for the claude binary: it answers `agents --json` from a
// table and records respawns, optionally starting a "hook" that collects.
type fakeCLI struct {
	mu       sync.Mutex
	status   string
	respawns []string
	onWake   func()
}

func (f *fakeCLI) run(_ context.Context, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch args[0] {
	case "agents":
		return []byte(`[{"id":"abcd1234","sessionId":"abcd1234-0000-0000-0000-000000000000","name":"worker","status":"` + f.status + `"}]`), nil
	case "respawn":
		f.respawns = append(f.respawns, args[1])
		if f.onWake != nil {
			go f.onWake()
		}
		return []byte("restarted " + args[1]), nil
	}
	return nil, nil
}

func newTestSink(t *testing.T, cli *fakeCLI) *Sink {
	t.Helper()
	pickupWait, wakeWait = 200*time.Millisecond, 2*time.Second
	s := New(filepath.Join(t.TempDir(), "mailbox.json"))
	s.claude = cli.run
	return s
}

// A hook already waiting in the session collects the message.
func TestAWaitingHookCollects(t *testing.T) {
	cli := &fakeCLI{status: "idle"}
	s := newTestSink(t, cli)
	got := make(chan []agent.Letter, 1)
	go func() { got <- s.Collect(context.Background(), "abcd1234-0000-0000-0000-000000000000", 2*time.Second) }()
	time.Sleep(50 * time.Millisecond)

	r, err := s.Deliver(context.Background(), "abcd1234", agent.Message{Text: "use Postgres", Conversation: "db"})
	if err != nil {
		t.Fatal(err)
	}
	if r.Woke || r.Queued || !strings.Contains(r.How, "collected") {
		t.Errorf("receipt = %+v", r)
	}
	letters := <-got
	if len(letters) != 1 || letters[0].Text != "use Postgres" || letters[0].Conversation != "db" {
		t.Errorf("letters = %+v", letters)
	}
	if len(cli.respawns) != 0 {
		t.Errorf("a session with a waiting hook was restarted: %v", cli.respawns)
	}
}

// A busy session is not interrupted: the message waits for its hook.
func TestABusySessionIsNotWoken(t *testing.T) {
	cli := &fakeCLI{status: "busy"}
	s := newTestSink(t, cli)
	r, err := s.Deliver(context.Background(), "worker", agent.Message{Text: "later"})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Queued || len(cli.respawns) != 0 {
		t.Fatalf("receipt = %+v, respawns = %v", r, cli.respawns)
	}
	if letters := s.Collect(context.Background(), "abcd1234", 0); len(letters) != 1 {
		t.Errorf("the queued message is gone: %+v", letters)
	}
}

// An idle or stopped session with nobody waiting is restarted, and its
// SessionStart hook collects.
func TestAnIdleSessionIsRespawnedAndCollects(t *testing.T) {
	cli := &fakeCLI{status: ""}
	s := newTestSink(t, cli)
	cli.onWake = func() { s.Collect(context.Background(), "abcd1234", time.Second) }

	r, err := s.Deliver(context.Background(), "abcd1234", agent.Message{Text: "wake up"})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Woke || len(cli.respawns) != 1 || cli.respawns[0] != "abcd1234" {
		t.Fatalf("receipt = %+v, respawns = %v", r, cli.respawns)
	}
}

// A restarted session that never collects is a failed delivery, said plainly,
// and the message does not linger to arrive unexpectedly later.
func TestARespawnNobodyCollectsFails(t *testing.T) {
	cli := &fakeCLI{status: "idle"}
	s := newTestSink(t, cli)
	if _, err := s.Deliver(context.Background(), "abcd1234", agent.Message{Text: "hello?"}); err == nil {
		t.Fatal("an uncollected message was reported delivered")
	}
	if letters := s.Collect(context.Background(), "abcd1234", 0); len(letters) != 0 {
		t.Errorf("a failed message stayed queued: %+v", letters)
	}
}

// The mailbox survives a courier restart.
func TestTheMailboxSurvivesARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mailbox.json")
	a := New(path)
	a.put("abcd1234", agent.Letter{Text: "kept"})
	b := New(path)
	if letters := b.Collect(context.Background(), "abcd1234", 0); len(letters) != 1 || letters[0].Text != "kept" {
		t.Errorf("after restart = %+v", letters)
	}
}

func TestAnUnknownSessionIsNotFound(t *testing.T) {
	s := newTestSink(t, &fakeCLI{status: "idle"})
	if _, err := s.Resolve(context.Background(), "nobody"); err == nil {
		t.Fatal("an unknown session resolved")
	}
}
