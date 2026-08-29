package sessionquery

import (
	session "dshgo/session"
)

// BuildSessionEventRecords projects a raw log into lightweight
// surface-aware event records: one record per event in ascending seq
// order.
func BuildSessionEventRecords(sessionID session.SessionID, events []session.Event) ([]SessionEventRecord, error) {
	surfaceBySeq, err := classifySurface(events)
	if err != nil {
		return nil, err
	}
	records := make([]SessionEventRecord, 0, len(events))
	for _, event := range events {
		surface, ok := surfaceBySeq[event.Seq]
		if !ok {
			surface = SurfaceLogOnly
		}
		records = append(records, SessionEventRecord{
			SessionID: sessionID,
			Seq:       event.Seq,
			Type:      event.Type,
			Time:      event.Time,
			Surface:   surface,
		})
	}
	return records, nil
}

// BuildSessionEventSearchDocuments builds first-party semantic documents
// for one complete raw event log: searchable documents in ascending seq
// order; structural events are omitted.
func BuildSessionEventSearchDocuments(sessionID session.SessionID, events []session.Event) ([]SessionEventSearchDocument, error) {
	surfaceBySeq, err := classifySurface(events)
	if err != nil {
		return nil, err
	}
	documents := []SessionEventSearchDocument{}
	for _, event := range events {
		text := ExtractSessionEventText(event)
		if len(text) == 0 {
			continue
		}
		surface, ok := surfaceBySeq[event.Seq]
		if !ok {
			surface = SurfaceLogOnly
		}
		documents = append(documents, SessionEventSearchDocument{
			SessionEventRecord: SessionEventRecord{
				SessionID: sessionID,
				Seq:       event.Seq,
				Type:      event.Type,
				Time:      event.Time,
				Surface:   surface,
			},
			Text: text,
		})
	}
	return documents, nil
}

// classifySurface folds the log once and maps every seq to its surface
// placement; events absent from the map are log-only.
func classifySurface(events []session.Event) (map[int64]string, error) {
	folded, err := session.FoldSurface(events)
	if err != nil {
		return nil, queryErrorCause(CodeInvalidSurface, err, "invalid session surface: %v", err)
	}
	result := make(map[int64]string, len(events))
	for _, seq := range folded.Nodes {
		result[seq] = SurfaceCurrent
	}
	for _, replacement := range folded.Replacements {
		for _, seq := range replacement.ShadowedSeqs {
			result[seq] = SurfaceShadowed
		}
	}
	return result, nil
}
