package server

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kvaps/courier/pkg/api"
	"github.com/kvaps/courier/pkg/backend"
	"github.com/kvaps/courier/pkg/courier"
	"github.com/kvaps/courier/pkg/store"
)

func newServer(t *testing.T, token string) *httptest.Server {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc := courier.New(st, t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(svc.Close)

	srv := httptest.NewServer(New(Options{Service: svc, Version: "test", Token: token}).Handler())
	t.Cleanup(srv.Close)
	return srv
}

func get(t *testing.T, url string) (int, []byte) {
	t.Helper()
	resp, err := http.Get(url) //nolint:noctx // a test against a local httptest server
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, b
}

// Every failure leaves the daemon in the same shape, so a client can branch on
// the reason instead of matching prose.
func TestErrorsAreStatusObjects(t *testing.T) {
	srv := newServer(t, "")
	code, body := get(t, srv.URL+"/api/v1/conversations/nope")
	if code != http.StatusNotFound {
		t.Errorf("code = %d", code)
	}
	var st api.Status
	if err := json.Unmarshal(body, &st); err != nil {
		t.Fatal(err)
	}
	if st.Kind != "Status" || st.Reason != api.ReasonNotFound || st.Code != http.StatusNotFound {
		t.Errorf("status = %+v", st)
	}
	if st.Message == "" {
		t.Error("a Status with no message tells a person nothing")
	}
}

func TestListIsAnEnvelopeWithAVersion(t *testing.T) {
	srv := newServer(t, "")
	code, body := get(t, srv.URL+"/api/v1/messages")
	if code != http.StatusOK {
		t.Fatalf("code = %d: %s", code, body)
	}
	var list api.List[api.Message]
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatal(err)
	}
	if list.Kind != "MessageList" || list.APIVersion != api.Version {
		t.Errorf("envelope = %+v", list.TypeMeta)
	}
	// An empty list must serialize as [], not null: a client iterating over
	// null is a crash a daemon should not hand out.
	if !strings.Contains(string(body), `"items":[]`) {
		t.Errorf("empty items did not serialize as an array: %s", body)
	}
}

func TestBadRequestBodyIsRejectedWithAReason(t *testing.T) {
	srv := newServer(t, "")
	resp, err := http.Post(srv.URL+"/api/v1/channels", "application/json", strings.NewReader(`{"spec":{"typo":1}}`)) //nolint:noctx // local httptest server
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("code = %d", resp.StatusCode)
	}
}

// Health and the discovery index stay open so a supervisor can check the daemon
// without holding a credential; everything else needs the token.
func TestTokenGuardsTheDataButNotHealth(t *testing.T) {
	srv := newServer(t, "secret")
	if code, _ := get(t, srv.URL+"/api/healthz"); code != http.StatusOK {
		t.Errorf("healthz = %d", code)
	}
	if code, _ := get(t, srv.URL+"/api/v1"); code != http.StatusOK {
		t.Errorf("index = %d", code)
	}
	if code, _ := get(t, srv.URL+"/api/v1/messages"); code != http.StatusBadRequest {
		t.Errorf("an unauthenticated read returned %d", code)
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/api/v1/messages", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer secret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("an authenticated read returned %d", resp.StatusCode)
	}
}

// The watch is chunked JSON, one event per line — readable by anything that can
// read a streaming body, curl included.
func TestWatchStreamsEvents(t *testing.T) {
	srv := newServer(t, "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/v1/watch?kind=Channel", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("code = %d", resp.StatusCode)
	}

	lines := make(chan string, 4)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if line := strings.TrimSpace(sc.Text()); line != "" {
				lines <- line
			}
		}
	}()

	// A channel whose backend does not exist still creates the object, carrying
	// the reason on its status — which is exactly the event we want to observe.
	go func() {
		body := `{"metadata":{"name":"c1"},"spec":{"backend":"nope"}}`
		resp, err := http.Post(srv.URL+"/api/v1/channels", "application/json", strings.NewReader(body)) //nolint:noctx // local httptest server
		if err == nil {
			_ = resp.Body.Close()
		}
	}()

	select {
	case line := <-lines:
		var ev api.WatchEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("frame is not a WatchEvent: %s", line)
		}
		if ev.Type != api.Added {
			t.Errorf("event type = %s", ev.Type)
		}
		var ch api.Channel
		if err := json.Unmarshal(ev.Object, &ch); err != nil {
			t.Fatalf("event object is not a Channel: %s", ev.Object)
		}
		if ch.Metadata.Name != "c1" {
			t.Errorf("watched object = %+v", ch.Metadata)
		}
	case <-ctx.Done():
		t.Fatal("no watch frame arrived")
	}
}

// A version the store cannot honour is refused rather than served a stream with
// a silent hole in it.
func TestWatchRefusesAnImpossibleVersion(t *testing.T) {
	srv := newServer(t, "")
	code, body := get(t, srv.URL+"/api/v1/watch?resourceVersion=notanumber")
	if code != http.StatusBadRequest {
		t.Errorf("code = %d: %s", code, body)
	}
}

func TestDiscoveryIndexListsTheRoutes(t *testing.T) {
	srv := newServer(t, "")
	code, body := get(t, srv.URL+"/api/v1")
	if code != http.StatusOK {
		t.Fatalf("code = %d", code)
	}
	for _, want := range []string{"/api/v1/conversations", "/api/v1/messages", "/mcp", "awaitReply"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("the index does not mention %q", want)
		}
	}
}

