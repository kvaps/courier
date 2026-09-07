package api

import (
	"strings"
	"testing"
)

// The running order is the part senders get wrong, so it is the part pinned
// down: re-orientation, then the choice, then the recommendation, then the
// invitation to answer.
func TestRenderOrder(t *testing.T) {
	b := Body{
		Context:  "PR #17 (ComputePlane), rewording the overview",
		Question: "mention security at all?",
		Proposal: "I'd keep one honest sentence",
		Progress: &Progress{Index: 2, Total: 4},
	}
	got := b.Render("OK?", true)
	want := []string{"2/4 — PR #17", "mention security", "→ I'd keep", "OK?"}
	at := -1
	for _, w := range want {
		i := strings.Index(got, w)
		if i < 0 {
			t.Fatalf("missing %q in:\n%s", w, got)
		}
		if i < at {
			t.Errorf("%q came out of order in:\n%s", w, got)
		}
		at = i
	}
}

func TestRenderOmitsWhatIsNotThere(t *testing.T) {
	got := Body{Text: "done, PR is open"}.Render("OK?", false)
	if got != "done, PR is open" {
		t.Errorf("a plain note should render as itself, got %q", got)
	}
	// No reply prompt on something that is not a question: it would invite an
	// answer to a message that is not asking for one.
	if strings.Contains(got, "OK?") {
		t.Errorf("reply prompt leaked onto a non-question: %q", got)
	}
}

func TestRenderReplyPromptIsTheChannelsToChoose(t *testing.T) {
	b := Body{Question: "ship it?"}
	if got := b.Render("Ready?", true); !strings.HasSuffix(got, "Ready?") {
		t.Errorf("got %q", got)
	}
	// An empty prompt is a channel saying "don't add one", not a request for
	// the default.
	if got := b.Render("", true); strings.Contains(got, "?\n\n") {
		t.Errorf("an empty prompt should add nothing, got %q", got)
	}
}

// A wrong counter is worse than none: it tells the reader the run is nearly
// over when it is not.
func TestProgressPrefixRefusesNonsense(t *testing.T) {
	for _, p := range []*Progress{nil, {0, 0}, {3, 0}, {0, 5}, {6, 5}, {-1, 5}} {
		if got := p.Prefix(); got != "" {
			t.Errorf("Prefix(%+v) = %q, want empty", p, got)
		}
	}
	if got := (&Progress{3, 12}).Prefix(); got != "3/12" {
		t.Errorf("Prefix = %q", got)
	}
}

// An empty message wastes a notification and, on a question, leaves the reader
// nothing to answer.
func TestBodyEmpty(t *testing.T) {
	if !(Body{}).Empty() {
		t.Error("a zero body is empty")
	}
	if !(Body{Context: "  \n "}).Empty() {
		t.Error("whitespace is empty")
	}
	// Progress alone is not content.
	if !(Body{Progress: &Progress{1, 2}}).Empty() {
		t.Error("progress alone is not a message")
	}
	if (Body{Proposal: "yes"}).Empty() {
		t.Error("a proposal is content")
	}
}

func TestBodySummaryPrefersTheQuestion(t *testing.T) {
	b := Body{Context: "some context", Question: "ship it?", Text: "a wall of text"}
	if got := b.Summary(); got != "ship it?" {
		t.Errorf("summary = %q", got)
	}
	long := Body{Text: strings.Repeat("word ", 100)}
	if got := []rune(long.Summary()); len(got) > 120 {
		t.Errorf("summary is %d runes", len(got))
	}
}

func TestMessageOpen(t *testing.T) {
	q := func(dir Direction, await bool, phase Phase) *Message {
		return &Message{
			Spec:   MessageSpec{Direction: dir, AwaitReply: await},
			Status: MessageStatus{Phase: phase},
		}
	}
	if !q(Outbound, true, PhaseSent).Open() {
		t.Error("a sent question is open")
	}
	if !q(Outbound, true, PhasePending).Open() {
		t.Error("a question not yet on the wire is still open")
	}
	// Everything that has been settled is closed, so the next thing the human
	// writes cannot land on a dead question.
	for _, p := range []Phase{PhaseAnswered, PhaseCancelled, PhaseFailed} {
		if q(Outbound, true, p).Open() {
			t.Errorf("phase %s should not be open", p)
		}
	}
	if q(Outbound, false, PhaseSent).Open() {
		t.Error("a message that asked for nothing is not open")
	}
	if q(Inbound, true, PhaseSent).Open() {
		t.Error("an inbound message is never an open question")
	}
}
