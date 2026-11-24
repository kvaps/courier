package telegram

import (
	"context"
	"strings"
	"time"
)

// Sweep deletes posts that have settled: no open question in them and nothing
// written under them for the retention period. Deleting a channel post takes
// its comment thread with it. A post Telegram refuses to delete — older than
// the 48 hours a bot is allowed — is marked kept and left as it is, already
// rewritten as settled.
func (b *Backend) Sweep(ctx context.Context, open map[string]bool) error {
	channel, _ := b.chats()
	if channel == 0 {
		return nil
	}
	retention := time.Duration(b.cfg.RetentionHours) * time.Hour
	if retention <= 0 {
		retention = 12 * time.Hour
	}
	for _, id := range b.feed.stale(retention) {
		if open[postRef("", id).ID] {
			continue
		}
		if err := b.api.deleteMessage(ctx, channel, id); err != nil {
			d := strings.ToLower(err.Error())
			if strings.Contains(d, "not found") {
				b.feed.forget(id) // already gone, by hand
				continue
			}
			b.logger().Info("a settled post could not be deleted; keeping it", "post", id, "err", err)
			b.feed.keep(id)
			continue
		}
		b.logger().Info("swept a settled post", "post", id)
		b.feed.forget(id)
	}
	return nil
}
