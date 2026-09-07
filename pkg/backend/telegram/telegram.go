// Package telegram is courier's Telegram backend: one forum topic per
// conversation, in a supergroup the bot administers.
//
// Two facts about Telegram shape everything here. Only one process may
// long-poll getUpdates for a given bot token — a second one gets 409 Conflict
// and the two then split inbound messages arbitrarily — so the daemon says so
// plainly rather than looking like an outage. And a bot with privacy mode on
// sees only messages addressed to it, unless it is an administrator of the
// group, which is why administrator rights are a requirement and not a
// convenience.
package telegram

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
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
	// Chat is the destination: a numeric id ("-1001234567890"), a public
	// "@username", or a t.me link to either.
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
	// AllowFrom, when set, restricts who may drive agents from this chat.
	// Empty means anyone who can write in the group, which is acceptable only
	// because the group is private.
	AllowFrom []int64 `json:"allowFrom,omitempty"`
	// PollTimeout is the getUpdates long-poll timeout in seconds.
	PollTimeout int `json:"pollTimeout,omitempty"`

	// Token is refused. It exists in the struct only so that supplying one is
	// a clear error instead of a silently ignored field.
	Token string `json:"token,omitempty"`
}

// Backend implements backend.Backend over the Telegram Bot API.
type Backend struct {
	channel  string
	stateDir string
	cfg      Config
	api      *botAPI

	mu     sync.RWMutex
	chatID int64
	title  string
	forum  bool
	allow  map[int64]bool
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
	if cfg.PollTimeout <= 0 {
		cfg.PollTimeout = 30
	}

	token, err := loadToken(cfg.TokenEnv, cfg.TokenFile)
	if err != nil {
		return nil, err
	}
	allow := map[int64]bool{}
	for _, id := range cfg.AllowFrom {
		allow[id] = true
	}
	return &Backend{
		channel:  env.Channel,
		stateDir: env.StateDir,
		cfg:      cfg,
		// The HTTP timeout must clear the long poll, or every poll would be
		// cancelled client-side just before Telegram was about to answer it.
		api:   newBotAPI(token, time.Duration(cfg.PollTimeout+30)*time.Second),
		allow: allow,
	}, nil
}

// Kind returns the registered backend name.
func (b *Backend) Kind() string { return Kind }

// ReplyPrompt is the channel's configured closing line for a question.
func (b *Backend) ReplyPrompt() string { return b.cfg.ReplyPrompt }

// Connect authenticates and resolves the destination chat.
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

	b.mu.Lock()
	b.chatID, b.title, b.forum = chat.ID, chat.Title, chat.IsForum
	b.mu.Unlock()

	target := chat.Title
	if target == "" {
		target = ref
	}
	if !chat.IsForum {
		// Not fatal: every conversation then shares the one stream. Worth
		// saying out loud, because the per-agent separation silently vanishes.
		target += " (topics are off — every conversation shares one thread)"
	}
	return backend.Identity{Self: me.label(), Target: target}, nil
}

// OpenThread creates a forum topic for a conversation. In a group without
// topics it returns the channel's own thread, so conversations still work —
// they just share one stream.
func (b *Backend) OpenThread(ctx context.Context, t backend.ThreadSpec) (backend.ThreadRef, error) {
	b.mu.RLock()
	chatID, forum := b.chatID, b.forum
	b.mu.RUnlock()
	if chatID == 0 {
		return "", api.NewBackendError("telegram channel %s is not connected", b.channel)
	}
	if !forum {
		return "", nil
	}
	name := strings.TrimSpace(t.Title)
	if name == "" {
		name = t.Conversation
	}
	if r := []rune(name); len(r) > 128 { // Telegram's topic-name limit
		name = string(r[:128])
	}
	id, err := b.api.createForumTopic(ctx, chatID, name)
	if err != nil {
		if isNotSupported(err) {
			return "", api.NewNotSupported("telegram cannot create a topic in this chat: %v", err)
		}
		return "", api.NewBackendError("telegram createForumTopic: %v", err)
	}
	return backend.ThreadRef(strconv.Itoa(id)), nil
}

// CloseThread closes a topic, leaving its history where the human can read it.
func (b *Backend) CloseThread(ctx context.Context, ref backend.ThreadRef) error {
	id, err := threadID(ref)
	if err != nil || id == 0 {
		return err
	}
	b.mu.RLock()
	chatID := b.chatID
	b.mu.RUnlock()
	if err := b.api.closeForumTopic(ctx, chatID, id); err != nil {
		if isNotSupported(err) {
			return nil // already closed, or a chat without topics
		}
		return api.NewBackendError("telegram closeForumTopic: %v", err)
	}
	return nil
}

// Send delivers text into a thread.
func (b *Backend) Send(ctx context.Context, ref backend.ThreadRef, out backend.Outgoing) (backend.MessageRef, error) {
	id, err := threadID(ref)
	if err != nil {
		return backend.MessageRef{}, err
	}
	b.mu.RLock()
	chatID := b.chatID
	b.mu.RUnlock()
	if chatID == 0 {
		return backend.MessageRef{}, api.NewBackendError("telegram channel %s is not connected", b.channel)
	}

	var last error
	for attempt := 0; attempt < 2; attempt++ {
		msgID, err := b.api.sendMessage(ctx, chatID, id, out.Text, 0)
		if err == nil {
			return backend.MessageRef{Thread: ref, ID: strconv.Itoa(msgID)}, nil
		}
		last = err
		var ae *apiError
		if attempt == 0 && asAPIError(err, &ae) && ae.RetryAfter > 0 {
			// Telegram says exactly how long to wait. Ignoring it and retrying
			// immediately is how a bot earns a longer ban.
			select {
			case <-ctx.Done():
				return backend.MessageRef{}, ctx.Err()
			case <-time.After(time.Duration(ae.RetryAfter+1) * time.Second):
			}
			continue
		}
		break
	}
	return backend.MessageRef{}, api.NewBackendError("telegram sendMessage: %v", last)
}

// Edit replaces a sent message, which is how a withdrawn question stops looking
// like a live one.
func (b *Backend) Edit(ctx context.Context, ref backend.MessageRef, text string) error {
	id, err := strconv.Atoi(ref.ID)
	if err != nil {
		return api.NewInvalid("telegram message ref %q is not a message id", ref.ID)
	}
	b.mu.RLock()
	chatID := b.chatID
	b.mu.RUnlock()
	if err := b.api.editMessageText(ctx, chatID, id, text); err != nil {
		if isNotSupported(err) {
			return api.NewNotSupported("telegram will not edit this message: %v", err)
		}
		return api.NewBackendError("telegram editMessageText: %v", err)
	}
	return nil
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
	id, err := strconv.Atoi(ref.ID)
	if err != nil {
		return api.NewInvalid("telegram message ref %q is not a message id", ref.ID)
	}
	b.mu.RLock()
	chatID := b.chatID
	b.mu.RUnlock()
	if err := b.api.setMessageReaction(ctx, chatID, id, emoji); err != nil {
		return api.NewBackendError("telegram setMessageReaction: %v", err)
	}
	return nil
}

func threadID(ref backend.ThreadRef) (int, error) {
	s := strings.TrimSpace(string(ref))
	if s == "" {
		return 0, nil // a chat without topics: the one implicit thread
	}
	id, err := strconv.Atoi(s)
	if err != nil {
		return 0, api.NewInvalid("telegram thread ref %q is not a topic id", ref)
	}
	return id, nil
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
