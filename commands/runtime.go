package commands

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"sync"

	"dshgo/agent"
	"dshgo/cordis"
	"dshgo/scope"
	"dshgo/session"
)

// commandLayer owns all command registrations of one global or scoped layer.
type commandLayer struct {
	commands *scope.NamedEntries[CommandDefinition]
}

// newCommandLayer builds one layer with diagnostics specific to its
// ownership scope.
func newCommandLayer(scopeKey scope.ScopeKey) *commandLayer {
	return &commandLayer{
		commands: scope.NewNamedEntries[CommandDefinition](func(name string) error {
			if scopeKey == nil {
				return fmt.Errorf("command %q is already registered (for a per-agent variant, mount a command-injected plugin under that agent's `agent.ctx`)", name)
			}
			return fmt.Errorf("command %q is already registered in this scope", name)
		}),
	}
}

func (l *commandLayer) isEmpty() bool { return l.commands.IsEmpty() }

// ImageAdmitter admits encoded composer images into durable attachments. It
// is the `attachments` service seam: absent (nil) when no attachment store
// is composed.
type ImageAdmitter func(images []any) ([]ImageAttachment, error)

// ImageAdmissionErrorCode is a stable, caller-correctable attachment
// failure code: the proposed image content or batch can be corrected and
// resubmitted (the official AttachmentError code vocabulary).
type ImageAdmissionErrorCode string

// The official image-admission code set. Storage faults never use these.
const (
	AdmissionTooManyImages       ImageAdmissionErrorCode = "TOO_MANY_IMAGES"
	AdmissionImagesTooLarge      ImageAdmissionErrorCode = "IMAGES_TOO_LARGE"
	AdmissionUnsupportedImgType  ImageAdmissionErrorCode = "UNSUPPORTED_IMAGE_TYPE"
	AdmissionInvalidImageBase64  ImageAdmissionErrorCode = "INVALID_IMAGE_BASE64"
	AdmissionInvalidImage        ImageAdmissionErrorCode = "INVALID_IMAGE"
	AdmissionImageTypeMismatch   ImageAdmissionErrorCode = "IMAGE_TYPE_MISMATCH"
	AdmissionImageTooLarge       ImageAdmissionErrorCode = "IMAGE_TOO_LARGE"
	AdmissionImageTooManyPixels  ImageAdmissionErrorCode = "IMAGE_TOO_MANY_PIXELS"
	AdmissionImageDimensionLarge ImageAdmissionErrorCode = "IMAGE_DIMENSION_TOO_LARGE"
)

// ImageAdmissionError marks a caller-correctable image admission failure.
// The command execution settles it as a gentle error result (visible in the
// UI, no throw); any other admission failure is a runtime failure that
// settles thrown and propagates. Mirrors the official
// `error instanceof AttachmentError` branch so the attachment round's
// admitter keeps the official two-way classification.
type ImageAdmissionError struct {
	// Message is the human-readable failure description without raw bytes
	// or host paths — the settled error-result text.
	Message string
	// Code is the stable machine-routing code.
	Code ImageAdmissionErrorCode
}

func (e *ImageAdmissionError) Error() string { return e.Message }

// CommandRuntime is the human-command registry. Scoped registrations shadow
// globals for their agent; names are unique per layer. Go adaptation: the
// receiving agent's scope comes in as an explicit ScopeKey (the tools
// runtime's pattern), not an Agent parameter.
type CommandRuntime struct {
	logger cordis.Logger

	mu     sync.Mutex
	layers *scope.Layers[commandLayer]

	commandSeq    int
	instanceToken string

	// admitImages is the optional attachment-store seam.
	admitImages ImageAdmitter

	listenersMu sync.Mutex
	listeners   map[int]func()
	nextHandle  int
}

// NewCommandRuntime builds the runtime.
func NewCommandRuntime(logger cordis.Logger) *CommandRuntime {
	runtime := &CommandRuntime{
		logger:        logger,
		instanceToken: instanceToken(),
		listeners:     map[int]func(){},
	}
	runtime.layers = scope.NewLayers(newCommandLayer, func(layer *commandLayer) bool { return layer.isEmpty() }, runtime.notifyChange)
	return runtime
}

// instanceToken keeps minted ids unique across process restarts over one
// resumed log.
func instanceToken() string {
	raw := make([]byte, 4)
	if _, err := rand.Read(raw); err != nil {
		return "00000000"
	}
	return hex.EncodeToString(raw)
}

// SetImageAdmitter installs the attachment-store seam.
func (r *CommandRuntime) SetImageAdmitter(admit ImageAdmitter) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.admitImages = admit
}

