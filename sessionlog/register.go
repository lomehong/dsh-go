// The dsh_session_log request contribution: the session-log package owns the
// one top-level DeepSeek request field carrying the incremental canonical
// log, registering its provider into the extension registry.
package sessionlog

import (
	"encoding/json"

	"dshgo/llm/deepseek"
	"dshgo/session"
)

// SessionLogField is the top-level request field this package owns.
const SessionLogField = "dsh_session_log"

// RegisterDeepseekField registers the sole provider of dsh_session_log.
// Each request with a session id resolves its live session; the prepared
// value is the canonical events the receiver has not confirmed, bracketed by
// the watermark interval, and acceptance records the watermark durably.
// Sessions without an id, unknown session ids, and empty logs contribute no
// field. The returned disposer releases the field.
func RegisterDeepseekField(registry *deepseek.ExtensionRegistry, sessions *session.Store, folder *Folder) (func(), error) {
	return registry.Register(SessionLogField, deepseek.FieldProviderFunc(func(request deepseek.RequestFacts) (deepseek.FieldValue, bool, error) {
		if request.SessionID == "" || sessions == nil || folder == nil {
			return deepseek.FieldValue{}, false, nil
		}
		sess := sessions.Get(session.SessionID(request.SessionID))
		if sess == nil {
			return deepseek.FieldValue{}, false, nil
		}
		prepared, err := Prepare(folder, sess)
		if err != nil {
			return deepseek.FieldValue{}, false, err
		}
		if prepared == nil {
			return deepseek.FieldValue{}, false, nil
		}
		// The registry merges JSON values; hand it a detached copy so the
		// provider retains no alias.
		raw, err := json.Marshal(prepared.Value)
		if err != nil {
			return deepseek.FieldValue{}, false, err
		}
		var value map[string]any
		if err := json.Unmarshal(raw, &value); err != nil {
			return deepseek.FieldValue{}, false, err
		}
		return deepseek.FieldValue{Value: value, Accept: prepared.Accept}, true, nil
	}))
}
