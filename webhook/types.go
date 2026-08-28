// Package webhook ports packages/webhook/webhook/src/{types,brand,index}.ts:
// the fire-and-forget webhook rule registry. Session creation is the only
// built-in action and reaches it through a SessionCreator seam — the
// workspace-backed creation transaction (src/session.ts) composes the
// workspace/preset/permission services and lands with those rounds.
package webhook

import (
	"context"
	"encoding/json"
	"fmt"
)

// WebhookRuleID identifies one programmatic webhook rule.
type WebhookRuleID = string

// WebhookSourceID identifies one configured webhook adapter instance.
type WebhookSourceID = string

// WebhookDeliveryID identifies one provider delivery. The runtime assigns no
// deduplication semantics.
type WebhookDeliveryID = string

// VerifiedWebhookDelivery is one authenticated and parsed provider delivery.
// Event carries the provider-normalized lossless JSON: a known provider
// adapter's struct, or generic JSON data for an out-of-tree kind.
type VerifiedWebhookDelivery struct {
	// Kind is the provider family such as `github`.
	Kind string `json:"kind"`
	// Source is the configured adapter instance such as `primary-github`.
	Source WebhookSourceID `json:"source"`
	// DeliveryID is the provider identity exposed as provenance, never as
	// built-in deduplication state.
	DeliveryID WebhookDeliveryID `json:"deliveryId"`
	// Event is the provider-normalized lossless JSON payload.
	Event any `json:"event,omitempty"`
	// ReceivedAt is the host receipt time in Unix epoch milliseconds.
	ReceivedAt int64 `json:"receivedAt"`
}

// WebhookModelSelection is the optional explicit model route and output cap
// for a webhook-created Agent.
type WebhookModelSelection struct {
	// Provider is the registered provider route.
	Provider string `json:"provider"`
	// Model is the provider-owned model id.
	Model string `json:"model"`
	// MaxTokens is the optional positive output-token cap.
	MaxTokens *int64 `json:"maxTokens,omitempty"`
}

// WebhookSessionRequest is the sole runtime action: create and prompt one
// root Session.
type WebhookSessionRequest struct {
	// WorkspacePath is the existing local directory to resolve or create as
	// a Web Workspace.
	WorkspacePath string `json:"workspacePath"`
	// Title is the explicit Session title.
	Title string `json:"title"`
	// Prompt is the non-empty initial text prompt.
	Prompt string `json:"prompt"`
	// AgentPreset is the agent composition mounted before publication.
	AgentPreset string `json:"agentPreset"`
	// PermissionPreset is the sandbox and approval preset applied before
	// prompt admission.
	PermissionPreset string `json:"permissionPreset"`
	// Model is the optional explicit route; omission uses the complete
	// current default, including reasoning effort.
	Model *WebhookModelSelection `json:"model,omitempty"`
}

// Rule is trusted code that optionally creates one Session for a delivery.
type Rule interface {
	// ID is the globally unique diagnostic identity.
	ID() WebhookRuleID
	// Kind is the provider kind this rule receives.
	Kind() string
	// Run executes arbitrary trusted code and optionally requests one
	// Session. A nil request means no action. The signal aborts when this
	// registration or the runtime unloads.
	Run(delivery VerifiedWebhookDelivery, signal context.Context) (*WebhookSessionRequest, error)
}

// SessionCreator is the workspace-backed Session creation seam
// (createWebhookSession). It receives the exact verified delivery, the
// requesting rule's id, the resolved request, and the registration lifetime
// signal; successful prompt admission ends webhook ownership of the
// operation.
type SessionCreator func(delivery VerifiedWebhookDelivery, ruleID WebhookRuleID, request WebhookSessionRequest, signal context.Context) error

// snapshotDelivery validates and detaches one delivery before sharing it
// across arbitrary rules.
func snapshotDelivery(delivery VerifiedWebhookDelivery) (VerifiedWebhookDelivery, error) {
	if trimSpace(delivery.Kind) == "" {
		return VerifiedWebhookDelivery{}, fmt.Errorf("webhook delivery kind must be a non-empty string")
	}
	if trimSpace(delivery.Source) == "" {
		return VerifiedWebhookDelivery{}, fmt.Errorf("webhook delivery source must be a non-empty string")
	}
	if trimSpace(delivery.DeliveryID) == "" {
		return VerifiedWebhookDelivery{}, fmt.Errorf("webhook delivery id must be a non-empty string")
	}
	if delivery.ReceivedAt < 0 || delivery.ReceivedAt > maxSafeInteger {
		return VerifiedWebhookDelivery{}, fmt.Errorf("webhook delivery receivedAt must be a non-negative safe integer")
	}
	// snapshotJsonValue: one canonical JSON round-trip both proves the
	// delivery is lossless JSON and detaches it from the caller's objects.
	encoded, err := json.Marshal(delivery)
	if err != nil {
		return VerifiedWebhookDelivery{}, fmt.Errorf("webhook delivery must be lossless JSON")
	}
	var snapshot VerifiedWebhookDelivery
	if err := json.Unmarshal(encoded, &snapshot); err != nil {
		return VerifiedWebhookDelivery{}, fmt.Errorf("webhook delivery must be lossless JSON")
	}
	return snapshot, nil
}

// maxSafeInteger is the IEEE-754 double integer ceiling the source checks
// through Number.isSafeInteger.
const maxSafeInteger = int64(1)<<53 - 1

// trimSpace reports the value without surrounding whitespace (the trim()
// validation roles).
func trimSpace(value string) string {
	start := 0
	for start < len(value) && isSpaceByte(value[start]) {
		start++
	}
	end := len(value)
	for end > start && isSpaceByte(value[end-1]) {
		end--
	}
	return value[start:end]
}

func isSpaceByte(b byte) bool {
	switch b {
	case ' ', '\t', '\n', '\v', '\f', '\r':
		return true
	}
	return false
}