// Register registers a global or calling-agent-scoped command and returns
// the exact effect disposer that unregisters this definition.
func (r *CommandRuntime) Register(scopeKey scope.ScopeKey, definition CommandDefinition) (func(), error) {
	normalized, _, err := normalizeDefinition(definition)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	dispose, changed, err := r.layers.Mutate(scopeKey, func(layer *commandLayer) (func(), error) {
		if err := layer.commands.Insert(normalized.Name, normalized); err != nil {
			return nil, err // The layer's constructor owns the duplicate diagnostic.
		}
		return func() { layer.commands.Remove(normalized.Name) }, nil
	})
	r.mu.Unlock()
	if changed {
		r.notifyChange()
	}
	return dispose, err
}

// List returns the effective immutable command descriptors for one agent
// scope: name-sorted after scoped shadowing.
func (r *CommandRuntime) List(scopeKey scope.ScopeKey) []CommandDescriptor {
	definitions := r.effective(scopeKey)
	descriptors := make([]CommandDescriptor, 0, len(definitions))
	for _, definition := range definitions {
		_, descriptor, err := normalizeDefinition(definition)
		if err != nil {
			continue
		}
		descriptors = append(descriptors, descriptor)
	}
	sort.Slice(descriptors, func(i int, j int) bool {
		// Names are unique in the effective view, so equality is impossible.
		return descriptors[i].Name < descriptors[j].Name
	})
	return descriptors
}

// Find resolves one effective command definition for an agent scope.
func (r *CommandRuntime) Find(scopeKey scope.ScopeKey, name string) (CommandDefinition, bool) {
	for _, definition := range r.effective(scopeKey) {
		if definition.Name == name {
			return definition, true
		}
	}
	return CommandDefinition{}, false
}

// effective materializes global definitions followed by exact scoped
// shadows.
func (r *CommandRuntime) effective(scopeKey scope.ScopeKey) []CommandDefinition {
	r.mu.Lock()
	merged := scope.MergeLayers(r.layers, scopeKey, func(layer *commandLayer) *scope.NamedEntries[CommandDefinition] {
		return layer.commands
	})
	r.mu.Unlock()
	return merged.Values()
}

// Execute parses and executes a known command without sending it to the
// model.
//
// A resolved command's lifecycle is logged: `command/run` is appended
// before the handler is invoked and `command/done` after settlement (a
// thrown or aborted handler settles as kind "error"). Both are direct
// log-only appends — no turn wraps them, and persistence drains them at
// ordinary checkpoints. Admission misses (syntax or unknown name) log
// nothing — they never entered a handler. A `command/run` append failure
// fails the execution loud; a `command/done` append failure on the
// handler-failure path is contained so the handler's own error stays the
// reported failure.
//
// Image admission is enforced here, not in the composer: images sent to a
// command that does not declare input.images, and an absent attachment
// store, each settle as an error result before the handler runs, and a
// rejected batch publishes no durable object.
//
// Go adaptations: the cancellation signal is the UI request's context — an
// already-cancelled context fails the execution before the handler, and a
// context cancelled while a synchronous Go handler runs abandons it (the
// same semantics as the source's withAbort race): the handler result is
// discarded and the execution settles as the context error. The receiving
// agent stays explicit; the scope key travels beside it.
func (r *CommandRuntime) Execute(ctx context.Context, scopeKey scope.ScopeKey, sess *session.Session, line string, images []any) (*CommandExecution, error) {
	return r.ExecuteForAgent(ctx, nil, scopeKey, sess, line, images)
}

