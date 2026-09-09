package courier

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kvaps/courier/pkg/api"
	"github.com/kvaps/courier/pkg/backend"
)

// waitFor polls until a condition holds. Delivery to an agent happens on its own
// goroutine, so a test that asserted immediately would be racing the daemon.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("timed out waiting for the daemon to catch up")
}

// sendRefuse is the pair the drafted-answer flow was built for: one button that
// sends the answer somebody wrote, one that refuses in words the agent can act
// on. Neither is an acknowledgement — both are answers.
func sendRefuse() []api.Choice {
	return []api.Choice{
		{ID: "send", Label: "Send", Answer: "Keep one honest sentence about isolation."},
		{ID: "refuse", Label: "Refuse", Answer: "Drop the security line; say only what it does."},
	}
}

func draft(t *testing.T, svc *Service, question string, choices []api.Choice) *api.Message {
	t.Helper()
	m, err := svc.Draft(context.Background(), question, DraftRequest{
		DraftedBy: "the orchestrator",
		Choices:   choices,
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// press taps a button, as the transport would report it.
func press(t *testing.T, be *fakeBackend, svc *Service, d *api.Message, choiceID string) backend.PressResult {
	t.Helper()
	var ref string
	for _, c := range d.Spec.Choices {
		if c.ID == choiceID {
			ref = c.Ref
		}
	}
	if ref == "" {
		t.Fatalf("no choice %q on draft %s", choiceID, d.Metadata.Name)
	}
	sink := be.waitSink(t)
	pr, ok := sink.(interface {
		Press(context.Context, string, backend.Press) (backend.PressResult, error)
	})
	if !ok {
		t.Fatal("the sink cannot take a press")
	}
	conv, err := svc.GetConversation(d.Spec.Conversation)
	if err != nil {
		t.Fatal(err)
	}
	res, err := pr.Press(context.Background(), "chan", backend.Press{
		Thread:   backend.ThreadRef(conv.Status.Ref),
		Message:  backend.MessageRef{Thread: backend.ThreadRef(conv.Status.Ref), ID: d.Status.Ref},
		Choice:   ref,
		Author:   "@tester",
		AuthorID: "1",
	})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// The buttons are drawn under the question, carrying handles rather than the
// answers themselves — Telegram allows a button 64 bytes, and the words have to
// survive somewhere the record can still be read from.
func TestDraftDrawsButtonsUnderTheQuestion(t *testing.T) {
	svc, be, _ := newService(t)
	openConv(t, svc, "a", nil)
	q := ask(t, svc, "a", "Mention security at all?")
	d := draft(t, svc, q.Metadata.Name, sendRefuse())

	if d.Spec.InReplyTo != q.Metadata.Name {
		t.Errorf("draft answers %q, want %q", d.Spec.InReplyTo, q.Metadata.Name)
	}
	out := be.sent[len(be.sent)-1]
	if len(out.Choices) != 2 {
		t.Fatalf("choices delivered = %d", len(out.Choices))
	}
	if out.ReplyTo.ID != q.Status.Ref {
		t.Errorf("draft quotes %q, want the question %q", out.ReplyTo.ID, q.Status.Ref)
	}
	for _, c := range out.Choices {
		if c.Ref == "" || len(c.Ref) > 64 {
			t.Errorf("choice %q has an unusable button handle %q", c.ID, c.Ref)
		}
		if strings.Contains(c.Ref, c.Answer) {
			t.Errorf("the answer travelled in the button handle for %q", c.ID)
		}
	}
	// The reader has to see the words before approving them, and be told they
	// are not their own.
	for _, want := range []string{"Draft by the orchestrator", "Keep one honest sentence", "Drop the security line", "write your own answer"} {
		if !strings.Contains(out.Text, want) {
			t.Errorf("the drafted answer does not show %q:\n%s", want, out.Text)
		}
	}
}

// The whole point: the agent gets the drafted words as its answer, not a bare
// confirmation, and is told whose words they are.
func TestPressAnswersTheQuestionWithTheDraft(t *testing.T) {
	svc, be, sink := newService(t)
	openConv(t, svc, "a", &api.AgentRef{Sink: "fake", Address: "s1"})
	q := ask(t, svc, "a", "Mention security at all?")
	d := draft(t, svc, q.Metadata.Name, sendRefuse())

	res := press(t, be, svc, d, "send")
	// A press that seems to do nothing gets pressed again, and editing the
	// message raises no notification — so the acknowledgement over the button is
	// the only thing that happens at the moment of the tap, and it insists.
	if !strings.Contains(res.Toast, "Send") || !strings.Contains(res.Toast, "sent to the agent") {
		t.Errorf("toast = %q — it does not say what happened", res.Toast)
	}
	if !res.Alert {
		t.Error("a taken decision was acknowledged with the toast that fades on its own")
	}

	// And the outcome leads the rewritten message, for the reader who comes back
	// to the thread later.
	be.mu.Lock()
	edit := be.edits[d.Status.Ref]
	be.mu.Unlock()
	if !strings.HasPrefix(edit, "✓ @tester chose \"Send\"") {
		t.Errorf("the outcome is buried in the rewritten draft:\n%s", edit)
	}

	got, err := svc.GetMessage(q.Metadata.Name)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != api.PhaseAnswered {
		t.Fatalf("phase = %s, want Answered", got.Status.Phase)
	}
	if got.Status.Answer != "Keep one honest sentence about isolation." {
		t.Errorf("answer = %q — the agent needs the grounds, not a bare yes", got.Status.Answer)
	}
	a := got.Status.Approval
	if a == nil {
		t.Fatal("the answer carries no approval — it would read as the operator's own words")
	}
	if a.DraftedBy != "the orchestrator" || a.Choice != "send" || a.Label != "Send" || a.Draft != d.Metadata.Name {
		t.Errorf("approval = %+v", a)
	}
	if got.Status.AnsweredBy != "@tester" {
		t.Errorf("answeredBy = %q", got.Status.AnsweredBy)
	}

	waitFor(t, func() bool { return len(sink.got()) > 0 })
	env := sink.got()[0]
	for _, want := range []string{"approved an answer", "not their own words", "the orchestrator drafted them", "Keep one honest sentence"} {
		if !strings.Contains(env, want) {
			t.Errorf("the envelope does not say %q:\n%s", want, env)
		}
	}

	c, err := svc.GetConversation("a")
	if err != nil {
		t.Fatal(err)
	}
	if c.Status.PendingQuestion != "" || c.Status.PendingQuestionSince != nil {
		t.Errorf("the question is still pending after being answered: %+v", c.Status)
	}
}

// Refusing is answering. It is what separates a button from cancel: cancel is
// the asker saying the question stopped mattering, refuse is the reader saying
// no — and the agent has to be able to act on the difference.
func TestRefuseIsAnAnswerAndNotAWithdrawal(t *testing.T) {
	svc, be, _ := newService(t)
	openConv(t, svc, "a", nil)
	q := ask(t, svc, "a", "Mention security at all?")
	d := draft(t, svc, q.Metadata.Name, sendRefuse())
	press(t, be, svc, d, "refuse")

	got, err := svc.GetMessage(q.Metadata.Name)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != api.PhaseAnswered {
		t.Fatalf("phase = %s, want Answered — a refusal is an answer", got.Status.Phase)
	}
	if !strings.Contains(got.Status.Answer, "Drop the security line") {
		t.Errorf("answer = %q", got.Status.Answer)
	}
}

// A button that answered nothing would let a question be closed without a
// decision behind it, and the queue would look shorter than it is.
func TestDraftRefusesAnOptionThatAnswersNothing(t *testing.T) {
	svc, _, _ := newService(t)
	openConv(t, svc, "a", nil)
	q := ask(t, svc, "a", "Mention security at all?")
	_, err := svc.Draft(context.Background(), q.Metadata.Name, DraftRequest{
		DraftedBy: "the orchestrator",
		Choices:   []api.Choice{{ID: "ok", Label: "OK", Answer: "  "}},
	})
	if err == nil || !strings.Contains(err.Error(), "cancel") {
		t.Fatalf("err = %v, want a refusal pointing at cancel", err)
	}
}

// The record has to say whose words were approved. A one-tap approval is cheap,
// and crediting the reader with authorship they never had is how a wrong answer
// becomes nobody's.
func TestDraftRequiresAnAuthor(t *testing.T) {
	svc, _, _ := newService(t)
	openConv(t, svc, "a", nil)
	q := ask(t, svc, "a", "Mention security at all?")
	_, err := svc.Draft(context.Background(), q.Metadata.Name, DraftRequest{Choices: sendRefuse()})
	if err == nil || !strings.Contains(err.Error(), "draftedBy") {
		t.Fatalf("err = %v, want draftedBy to be required", err)
	}
}

func TestDraftNeedsAnOpenQuestion(t *testing.T) {
	svc, _, _ := newService(t)
	openConv(t, svc, "a", nil)
	q := ask(t, svc, "a", "Mention security at all?")
	if _, err := svc.Cancel(context.Background(), q.Metadata.Name, "moot"); err != nil {
		t.Fatal(err)
	}
	_, err := svc.Draft(context.Background(), q.Metadata.Name, DraftRequest{
		DraftedBy: "the orchestrator", Choices: sendRefuse(),
	})
	if err == nil || !strings.Contains(err.Error(), "not an open question") {
		t.Fatalf("err = %v", err)
	}
}

// Two keyboards for one decision means the reader can answer the same question
// twice, and only one of the answers is the one that was meant.
func TestSecondDraftForOneQuestionIsRefused(t *testing.T) {
	svc, _, _ := newService(t)
	openConv(t, svc, "a", nil)
	q := ask(t, svc, "a", "Mention security at all?")
	draft(t, svc, q.Metadata.Name, sendRefuse())

	_, err := svc.Draft(context.Background(), q.Metadata.Name, DraftRequest{
		DraftedBy: "the orchestrator", Choices: sendRefuse(),
	})
	if !api.IsConflict(err) {
		t.Fatalf("err = %v, want Conflict", err)
	}
}

// Buttons are an offer laid over the ordinary way of answering, never a
// replacement for it: writing still works, and it takes the offer down.
func TestTypingAnAnswerRetiresTheDraft(t *testing.T) {
	svc, be, _ := newService(t)
	conv := openConv(t, svc, "a", nil)
	q := ask(t, svc, "a", "Mention security at all?")
	d := draft(t, svc, q.Metadata.Name, sendRefuse())

	be.say(t, backend.ThreadRef(conv.Status.Ref), "no, drop it entirely")

	got, err := svc.GetMessage(q.Metadata.Name)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status.Answer != "no, drop it entirely" || got.Status.Approval != nil {
		t.Errorf("a typed answer must be recorded as the reader's own: %+v", got.Status)
	}
	retired, err := svc.GetMessage(d.Metadata.Name)
	if err != nil {
		t.Fatal(err)
	}
	if retired.Status.Phase != api.PhaseCancelled {
		t.Errorf("the draft is still live after the question was answered by hand: %s", retired.Status.Phase)
	}
	be.mu.Lock()
	edit := be.edits[d.Status.Ref]
	be.mu.Unlock()
	if !strings.Contains(edit, "nothing was sent") {
		t.Errorf("the retired draft still reads as live:\n%s", edit)
	}
}

// A draft that outlives the question it was written for must not answer
// whatever question came next.
//
// The ordinary path retires it, so this reconstructs the state a daemon killed
// between recording an answer and taking the buttons down would come back to:
// the guard is checked at the moment of the tap, not trusted from when the
// buttons were drawn.
func TestPressAfterTheQuestionIsSettledSendsNothing(t *testing.T) {
	svc, be, sink := newService(t)
	conv := openConv(t, svc, "a", &api.AgentRef{Sink: "fake", Address: "s1"})
	q := ask(t, svc, "a", "Mention security at all?")
	d := draft(t, svc, q.Metadata.Name, sendRefuse())

	be.say(t, backend.ThreadRef(conv.Status.Ref), "no, drop it entirely")
	waitFor(t, func() bool { return len(sink.got()) > 0 })
	before := len(sink.got())

	revived, err := svc.GetMessage(d.Metadata.Name)
	if err != nil {
		t.Fatal(err)
	}
	revived.Status.Phase, revived.Status.Message = api.PhaseSent, ""
	if _, err := svc.messages.UpdateStatus(revived); err != nil {
		t.Fatal(err)
	}

	res := press(t, be, svc, revived, "send")
	if !strings.Contains(res.Toast, "no longer open") {
		t.Errorf("toast = %q", res.Toast)
	}
	// Long enough that a delivery this press should not have made would have
	// landed by now, rather than merely not having landed yet.
	time.Sleep(100 * time.Millisecond)
	got, err := svc.GetMessage(q.Metadata.Name)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status.Answer != "no, drop it entirely" {
		t.Errorf("a stale draft overwrote the answer: %q", got.Status.Answer)
	}
	if len(sink.got()) != before {
		t.Errorf("a stale press was delivered to the agent: %v", sink.got())
	}
}

func TestPressTwiceDecidesOnce(t *testing.T) {
	svc, be, _ := newService(t)
	openConv(t, svc, "a", nil)
	q := ask(t, svc, "a", "Mention security at all?")
	d := draft(t, svc, q.Metadata.Name, sendRefuse())

	press(t, be, svc, d, "send")
	again := press(t, be, svc, d, "refuse")
	if !strings.Contains(again.Toast, "Already decided") {
		t.Errorf("toast = %q", again.Toast)
	}
	got, err := svc.GetMessage(q.Metadata.Name)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Status.Answer, "Keep one honest sentence") {
		t.Errorf("the second press changed the answer: %q", got.Status.Answer)
	}
}

// Withdrawing a question takes its buttons down with it: a live keyboard under
// a question nobody is waiting on is an answer going nowhere.
func TestCancellingTheQuestionRetiresItsDraft(t *testing.T) {
	svc, _, _ := newService(t)
	openConv(t, svc, "a", nil)
	q := ask(t, svc, "a", "Mention security at all?")
	d := draft(t, svc, q.Metadata.Name, sendRefuse())

	if _, err := svc.Cancel(context.Background(), q.Metadata.Name, "answered elsewhere"); err != nil {
		t.Fatal(err)
	}
	got, err := svc.GetMessage(d.Metadata.Name)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != api.PhaseCancelled {
		t.Errorf("the draft outlived its question: %s", got.Status.Phase)
	}
}

// The drafter can take a draft back and offer a better one.
func TestCancellingADraftLeavesTheQuestionOpen(t *testing.T) {
	svc, _, _ := newService(t)
	openConv(t, svc, "a", nil)
	q := ask(t, svc, "a", "Mention security at all?")
	d := draft(t, svc, q.Metadata.Name, sendRefuse())

	if _, err := svc.Cancel(context.Background(), d.Metadata.Name, "better wording"); err != nil {
		t.Fatal(err)
	}
	got, err := svc.GetMessage(q.Metadata.Name)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Open() {
		t.Fatalf("withdrawing the draft closed the question: %s", got.Status.Phase)
	}
	if _, err := svc.Draft(context.Background(), q.Metadata.Name, DraftRequest{
		DraftedBy: "the orchestrator", Choices: sendRefuse(),
	}); err != nil {
		t.Fatalf("a replacement draft was refused: %v", err)
	}
}

// A drafted answer can outlive its conversation the same way a question can:
// deleting a thread withdraws the open question first, but that withdrawal is
// best-effort, and the delete goes ahead either way. Withdrawing must still
// settle the draft — there is simply no thread left to take the buttons out of.
func TestCancellingADraftWhoseConversationIsGone(t *testing.T) {
	svc, _, _ := newService(t)
	openConv(t, svc, "a", nil)
	q := ask(t, svc, "a", "Mention security at all?")
	d := draft(t, svc, q.Metadata.Name, sendRefuse())

	// The thread cleaned up under a live draft, as it would be if the
	// withdrawal that precedes a delete had failed.
	if err := svc.conversations.Delete("a"); err != nil {
		t.Fatal(err)
	}

	got, err := svc.Cancel(context.Background(), d.Metadata.Name, "no longer relevant")
	if err != nil {
		t.Fatalf("a draft with no thread left could not be withdrawn: %v", err)
	}
	if got.Status.Phase != api.PhaseCancelled {
		t.Errorf("phase = %s, want Cancelled", got.Status.Phase)
	}
	if !strings.Contains(got.Status.Message, "no longer exists") {
		t.Errorf("the record does not say why nothing was struck: %q", got.Status.Message)
	}
}

// A question with no answer stays open however long it takes. What the API owes
// the reader is the timestamp to measure that from.
func TestAQuestionRecordsHowLongItHasBeenWaiting(t *testing.T) {
	svc, _, _ := newService(t)
	openConv(t, svc, "a", nil)
	q := ask(t, svc, "a", "Mention security at all?")

	c, err := svc.GetConversation("a")
	if err != nil {
		t.Fatal(err)
	}
	if c.Status.PendingQuestion != q.Metadata.Name || c.Status.PendingQuestionSince == nil {
		t.Fatalf("conversation status = %+v", c.Status)
	}
	if !c.Status.PendingQuestionSince.Equal(*q.Status.SentAt) {
		t.Errorf("pendingQuestionSince = %v, want the question's sentAt %v",
			c.Status.PendingQuestionSince, q.Status.SentAt)
	}
}

// An ordinary question is untouched by any of this: no buttons, same text, same
// reply prompt as before.
func TestAQuestionWithoutADraftIsUnchanged(t *testing.T) {
	svc, be, _ := newService(t)
	openConv(t, svc, "a", nil)
	ask(t, svc, "a", "Mention security at all?")

	out := be.sent[len(be.sent)-1]
	if len(out.Choices) != 0 || !out.ReplyTo.Zero() {
		t.Errorf("an ordinary question grew buttons: %+v", out)
	}
	if !strings.HasSuffix(strings.TrimSpace(out.Text), api.DefaultReplyPrompt) {
		t.Errorf("question text = %q", out.Text)
	}
}

// The same defect the approval marker had: an edit raises no notification, so a
// note appended after the whole question is not something the reader notices on
// a screen they are already looking at.
func TestAWithdrawnQuestionSaysSoFirst(t *testing.T) {
	svc, be, _ := newService(t)
	openConv(t, svc, "a", nil)
	q := ask(t, svc, "a", "Mention security at all?")

	if _, err := svc.Cancel(context.Background(), q.Metadata.Name, "answered elsewhere"); err != nil {
		t.Fatal(err)
	}
	be.mu.Lock()
	edit := be.edits[q.Status.Ref]
	be.mu.Unlock()
	if !strings.HasPrefix(edit, "— answered elsewhere (no answer needed)") {
		t.Errorf("the withdrawal is buried:\n%s", edit)
	}
	if !strings.Contains(edit, "Mention security at all?") {
		t.Errorf("the question itself is gone:\n%s", edit)
	}
}
