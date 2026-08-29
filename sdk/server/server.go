// Package server ports packages/sdk/server: JSON-RPC methods and
// notifications for out-of-process harness SDKs. The surrounding composition
// owns plugins, persistence, and configured adapters.
package server

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"

	"dshgo/agent"
	"dshgo/llm"
	"dshgo/sdk/protocol"
	"dshgo/session"
	"dshgo/subagent"
)

// ServerVersion is the version advertised by initialization.
const ServerVersion = "0.0.1"

// DefaultProvider is the SDK route provider before initialization.
const DefaultProvider = "deepseek-official"

// AgentFactory creates and disposes the live agent+session pairs SDK
// sessions run on. The composition owns the real factory; the server only
// needs these two operations plus registry liveness.
type AgentFactory interface {
	// Create builds one live agent+session pair for the SDK session id. A
	// duplicate live id fails.
	Create(sessionID string, options CreateAgentOptions) (*agent.Agent, error)
	// Dispose tears one server-created agent down to quiescence.
	Dispose(a *agent.Agent) error
}

// CreateAgentOptions is the SDK route applied to one created agent.
type CreateAgentOptions struct {
	// Cwd is recorded on the session header.
	Cwd string
	// Provider and Model are the route every SDK-created agent runs on.
	Provider string
	Model    string
	// ReasoningEffort is the optional adapter-owned effort.
	ReasoningEffort string
	// MaxTokens is the optional positive output-token cap.
	MaxTokens int64
}

// LLMRouter is the optional LLM seam the server reads during initialization
// (official ctx.get('llm'); absent until a provider mounts).
type LLMRouter interface {
	// HasAdapter reports whether the named provider has a registered adapter.
	HasAdapter(provider string) bool
	// MountDefault mounts the DeepSeek fallback adapter; the disposer
	// unmounts it.
	MountDefault() (func(), error)
	// ResolveCallConfig validates the route resolves to a callable
	// configuration.
	ResolveCallConfig(provider, model, reasoningEffort string, maxTokens int64) error
}

// AttachmentAdmitter is the optional durable attachment store for inline
// prompt images.
type AttachmentAdmitter interface {
	// AdmitEncoded admits base64 raster blocks and returns the durable image
	// content blocks replacing them, in order.
	AdmitEncoded(images []protocol.SdkEncodedImageBlock) ([]llm.ContentBlock, error)
}

// Loader is the optional loader readiness seam: initialize waits for it
// because the plugin tree may still be settling when the first request
// arrives.
type Loader interface {
	Await() error
}

// Deps are the composition-owned services the server reads.
type Deps struct {
	// Registry resolves live agents and carries agent status events.
	Registry *agent.AgentRegistry
	// Store taps session/event and session/created.
	Store *session.Store
	// SubagentEvents carries subagent lifecycle edges (the registry bus in
	// the shipped composition).
	SubagentEvents *agent.SubjectEventBus
	// Agents creates and disposes SDK agents.
	Agents AgentFactory
	// LLM is optional; a missing adapter for the requested route mounts the
	// DeepSeek fallback.
	LLM LLMRouter
	// Attachments is optional; inline prompt images require it.
	Attachments AttachmentAdmitter
	// Loader is optional; initialize awaits it.
	Loader Loader
}

// Options are deployment-specific status mappings.
type Options struct {
	// MaxTokensAsSuccess reports max-token termination as an accepted result
	// instead of an infrastructure error.
	MaxTokensAsSuccess bool
	// Version overrides the advertised server version (tests).
	Version string
}

// sessionRecord is one SDK-created agent+session pair.
type sessionRecord struct {
	agent *agent.Agent
}

// Server is the SDK server over one booted composition and transport peer.
// Construction subscribes to session, agent, and subagent lifecycle events
// until shutdown; reinitialization is unsupported.
type Server struct {
	deps      Deps
	options   Options
	transport protocol.Peer

	mu              sync.Mutex
	cwd             string
	provider        string
	model           string
	reasoningEffort string
	maxTokens       int64
	llmDispose      func()
	sessions        map[string]*sessionRecord
	creations       map[string]*creation
	disposers       []func()
	shutdownOnce    bool
	shutdownErr     error
	shutdownDone    chan struct{}
	shuttingDown    bool
	initialized     bool
}

// creation is one in-flight session creation, shared by racing prompts for
// the same id.
type creation struct {
	done chan struct{}
	rec  *sessionRecord
	err  error
}

