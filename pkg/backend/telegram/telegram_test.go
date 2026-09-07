package telegram

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kvaps/courier/pkg/api"
	"github.com/kvaps/courier/pkg/backend"
)

// A synthetic credential. It is not, and must never be, a real one: fixtures
// end up in git, in CI logs and in error output.
const testToken = "111111:AAAA-this-is-not-a-real-token"

func build(t *testing.T, cfg map[string]any) (backend.Backend, error) {
	t.Helper()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return newBackend(backend.Env{Channel: "tg", StateDir: t.TempDir()}, raw)
}

// The channel object is served by the API and written to disk, so an inline
// credential is refused rather than accepted and redacted afterwards.
func TestConfigRefusesAnInlineToken(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", testToken)
	_, err := build(t, map[string]any{"chat": "-100123", "token": "whatever"})
	if err == nil {
		t.Fatal("an inline token was accepted")
	}
	if !strings.Contains(err.Error(), "tokenFile") {
		t.Errorf("the error should say what to use instead: %v", err)
	}
}

func TestConfigRequiresAChat(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", testToken)
	if _, err := build(t, map[string]any{}); err == nil {
		t.Fatal("a config with no chat was accepted")
	}
}

func TestConfigRejectsUnknownFields(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", testToken)
	// A typo in a config field would otherwise be silently ignored, and the
	// operator would spend the evening wondering why allowFrom does nothing.
	if _, err := build(t, map[string]any{"chat": "-100123", "alowFrom": []int64{1}}); err == nil {
		t.Fatal("an unknown field was accepted")
	}
}

func TestConfigDefaults(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", testToken)
	b, err := build(t, map[string]any{"chat": "-100123"})
	if err != nil {
		t.Fatal(err)
	}
	tg, ok := b.(*Backend)
	if !ok {
		t.Fatalf("unexpected type %T", b)
	}
	if tg.ReplyPrompt() != api.DefaultReplyPrompt {
		t.Errorf("reply prompt = %q", tg.ReplyPrompt())
	}
	if tg.cfg.PollTimeout <= 0 {
		t.Errorf("poll timeout = %d", tg.cfg.PollTimeout)
	}
}

func TestChatRef(t *testing.T) {
	cases := map[string]string{
		"-1001234567890":              "-1001234567890",
		"@some_group":                 "@some_group",
		"https://t.me/c/1234567890/1": "-1001234567890",
		"t.me/c/1234567890":           "-1001234567890",
		"https://t.me/some_group":     "@some_group",
		"https://t.me/some_group/12":  "@some_group",
	}
	for in, want := range cases {
		got, err := chatRef(in)
		if err != nil {
			t.Errorf("chatRef(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("chatRef(%q) = %q, want %q", in, got, want)
		}
	}
}

// A bot cannot resolve an invite link, and failing with the reason beats
// failing with "chat not found" an hour later.
func TestChatRefRejectsInviteLinks(t *testing.T) {
	for _, in := range []string{"https://t.me/+AbCdEf", "https://t.me/joinchat/AbCdEf", "nonsense"} {
		if _, err := chatRef(in); err == nil {
			t.Errorf("chatRef(%q) was accepted", in)
		}
	}
}

func TestLoadTokenPrefersTheEnvironment(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, ".env")
	if err := os.WriteFile(file, []byte("TELEGRAM_BOT_TOKEN=from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := loadToken("TELEGRAM_BOT_TOKEN", file); err != nil || got != "from-file" {
		t.Errorf("from file: %q, %v", got, err)
	}
	// The environment wins so a second daemon can be pointed at a second bot
	// without editing a file shared with something else.
	t.Setenv("TELEGRAM_BOT_TOKEN", "from-env")
	if got, err := loadToken("TELEGRAM_BOT_TOKEN", file); err != nil || got != "from-env" {
		t.Errorf("from env: %q, %v", got, err)
	}
}

func TestLoadTokenExplainsItself(t *testing.T) {
	_, err := loadToken("TELEGRAM_BOT_TOKEN", filepath.Join(t.TempDir(), "absent"))
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "TELEGRAM_BOT_TOKEN") {
		t.Errorf("the error should name what to set: %v", err)
	}
}

// The token sits in the Bot API's request path, so net/http quotes it in its own
// errors. A leak here would put the credential in the log on the first blip.
func TestScrubRemovesBothHalves(t *testing.T) {
	a := newBotAPI(testToken, 0)
	in := `Get "https://api.telegram.org/bot` + testToken + `/getUpdates": dial tcp: timeout`
	got := a.scrub(in)
	if strings.Contains(got, testToken) {
		t.Fatal("the whole token survived scrubbing")
	}
	if strings.Contains(got, "AAAA-this-is-not-a-real-token") {
		t.Fatal("the secret half survived scrubbing")
	}
	if strings.Contains(a.scrub("leaked AAAA-this-is-not-a-real-token here"), "AAAA-this") {
		t.Fatal("the secret half is not scrubbed on its own")
	}
}

func TestThreadID(t *testing.T) {
	if id, err := threadID(""); err != nil || id != 0 {
		t.Errorf("an empty ref is the one implicit thread: %d, %v", id, err)
	}
	if id, err := threadID("17"); err != nil || id != 17 {
		t.Errorf("threadID = %d, %v", id, err)
	}
	if _, err := threadID("General"); err == nil {
		t.Error("a non-numeric thread ref was accepted")
	}
}

func TestServiceMessagesAreNotContent(t *testing.T) {
	// A topic being created arrives as a message in that topic; delivering one
	// would seed an agent with an empty turn.
	if !(&tgMessage{TopicCreated: map[string]any{"name": "x"}}).isService() {
		t.Error("forum_topic_created is a service message")
	}
	if (&tgMessage{Text: "real"}).isService() {
		t.Error("a real message is not a service message")
	}
	if got := (&tgMessage{Caption: " a photo caption "}).body(); got != "a photo caption" {
		t.Errorf("caption fallback = %q", got)
	}
}
