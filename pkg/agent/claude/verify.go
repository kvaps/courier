package claude

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kvaps/courier/pkg/api"
)

// A successful op:reply means the text reached the session's REPL, not that a
// turn started. For a long or multi-line body the REPL collapses the input into
// an unsubmitted bracketed paste that still needs an Enter — so a delivery
// reported on the ack alone can leave a message sitting in an input box, unread,
// while courier tells the operator it arrived. Everything in this file exists to
// close that gap: find the text in the session's own transcript, and press Enter
// if it is not there yet.

// transcriptFiles returns every on-disk transcript for a session id. There is
// normally one, but a session that changed directory mid-run can have records
// under a second project directory, so all are considered.
func transcriptFiles(sid string) []string {
	if sid == "" {
		return nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	matches, _ := filepath.Glob(filepath.Join(home, ".claude", "projects", "*", sid+".jsonl"))
	return matches
}

// mark is a snapshot of a session's transcripts taken immediately before a
// delivery: the size of each file that already existed. Later checks read only
// what was appended past those offsets, so an identical message delivered
// earlier in the conversation cannot be mistaken for this one.
type mark struct {
	sid   string
	sizes map[string]int64
}

func markTranscripts(sid string) mark {
	m := mark{sid: sid, sizes: map[string]int64{}}
	for _, p := range transcriptFiles(sid) {
		if fi, err := os.Stat(p); err == nil {
			m.sizes[p] = fi.Size()
		}
	}
	return m
}

// available reports whether ground truth can be had at all. A session with no
// id has no locatable transcript, and its deliveries fall back to weaker
// evidence rather than being reported as failures.
func (m mark) available() bool { return m.sid != "" }

// grew reports whether any transcript was appended to since the mark. It means
// the session is doing something — a turn writes records continuously — which,
// when our own text has not appeared, means the message is queued behind a turn
// that was already running rather than stuck unsubmitted.
func (m mark) grew() bool {
	if !m.available() {
		return false
	}
	for _, p := range transcriptFiles(m.sid) {
		if fi, err := os.Stat(p); err == nil && fi.Size() > m.sizes[p] {
			return true
		}
	}
	return false
}

// landed reports whether the message appears as a user record in what was
// appended since the mark.
func (m mark) landed(body string) bool {
	needle := jsonNeedle(fragment(body))
	if needle == "" || !m.available() {
		return false
	}
	for _, p := range transcriptFiles(m.sid) {
		chunk, err := readFrom(p, m.sizes[p])
		if err != nil {
			continue
		}
		for _, line := range strings.Split(chunk, "\n") {
			// Tool results are user records too, so the fragment is what
			// identifies our message; requiring both on one record keeps an
			// assistant quoting the text back from counting as delivery.
			if strings.Contains(line, `"type":"user"`) && strings.Contains(line, needle) {
				return true
			}
		}
	}
	return false
}

func readFrom(path string, offset int64) (string, error) {
	f, err := os.Open(path) //nolint:gosec // path came from our own glob of the transcript directory
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return "", err
	}
	b, err := io.ReadAll(f)
	return string(b), err
}

// fragment picks the most distinctive slice of a body to search for: its
// longest single line, capped. Matching a fragment rather than the whole body
// keeps the check robust against the REPL normalising whitespace at the edges
// of a multi-line paste.
func fragment(body string) string {
	best := ""
	for _, ln := range strings.Split(body, "\n") {
		if ln = strings.TrimSpace(ln); len(ln) > len(best) {
			best = ln
		}
	}
	const maxNeedle = 160
	if len(best) > maxNeedle {
		// Cut on a rune boundary so the needle stays valid UTF-8 and escapes
		// the way the transcript encoded it.
		best = strings.ToValidUTF8(best[:maxNeedle], "")
	}
	return best
}

// jsonNeedle renders a fragment as it appears inside a transcript record —
// JSON-escaped, without the surrounding quotes.
//
// HTML escaping is off on purpose. The transcript is written by the CLI's
// JavaScript, which leaves <, > and & literal; Go's encoder escapes them by
// default, so a marshalled needle would never match a message containing a
// shell redirect or a tag — exactly the messages where the check would then
// silently fall back to weaker evidence.
func jsonNeedle(frag string) string {
	if frag == "" {
		return ""
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(frag); err != nil {
		return ""
	}
	s := strings.TrimRight(buf.String(), "\n")
	if len(s) < 2 {
		return ""
	}
	return s[1 : len(s)-1]
}

// pressEnter attaches to a session's PTY and submits whatever is sitting in its
// input box.
//
// Enter is not a neutral keystroke — whatever holds focus consumes it — so the
// caller must have established that no dialog is up and no turn is running
// before calling this. Attach is additive, so it does not disturb a human
// watching the same session.
func (c control) pressEnter(short string) error {
	sock, err := c.socketPath()
	if err != nil {
		return err
	}
	conn, err := net.DialTimeout("unix", sock, 2*time.Second)
	if err != nil {
		return api.NewBackendError("attach to %s: %v", short, err)
	}
	defer func() { _ = conn.Close() }()

	req, err := json.Marshal(map[string]any{
		"proto": 1, "op": "attach", "short": short,
		"cols": 120, "rows": 40, "attachId": nonce(),
		"caps": map[string]any{"terminal": "xterm-256color", "mux": nil, "ssh": false},
	})
	if err != nil {
		return api.NewInternalError("encode attach: %v", err)
	}
	if _, err := conn.Write(append(req, '\n')); err != nil {
		return api.NewBackendError("attach to %s: %v", short, err)
	}

	ackLine, err := readLine(conn, 2*time.Second)
	if err != nil && len(ackLine) == 0 {
		return api.NewBackendError("attach to %s: no ack: %v", short, err)
	}
	var ack struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	_ = json.Unmarshal(ackLine, &ack)
	if !ack.OK {
		return api.NewBackendError("attach to %s rejected: %s", short, ack.Error)
	}
	if _, err := conn.Write([]byte("\r")); err != nil {
		return api.NewBackendError("submit in %s: %v", short, err)
	}
	// A short read so the keystroke is flushed before the connection closes.
	_ = conn.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	buf := make([]byte, 4096)
	_, _ = conn.Read(buf)
	return nil
}

// readLine reads until a newline or the timeout.
func readLine(conn net.Conn, timeout time.Duration) ([]byte, error) {
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	var buf []byte
	tmp := make([]byte, 4096)
	for {
		n, err := conn.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			if i := bytes.IndexByte(buf, '\n'); i >= 0 {
				return buf[:i], nil
			}
		}
		if err != nil {
			return buf, err
		}
	}
}
