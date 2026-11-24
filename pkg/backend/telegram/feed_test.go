package telegram

import (
	"context"
	"strings"
	"testing"

	"github.com/kvaps/courier/pkg/backend"
)

// A question is a post of its own in the channel, headed by whose it is, and it
// is the one message that makes a sound.
func TestAQuestionIsANewPost(t *testing.T) {
	tg, stub := connected(t, map[string]any{"chat": "-100123"})
	ref, err := tg.Send(context.Background(), "conv:db", backend.Outgoing{
		Text: "Postgres or MySQL?", AwaitReply: true, ThreadTitle: "▣ db-agent",
	})
	if err != nil {
		t.Fatal(err)
	}
	sends := stub.calledWith("sendMessage")
	if len(sends) != 1 {
		t.Fatalf("sendMessage called %d times", len(sends))
	}
	for _, want := range []string{"chat_id=-100123", "%E2%96%A3+db-agent"} {
		if !strings.Contains(sends[0], want) {
			t.Errorf("the post is missing %q: %s", want, sends[0])
		}
	}
	if strings.Contains(sends[0], "disable_notification") {
		t.Errorf("a question went out silently: %s", sends[0])
	}
	if !strings.HasPrefix(ref.ID, "p") {
		t.Errorf("a post's reference = %q", ref.ID)
	}
	if thread, ok := tg.feed.threadOfPost(1); !ok || thread != "conv:db" {
		t.Errorf("the post is not recorded against its conversation: %q %v", thread, ok)
	}
}

// A note that is not a question goes under the conversation's latest post,
// silently, instead of starting another problem in the feed.
func TestANoteGoesUnderTheLatestPost(t *testing.T) {
	tg, stub := connected(t, map[string]any{"chat": "-100123"})
	ref, err := tg.Send(context.Background(), "7", backend.Outgoing{Text: "tests are green"})
	if err != nil {
		t.Fatal(err)
	}
	sends := stub.calledWith("sendMessage")
	if len(sends) != 1 {
		t.Fatalf("sendMessage called %d times", len(sends))
	}
	for _, want := range []string{"chat_id=-100456", "reply_to_message_id=7", "disable_notification=true"} {
		if !strings.Contains(sends[0], want) {
			t.Errorf("the note is missing %q: %s", want, sends[0])
		}
	}
	if !strings.HasPrefix(ref.ID, "c") {
		t.Errorf("a comment's reference = %q", ref.ID)
	}
}

// A conversation with no post yet gets one even for a note: there is nothing
// to hang it under.
func TestANoteWithoutAPostStartsOne(t *testing.T) {
	tg, stub := connected(t, map[string]any{"chat": "-100123"})
	if _, err := tg.Send(context.Background(), "conv:new", backend.Outgoing{Text: "found something"}); err != nil {
		t.Fatal(err)
	}
	sends := stub.calledWith("sendMessage")
	if len(sends) != 1 || !strings.Contains(sends[0], "chat_id=-100123") || !strings.Contains(sends[0], "disable_notification=true") {
		t.Fatalf("expected one silent post: %v", sends)
	}
}

// The discussion group's copy of a post is what ties the post to its comments.
func TestTheGroupsCopyOfAPostIsRecorded(t *testing.T) {
	tg, _ := connected(t, map[string]any{"chat": "-100123"})
	tg.feed.addPost(50, "conv:x", "")
	tg.handle(context.Background(), &recordingSink{}, tgUpdate{Message: &tgMessage{
		MessageID: 9, Chat: tgChat{ID: -100456}, IsAutomaticForward: true,
		SenderChat:    &tgChat{ID: -100123},
		ForwardOrigin: &tgOrigin{Type: "channel", Chat: &tgChat{ID: -100123}, MessageID: 50},
	}})
	if thread, post, ok := tg.feed.threadOfCopy(9); !ok || thread != "conv:x" || post != 50 {
		t.Errorf("copy 9 → %q %d %v", thread, post, ok)
	}
}

