// Tests for session/prompt + session/cancel: the browser chat send loop's
// host half. The recording driver captures the steer/followup delivery
// split, the echo dedup, the timezone and content gates, and the cancel
// cause/options pairing.
package gateway

import (
	"context"
	"fmt"
	"iter"
	"strings"
	"sync"
	"testing"

	"dshgo/agent"
	"dshgo/agentdefaultmodel"
	"dshgo/attachment"
	"dshgo/cordis"
	"dshgo/llm"
	"dshgo/session"
)

// promptRecordingDriver records the control deliveries the prompt endpoint
// makes into the live agent. When session is wired (the real ReactLoopAgent
// appends the durable user message at claim time), deliveries replay that
// append so the echo dedup sees the log.
type promptRecordingDriver struct {
	mu        sync.Mutex
	followups []llm.Message
	steers    []llm.Message
	cancels   []session.TurnEndCancelCause
	session   *session.Session
}

func (d *promptRecordingDriver) deliver(message llm.Message) {
	if d.session != nil {
		if _, err := d.session.Append(session.EventUserMessage, message, &session.SurfaceIntent{SurfaceOp: session.SurfaceOp{Kind: session.SurfaceAppend}}); err != nil {
			return
		}
	}
}

func (d *promptRecordingDriver) Followup(message llm.Message) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.followups = append(d.followups, message)
	d.deliver(message)
}

func (d *promptRecordingDriver) Steer(message llm.Message) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.steers = append(d.steers, message)
	d.deliver(message)
}

func (d *promptRecordingDriver) Cancel(cause session.TurnEndCancelCause, _ agent.CancelOptions) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.cancels = append(d.cancels, cause)
}

func (d *promptRecordingDriver) WhenIdle() <-chan struct{} {
	done := make(chan struct{})
	close(done)
	return done
}

func (d *promptRecordingDriver) RunMaintenance(task func(context.Context) error) error {
	return task(context.Background())
}

func (d *promptRecordingDriver) Send(llm.Message, agent.InboxTarget, bool) {}
func (d *promptRecordingDriver) Inject(llm.Message)                        {}

func (d *promptRecordingDriver) counts() (followups, steers, cancels int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.followups), len(d.steers), len(d.cancels)
}

// promptFakeStore is the attachment-store stand-in: every image admits to a
// synthetic reference; err fails the batch.
type promptFakeStore struct{ err error }

func (s *promptFakeStore) ImageLimits() attachment.ImageAttachmentLimits {
	return attachment.ImageAttachmentLimits{}
}

func (s *promptFakeStore) ValidateImage(attachment.SaveImageAttachment) error { return nil }

func (s *promptFakeStore) SaveImages(inputs []attachment.SaveImageAttachment) ([]attachment.ImageAttachmentRef, error) {
	if s.err != nil {
		return nil, s.err
	}
	refs := make([]attachment.ImageAttachmentRef, 0, len(inputs))
	for index, input := range inputs {
		refs = append(refs, attachment.ImageAttachmentRef{
			AttachmentID: fmt.Sprintf("att-%d", index), MediaType: input.MediaType, Bytes: len(input.Data),
		})
	}
	return refs, nil
}

func (s *promptFakeStore) SaveImage(input attachment.SaveImageAttachment) (attachment.ImageAttachmentRef, error) {
	refs, err := s.SaveImages([]attachment.SaveImageAttachment{input})
	if err != nil {
		return attachment.ImageAttachmentRef{}, err
	}
	return refs[0], nil
}

func (s *promptFakeStore) ReadImage(attachment.ImageAttachmentRef) (attachment.StoredImageAttachment, error) {
	return attachment.StoredImageAttachment{}, nil
}

func (s *promptFakeStore) ImageHostPath(attachment.ImageAttachmentRef) (string, bool, error) {
	return "", false, nil
}

func (s *promptFakeStore) ReadImageRequest(attachment.ImageAttachmentRef, attachment.ImageRequestPolicy) (attachment.RequestImageAttachment, error) {
	return attachment.RequestImageAttachment{}, nil
}

// promptTestAdapter is a minimal llm adapter serving the default test route
// (text-only modalities; registration is all the route gate reads).
type promptTestAdapter struct{}

func (a *promptTestAdapter) Stream(options llm.GenerateOptions) iter.Seq[llm.StreamChunk] {
	return func(yield func(llm.StreamChunk) bool) {}
}

func (a *promptTestAdapter) ProviderInfo(provider string) llm.LlmProviderInfo {
	return llm.LlmProviderInfo{ID: provider, Name: provider}
}

func (a *promptTestAdapter) ProviderRetryPolicy(string) *llm.ResolvedRetryPolicy { return nil }

func (a *promptTestAdapter) ListModels(string) ([]llm.LlmModelInfo, error) { return nil, nil }

