package telegram

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// heardIn and heardOut mirror the tool courier calls. They are written out here
// rather than imported so that a change on the other side shows up as a failing
// test instead of a voice message that quietly stops arriving.
type heardIn struct {
	Peer        string `json:"peer"`
	MessageID   int    `json:"messageId"`
	WaitSeconds int    `json:"waitSeconds,omitempty"`
}

type heardOut struct {
	Status    string `json:"status"`
	MessageID int    `json:"messageId,omitempty"`
	Text      string `json:"text,omitempty"`
	Output    string `json:"output,omitempty"`
}

// transcribeStub is an MCP server that answers the one tool courier asks for.
func transcribeStub(t *testing.T, reply func(heardIn) (heardOut, error)) (*transcriber, *[]heardIn) {
	t.Helper()
	var asked []heardIn

	srv := mcp.NewServer(&mcp.Implementation{Name: "mcp-tg-stub", Version: "test"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: transcribeTool, Description: "stub"},
		func(_ context.Context, _ *mcp.CallToolRequest, in heardIn) (*mcp.CallToolResult, heardOut, error) {
			asked = append(asked, in)
			out, err := reply(in)
			if err != nil {
				return &mcp.CallToolResult{IsError: true}, heardOut{}, err
			}
			return nil, out, nil
		})

	http := httptest.NewServer(mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return srv }, nil))
	t.Cleanup(http.Close)

	return &transcriber{endpoint: http.URL, wait: 1}, &asked
}

// The words come back, and the message is named the way the transport names it.
func TestTranscriberAsksForOneMessageAndReadsTheWords(t *testing.T) {
	hear, asked := transcribeStub(t, func(heardIn) (heardOut, error) {
		return heardOut{Status: transcribedOK, Text: "  оставь одно предложение  "}, nil
	})

	got, err := hear.heard(context.Background(), -100123, 42)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != transcribedOK || got.Text != "  оставь одно предложение  " {
		t.Fatalf("result = %+v", got)
	}
	if len(*asked) != 1 {
		t.Fatalf("the tool was called %d times", len(*asked))
	}
	if (*asked)[0].Peer != "-100123" || (*asked)[0].MessageID != 42 {
		t.Errorf("asked for %+v — the message must be named by chat and id", (*asked)[0])
	}
	if (*asked)[0].WaitSeconds != 1 {
		t.Errorf("waitSeconds = %d, want the configured wait", (*asked)[0].WaitSeconds)
	}
}

// Every status that carries no words has a sentence for the person who spoke,
// because they are the only one who can do anything about it.
func TestEveryWordlessStatusExplainsItself(t *testing.T) {
	for _, status := range []string{transcribedPending, transcribedPremiumRequired, transcribedNotAudio, transcribedFailed, "something new"} {
		why := result{Status: status}.why()
		if strings.TrimSpace(why) == "" {
			t.Errorf("status %q has nothing to say for itself", status)
		}
	}
	if !strings.Contains(result{Status: transcribedPremiumRequired}.why(), "Premium") {
		t.Error("the one status an operator can actually fix does not name the fix")
	}
}

func TestTranscriberReportsARefusal(t *testing.T) {
	hear, _ := transcribeStub(t, func(heardIn) (heardOut, error) {
		return heardOut{}, context.Canceled
	})
	if _, err := hear.heard(context.Background(), -100123, 42); err == nil {
		t.Fatal("a refused call was read as a transcription")
	}
}