// New builds the server and subscribes its lifecycle taps.
func New(deps Deps, transport protocol.Peer, options Options) *Server {
	s := &Server{
		deps: deps, options: options, transport: transport,
		cwd:          "",
		provider:     DefaultProvider,
		model:        DefaultProvider,
		sessions:     map[string]*sessionRecord{},
		creations:    map[string]*creation{},
		shutdownDone: make(chan struct{}),
	}
	s.subscribe()
	return s
}

// Serve installs the JSON-RPC dispatch into a line transport: requests go
// to HandleRequest and results serialize back over the same transport. This
// mirrors the official jsonrpc.serve effect wiring; it is the composition
// side's job to call it once before serving traffic.
func (s *Server) Serve(transport *protocol.LineTransport) {
	transport.OnRequest(func(method string, params map[string]any) (any, error) {
		return s.HandleRequest(method, params)
	})
}

// subscribe installs the four lifecycle taps; each disposer joins the
// shutdown sweep.
func (s *Server) subscribe() {
	s.deps.Store.OnEvent(func(sess *session.Session, event session.Event) {
		encoded, err := json.Marshal(event)
		if err != nil {
			return
		}
		s.transport.Notify(protocol.NotifySessionEvent, protocol.SessionEventNotification{
			SessionID: string(sess.ID()),
			Event:     encoded,
		})
	})
	s.deps.Registry.Events().OnEmit(agent.EventAgentStatus, nil, func(payload any) error {
		status, ok := payload.(agent.AgentStatusPayload)
		if !ok {
			return nil
		}
		s.transport.Notify(protocol.NotifySessionStatus, protocol.SessionStatusNotification{
			SessionID: string(status.Agent.Session.ID()),
			Status:    string(status.Status),
		})
		return nil
	})
	s.deps.Store.OnCreated(func(sess *session.Session) error {
		parent := sess.Header().ParentSession
		if parent == "" {
			return nil
		}
		s.transport.Notify(protocol.NotifySubagentStarted, protocol.SubagentStartedNotification{
			ParentSessionID: string(parent),
			ChildSessionID:  string(sess.ID()),
		})
		return nil
	})
	if s.deps.SubagentEvents != nil {
		s.deps.SubagentEvents.OnEmit(subagent.EventSubagentEnd, nil, func(payload any) error {
			info, ok := payload.(subagent.SubagentRunEndInfo)
			if !ok {
				return nil
			}
			// This protocol reports only in-process child sessions. The
			// service snapshots the provider name and local flag through
			// child disposal; matching ids or parent lineage alone never
			// establishes locality.
			if !info.Local {
				return nil
			}
			notification := protocol.SubagentFinishedNotification{
				Provider:       info.Provider,
				AgentID:        string(info.ID),
				ChildSessionID: string(info.ID),
				Status:         s.successStatus(string(info.StopReason)),
				StopReason:     string(info.StopReason),
			}
			if info.LastAssistantMessage != nil {
				notification.LastAssistantMessage = info.LastAssistantMessage
			}
			s.transport.Notify(protocol.NotifySubagentFinished, notification)
			return nil
		})
	}
}

// successStatus is the deployment-specific outcome mapping for SDK subagent
// results.
func (s *Server) successStatus(reason string) protocol.SdkRunStatus {
	if reason == "completed" {
		return protocol.RunStatusOk
	}
	if reason == "max-tokens" && s.options.MaxTokensAsSuccess {
		return protocol.RunStatusOk
	}
	return protocol.RunStatusError
}

// Initialize validates and configures the SDK route, mounting the DeepSeek
// fallback only when unowned.
func (s *Server) Initialize(params protocol.InitializeParams) (protocol.InitializeResult, error) {
	if params.MaxTokens < 0 {
		return protocol.InitializeResult{}, fmt.Errorf("initialize maxTokens must be a positive safe integer")
	}
	cwd, err := filepath.Abs(params.Cwd)
	if err != nil {
		return protocol.InitializeResult{}, fmt.Errorf("initialize cwd does not resolve: %w", err)
	}
	provider := params.Provider
	if provider == "" {
		provider = DefaultProvider
	}
	if s.deps.LLM != nil && !s.deps.LLM.HasAdapter(provider) {
		if provider != DefaultProvider {
			return protocol.InitializeResult{}, fmt.Errorf("no adapter registered for provider %q", provider)
		}
		dispose, err := s.deps.LLM.MountDefault()
		if err != nil {
			return protocol.InitializeResult{}, err
		}
		s.mu.Lock()
		s.llmDispose = dispose
		s.mu.Unlock()
	}
	if s.deps.LLM != nil {
		// Adapter presence was read from this service above; a successful
		// fallback mount also requires it.
		if err := s.deps.LLM.ResolveCallConfig(provider, params.Model, params.ReasoningEffort, params.MaxTokens); err != nil {
			return protocol.InitializeResult{}, err
		}
	}
	s.mu.Lock()
	s.cwd = cwd
	s.provider = provider
	s.model = params.Model
	s.reasoningEffort = params.ReasoningEffort
	s.maxTokens = params.MaxTokens
	s.initialized = true
	s.mu.Unlock()
	version := s.options.Version
	if version == "" {
		version = ServerVersion
	}
	return protocol.InitializeResult{ServerInfo: protocol.ServerIdentity{Name: protocol.ServerName, Version: version}}, nil
}

