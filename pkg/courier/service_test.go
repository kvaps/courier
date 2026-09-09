package courier

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kvaps/courier/pkg/agent"
	"github.com/kvaps/courier/pkg/api"
	"github.com/kvaps/courier/pkg/backend"
	"github.com/kvaps/courier/pkg/store"
)

// fakeBackend stands in for a transport: it records what was sent and lets a
// test put words in the human's mouth.
type fakeBackend struct {
	mu       sync.Mutex
	sink     backend.Sink
	sent     []backend.Outgoing
	edits    map[string]string
	threads  int
	noEdit   bool
	noTopics bool
}

func (f *fakeBackend) Kind() string { return "fake" }

func (f *fakeBackend) Connect(context.Context) (backend.Identity, error) {
	return backend.Identity{Self: "@fake", Target: "a test chat"}, nil
}

func (f *fakeBackend) Run(ctx context.Context, sink backend.Sink) error {
	f.mu.Lock()
	f.sink = sink
	f.mu.Unlock()
	<-ctx.Done()
	return nil
}

// waitSink blocks until the receive loop has started. The service starts it on
// its own goroutine, so a test that speaks too early would talk to nobody.
func (f *fakeBackend) waitSink(t *testing.T) backend.Sink {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		sink := f.sink
		f.mu.Unlock()
		if sink != nil {
			return sink
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the fake backend never started its receive loop")
	return nil
}

func (f *fakeBackend) OpenThread(context.Context, backend.ThreadSpec) (backend.ThreadRef, error) {
	if f.noTopics {
		return "", nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.threads++
	return backend.ThreadRef(itoa(f.threads)), nil
}

func (f *fakeBackend) CloseThread(context.Context, backend.ThreadRef) error { return nil }

func (f *fakeBackend) Send(_ context.Context, ref backend.ThreadRef, out backend.Outgoing) (backend.MessageRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, out)
	return backend.MessageRef{Thread: ref, ID: itoa(len(f.sent))}, nil
}

func (f *fakeBackend) Edit(_ context.Context, ref backend.MessageRef, text string) error {
	if f.noEdit {
		return api.NewNotSupported("this transport cannot edit")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.edits == nil {
		f.edits = map[string]string{}
	}
	f.edits[ref.ID] = text
	return nil
}

func (f *fakeBackend) React(context.Context, backend.MessageRef, backend.Mark) error { return nil }

func (f *fakeBackend) texts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.sent))
	for i, s := range f.sent {
		out[i] = s.Text
	}
	return out
}

// say puts a message in the human's mouth, as the transport would deliver it.
func (f *fakeBackend) say(t *testing.T, thread backend.ThreadRef, text string) {
	t.Helper()
	f.sayWith(t, thread, text, nil)
}

// sayWith is say plus attachments the transport has already downloaded.
func (f *fakeBackend) sayWith(t *testing.T, thread backend.ThreadRef, text string, files []api.Attachment) {
	t.Helper()
	sink := f.waitSink(t)
	sink.Receive(context.Background(), "chan", backend.Inbound{
		Files:  files,
		Thread: thread,
		Ref:    backend.MessageRef{Thread: thread, ID: "in"},
		Author: "@tester",
		Text:   text,
		At:     time.Now().UTC(),
	})
}

// fakeSink stands in for an agent.
type fakeSink struct {
	mu        sync.Mutex
	delivered []string
	fail      error
}

func (f *fakeSink) Kind() string { return "fake" }

func (f *fakeSink) Resolve(_ context.Context, target string) (agent.Target, error) {
	if target == "missing" {
		return agent.Target{}, api.NewNotFound("agent session", target)
	}
	return agent.Target{Address: target, Name: target, Live: true, Wakeable: true}, nil
}

func (f *fakeSink) Deliver(_ context.Context, target string, msg agent.Message) (agent.Receipt, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return agent.Receipt{}, f.fail
	}
	f.delivered = append(f.delivered, msg.Text)
	return agent.Receipt{Address: target, How: "test"}, nil
}

