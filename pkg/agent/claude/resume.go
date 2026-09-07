package claude

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kvaps/courier/pkg/api"
)

// jobState is the part of ~/.claude/jobs/<short>/state.json needed to bring a
// stopped session back exactly as the agents view does.
type jobState struct {
	SessionID       string   `json:"sessionId"`
	ResumeSessionID string   `json:"resumeSessionId"`
	RespawnFlags    []string `json:"respawnFlags"`
	Cwd             string   `json:"cwd"`
	LinkScanPath    string   `json:"linkScanPath"`
	Name            string   `json:"name"`
	Intent          string   `json:"intent"`
	State           string   `json:"state"`
	Detail          string   `json:"detail"`
}

func jobsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", api.NewInternalError("resolve home directory: %v", err)
	}
	return filepath.Join(home, ".claude", "jobs"), nil
}

// readJobState loads a session's on-disk state. A session with no job directory
// returns NotFound: it is not resumable by this route.
func readJobState(short string) (*jobState, error) {
	dir, err := jobsDir()
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(dir, short, "state.json")) //nolint:gosec // path built from a validated short id
	if err != nil {
		if os.IsNotExist(err) {
			return nil, api.NewNotFound("session job state", short)
		}
		return nil, api.NewBackendError("read job state for %s: %v", short, err)
	}
	var js jobState
	if err := json.Unmarshal(b, &js); err != nil {
		return nil, api.NewBackendError("parse job state for %s: %v", short, err)
	}
	return &js, nil
}

func (js *jobState) resumeID() string {
	if js.ResumeSessionID != "" {
		return js.ResumeSessionID
	}
	return js.SessionID
}

// cwdGone reports whether a session's saved working directory has disappeared,
// which is the most common reason a resume crashes — usually a deleted
// worktree. Checking first turns a crash-loop into a clear message.
func cwdGone(cwd string) bool {
	if strings.TrimSpace(cwd) == "" {
		return false
	}
	fi, err := os.Stat(cwd)
	return err != nil || !fi.IsDir()
}

// wakeable reports whether a stopped session can be brought back in place.
func wakeable(short string) bool {
	js, err := readJobState(short)
	if err != nil {
		return false
	}
	return js.resumeID() != "" && !cwdGone(js.Cwd)
}

// findTranscript locates the session's transcript for the dispatch descriptor.
//
// This is not optional. The resumed worker resolves `--resume <sessionId>`
// against the project directory derived from its launch cwd, so a session whose
// transcript lives under a different project directory — typically one that
// moved into a worktree mid-run — exits at startup with "No conversation found"
// and crash-loops, even though the same session resumes fine from the agents
// view. The view avoids it by passing the path explicitly; so does this.
//
// An empty result is fine for a session that has never been prompted: the
// worker then falls back to its own lookup.
func findTranscript(js *jobState, sid string) string {
	if p := js.LinkScanPath; p != "" && filepath.Base(p) == sid+".jsonl" {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	var newest string
	var newestMod time.Time
	for _, m := range transcriptFiles(sid) {
		if fi, err := os.Stat(m); err == nil && fi.ModTime().After(newestMod) {
			newest, newestMod = m, fi.ModTime()
		}
	}
	return newest
}

func nonce() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "00000000"
	}
	return hex.EncodeToString(b[:])
}

// wake brings a stopped session back under its own short id, with its history,
// and waits until it is genuinely ready to receive input.
//
// It uses op:dispatch with launch.mode "resume" — what pressing Enter on a
// session in the agents view does. The alternative, `claude --bg --resume`,
// forks: it spawns a worker under a fresh short id and leaves the original
// behind as a duplicate, which is not what "wake this agent" should mean.
func (c control) wake(short string) error {
	js, err := readJobState(short)
	if err != nil {
		return err
	}
	sid := js.resumeID()
	if sid == "" {
		return api.NewInvalid("session %s has no session id on disk and cannot be resumed", short)
	}
	if cwdGone(js.Cwd) {
		return api.NewInvalid("session %s cannot be woken: its working directory %s no longer exists (a deleted worktree is the usual cause)", short, js.Cwd)
	}
	key, err := c.key()
	if err != nil {
		return err
	}

	launch := map[string]any{
		"mode":      "resume",
		"sessionId": sid,
		"fork":      false,
		"flagArgs":  js.RespawnFlags,
	}
	if p := findTranscript(js, sid); p != "" {
		launch["transcriptPath"] = p
	}
	desc := map[string]any{
		"proto":        1,
		"short":        short,
		"nonce":        nonce(),
		"sessionId":    js.SessionID,
		"createdAt":    time.Now().UnixMilli(),
		"source":       "courier",
		"cwd":          js.Cwd,
		"launch":       launch,
		"env":          map[string]any{},
		"isolation":    "none",
		"respawnFlags": js.RespawnFlags,
		"seed":         map[string]any{"intent": js.Intent, "name": js.Name},
	}

	raw, err := c.request(map[string]any{
		"proto": 1, "op": "dispatch", "d": desc, "timeoutMs": 5000, "auth": key,
	})
	if err != nil {
		return err
	}
	var resp struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
		Short string `json:"short"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return api.NewBackendError("parse dispatch reply: %v", err)
	}
	if !resp.OK {
		return api.NewBackendError("daemon refused to wake %s: %s", short, resp.Error)
	}
	return c.awaitAwake(short, 30*time.Second, 2*time.Second)
}

// awaitAwake waits until a woken session holds a normal state for a settle
// window. A resume replays history first, so a worker can sit in "resuming" for
// several seconds; delivering into that window races the boot.
func (c control) awaitAwake(short string, timeout, settle time.Duration) error {
	deadline := time.Now().Add(timeout)
	seen := false
	var steadySince time.Time
	var last session
	for {
		if jobs, err := c.roster(); err == nil {
			found := false
			for _, j := range jobs {
				if j.Short == short {
					last, found, seen = j, true, true
					break
				}
			}
			switch {
			case found && !last.booting() && last.State != "":
				if steadySince.IsZero() {
					steadySince = time.Now()
				}
				if time.Since(steadySince) >= settle {
					return nil
				}
			case found:
				steadySince = time.Time{} // still booting; restart the window
			case seen:
				// It appeared and then left the roster: a resume that crashed
				// during startup. The daemon does not retry those.
				return api.NewBackendError("session %s exited while waking up%s", short, reason(last.Detail))
			}
		}
		if time.Now().After(deadline) {
			if seen {
				return api.NewTimeout("session %s did not settle within %s (last state %q)", short, timeout, last.State)
			}
			return api.NewTimeout("session %s never registered with the daemon within %s", short, timeout)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func reason(detail string) string {
	if strings.TrimSpace(detail) == "" {
		return ""
	}
	return ": " + strings.TrimSpace(detail)
}
