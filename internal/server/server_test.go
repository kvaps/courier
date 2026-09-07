package server

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kvaps/courier/pkg/api"
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
