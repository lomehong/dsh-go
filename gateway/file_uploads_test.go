// Tests for the staged file-upload service: the receipt authority fence,
// the raw-byte HTTP route contract, and the prompt file-part resolution.
package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dshgo/agent"
	"dshgo/attachment"
	"dshgo/cordis"
	"dshgo/session"
)

// uploadFakeFileStore is the durable file-store stand-in.
type uploadFakeFileStore struct{}

func (s *uploadFakeFileStore) SaveFile(input attachment.SaveFileAttachment) (attachment.FileAttachmentRef, error) {
	return attachment.FileAttachmentRef{
		AttachmentID: "sha256-abc", Name: input.Name, Bytes: len(input.Data),
	}, nil
}

func (s *uploadFakeFileStore) SaveFileStream(input attachment.SaveFileStreamAttachment) (attachment.FileAttachmentRef, error) {
	data := make([]byte, 0, 64)
	buffer := make([]byte, 32)
	for {
		n, err := input.Data.Read(buffer)
		data = append(data, buffer[:n]...)
		if err != nil {
			break
		}
	}
	return s.SaveFile(attachment.SaveFileAttachment{Data: data, Name: input.Name})
}

func (s *uploadFakeFileStore) FileHostPath(attachment.FileAttachmentRef) (string, bool, error) {
	return "", false, nil
}

// newUploadService builds the service over a live agent registry.
func newUploadService(t *testing.T) (*FileUploads, *agent.AgentRegistry) {
	t.Helper()
	registry := agent.NewAgentRegistry(nil, cordis.Discard{})
	return NewFileUploads(&uploadFakeFileStore{}, registry), registry
}

// registerLiveUploadAgent materializes one live agent in the registry.
func registerLiveUploadAgent(t *testing.T, registry *agent.AgentRegistry, id string) error {
	t.Helper()
	sess, err := session.NewDetached(session.SessionID(id), nil,
		&session.SessionHeader{Version: session.SESSION_FORMAT_VERSION, ID: session.SessionID(id)}, 0)
	if err != nil {
		return err
	}
	built := agent.NewAgent(agent.AgentConfig{
		ID: session.SessionID(id), Session: sess, Ctx: cordis.NewRoot(cordis.Discard{}).Child(),
	}, registry.Events())
	_, err = registry.Register(built)
	return err
}

// The upload authority requires a live receiving session: a cold identity
// is refused before any byte is stored.
func TestUploadRefusesColdSession(t *testing.T) {
	service, _ := newUploadService(t)
	if _, err := service.Upload(context.Background(), "session-cold", "a.txt", []byte("data")); err == nil ||
		!strings.Contains(err.Error(), "not live") {
		t.Fatalf("err = %v, want the not-live refusal", err)
	}
}

// The receipt binds to its receiving session: another session presenting it
// is an authorization failure.
func TestResolveFencesReceiptToItsSession(t *testing.T) {
	service, registry := newUploadService(t)
	if err := registerLiveUploadAgent(t, registry, "session-a"); err != nil {
		t.Fatalf("register: %v", err)
	}
	value, err := service.Upload(context.Background(), "session-a", "a.txt", []byte("data"))
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	owner := registry.Get("session-a")
	if _, err := service.Resolve(owner, value.ReceiptID); err != nil {
		t.Fatalf("resolve for the owning agent: %v", err)
	}
	if err := registerLiveUploadAgent(t, registry, "session-b"); err != nil {
		t.Fatalf("register b: %v", err)
	}
	if _, err := service.Resolve(registry.Get("session-b"), value.ReceiptID); err == nil {
		t.Fatal("a foreign session resolved another session's receipt")
	}
}

