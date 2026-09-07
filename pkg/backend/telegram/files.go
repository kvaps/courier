package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Telegram's own limits for bots. They are enforced here rather than left to
// the API, so a file that cannot possibly arrive is refused with a sentence a
// person can act on instead of a 413 from somewhere in the middle.
const (
	maxUploadBytes   = 50 << 20
	maxDownloadBytes = 20 << 20
)

// tgFile is a file as Telegram describes it in an update.
type tgFile struct {
	FileID   string `json:"file_id"`
	UniqueID string `json:"file_unique_id"`
	FileName string `json:"file_name"`
	MimeType string `json:"mime_type"`
	FileSize int64  `json:"file_size"`
}

// tgPhotoSize is one rendition of a photo; Telegram sends several.
type tgPhotoSize struct {
	FileID   string `json:"file_id"`
	UniqueID string `json:"file_unique_id"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	FileSize int64  `json:"file_size"`
}

// sendDocument uploads a file into a thread.
//
// Everything goes as a document, including images. A photo sent as a photo is
// re-encoded and downscaled by Telegram, which is precisely wrong for the thing
// an agent most often sends — a screenshot someone needs to read text off.
func (a *botAPI) sendDocument(ctx context.Context, chatID int64, threadID int, path, name, caption string) (int, error) {
	f, err := os.Open(path) //nolint:gosec // the path is validated by the caller
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()

	// Refused here rather than mid-upload: pushing 200 MB across the network to
	// be told no wastes the operator's bandwidth and the sender's turn.
	if fi, serr := f.Stat(); serr == nil && fi.Size() > maxUploadBytes {
		return 0, fmt.Errorf("%s is %d bytes; Telegram lets a bot upload at most %d", name, fi.Size(), int64(maxUploadBytes))
	}

	// A pipe keeps the upload streaming: a 50 MB attachment must not be
	// assembled in memory first.
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		var werr error
		defer func() { _ = pw.CloseWithError(werr) }()

		fields := map[string]string{"chat_id": strconv.FormatInt(chatID, 10)}
		if threadID > 0 {
			fields["message_thread_id"] = strconv.Itoa(threadID)
		}
		if caption != "" {
			fields["caption"] = caption
		}
		for k, v := range fields {
			if werr = mw.WriteField(k, v); werr != nil {
				return
			}
		}
		var part io.Writer
		if part, werr = mw.CreateFormFile("document", name); werr != nil {
			return
		}
		if _, werr = io.Copy(part, f); werr != nil {
			return
		}
		werr = mw.Close()
	}()

	endpoint := a.base + "/bot" + a.token + "/sendDocument"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, pr)
	if err != nil {
		return 0, a.scrubErr(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := a.client.Do(req)
	if err != nil {
		return 0, a.scrubErr(err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, a.scrubErr(err)
	}
	var env struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
		ErrorCode   int    `json:"error_code"`
		Parameters  struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
		Result struct {
			MessageID int `json:"message_id"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return 0, fmt.Errorf("sendDocument: parse reply (http %d): %w", resp.StatusCode, a.scrubErr(err))
	}
	if !env.OK {
		return 0, &apiError{Code: env.ErrorCode, Description: a.scrub(env.Description), RetryAfter: env.Parameters.RetryAfter}
	}
	return env.Result.MessageID, nil
}

// fetch downloads a file Telegram holds into dir, returning the path it wrote.
//
// The name on disk is built here and never taken from the sender: a name is
// attacker-controlled input, and courier hands these paths to agents.
func (a *botAPI) fetch(ctx context.Context, fileID, suggestedName, dir string) (string, int64, error) {
	var meta struct {
		FilePath string `json:"file_path"`
		FileSize int64  `json:"file_size"`
	}
	if err := a.call(ctx, "getFile", url.Values{"file_id": {fileID}}, &meta); err != nil {
		return "", 0, err
	}
	if meta.FileSize > maxDownloadBytes {
		return "", 0, fmt.Errorf("file is %d bytes; Telegram lets a bot download at most %d", meta.FileSize, int64(maxDownloadBytes))
	}
	if meta.FilePath == "" {
		return "", 0, fmt.Errorf("telegram returned no path for the file")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.base+"/file/bot"+a.token+"/"+meta.FilePath, nil)
	if err != nil {
		return "", 0, a.scrubErr(err)
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return "", 0, a.scrubErr(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("downloading the file failed with http %d", resp.StatusCode)
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", 0, err
	}
	name := safeName(suggestedName, meta.FilePath, fileID)
	dest := filepath.Join(dir, name)
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec // name is sanitised by safeName
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = out.Close() }()

	// Bounded even though getFile reported a size: the size is the sender's
	// claim until the bytes are counted.
	n, err := io.Copy(out, io.LimitReader(resp.Body, maxDownloadBytes+1))
	if err != nil {
		_ = os.Remove(dest)
		return "", 0, err
	}
	if n > maxDownloadBytes {
		_ = os.Remove(dest)
		return "", 0, fmt.Errorf("file exceeded the %d byte download limit", int64(maxDownloadBytes))
	}
	return dest, n, nil
}

// safeName builds the on-disk name for a received file.
//
// The sender's filename is a hint, never a path: it is reduced to its base,
// stripped of anything that could steer a write, and prefixed with the file's
// unique id so two people sending "screenshot.png" do not overwrite each other.
func safeName(suggested, telegramPath, fileID string) string {
	base := filepath.Base(strings.TrimSpace(suggested))
	if base == "" || base == "." || base == ".." || base == string(filepath.Separator) {
		base = filepath.Base(telegramPath)
	}
	cleaned := strings.Map(func(r rune) rune {
		switch {
		case r == '/' || r == '\\' || r == 0:
			return '_'
		case r < 0x20 || r == 0x7f:
			return -1
		default:
			return r
		}
	}, base)
	cleaned = strings.TrimLeft(cleaned, ".")
	if cleaned == "" {
		cleaned = "file"
	}
	if len(cleaned) > 100 {
		cleaned = cleaned[len(cleaned)-100:]
	}
	prefix := fileID
	if len(prefix) > 12 {
		prefix = prefix[len(prefix)-12:]
	}
	prefix = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return r
		}
		return '_'
	}, prefix)
	return prefix + "-" + cleaned
}

// guessMediaType names a file's type from its extension, for the record.
func guessMediaType(path, fallback string) string {
	if t := mime.TypeByExtension(strings.ToLower(filepath.Ext(path))); t != "" {
		return strings.SplitN(t, ";", 2)[0]
	}
	if fallback != "" {
		return fallback
	}
	return "application/octet-stream"
}
