package telegram

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/kvaps/courier/pkg/backend"
)

func TestTheBoardIsPostedPinnedAndThenEdited(t *testing.T) {
	tg, stub := connected(t, map[string]any{"chat": "-100123", "defaultConversation": "orch"})
	board := backend.Board{
		Waiting: []backend.BoardItem{{Title: "▣ db", Summary: "Postgres or MySQL?", Since: time.Now(),
			Ref: backend.MessageRef{ID: "p41"}}},
		Settled: []backend.BoardItem{{Title: "▣ ui", Summary: "blue or green?", Answer: "green, see decisions.md", Since: time.Now()}},
		Held:    1,
	}
	if err := tg.Board(context.Background(), board); err != nil {
		t.Fatal(err)
	}
	sends := stub.calledWith("sendRichMessage")
	if len(sends) != 1 {
		t.Fatalf("board sends = %d", len(sends))
	}
	text, _ := url.QueryUnescape(sends[0])
	for _, want := range []string{"Ждут тебя: 1", "https://t.me/c/123/41", "Postgres or MySQL?",
		"Оркестратор ответил сам за сутки: 1", "green, see decisions.md", "На проверке у оркестратора: 1",
		"disable_notification=true"} {
		if !strings.Contains(text, want) {
			t.Errorf("board is missing %q: %s", want, text)
		}
	}
	if pins := stub.calledWith("pinChatMessage"); len(pins) != 1 || !strings.Contains(pins[0], "disable_notification=true") {
		t.Errorf("pins = %v", pins)
	}
	// Comments under the board reach the default conversation, but the board
	// never becomes where that conversation's notes go.
	if thread, ok := tg.feed.threadOfPost(1); !ok || thread != "conv:orch" {
		t.Errorf("board thread = %q %v", thread, ok)
	}
	if _, ok := tg.feed.recentPost("conv:orch"); ok {
		t.Error("the board became the orchestrator's latest post")
	}

	if err := tg.Board(context.Background(), backend.Board{}); err != nil {
		t.Fatal(err)
	}
	edits := stub.calledWith("editMessageText")
	if len(edits) != 1 {
		t.Fatalf("the board was not edited in place: %v", edits)
	}
	text, _ = url.QueryUnescape(edits[0])
	if !strings.Contains(text, "Тебя ничего не ждёт") {
		t.Errorf("empty board = %s", text)
	}
}