// Drafting an answer is routed and fails in the same shape as everything else.
// It is on the HTTP API and has no MCP counterpart: the MCP tool set is what an
// agent is handed, and an agent drafting the approval of its own question is the
// gate dissolving rather than the gate working.
func TestDraftEndpointIsRoutedAndKeepsTheErrorShape(t *testing.T) {
	srv := newServer(t, "")
	body := `{"draftedBy":"the orchestrator","choices":[{"id":"send","label":"Send","answer":"yes"}]}`
	resp, err := http.Post(srv.URL+"/api/v1/messages/nope/draft", "application/json", strings.NewReader(body)) //nolint:noctx // a test against a local httptest server
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("code = %d: %s", resp.StatusCode, raw)
	}
	var st api.Status
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	if st.Reason != api.ReasonNotFound {
		t.Errorf("reason = %q", st.Reason)
	}
}

// A duration is honest only where it is computed on read. Health is where the
// orchestrator asks how long an agent has been standing still — and the list is
// empty rather than absent when nothing is waiting, so a reader can tell "no
// queue" from "this daemon does not report one".
func TestHealthzReportsWhatIsWaiting(t *testing.T) {
	srv := newServer(t, "")
	code, body := get(t, srv.URL+"/api/healthz")
	if code != http.StatusOK {
		t.Fatalf("code = %d: %s", code, body)
	}
	var h struct {
		Waiting []waitingQuestion `json:"waiting"`
	}
	if err := json.Unmarshal(body, &h); err != nil {
		t.Fatal(err)
	}
	if h.Waiting == nil {
		t.Error("health does not say what is waiting on the operator")
	}
}

// stubBackend is a transport that records what it was asked to deliver, so an
// end-to-end test can check the shape of a request an operator actually types.
type stubBackend struct {
	mu   sync.Mutex
	sent []backend.Outgoing
}

func (b *stubBackend) Kind() string { return "stub" }

func (b *stubBackend) Connect(context.Context) (backend.Identity, error) {
	return backend.Identity{Self: "@stub", Target: "a test chat"}, nil
}

func (b *stubBackend) Run(ctx context.Context, _ backend.Sink) error { <-ctx.Done(); return nil }

func (b *stubBackend) OpenThread(context.Context, backend.ThreadSpec) (backend.ThreadRef, error) {
	return "1", nil
}

func (b *stubBackend) CloseThread(context.Context, backend.ThreadRef) error { return nil }

func (b *stubBackend) Send(_ context.Context, ref backend.ThreadRef, out backend.Outgoing) (backend.MessageRef, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sent = append(b.sent, out)
	return backend.MessageRef{Thread: ref, ID: strconv.Itoa(len(b.sent))}, nil
}

func (b *stubBackend) Edit(context.Context, backend.MessageRef, string) error { return nil }

func (b *stubBackend) React(context.Context, backend.MessageRef, backend.Mark) error { return nil }

func (b *stubBackend) last() backend.Outgoing {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sent[len(b.sent)-1]
}

var (
	stubOnce sync.Once
	theStub  = &stubBackend{}
)

func post(t *testing.T, url, body string) (int, []byte) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body)) //nolint:noctx // a test against a local httptest server
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, raw
}

// The whole flow over the wire, in the shape the orchestrator actually types:
// open a thread, ask, draft an answer onto the question. A field name that did
// not match would otherwise be discovered at two in the morning.
func TestDraftOverTheWire(t *testing.T) {
	stubOnce.Do(func() {
		backend.Register("stub", func(backend.Env, json.RawMessage) (backend.Backend, error) { return theStub, nil })
	})
	theStub.mu.Lock()
	theStub.sent = nil
	theStub.mu.Unlock()

	srv := newServer(t, "")
	if code, body := post(t, srv.URL+"/api/v1/channels",
		`{"metadata":{"name":"stub"},"spec":{"backend":"stub"}}`); code != http.StatusCreated {
		t.Fatalf("channel: %d %s", code, body)
	}
	if code, body := post(t, srv.URL+"/api/v1/conversations",
		`{"metadata":{"name":"w"},"spec":{"channel":"stub","title":"▣ worker"}}`); code != http.StatusCreated {
		t.Fatalf("conversation: %d %s", code, body)
	}
	code, body := post(t, srv.URL+"/api/v1/messages",
		`{"spec":{"conversation":"w","awaitReply":true,"body":{"question":"Mention security at all?"}}}`)
	if code != http.StatusCreated {
		t.Fatalf("question: %d %s", code, body)
	}
	var question api.Message
	if err := json.Unmarshal(body, &question); err != nil {
		t.Fatal(err)
	}

	code, body = post(t, srv.URL+"/api/v1/messages/"+question.Metadata.Name+"/draft",
		`{"draftedBy":"orchestrator","choices":[
		   {"id":"send","label":"Send","answer":"Keep one honest sentence about isolation."},
		   {"id":"refuse","label":"Refuse","answer":"Drop the security framing entirely."}]}`)
	if code != http.StatusCreated {
		t.Fatalf("draft: %d %s", code, body)
	}
	var d api.Message
	if err := json.Unmarshal(body, &d); err != nil {
		t.Fatal(err)
	}
	if d.Spec.DraftedBy != "orchestrator" || len(d.Spec.Choices) != 2 || d.Spec.InReplyTo != question.Metadata.Name {
		t.Fatalf("draft = %+v", d.Spec)
	}
	out := theStub.last()
	if len(out.Choices) != 2 || out.ReplyTo.ID != question.Status.Ref {
		t.Errorf("delivered = %+v", out)
	}
	if !strings.Contains(out.Text, "Draft by orchestrator") || !strings.Contains(out.Text, "Keep one honest sentence") {
		t.Errorf("the reader cannot see what they would be approving:\n%s", out.Text)
	}
}
