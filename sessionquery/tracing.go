package sessionquery

import (
	"sort"

	session "dshgo/session"
)

// eventLogAnalysis is the canonical single-fold classification of one raw
// event log.
type eventLogAnalysis struct {
	records           []SessionEventRecord
	replacedBy        map[int64]int64
	replacedEventSeqs map[int64][]int64
	currentSeqs       []int64
}

// EventRecords classifies a raw event log with one canonical surface fold:
// lightweight records in ascending log order.
func EventRecords(sessionID session.SessionID, events []session.Event) ([]SessionEventRecord, error) {
	analysis, err := analyzeEventLog(sessionID, events)
	if err != nil {
		return nil, err
	}
	return analysis.records, nil
}

// CurrentSurfaceEvents folds and returns the current model surface after
// validating the whole log: detached current surface events in folded
// order.
func CurrentSurfaceEvents(sessionID session.SessionID, events []session.Event) ([]session.Event, error) {
	analysis, err := analyzeEventLog(sessionID, events)
	if err != nil {
		return nil, err
	}
	surface := make([]session.Event, 0, len(analysis.currentSeqs))
	for _, seq := range analysis.currentSeqs {
		if seq < 0 || seq >= int64(len(events)) || events[seq].Seq != seq || !session.IsSurfaceEventType(events[seq].Type) {
			return nil, queryError(CodeInvalidSurface, "invalid session surface: current node %d is not a surface event", seq)
		}
		surface = append(surface, events[seq])
	}
	return surface, nil
}

// TraceEvent traces one target after one canonical surface fold and
// whole-log validation: direct surface replacements and relationships to
// cited source events.
func TraceEvent(sessionID session.SessionID, events []session.Event, seq int64) (SessionEventTrace, error) {
	if seq < 0 || seq >= int64(len(events)) || events[seq].Seq != seq {
		return SessionEventTrace{}, queryError(CodeEventNotFound, "session %q has no event at seq %d", sessionID, seq)
	}
	analysis, err := analyzeEventLog(sessionID, events)
	if err != nil {
		return SessionEventTrace{}, err
	}
	target := events[seq]

	replacementChain := []int64{}
	replacement, hasReplacement := analysis.replacedBy[seq]
	for hasReplacement {
		replacementChain = append(replacementChain, replacement)
		replacement, hasReplacement = analysis.replacedBy[replacement]
	}

	derivedEventSeqs := []int64{}
	for _, event := range events {
		if event.Seq <= seq {
			continue
		}
		if containsSeq(event.SourceEventSeqs, seq) {
			derivedEventSeqs = append(derivedEventSeqs, event.Seq)
		}
	}

	trace := SessionEventTrace{
		Target:            analysis.records[seq],
		ReplacementChain:  replacementChain,
		ReplacedEventSeqs: []int64{},
		SourceEventSeqs:   []int64{},
		DerivedEventSeqs:  derivedEventSeqs,
	}
	if len(target.SourceEventSeqs) > 0 {
		trace.SourceEventSeqs = append([]int64(nil), target.SourceEventSeqs...)
	}
	if replaced, ok := analysis.replacedEventSeqs[seq]; ok {
		trace.ReplacedEventSeqs = append([]int64(nil), replaced...)
	}
	if replacedBy, ok := analysis.replacedBy[seq]; ok {
		value := replacedBy
		trace.ReplacedBy = &value
	}
	return trace, nil
}

