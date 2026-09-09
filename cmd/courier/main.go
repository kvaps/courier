// Command courier runs the message daemon: a local service that carries a
// question from an agent to a person and the answer back.
//
// One process does everything. It owns the store, runs each channel's receive
// loop, serves the REST API, and serves the MCP tool set at /mcp — because the
// store is a single-writer index over a directory, and a second process opening
// it would be a second truth.
//
//	courier serve --telegram-chat https://t.me/c/1234567890/1
//	claude mcp add --transport http courier http://127.0.0.1:7717/mcp
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/kvaps/courier/internal/server"
	"github.com/kvaps/courier/pkg/api"
	"github.com/kvaps/courier/pkg/courier"
	"github.com/kvaps/courier/pkg/mcpserver"
	"github.com/kvaps/courier/pkg/store"

	// Registering a backend and a sink is what makes them available; nothing
	// else in the daemon names them.
	_ "github.com/kvaps/courier/pkg/agent/claude"
	"github.com/kvaps/courier/pkg/backend/telegram"
)

// version is set at build time.
var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "courier: "+err.Error())
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return errors.New("a subcommand is required")
	}
	switch args[0] {
	case "serve":
		return serve(args[1:])
	case "version":
		fmt.Println(version)
		return nil
	case "help", "-h", "--help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown subcommand %q", args[0])
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `courier — carry a question to a person and the answer back

  courier serve [flags]   run the daemon (REST at /api/v1, MCP at /mcp)
  courier version
  courier help

Run `+"`courier serve --help`"+` for the flags.
`)
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	var (
		listen   = fs.String("listen", "127.0.0.1:7717", "address to serve the API and MCP on; loopback by default, where the operating system is the access control")
		stateDir = fs.String("state", defaultStateDir(), "directory the daemon keeps its resources in")
		token    = fs.String("token", os.Getenv("COURIER_TOKEN"), "require this bearer token on API calls; only needed if you bind off loopback")
		verbose  = fs.Bool("verbose", false, "log every request")

		tgChat   = fs.String("telegram-chat", "", "Telegram destination: a numeric id (-100…), an @username, or a t.me link. Given on a first run, it registers a channel named by --telegram-channel")
		tgName   = fs.String("telegram-channel", "telegram", "resource name for the channel created from --telegram-chat")
		tgFile   = fs.String("telegram-token-file", "", "KEY=VALUE file holding the bot token; defaults to ~/.claude/channels/telegram/.env")
		tgEnv    = fs.String("telegram-token-env", "TELEGRAM_BOT_TOKEN", "environment variable holding the bot token, checked before the file")
		tgPrompt = fs.String("telegram-reply-prompt", api.DefaultReplyPrompt, "the line that closes a question, inviting a one-word answer")
		tgChoice = fs.String("telegram-choice-prompt", api.DefaultChoicePrompt, "the line that closes a drafted answer, saying the buttons are an offer and writing a reply still works")
		tgAllow  = fs.String("telegram-allow-from", "", "comma-separated Telegram user ids allowed to drive agents; empty means anyone in the chat")
		tgHear   = fs.String("telegram-transcribe", "", "MCP server that turns voice messages into words, e.g. http://127.0.0.1:8787/mcp; without it a voice message is not carried")
		tgHearS  = fs.Int("telegram-transcribe-wait", 0, "seconds to wait for a transcription; it is the receive loop's own time, so keep it short (default 20)")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)

	st, err := store.Open(filepath.Join(*stateDir, "resources"))
	if err != nil {
		return fmt.Errorf("open the store: %w", err)
	}
	svc := courier.New(st, *stateDir, log)
	defer svc.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := svc.Start(ctx); err != nil {
		return fmt.Errorf("start channels: %w", err)
	}
	if *tgChat != "" {
		if err := ensureTelegram(ctx, svc, *tgName, telegram.Config{
			Chat:         *tgChat,
			TokenFile:    *tgFile,
			TokenEnv:     *tgEnv,
			ReplyPrompt:  *tgPrompt,
			ChoicePrompt: *tgChoice,
			AllowFrom:    parseIDs(*tgAllow),
			Transcribe:   transcribeConfig(*tgHear, *tgHearS),
		}); err != nil {
			return err
		}
	}

	srv := &http.Server{
		Addr: *listen,
		Handler: server.New(server.Options{
			Service: svc,
			Logger:  log,
			Version: version,
			Token:   *token,
			MCP:     mcpserver.New(mcpserver.Config{Service: svc, Version: version}),
		}).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// No write timeout: a watch and a wait_for_answer are meant to be long.
	}

	errs := make(chan error, 1)
	go func() {
		log.Info("courier listening", "addr", *listen, "state", *stateDir, "version", version)
		log.Info("connect an agent with", "cmd",
			fmt.Sprintf("claude mcp add --transport http courier http://%s/mcp", *listen))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
		}
	}()

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdown)
	}
}

// ensureTelegram registers the channel described by the flags, or reports how
// the existing one differs.
//
// It is idempotent so the flags can stay in a service definition: restarting
// the daemon must not fail because the channel it was told to create is already
// there. A channel that exists is left alone — its configuration is in the store
// and editable through the API, and silently rewriting it from command-line
// flags would make the API's copy a lie.
func ensureTelegram(ctx context.Context, svc *courier.Service, name string, cfg telegram.Config) error {
	if existing, err := svc.GetChannel(name); err == nil {
		slog.Info("channel already registered; leaving it as it is",
			"channel", name, "phase", existing.Status.Phase, "target", existing.Status.Target)
		return nil
	} else if !api.IsNotFound(err) {
		return err
	}

	raw, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	ch, err := svc.CreateChannel(ctx, &api.Channel{
		Metadata: api.ObjectMeta{Name: name},
		Spec:     api.ChannelSpec{Backend: telegram.Kind, Config: raw},
	})
	if err != nil {
		return fmt.Errorf("register the telegram channel: %w", err)
	}
	if ch.Status.Phase != api.PhaseReady {
		// Not fatal: the object is stored with the reason on it, so the operator
		// can fix the configuration through the API without restarting.
		slog.Error("telegram channel did not connect", "channel", name, "reason", ch.Status.Message)
	}
	return nil
}

// transcribeConfig turns the flags into a channel's transcription setting, or
// nothing at all when no endpoint was given.
func transcribeConfig(endpoint string, wait int) *telegram.TranscribeConfig {
	if strings.TrimSpace(endpoint) == "" {
		return nil
	}
	return &telegram.TranscribeConfig{Endpoint: strings.TrimSpace(endpoint), WaitSeconds: wait}
}

func parseIDs(s string) []int64 {
	var out []int64
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part == "" {
			continue
		}
		if id, err := strconv.ParseInt(part, 10, 64); err == nil {
			out = append(out, id)
		} else {
			slog.Warn("ignoring an allow-from entry that is not a user id", "value", part)
		}
	}
	return out
}

func defaultStateDir() string {
	if d := os.Getenv("COURIER_STATE"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".courier"
	}
	return filepath.Join(home, ".courier")
}
