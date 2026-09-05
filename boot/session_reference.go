// The session-reference exact-read adapter: the composed session-query
// engine observed through the sessionreference SnapshotReader seam (the
// official ctx.sessionQuery adaptation). Exact reads only — the projection
// is bounded by the configured byte budget.
package boot

import (
	"context"
	"encoding/json"

	"dshgo/llm"
	"dshgo/session"
	"dshgo/sessionquery"
	"dshgo/sessionreference"
)

// sessionReferenceReader adapts the session-query engine to the
// sessionreference exact-read seam.
type sessionReferenceReader struct {
	engine *sessionquery.Engine
	ctx    context.Context
}

// ReadSurface observes one session's current surface: user and assistant
// messages project as text-bearing events; other event kinds stay outside
// the seam (matching the Go projection vocabulary).
func (r *sessionReferenceReader) ReadSurface(sessionID string) (sessionreference.SessionSnapshot, error) {
	snapshot, err := r.engine.ReadSession(r.ctx, session.SessionID(sessionID))
	if err != nil {
		return sessionreference.SessionSnapshot{}, err
	}
	out := sessionreference.SessionSnapshot{SessionID: sessionID, Cwd: snapshot.Session.CWD}
	for _, event := range snapshot.Events {
		out.CapturedThroughSeq = event.Seq
		out.HasCapturedThroughSeq = true
		switch event.Type {
		case session.EventUserMessage:
			var message llm.Message
			if json.Unmarshal(event.Data, &message) == nil {
				out.Events = append(out.Events, sessionreference.SurfaceEvent{
					Type: sessionreference.EventUserMessage,
					User: &sessionreference.SurfaceUserMessage{Source: message.Source, Content: message.Content},
				})
			}
		case session.EventAssistantMsg:
			if data, err := session.DecodeAssistantMessage(event); err == nil {
				out.Events = append(out.Events, sessionreference.SurfaceEvent{
					Type:      sessionreference.EventAssistantMessage,
					Assistant: &sessionreference.SurfaceAssistantMessage{Content: data.Message.Content},
				})
			}
		}
	}
	return out, nil
}

// ListSessions lists every session record visible to discovery.
func (r *sessionReferenceReader) ListSessions() ([]sessionreference.SessionRecord, error) {
	records, err := r.engine.ListSessions(r.ctx)
	if err != nil {
		return nil, err
	}
	out := make([]sessionreference.SessionRecord, 0, len(records))
	for _, record := range records {
		out = append(out, sessionreference.SessionRecord{
			ID:        string(record.Header.ID),
			Cwd:       record.Header.CWD,
			CreatedAt: record.Header.CreatedAt,
		})
	}
	return out, nil
}
