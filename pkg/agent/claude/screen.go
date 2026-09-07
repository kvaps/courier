package claude

import (
	"bufio"
	"encoding/json"
	"net"
	"regexp"
	"strings"
	"time"
)

// Enter is a confirmation keystroke, not a neutral one: whatever holds focus
// consumes it. A resumed session can be sitting on the CLI's startup dialog
// offering to compact the conversation, with the compacting option already
// selected — an Enter meant for the input box would accept that instead,
// spending the agent's context and discarding the message that prompted it.
//
// So the screen is read before any Enter, and anything that looks modal stops
// the delivery. The detector is deliberately biased towards seeing a dialog
// that is not there: a false positive costs a message reported as undelivered,
// which the operator can act on, while a false negative costs a conversation.

// snapshot returns the session's current screen as plain text.
func (c control) snapshot(short string, tail int) (string, error) {
	sock, err := c.socketPath()
	if err != nil {
		return "", err
	}
	conn, err := net.DialTimeout("unix", sock, 2*time.Second)
	if err != nil {
		return "", err
	}
	defer func() { _ = conn.Close() }()

	req, err := json.Marshal(map[string]any{"proto": 1, "op": "subscribe", "short": short, "tail": tail})
	if err != nil {
		return "", err
	}
	if _, err := conn.Write(append(req, '\n')); err != nil {
		return "", err
	}
	_ = conn.SetReadDeadline(time.Now().Add(4 * time.Second))

	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		var frame struct {
			Type       string   `json:"type"`
			StreamTail []string `json:"streamTail"`
			Error      string   `json:"error"`
		}
		if json.Unmarshal(sc.Bytes(), &frame) != nil {
			continue
		}
		if frame.Type == "snapshot" {
			return stripANSI(strings.Join(frame.StreamTail, "")), nil
		}
	}
	return "", nil
}

var ansi = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[@-Z\\-_]`)

// stripANSI removes escape sequences so the screen can be scanned as text.
func stripANSI(s string) string {
	s = ansi.ReplaceAllString(s, "")
	return strings.ReplaceAll(s, "\r", "")
}

// numberedOption matches a rendered choice line such as "❯ 1. Continue".
var numberedOption = regexp.MustCompile(`(?m)^\s*[│|]?\s*[❯>]?\s*\d\.\s+\S`)

// modalPhrases are wordings the CLI uses on its blocking dialogs. They are
// matched case-insensitively against the last screenful.
var modalPhrases = []string{
	"do you want to",
	"continue with",
	"compact",
	"esc to cancel",
	"press enter to confirm",
}

// dialogUp reports whether the session's screen looks like it is holding a
// modal that would eat an Enter.
//
// A rendered choice needs two things to count: a numbered option and the box
// the CLI draws around a real dialog. A printed list in an assistant's answer
// has the numbers but not the frame, and treating that as a dialog would wedge
// every delivery into a session that happened to be showing one.
func dialogUp(screen string) bool {
	if screen == "" {
		return false
	}
	tail := lastLines(screen, 40)
	framed := strings.ContainsAny(tail, "╭╮╰╯│")
	if framed && numberedOption.MatchString(tail) {
		return true
	}
	low := strings.ToLower(tail)
	for _, p := range modalPhrases {
		if framed && strings.Contains(low, p) {
			return true
		}
	}
	return false
}

func lastLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
