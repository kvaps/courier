// Package claude delivers courier messages into local Claude Code background
// sessions.
//
// It speaks the Claude Code daemon's control socket directly — the same socket
// and the same ops the `claude` CLI uses to hand text to a background session.
// Two properties make this the right channel and not merely a working one.
//
// It needs no approval. The daemon's reply handler either delivers or returns
// one of ENOJOB / ERESPAWNING / ENOREPLY; it raises no dialog. The approval
// prompt an operator may have seen ("approve message from uds:/tmp/cc-socks/…")
// belongs to a different path entirely — Claude Code's peer-to-peer session
// inbox, which gates unknown writers on purpose. Courier deliberately does not
// speak that protocol: a channel the operator has to approve message by message
// is not a channel.
//
// And it reaches sessions that are not running. A stopped-but-resumable session
// is woken in place first, keeping its history, so a message sent to a parked
// agent is delivered rather than refused.
//
// The only credential involved is ~/.claude/daemon/control.key, a local file
// owned by the user courier runs as.
package claude

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kvaps/courier/pkg/api"
)

// control is a connection factory for the local daemon's control socket. The
// socket path is resolved on every call rather than cached, so the sink
// survives a daemon restart without being restarted itself.
type control struct{}

// socketPath returns the freshest control socket belonging to this user.
func (control) socketPath() (string, error) {
	pattern := fmt.Sprintf("/tmp/cc-daemon-%d/*/control.sock", os.Getuid())
	matches, _ := filepath.Glob(pattern)
	if len(matches) == 0 {
		return "", api.NewBackendError("no Claude Code daemon socket matching %s — is `claude agents` running?", pattern)
	}
	sort.Slice(matches, func(i, j int) bool {
		fi, ei := os.Stat(matches[i])
		fj, ej := os.Stat(matches[j])
		if ei != nil || ej != nil {
			return false
		}
		return fi.ModTime().After(fj.ModTime())
	})
	return matches[0], nil
}

// key reads the daemon's control-authentication key. Only the mutating ops
// (reply, dispatch) require it; list and attach do not.
func (control) key() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", api.NewInternalError("resolve home directory: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(home, ".claude", "daemon", "control.key")) //nolint:gosec // fixed path under the user's own home
	if err != nil {
		return "", api.NewBackendError("read the Claude Code daemon control key: %v", err)
	}
	return strings.TrimSpace(string(b)), nil
}

// request sends one newline-framed JSON request and returns the reply line.
func (c control) request(payload map[string]any) ([]byte, error) {
	sock, err := c.socketPath()
	if err != nil {
		return nil, err
	}
	conn, err := net.DialTimeout("unix", sock, 3*time.Second)
	if err != nil {
		return nil, api.NewBackendError("dial the Claude Code daemon: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetDeadline(time.Now().Add(8 * time.Second)); err != nil {
		return nil, api.NewBackendError("set deadline: %v", err)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, api.NewInternalError("encode control request: %v", err)
	}
	if _, err := conn.Write(append(body, '\n')); err != nil {
		return nil, api.NewBackendError("write to the daemon: %v", err)
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil && len(line) == 0 {
		return nil, api.NewBackendError("read from the daemon: %v", err)
	}
	return []byte(line), nil
}

// session is one row of the daemon roster (op:list).
type session struct {
	Short     string `json:"short"`
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd"`
	Backend   string `json:"backend"`
	Tempo     string `json:"tempo"`
	State     string `json:"state"`
	Detail    string `json:"detail"`
	Name      string `json:"name"`
	Needs     string `json:"needs"`
}

// busy reports whether the session is running a turn right now.
//
// It is deliberately narrow. An ordinary session runs a turn as
// state=="running", tempo=="active" — indistinguishable from one sitting idle
// at its prompt — so only state=="working", which the daemon sets for goal and
// loop sessions, is trusted here. Everything else is settled from the
// transcript, which cannot be misread the same way.
func (s session) busy() bool { return s.State == "working" }

// booting reports a session that is replaying history or crashed mid-startup —
// transient states to wait through rather than deliver into.
func (s session) booting() bool { return s.State == "resuming" || s.State == "crashed" }

// roster returns the live sessions the daemon knows about.
func (c control) roster() ([]session, error) {
	raw, err := c.request(map[string]any{"proto": 1, "op": "list"})
	if err != nil {
		return nil, err
	}
	var resp struct {
		OK    bool      `json:"ok"`
		Error string    `json:"error"`
		Jobs  []session `json:"jobs"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, api.NewBackendError("parse the daemon roster: %v", err)
	}
	if !resp.OK {
		return nil, api.NewBackendError("daemon refused list: %s", resp.Error)
	}
	return resp.Jobs, nil
}

// find looks a session up in the live roster by short id, session id or name.
func (c control) find(ref string) (session, bool) {
	jobs, err := c.roster()
	if err != nil {
		return session{}, false
	}
	for _, j := range jobs {
		if strings.EqualFold(j.Short, ref) || strings.EqualFold(j.SessionID, ref) || strings.EqualFold(j.Name, ref) {
			return j, true
		}
	}
	for _, j := range jobs {
		if strings.HasPrefix(j.Short, ref) || strings.HasPrefix(j.SessionID, ref) {
			return j, true
		}
	}
	return session{}, false
}

// reply hands text to a running session's REPL over the daemon's native op.
//
// It mirrors the CLI's own sender by retrying the two transient refusals: the
// worker still coming up (ESTARTING) and momentarily not accepting input
// (ENOREPLY). EAUTH means the daemon restarted and rotated its key, so the key
// is re-read once. Anything else is returned for the caller to fall back on.
//
// A successful ack means the text reached the REPL — not that a turn started.
// That distinction is the whole of deliver.go.
func (c control) reply(short, text string) error {
	if strings.TrimSpace(short) == "" {
		return api.NewInvalid("no session to reply to")
	}
	key, err := c.key()
	if err != nil {
		return err
	}
	refreshed := false
	for attempt := 0; attempt < 12; attempt++ {
		raw, reqErr := c.request(map[string]any{
			"proto": 1, "op": "reply", "short": short, "text": text, "auth": key,
		})
		if reqErr != nil {
			return reqErr
		}
		var resp struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
			Code  string `json:"code"`
		}
		if err := json.Unmarshal(raw, &resp); err != nil {
			return api.NewBackendError("parse reply response: %v", err)
		}
		if resp.OK {
			return nil
		}
		switch resp.Code {
		case "ESTARTING", "ENOREPLY", "ERESPAWNING":
			time.Sleep(200 * time.Millisecond)
		case "EAUTH":
			if refreshed {
				return api.NewBackendError("daemon rejected the control key: %s", resp.Error)
			}
			k, kerr := c.key()
			if kerr != nil {
				return kerr
			}
			key, refreshed = k, true
		case "ENOJOB":
			return api.NewNotFound("session", short)
		default:
			return api.NewBackendError("daemon refused reply: %s", resp.Error)
		}
	}
	return api.NewBackendError("session %s did not accept the message after 12 attempts", short)
}
