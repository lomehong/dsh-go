package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"dshgo/gatewaystream"
)

// eventsEndpoint is the gateway-internal forwarded-events stream
// (gatewaystream.RemoteEventStreamEndpoint).
const eventsEndpoint = gatewaystream.RemoteEventStreamEndpoint

// OpenWireStream adapts the Gateway to the gatewaystream opener contract.
// The $events internal endpoint forwards to OpenRemoteEvents; every other
// endpoint fails service-unavailable until its Go stream port lands. The
// wire payload is the standard Remote shape {args: {...}}; the args object
// is unwrapped before dispatch.
func (g *Gateway) OpenWireStream(endpoint string, payload any) (<-chan any, <-chan error, func()) {
	errCh := make(chan error, 1)
	args := map[string]any{}
	if m, ok := payload.(map[string]any); ok {
		if raw, ok := m["args"]; ok {
			if am, ok := raw.(map[string]any); ok {
				args = am
			}
		}
	}
	if endpoint != eventsEndpoint {
		errCh <- wrapGatewayError("gateway/service-unavailable", endpoint, "", nil, "stream endpoint %q has no Go port", endpoint)
		close(errCh)
		return nil, errCh, func() {}
	}
	items, done, cancel, err := g.OpenRemoteEvents(args, context.Background())
	if err != nil {
		errCh <- err
		close(errCh)
		return nil, errCh, func() {}
	}
	out := make(chan any)
	go func() {
		defer close(out)
		for value := range items {
			out <- value
		}
		<-done
		errCh <- nil
		close(errCh)
	}()
	return out, errCh, cancel
}

// legacyHostDescription answers the apiproxy-era bootstrap call with the
// host facts the old browser readiness gate consumes (official
// hostDescribeValueSchema): version/cwd/provider?/model?/attachedSessions/
// home/canOpenPath. provider/model stay absent (optional in the schema);
// no secret value is ever included.
func legacyHostDescription() map[string]any {
	cwd, _ := os.Getwd()
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	return map[string]any{
		"version":          "0.0.1",
		"cwd":              cwd,
		"attachedSessions": 0,
		"home":             home,
		"canOpenPath":      true,
	}
}

// splitEndpoint splits one RPC endpoint into namespace and method. The
// browser writes `namespace.method` (POST /api/host.describe); the
// gateway-internal and Go-test callers write `namespace/method`. Both forms
// parse; a missing separator fails.
func splitEndpoint(endpoint string) (namespace string, method string, ok bool) {
	if i := strings.Index(endpoint, "."); i > 0 {
		return endpoint[:i], endpoint[i+1:], endpoint[i+1:] != ""
	}
	if i := strings.Index(endpoint, "/"); i > 0 {
		return endpoint[:i], endpoint[i+1:], endpoint[i+1:] != ""
	}
	return "", "", false
}

// unaryRequest is the wire envelope of one unary Remote call (official
// ClientRequest): POST /api/<namespace>/<method> with this JSON body.
type unaryRequest struct {
	Type    string         `json:"type"`
	RPCID   string         `json:"rpcId"`
	Method  string         `json:"method"`
	Payload map[string]any `json:"payload"`
}

// UnaryHandler serves the browser unary carrier: POST /api/<namespace>.<method>
// with the client-request envelope, answered with the server-response
// envelope. The browser composes the endpoint as `namespace.method`
// (empirically: POST /api/host.describe), while the gateway-internal and
// Go-test callers use `namespace/method`; both separators are accepted.
// Status semantics follow the official rpc-host: 404 non-POST or unclaimed
// endpoint, 415 wrong content type, 400 bad body/envelope, 500 handler
// crash; business failures ride a 200 body with ok:false.
func (g *Gateway) UnaryHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		endpoint := strings.TrimPrefix(r.URL.Path, "/api/")
		namespace, method, ok := splitEndpoint(endpoint)
		if r.Method != http.MethodPost || !ok {
			http.NotFound(w, r)
			return
		}
		if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			http.Error(w, "unsupported media type", http.StatusUnsupportedMediaType)
			return
		}
		var message unaryRequest
		if err := json.NewDecoder(r.Body).Decode(&message); err != nil {
			http.Error(w, "malformed JSON body", http.StatusBadRequest)
			return
		}
		if message.Type != "client-request" || message.RPCID == "" || message.Method != endpoint {
			http.Error(w, "invalid request envelope", http.StatusBadRequest)
			return
		}
		args := message.Payload
		if args == nil {
			args = map[string]any{}
		}
		// The standard Remote payload wraps named args one level deep
		// (official remoteRequest(): `args: payload.args`); tolerate the
		// bare-args form defensively.
		if len(args) == 1 {
			if inner, ok := args["args"].(map[string]any); ok {
				args = inner
			}
		}
		var value any
		var err error
		if namespace == "host" && method == "describe" {
			// The legacy bootstrap method (apiproxy-era wire): the browser
			// readiness gate consumes these host facts before any typert
			// namespace resolves (official hostDescribeValueSchema).
			value = legacyHostDescription()
		} else {
			value, err = g.Invoke(r.Context(), InvokeRequest{
				Namespace: namespace,
				Method:    method,
				Args:      args,
				Signal:    r.Context(),
			})
		}
		w.Header().Set("Content-Type", "application/json")
		result := map[string]any{}
		if err == nil {
			result["ok"] = true
			result["value"] = value
		} else {
			var gerr *GatewayError
			if !errors.As(err, &gerr) {
				gerr = wrapGatewayError("gateway/unknown", endpoint, "", err, "")
			}
			result["ok"] = false
			result["error"] = map[string]any{
				"code":    string(gerr.Code),
				"message": gerr.message,
			}
		}
		if err := json.NewEncoder(w).Encode(map[string]any{
			"type":   "server-response",
			"rpcId":  message.RPCID,
			"result": result,
		}); err != nil {
			fmt.Println(fmt.Sprintf("dsh: unary encode: %v", err))
		}
	}
}
