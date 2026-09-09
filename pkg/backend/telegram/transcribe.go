package telegram

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kvaps/courier/pkg/api"
)

// Transcription defaults and limits, matching what the tool on the other end
// accepts. Telegram transcribes asynchronously and the tool waits for the
// result, so this wait is time the receive loop spends standing still.
const (
	transcribeTool        = "tg_messages_transcribe_audio"
	defaultTranscribeWait = 20
	maxTranscribeWait     = 120
)

// Statuses the transcription tool reports. Only one of them carries words.
const (
	transcribedOK              = "completed"
	transcribedPending         = "pending"
	transcribedPremiumRequired = "premium_required"
	transcribedNotAudio        = "not_transcribable"
	transcribedFailed          = "failed"
)

// TranscribeConfig points the channel at something that can turn a voice
// message into words.
//
// It is an MCP server rather than a library because the capability is not the
// bot's to have: Telegram transcribes for a user account, and courier is a bot.
// The account already has a client on this machine; courier borrows it over MCP
// instead of opening a second Telegram session, which is not a style choice —
// two clients on one account is how you earn AUTH_KEY_DUPLICATED.
type TranscribeConfig struct {
	// Endpoint is the MCP server's streamable-HTTP address, e.g.
	// http://127.0.0.1:8787/mcp.
	Endpoint string `json:"endpoint"`
	// WaitSeconds is how long to wait for Telegram to finish. It is the receive
	// loop's own time, so it is short by default: a note that has not been
	// transcribed in twenty seconds is better reported than waited on.
	WaitSeconds int `json:"waitSeconds,omitempty"`
}

// transcriber turns a voice message into words by asking an MCP server.
type transcriber struct {
	endpoint string
	wait     int
}

func newTranscriber(cfg *TranscribeConfig) (*transcriber, error) {
	if cfg == nil {
		return nil, nil //nolint:nilnil // no transcriber configured is not a failure
	}
	if cfg.Endpoint == "" {
		return nil, api.NewInvalid("telegram transcribe: endpoint is required (e.g. http://127.0.0.1:8787/mcp)")
	}
	wait := cfg.WaitSeconds
	if wait <= 0 {
		wait = defaultTranscribeWait
	}
	if wait > maxTranscribeWait {
		return nil, api.NewInvalid("telegram transcribe: waitSeconds is %d; %d is the most the tool accepts, "+
			"and it is the receive loop's own time", wait, maxTranscribeWait)
	}
	return &transcriber{endpoint: cfg.Endpoint, wait: wait}, nil
}

// result is what came back: the words, and why there are none when there are none.
type result struct {
	Status string `json:"status"`
	Text   string `json:"text"`
}

// heard asks for the words of one voice message, named the way the transport
// names it: a chat and a message id, never anything read out of the audio.
//
// A session is opened per call and closed again. Transcriptions are rare, the
// server is on loopback, and a handshake costs a millisecond — whereas a kept
// session is state that goes stale exactly when it is least convenient, when
// the server on the other end was restarted and courier is holding a
// connection to something that no longer exists.
func (t *transcriber) heard(ctx context.Context, chatID int64, messageID int) (result, error) {
	// The tool's own wait plus room for the handshake and the reply, so the
	// deadline here never fires before the tool has had its full say.
	ctx, cancel := context.WithTimeout(ctx, time.Duration(t.wait+15)*time.Second)
	defer cancel()

	client := mcp.NewClient(&mcp.Implementation{Name: "courier", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: t.endpoint}, nil)
	if err != nil {
		return result{}, api.NewBackendError("transcription: connect to %s: %v", t.endpoint, err)
	}
	defer func() { _ = session.Close() }()

	out, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: transcribeTool,
		Arguments: map[string]any{
			"peer":        strconv.FormatInt(chatID, 10),
			"messageId":   messageID,
			"waitSeconds": t.wait,
		},
	})
	if err != nil {
		return result{}, api.NewBackendError("transcription: %s: %v", transcribeTool, err)
	}
	if out.IsError {
		return result{}, api.NewBackendError("transcription: %s refused: %s", transcribeTool, contentText(out))
	}

	var res result
	raw, err := json.Marshal(out.StructuredContent)
	if err != nil {
		return result{}, api.NewBackendError("transcription: re-read the result: %v", err)
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return result{}, api.NewBackendError("transcription: the result is not the shape %s promises: %v", transcribeTool, err)
	}
	return res, nil
}

// why turns a status with no words into a sentence for the person who spoke.
// They are the only one who can do anything about any of these.
func (r result) why() string {
	switch r.Status {
	case transcribedPending:
		return "Telegram is still working on it — say it again, or write it out"
	case transcribedPremiumRequired:
		return "transcription needs Telegram Premium on the account courier asks through"
	case transcribedNotAudio:
		return "Telegram will not transcribe this one"
	case transcribedFailed:
		return "Telegram could not make it out"
	default:
		return "no words came back (" + r.Status + ")"
	}
}

// contentText pulls whatever text a tool result carries, for an error message.
func contentText(out *mcp.CallToolResult) string {
	for _, c := range out.Content {
		if tc, ok := c.(*mcp.TextContent); ok && tc.Text != "" {
			return tc.Text
		}
	}
	return "no detail"
}
