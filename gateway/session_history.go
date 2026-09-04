// Shared session history paging primitives (official api-session-controller
// history.ts paginate/pageRecords): the message-aligned backwards cut both
// the follow snapshot and the page endpoint consume, plus the wire record
// translation. Chunk packing stays a later round: unpacked chunk events
// remain valid SessionEventEntry records.
package gateway

import (
	"encoding/json"

	"dshgo/session"
)

// followMessageTypes are the events paginate counts toward maxMessages
// (official MESSAGE_TYPES).
var followMessageTypes = map[string]bool{
	"user/message":      true,
	"assistant/message": true,
}

// followDefaultMaxMessages is the official DEFAULT_MAX_MESSAGES.
const followDefaultMaxMessages = 50

// isAppendSurfaceEvent reports whether the event appended to the surface
// tail (official isAppendSurfaceEvent): replacement copies stay model-only
// and never count as transcript messages.
func isAppendSurfaceEvent(event session.Event) bool {
	return event.SurfaceOp != nil && event.SurfaceOp.Kind == session.SurfaceAppend
}

// paginateHistory is the message-aligned backwards cut (official paginate):
// walk from the newest event counting message-typed append-surface events
// up to the bound, then slice from the cut. throughSeq is the inclusive
// newest seq considered (the page's anchor); beforeSeq optionally bounds
// the read window further back. An empty event list pages as an empty
// record set with no more.
func paginateHistory(events []session.Event, beforeSeq *int64, maxMessages int, throughSeq int64) (page []session.Event, hasMore bool) {
	if len(events) == 0 {
		return events, false
	}
	end := throughSeq + 1
	if beforeSeq != nil && *beforeSeq < end {
		end = *beforeSeq
	}
	if end > int64(len(events)) {
		end = int64(len(events))
	}
	if end < 0 {
		end = 0
	}
	count := 0
	cut := int64(0)
	for index := end - 1; index >= 0; index-- {
		event := events[index]
		if !followMessageTypes[event.Type] || !isAppendSurfaceEvent(event) {
			continue
		}
		count++
		groupStart := event.Seq
		if len(event.SourceEventSeqs) > 0 {
			for _, source := range event.SourceEventSeqs {
				if source < groupStart {
					groupStart = source
				}
			}
		}
		if count >= maxMessages {
			cut = groupStart
			break
		}
	}
	return events[cut:end], cut > 0
}

// followEventFrame is the wire form of one raw event (official
// SessionWireEvent): the durable event fields the browser journal renders.
type followEventFrame struct {
	Type            string          `json:"type"`
	Seq             int64           `json:"seq"`
	Time            int64           `json:"time"`
	Data            json.RawMessage `json:"data"`
	Ignorable       bool            `json:"ignorable,omitempty"`
	SourceEventSeqs []int64         `json:"sourceEventSeqs,omitempty"`
}

// followRecord is one history-page record (official SessionHistoryRecord =
// SessionEventEntry | SessionChunkRun).
type followRecord struct {
	Type  string           `json:"type"`
	Event followEventFrame `json:"event"`
}

// followRecords translates events into wire records.
func followRecords(events []session.Event) []followRecord {
	records := make([]followRecord, 0, len(events))
	for _, event := range events {
		records = append(records, followRecord{
			Type: "event",
			Event: followEventFrame{
				Type:            event.Type,
				Seq:             event.Seq,
				Time:            event.Time,
				Data:            event.Data,
				Ignorable:       event.Ignorable,
				SourceEventSeqs: event.SourceEventSeqs,
			},
		})
	}
	return records
}
