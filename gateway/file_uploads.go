// Staged file uploads for the api-session namespace (official
// dsh-client-file-upload host half + the fileUploads service the session
// controller injects): the authenticated raw-byte route
// (POST /api/session/uploadFileBinary), the base64 Remote fallback
// (fileUploads.upload), and the receipt authority the prompt endpoint
// resolves. Receipts are process-local authority tokens bound to one live
// session; the durable artifact is the content-addressed file reference.
package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"dshgo/agent"
	"dshgo/attachment"
	"dshgo/identity"
	"dshgo/session"
	"dshgo/typert"
)

// FileUploadValue is the durable receipt for one staged upload (official
// FileUploadValue).
type FileUploadValue struct {
	// ReceiptID is the per-upload authority accepted only inside the
	// receiving Agent scope.
	ReceiptID string                       `json:"receiptId"`
	File      attachment.FileAttachmentRef `json:"file"`
}

// fileUploadReceipt binds one receipt to its receiving session.
type fileUploadReceipt struct {
	sessionID session.SessionID
	file      attachment.FileAttachmentRef
}

// FileUploads stages browser file uploads into durable references and mints
// the per-agent receipts the prompt endpoint resolves.
type FileUploads struct {
	store  attachment.FileStore
	agents *agent.AgentRegistry

	mu       sync.Mutex
	receipts map[string]fileUploadReceipt
}

// NewFileUploads binds the service to the durable file store and the live
// agent registry (the upload authority requires a live receiving session).
func NewFileUploads(store attachment.FileStore, agents *agent.AgentRegistry) *FileUploads {
	return &FileUploads{store: store, agents: agents, receipts: map[string]fileUploadReceipt{}}
}

// Upload stages one file: the receiving session must be live (the official
// registerAgentResolver gate), the bytes commit verbatim, and the minted
// receipt binds to the session.
func (s *FileUploads) Upload(ctx context.Context, sessionID session.SessionID, name string, data []byte) (FileUploadValue, error) {
	if s.agents.Get(sessionID) == nil {
		return FileUploadValue{}, fmt.Errorf("session %q is not live", sessionID)
	}
	ref, err := s.store.SaveFile(attachment.SaveFileAttachment{Data: data, Name: name})
	if err != nil {
		return FileUploadValue{}, err
	}
	receiptID := identity.RandomUUID()
	s.mu.Lock()
	s.receipts[receiptID] = fileUploadReceipt{sessionID: sessionID, file: ref}
	s.mu.Unlock()
	return FileUploadValue{ReceiptID: receiptID, File: ref}, nil
}

// Resolve turns one prompt file part's receipt into its durable reference.
// The receipt is valid only inside the receiving Agent's scope: a foreign
// session presenting another session's receipt is an authorization failure.
func (s *FileUploads) Resolve(agentObj *agent.Agent, receiptID string) (attachment.FileAttachmentRef, error) {
	s.mu.Lock()
	receipt, ok := s.receipts[receiptID]
	s.mu.Unlock()
	if !ok {
		return attachment.FileAttachmentRef{}, fmt.Errorf("unknown file upload receipt %q", receiptID)
	}
	if agentObj == nil || receipt.sessionID != agentObj.ID {
		return attachment.FileAttachmentRef{}, fmt.Errorf(
			"file upload receipt %q was not staged for session %q", receiptID, agentObj.ID)
	}
	return receipt.file, nil
}

// HandleHTTP serves the authenticated raw-byte upload route (official
// handleFileUploadHttp): POST only, application/octet-stream only,
// sessionId query required, name query optional, results answered 200 with
// the ok envelope.
func (s *FileUploads) HandleHTTP() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		mediaType := ""
		if raw := r.Header.Get("Content-Type"); raw != "" {
			if index := strings.Index(raw, ";"); index >= 0 {
				mediaType = raw[:index]
			} else {
				mediaType = raw
			}
			mediaType = strings.ToLower(strings.TrimSpace(mediaType))
		}
		if mediaType != "application/octet-stream" {
			http.Error(w, "content type must be application/octet-stream", http.StatusUnsupportedMediaType)
			return
		}
		query := r.URL.Query()
		sessionID := query.Get("sessionId")
		if sessionID == "" {
			http.Error(w, "sessionId is required", http.StatusBadRequest)
			return
		}
		data, err := io.ReadAll(r.Body)
		if err != nil {
			s.writeUploadResult(w, FileUploadValue{}, wrapGatewayError("gateway/internal", "session/uploadFileBinary", "", err, "%v", err))
			return
		}
		value, uploadErr := s.Upload(r.Context(), session.SessionID(sessionID), query.Get("name"), data)
		s.writeUploadResult(w, value, uploadErr)
	}
}

// writeUploadResult renders the ok envelope the browser client validates
// (200 always; business failures ride the body).
func (s *FileUploads) writeUploadResult(w http.ResponseWriter, value FileUploadValue, err error) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	encoder := json.NewEncoder(w)
	if err == nil {
		_ = encoder.Encode(map[string]any{"ok": true, "value": value})
		return
	}
	var gerr *GatewayError
	message := err.Error()
	code := "gateway/internal"
	if errors.As(err, &gerr) {
		code = string(gerr.Code)
		message = gerr.message
	}
	_ = encoder.Encode(map[string]any{"ok": false, "error": map[string]any{"code": code, "message": message, "details": map[string]any{}}})
}

// Contribution registers the base64 Remote fallback (official
// fileUploads.upload): the same receipt value without the raw-byte route.
func (s *FileUploads) Contribution() typert.Contribution {
	jsonCodec := typert.Codec{Mode: typert.CodecSrcJSON}
	requestParam := typert.InvocationParameterDescriptor{
		Name: "_request", Wire: "_request", Source: typert.SourceJSON, Codec: jsonCodec,
	}
	return typert.Contribution{
		Package: "file-uploads",
		Face:    typert.FaceHost,
		Invocations: func() []typert.InvocationDescriptor {
			return []typert.InvocationDescriptor{{
				ID: "fileUploads.upload", Service: "fileUploadsController", Namespace: "fileUploads", Method: "upload",
				Implementation:        "UploadRemote",
				Invocation:            typert.InvocationReceiver{Kind: typert.ReceiverDirect},
				CancellationParameter: "signal",
				Parameters:            []typert.InvocationParameterDescriptor{requestParam},
				Result:                jsonCodec,
			}}
		}(),
	}
}

// UploadRemote is the Remote fallback body: {sessionId, data, name?} over
// canonical base64.
func (s *FileUploads) UploadRemote(ctx context.Context, request map[string]any) (any, error) {
	sessionID := session.SessionID(requestString(request, "sessionId"))
	data := requestString(request, "data")
	if sessionID == "" {
		return nil, wrapGatewayError("gateway/arguments-invalid", "fileUploads/upload", "sessionId", nil, "file upload requires a sessionId")
	}
	if data == "" {
		return nil, wrapGatewayError("gateway/arguments-invalid", "fileUploads/upload", "data", nil, "file upload requires base64 data")
	}
	decoded, decodeErr := base64.StdEncoding.DecodeString(data)
	if decodeErr != nil {
		return nil, wrapGatewayError("gateway/arguments-invalid", "fileUploads/upload", "data", decodeErr, "%v", decodeErr)
	}
	value, err := s.Upload(ctx, sessionID, requestString(request, "name"), decoded)
	if err != nil {
		return nil, wrapGatewayError("session/not-found", "fileUploads/upload", "sessionId", err, "%v", err)
	}
	return value, nil
}
