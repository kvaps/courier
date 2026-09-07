package courier

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kvaps/courier/pkg/api"
)

func tempFile(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// Only the path comes from the sender; everything else is observed, so the
// record says what was actually sent rather than what was claimed.
func TestPrepareAttachmentsObservesTheFile(t *testing.T) {
	p := tempFile(t, "shot.png", "not really a png, but bytes")
	got, err := prepareAttachments([]api.Attachment{{Path: p, Size: 999999, MediaType: "text/lies"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d attachments", len(got))
	}
	a := got[0]
	if a.Name != "shot.png" {
		t.Errorf("name = %q", a.Name)
	}
	if a.Size != int64(len("not really a png, but bytes")) {
		t.Errorf("size = %d — the sender's claim was taken instead of the file's", a.Size)
	}
	if a.MediaType != "image/png" {
		t.Errorf("media type = %q", a.MediaType)
	}
	if !filepath.IsAbs(a.Path) {
		t.Errorf("path was not made absolute: %q", a.Path)
	}
}

// Everything that cannot survive the trip is refused now, naming the file,
// rather than failing inside a transport after the text has already gone.
func TestPrepareAttachmentsRefusesWhatCannotBeSent(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.txt")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"missing":   filepath.Join(dir, "nope.txt"),
		"directory": dir,
		"empty":     empty,
		"no path":   "   ",
	}
	for name, path := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := prepareAttachments([]api.Attachment{{Path: path}}); err == nil {
				t.Errorf("%s was accepted", name)
			}
		})
	}
}

func TestPrepareAttachmentsCapsTheCount(t *testing.T) {
	p := tempFile(t, "a.txt", "x")
	many := make([]api.Attachment, maxAttachments+1)
	for i := range many {
		many[i] = api.Attachment{Path: p}
	}
	_, err := prepareAttachments(many)
	if err == nil {
		t.Fatal("an unbounded pile of attachments was accepted")
	}
	if !strings.Contains(err.Error(), "several messages") {
		t.Errorf("the refusal should say what to do instead: %v", err)
	}
}

func TestPrepareAttachmentsEmptyIsNil(t *testing.T) {
	got, err := prepareAttachments(nil)
	if err != nil || got != nil {
		t.Errorf("got %v, %v", got, err)
	}
}

func TestHumanSize(t *testing.T) {
	for in, want := range map[int64]string{
		0:       "0 B",
		512:     "512 B",
		1024:    "1.0 KB",
		1536:    "1.5 KB",
		5 << 20: "5.0 MB",
		3 << 30: "3.0 GB",
	} {
		if got := humanSize(in); got != want {
			t.Errorf("humanSize(%d) = %q, want %q", in, got, want)
		}
	}
}
