package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/kvaps/courier/pkg/api"
	"github.com/kvaps/courier/pkg/backend"
)

// recordingSink stands in for the daemon: it records the presses it was handed
// and answers each one the way the service would.
type recordingSink struct {
	mu      sync.Mutex
	presses []backend.Press
	toast   string
}

func (r *recordingSink) Receive(context.Context, string, backend.Inbound) {}

func (r *recordingSink) Press(_ context.Context, _ string, p backend.Press) (backend.PressResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.presses = append(r.presses, p)
	return backend.PressResult{Toast: r.toast}, nil
}

func (r *recordingSink) SetStatus(context.Context, string, api.Phase, string) {}

func (r *recordingSink) got() []backend.Press {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]backend.Press(nil), r.presses...)
}

// botStub is the Telegram Bot API, reduced to recording the calls made against
// it and saying ok to all of them.
type botStub struct {
	mu    sync.Mutex
	calls map[string][]string
}

func newBotStub(t *testing.T) (*botStub, *httptest.Server) {
	t.Helper()
	s := &botStub{calls: map[string][]string{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		method := r.URL.Path[strings.LastIndexByte(r.URL.Path, '/')+1:]
		s.mu.Lock()
		s.calls[method] = append(s.calls[method], r.Form.Encode())
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":1}}`))
	}))
	t.Cleanup(srv.Close)
	return s, srv
}

func (s *botStub) calledWith(method string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls[method]...)
}

// connected builds a backend wired to the stub and already "connected" to a chat.
func connected(t *testing.T, cfg map[string]any) (*Backend, *botStub) {
	t.Helper()
	t.Setenv("TELEGRAM_BOT_TOKEN", testToken)
	stub, srv := newBotStub(t)
	b, err := build(t, cfg)
	if err != nil {
		t.Fatal(err)
	}
	tg, ok := b.(*Backend)
	if !ok {
		t.Fatalf("unexpected type %T", b)
	}
	tg.api.base = srv.URL
	tg.chatID, tg.forum = -100123, true
	return tg, stub
}

func pressUpdate(from int64, data string) tgUpdate {
	return tgUpdate{
		UpdateID: 1,
		CallbackQuery: &tgCallbackQuery{
			ID:      "cq-1",
			From:    &tgUser{ID: from, Username: "someone"},
			Message: &tgMessage{MessageID: 42, ThreadID: 7, Chat: tgChat{ID: -100123}},
			Data:    data,
		},
	}
}

// A press is the other way to drive an agent, and it arrives as its own update
// type with its own sender. An allow-list that covered only written messages
// would let anybody in the group close another person's question with one tap.
func TestPressFromOutsideTheAllowListIsRefused(t *testing.T) {
	tg, stub := connected(t, map[string]any{"chat": "-100123", "allowFrom": []int64{11}})
	sink := &recordingSink{}

	tg.handle(context.Background(), sink, pressUpdate(99, "c-abc"))

	if got := sink.got(); len(got) != 0 {
		t.Fatalf("a press from outside the allow-list reached the daemon: %+v", got)
	}
	answers := stub.calledWith("answerCallbackQuery")
	if len(answers) != 1 {
		t.Fatalf("answerCallbackQuery called %d times — a refused press leaves a spinner", len(answers))
	}
	if !strings.Contains(answers[0], "not+yours") {
		t.Errorf("the refusal does not say why: %s", answers[0])
	}
}

func TestPressFromTheAllowListReachesTheDaemon(t *testing.T) {
	tg, stub := connected(t, map[string]any{"chat": "-100123", "allowFrom": []int64{11}})
	sink := &recordingSink{toast: "✓ Send"}

	tg.handle(context.Background(), sink, pressUpdate(11, "c-abc"))

	got := sink.got()
	if len(got) != 1 {
		t.Fatalf("presses delivered = %d", len(got))
	}
	if got[0].Choice != "c-abc" || got[0].Message.ID != "42" || got[0].Thread != "7" {
		t.Errorf("press = %+v", got[0])
	}
	if got[0].AuthorID != "11" {
		t.Errorf("authorID = %q — the allow-list is checked against it", got[0].AuthorID)
	}
	if answers := stub.calledWith("answerCallbackQuery"); len(answers) != 1 || !strings.Contains(answers[0], "Send") {
		t.Errorf("the press was not acknowledged with its outcome: %v", answers)
	}
}

// Another bot cannot press a button, and a press from a chat this channel does
// not serve is not this daemon's to act on.
func TestPressIsIgnoredFromABotOrAnotherChat(t *testing.T) {
	tg, _ := connected(t, map[string]any{"chat": "-100123"})
	sink := &recordingSink{}

	bot := pressUpdate(11, "c-abc")
	bot.CallbackQuery.From.IsBot = true
	tg.handle(context.Background(), sink, bot)

	elsewhere := pressUpdate(11, "c-abc")
	elsewhere.CallbackQuery.Message.Chat.ID = -100999
	tg.handle(context.Background(), sink, elsewhere)

	if got := sink.got(); len(got) != 0 {
		t.Fatalf("presses delivered = %+v", got)
	}
}