// End to end through the receive path: a voice message becomes words, and it is
// marked as heard rather than typed.
func TestAVoiceMessageArrivesAsWords(t *testing.T) {
	tg, _ := connected(t, map[string]any{"chat": "-100123"})
	tg.hear, _ = transcribeStub(t, func(heardIn) (heardOut, error) {
		return heardOut{Status: transcribedOK, Text: "оставь одно предложение"}, nil
	})
	sink := &recordingSink{}

	tg.handle(context.Background(), sink, tgUpdate{
		UpdateID: 1,
		Message: &tgMessage{
			MessageID: 42, ThreadID: 7, Date: 1,
			From:  &tgUser{ID: 11, Username: "kvaps"},
			Chat:  tgChat{ID: -100123},
			Voice: &tgFile{FileID: "v", MimeType: "audio/ogg"},
		},
	})

	got := sink.inbound()
	if len(got) != 1 {
		t.Fatalf("messages delivered = %d", len(got))
	}
	if got[0].Text != "оставь одно предложение" {
		t.Errorf("text = %q", got[0].Text)
	}
	if !got[0].Spoken {
		t.Error("the words are not marked as heard, so nothing downstream knows to double-take on them")
	}
	if len(got[0].Files) != 0 {
		t.Errorf("the audio was downloaded as well: %+v", got[0].Files)
	}
}

// A voice message that could not be transcribed carries nothing, and the person
// who spoke is told so where they spoke. Silence would leave them believing
// they had answered.
func TestAVoiceMessageThatCannotBeHeardIsNotDelivered(t *testing.T) {
	tg, stub := connected(t, map[string]any{"chat": "-100123"})
	tg.hear, _ = transcribeStub(t, func(heardIn) (heardOut, error) {
		return heardOut{Status: transcribedPremiumRequired}, nil
	})
	sink := &recordingSink{}

	tg.handle(context.Background(), sink, tgUpdate{
		UpdateID: 1,
		Message: &tgMessage{
			MessageID: 42, ThreadID: 7, Date: 1,
			From:  &tgUser{ID: 11, Username: "kvaps"},
			Chat:  tgChat{ID: -100123},
			Voice: &tgFile{FileID: "v"},
		},
	})

	if got := sink.inbound(); len(got) != 0 {
		t.Fatalf("a voice message with no words was delivered anyway: %+v", got)
	}
	sends := stub.calledWith("sendMessage")
	if len(sends) != 1 {
		t.Fatalf("the speaker was not told: %v", sends)
	}
	if !strings.Contains(sends[0], "Premium") || !strings.Contains(sends[0], "message_thread_id=7") {
		t.Errorf("the note is wrong or in the wrong topic: %s", sends[0])
	}
}

// Without a transcriber a voice message is not carried at all, and that is said
// out loud rather than left as a message that never arrives.
func TestWithoutATranscriberAVoiceMessageIsRefusedOutLoud(t *testing.T) {
	tg, stub := connected(t, map[string]any{"chat": "-100123"})
	sink := &recordingSink{}

	tg.handle(context.Background(), sink, tgUpdate{
		UpdateID: 1,
		Message: &tgMessage{
			MessageID: 42, ThreadID: 7, Date: 1,
			From:  &tgUser{ID: 11, Username: "kvaps"},
			Chat:  tgChat{ID: -100123},
			Voice: &tgFile{FileID: "v"},
		},
	})

	if got := sink.inbound(); len(got) != 0 {
		t.Fatalf("delivered = %+v", got)
	}
	if sends := stub.calledWith("sendMessage"); len(sends) != 1 {
		t.Fatalf("the speaker was not told: %v", sends)
	}
}

func TestTranscribeConfigRefusesAnEndpointlessSetting(t *testing.T) {
	if _, err := newTranscriber(&TranscribeConfig{}); err == nil {
		t.Fatal("a transcriber with nowhere to ask was accepted")
	}
	if _, err := newTranscriber(&TranscribeConfig{Endpoint: "http://x", WaitSeconds: maxTranscribeWait + 1}); err == nil {
		t.Fatal("a wait longer than the tool accepts was allowed to stall the receive loop")
	}
	none, err := newTranscriber(nil)
	if err != nil || none != nil {
		t.Errorf("no transcription configured should be no transcriber and no error: %v %v", none, err)
	}
}
