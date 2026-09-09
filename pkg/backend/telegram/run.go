package telegram

import (
	"context"
	"fmt"
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
	// conflictGrace is how many conflicts in a row are taken as a handover
	// rather than a rival. Restarting the daemon produces one or two: the
	// process being replaced still holds a long poll open, and Telegram takes a
	// moment to notice the connection is gone. Reporting that as a failed
	// channel trains the operator to ignore the message that matters — the
	// other poller that never goes away. Past this count the channel goes
	// Failed exactly as before, about six seconds in.
	conflictGrace = 3
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
				// Shutdown cancelled the poll; that is a clean stop, not a
				// transport failure worth reporting as one.
				return nil //nolint:nilerr // ctx cancellation is the caller's own doing
			}
			if isConflict(err) {
				conflicts++
				if msg, failed := conflictReport(conflicts); failed {
					log.Error("getUpdates conflict", "count", conflicts, "retry_in", backoff)
					sink.SetStatus(ctx, b.channel, api.PhaseFailed, msg)
				} else {
					log.Info("getUpdates conflict; waiting for the previous poller to let go",
						"count", conflicts, "retry_in", backoff)
				}
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

// conflictReport decides how a run of getUpdates conflicts should be reported.
//
// The two cases look identical on the wire and could not be less alike in
// meaning. A restart produces a conflict or two while the process being
// replaced lets go of its long poll, and it clears itself. A second poller that
// is there to stay — the Claude Code Telegram plugin, most often — produces one
// every time, and it is the failure that looks like an outage and is not: the
// daemon keeps sending, the topics fill up, and replies simply never arrive.
//
// So the first few are quiet and the rest are loud. Calling a handover a failed
// channel is how an operator learns to scroll past the message that matters.
func conflictReport(count int) (message string, failed bool) {
	if count < conflictGrace {
		return "", false
	}
	return "another process is polling this bot token — Telegram allows exactly one. " +
		"Give courier its own bot, or stop the other poller", true
}

// handle turns one update into an inbound message or a button press, dropping
// everything that is not a person acting in this chat.
func (b *Backend) handle(ctx context.Context, sink backend.Sink, up tgUpdate) {
	if up.CallbackQuery != nil {
		b.handlePress(ctx, sink, up.CallbackQuery)
		return
	}
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
	ref := backend.ThreadRef("")
	if msg.ThreadID > 0 {
		ref = backend.ThreadRef(strconv.Itoa(msg.ThreadID))
	}

	files := b.download(ctx, msg, string(ref))
	text := msg.body()
	spoken := false
	// A voice message carries words, and the words are the whole of it. An agent
	// has no ears, so the audio is turned into text here or the message does not
	// travel at all — handing over the path to an .oga nobody can open would be
	// a delivery in name only.
	if msg.Voice != nil && text == "" {
		heard, ok := b.listen(ctx, msg, ref)
		if !ok {
			return // the person has been told; there are no words to carry
		}
		text, spoken = heard, true
	}
	if text == "" && len(files) == 0 {
		return // a sticker, or something with no content to carry
	}

	sink.Receive(ctx, b.channel, backend.Inbound{
		Thread:   ref,
		Ref:      backend.MessageRef{Thread: ref, ID: strconv.Itoa(msg.MessageID)},
		Author:   msg.From.label(),
		AuthorID: strconv.FormatInt(msg.From.ID, 10),
		Text:     text,
		Spoken:   spoken,
		At:       time.Unix(msg.Date, 0).UTC(),
		Files:    files,
	})
}

// listen turns a voice message into words, or tells the person why it could not.
//
// The message is named to the transcriber the way the transport names it — this
// chat, this message id — and never by anything read out of the audio. Which
// agent an answer belongs to is decided by the topic it was spoken in, and a
// recognised word must not be able to change that.
//
// Every failure ends in the thread rather than in the log alone. The person who
// spoke is the only one who can do anything about any of it, and a voice message
// that vanishes silently is the worst of the available outcomes: they believe
// they have answered.
func (b *Backend) listen(ctx context.Context, msg *tgMessage, thread backend.ThreadRef) (string, bool) {
	log := b.logger()
	if b.hear == nil {
		log.Warn("a voice message arrived but this channel has no transcription configured", "message", msg.MessageID)
		b.sayInThread(ctx, thread, "🎤 I cannot turn voice into words on this channel — please write it out.")
		return "", false
	}

	b.mu.RLock()
	chatID := b.chatID
	b.mu.RUnlock()

	started := time.Now()
	res, err := b.hear.heard(ctx, chatID, msg.MessageID)
	if err != nil {
		log.Error("could not transcribe a voice message", "message", msg.MessageID, "err", err)
		b.sayInThread(ctx, thread, "🎤 could not transcribe that — please write it out.")
		return "", false
	}
	if res.Status != transcribedOK || strings.TrimSpace(res.Text) == "" {
		log.Warn("a voice message came back without words", "message", msg.MessageID, "status", res.Status)
		b.sayInThread(ctx, thread, "🎤 "+res.why()+".")
		return "", false
	}
	log.Info("transcribed a voice message", "message", msg.MessageID, "status", res.Status,
		"took", time.Since(started).Round(time.Millisecond))
	return strings.TrimSpace(res.Text), true
}

// sayInThread posts a short line where the person is looking. Best-effort: it
// is how they are told something went wrong, and failing to tell them is worth
// a log line and nothing more.
func (b *Backend) sayInThread(ctx context.Context, thread backend.ThreadRef, text string) {
	id, err := threadID(thread)
	if err != nil {
		return
	}
	b.mu.RLock()
	chatID := b.chatID
	b.mu.RUnlock()
	if _, serr := b.api.sendMessage(ctx, chatID, id, text, 0, ""); serr != nil {
		b.logger().Warn("could not tell the reader about a voice message", "err", serr)
	}
}

// toastMax is Telegram's cap on the text shown over a pressed button.
const toastMax = 200

// handlePress turns a tap into a press the daemon can act on, and tells the
// person what their tap did.
//
// The allow-list is checked here and not only on written messages. A press is
// the other way to drive an agent, and it arrives as its own update type with
// its own sender: an allow-list that covers only messages would let anybody who
// can see the group close another person's question with one tap. The group is
// two people today, which is exactly the kind of fact that stops being true
// without anybody revisiting the code that assumed it.
func (b *Backend) handlePress(ctx context.Context, sink backend.Sink, cq *tgCallbackQuery) {
	log := b.logger()
	if cq.From == nil || cq.From.IsBot || cq.Message == nil {
		return
	}
	b.mu.RLock()
	chatID := b.chatID
	b.mu.RUnlock()
	if cq.Message.Chat.ID != chatID {
		// Not ours to act on, but the clock is spinning on somebody's button.
		b.answerPress(ctx, cq.ID, "", false)
		return
	}
	if len(b.allow) > 0 && !b.allow[cq.From.ID] {
		log.Warn("refusing a button press from a sender not in allowFrom", "from", cq.From.ID)
		b.answerPress(ctx, cq.ID, "This is not yours to answer.", true)
		return
	}

	ref := backend.ThreadRef("")
	if cq.Message.ThreadID > 0 {
		ref = backend.ThreadRef(strconv.Itoa(cq.Message.ThreadID))
	}
	// Logged on arrival, before the daemon decides anything. A press is the one
	// inbound event that leaves no trace in the chat when it goes wrong — the
	// message it was on looks the same either way — so the log is the only place
	// anyone can find out that it happened at all.
	log.Info("button press", "from", cq.From.label(), "thread", ref, "message", cq.Message.MessageID)

	res, err := sink.Press(ctx, b.channel, backend.Press{
		Thread:   ref,
		Message:  backend.MessageRef{Thread: ref, ID: strconv.Itoa(cq.Message.MessageID)},
		Choice:   cq.Data,
		Author:   cq.From.label(),
		AuthorID: strconv.FormatInt(cq.From.ID, 10),
		At:       time.Now().UTC(),
	})
	toast, alert := res.Toast, res.Alert
	if err != nil {
		log.Warn("a button press was refused", "from", cq.From.ID, "err", err)
		toast, alert = err.Error(), true
	}
	log.Debug("acknowledging a button press", "from", cq.From.label(), "toast", toast, "alert", alert)
	b.answerPress(ctx, cq.ID, toast, alert)
}

// answerPress closes the spinner Telegram puts on a pressed button. It is
// best-effort: the decision is already recorded, and failing to draw the
// acknowledgement must not look like a failure to take the answer.
func (b *Backend) answerPress(ctx context.Context, id, text string, alert bool) {
	if r := []rune(text); len(r) > toastMax {
		text = string(r[:toastMax-1]) + "…"
	}
	if err := b.api.answerCallbackQuery(ctx, id, text, alert); err != nil {
		b.logger().Warn("could not acknowledge a button press", "err", err)
	}
}

// download fetches whatever the person attached, into the daemon's inbox.
//
// A file that cannot be fetched is logged and skipped rather than failing the
// whole message: the caption may be the important half, and an agent that gets
// the words without the screenshot is better off than one that gets neither.
func (b *Backend) download(ctx context.Context, msg *tgMessage, thread string) []api.Attachment {
	wanted := attached(msg)
	if len(wanted) == 0 {
		return nil
	}
	if b.inboxDir == "" {
		b.logger().Warn("dropping attachments: this channel has no inbox directory")
		return nil
	}
	dir := filepath.Join(b.inboxDir, sanitiseSegment(thread))

	out := make([]api.Attachment, 0, len(wanted))
	for _, w := range wanted {
		path, size, err := b.api.fetch(ctx, w.FileID, w.FileName, dir)
		if err != nil {
			b.logger().Error("could not fetch an attachment", "name", w.FileName, "err", err)
			continue
		}
		out = append(out, api.Attachment{
			Path:      path,
			Name:      displayName(w, path),
			Size:      size,
			MediaType: guessMediaType(path, w.MimeType),
			Ref:       w.UniqueID,
		})
	}
	return out
}

// attached flattens whatever file-bearing fields an update carries into one
// list. A photo arrives as several renditions of the same image; only the
// largest is worth having, since the smaller ones are Telegram's thumbnails.
func attached(msg *tgMessage) []tgFile {
	var out []tgFile
	if msg.Document != nil {
		out = append(out, *msg.Document)
	}
	// Voice is deliberately absent. It is the one attachment whose bytes are not
	// the content — the content is what was said — so it is transcribed rather
	// than downloaded, and a copy of the audio on disk would be a file nothing
	// ever opens. An audio file sent as a file is a different thing and stays.
	for _, f := range []*tgFile{msg.Video, msg.Audio} {
		if f != nil {
			out = append(out, *f)
		}
	}
	if len(msg.Photo) > 0 {
		best := msg.Photo[0]
		for _, p := range msg.Photo[1:] {
			if p.FileSize > best.FileSize {
				best = p
			}
		}
		out = append(out, tgFile{
			FileID:   best.FileID,
			UniqueID: best.UniqueID,
			FileName: fmt.Sprintf("photo-%dx%d.jpg", best.Width, best.Height),
			MimeType: "image/jpeg",
			FileSize: best.FileSize,
		})
	}
	return out
}

func displayName(f tgFile, path string) string {
	if n := strings.TrimSpace(f.FileName); n != "" {
		return filepath.Base(n)
	}
	return filepath.Base(path)
}

// sanitiseSegment keeps a thread id usable as one path segment. The id is a
// number today, but it comes off the wire, and this path is created on disk.
func sanitiseSegment(s string) string {
	if s == "" {
		return "general"
	}
	out := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, s)
	if out == "" || strings.Trim(out, "_") == "" {
		return "thread"
	}
	return out
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
