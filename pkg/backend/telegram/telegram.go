// Package telegram is courier's Telegram backend: a channel where each
// question is a post, and the channel's discussion group where the replies to
// each post live as its comments. See feed.go.
//
// Two facts about Telegram shape everything here. Only one process may
// long-poll getUpdates for a given bot token — a second one gets 409 Conflict
// and the two then split inbound messages arbitrarily — so the daemon says so
// plainly rather than looking like an outage. And a bot with privacy mode on
// sees only messages addressed to it, unless it is an administrator of the
// group, which is why administrator rights in the discussion group are a
// requirement and not a convenience.
package telegram

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/kvaps/courier/pkg/api"
	"github.com/kvaps/courier/pkg/backend"
)

// Kind is the registered backend name.
const Kind = "telegram"

func init() { backend.Register(Kind, newBackend) }

// Config is a Telegram channel's configuration.
//
// It names a credential; it never carries one. A config with an inline token
// would be readable through the API and written to the store as plain text, so
// one is refused outright rather than accepted and redacted later.
type Config struct {
	// Chat is the channel: a numeric id ("-1001234567890"), a public
	// "@username", or a t.me link to either. It must have a discussion group
	// linked, and the bot must administer both.
	Chat string `json:"chat"`
	// TokenFile is a KEY=VALUE file holding the bot token. Defaults to
	// ~/.claude/channels/telegram/.env, the file the Claude Code Telegram
	// channel uses — convenient, and the reason the 409 above is worth knowing.
	TokenFile string `json:"tokenFile,omitempty"`
	// TokenEnv is the environment variable to read the token from, checked
	// before the file. Defaults to TELEGRAM_BOT_TOKEN.
	TokenEnv string `json:"tokenEnv,omitempty"`
	// ReplyPrompt closes a question with the shortest invitation to answer.
	// It is a channel setting because the language a person is addressed in
	// belongs to the channel, not to the daemon.
	ReplyPrompt string `json:"replyPrompt,omitempty"`
	// ChoicePrompt closes a drafted answer, saying the buttons are an offer and
	// writing a reply by hand still works. A channel setting for the same
	// reason ReplyPrompt is one.
	ChoicePrompt string `json:"choicePrompt,omitempty"`
	// AllowFrom, when set, restricts who may drive agents from this chat. It
	// covers both ways of driving one: writing a message and pressing a button.
	// Empty means anyone who can write in the group, which is acceptable only
	// because the group is private — and a group of two is only ever a group of
	// two until somebody is added to it.
	AllowFrom []int64 `json:"allowFrom,omitempty"`
	// PollTimeout is the getUpdates long-poll timeout in seconds.
	PollTimeout int `json:"pollTimeout,omitempty"`
	// Transcribe, when set, turns voice messages into words. Without it a voice
	// message is not carried at all: an agent has no ears, and handing it the
	// path to an .oga it cannot open is a delivery in name only.
	Transcribe *TranscribeConfig `json:"transcribe,omitempty"`

	// Token is refused. It exists in the struct only so that supplying one is
	// a clear error instead of a silently ignored field.
	Token string `json:"token,omitempty"`
}

// Backend implements backend.Backend over the Telegram Bot API.
type Backend struct {
	channel  string
	stateDir string
	inboxDir string
	cfg      Config
	api      *botAPI

	hear *transcriber

	feed *feed

	mu      sync.RWMutex
	chatID  int64 // the channel
	groupID int64 // its discussion group
	title   string
	allow   map[int64]bool
}

