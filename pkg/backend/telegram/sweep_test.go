package telegram

import (
	"context"
	"testing"
	"time"
)

func age(tg *Backend, post int, d time.Duration) {
	tg.feed.mu.Lock()
	tg.feed.st.Posts[post].At = time.Now().Add(-d)
	tg.feed.mu.Unlock()
}

// A settled post — no open question, nothing new under it — leaves the feed;
// an open one, a recent one and the board stay.
func TestTheSweepClearsOnlySettledPosts(t *testing.T) {
	tg, stub := connected(t, map[string]any{"chat": "-100123"})
	tg.feed.addPost(50, "conv:a", "") // settled
	tg.feed.addPost(51, "conv:b", "") // still open
	tg.feed.addPost(52, "conv:c", "") // recent
	tg.feed.setBoard(53, "conv:o")
	for _, p := range []int{41, 50, 51, 53} {
		age(tg, p, 13*time.Hour)
	}
	if err := tg.Sweep(context.Background(), map[string]bool{"p51": true, "p41": true}); err != nil {
		t.Fatal(err)
	}
	dels := stub.calledWith("deleteMessage")
	if len(dels) != 1 || dels[0] != "chat_id=-100123&message_id=50" {
		t.Fatalf("deleted = %v", dels)
	}
	if tg.feed.known(50) {
		t.Error("a swept post is still recorded")
	}
	for _, p := range []int{41, 51, 52, 53} {
		if !tg.feed.known(p) {
			t.Errorf("post %d was forgotten", p)
		}
	}
}

// A comment is activity: it keeps its post in the feed.
func TestACommentKeepsItsPostAlive(t *testing.T) {
	tg, _ := connected(t, map[string]any{"chat": "-100123"})
	age(tg, 41, 13*time.Hour)
	tg.feed.touch(41)
	if stale := tg.feed.stale(12 * time.Hour); len(stale) != 0 {
		t.Errorf("stale = %v", stale)
	}
}