// Prompt queues one identified prompt without assigning later activity to
// it.
func (s *Server) Prompt(params protocol.SessionPromptParams) (protocol.SessionPromptResult, error) {
	s.mu.Lock()
	initialized := s.initialized
	s.mu.Unlock()
	if !initialized {
		return protocol.SessionPromptResult{}, fmt.Errorf("SDK server is not initialized")
	}
	rec, err := s.getOrCreateSession(params.SessionID)
	if err != nil {
		return protocol.SessionPromptResult{}, err
	}
	// An agent-loop-only reload disposes the loop's agents while this record
	// survives; a retained agent accepts followup() silently, so validate
	// the record against the live registry before delivery.
	if err := s.assertLiveAgent(rec, params.SessionID); err != nil {
		return protocol.SessionPromptResult{}, err
	}
	content, err := s.durablePromptContent(params.ContentBlocks)
	if err != nil {
		return protocol.SessionPromptResult{}, err
	}
	// Attachment admission crosses an async boundary where shutdown or an
	// agent-loop reload may detach the retained handle.
	if err := s.assertLiveAgent(rec, params.SessionID); err != nil {
		return protocol.SessionPromptResult{}, err
	}
	message := llm.NewUserMessage(content, llm.MessageSource{Kind: llm.SourceUser})
	driver := rec.agent.Driver()
	if driver == nil {
		return protocol.SessionPromptResult{}, fmt.Errorf("session agent has no running loop: %s", params.SessionID)
	}
	driver.Followup(message)
	return protocol.SessionPromptResult{MessageID: string(message.ID)}, nil
}

// durablePromptContent admits inline raster blocks into durable image
// blocks, in order.
func (s *Server) durablePromptContent(blocks []json.RawMessage) ([]llm.ContentBlock, error) {
	var images []protocol.SdkEncodedImageBlock
	for _, raw := range blocks {
		var probe protocol.SdkEncodedImageBlock
		if err := json.Unmarshal(raw, &probe); err == nil && probe.Type == "image" && probe.Data != "" {
			images = append(images, probe)
		}
	}
	content := make([]llm.ContentBlock, 0, len(blocks))
	if len(images) == 0 {
		for _, raw := range blocks {
			var block llm.ContentBlock
			if err := json.Unmarshal(raw, &block); err != nil {
				return nil, fmt.Errorf("SDK prompt block is not a content block: %w", err)
			}
			content = append(content, block)
		}
		return content, nil
	}
	if s.deps.Attachments == nil {
		return nil, fmt.Errorf("SDK image prompt requires an attachment store")
	}
	admitted, err := s.deps.Attachments.AdmitEncoded(images)
	if err != nil {
		return nil, err
	}
	next := 0
	for _, raw := range blocks {
		var probe protocol.SdkEncodedImageBlock
		if err := json.Unmarshal(raw, &probe); err == nil && probe.Type == "image" && probe.Data != "" {
			content = append(content, admitted[next])
			next++
			continue
		}
		var block llm.ContentBlock
		if err := json.Unmarshal(raw, &block); err != nil {
			return nil, fmt.Errorf("SDK prompt block is not a content block: %w", err)
		}
		content = append(content, block)
	}
	return content, nil
}

func (s *Server) assertLiveAgent(rec *sessionRecord, sessionID string) error {
	if s.deps.Registry.Get(rec.agent.ID) != rec.agent {
		return fmt.Errorf("session agent was disposed outside the server: %s", sessionID)
	}
	return nil
}

// getOrCreateSession returns the live record for the id, deduplicating
// racing creations.
func (s *Server) getOrCreateSession(sessionID string) (*sessionRecord, error) {
	s.mu.Lock()
	if s.shuttingDown {
		s.mu.Unlock()
		return nil, fmt.Errorf("SDK server is shutting down")
	}
	if existing, ok := s.sessions[sessionID]; ok {
		s.mu.Unlock()
		return existing, nil
	}
	if pending, ok := s.creations[sessionID]; ok {
		s.mu.Unlock()
		<-pending.done
		return pending.rec, pending.err
	}
	pending := &creation{done: make(chan struct{})}
	s.creations[sessionID] = pending
	s.mu.Unlock()
	pending.rec, pending.err = s.createSession(sessionID)
	close(pending.done)
	s.mu.Lock()
	delete(s.creations, sessionID)
	s.mu.Unlock()
	return pending.rec, pending.err
}