func newBackend(env backend.Env, raw json.RawMessage) (backend.Backend, error) {
	cfg := Config{}
	if len(raw) > 0 {
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&cfg); err != nil {
			return nil, api.NewInvalid("telegram config: %v", err)
		}
	}
	if cfg.Token != "" {
		return nil, api.NewInvalid(
			"telegram config must not contain a literal token: the channel object is served by the API and written to disk. " +
				"Use tokenFile or tokenEnv instead")
	}
	if strings.TrimSpace(cfg.Chat) == "" {
		return nil, api.NewInvalid("telegram config: chat is required (a numeric id, an @username, or a t.me link)")
	}
	if cfg.TokenEnv == "" {
		cfg.TokenEnv = "TELEGRAM_BOT_TOKEN"
	}
	if cfg.TokenFile == "" {
		cfg.TokenFile = defaultTokenFile()
	}
	if cfg.ReplyPrompt == "" {
		cfg.ReplyPrompt = api.DefaultReplyPrompt
	}
	if cfg.ChoicePrompt == "" {
		cfg.ChoicePrompt = api.DefaultChoicePrompt
	}
	if cfg.PollTimeout <= 0 {
		cfg.PollTimeout = 30
	}

	token, err := loadToken(cfg.TokenEnv, cfg.TokenFile)
	if err != nil {
		return nil, err
	}
	hear, err := newTranscriber(cfg.Transcribe)
	if err != nil {
		return nil, err
	}
	allow := map[int64]bool{}
	for _, id := range cfg.AllowFrom {
		allow[id] = true
	}
	return &Backend{
		hear:     hear,
		channel:  env.Channel,
		stateDir: env.StateDir,
		inboxDir: env.InboxDir,
		cfg:      cfg,
		// The HTTP timeout must clear the long poll, or every poll would be
		// cancelled client-side just before Telegram was about to answer it.
		api:   newBotAPI(token, time.Duration(cfg.PollTimeout+30)*time.Second),
		allow: allow,
		feed:  newFeed(env.StateDir),
	}, nil
}

// Kind returns the registered backend name.
func (b *Backend) Kind() string { return Kind }

// ReplyPrompt is the channel's configured closing line for a question.
func (b *Backend) ReplyPrompt() string { return b.cfg.ReplyPrompt }

// ChoicePrompt is the channel's configured closing line for a drafted answer.
func (b *Backend) ChoicePrompt() string { return b.cfg.ChoicePrompt }

// Connect authenticates and resolves the channel and its discussion group.
func (b *Backend) Connect(ctx context.Context) (backend.Identity, error) {
	me, err := b.api.getMe(ctx)
	if err != nil {
		return backend.Identity{}, api.NewBackendError("telegram getMe: %v", err)
	}
	ref, err := chatRef(b.cfg.Chat)
	if err != nil {
		return backend.Identity{}, err
	}
	chat, err := b.api.getChat(ctx, ref)
	if err != nil {
		return backend.Identity{}, api.NewBackendError("telegram getChat %s: %v", ref, err)
	}
	if chat.Type != "channel" {
		return backend.Identity{}, api.NewInvalid(
			"telegram chat %s is a %s, not a channel: courier posts one question per channel post "+
				"and reads the replies in the channel's discussion group", ref, chat.Type)
	}
	if chat.LinkedChatID == 0 {
		return backend.Identity{}, api.NewInvalid(
			"telegram channel %s has no discussion group: link one in the channel's settings "+
				"(Edit → Discussion) and make the bot an administrator there", ref)
	}

	b.mu.Lock()
	b.chatID, b.groupID, b.title = chat.ID, chat.LinkedChatID, chat.Title
	b.mu.Unlock()

	target := chat.Title
	if target == "" {
		target = ref
	}
	return backend.Identity{Self: me.label(), Target: target}, nil
}

func (b *Backend) chats() (channel, group int64) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.chatID, b.groupID
}

// OpenThread creates nothing. A conversation is an agent's address; what the
// reader sees is a post per question, made when the question is sent.
func (b *Backend) OpenThread(_ context.Context, t backend.ThreadSpec) (backend.ThreadRef, error) {
	return backend.ThreadRef("conv:" + t.Conversation), nil
}

// CloseThread has nothing to close: posts stay where the reader left them.
func (b *Backend) CloseThread(context.Context, backend.ThreadRef) error { return nil }

