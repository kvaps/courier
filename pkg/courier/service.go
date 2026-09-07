// Package courier is the daemon's rules layer: everything that decides what a
// message means, sitting between the store and the two transports.
//
// The HTTP API and the MCP tool set are siblings over this package, not layers
// on top of each other. Both construct a Service and call it in-process, so a
// tool call and a REST call take the same path, hit the same store and reach
// the same watchers.
package courier

import (
	"context"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/kvaps/courier/pkg/agent"
	"github.com/kvaps/courier/pkg/api"
	"github.com/kvaps/courier/pkg/backend"
	"github.com/kvaps/courier/pkg/store"
)

// Service carries conversations between people and agents.
type Service struct {
	store    *store.Store
	stateDir string
	log      *slog.Logger

	channels      *store.Collection[api.Channel, *api.Channel]
	conversations *store.Collection[api.Conversation, *api.Conversation]
	messages      *store.Collection[api.Message, *api.Message]

	mu      sync.Mutex
	running map[string]*channelRuntime
	sinks   map[string]agent.Sink

	// baseCtx is the daemon's lifetime, used for work that outlives the request
	// that started it — a channel's receive loop, and a push to an agent that
	// must not be cancelled because an HTTP client hung up.
	baseCtx context.Context
	stop    context.CancelFunc
	wg      sync.WaitGroup
}

type channelRuntime struct {
	backend backend.Backend
	cancel  context.CancelFunc
}

// New builds a service over a store. stateDir is where backends may keep
// private state; it is not part of the API.
func New(s *store.Store, stateDir string, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	ctx, stop := context.WithCancel(context.Background())
	return &Service{
		store:         s,
		stateDir:      stateDir,
		log:           log,
		channels:      store.For[api.Channel, *api.Channel](s),
		conversations: store.For[api.Conversation, *api.Conversation](s),
		messages:      store.For[api.Message, *api.Message](s),
		running:       map[string]*channelRuntime{},
		sinks:         map[string]agent.Sink{},
		baseCtx:       ctx,
		stop:          stop,
	}
}

// Store exposes the underlying store, for the API layer's watch endpoint.
func (s *Service) Store() *store.Store { return s.store }

// Start brings every stored channel back up. A channel that will not connect is
// left Failed with the reason on its status rather than stopping the daemon:
// the other channels, and every conversation already recorded, still work.
func (s *Service) Start(ctx context.Context) error {
	list, _, err := s.channels.List()
	if err != nil {
		return err
	}
	for i := range list {
		ch := list[i]
		if err := s.activate(ctx, &ch); err != nil {
			s.log.Error("channel did not start", "channel", ch.Metadata.Name, "err", err)
		}
	}
	return nil
}

// Close stops every channel's receive loop and waits for them.
func (s *Service) Close() {
	s.stop()
	s.mu.Lock()
	for name, rt := range s.running {
		rt.cancel()
		delete(s.running, name)
	}
	s.mu.Unlock()
	s.wg.Wait()
}

// activate connects a channel's backend and starts its receive loop.
func (s *Service) activate(ctx context.Context, ch *api.Channel) error {
	be, err := backend.New(ch.Spec.Backend, backend.Env{
		Channel:  ch.Metadata.Name,
		StateDir: filepath.Join(s.stateDir, "backends", ch.Metadata.Name),
	}, ch.Spec.Config)
	if err != nil {
		s.markChannel(ch.Metadata.Name, api.PhaseFailed, "", "", err.Error())
		return err
	}
	identity, err := be.Connect(ctx)
	if err != nil {
		s.markChannel(ch.Metadata.Name, api.PhaseFailed, "", "", err.Error())
		return err
	}
	s.markChannel(ch.Metadata.Name, api.PhaseReady, identity.Self, identity.Target, "")

	runCtx, cancel := context.WithCancel(s.baseCtx)
	s.mu.Lock()
	if prev, ok := s.running[ch.Metadata.Name]; ok {
		prev.cancel()
	}
	s.running[ch.Metadata.Name] = &channelRuntime{backend: be, cancel: cancel}
	s.mu.Unlock()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer cancel()
		if err := be.Run(runCtx, s); err != nil && runCtx.Err() == nil {
			s.log.Error("channel receive loop stopped", "channel", ch.Metadata.Name, "err", err)
			s.markChannel(ch.Metadata.Name, api.PhaseFailed, "", "", err.Error())
		}
	}()
	s.log.Info("channel ready", "channel", ch.Metadata.Name, "backend", ch.Spec.Backend,
		"as", identity.Self, "target", identity.Target)
	return nil
}

