package courier

import (
	"context"
	"testing"
	"time"

	"github.com/kvaps/courier/pkg/api"
)

// withReviewer makes "orch" the channel's reviewer, with its own agent, and
// opens an agent conversation "a".
func withReviewer(t *testing.T, timeout int) (*Service, *fakeBackend, *fakeSink) {
	t.Helper()
	svc, be, sink := newService(t)
	ch, err := svc.channels.Get("chan")
	if err != nil {
		t.Fatal(err)
	}
	ch.Spec.Reviewer = "orch"
	ch.Spec.ReviewTimeoutSeconds = timeout
	if _, err := svc.channels.Update(ch); err != nil {
		t.Fatal(err)
	}
	openConv(t, svc, "orch", &api.AgentRef{Sink: "fake", Address: "orch-session"})
	openConv(t, svc, "a", &api.AgentRef{Sink: "fake", Address: "agent-session"})
	return svc, be, sink
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// A question goes to the reviewer, not to the reader, and stays open: the
// asking agent waits on it exactly as on a published one.
func TestAQuestionIsHeldForTheReviewer(t *testing.T) {
	svc, be, sink := withReviewer(t, 600)
	q := ask(t, svc, "a", "ship it?")

	if q.Status.Phase != api.PhaseHeld || !q.Open() {
		t.Fatalf("question = %+v", q.Status)
	}
	if got := be.texts(); len(got) != 0 {
		t.Errorf("the reader saw a held question: %v", got)
	}
	eventually(t, "the reviewer to be told", func() bool {
		for _, d := range sink.got() {
			if contains(d, "ship it?") && contains(d, q.Metadata.Name+"/publish") && contains(d, q.Metadata.Name+"/answer") {
				return true
			}
		}
		return false
	})
	conv, _ := svc.GetConversation("a")
	if conv.Status.PendingQuestion != q.Metadata.Name {
		t.Errorf("a held question is not the pending one: %q", conv.Status.PendingQuestion)
	}
}

// The reviewer's own questions are not held: there is nobody above it.
func TestTheReviewersOwnQuestionIsNotHeld(t *testing.T) {
	svc, be, _ := withReviewer(t, 600)
	q := ask(t, svc, "orch", "what about blockstor?")
	if q.Status.Phase != api.PhaseSent || len(be.texts()) != 1 {
		t.Fatalf("question = %+v, sent = %v", q.Status, be.texts())
	}
}

// Publishing with a rewrite shows the reader the rewrite and keeps the agent's
// own words on the record.
func TestPublishingARewriteKeepsTheOriginal(t *testing.T) {
	svc, be, _ := withReviewer(t, 600)
	q := ask(t, svc, "a", "ship it?")

	got, err := svc.Publish(context.Background(), q.Metadata.Name,
		&api.Body{Context: "PR #1 — release", Question: "Release today or tomorrow?", Proposal: "today"}, "orchestrator")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != api.PhaseSent || got.Status.Review == nil || got.Status.Review.Original == nil ||
		got.Status.Review.Original.Question != "ship it?" || got.Status.Review.By != "orchestrator" {
		t.Fatalf("published = %+v", got.Status)
	}
	texts := be.texts()
	if len(texts) != 1 || !contains(texts[0], "Release today or tomorrow?") || contains(texts[0], "ship it?") {
		t.Errorf("the reader saw %v", texts)
	}
	// The reader's answer still lands on it.
	conv, _ := svc.GetConversation("a")
	if conv.Status.PendingQuestion != q.Metadata.Name {
		t.Errorf("pending = %q", conv.Status.PendingQuestion)
	}
}

// The reviewer can settle a question itself: the reader never sees it, and the
// agent is told whose answer it is.
func TestTheReviewerCanAnswerAHeldQuestion(t *testing.T) {
	svc, be, sink := withReviewer(t, 600)
	q := ask(t, svc, "a", "ship it?")

	got, err := svc.AnswerFor(context.Background(), q.Metadata.Name, "already decided: ship it, see decisions.md", "orchestrator")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != api.PhaseAnswered || got.Status.AnsweredBy != "orchestrator" || got.Status.Review.Action != "answered" {
		t.Fatalf("answered = %+v", got.Status)
	}
	if texts := be.texts(); len(texts) != 0 {
		t.Errorf("the reader saw it anyway: %v", texts)
	}
	eventually(t, "the agent to get the answer", func() bool {
		for _, d := range sink.got() {
			if contains(d, "already decided") && contains(d, "on the operator's behalf") {
				return true
			}
		}
		return false
	})
	conv, _ := svc.GetConversation("a")
	if conv.Status.PendingQuestion != "" {
		t.Errorf("still pending: %q", conv.Status.PendingQuestion)
	}
}

// A reviewer that does not act costs the reader a delay, never a question.
func TestAHeldQuestionIsPublishedWhenTheReviewerIsSilent(t *testing.T) {
	svc, be, _ := withReviewer(t, 1)
	q := ask(t, svc, "a", "ship it?")

	eventually(t, "the question to be published", func() bool { return len(be.texts()) == 1 })
	got, err := svc.GetMessage(q.Metadata.Name)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != api.PhaseSent || got.Status.Review == nil || got.Status.Review.Action != "timeout" {
		t.Fatalf("after timeout = %+v", got.Status)
	}
}

// Only a held question can be published.
func TestPublishingWhatIsNotHeldIsRefused(t *testing.T) {
	svc, _, _ := withReviewer(t, 600)
	q := ask(t, svc, "orch", "mine")
	if _, err := svc.Publish(context.Background(), q.Metadata.Name, nil, "x"); !api.IsConflict(err) {
		t.Fatalf("err = %v, want Conflict", err)
	}
}

// The board shows what waits on the reader and what the reviewer settled.
func TestTheBoardCountsWaitingSettledAndHeld(t *testing.T) {
	svc, _, _ := withReviewer(t, 600)
	openConv(t, svc, "b", &api.AgentRef{Sink: "fake", Address: "b-session"})
	openConv(t, svc, "c", &api.AgentRef{Sink: "fake", Address: "c-session"})

	settled := ask(t, svc, "a", "settled?")
	if _, err := svc.AnswerFor(context.Background(), settled.Metadata.Name, "yes", "orchestrator"); err != nil {
		t.Fatal(err)
	}
	waiting := ask(t, svc, "b", "waiting?")
	if _, err := svc.Publish(context.Background(), waiting.Metadata.Name, nil, "orchestrator"); err != nil {
		t.Fatal(err)
	}
	ask(t, svc, "c", "held?")

	board := svc.board("chan", time.Now())
	if len(board.Waiting) != 1 || board.Waiting[0].Summary != "waiting?" {
		t.Errorf("waiting = %+v", board.Waiting)
	}
	if len(board.Settled) != 1 || board.Settled[0].Answer != "yes" {
		t.Errorf("settled = %+v", board.Settled)
	}
	if board.Held != 1 {
		t.Errorf("held = %d", board.Held)
	}
}