// Send delivers a message as a post of its own or as a comment under one.
//
// A question is always a new post: one problem, one post, one thread of
// replies. A drafted answer goes under the post it answers. Anything else — a
// status note, a result — goes under the conversation's latest post while that
// post is recent, and becomes a post of its own otherwise.
//
// Files go as separate comments after the text, never as a captioned upload: a
// caption is capped at 1024 characters, and keeping the reference on a text
// message keeps Edit — how a withdrawn question is struck — pointed at text.
func (b *Backend) Send(ctx context.Context, ref backend.ThreadRef, out backend.Outgoing) (backend.MessageRef, error) {
	channel, group := b.chats()
	if channel == 0 {
		return backend.MessageRef{}, api.NewBackendError("telegram channel %s is not connected", b.channel)
	}
	markup, merr := keyboard(out.Choices)
	if merr != nil {
		return backend.MessageRef{}, merr
	}
	if strings.TrimSpace(out.Text) == "" && markup != "" {
		return backend.MessageRef{}, api.NewInvalid("buttons need a message to sit under: this one has no text")
	}

	// Where does it go?
	under := 0 // the post to comment under; 0 means a new post
	if !out.ReplyTo.Zero() {
		if isPost, n, err := parseRef(out.ReplyTo.ID); err == nil && isPost {
			under = n
		}
	} else if !out.AwaitReply {
		if n, ok := b.feed.recentPost(string(ref)); ok {
			under = n
		}
	}

	var first backend.MessageRef
	text := strings.TrimSpace(out.Text)
	if under == 0 {
		header := postHeader(out.ThreadTitle)
		if text == "" {
			text = "📎"
		}
		// A question is the one thing worth a sound; everything else waits to
		// be looked at.
		id, err := b.sendText(ctx, channel, withHeader(header, text), 0, markup, !out.AwaitReply)
		if err != nil {
			return backend.MessageRef{}, err
		}
		b.feed.addPost(id, string(ref), header)
		first = postRef(ref, id)
		under = id
		text = ""
	}

	copyID := 0
	if text != "" || len(out.Files) > 0 {
		c, ok := b.feed.copyOf(ctx, under)
		if !ok {
			return first, api.NewBackendError(
				"telegram: the discussion group never received a copy of post %d, so nothing can be added under it — "+
					"is the bot an administrator of the discussion group?", under)
		}
		copyID = c
	}
	if text != "" {
		id, err := b.sendText(ctx, group, text, copyID, markup, true)
		if err != nil {
			return first, err
		}
		if first.Zero() {
			first = commentRef(ref, id)
		}
	}
	for _, f := range out.Files {
		name := f.Name
		if name == "" {
			name = filepath.Base(f.Path)
		}
		id, ferr := b.api.sendDocument(ctx, group, copyID, f.Path, name, "")
		if ferr != nil {
			return first, api.NewBackendError("telegram sendDocument %q: %v", name, ferr)
		}
		if first.Zero() {
			first = commentRef(ref, id)
		}
	}
	return first, nil
}

// sendText posts one text message, honouring a flood wait once. Telegram says
// exactly how long to wait; ignoring it and retrying at once is how a bot earns
// a longer ban.
func (b *Backend) sendText(ctx context.Context, chatID int64, text string, replyTo int, markup string, silent bool) (int, error) {
	var last error
	for attempt := 0; attempt < 2; attempt++ {
		msgID, err := b.api.sendMessage(ctx, chatID, 0, text, replyTo, markup, silent)
		if err == nil {
			return msgID, nil
		}
		last = err
		var ae *apiError
		if attempt == 0 && asAPIError(err, &ae) && ae.RetryAfter > 0 {
			select {
			case <-ctx.Done():
				return 0, ctx.Err()
			case <-time.After(time.Duration(ae.RetryAfter+1) * time.Second):
			}
			continue
		}
		break
	}
	return 0, api.NewBackendError("telegram sendMessage: %v", last)
}

// Edit replaces a sent message, which is how a withdrawn or answered question
// stops looking like a live one. A post keeps its header line: the reader
// still needs to see whose problem it was.
func (b *Backend) Edit(ctx context.Context, ref backend.MessageRef, text string) error {
	isPost, id, err := parseRef(ref.ID)
	if err != nil {
		return err
	}
	channel, group := b.chats()
	chatID := group
	if isPost {
		chatID = channel
		text = withHeader(b.feed.header(id), text)
	}
	if err := b.api.editMessageText(ctx, chatID, id, text); err != nil {
		if isNotSupported(err) {
			return api.NewNotSupported("telegram will not edit this message: %v", err)
		}
		return api.NewBackendError("telegram editMessageText: %v", err)
	}
	return nil
}

// callbackDataMax is Telegram's hard cap on what a button may carry: 64 bytes.
// It is the reason a choice's answer lives in the message resource and only a
// handle to it travels in the button.
const callbackDataMax = 64