// One button per row: these are approval buttons read on a phone, and two
// adjacent targets are two chances to send the wrong answer with a thumb.
func TestKeyboardIsOneButtonPerRow(t *testing.T) {
	raw, err := keyboard([]api.Choice{
		{ID: "send", Label: "Send", Answer: "a long answer that must not travel in the button", Ref: "c-1"},
		{ID: "refuse", Label: "Refuse", Answer: "another one", Ref: "c-2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Rows [][]struct {
			Text string `json:"text"`
			Data string `json:"callback_data"`
		} `json:"inline_keyboard"`
	}
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Rows) != 2 {
		t.Fatalf("rows = %d, want one per button", len(got.Rows))
	}
	for i, row := range got.Rows {
		if len(row) != 1 {
			t.Fatalf("row %d holds %d buttons", i, len(row))
		}
		if len(row[0].Data) > callbackDataMax {
			t.Errorf("button data %q is over Telegram's %d bytes", row[0].Data, callbackDataMax)
		}
	}
	if got.Rows[0][0].Text != "Send" || got.Rows[0][0].Data != "c-1" {
		t.Errorf("first button = %+v", got.Rows[0][0])
	}
}

func TestKeyboardRefusesAHandleTelegramCannotCarry(t *testing.T) {
	_, err := keyboard([]api.Choice{{ID: "send", Label: "Send", Answer: "yes", Ref: strings.Repeat("x", 65)}})
	if err == nil {
		t.Fatal("a handle over 64 bytes was accepted")
	}
}

// Every edit courier makes is a message becoming final, so the buttons come off
// with it. A live button on a decided question is one the reader can still press.
func TestEditClearsTheKeyboard(t *testing.T) {
	tg, stub := connected(t, map[string]any{"chat": "-100123"})
	if err := tg.Edit(context.Background(), backend.MessageRef{Thread: "7", ID: "42"}, "decided"); err != nil {
		t.Fatal(err)
	}
	edits := stub.calledWith("editMessageText")
	if len(edits) != 1 || !strings.Contains(edits[0], "inline_keyboard") {
		t.Fatalf("the edit did not clear the keyboard: %v", edits)
	}
}

// Buttons are drawn only when courier asks for them, so a question sent the way
// they were sent before this existed goes out exactly as it did.
func TestSendWithoutChoicesDrawsNoKeyboard(t *testing.T) {
	tg, stub := connected(t, map[string]any{"chat": "-100123"})
	if _, err := tg.Send(context.Background(), "7", backend.Outgoing{Text: "plain question", AwaitReply: true}); err != nil {
		t.Fatal(err)
	}
	sends := stub.calledWith("sendMessage")
	if len(sends) != 1 {
		t.Fatalf("sendMessage called %d times", len(sends))
	}
	if strings.Contains(sends[0], "reply_markup") || strings.Contains(sends[0], "reply_to_message_id") {
		t.Errorf("an ordinary question grew a keyboard or a quotation: %s", sends[0])
	}
}

func TestSendWithChoicesDrawsThemUnderTheQuotedQuestion(t *testing.T) {
	tg, stub := connected(t, map[string]any{"chat": "-100123"})
	_, err := tg.Send(context.Background(), "7", backend.Outgoing{
		Text:    "Draft by the orchestrator:",
		ReplyTo: backend.MessageRef{Thread: "7", ID: "41"},
		Choices: []api.Choice{{ID: "send", Label: "Send", Answer: "yes", Ref: "c-1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	sends := stub.calledWith("sendMessage")
	if len(sends) != 1 {
		t.Fatalf("sendMessage called %d times", len(sends))
	}
	for _, want := range []string{"reply_markup", "callback_data", "reply_to_message_id=41"} {
		if !strings.Contains(sends[0], want) {
			t.Errorf("the drafted answer is missing %q: %s", want, sends[0])
		}
	}
}

// Withdrawing a question is how the fleet works today and predates buttons
// entirely. If Telegram will not take the empty keyboard the edit now asks for,
// the text still has to go out — a cancel that started failing would be this
// change breaking something that had nothing to do with it.
func TestEditFallsBackWhenTheEmptyKeyboardIsRefused(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", testToken)
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		bodies = append(bodies, r.Form.Encode())
		w.Header().Set("Content-Type", "application/json")
		if r.Form.Get("reply_markup") != "" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: can't parse reply_markup JSON object"}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
	}))
	defer srv.Close()

	b, err := build(t, map[string]any{"chat": "-100123"})
	if err != nil {
		t.Fatal(err)
	}
	tg, ok := b.(*Backend)
	if !ok {
		t.Fatalf("unexpected type %T", b)
	}
	tg.api.base = srv.URL
	tg.chatID = -100123

	if err := tg.Edit(context.Background(), backend.MessageRef{Thread: "7", ID: "42"}, "withdrawn"); err != nil {
		t.Fatalf("the edit gave up instead of retrying without the keyboard: %v", err)
	}
	if len(bodies) != 2 {
		t.Fatalf("calls = %d, want the attempt and the retry", len(bodies))
	}
	if strings.Contains(bodies[1], "reply_markup") {
		t.Errorf("the retry still asked for a keyboard: %s", bodies[1])
	}
}
