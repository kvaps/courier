package courier

import (
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"strings"

	"github.com/kvaps/courier/pkg/api"
)

// maxAttachments bounds one message. It is a courtesy limit, not a transport
// one: a person reading on a phone can act on a couple of files, and a dozen
// arriving at once is a directory listing, not a message.
const maxAttachments = 10

// prepareAttachments validates what a sender asked to send and fills in what it
// left out.
//
// Only Path comes from the caller. Everything else is observed here, so the
// record says what was actually sent rather than what the sender claimed — and
// so a file that will not survive the trip is refused now, with a sentence
// naming it, instead of failing somewhere inside a transport.
func prepareAttachments(in []api.Attachment) ([]api.Attachment, error) {
	if len(in) == 0 {
		return nil, nil
	}
	if len(in) > maxAttachments {
		return nil, api.NewInvalid("%d attachments is too many for one message (limit %d) — send them as several messages", len(in), maxAttachments)
	}

	out := make([]api.Attachment, 0, len(in))
	for _, a := range in {
		path := strings.TrimSpace(a.Path)
		if path == "" {
			return nil, api.NewInvalid("an attachment has no path")
		}
		abs, err := filepath.Abs(path)
		if err != nil {
			return nil, api.NewInvalid("attachment %q: %v", path, err)
		}
		// Stat, not Lstat: a symlink to a real file is a fine thing to send,
		// and following it is what the reader would expect.
		fi, err := os.Stat(abs)
		switch {
		case os.IsNotExist(err):
			return nil, api.NewInvalid("attachment %q does not exist", abs)
		case err != nil:
			return nil, api.NewInvalid("attachment %q: %v", abs, err)
		case fi.IsDir():
			return nil, api.NewInvalid("attachment %q is a directory — send a file, or archive it first", abs)
		case !fi.Mode().IsRegular():
			return nil, api.NewInvalid("attachment %q is not a regular file", abs)
		case fi.Size() == 0:
			return nil, api.NewInvalid("attachment %q is empty", abs)
		}
		// Readability is checked now rather than discovered mid-upload, where
		// the failure would arrive after the message text had already been sent.
		f, err := os.Open(abs) //nolint:gosec // an operator-supplied path is the point of this feature
		if err != nil {
			return nil, api.NewInvalid("attachment %q cannot be read: %v", abs, err)
		}
		_ = f.Close()

		name := strings.TrimSpace(a.Name)
		if name == "" {
			name = filepath.Base(abs)
		}
		// Observed, never taken from the caller: a media type is a claim about
		// the bytes, and the record must not repeat a claim it did not check.
		// Name is different — that is presentation, and the sender may choose it.
		mediaType := "application/octet-stream"
		if t := mime.TypeByExtension(strings.ToLower(filepath.Ext(abs))); t != "" {
			mediaType = strings.SplitN(t, ";", 2)[0]
		}
		out = append(out, api.Attachment{
			Path:      abs,
			Name:      filepath.Base(name),
			Size:      fi.Size(),
			MediaType: mediaType,
		})
	}
	return out, nil
}

// humanSize renders a byte count the way a person reads one.
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit && exp < 3; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
}