func (a *promptTestAdapter) ResolveModel(provider, model string) (llm.LlmResolvedModelInfo, error) {
	return llm.LlmResolvedModelInfo{
		LlmModelInfo: llm.LlmModelInfo{
			Provider: provider, ID: model, Name: model, InputModalities: []string{"text", "image"},
		},
	}, nil
}

// newPromptController builds the controller over the create fake factory
// with a recording driver and the fake store attached.
func newPromptController(t *testing.T, store attachment.Store) (*SessionController, *createFakeFactory, *promptRecordingDriver) {
	t.Helper()
	host := cordis.NewRoot(cordis.Discard{})
	storeSessions := session.NewStore(nil)
	driver := &promptRecordingDriver{}
	factory := &createFakeFactory{host: host, store: storeSessions, registry: agent.NewAgentRegistry(host, nil), driver: driver}
	if _, err := factory.registry.SetFactory(factory); err != nil {
		t.Fatalf("SetFactory: %v", err)
	}
	defaultModel, err := agentdefaultmodel.New(agentdefaultmodel.Settings{Provider: "deepseek-official", Model: "deepseek-v4-flash"})
	if err != nil {
		t.Fatalf("default model: %v", err)
	}
	llmRuntime := llm.NewRuntime()
	if _, err := llmRuntime.RegisterAdapter([]string{"deepseek-official"}, &promptTestAdapter{}); err != nil {
		t.Fatalf("register adapter: %v", err)
	}
	controller := NewSessionController(nil, nil, func() any { return llmRuntime }, func() any { return defaultModel })
	controller.EnableCreate(SessionCreateDeps{
		Agents:      func() any { return factory.registry },
		Sessions:    func() any { return storeSessions },
		Attachments: func() any { return store },
	})
	return controller, factory, driver
}

// createPromptSession runs one successful create, wires the recording
// driver to the live session (so deliveries replay the durable append),
// and returns the identity.
func createPromptSession(t *testing.T, controller *SessionController, factory *createFakeFactory, driver *promptRecordingDriver) session.SessionID {
	t.Helper()
	value, err := controller.Create(context.Background(), map[string]any{"cwd": `C:\tmp`})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	created, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("create value = %#v", value)
	}
	sessionID := session.SessionID(created["sessionId"].(string))
	if live := factory.registry.Get(sessionID); live != nil {
		driver.session = live.Session
	}
	return sessionID
}

func TestPromptQueueDeliversFollowupWithEchoSource(t *testing.T) {
	controller, factory, driver := newPromptController(t, &promptFakeStore{})
	sessionID := createPromptSession(t, controller, factory, driver)

	value, err := controller.Prompt(context.Background(), map[string]any{
		"sessionId": string(sessionID),
		"requestId": "req-1",
		"mode":      "queue",
		"content":   []any{map[string]any{"type": "text", "text": "hello"}},
	})
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if value.(map[string]any)["accepted"] != true {
		t.Fatalf("value = %#v", value)
	}
	followups, steers, _ := driver.counts()
	if followups != 1 || steers != 0 {
		t.Fatalf("deliveries = %d followups / %d steers", followups, steers)
	}
	message := driver.followups[0]
	if message.Source.Kind != llm.SourceUser || message.Source.RPCID != "req-1" {
		t.Fatalf("source = %+v", message.Source)
	}
	if len(message.Content) != 1 || message.Content[0].Type != llm.BlockText || message.Content[0].Text != "hello" {
		t.Fatalf("content = %+v", message.Content)
	}
}

// The submission-echo dedup: a retry carrying the same requestId (a lost
// response) must not double-submit.
func TestPromptEchoDedupAcceptsWithoutRedelivery(t *testing.T) {
	controller, factory, driver := newPromptController(t, &promptFakeStore{})
	sessionID := createPromptSession(t, controller, factory, driver)
	request := map[string]any{
		"sessionId": string(sessionID),
		"requestId": "req-echo",
		"content":   []any{map[string]any{"type": "text", "text": "once"}},
	}
	if _, err := controller.Prompt(context.Background(), request); err != nil {
		t.Fatalf("first prompt: %v", err)
	}
	if _, err := controller.Prompt(context.Background(), request); err != nil {
		t.Fatalf("retry prompt: %v", err)
	}
	followups, _, _ := driver.counts()
	if followups != 1 {
		t.Fatalf("followups = %d, want the echo deduped to one delivery", followups)
	}
}

func TestPromptSteerModeDeliversSteer(t *testing.T) {
	controller, factory, driver := newPromptController(t, &promptFakeStore{})
	sessionID := createPromptSession(t, controller, factory, driver)
	if _, err := controller.Prompt(context.Background(), map[string]any{
		"sessionId": string(sessionID),
		"requestId": "req-steer",
		"mode":      "steer",
		"content":   []any{map[string]any{"type": "text", "text": "redirect"}},
	}); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	followups, steers, _ := driver.counts()
	if steers != 1 || followups != 0 {
		t.Fatalf("deliveries = %d followups / %d steers", followups, steers)
	}
}