// ExecuteForAgent dispatches like Execute and additionally hands the
// receiving agent to the handler (the source invocation always carries the
// Agent; UI surfaces that resolved one pass it here).
func (r *CommandRuntime) ExecuteForAgent(ctx context.Context, agentObj *agent.Agent, scopeKey scope.ScopeKey, sess *session.Session, line string, images []any) (*CommandExecution, error) {
	parsed, ok := ParseCommand(line)
	if !ok {
		return nil, nil
	}
	definition, resolved := r.Find(scopeKey, parsed.Name)
	if !resolved {
		return nil, nil
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	commandID := r.mintCommandID()
	runData := CommandRunData{
		CommandID: commandID,
		Name:      parsed.Name,
		Source:    CommandSource{Kind: "user"},
	}
	if definition.RecordInput == nil || *definition.RecordInput {
		rawInput := parsed.RawInput
		runData.Args = &rawInput
	}
	if _, err := sess.Append(EventCommandRun, runData, nil); err != nil {
		return nil, err
	}
	settle := func(result CommandResult) (*CommandExecution, error) {
		doneData := CommandDoneData{CommandID: commandID, Kind: result.Kind}
		if result.HasText {
			text := result.Text
			doneData.Text = &text
		}
		if result.Kind == ResultSuccess && result.SourceEventSeq != nil {
			seq := *result.SourceEventSeq
			doneData.SourceEventSeq = &seq
		}
		if _, err := sess.Append(EventCommandDone, doneData, nil); err != nil {
			return nil, err
		}
		return &CommandExecution{CommandID: commandID, Result: result}, nil
	}

	var attachments []ImageAttachment
	if len(images) > 0 {
		if definition.Input == nil || !definition.Input.Images {
			return settle(CommandResult{Kind: ResultError, HasText: true,
				Text: fmt.Sprintf("/%s does not accept image attachments", parsed.Name)})
		}
		r.mu.Lock()
		admit := r.admitImages
		r.mu.Unlock()
		if admit == nil {
			return settle(CommandResult{Kind: ResultError, HasText: true,
				Text: fmt.Sprintf("/%s: image attachments are unavailable because no attachment store is composed", parsed.Name)})
		}
		refs, err := admit(images)
		if err != nil {
			// A caller-correctable admission failure settles as a gentle
			// error result (the official AttachmentError branch); anything
			// else is a runtime failure: settle thrown and propagate loud.
			var admissionErr *ImageAdmissionError
			if errors.As(err, &admissionErr) {
				return settle(CommandResult{Kind: ResultError, HasText: true, Text: admissionErr.Message})
			}
			r.settleThrown(sess, parsed.Name, commandID, err)
			return nil, err
		}
		attachments = refs
		// Cancellation must be honored BEFORE the handler runs: admission
		// may await slow storage, and a handler entered after the caller
		// cancelled would mutate state the retrying caller then duplicates.
		if ctx.Err() != nil {
			r.settleThrown(sess, parsed.Name, commandID, ctx.Err())
			return nil, ctx.Err()
		}
	}

	// Run the handler with the withAbort race: a context cancelled mid-run
	// abandons the result and settles the execution as the context error.
	type outcome struct {
		result CommandResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := definition.Handler(Invocation{
			CommandID:   commandID,
			Session:     sess,
			Agent:       agentObj,
			RawInput:    parsed.RawInput,
			Attachments: attachments,
			// The dispatching UI request's cancellation: the AbortSignal
			// counterpart handlers pass into long-running work.
			Context: ctx,
		})
		done <- outcome{result: result, err: err}
	}()
	select {
	case <-ctx.Done():
		r.settleThrown(sess, parsed.Name, commandID, ctx.Err())
		return nil, ctx.Err()
	case finished := <-done:
		result, err := normalizeResult(parsed.Name, finished.result, finished.err)
		if err != nil {
			r.settleThrown(sess, parsed.Name, commandID, err)
			return nil, err
		}
		return settle(result)
	}
}

// settleThrown is the contained `command/done` error append for a thrown
// handler or admission failure.
func (r *CommandRuntime) settleThrown(sess *session.Session, command string, commandID CommandID, err error) {
	text := err.Error()
	doneData := CommandDoneData{CommandID: commandID, Kind: ResultError, Text: &text}
	if _, appendErr := sess.Append(EventCommandDone, doneData, nil); appendErr != nil && r.logger != nil {
		r.logger.Warn(fmt.Sprintf("command %q: command/done append failed: %v", command, appendErr))
	}
}

// mintCommandID mints the next pairing id (monotonic; instance-token
// prefixed so a resumed log never repeats one).
func (r *CommandRuntime) mintCommandID() CommandID {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.commandSeq++
	return CommandID(fmt.Sprintf("cmd-%s-%d", r.instanceToken, r.commandSeq))
}

// OnChange registers a registry observer and returns its idempotent undo.
// Notifications are non-vetoing: observer failures are contained and cannot
// affect the registry mutation.
func (r *CommandRuntime) OnChange(listener func()) func() {
	r.listenersMu.Lock()
	defer r.listenersMu.Unlock()
	handle := r.nextHandle
	r.nextHandle++
	r.listeners[handle] = listener
	return func() {
		r.listenersMu.Lock()
		defer r.listenersMu.Unlock()
		delete(r.listeners, handle)
	}
}

// notifyChange dispatches to every observer, containing each independently.
func (r *CommandRuntime) notifyChange() {
	r.listenersMu.Lock()
	handlers := make([]func(), 0, len(r.listeners))
	for _, listener := range r.listeners {
		handlers = append(handlers, listener)
	}
	r.listenersMu.Unlock()
	for _, handler := range handlers {
		func() {
			defer func() {
				if rec := recover(); rec != nil && r.logger != nil {
					r.logger.Warn(fmt.Sprintf("commands/change listener threw: %v", rec))
				}
			}()
			handler()
		}()
	}
}
