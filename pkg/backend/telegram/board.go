package telegram

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/kvaps/courier/pkg/api"
	"github.com/kvaps/courier/pkg/backend"
)

// Board keeps one pinned post in the channel current: what waits on the reader,
// with links to the posts, and what the reviewer settled on their behalf.
// Comments under it go to the default conversation, which is how a settled
// answer is disputed.
func (b *Backend) Board(ctx context.Context, board backend.Board) error {
	channel, _ := b.chats()
	if channel == 0 {
		return api.NewBackendError("telegram channel %s is not connected", b.channel)
	}
	text := b.renderBoard(board, time.Now())
	if id := b.feed.boardPost(); id > 0 {
		err := b.api.editRichText(ctx, channel, id, text)
		if err == nil || strings.Contains(strings.ToLower(err.Error()), "not modified") {
			return nil
		}
		if !strings.Contains(strings.ToLower(err.Error()), "not found") {
			return api.NewBackendError("telegram edit board: %v", err)
		}
		// The board was deleted by hand: make a new one.
	}
	b.sending.Add(1)
	defer b.sending.Add(-1)
	id, err := b.api.sendRichMessage(ctx, channel, text, 0, "", true)
	if err != nil {
		return api.NewBackendError("telegram send board: %v", err)
	}
	thread := ""
	if b.cfg.DefaultConversation != "" {
		thread = "conv:" + b.cfg.DefaultConversation
	}
	b.feed.setBoard(id, thread)
	if err := b.api.pinChatMessage(ctx, channel, id); err != nil {
		b.logger().Warn("could not pin the board", "err", err)
	}
	return nil
}

func (b *Backend) renderBoard(board backend.Board, now time.Time) string {
	channel, _ := b.chats()
	var s strings.Builder
	fmt.Fprintf(&s, "📌 **Сводка** · %s\n\n", now.Local().Format("02.01 15:04"))
	if len(board.Waiting) == 0 {
		s.WriteString("**Тебя ничего не ждёт.**\n")
	} else {
		fmt.Fprintf(&s, "**Ждут тебя: %d**\n\n", len(board.Waiting))
		for i, it := range board.Waiting {
			line := oneLine(it.Summary, 90)
			if link := b.linkTo(channel, it.Ref); link != "" {
				line = "[" + strings.ReplaceAll(line, "]", ")") + "](" + link + ")"
			}
			fmt.Fprintf(&s, "%d. %s — %s · с %s\n", i+1, it.Title, line, it.Since.Local().Format("02.01 15:04"))
		}
	}
	if len(board.Settled) > 0 {
		fmt.Fprintf(&s, "\n**Оркестратор ответил сам за сутки: %d**\n\n", len(board.Settled))
		for _, it := range board.Settled {
			fmt.Fprintf(&s, "- %s — %s → %s\n", it.Title, oneLine(it.Summary, 70), oneLine(it.Answer, 90))
		}
		s.WriteString("\nНе согласен с ответом — напиши в комментариях к сводке.\n")
	}
	if board.Held > 0 {
		fmt.Fprintf(&s, "\nНа проверке у оркестратора: %d\n", board.Held)
	}
	return s.String()
}

// linkTo is a t.me link to a post, for a private channel.
func (b *Backend) linkTo(channel int64, ref backend.MessageRef) string {
	isPost, id, err := parseRef(ref.ID)
	if err != nil || !isPost {
		return ""
	}
	short := strings.TrimPrefix(strconv.FormatInt(channel, 10), "-100")
	return "https://t.me/c/" + short + "/" + strconv.Itoa(id)
}

func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		s = string(r[:max-1]) + "…"
	}
	return s
}