func TestPromptAdmitsImageThroughStore(t *testing.T) {
	controller, factory, driver := newPromptController(t, &promptFakeStore{})
	sessionID := createPromptSession(t, controller, factory, driver)
	if _, err := controller.Prompt(context.Background(), map[string]any{
		"sessionId": string(sessionID),
		"requestId": "req-img",
		"content": []any{
			map[string]any{"type": "text", "text": "look"},
			map[string]any{"type": "image", "mediaType": "image/png", "data": "aGVsbG8="},
		},
	}); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	followups, _, _ := driver.counts()
	if followups != 1 {
		t.Fatalf("followups = %d", followups)
	}
	content := driver.followups[0].Content
	if len(content) != 2 || content[0].Type != llm.BlockText || content[1].Type != llm.BlockImage {
		t.Fatalf("content = %+v", content)
	}
	ref, ok := content[1].Attachment.(attachment.ImageAttachmentRef)
	if !ok || ref.MediaType != "image/png" || ref.AttachmentID == "" {
		t.Fatalf("attachment = %#v", content[1].Attachment)
	}
}

func TestPromptRejectsFilePartHonestly(t *testing.T) {
	controller, factory, driver := newPromptController(t, &promptFakeStore{})
	sessionID := createPromptSession(t, controller, factory, driver)
	_, err := controller.Prompt(context.Background(), map[string]any{
		"sessionId": string(sessionID),
		"requestId": "req-file",
		"content":   []any{map[string]any{"type": "file", "receiptId": "rcpt-1"}},
	})
	gerr := asGatewayError(t, err)
	if gerr == nil || gerr.Code != "session/attachment-invalid" {
		t.Fatalf("err = %v, want session/attachment-invalid", err)
	}
	followups, _, _ := driver.counts()
	if followups != 0 {
		t.Fatalf("followups = %d, want none", followups)
	}
}

func TestPromptValidatesClientTimeZone(t *testing.T) {
	controller, factory, driver := newPromptController(t, &promptFakeStore{})
	sessionID := createPromptSession(t, controller, factory, driver)
	_, err := controller.Prompt(context.Background(), map[string]any{
		"sessionId":      string(sessionID),
		"requestId":      "req-tz",
		"clientTimeZone": "Mars/Olympus",
		"content":        []any{map[string]any{"type": "text", "text": "hi"}},
	})
	gerr := asGatewayError(t, err)
	if gerr == nil || gerr.Code != "session/invalid-time-zone" {
		t.Fatalf("err = %v, want session/invalid-time-zone", err)
	}
}

func TestPromptAnswersNotFoundForColdSession(t *testing.T) {
	controller, _, _ := newPromptController(t, &promptFakeStore{})
	_, err := controller.Prompt(context.Background(), map[string]any{
		"sessionId": "session-missing",
		"requestId": "req-x",
		"content":   []any{map[string]any{"type": "text", "text": "hi"}},
	})
	gerr := asGatewayError(t, err)
	if gerr == nil || gerr.Code != "session/not-found" {
		t.Fatalf("err = %v, want session/not-found", err)
	}
}

func TestPromptRejectsInvalidMode(t *testing.T) {
	controller, factory, driver := newPromptController(t, &promptFakeStore{})
	sessionID := createPromptSession(t, controller, factory, driver)
	_, err := controller.Prompt(context.Background(), map[string]any{
		"sessionId": string(sessionID),
		"requestId": "req-mode",
		"mode":      "interrupt",
		"content":   []any{map[string]any{"type": "text", "text": "hi"}},
	})
	if err == nil || !strings.Contains(err.Error(), "queue or steer") {
		t.Fatalf("err = %v, want the mode refusal", err)
	}
}

func TestCancelForwardsUserCauseKeepInbox(t *testing.T) {
	controller, factory, driver := newPromptController(t, &promptFakeStore{})
	sessionID := createPromptSession(t, controller, factory, driver)
	value, err := controller.Cancel(context.Background(), map[string]any{"sessionId": string(sessionID)})
	if err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if value.(map[string]any)["accepted"] != true {
		t.Fatalf("value = %#v", value)
	}
	_, _, cancels := driver.counts()
	if cancels != 1 {
		t.Fatalf("cancels = %d", cancels)
	}
	if driver.cancels[0].Kind != session.CancelUser {
		t.Fatalf("cause = %+v", driver.cancels[0])
	}
}

func TestCancelAnswersNotFoundForColdSession(t *testing.T) {
	controller, _, _ := newPromptController(t, &promptFakeStore{})
	_, err := controller.Cancel(context.Background(), map[string]any{"sessionId": "session-missing"})
	gerr := asGatewayError(t, err)
	if gerr == nil || gerr.Code != "session/not-found" {
		t.Fatalf("err = %v, want session/not-found", err)
	}
}
