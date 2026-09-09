package server

import (
	"net/http"
	"strconv"
	"time"

	"github.com/kvaps/courier/pkg/api"
	"github.com/kvaps/courier/pkg/courier"
)

func (s *Server) listChannels(w http.ResponseWriter, r *http.Request) {
	if s.streamIfWatch(w, r, api.KindChannel) {
		return
	}
	list, err := s.svc.ListChannels()
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) createChannel(w http.ResponseWriter, r *http.Request) {
	var ch api.Channel
	if err := decode(r, &ch); err != nil {
		writeErr(w, err)
		return
	}
	created, err := s.svc.CreateChannel(r.Context(), &ch)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) getChannel(w http.ResponseWriter, r *http.Request) {
	ch, err := s.svc.GetChannel(r.PathValue("name"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ch)
}

func (s *Server) deleteChannel(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.DeleteChannel(r.PathValue("name")); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listConversations(w http.ResponseWriter, r *http.Request) {
	if s.streamIfWatch(w, r, api.KindConversation) {
		return
	}
	list, err := s.svc.ListConversations()
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) createConversation(w http.ResponseWriter, r *http.Request) {
	var c api.Conversation
	if err := decode(r, &c); err != nil {
		writeErr(w, err)
		return
	}
	created, err := s.svc.OpenConversation(r.Context(), &c)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) getConversation(w http.ResponseWriter, r *http.Request) {
	c, err := s.svc.GetConversation(r.PathValue("name"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) closeConversation(w http.ResponseWriter, r *http.Request) {
	c, err := s.svc.CloseConversation(r.Context(), r.PathValue("name"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) deleteConversation(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.DeleteConversation(r.PathValue("name")); err != nil {
		writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listMessages(w http.ResponseWriter, r *http.Request) {
	if s.streamIfWatch(w, r, api.KindMessage) {
		return
	}
	list, err := s.svc.ListMessages(r.URL.Query().Get("conversation"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) createMessage(w http.ResponseWriter, r *http.Request) {
	var m api.Message
	if err := decode(r, &m); err != nil {
		writeErr(w, err)
		return
	}
	sent, err := s.svc.Send(r.Context(), &m)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, sent)
}

func (s *Server) getMessage(w http.ResponseWriter, r *http.Request) {
	m, err := s.svc.GetMessage(r.PathValue("name"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

// cancelBody is the optional payload of a withdrawal: why, in the words the
// reader will see.
type cancelBody struct {
	Reason string `json:"reason,omitempty"`
}

func (s *Server) cancelMessage(w http.ResponseWriter, r *http.Request) {
	var body cancelBody
	if r.ContentLength > 0 {
		if err := decode(r, &body); err != nil {
			writeErr(w, err)
			return
		}
	}
	m, err := s.svc.Cancel(r.Context(), r.PathValue("name"), body.Reason)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

// draftBody is what the orchestrator offers the reader: who wrote the answers,
// an optional line of framing, and the options themselves.
type draftBody struct {
	DraftedBy string       `json:"draftedBy"`
	Context   string       `json:"context,omitempty"`
	Text      string       `json:"text,omitempty"`
	Choices   []api.Choice `json:"choices"`
}

// draftMessage offers a ready answer to an open question, as buttons the reader
// can confirm with one tap.
//
// It lives on the HTTP API and has no MCP counterpart on purpose. The MCP tool
// set is what an agent is handed; drawing an approval gate over your own
// question is the gate dissolving, so the tool that draws it is not in the set
// the asker holds.
func (s *Server) draftMessage(w http.ResponseWriter, r *http.Request) {
	var body draftBody
	if err := decode(r, &body); err != nil {
		writeErr(w, err)
		return
	}
	m, err := s.svc.Draft(r.Context(), r.PathValue("name"), courier.DraftRequest{
		DraftedBy: body.DraftedBy,
		Context:   body.Context,
		Text:      body.Text,
		Choices:   body.Choices,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, m)
}

// awaitAnswer blocks until the question is settled or the timeout elapses. A
// timeout is not an error — it returns the question still open, which is the
// true answer to "has anyone replied yet".
func (s *Server) awaitAnswer(w http.ResponseWriter, r *http.Request) {
	timeout, err := parseTimeout(r.URL.Query().Get("timeout"), 60*time.Second)
	if err != nil {
		writeErr(w, err)
		return
	}
	m, err := s.svc.WaitForAnswer(r.Context(), r.PathValue("name"), timeout)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func parseTimeout(s string, def time.Duration) (time.Duration, error) {
	if s == "" {
		return def, nil
	}
	if d, err := time.ParseDuration(s); err == nil {
		return d, nil
	}
	// Kubernetes spells this one in bare seconds, so accept that too.
	if n, err := strconv.Atoi(s); err == nil {
		return time.Duration(n) * time.Second, nil
	}
	return 0, api.NewBadRequest("timeout %q is neither a duration (30s) nor a number of seconds", s)
}
