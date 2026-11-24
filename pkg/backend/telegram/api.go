package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// botAPI is a minimal Telegram Bot API client — only what this backend needs,
// so courier carries no third-party Telegram dependency.
//
// Every string it returns is scrubbed of the token. That is not decoration: the
// token sits in the request path, so net/http's own errors quote it, and the
// first network blip would otherwise print the credential into the log.
type botAPI struct {
	token  string
	base   string
	client *http.Client
}

func newBotAPI(token string, timeout time.Duration) *botAPI {
	return &botAPI{token: token, base: "https://api.telegram.org", client: &http.Client{Timeout: timeout}}
}

// scrub removes the token from anything bound for a log or an error, both whole
// and as its secret half alone.
func (a *botAPI) scrub(s string) string {
	if a.token == "" {
		return s
	}
	s = strings.ReplaceAll(s, a.token, "<token>")
	if _, secret, ok := strings.Cut(a.token, ":"); ok && len(secret) > 4 {
		s = strings.ReplaceAll(s, secret, "<token>")
	}
	return s
}

func (a *botAPI) scrubErr(err error) error {
	if err == nil {
		return nil
	}
	return errors.New(a.scrub(err.Error()))
}

// apiError is a non-ok response. RetryAfter carries the flood-wait hint from a
// 429 so a caller can wait exactly as long as it is told to.
type apiError struct {
	Code        int
	Description string
	RetryAfter  int
}

func (e *apiError) Error() string {
	return fmt.Sprintf("telegram api error %d: %s", e.Code, e.Description)
}

// isConflict reports the 409 Telegram returns when a second process is already
// long-polling this token. It is the single most likely operational failure of
// this backend, so it is a named condition rather than a string match at the
// call site.
func isConflict(err error) bool {
	var ae *apiError
	return errors.As(err, &ae) && ae.Code == 409
}

func isNotSupported(err error) bool {
	var ae *apiError
	// 400 with a "not enough rights" or "method is available only for" body is
	// how Telegram says a chat cannot do this — a permanent no, not a blip.
	if !errors.As(err, &ae) || ae.Code != 400 {
		return false
	}
	d := strings.ToLower(ae.Description)
	return strings.Contains(d, "not enough rights") ||
		strings.Contains(d, "available only for") ||
		strings.Contains(d, "topics") ||
		strings.Contains(d, "not modified")
}

