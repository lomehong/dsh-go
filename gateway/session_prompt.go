// Session prompt + cancel for the api-session-controller Remote namespace.
// Port of packages/api/session-controller/src/commands.ts prompt() and
// cancel(): client-time-zone validation, live-agent resolution with the
// subagent-ownership fence, submission-echo dedup by the client-minted
// request id, the route-served and model-modality gates, prompt content
// admission through the shared attachment store, and the steer/followup
// delivery split. File receipts (fileUploads) stay unported — a file part
// answers the honest attachment-invalid wording until the upload wire
// round.
package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"time"

	"dshgo/agent"
	"dshgo/attachment"
	"dshgo/llm"
	"dshgo/session"
)

// ianaTimeZoneShape matches the Host-canonicalized browser zone: canonical
// UTC or an IANA Area/Location path (the timecontext request-zone grammar).
var ianaTimeZoneShape = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_+.-]*(?:/[A-Za-z0-9_+.-]+)+$`)

// canonicalClientTimeZone reports whether one clientTimeZone value is Host
// canonical: UTC or an IANA Area/Location that loads in this runtime.
func canonicalClientTimeZone(value string) bool {
	if value == "UTC" {
		return true
	}
	if !ianaTimeZoneShape.MatchString(value) {
		return false
	}
	_, err := time.LoadLocation(value)
	return err == nil
}

// promptContentPart is one decoded browser prompt content part (official
// PromptContentPart: text | image | file-receipt).
type promptContentPart struct {
	Type      string
	Text      string
	MediaType string
	Data      string
	Name      string
	ReceiptID string
}

// decodePromptContentParts reads the wire content array. A part that is not
// an object with a known type is an arguments failure.
func decodePromptContentParts(request map[string]any) ([]promptContentPart, error) {
	raw, ok := request["content"].([]any)
	if !ok {
		return nil, fmt.Errorf("content must be an array of prompt parts")
	}
	parts := make([]promptContentPart, 0, len(raw))
	for index, item := range raw {
		fields, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("content part %d must be an object", index)
		}
		part := promptContentPart{Type: requestString(fields, "type")}
		switch part.Type {
		case "text":
			part.Text = requestString(fields, "text")
		case "image":
			part.MediaType = requestString(fields, "mediaType")
			part.Data = requestString(fields, "data")
			part.Name = requestString(fields, "name")
			if part.MediaType == "" || part.Data == "" {
				return nil, fmt.Errorf("content part %d (image) requires mediaType and data", index)
			}
		case "file":
			part.ReceiptID = requestString(fields, "receiptId")
			if part.ReceiptID == "" {
				return nil, fmt.Errorf("content part %d (file) requires receiptId", index)
			}
		default:
			return nil, fmt.Errorf("content part %d names unsupported type %q", index, part.Type)
		}
		parts = append(parts, part)
	}
	return parts, nil
}

// Prompt admits one browser prompt into the target live Agent (official
// session/prompt). Acceptance means the message entered the Agent's inbox —
// queue appends a turn, steer submits for the nearest step boundary.
func (c *SessionController) Prompt(ctx context.Context, request map[string]any) (any, error) {
	if !c.createReady() {
		return nil, wrapGatewayError("gateway/not-composed", "session/prompt", "", nil, "session prompt is not composed on this profile")
	}
	requestID := requestString(request, "requestId")
	sessionID := session.SessionID(requestString(request, "sessionId"))
	mode := requestString(request, "mode")
	if mode == "" {
		mode = "queue"
	}
	if requestID == "" {
		return nil, wrapGatewayError("gateway/arguments-invalid", "session/prompt", "requestId", nil, "session prompt requires a requestId")
	}
	if sessionID == "" {
		return nil, wrapGatewayError("gateway/arguments-invalid", "session/prompt", "sessionId", nil, "session prompt requires a sessionId")
	}
	if mode != "queue" && mode != "steer" {
		return nil, wrapGatewayError("gateway/arguments-invalid", "session/prompt", "mode", nil, "session prompt mode must be queue or steer")
	}
	clientTimeZone := requestString(request, "clientTimeZone")
	if clientTimeZone != "" && !canonicalClientTimeZone(clientTimeZone) {
		return nil, wrapGatewayError("session/invalid-time-zone", "session/prompt", "clientTimeZone", nil,
			"clientTimeZone must be UTC or a valid IANA Area/Location name")
	}

	live := c.liveAgent(sessionID)
	if live == nil {
		return nil, wrapGatewayError("session/not-found", "session/prompt", "sessionId", nil, "session %q is not live", sessionID)
	}
	if depth := live.Session.Header().DelegationDepth; depth != nil && *depth > 0 {
		return nil, wrapGatewayError("session/subagent-owned", "session/prompt", "sessionId", nil,
			"session %q is owned by a live subagent", sessionID)
	}

	// Submission echo dedup: the browser mints one requestId ahead of the
	// RPC; a retry after a lost response must not double-submit.
	if c.hasPromptRequest(live, requestID) {
		return map[string]any{"accepted": true}, nil
	}

	parts, err := decodePromptContentParts(request)
	if err != nil {
		return nil, wrapGatewayError("gateway/arguments-invalid", "session/prompt", "content", err, "%v", err)
	}

	// Route-served gate: the session's selection must name a provider this
	// deployment serves (official routeServed over the default/installed
	// selection).
	selection := c.selectionFor(live)
	rt := c.runtime()
	served := false
	if rt != nil {
		for _, provider := range rt.ListProviders() {
			if provider.ID == selection.Provider {
				served = true
				break
			}
		}
	}
	if !served {
		return nil, wrapGatewayError("session/model-unavailable", "session/prompt", "", nil,
			"no adapter serves provider %q; select a model for this session", selection.Provider)
	}

	hasImage := false
	for _, part := range parts {
		if part.Type == "image" {
			hasImage = true
			break
		}
	}

	// Content assembly: text rides verbatim; image bytes admit through the
	// shared attachment store behind the model-modality gate; file receipts
	// have no upload wire yet and answer honestly.
	blocks := make([]llm.ContentBlock, 0, len(parts))
	for _, part := range parts {
		switch part.Type {
		case "text":
			blocks = append(blocks, llm.ContentBlock{Type: llm.BlockText, Text: part.Text})
		case "image":
			if hasImage && rt != nil {
				if resolved, err := rt.ResolveModelInfo(selection.Provider, selection.Model); err == nil &&
					resolved.InputModalities != nil && !containsString(resolved.InputModalities, "image") {
					return nil, wrapGatewayError("session/attachment-invalid", "session/prompt", "", nil,
						"Model %q does not support image input.", selection.Model)
				}
			}
			store := c.attachmentStore()
			if store == nil {
				return nil, wrapGatewayError("session/attachment-invalid", "session/prompt", "", nil,
					"image input is unavailable: no attachment store is composed")
			}
			admitted, err := attachment.AdmitPromptContent(store, []llm.ContentBlock{{
				Type: llm.BlockImage,
				Attachment: attachment.EncodedImageAttachment{
					MediaType: part.MediaType, Data: part.Data, Name: part.Name,
				},
			}})
			if err != nil {
				return nil, wrapGatewayError("session/attachment-invalid", "session/prompt", "", err, "%v", err)
			}
			blocks = append(blocks, admitted...)
		case "file":
			uploads := c.uploads()
			if uploads == nil {
				return nil, wrapGatewayError("session/attachment-invalid", "session/prompt", "", nil,
					"file uploads are not composed on this deployment")
			}
			ref, resolveErr := uploads.Resolve(live, part.ReceiptID)
			if resolveErr != nil {
				return nil, wrapGatewayError("session/attachment-invalid", "session/prompt", "", resolveErr, "%v", resolveErr)
			}
			// The durable file block: name/bytes ride the reference (the
			// provider-side projection turns it into read-handle text).
			blocks = append(blocks, llm.ContentBlock{Type: llm.BlockFile, Attachment: map[string]any{
				"attachmentId": ref.AttachmentID, "name": ref.Name, "bytes": ref.Bytes,
			}})
		}
	}

	message := llm.NewUserMessage(blocks, llm.MessageSource{
		Kind: llm.SourceUser, RPCID: requestID, ClientTimeZone: clientTimeZone,
	})

	// Liveness re-check: the agent may dispose during admission (official
	// `session "x" was disposed during prompt admission`).
	if c.liveAgent(sessionID) == nil {
		return nil, wrapGatewayError("session/not-found", "session/prompt", "sessionId", nil,
			"session %q was disposed during prompt admission", sessionID)
	}

	driver := live.Driver()
	if mode == "steer" {
		driver.Steer(message)
	} else {
		driver.Followup(message)
	}
	return map[string]any{"accepted": true}, nil
}

// Cancel cancels one live ordinary Agent's active turn while retaining
// pending inbox work (official session/cancel: `cancel({kind:'user'},
// {keepInbox:true})`).
func (c *SessionController) Cancel(ctx context.Context, request map[string]any) (any, error) {
	if !c.createReady() {
		return nil, wrapGatewayError("gateway/not-composed", "session/cancel", "", nil, "session cancel is not composed on this profile")
	}
	sessionID := session.SessionID(requestString(request, "sessionId"))
	if sessionID == "" {
		return nil, wrapGatewayError("gateway/arguments-invalid", "session/cancel", "sessionId", nil, "session cancel requires a sessionId")
	}
	live := c.liveAgent(sessionID)
	if live == nil {
		return nil, wrapGatewayError("session/not-found", "session/cancel", "sessionId", nil,
			"session %q not found (not attached)", sessionID)
	}
	if depth := live.Session.Header().DelegationDepth; depth != nil && *depth > 0 {
		return nil, wrapGatewayError("session/subagent-owned", "session/cancel", "sessionId", nil,
			"session %q is owned by a live subagent", sessionID)
	}
	live.Cancel(session.TurnEndCancelCause{Kind: session.CancelUser}, agent.CancelOptions{KeepInbox: true})
	return map[string]any{"accepted": true}, nil
}

// selectionFor resolves the agent's current model selection: the explicit
// session override (selectModel) first, then the persisted request header's
// route, else the deployment's installed default.
func (c *SessionController) selectionFor(live *agent.Agent) agent.ModelSelection {
	if selections := c.selections(); selections != nil {
		if selection, ok := selections.Get(live.ID); ok {
			return selection
		}
	}
	if header := live.Session.RequestHeader(); header != nil && header.Config.Provider != "" {
		return agent.ModelSelection{Provider: header.Config.Provider, Model: header.Config.Model}
	}
	if selection := c.defaultSelection(); selection.Provider != "" {
		return selection
	}
	return agent.ModelSelection{}
}

// hasPromptRequest reports whether the session log already carries the
// accepted durable user message for one browser submission echo.
func (c *SessionController) hasPromptRequest(live *agent.Agent, requestID string) bool {
	for _, event := range live.Session.Events() {
		if event.Type != session.EventUserMessage {
			continue
		}
		var message llm.Message
		if err := json.Unmarshal(event.Data, &message); err != nil {
			continue
		}
		if message.Source.Kind == llm.SourceUser && message.Source.RPCID == requestID {
			return true
		}
	}
	return false
}

// containsString is the membership test over one string slice.
func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
