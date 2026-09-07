// Package server is courier's HTTP layer: a Kubernetes-shaped REST API over
// pkg/courier.
//
// It is a sibling of the MCP tool set, not a layer beneath it. Both construct a
// Service and call it in-process, so a curl and a tool call take the same path,
// write the same store and reach the same watchers. Nothing here holds state of
// its own.
package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kvaps/courier/pkg/api"
	"github.com/kvaps/courier/pkg/courier"
)

// Server serves the API.
type Server struct {
	svc     *courier.Service
	log     *slog.Logger
	version string
	// token, when set, is required as a bearer token. The daemon binds to
	// loopback by default, where the operating system is the access control;
	// this exists for the operator who binds it somewhere else.
	token string
	// mcp, when set, is served at /mcp over streamable HTTP.
	mcp *mcp.Server

	handler http.Handler
}

// Options configures a Server.
type Options struct {
	Service *courier.Service
	Logger  *slog.Logger
	Version string
	Token   string
	// MCP is the tool server to expose at /mcp. It runs against the same
	// Service, so a tool call and a REST call are the same operation.
	MCP *mcp.Server
}

// New builds the HTTP server.
func New(opts Options) *Server {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	s := &Server{svc: opts.Service, log: log, version: opts.Version, token: opts.Token, mcp: opts.MCP}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/healthz", s.healthz)
	mux.HandleFunc("GET /api/v1", s.index)

	mux.HandleFunc("GET /api/v1/channels", s.listChannels)
	mux.HandleFunc("POST /api/v1/channels", s.createChannel)
	mux.HandleFunc("GET /api/v1/channels/{name}", s.getChannel)
	mux.HandleFunc("DELETE /api/v1/channels/{name}", s.deleteChannel)

	mux.HandleFunc("GET /api/v1/conversations", s.listConversations)
	mux.HandleFunc("POST /api/v1/conversations", s.createConversation)
	mux.HandleFunc("GET /api/v1/conversations/{name}", s.getConversation)
	mux.HandleFunc("DELETE /api/v1/conversations/{name}", s.deleteConversation)
	mux.HandleFunc("POST /api/v1/conversations/{name}/close", s.closeConversation)

	mux.HandleFunc("GET /api/v1/messages", s.listMessages)
	mux.HandleFunc("POST /api/v1/messages", s.createMessage)
	mux.HandleFunc("GET /api/v1/messages/{name}", s.getMessage)
	mux.HandleFunc("POST /api/v1/messages/{name}/cancel", s.cancelMessage)
	mux.HandleFunc("GET /api/v1/messages/{name}/answer", s.awaitAnswer)

	mux.HandleFunc("GET /api/v1/watch", s.watchAll)

	// MCP is served by the daemon itself rather than by a second process.
	// The store is a single-writer in-memory index over a directory, so a
	// separate `courier mcp` would open a second index over the same files and
	// the two would silently diverge. One process, one writer, one truth —
	// agents connect with: claude mcp add --transport http courier <url>/mcp
	if s.mcp != nil {
		handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s.mcp }, nil)
		mux.Handle("/mcp", handler)
		mux.Handle("/mcp/", handler)
	}

	s.handler = s.logRequests(s.authenticate(mux))
	return s
}

// Handler is the server's http.Handler.
func (s *Server) Handler() http.Handler { return s.handler }

// authenticate enforces the bearer token when one is configured. Health and the
// discovery index stay open, so a supervisor can check the daemon without
// holding a credential.
func (s *Server) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.token == "" || r.URL.Path == "/api/healthz" || r.URL.Path == "/api/v1" {
			next.ServeHTTP(w, r)
			return
		}
		if got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); !ok || got != s.token {
			writeErr(w, api.NewBadRequest("a bearer token is required"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// logRequests logs at a level chosen by how long the request took, so an ordinary
// read is quiet and a slow one is not. A watch is exempt: it is meant to be long.
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		if strings.HasSuffix(r.URL.Path, "/watch") || r.URL.Query().Get("watch") == "true" {
			return
		}
		took := time.Since(start)
		attrs := []any{"method", r.Method, "path", r.URL.Path, "status", rec.status, "took", took}
		switch {
		case rec.status >= 500:
			s.log.Error("request failed", attrs...)
		case took > time.Second:
			s.log.Info("slow request", attrs...)
		default:
			s.log.Debug("request", attrs...)
		}
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Unwrap exposes the underlying writer so http.Flusher survives the wrapper —
// without it the watch stream would buffer and never reach the client.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeErr renders any error as a Status object with the code its reason maps
// to, so every failure leaves the daemon in the same shape.
func writeErr(w http.ResponseWriter, err error) {
	writeJSON(w, api.CodeOf(err), api.StatusOf(err))
}

func decode(r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 4<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return api.NewBadRequest("parse request body: %v", err)
	}
	return nil
}

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	channels, err := s.svc.ListChannels()
	if err != nil {
		writeErr(w, err)
		return
	}
	status := "ok"
	degraded := []string{}
	for _, c := range channels.Items {
		if c.Status.Phase != api.PhaseReady {
			status = "degraded"
			degraded = append(degraded, c.Metadata.Name+": "+string(c.Status.Phase))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":          status,
		"version":         s.version,
		"channels":        len(channels.Items),
		"degraded":        degraded,
		"resourceVersion": s.svc.Store().Version(),
	})
}

// endpoint is one row of the discovery index.
type endpoint struct {
	Method      string `json:"method"`
	Path        string `json:"path"`
	Description string `json:"description"`
}

// index is the discovery document. It is hand-written prose rather than an
// OpenAPI schema because its reader is usually a model deciding which call to
// make, and a sentence saying what a route is for beats a type signature.
func (s *Server) index(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"apiVersion": api.Version,
		"kind":       "APIIndex",
		"version":    s.version,
		"endpoints": []endpoint{
			{"GET", "/api/healthz", "Daemon health and per-channel readiness."},
			{"GET", "/api/v1/channels", "List transports. Add ?watch=true&resourceVersion=N to stream changes."},
			{"POST", "/api/v1/channels", "Register a transport and connect it. The config names a credential; it must never contain one."},
			{"GET", "/api/v1/channels/{name}", "One transport, including what it connected as."},
			{"DELETE", "/api/v1/channels/{name}", "Stop and remove a transport. Its conversations are kept."},
			{"GET", "/api/v1/conversations", "List threads. Add ?watch=true to stream."},
			{"POST", "/api/v1/conversations", "Open a thread — one per agent. spec.agent, when set, is where the human's messages are pushed."},
			{"GET", "/api/v1/conversations/{name}", "One thread."},
			{"POST", "/api/v1/conversations/{name}/close", "Close the thread, keeping its history."},
			{"DELETE", "/api/v1/conversations/{name}", "Remove the thread record."},
			{"GET", "/api/v1/messages", "List messages; ?conversation=NAME to scope, ?watch=true to stream."},
			{"POST", "/api/v1/messages", "Send. spec.awaitReply holds the message open as a question until it is answered or withdrawn."},
			{"GET", "/api/v1/messages/{name}", "One message, including status.answer once someone has replied."},
			{"GET", "/api/v1/messages/{name}/answer", "Block until the question is answered or withdrawn; ?timeout=60s. A timeout returns it still open."},
			{"POST", "/api/v1/messages/{name}/cancel", "Withdraw a question and strike it where the reader can see it."},
			{"GET", "/api/v1/watch", "Stream every kind at once; ?resourceVersion=N to resume, ?kind=Message to filter."},
			{"POST", "/mcp", "The MCP tool set, over streamable HTTP. Agents connect here."},
		},
	})
}