func (s *Server) createSession(sessionID string) (*sessionRecord, error) {
	// No preset composition: this server's compositions keep the
	// model-facing rows in the host plane, so this agent reads them from the
	// global layer.
	s.mu.Lock()
	options := CreateAgentOptions{
		Cwd:             s.cwd,
		Provider:        s.provider,
		Model:           s.model,
		ReasoningEffort: s.reasoningEffort,
		MaxTokens:       s.maxTokens,
	}
	s.mu.Unlock()
	built, err := s.deps.Agents.Create(sessionID, options)
	if err != nil {
		return nil, err
	}
	rec := &sessionRecord{agent: built}
	s.mu.Lock()
	s.sessions[sessionID] = rec
	s.mu.Unlock()
	return rec, nil
}

// Shutdown disposes server-owned agents, the adapter, and subscriptions to
// quiescence. The surrounding composition remains running. Idempotent.
func (s *Server) Shutdown() error {
	s.mu.Lock()
	if s.shutdownOnce {
		s.mu.Unlock()
		<-s.shutdownDone
		return s.shutdownErr
	}
	s.shutdownOnce = true
	s.shuttingDown = true
	// Drain in-flight creations so their records are dispossession
	// candidates.
	for _, pending := range s.creations {
		s.mu.Unlock()
		<-pending.done
		s.mu.Lock()
	}
	records := make([]*sessionRecord, 0, len(s.sessions))
	for _, rec := range s.sessions {
		records = append(records, rec)
	}
	s.sessions = map[string]*sessionRecord{}
	disposers := s.disposers
	s.disposers = nil
	llmDispose := s.llmDispose
	s.llmDispose = nil
	s.mu.Unlock()

	var failures []error
	for index := len(disposers) - 1; index >= 0; index-- {
		if err := runDisposer(disposers[index]); err != nil {
			failures = append(failures, err)
		}
	}
	for _, rec := range records {
		if err := s.deps.Agents.Dispose(rec.agent); err != nil {
			failures = append(failures, err)
		}
	}
	if llmDispose != nil {
		if err := runDisposer(llmDispose); err != nil {
			failures = append(failures, err)
		}
	}
	s.mu.Lock()
	if len(failures) == 1 {
		s.shutdownErr = failures[0]
	} else if len(failures) > 1 {
		messages := ""
		for _, failure := range failures {
			messages += failure.Error() + "; "
		}
		s.shutdownErr = fmt.Errorf("SDK server teardown failed: %s", messages[:len(messages)-2])
	}
	s.mu.Unlock()
	close(s.shutdownDone)
	return s.shutdownErr
}

func runDisposer(dispose func()) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("disposer panicked: %v", rec)
		}
	}()
	dispose()
	return nil
}

// HandleRequest dispatches one incoming JSON-RPC request to its typed
// handler. A non-nil error becomes a JSON-RPC error response.
func (s *Server) HandleRequest(method string, params map[string]any) (any, error) {
	if method == protocol.MethodInitialize && s.deps.Loader != nil {
		// initialize is the SDK's readiness boundary: the plugin tree may
		// still be settling, so do not advertise a ready runtime until the
		// complete current tree has settled.
		if err := s.deps.Loader.Await(); err != nil {
			return nil, err
		}
	}
	switch method {
	case protocol.MethodInitialize:
		var decoded protocol.InitializeParams
		if err := decodeInto(params, &decoded); err != nil {
			return nil, err
		}
		return s.Initialize(decoded)
	case protocol.MethodSessionPrompt:
		var decoded protocol.SessionPromptParams
		if err := decodeInto(params, &decoded); err != nil {
			return nil, err
		}
		return s.Prompt(decoded)
	case protocol.MethodShutdown:
		if err := s.Shutdown(); err != nil {
			return nil, err
		}
		return map[string]any{}, nil
	default:
		return nil, fmt.Errorf("unknown DeepSeek Harness SDK runtime method: %s", method)
	}
}

func decodeInto(params map[string]any, target any) error {
	encoded, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("SDK request params are not JSON-serializable: %w", err)
	}
	if err := json.Unmarshal(encoded, target); err != nil {
		return fmt.Errorf("SDK request params are invalid: %w", err)
	}
	return nil
}