// A reply under a post reaches the conversation that owns the post — the reader
// no longer chooses a recipient.
func TestAReplyUnderAPostReachesItsConversation(t *testing.T) {
	tg, _ := connected(t, map[string]any{"chat": "-100123"})
	sink := &recordingSink{}
	tg.handle(context.Background(), sink, tgUpdate{Message: &tgMessage{
		MessageID: 12, ThreadID: 7, Date: 1, Text: "MySQL",
		From: &tgUser{ID: 11, Username: "kvaps"}, Chat: tgChat{ID: -100456},
	}})
	got := sink.inbound()
	if len(got) != 1 || got[0].Thread != "7" || got[0].Text != "MySQL" || got[0].Ref.ID != "c12" {
		t.Fatalf("inbound = %+v", got)
	}
}

// The reader usually comments as an anonymous administrator, which Telegram
// reports as a bot writing on behalf of the group. Only an administrator can do
// that, so it is accepted — while any other bot is not.
func TestAnAnonymousAdministratorIsHeard(t *testing.T) {
	tg, _ := connected(t, map[string]any{"chat": "-100123", "allowFrom": []int64{11}})
	sink := &recordingSink{}
	tg.handle(context.Background(), sink, tgUpdate{Message: &tgMessage{
		MessageID: 13, ThreadID: 7, Date: 1, Text: "не согласен",
		From:       &tgUser{ID: 1087968824, IsBot: true, Username: "GroupAnonymousBot"},
		SenderChat: &tgChat{ID: -100456, Title: "lab"},
		Chat:       tgChat{ID: -100456},
	}})
	tg.handle(context.Background(), sink, tgUpdate{Message: &tgMessage{
		MessageID: 14, ThreadID: 7, Date: 1, Text: "spam",
		From: &tgUser{ID: 555, IsBot: true}, Chat: tgChat{ID: -100456},
	}})
	got := sink.inbound()
	if len(got) != 1 || got[0].Text != "не согласен" {
		t.Fatalf("inbound = %+v", got)
	}
}

// A message under no known post is not delivered anywhere, and the writer is
// told where to write instead of being left believing they answered.
func TestAMessageOutsideAnyPostIsTurnedBack(t *testing.T) {
	tg, stub := connected(t, map[string]any{"chat": "-100123"})
	sink := &recordingSink{}
	tg.handle(context.Background(), sink, tgUpdate{Message: &tgMessage{
		MessageID: 15, Date: 1, Text: "кому это?",
		From: &tgUser{ID: 11}, Chat: tgChat{ID: -100456},
	}})
	if got := sink.inbound(); len(got) != 0 {
		t.Fatalf("delivered somewhere arbitrary: %+v", got)
	}
	if sends := stub.calledWith("sendMessage"); len(sends) != 1 || !strings.Contains(sends[0], "reply_to_message_id=15") {
		t.Errorf("the writer was not told: %v", sends)
	}
}

// Editing a post keeps its header: the reader still needs to see whose it was.
func TestEditingAPostKeepsItsHeader(t *testing.T) {
	tg, stub := connected(t, map[string]any{"chat": "-100123"})
	if err := tg.Edit(context.Background(), backend.MessageRef{Thread: "7", ID: "p41"}, "✓ answered: MySQL"); err != nil {
		t.Fatal(err)
	}
	edits := stub.calledWith("editMessageText")
	if len(edits) != 1 || !strings.Contains(edits[0], "chat_id=-100123") || !strings.Contains(edits[0], "agent") {
		t.Fatalf("edit = %v", edits)
	}
}

// The maps survive a restart: a reply written after courier restarts still
// finds its conversation.
func TestTheFeedSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	f := newFeed(dir)
	f.addPost(70, "conv:y", "h")
	f.sawCopy(70, 21)
	g := newFeed(dir)
	if thread, post, ok := g.threadOfCopy(21); !ok || thread != "conv:y" || post != 70 {
		t.Errorf("after restart: %q %d %v", thread, post, ok)
	}
}