func (f *fakeSink) got() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.delivered...)
}

var (
	registerOnce sync.Once
	theBackend   = &fakeBackend{}
	theSink      = &fakeSink{}
)

func registerFakes() {
	registerOnce.Do(func() {
		backend.Register("fake", func(backend.Env, json.RawMessage) (backend.Backend, error) {
			return theBackend, nil
		})
		agent.Register("fake", func() (agent.Sink, error) { return theSink, nil })
	})
}

// newService builds a service with the fake transport connected. The fakes are
// package-level because a registry is process-wide; each test resets them.
func newService(t *testing.T) (*Service, *fakeBackend, *fakeSink) {
	t.Helper()
	registerFakes()

	theBackend.mu.Lock()
	theBackend.sent, theBackend.edits, theBackend.threads = nil, nil, 0
	theBackend.noEdit, theBackend.noTopics = false, false
	theBackend.sink = nil
	theBackend.mu.Unlock()
	theSink.mu.Lock()
	theSink.delivered, theSink.fail = nil, nil
	theSink.mu.Unlock()

	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc := New(st, t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(svc.Close)

	if _, err := svc.CreateChannel(context.Background(), &api.Channel{
		Metadata: api.ObjectMeta{Name: "chan"},
		Spec:     api.ChannelSpec{Backend: "fake"},
	}); err != nil {
		t.Fatal(err)
	}
	return svc, theBackend, theSink
}

func openConv(t *testing.T, svc *Service, name string, agentRef *api.AgentRef) *api.Conversation {
	t.Helper()
	c, err := svc.OpenConversation(context.Background(), &api.Conversation{
		Metadata: api.ObjectMeta{Name: name},
		Spec:     api.ConversationSpec{Channel: "chan", Title: "t-" + name, Agent: agentRef},
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func ask(t *testing.T, svc *Service, conv, question string) *api.Message {
	t.Helper()
	m, err := svc.Send(context.Background(), &api.Message{
		Spec: api.MessageSpec{
			Conversation: conv,
			AwaitReply:   true,
			Body:         api.Body{Question: question, Proposal: "yes"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestOpenConversationCreatesAThread(t *testing.T) {
	svc, _, _ := newService(t)
	c := openConv(t, svc, "a", nil)
	if c.Status.Phase != api.PhaseReady || c.Status.Ref == "" {
		t.Fatalf("conversation = %+v", c.Status)
	}
}

// A typo in an agent address should fail while the orchestrator is still
// holding the context, not silently produce a thread nobody is behind.
func TestOpenConversationRejectsAnUnreachableAgent(t *testing.T) {
	svc, _, _ := newService(t)
	_, err := svc.OpenConversation(context.Background(), &api.Conversation{
		Metadata: api.ObjectMeta{Name: "bad"},
		Spec: api.ConversationSpec{
			Channel: "chan", Title: "t",
			Agent: &api.AgentRef{Sink: "fake", Address: "missing"},
		},
	})
	if !api.IsNotFound(err) {
		t.Fatalf("err = %v, want NotFound", err)
	}
}

func TestSendRendersTheDecideShape(t *testing.T) {
	svc, be, _ := newService(t)
	openConv(t, svc, "a", nil)
	if _, err := svc.Send(context.Background(), &api.Message{
		Spec: api.MessageSpec{
			Conversation: "a",
			Body: api.Body{
				Context:  "PR #17",
				Question: "ship it?",
				Proposal: "yes, it is reversible",
				Progress: &api.Progress{Index: 2, Total: 3},
			},
			AwaitReply: true,
		},
	}); err != nil {
		t.Fatal(err)
	}
	got := be.texts()
	if len(got) != 1 {
		t.Fatalf("sent %d messages", len(got))
	}
	for _, want := range []string{"2/3 — PR #17", "ship it?", "→ yes, it is reversible", api.DefaultReplyPrompt} {
		if !contains(got[0], want) {
			t.Errorf("missing %q in:\n%s", want, got[0])
		}
	}
}

func TestSendRefusesAnEmptyBody(t *testing.T) {
	svc, _, _ := newService(t)
	openConv(t, svc, "a", nil)
	_, err := svc.Send(context.Background(), &api.Message{Spec: api.MessageSpec{Conversation: "a"}})
	if err == nil {
		t.Fatal("an empty message was accepted")
	}
}

// The human answers by writing in the thread, not by quoting, so two open
// questions would mean the daemon guessing which one a bare "OK" belongs to.
func TestOneQuestionAtATime(t *testing.T) {
	svc, _, _ := newService(t)
	openConv(t, svc, "a", nil)
	first := ask(t, svc, "a", "first?")

	_, err := svc.Send(context.Background(), &api.Message{
		Spec: api.MessageSpec{Conversation: "a", AwaitReply: true, Body: api.Body{Question: "second?"}},
	})
	if !api.IsConflict(err) {
		t.Fatalf("err = %v, want Conflict", err)
	}
	if !contains(err.Error(), first.Metadata.Name) {
		t.Errorf("the refusal should name the question in the way: %v", err)
	}

	// A message that asks for nothing is not blocked by an open question.
	if _, err := svc.Send(context.Background(), &api.Message{
		Spec: api.MessageSpec{Conversation: "a", Body: api.Body{Text: "just a note"}},
	}); err != nil {
		t.Errorf("a plain note was refused: %v", err)
	}
}

func TestInboundAnswersTheOpenQuestion(t *testing.T) {
	svc, be, _ := newService(t)
	c := openConv(t, svc, "a", nil)
	q := ask(t, svc, "a", "ship it?")

	be.say(t, backend.ThreadRef(c.Status.Ref), "yes, go ahead")

	got, err := svc.GetMessage(q.Metadata.Name)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != api.PhaseAnswered || got.Status.Answer != "yes, go ahead" {
		t.Fatalf("question = %+v", got.Status)
	}
	if got.Status.AnsweredBy != "@tester" {
		t.Errorf("answeredBy = %q", got.Status.AnsweredBy)
	}
	conv, err := svc.GetConversation("a")
	if err != nil {
		t.Fatal(err)
	}
	if conv.Status.PendingQuestion != "" {
		t.Errorf("the answered question is still pending: %q", conv.Status.PendingQuestion)
	}
}

// The operator writing unprompted must still be recorded and reachable, not
// dropped for answering nothing.
func TestInboundWithNoQuestionIsStillRecorded(t *testing.T) {
	svc, be, _ := newService(t)
	c := openConv(t, svc, "a", nil)
	be.say(t, backend.ThreadRef(c.Status.Ref), "an unprompted message")

	list, err := svc.ListMessages("a")
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 {
		t.Fatalf("recorded %d messages", len(list.Items))
	}
	m := list.Items[0]
	if m.Spec.Direction != api.Inbound || m.Spec.Body.Text != "an unprompted message" {
		t.Errorf("message = %+v", m.Spec)
	}
	if m.Spec.InReplyTo != "" {
		t.Errorf("it answered nothing, so inReplyTo should be empty: %q", m.Spec.InReplyTo)
	}
}

// A message in a thread nobody bound is logged, not delivered somewhere
// arbitrary: in front of the wrong agent is worse than nowhere.
func TestInboundInAnUnmappedThreadIsDropped(t *testing.T) {
	svc, be, _ := newService(t)
	openConv(t, svc, "a", nil)
	be.say(t, backend.ThreadRef("999"), "who is this for?")

	list, err := svc.ListMessages("")
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 0 {
		t.Fatalf("an unmapped message was recorded: %+v", list.Items)
	}
}

func TestCancelWithdrawsAndStrikes(t *testing.T) {
	svc, be, _ := newService(t)
	openConv(t, svc, "a", nil)
	q := ask(t, svc, "a", "still relevant?")

	got, err := svc.Cancel(context.Background(), q.Metadata.Name, "moot now")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != api.PhaseCancelled || got.Status.Message != "moot now" {
		t.Fatalf("cancelled = %+v", got.Status)
	}
	be.mu.Lock()
	edited := be.edits[q.Status.Ref]
	be.mu.Unlock()
	if !contains(edited, "moot now") {
		t.Errorf("the reader was not told: %q", edited)
	}
	// The reply prompt must go with it, so it stops looking like a live question.
	if contains(edited, api.DefaultReplyPrompt) {
		t.Errorf("a withdrawn question still invites an answer: %q", edited)
	}
	// And the conversation is free for the next one.
	if _, err := svc.Send(context.Background(), &api.Message{
		Spec: api.MessageSpec{Conversation: "a", AwaitReply: true, Body: api.Body{Question: "the replacement?"}},
	}); err != nil {
		t.Errorf("a replacement question was refused: %v", err)
	}
}

// A transport that cannot edit still has to tell the reader.
func TestCancelFallsBackToANote(t *testing.T) {
	svc, be, _ := newService(t)
	openConv(t, svc, "a", nil)
	q := ask(t, svc, "a", "still relevant?")
	be.mu.Lock()
	be.noEdit = true
	be.mu.Unlock()

	if _, err := svc.Cancel(context.Background(), q.Metadata.Name, "moot"); err != nil {
		t.Fatal(err)
	}
	texts := be.texts()
	if !contains(texts[len(texts)-1], "moot") {
		t.Errorf("no follow-up note was posted: %q", texts[len(texts)-1])
	}
}

func TestCancelRefusesWhatIsNotOpen(t *testing.T) {
	svc, be, _ := newService(t)
	c := openConv(t, svc, "a", nil)
	q := ask(t, svc, "a", "ship it?")
	be.say(t, backend.ThreadRef(c.Status.Ref), "OK")

	if _, err := svc.Cancel(context.Background(), q.Metadata.Name, "too late"); err == nil {
		t.Error("an answered question was cancelled")
	}
}

func TestInboundIsPushedToTheAgent(t *testing.T) {
	svc, be, sink := newService(t)
	c := openConv(t, svc, "a", &api.AgentRef{Sink: "fake", Address: "worker-1"})
	be.say(t, backend.ThreadRef(c.Status.Ref), "look up for a second")

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && len(sink.got()) == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	got := sink.got()
	if len(got) != 1 {
		t.Fatalf("delivered %d messages", len(got))
	}
	// The envelope must say who wrote and how to answer — courier does not
	// impersonate a session, so the agent needs to be told the way back.
	for _, want := range []string{"[courier]", "@tester", "look up for a second", "conversation"} {
		if !contains(got[0], want) {
			t.Errorf("missing %q in the envelope:\n%s", want, got[0])
		}
	}
}

// A message the agent never received must not look delivered.
func TestFailedAgentPushTellsTheHuman(t *testing.T) {
	svc, be, sink := newService(t)
	c := openConv(t, svc, "a", &api.AgentRef{Sink: "fake", Address: "worker-1"})
	sink.mu.Lock()
	sink.fail = api.NewBackendError("the session is gone")
	sink.mu.Unlock()

	before := len(be.texts())
	be.say(t, backend.ThreadRef(c.Status.Ref), "anyone there?")

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && len(be.texts()) == before {
		time.Sleep(20 * time.Millisecond)
	}
	texts := be.texts()
	if len(texts) == before {
		t.Fatal("the human was not told the delivery failed")
	}
	if !contains(texts[len(texts)-1], "the session is gone") {
		t.Errorf("the reason was not passed on: %q", texts[len(texts)-1])
	}
}

func TestWaitForAnswerReturnsWhenAnswered(t *testing.T) {
	svc, be, _ := newService(t)
	c := openConv(t, svc, "a", nil)
	q := ask(t, svc, "a", "ship it?")

	done := make(chan *api.Message, 1)
	go func() {
		m, err := svc.WaitForAnswer(context.Background(), q.Metadata.Name, 5*time.Second)
		if err != nil {
			t.Error(err)
			done <- nil
			return
		}
		done <- m
	}()
	time.Sleep(100 * time.Millisecond)
	be.say(t, backend.ThreadRef(c.Status.Ref), "yes")

	select {
	case m := <-done:
		if m == nil || m.Status.Answer != "yes" {
			t.Fatalf("waited result = %+v", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the wait never returned")
	}
}

// A timeout is not a failure: it comes back still open, which is a true answer
// to "has anyone replied yet".
func TestWaitForAnswerTimesOutOpen(t *testing.T) {
	svc, _, _ := newService(t)
	openConv(t, svc, "a", nil)
	q := ask(t, svc, "a", "ship it?")

	m, err := svc.WaitForAnswer(context.Background(), q.Metadata.Name, 200*time.Millisecond)
	if err != nil {
		t.Fatalf("a timeout should not be an error: %v", err)
	}
	if !m.Open() {
		t.Errorf("phase = %s, want still open", m.Status.Phase)
	}
}

func TestWaitForInboundCollectsWhatWasMissed(t *testing.T) {
	svc, be, _ := newService(t)
	c := openConv(t, svc, "a", nil)
	since := svc.Store().Version()

	// Written while the agent was not looking.
	be.say(t, backend.ThreadRef(c.Status.Ref), "the first one")

	msgs, rv, err := svc.WaitForInbound(context.Background(), "a", since, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Spec.Body.Text != "the first one" {
		t.Fatalf("collected %+v", msgs)
	}
	if rv <= since {
		t.Errorf("the cursor did not advance: %d -> %d", since, rv)
	}
	// Passing the cursor back must not hand the same message over twice.
	msgs, _, err = svc.WaitForInbound(context.Background(), "a", rv, 200*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Errorf("a message was delivered twice: %+v", msgs)
	}
}

func contains(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0)
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func TestSendCarriesFilesToTheTransport(t *testing.T) {
	svc, be, _ := newService(t)
	openConv(t, svc, "a", nil)
	shot := tempFile(t, "screenshot.png", "pretend pixels")

	sent, err := svc.Send(context.Background(), &api.Message{
		Spec: api.MessageSpec{
			Conversation: "a",
			Body:         api.Body{Text: "here is what it looks like"},
			Attachments:  []api.Attachment{{Path: shot}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	be.mu.Lock()
	defer be.mu.Unlock()
	if len(be.sent) != 1 || len(be.sent[0].Files) != 1 {
		t.Fatalf("transport received %+v", be.sent)
	}
	f := be.sent[0].Files[0]
	if f.Name != "screenshot.png" || f.Size == 0 || f.MediaType != "image/png" {
		t.Errorf("the attachment reached the transport unobserved: %+v", f)
	}
	// And the record keeps it, so the conversation says what was actually sent.
	if len(sent.Spec.Attachments) != 1 || sent.Spec.Attachments[0].Path != f.Path {
		t.Errorf("the stored message lost its attachment: %+v", sent.Spec.Attachments)
	}
}

// A screenshot with no words is a message. Refusing it would be refusing the
// most natural thing a person does with their phone.
func TestSendAllowsAMessageThatIsOnlyAFile(t *testing.T) {
	svc, be, _ := newService(t)
	openConv(t, svc, "a", nil)
	shot := tempFile(t, "shot.png", "pixels")

	if _, err := svc.Send(context.Background(), &api.Message{
		Spec: api.MessageSpec{Conversation: "a", Attachments: []api.Attachment{{Path: shot}}},
	}); err != nil {
		t.Fatalf("a file-only message was refused: %v", err)
	}
	be.mu.Lock()
	defer be.mu.Unlock()
	if len(be.sent) != 1 || len(be.sent[0].Files) != 1 {
		t.Fatalf("transport received %+v", be.sent)
	}
	// With nothing to say, there is nothing to render.
	if strings.TrimSpace(be.sent[0].Text) != "" {
		t.Errorf("an empty body rendered to %q", be.sent[0].Text)
	}
}

func TestSendStillRefusesAnEmptyMessage(t *testing.T) {
	svc, _, _ := newService(t)
	openConv(t, svc, "a", nil)
	if _, err := svc.Send(context.Background(), &api.Message{
		Spec: api.MessageSpec{Conversation: "a"},
	}); err == nil {
		t.Fatal("a message with neither words nor files was accepted")
	}
}

func TestSendRefusesAFileThatIsNotThere(t *testing.T) {
	svc, _, _ := newService(t)
	openConv(t, svc, "a", nil)
	_, err := svc.Send(context.Background(), &api.Message{
		Spec: api.MessageSpec{
			Conversation: "a",
			Body:         api.Body{Text: "attached"},
			Attachments:  []api.Attachment{{Path: filepath.Join(t.TempDir(), "ghost.txt")}},
		},
	})
	if err == nil {
		t.Fatal("a missing file was accepted")
	}
	// Refused before anything was posted: the operator must not get a message
	// announcing an attachment that never arrives.
	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("err = %v", err)
	}
}

// Files the operator sends are already on this machine, so the agent is told
// where they are rather than how to fetch them.
func TestInboundFilesReachTheAgentAsPaths(t *testing.T) {
	svc, be, sink := newService(t)
	c := openConv(t, svc, "a", &api.AgentRef{Sink: "fake", Address: "worker-1"})
	saved := tempFile(t, "abc-report.pdf", "%PDF-fake")

	be.sayWith(t, backend.ThreadRef(c.Status.Ref), "take a look", []api.Attachment{{
		Path: saved, Name: "report.pdf", Size: 9, MediaType: "application/pdf", Ref: "uniq1",
	}})

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && len(sink.got()) == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	got := sink.got()
	if len(got) != 1 {
		t.Fatalf("delivered %d messages", len(got))
	}
	for _, want := range []string{saved, "report.pdf", "read them as ordinary files"} {
		if !contains(got[0], want) {
			t.Errorf("the envelope does not mention %q:\n%s", want, got[0])
		}
	}

	list, err := svc.ListMessages("a")
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 1 || len(list.Items[0].Spec.Attachments) != 1 {
		t.Fatalf("the inbound message did not keep its attachment: %+v", list.Items)
	}
	if list.Items[0].Spec.Attachments[0].Path != saved {
		t.Errorf("attachment = %+v", list.Items[0].Spec.Attachments[0])
	}
}

// A message can outlive its conversation: threads get cleaned up while a
// question is still standing. Withdrawing must still settle the message —
// refusing because the thread is gone would leave it open with no way to close
// it, which is exactly what happened in practice.
func TestCancelWorksAfterTheConversationIsGone(t *testing.T) {
	svc, _, _ := newService(t)
	openConv(t, svc, "a", nil)
	q := ask(t, svc, "a", "still relevant?")

	// Removed behind the service's back, which is how the real orphan appeared:
	// the thread went away without anything withdrawing the question first.
	if err := svc.conversations.Delete("a"); err != nil {
		t.Fatal(err)
	}
	got, err := svc.Cancel(context.Background(), q.Metadata.Name, "moot")
	if err != nil {
		t.Fatalf("withdrawing an orphaned question failed: %v", err)
	}
	if got.Status.Phase != api.PhaseCancelled {
		t.Errorf("phase = %s", got.Status.Phase)
	}
	if !contains(got.Status.Message, "no longer exists") {
		t.Errorf("the record should say why nothing was struck: %q", got.Status.Message)
	}
}

// Deleting a thread out from under an open question would leave it standing
// forever: the person cannot answer in a place that is gone.
func TestDeletingAConversationWithdrawsItsOpenQuestion(t *testing.T) {
	svc, _, _ := newService(t)
	openConv(t, svc, "a", nil)
	q := ask(t, svc, "a", "still relevant?")

	if err := svc.DeleteConversation(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	got, err := svc.GetMessage(q.Metadata.Name)
	if err != nil {
		t.Fatal(err)
	}
	if got.Open() {
		t.Errorf("the question is still standing with nowhere to answer it: %+v", got.Status)
	}
	if got.Status.Phase != api.PhaseCancelled {
		t.Errorf("phase = %s, want Cancelled", got.Status.Phase)
	}
}
