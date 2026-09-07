package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/kvaps/courier/pkg/api"
)

// Watch is chunked JSON, one event per line — the Kubernetes wire format, and
// the one that needs no dependency and no protocol upgrade: any client that can
// read a streaming HTTP body can follow it, curl included.
//
// A client that lists first and then watches from the list's resourceVersion
// sees every later change exactly once. A version the store no longer retains
// is refused with a Conflict rather than served a stream with a silent hole in
// it, and the client re-lists.

// streamIfWatch handles ?watch=true on a list endpoint, returning whether it
// took over the response.
func (s *Server) streamIfWatch(w http.ResponseWriter, r *http.Request, kind string) bool {
	if r.URL.Query().Get("watch") != "true" {
		return false
	}
	s.stream(w, r, kind)
	return true
}

func (s *Server) watchAll(w http.ResponseWriter, r *http.Request) {
	s.stream(w, r, r.URL.Query().Get("kind"))
}

func (s *Server) stream(w http.ResponseWriter, r *http.Request, kind string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, api.NewInternalError("this connection cannot stream"))
		return
	}
	since, err := parseVersion(r.URL.Query().Get("resourceVersion"))
	if err != nil {
		writeErr(w, err)
		return
	}
	var kinds []string
	if kind != "" {
		kinds = []string{kind}
	}
	events, cancel, err := s.svc.Store().Watch(since, kinds...)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer cancel()

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	enc := json.NewEncoder(w)
	// A keepalive so an idle stream does not look dead to anything counting
	// bytes between here and the client.
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if _, err := w.Write([]byte("\n")); err != nil {
				return
			}
			flusher.Flush()
		case ev, open := <-events:
			if !open {
				// The store dropped this watcher for falling behind. Say so in
				// the stream's own vocabulary so the client re-lists instead of
				// assuming nothing has happened since.
				status := api.StatusOf(api.NewConflictf(
					"this watch fell too far behind and was closed — list again and watch from the new resourceVersion"))
				raw, merr := json.Marshal(status)
				if merr != nil {
					return
				}
				_ = enc.Encode(api.WatchEvent{Type: api.Error, Object: raw})
				flusher.Flush()
				return
			}
			if err := enc.Encode(api.WatchEvent{Type: ev.Type, Object: ev.Object}); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func parseVersion(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil || v < 0 {
		return 0, api.NewBadRequest("resourceVersion %q is not a version", s)
	}
	return v, nil
}