// backendFor returns a channel's running backend.
func (s *Service) backendFor(channel string) (backend.Backend, error) {
	s.mu.Lock()
	rt, ok := s.running[channel]
	s.mu.Unlock()
	if !ok {
		return nil, api.NewInvalid("channel %q is not running", channel)
	}
	return rt.backend, nil
}

// sinkFor returns a delivery sink by name, building it once and reusing it.
func (s *Service) sinkFor(kind string) (agent.Sink, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sk, ok := s.sinks[kind]; ok {
		return sk, nil
	}
	sk, err := agent.New(kind)
	if err != nil {
		return nil, err
	}
	s.sinks[kind] = sk
	return sk, nil
}

// markChannel writes a channel's observed state. It is best-effort by design:
// a status write failing must not take down the transport it describes.
func (s *Service) markChannel(name string, phase api.Phase, identity, target, message string) {
	ch, err := s.channels.Get(name)
	if err != nil {
		return
	}
	now := time.Now().UTC()
	ch.Status.Phase = phase
	ch.Status.ObservedAt = &now
	ch.Status.Message = message
	if identity != "" {
		ch.Status.Identity = identity
	}
	if target != "" {
		ch.Status.Target = target
	}
	if phase == api.PhaseReady && ch.Status.ConnectedAt == nil {
		ch.Status.ConnectedAt = &now
	}
	if _, err := s.channels.UpdateStatus(ch); err != nil {
		s.log.Warn("could not record channel status", "channel", name, "err", err)
	}
}

// SetStatus implements backend.Sink.
func (s *Service) SetStatus(_ context.Context, channel string, phase api.Phase, message string) {
	s.markChannel(channel, phase, "", "", message)
}

// CreateChannel registers a channel and brings it up. Connecting is part of
// creation on purpose: a channel that cannot reach its transport is a
// misconfiguration the operator should hear about now, while they are looking,
// rather than at the first message an agent tries to send.
func (s *Service) CreateChannel(ctx context.Context, ch *api.Channel) (*api.Channel, error) {
	if ch.Spec.Backend == "" {
		return nil, api.NewInvalid("spec.backend is required; registered backends: %v", backend.Kinds())
	}
	ch.TypeMeta = api.TypeMeta{APIVersion: api.Version, Kind: api.KindChannel}
	ch.Status = api.ChannelStatus{Phase: api.PhasePending}

	created, err := s.channels.Create(ch)
	if err != nil {
		return nil, err
	}
	if err := s.activate(ctx, created); err != nil {
		// The object stays, carrying the reason on its status: deleting it would
		// throw away the configuration the operator needs in order to fix it.
		return s.channels.Get(created.Metadata.Name)
	}
	return s.channels.Get(created.Metadata.Name)
}

// GetChannel returns one channel.
func (s *Service) GetChannel(name string) (*api.Channel, error) { return s.channels.Get(name) }

// ListChannels returns every channel.
func (s *Service) ListChannels() (api.List[api.Channel], error) {
	items, rv, err := s.channels.List()
	if err != nil {
		return api.List[api.Channel]{}, err
	}
	return api.NewList(api.KindChannel, rv, items), nil
}

// DeleteChannel stops a channel and removes it. Its conversations are left
// alone: they hold the record of what was said, which outlives the transport.
func (s *Service) DeleteChannel(name string) error {
	s.mu.Lock()
	if rt, ok := s.running[name]; ok {
		rt.cancel()
		delete(s.running, name)
	}
	s.mu.Unlock()
	return s.channels.Delete(name)
}

// replyPrompt returns the channel's configured closing line for a question.
func (s *Service) replyPrompt(channel string) string {
	be, err := s.backendFor(channel)
	if err != nil {
		return api.DefaultReplyPrompt
	}
	if p, ok := be.(interface{ ReplyPrompt() string }); ok {
		if v := p.ReplyPrompt(); v != "" {
			return v
		}
	}
	return api.DefaultReplyPrompt
}
