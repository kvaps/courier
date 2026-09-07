package telegram

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/kvaps/courier/pkg/api"
	"github.com/kvaps/courier/pkg/backend"
)

// Backoff bounds for the poll loop. A conflict is treated as recoverable: the
// other poller may be a process that is about to exit, and a backend that gave
// up on the first 409 would stay deaf until somebody noticed.
const (
	backoffMin = 2 * time.Second
	backoffMax = 60 * time.Second
)

// Run consumes inbound Telegram traffic until ctx is cancelled.
func (b *Backend) Run(ctx context.Context, sink backend.Sink) error {
	log := b.logger()

	offset, err := b.startOffset(ctx)
	if err != nil {
		log.Warn("could not establish a starting offset", "err", err)
	}

	sink.SetStatus(ctx, b.channel, api.PhaseReady, "")
	backoff := backoffMin
	conflicts := 0

	for ctx.Err() == nil {
		ups, err := b.api.getUpdates(ctx, offset, b.cfg.PollTimeout)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if isConflict(err) {
				conflicts++
				msg := "another process is polling this bot token — Telegram allows exactly one. " +
					"Give courier its own bot, or stop the other poller"
				log.Error("getUpdates conflict", "count", conflicts, "retry_in", backoff)
				sink.SetStatus(ctx, b.channel, api.PhaseFailed, msg)
			} else {
				log.Warn("getUpdates failed", "err", err, "retry_in", backoff)
				sink.SetStatus(ctx, b.channel, api.PhaseFailed, err.Error())
			}
			if !sleepCtx(ctx, backoff) {
				return nil
			}
			backoff = min(backoff*2, backoffMax)
			continue
		}
		if backoff != backoffMin || conflicts > 0 {
			sink.SetStatus(ctx, b.channel, api.PhaseReady, "")
		}
		backoff, conflicts = backoffMin, 0

		for _, up := range ups {
			offset = up.UpdateID + 1
			b.handle(ctx, sink, up)
			if err := b.saveOffset(offset); err != nil {
				log.Warn("could not persist the update offset", "err", err)
			}
		}
	}
	return nil
}

// handle turns one update into an inbound message, dropping everything that is
// not a person writing in this chat.
func (b *Backend) handle(ctx context.Context, sink backend.Sink, up tgUpdate) {
	log := b.logger()
	msg := up.Message
	b.mu.RLock()
	chatID := b.chatID
	b.mu.RUnlock()

	switch {
	case msg == nil, msg.From == nil, msg.From.IsBot, msg.isService():
		return
	case msg.Chat.ID != chatID:
		return
	}
	if len(b.allow) > 0 && !b.allow[msg.From.ID] {
		log.Warn("refusing a message from a sender not in allowFrom", "from", msg.From.ID)
		return
	}
	text := msg.body()
	if text == "" {
		return // a sticker, or a photo with no caption: nothing to deliver
	}

	ref := backend.ThreadRef("")
	if msg.ThreadID > 0 {
		ref = backend.ThreadRef(strconv.Itoa(msg.ThreadID))
	}
	sink.Receive(ctx, b.channel, backend.Inbound{
		Thread:   ref,
		Ref:      backend.MessageRef{Thread: ref, ID: strconv.Itoa(msg.MessageID)},
		Author:   msg.From.label(),
		AuthorID: strconv.FormatInt(msg.From.ID, 10),
		Text:     text,
		At:       time.Unix(msg.Date, 0).UTC(),
	})
}

// startOffset decides where inbound reading begins.
//
// Telegram holds undelivered updates for 24 hours, so a first start from zero
// would drain a day of group chatter and push all of it into live agents as if
// it had just been said. Instead the newest pending update is fetched and
// everything up to it is acknowledged unread: the channel starts at "now". A
// channel that has run before keeps its saved offset, so a message sent while
// the daemon was down is still delivered.
func (b *Backend) startOffset(ctx context.Context) (int64, error) {
	if off, ok := b.loadOffset(); ok {
		b.logger().Info("resuming inbound", "offset", off)
		return off, nil
	}
	ups, err := b.api.getUpdates(ctx, -1, 0)
	if err != nil {
		return 0, err
	}
	if len(ups) == 0 {
		return 0, nil
	}
	off := ups[len(ups)-1].UpdateID + 1
	b.logger().Info("first start: skipping the Telegram backlog", "offset", off)
	return off, b.saveOffset(off)
}

func (b *Backend) offsetPath() string {
	if b.stateDir == "" {
		return ""
	}
	return filepath.Join(b.stateDir, "offset")
}

func (b *Backend) loadOffset() (int64, bool) {
	p := b.offsetPath()
	if p == "" {
		return 0, false
	}
	raw, err := os.ReadFile(p) //nolint:gosec // path inside the daemon's own state directory
	if err != nil {
		return 0, false
	}
	off, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil || off <= 0 {
		return 0, false
	}
	return off, true
}

func (b *Backend) saveOffset(off int64) error {
	p := b.offsetPath()
	if p == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(strconv.FormatInt(off, 10)), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