// keyboard renders courier's choices as an inline keyboard, one button per row.
//
// One per row rather than side by side: these are approval buttons read on a
// phone, and two adjacent targets are two chances to send the wrong answer with
// a thumb. A tall keyboard costs a little scrolling; a mis-tap costs a decision.
func keyboard(choices []api.Choice) (string, error) {
	if len(choices) == 0 {
		return "", nil
	}
	rows := make([][]map[string]string, 0, len(choices))
	for _, c := range choices {
		if len(c.Ref) > callbackDataMax {
			return "", api.NewInvalid("choice %q has a handle longer than Telegram's %d-byte button data", c.ID, callbackDataMax)
		}
		if strings.TrimSpace(c.Label) == "" || strings.TrimSpace(c.Ref) == "" {
			return "", api.NewInvalid("choice %q needs both a label and a handle", c.ID)
		}
		rows = append(rows, []map[string]string{{"text": c.Label, "callback_data": c.Ref}})
	}
	raw, err := json.Marshal(map[string]any{"inline_keyboard": rows})
	if err != nil {
		return "", api.NewInternalError("render the keyboard: %v", err)
	}
	return string(raw), nil
}

// marks maps courier's acknowledgements onto emoji Telegram accepts.
var marks = map[backend.Mark]string{
	backend.MarkSeen:     "👀",
	backend.MarkAccepted: "👍",
	backend.MarkClear:    "",
}

// React puts an acknowledgement on a human's message. Best-effort by contract:
// reactions are cosmetic and a failure here must never read as a failed
// delivery.
func (b *Backend) React(ctx context.Context, ref backend.MessageRef, mark backend.Mark) error {
	emoji, ok := marks[mark]
	if !ok {
		return api.NewInvalid("unknown mark %q", mark)
	}
	isPost, id, err := parseRef(ref.ID)
	if err != nil {
		return err
	}
	channel, group := b.chats()
	chatID := group
	if isPost {
		chatID = channel
	}
	if err := b.api.setMessageReaction(ctx, chatID, id, emoji); err != nil {
		return api.NewBackendError("telegram setMessageReaction: %v", err)
	}
	return nil
}

func asAPIError(err error, target **apiError) bool {
	for err != nil {
		if ae, ok := err.(*apiError); ok { //nolint:errorlint // walked manually below
			*target = ae
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

var (
	tmePrivate = regexp.MustCompile(`^(?:https?://)?t\.me/c/(\d+)`)
	tmePublic  = regexp.MustCompile(`^(?:https?://)?t\.me/([A-Za-z][A-Za-z0-9_]{3,})`)
	numericRef = regexp.MustCompile(`^-?\d+$`)
)

// chatRef turns whatever the operator wrote into something the Bot API accepts.
func chatRef(chat string) (string, error) {
	c := strings.TrimSpace(chat)
	switch {
	case numericRef.MatchString(c), strings.HasPrefix(c, "@"):
		return c, nil
	case strings.Contains(c, "t.me/+"), strings.Contains(c, "joinchat"):
		return "", api.NewInvalid(
			"an invite link cannot be resolved by a bot: add the bot to the group as an administrator, " +
				"then configure the chat by its numeric id (-100…) or its @username")
	}
	if m := tmePrivate.FindStringSubmatch(c); m != nil {
		// A t.me/c link carries the bare channel id; the Bot API form prefixes it.
		return "-100" + m[1], nil
	}
	if m := tmePublic.FindStringSubmatch(c); m != nil {
		return "@" + m[1], nil
	}
	return "", api.NewInvalid("cannot make sense of chat %q: use a numeric id (-100…), an @username, or a t.me link", chat)
}

func defaultTokenFile() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "channels", "telegram", ".env")
}

var envLine = regexp.MustCompile(`^(\w+)=(.*)$`)

// loadToken resolves the bot token from the environment first, then from a
// KEY=VALUE file. The environment wins so a second daemon can be pointed at a
// second bot without editing a shared file — which is the supported way out of
// the one-poller-per-token limit.
func loadToken(envVar, file string) (string, error) {
	if t := strings.TrimSpace(os.Getenv(envVar)); t != "" {
		return t, nil
	}
	if file == "" {
		return "", api.NewInvalid("no bot token: set %s or configure tokenFile", envVar)
	}
	b, err := os.ReadFile(file) //nolint:gosec // operator-supplied configuration path
	if err != nil {
		return "", api.NewInvalid("read the bot token from %s: %v (or set %s)", file, err, envVar)
	}
	for _, line := range strings.Split(string(b), "\n") {
		m := envLine.FindStringSubmatch(strings.TrimSpace(line))
		if m != nil && m[1] == envVar {
			if t := strings.TrimSpace(strings.Trim(m[2], `"'`)); t != "" {
				return t, nil
			}
		}
	}
	return "", api.NewInvalid("no %s in %s", envVar, file)
}

func (b *Backend) logger() *slog.Logger {
	return slog.With("backend", Kind, "channel", b.channel)
}