func (a *botAPI) call(ctx context.Context, method string, params url.Values, out any) error {
	endpoint := a.base + "/bot" + a.token + "/" + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(params.Encode()))
	if err != nil {
		return a.scrubErr(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := a.client.Do(req)
	if err != nil {
		return a.scrubErr(err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return a.scrubErr(err)
	}
	var env struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		Description string          `json:"description"`
		ErrorCode   int             `json:"error_code"`
		Parameters  struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return fmt.Errorf("%s: parse reply (http %d): %w", method, resp.StatusCode, a.scrubErr(err))
	}
	if !env.OK {
		return &apiError{Code: env.ErrorCode, Description: a.scrub(env.Description), RetryAfter: env.Parameters.RetryAfter}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(env.Result, out); err != nil {
		return fmt.Errorf("%s: parse result: %w", method, a.scrubErr(err))
	}
	return nil
}

type tgUser struct {
	ID       int64  `json:"id"`
	IsBot    bool   `json:"is_bot"`
	Username string `json:"username"`
	First    string `json:"first_name"`
}

func (u tgUser) label() string {
	switch {
	case u.Username != "":
		return "@" + u.Username
	case u.First != "":
		return u.First
	default:
		return strconv.FormatInt(u.ID, 10)
	}
}

type tgChat struct {
	ID       int64  `json:"id"`
	Type     string `json:"type"`
	Title    string `json:"title"`
	Username string `json:"username"`
	// LinkedChatID is, on a channel, its discussion group: where every post
	// is copied automatically and where the replies to it live.
	LinkedChatID int64 `json:"linked_chat_id"`
}

// tgOrigin is where a forwarded message came from. Only the channel case
// matters here: the discussion group's copy of a post names the post it
// copies, and that is the only link between a post and its comments.
type tgOrigin struct {
	Type      string  `json:"type"`
	Chat      *tgChat `json:"chat"`
	MessageID int     `json:"message_id"`
}

type tgMessage struct {
	MessageID int     `json:"message_id"`
	From      *tgUser `json:"from"`
	// SenderChat is set when someone writes as a chat rather than as
	// themselves — an anonymous group administrator writes as the group.
	SenderChat *tgChat `json:"sender_chat"`
	Chat       tgChat  `json:"chat"`
	ThreadID   int     `json:"message_thread_id"`
	// IsAutomaticForward marks the discussion group's copy of a channel post.
	IsAutomaticForward bool      `json:"is_automatic_forward"`
	ForwardOrigin      *tgOrigin `json:"forward_origin"`
	ReplyTo            *struct {
		MessageID int `json:"message_id"`
	} `json:"reply_to_message"`
	Date    int64  `json:"date"`
	Text    string `json:"text"`
	Caption string `json:"caption"`

	Document *tgFile       `json:"document"`
	Photo    []tgPhotoSize `json:"photo"`
	Video    *tgFile       `json:"video"`
	Audio    *tgFile       `json:"audio"`
	Voice    *tgFile       `json:"voice"`

	// Housekeeping that arrives as a message; delivering one to an agent would
	// seed it with an empty turn.
	TopicCreated  any `json:"forum_topic_created"`
	TopicEdited   any `json:"forum_topic_edited"`
	TopicClosed   any `json:"forum_topic_closed"`
	TopicReopened any `json:"forum_topic_reopened"`
	PinnedMessage any `json:"pinned_message"`
	NewMembers    any `json:"new_chat_members"`
	LeftMember    any `json:"left_chat_member"`
}

func (m *tgMessage) body() string {
	if s := strings.TrimSpace(m.Text); s != "" {
		return s
	}
	return strings.TrimSpace(m.Caption)
}

func (m *tgMessage) isService() bool {
	return m.TopicCreated != nil || m.TopicEdited != nil || m.TopicClosed != nil || m.TopicReopened != nil ||
		m.PinnedMessage != nil || m.NewMembers != nil || m.LeftMember != nil
}

// tgCallbackQuery is a tap on an inline button. Its Data is the callback_data
// the button was drawn with — at most 64 bytes, which is why courier puts a
// handle there and keeps the answer in the message resource.
type tgCallbackQuery struct {
	ID      string     `json:"id"`
	From    *tgUser    `json:"from"`
	Message *tgMessage `json:"message"`
	Data    string     `json:"data"`
}

type tgUpdate struct {
	UpdateID      int64            `json:"update_id"`
	Message       *tgMessage       `json:"message"`
	CallbackQuery *tgCallbackQuery `json:"callback_query"`
}

func (a *botAPI) getMe(ctx context.Context) (tgUser, error) {
	var u tgUser
	return u, a.call(ctx, "getMe", url.Values{}, &u)
}

func (a *botAPI) getChat(ctx context.Context, chat string) (tgChat, error) {
	var c tgChat
	return c, a.call(ctx, "getChat", url.Values{"chat_id": {chat}}, &c)
}

// getUpdates long-polls. Plain messages and button presses are subscribed to,
// and nothing else: an edited message is deliberately left out, because
// re-delivering an edited answer would submit a second turn to an agent that
// already acted on the first.
//
// allowed_updates is a whitelist, so a type left out of it is not merely
// ignored — it is never sent. Buttons that draw fine and then do nothing when
// tapped are what this line forgotten looks like.
func (a *botAPI) getUpdates(ctx context.Context, offset int64, timeout int) ([]tgUpdate, error) {
	var ups []tgUpdate
	err := a.call(ctx, "getUpdates", url.Values{
		"offset":          {strconv.FormatInt(offset, 10)},
		"timeout":         {strconv.Itoa(timeout)},
		"allowed_updates": {`["message","callback_query"]`},
	}, &ups)
	return ups, err
}

// sendMessage posts plain text. No parse mode is ever set: agent output is full
// of backticks, underscores and asterisks, and asking Telegram to parse it turns
// a stray character into a 400 that silently drops the message someone was
// waiting for.
func (a *botAPI) sendMessage(ctx context.Context, chatID int64, threadID int, text string, replyTo int, markup string, silent bool) (int, error) {
	params := url.Values{
		"chat_id":                  {strconv.FormatInt(chatID, 10)},
		"text":                     {text},
		"disable_web_page_preview": {"true"},
	}
	if silent {
		params.Set("disable_notification", "true")
	}
	if threadID > 0 {
		params.Set("message_thread_id", strconv.Itoa(threadID))
	}
	if replyTo > 0 {
		params.Set("reply_to_message_id", strconv.Itoa(replyTo))
		params.Set("allow_sending_without_reply", "true")
	}
	if markup != "" {
		params.Set("reply_markup", markup)
	}
	var sent struct {
		MessageID int `json:"message_id"`
	}
	return sent.MessageID, a.call(ctx, "sendMessage", params, &sent)
}

// sendRichMessage posts agent text as Markdown, which is what agents write:
// bold, lists, tables and code arrive formatted instead of as raw asterisks.
// A parse failure is the caller's to handle by falling back to plain text.
func (a *botAPI) sendRichMessage(ctx context.Context, chatID int64, markdown string, replyTo int, markup string, silent bool) (int, error) {
	rich, err := json.Marshal(map[string]any{"markdown": markdown, "skip_entity_detection": false})
	if err != nil {
		return 0, err
	}
	params := url.Values{
		"chat_id":      {strconv.FormatInt(chatID, 10)},
		"rich_message": {string(rich)},
	}
	if silent {
		params.Set("disable_notification", "true")
	}
	if replyTo > 0 {
		params.Set("reply_parameters", fmt.Sprintf(`{"message_id":%d,"allow_sending_without_reply":true}`, replyTo))
	}
	if markup != "" {
		params.Set("reply_markup", markup)
	}
	var sent struct {
		MessageID int `json:"message_id"`
	}
	return sent.MessageID, a.call(ctx, "sendRichMessage", params, &sent)
}

// editRichText rewrites a message with Markdown content and clears its buttons.
func (a *botAPI) editRichText(ctx context.Context, chatID int64, messageID int, markdown string) error {
	rich, err := json.Marshal(map[string]any{"markdown": markdown})
	if err != nil {
		return err
	}
	return a.call(ctx, "editMessageText", url.Values{
		"chat_id":      {strconv.FormatInt(chatID, 10)},
		"message_id":   {strconv.Itoa(messageID)},
		"rich_message": {string(rich)},
		"reply_markup": {`{"inline_keyboard":[]}`},
	}, nil)
}

// editMessageCaption sets the caption of a media message — how a voice post
// shows the words it was heard as.
func (a *botAPI) editMessageCaption(ctx context.Context, chatID int64, messageID int, caption string) error {
	return a.call(ctx, "editMessageCaption", url.Values{
		"chat_id":    {strconv.FormatInt(chatID, 10)},
		"message_id": {strconv.Itoa(messageID)},
		"caption":    {caption},
	}, nil)
}

// pinChatMessage pins a message without notifying anyone.
func (a *botAPI) pinChatMessage(ctx context.Context, chatID int64, messageID int) error {
	return a.call(ctx, "pinChatMessage", url.Values{
		"chat_id":              {strconv.FormatInt(chatID, 10)},
		"message_id":           {strconv.Itoa(messageID)},
		"disable_notification": {"true"},
	}, nil)
}

func (a *botAPI) deleteMessage(ctx context.Context, chatID int64, messageID int) error {
	return a.call(ctx, "deleteMessage", url.Values{
		"chat_id":    {strconv.FormatInt(chatID, 10)},
		"message_id": {strconv.Itoa(messageID)},
	}, nil)
}

// isBadRequest reports a 400: the content was refused, as opposed to the
// transport failing or asking to wait.
func isBadRequest(err error) bool {
	var ae *apiError
	return errors.As(err, &ae) && ae.Code == 400
}

// editMessageText rewrites a message and, with it, always clears any buttons.
//
// Clearing is not a separate decision: every edit courier makes is a message
// becoming final — a question withdrawn, a draft taken — and a button left
// behind on a settled message is one the reader can still press. The empty
// keyboard is sent explicitly rather than relying on an omitted field to mean
// "remove", because that is a behaviour to be read out of prose rather than a
// promise, and the cost of being wrong is a live button on a dead offer.
func (a *botAPI) editMessageText(ctx context.Context, chatID int64, messageID int, text string) error {
	params := url.Values{
		"chat_id":                  {strconv.FormatInt(chatID, 10)},
		"message_id":               {strconv.Itoa(messageID)},
		"text":                     {text},
		"disable_web_page_preview": {"true"},
		"reply_markup":             {`{"inline_keyboard":[]}`},
	}
	err := a.call(ctx, "editMessageText", params, nil)
	if err == nil || !rejectedTheMarkup(err) {
		return err
	}
	// Withdrawing a question is how the fleet works today, and it predates any
	// of this: it must not start failing because an edit now also asks for a
	// keyboard to be cleared. If Telegram will not take the empty keyboard, the
	// text still goes out — omitting reply_markup drops the buttons anyway.
	params.Del("reply_markup")
	return a.call(ctx, "editMessageText", params, nil)
}

// rejectedTheMarkup reports a 400 that is about reply_markup rather than about
// the message.
func rejectedTheMarkup(err error) bool {
	var ae *apiError
	if !errors.As(err, &ae) || ae.Code != 400 {
		return false
	}
	d := strings.ToLower(ae.Description)
	return strings.Contains(d, "reply_markup") || strings.Contains(d, "button") || strings.Contains(d, "keyboard")
}

// answerCallbackQuery closes the loop on a press. Telegram spins a clock on the
// button until this is called, so it is called on every path — including the
// ones that refuse the press.
func (a *botAPI) answerCallbackQuery(ctx context.Context, id, text string, alert bool) error {
	params := url.Values{"callback_query_id": {id}}
	if text != "" {
		params.Set("text", text)
	}
	if alert {
		params.Set("show_alert", "true")
	}
	return a.call(ctx, "answerCallbackQuery", params, nil)
}

func (a *botAPI) setMessageReaction(ctx context.Context, chatID int64, messageID int, emoji string) error {
	reaction := "[]"
	if emoji != "" {
		b, err := json.Marshal([]map[string]string{{"type": "emoji", "emoji": emoji}})
		if err != nil {
			return err
		}
		reaction = string(b)
	}
	return a.call(ctx, "setMessageReaction", url.Values{
		"chat_id":    {strconv.FormatInt(chatID, 10)},
		"message_id": {strconv.Itoa(messageID)},
		"reaction":   {reaction},
	}, nil)
}