// TraceSession traces one target's known ancestry and recursively known
// descendants over the complete logical corpus from one observation.
func TraceSession(records []SessionRecord, sessionID session.SessionID) (SessionLineageTrace, error) {
	byID := make(map[session.SessionID]SessionRecord, len(records))
	for _, record := range records {
		byID[record.Header.ID] = record
	}
	target, ok := byID[sessionID]
	if !ok {
		return SessionLineageTrace{}, queryError(CodeSessionNotFound, "session %q not found", sessionID)
	}

	ancestors := []SessionRecord{}
	ancestrySeen := map[session.SessionID]bool{sessionID: true}
	unresolvedParentID := ""
	parentID := parentOf(target)
	for parentID != "" {
		if ancestrySeen[parentID] {
			return SessionLineageTrace{}, queryError(CodeInvalidLineage, "session lineage contains a cycle at %q", parentID)
		}
		ancestrySeen[parentID] = true
		parent, ok := byID[parentID]
		if !ok {
			unresolvedParentID = parentID
			break
		}
		ancestors = append(ancestors, parent)
		parentID = parentOf(parent)
	}

	childrenByParent := map[session.SessionID][]SessionRecord{}
	for _, record := range records {
		parent := parentOf(record)
		if parent == "" {
			continue
		}
		childrenByParent[parent] = append(childrenByParent[parent], record)
	}
	for _, children := range childrenByParent {
		sort.Slice(children, func(i, j int) bool {
			if children[i].Header.CreatedAt != children[j].Header.CreatedAt {
				return children[i].Header.CreatedAt < children[j].Header.CreatedAt
			}
			// Official ordering is localeCompare; the Go corpus compares
			// byte-wise (recorded divergence).
			return children[i].Header.ID < children[j].Header.ID
		})
	}

	descendants := buildDescendants(childrenByParent, sessionID)
	common := SessionLineageTrace{
		Target:      cloneRecord(target),
		Ancestors:   make([]SessionRecord, 0, len(ancestors)),
		Descendants: descendants,
	}
	for _, ancestor := range ancestors {
		common.Ancestors = append(common.Ancestors, cloneRecord(ancestor))
	}
	if unresolvedParentID != "" {
		common.Complete = false
		common.UnresolvedParentID = unresolvedParentID
		return common, nil
	}
	root := cloneRecord(target)
	if len(ancestors) > 0 {
		root = cloneRecord(ancestors[len(ancestors)-1])
	}
	common.Complete = true
	common.Root = &root
	return common, nil
}

func analyzeEventLog(sessionID session.SessionID, events []session.Event) (eventLogAnalysis, error) {
	folded, err := session.FoldSurface(events)
	if err != nil {
		return eventLogAnalysis{}, queryErrorCause(CodeInvalidSurface, err, "invalid session surface: %v", err)
	}
	current := make(map[int64]bool, len(folded.Nodes))
	for _, seq := range folded.Nodes {
		current[seq] = true
	}
	replacedBy := make(map[int64]int64)
	replacedEventSeqs := make(map[int64][]int64)
	for _, replacement := range folded.Replacements {
		removed := append([]int64(nil), replacement.ShadowedSeqs...)
		replacedEventSeqs[replacement.Seq] = removed
		for _, removedSeq := range removed {
			replacedBy[removedSeq] = replacement.Seq
		}
	}
	records := make([]SessionEventRecord, 0, len(events))
	for _, event := range events {
		surface := SurfaceLogOnly
		switch {
		case current[event.Seq]:
			surface = SurfaceCurrent
		default:
			if _, shadowed := replacedBy[event.Seq]; shadowed {
				surface = SurfaceShadowed
			}
		}
		records = append(records, SessionEventRecord{
			SessionID: sessionID,
			Seq:       event.Seq,
			Type:      event.Type,
			Time:      event.Time,
			Surface:   surface,
		})
	}
	currentSeqs := append([]int64(nil), folded.Nodes...)
	return eventLogAnalysis{
		records:           records,
		replacedBy:        replacedBy,
		replacedEventSeqs: replacedEventSeqs,
		currentSeqs:       currentSeqs,
	}, nil
}

// buildDescendants walks the descendant trees iteratively: children are
// attached to their parent frame in createdAt order, then pushed in
// reverse so the stack expands them in order.
func buildDescendants(childrenByParent map[session.SessionID][]SessionRecord, sessionID session.SessionID) []SessionLineageNode {
	descendants := []SessionLineageNode{}
	type frame struct {
		sessionID   session.SessionID
		descendants *[]SessionLineageNode
	}
	stack := []frame{{sessionID: sessionID, descendants: &descendants}}
	for len(stack) > 0 {
		current := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		children := childrenByParent[current.sessionID]
		for _, child := range children {
			*current.descendants = append(*current.descendants, SessionLineageNode{Session: cloneRecord(child), Descendants: []SessionLineageNode{}})
		}
		// Pointers are taken only after the last append: this slice is owned
		// by exactly this frame, so it cannot grow again underneath them.
		owned := *current.descendants
		for index := len(owned) - len(children); index < len(owned); index++ {
			node := &owned[index]
			stack = append(stack, frame{
				sessionID:   node.Session.Header.ID,
				descendants: &node.Descendants,
			})
		}
	}
	return descendants
}

func parentOf(record SessionRecord) session.SessionID {
	if record.Header.ParentSession == "" {
		return ""
	}
	return record.Header.ParentSession
}

func containsSeq(values []int64, want int64) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func cloneRecord(record SessionRecord) SessionRecord {
	return record
}