// The raw-byte route contract: method, media type, session identity, and
// the ok envelope.
func TestUploadHTTPRouteContract(t *testing.T) {
	service, registry := newUploadService(t)
	if err := registerLiveUploadAgent(t, registry, "session-a"); err != nil {
		t.Fatalf("register: %v", err)
	}
	handler := service.HandleHTTP()

	// Non-POST: 405 with the allow header.
	request := httptest.NewRequest(http.MethodGet, "/api/session/uploadFileBinary", nil)
	recorder := httptest.NewRecorder()
	handler(recorder, request)
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d", recorder.Code)
	}

	// Wrong media type: 415.
	request = httptest.NewRequest(http.MethodPost, "/api/session/uploadFileBinary?sessionId=session-a", strings.NewReader("x"))
	request.Header.Set("Content-Type", "text/plain")
	recorder = httptest.NewRecorder()
	handler(recorder, request)
	if recorder.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("text/plain status = %d", recorder.Code)
	}

	// Missing sessionId: 400.
	request = httptest.NewRequest(http.MethodPost, "/api/session/uploadFileBinary", strings.NewReader("x"))
	request.Header.Set("Content-Type", "application/octet-stream")
	recorder = httptest.NewRecorder()
	handler(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("no-session status = %d", recorder.Code)
	}

	// The happy path answers 200 with the ok envelope and the receipt.
	request = httptest.NewRequest(http.MethodPost,
		"/api/session/uploadFileBinary?sessionId=session-a&name=report.txt", strings.NewReader("file bytes"))
	request.Header.Set("Content-Type", "application/octet-stream")
	recorder = httptest.NewRecorder()
	handler(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("happy status = %d", recorder.Code)
	}
	body := recorder.Body.String()
	for _, want := range []string{`"ok":true`, `"receiptId"`, `"sha256-abc"`, `"report.txt"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("body = %s, want %s", body, want)
		}
	}
}

// The prompt file part resolves its receipt into the durable file block,
// and an unknown receipt refuses without delivering.
func TestPromptFilePartResolvesReceipt(t *testing.T) {
	controller, factory, driver := newPromptController(t, &promptFakeStore{})
	sessionID := createPromptSession(t, controller, factory, driver)

	uploads := NewFileUploads(&uploadFakeFileStore{}, factory.registry)
	controller.EnableCreate(SessionCreateDeps{
		Agents:      func() any { return factory.registry },
		Attachments: func() any { return &promptFakeStore{} },
		Uploads:     func() any { return uploads },
	})

	// An unknown receipt refuses gently (attachment-invalid), nothing is
	// delivered.
	_, err := controller.Prompt(context.Background(), map[string]any{
		"sessionId": string(sessionID),
		"requestId": "req-file-bad",
		"content":   []any{map[string]any{"type": "file", "receiptId": "missing"}},
	})
	gerr := asGatewayError(t, err)
	if gerr == nil || gerr.Code != "session/attachment-invalid" {
		t.Fatalf("err = %v, want session/attachment-invalid", err)
	}
	if followups, _, _ := driver.counts(); followups != 0 {
		t.Fatalf("followups = %d, want none for the refused prompt", followups)
	}

	// The staged receipt resolves into the durable file block.
	staged, err := uploads.Upload(context.Background(), sessionID, "notes.txt", []byte("file body"))
	if err != nil {
		t.Fatalf("stage upload: %v", err)
	}
	if _, err := controller.Prompt(context.Background(), map[string]any{
		"sessionId": string(sessionID),
		"requestId": "req-file-ok",
		"content": []any{
			map[string]any{"type": "text", "text": "summarize"},
			map[string]any{"type": "file", "receiptId": staged.ReceiptID},
		},
	}); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	followups, _, _ := driver.counts()
	if followups != 1 {
		t.Fatalf("followups = %d", followups)
	}
	content := driver.followups[0].Content
	if len(content) != 2 || content[0].Type != "text" || content[1].Type != "file" {
		t.Fatalf("content = %+v", content)
	}
	ref, ok := content[1].Attachment.(map[string]any)
	if !ok || ref["attachmentId"] != "sha256-abc" {
		t.Fatalf("file attachment = %#v", content[1].Attachment)
	}
}
