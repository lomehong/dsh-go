package compactionbasic

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"

	"dshgo/compaction"
	"dshgo/llm"
	"dshgo/session"
	"dshgo/tokenmeter"
)

// Stability rules for the surface relationship that must survive
// summarization.
const (
	// StabilityWholeSurface requires the summary's replacement boundaries to
	// still be exactly the ones it was built from.
	StabilityWholeSurface = "whole-surface"
	// StabilitySelectedSpan requires only that the selected span remain the
	// same present, contiguous, equally priced, balanced replacement target.
	StabilitySelectedSpan = "selected-span"
)

// Transaction stages carried by a recorded failure.
const (
	stageSummary = "summary"
	stageCommit  = "commit"
)

// surfaceChangedError rejects a summary whose replacement boundaries are no
// longer the ones it was built from, distinguished from summarizer and
// shrink failures so a manual caller can report the two causes differently.
type surfaceChangedError struct{ msg string }

func (e *surfaceChangedError) Error() string { return e.msg }

// ManualCompactionKind classifies one manual compaction failure.
type ManualCompactionKind string

// Manual compaction failure kinds.
const (
	ManualBusy        ManualCompactionKind = "busy"
	ManualCommit      ManualCompactionKind = "commit"
	ManualChanged     ManualCompactionKind = "changed"
	ManualSummary     ManualCompactionKind = "summary"
	ManualPersistence ManualCompactionKind = "persistence"
	ManualCancelled   ManualCompactionKind = "cancelled"
)

// ManualCompactionError classifies one failed manual compaction request.
type ManualCompactionError struct {
	// Kind is the machine-routable failure classification.
	Kind ManualCompactionKind
	// Message is the user-facing diagnostic.
	Message string
	// Cause is the underlying failure, when one was captured.
	Cause error
}

func (e *ManualCompactionError) Error() string { return e.Message }
func (e *ManualCompactionError) Unwrap() error { return e.Cause }

// SurfaceSelection is one validated inclusive span of current surface
// positions.
type SurfaceSelection struct {
	Start        int64
	End          int64
	StartIdx     int64
	EndIdx       int64
	ShadowedSeqs []int64
}

// preparedCompaction is a selection with its priced snapshot and the replay
// input built from it.
type preparedCompaction struct {
	SurfaceSelection
	Measurement             tokenmeter.Measurement
	SelectedNodes           []tokenmeter.TokenSurfaceNode
	ShadowedTokenCount      int64
	ShadowedRouteTokenCount int64
	Input                   SummarizationInput
}

// summarizedCompaction is a prepared compaction with its summary, checkpoint
// message, and call facts.
type summarizedCompaction struct {
	preparedCompaction
	SummaryResult
	CheckpointMessage llm.Message
}

// TransactionOptions bracket one compaction transaction: OwnerCurrentTurn
// derives a numbered owner, a standalone marker pair runs between turns.
type TransactionOptions struct {
	// OwnerCurrentTurn selects the numbered-owner bracket; false writes a
	// standalone bracket.
	OwnerCurrentTurn bool
	// Stability is the surface relationship that must survive asynchronous
	// summarization (StabilityWholeSurface or StabilitySelectedSpan).
	Stability string
	// Flush is the optional durability checkpoint after a successfully
	// closed bracket.
	Flush func() error
	// SourceCommandID is the manual command that initiated this transaction,
	// when present.
	SourceCommandID compaction.CommandID
}

// RegionDependencies bind the effective token meter, the owned tool-pairing
// balance, and the dynamically dispatched summarizer hook.
type RegionDependencies struct {
	// Meter is the singleton replay-aware token meter.
	Meter *tokenmeter.Meter
	// Balance is the tool-pairing balance owned by the component driving
	// the session.
	Balance *compaction.ToolPairingBalance
	// Summarize produces the safe summary for one replayed conversation
	// prefix.
	Summarize func(input SummarizationInput, agent AgentView, signal context.Context) (SummaryResult, error)
}

// transactionFailure captures a failure after compaction/start committed.
type transactionFailure struct {
	err   error
	stage string
}

// signalErr reads a cancellation off the summarization signal; the nil
// context is live.
func signalErr(signal context.Context) error {
	if signal == nil {
		return nil
	}
	return signal.Err()
}

// newUUID generates one random RFC-4122 v4 identity (randomUUID in the
// source).
func newUUID() string {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		panic(fmt.Sprintf("compaction-basic: cannot generate a compaction id: %v", err))
	}
	bytes[6] = (bytes[6] & 0x0f) | 0x40
	bytes[8] = (bytes[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", bytes[0:4], bytes[4:6], bytes[6:8], bytes[8:10], bytes[10:16])
}

// SelectCompactableRange resolves the next head-anchored range while
// retaining a priced recent tail and never splitting an assistant
// tool-call/result pair. ok is false when no range qualifies.
func SelectCompactableRange(deps RegionDependencies, sess *session.Session, measurement tokenmeter.Measurement, retainTokens int64) (start int64, end int64, ok bool, err error) {
	pricedNodes := measurement.Nodes
	if len(pricedNodes) == 0 {
		return 0, 0, false, nil
	}
	surfaceNodes := sess.Surface().Nodes()
	if int64(len(surfaceNodes)) != int64(len(pricedNodes)) {
		return 0, 0, false, fmt.Errorf("compaction: token-meter surface does not match the current session surface")
	}
	for index, seq := range surfaceNodes {
		if seq != pricedNodes[index].Seq {
			return 0, 0, false, fmt.Errorf("compaction: token-meter surface does not match the current session surface")
		}
	}

	accumulated := int64(0)
	keepFromIdx := int64(len(pricedNodes))
	for index := len(pricedNodes) - 1; index >= 0; index-- {
		accumulated += pricedNodes[index].Tokens
		keepFromIdx = int64(index)
		if accumulated >= retainTokens {
			break
		}
	}
	if keepFromIdx == 0 {
		return 0, 0, false, nil
	}
	for keepFromIdx > 0 {
		balanced, err := deps.Balance.ToolPairingBalancedBefore(sess, surfaceNodes[keepFromIdx])
		if err != nil {
			return 0, 0, false, err
		}
		if balanced {
			break
		}
		keepFromIdx--
	}
	if keepFromIdx == 0 {
		return 0, 0, false, nil
	}
	first := surfaceNodes[0]
	cutoff := surfaceNodes[keepFromIdx-1]
	return first, cutoff, true, nil
}

// ValidateSurfaceRegion validates one requested surface-position span before
// asynchronous work begins. Exported for the selected-span stability check.
func ValidateSurfaceRegion(deps RegionDependencies, sess *session.Session, start int64, end int64) (SurfaceSelection, error) {
	nodes := sess.Surface().Nodes()
	startIdx := indexOfSeq(nodes, start)
	endIdx := indexOfSeq(nodes, end)
	if startIdx == -1 {
		return SurfaceSelection{}, fmt.Errorf("compactRegion: start seq %d not found in surface", start)
	}
	if endIdx == -1 {
		return SurfaceSelection{}, fmt.Errorf("compactRegion: end seq %d not found in surface", end)
	}
	if startIdx > endIdx {
		return SurfaceSelection{}, fmt.Errorf(
			"compactRegion: start seq %d (position %d) is after end seq %d (position %d) on the surface",
			start, startIdx, end, endIdx)
	}
	balanced, err := deps.Balance.ToolPairingBalancedBefore(sess, nodes[startIdx])
	if err != nil {
		return SurfaceSelection{}, err
	}
	if !balanced {
		return SurfaceSelection{}, fmt.Errorf(
			"compactRegion: start seq %d is not a balanced boundary (would split a step's tool-call/result pair)", start)
	}
	balanced, err = deps.Balance.ToolPairingBalancedAfter(sess, nodes[endIdx])
	if err != nil {
		return SurfaceSelection{}, err
	}
	if !balanced {
		return SurfaceSelection{}, fmt.Errorf(
			"compactRegion: end seq %d is not a balanced boundary (would split a step, or the step is still open)", end)
	}
	shadowed := append([]int64{}, nodes[startIdx:endIdx+1]...)
	return SurfaceSelection{
		Start:        start,
		End:          end,
		StartIdx:     int64(startIdx),
		EndIdx:       int64(endIdx),
		ShadowedSeqs: shadowed,
	}, nil
}

// indexOfSeq finds the first surface position carrying the seq, or -1.
func indexOfSeq(nodes []int64, seq int64) int {
	for index, candidate := range nodes {
		if candidate == seq {
			return index
		}
	}
	return -1
}

// entryState inspects open-turn, unmatched-compaction, and latest
// seed-boundary state independently.
type entryState struct {
	openTurn                 *int64
	unmatchedCompactionStart *session.Event
	latestEndSeedSeq         *int64
}

// inspectCompactionEntryState scans the log tail once for the three
// independent facts.
func inspectCompactionEntryState(events []session.Event) entryState {
	state := entryState{}
	openTurnStateKnown := false
	compactionEntryStateKnown := false
	for index := len(events) - 1; index >= 0; index-- {
		event := events[index]
		if state.latestEndSeedSeq == nil && event.Type == session.EventEndSeed {
			seq := event.Seq
			state.latestEndSeedSeq = &seq
		}
		if !compactionEntryStateKnown {
			if event.Type == compaction.EventCompactionStart {
				start := event
				state.unmatchedCompactionStart = &start
				compactionEntryStateKnown = true
			} else if event.Type == compaction.EventCompactionEnd {
				compactionEntryStateKnown = true
			}
		}
		if !openTurnStateKnown {
			switch event.Type {
			case session.EventTurnStart:
				var data session.TurnStartData
				if err := json.Unmarshal(event.Data, &data); err == nil {
					turn := data.Turn
					state.openTurn = &turn
				}
				openTurnStateKnown = true
			case session.EventTurnEnd:
				openTurnStateKnown = true
			}
		}
		if openTurnStateKnown && compactionEntryStateKnown && state.latestEndSeedSeq != nil {
			break
		}
	}
	return state
}

// assertCompactionInactive rejects a durable unmatched compaction marker
// unless a later constructor-seed boundary proves that its owner belongs to
// an earlier session lifecycle.
func assertCompactionInactive(state entryState, stage string) error {
	if state.unmatchedCompactionStart == nil ||
		(state.latestEndSeedSeq != nil && *state.latestEndSeedSeq > state.unmatchedCompactionStart.Seq) {
		return nil
	}
	return &ManualCompactionError{
		Kind:    ManualBusy,
		Message: fmt.Sprintf("%s: compaction already in progress; the session compaction lock is already active", stage),
	}
}

// AssertNoActiveCompaction rechecks the durable compaction lock after an
// asynchronous policy decision.
func AssertNoActiveCompaction(sess *session.Session, stage string) error {
	return assertCompactionInactive(inspectCompactionEntryState(sess.Events()), stage)
}

// CompactSurfaceRegion runs the single compaction transaction over one
// selected positional span. Selection and validation are read-only. Idle/log
// validation and compaction/start are synchronously adjacent, so the durable
// opening marker is the compaction lock before summarization runs. Every
// later failure makes exactly one compaction/end attempt; a failed close
// deliberately leaves the unmatched start detectable.
func CompactSurfaceRegion(
	deps RegionDependencies,
	sess *session.Session,
	start int64,
	end int64,
	agent AgentView,
	options TransactionOptions,
	signal context.Context,
) (compaction.Result, error) {
	result := compaction.Result{}
	if !options.OwnerCurrentTurn {
		if err := signalErr(signal); err != nil {
			return result, err
		}
	}
	selection, err := ValidateSurfaceRegion(deps, sess, start, end)
	if err != nil {
		return result, err
	}
	state := inspectCompactionEntryState(sess.Events())
	if err := assertCompactionInactive(state, "compaction"); err != nil {
		return result, err
	}

	var owner *int64
	if !options.OwnerCurrentTurn {
		if state.openTurn != nil {
			return result, &ManualCompactionError{
				Kind:    ManualBusy,
				Message: "manual compaction: the session already has an open turn",
			}
		}
	} else {
		if state.openTurn == nil {
			return result, fmt.Errorf("compactRegion: no open turn — automatic compaction events must be enclosed in a turn")
		}
		owner = state.openTurn
	}

	compactionID := compaction.CompactionID(newUUID())
	startPayload := compaction.StartPayload{
		CompactionID:    compactionID,
		SourceCommandID: options.SourceCommandID,
		Turn:            owner,
	}
	startEvent, err := sess.Append(compaction.EventCompactionStart, startPayload, nil)
	if err != nil {
		return result, err
	}
	var failure *transactionFailure
	var flushFailure error
	var pending *compaction.Result
	closed := false
	closing := false
	stage := stageSummary

	func() {
		defer func() {
			if rec := recover(); rec != nil {
				failure = &transactionFailure{err: fmt.Errorf("%v", rec), stage: stage}
			}
		}()
		prepared, err := prepareCompaction(deps, sess, selection)
		if err != nil {
			failure = &transactionFailure{err: err, stage: stage}
			return
		}
		summarized, err := summarizeCompaction(deps, prepared, agent, compactionID, options.SourceCommandID, signal)
		if err != nil {
			failure = &transactionFailure{err: err, stage: stage}
			return
		}
		if !options.OwnerCurrentTurn {
			if err := signalErr(signal); err != nil {
				failure = &transactionFailure{err: err, stage: stage}
				return
			}
		}
		if err := assertStable(deps, options.Stability, sess, summarized); err != nil {
			failure = &transactionFailure{err: err, stage: stage}
			return
		}
		stage = stageCommit
		body, err := commitCompactionBody(sess, startEvent, summarized)
		if err != nil {
			failure = &transactionFailure{err: err, stage: stage}
			return
		}
		closing = true
		endEvent, err := sess.Append(compaction.EventCompactionEnd, compaction.EndPayload{
			CompactionID:    compactionID,
			SourceCommandID: options.SourceCommandID,
			Turn:            owner,
		}, nil)
		if err != nil {
			failure = &transactionFailure{err: err, stage: stageCommit}
			return
		}
		closed = true
		complete := body
		complete.EndSeq = endEvent.Seq
		pending = &complete
	}()

	if !closed && !closing {
		closing = true
		endErr := llm.ErrorChain(errCloseFailure(failure))
		_, appendErr := sess.Append(compaction.EventCompactionEnd, compaction.EndPayload{
			CompactionID:    compactionID,
			SourceCommandID: options.SourceCommandID,
			Turn:            owner,
			Error:           endErr,
		}, nil)
		if appendErr != nil {
			failure = &transactionFailure{err: appendErr, stage: stageCommit}
		} else {
			closed = true
		}
	}

	if closed && options.Flush != nil {
		if err := options.Flush(); err != nil {
			flushFailure = err
		}
	}

	if !options.OwnerCurrentTurn {
		if err := signalErr(signal); err != nil {
			return result, err
		}
	}
	if failure != nil {
		if !options.OwnerCurrentTurn {
			return result, throwManualFailure(failure)
		}
		return result, failure.err
	}
	if flushFailure != nil {
		return result, &ManualCompactionError{
			Kind:    ManualPersistence,
			Message: "manual compaction durability checkpoint failed",
			Cause:   flushFailure,
		}
	}
	if pending == nil {
		return result, fmt.Errorf("compaction committed without a result")
	}
	return *pending, nil
}

// errCloseFailure renders the transaction failure for the close event's
// error field; a failure recorded during the close attempt itself has no
// summary-stage error to report.
func errCloseFailure(failure *transactionFailure) error {
	if failure == nil {
		return fmt.Errorf("compaction failed")
	}
	return failure.err
}

// throwManualFailure classifies one closed manual attempt without weakening
// cancellation precedence.
func throwManualFailure(failure *transactionFailure) error {
	if failure.stage == stageCommit {
		return &ManualCompactionError{
			Kind:    ManualCommit,
			Message: "manual compaction did not commit cleanly",
			Cause:   failure.err,
		}
	}
	var changed *surfaceChangedError
	if errors.As(failure.err, &changed) {
		return &ManualCompactionError{
			Kind:    ManualChanged,
			Message: "the compacted history changed during manual compaction",
			Cause:   failure.err,
		}
	}
	return &ManualCompactionError{
		Kind:    ManualSummary,
		Message: "manual compaction could not produce a smaller summary",
		Cause:   failure.err,
	}
}

// prepareCompaction snapshots pricing and replay input for a validated
// surface range.
func prepareCompaction(deps RegionDependencies, sess *session.Session, selection SurfaceSelection) (preparedCompaction, error) {
	prepared := preparedCompaction{}
	measurement, err := deps.Meter.Measure(sess, nil)
	if err != nil {
		return prepared, err
	}
	if int(selection.EndIdx) >= len(measurement.Nodes) || int(selection.StartIdx) > int(selection.EndIdx) {
		return prepared, &surfaceChangedError{msg: "compaction: selected surface changed before summarization began"}
	}
	selectedNodes := append([]tokenmeter.TokenSurfaceNode{}, measurement.Nodes[selection.StartIdx:selection.EndIdx+1]...)
	if len(selectedNodes) != len(selection.ShadowedSeqs) {
		return prepared, &surfaceChangedError{msg: "compaction: selected surface changed before summarization began"}
	}
	for index, node := range selectedNodes {
		if node.Seq != selection.ShadowedSeqs[index] {
			return prepared, &surfaceChangedError{msg: "compaction: selected surface changed before summarization began"}
		}
	}
	shadowedTokenCount := int64(0)
	shadowedRouteTokenCount := int64(0)
	for _, node := range selectedNodes {
		shadowedTokenCount += node.HeuristicTokens
		shadowedRouteTokenCount += node.Tokens
	}
	return preparedCompaction{
		SurfaceSelection:        selection,
		Measurement:             measurement,
		SelectedNodes:           selectedNodes,
		ShadowedTokenCount:      shadowedTokenCount,
		ShadowedRouteTokenCount: shadowedRouteTokenCount,
		Input:                   buildSummarizationInput(sess, selection.ShadowedSeqs),
	}, nil
}

// summarizeCompaction runs the summarizer and frames its replacement
// checkpoint.
func summarizeCompaction(
	deps RegionDependencies,
	prepared preparedCompaction,
	agent AgentView,
	compactionID compaction.CompactionID,
	sourceCommandID compaction.CommandID,
	signal context.Context,
) (summarizedCompaction, error) {
	summarized := summarizedCompaction{}
	summaryResult, err := deps.Summarize(prepared.Input, agent, signal)
	if err != nil {
		return summarized, err
	}
	checkpointMessage := llm.NewUserMessage(
		FrameSummary(summaryResult.Summary),
		messageSourceFor(compaction.CompactCheckpointSource(compactionID, sourceCommandID)),
	)
	// The checkpoint is text-only, so its fixed-heuristic price IS its route
	// price; comparing it against the span's route price asks the real
	// question — does the replacement lower the next request's pressure.
	framedSummaryTokenCount := tokenmeter.EstimateMessage(checkpointMessage)
	if framedSummaryTokenCount >= prepared.ShadowedRouteTokenCount {
		return summarized, fmt.Errorf(
			"summary is not smaller than the shadowed content (%d estimated framed tokens >= %d)",
			framedSummaryTokenCount, prepared.ShadowedRouteTokenCount)
	}
	summarized.preparedCompaction = prepared
	summarized.SummaryResult = summaryResult
	summarized.CheckpointMessage = checkpointMessage
	return summarized, nil
}

// messageSourceFor projects the compaction checkpoint source onto a message
// source.
func messageSourceFor(source compaction.CompactionCheckpointSource) llm.MessageSource {
	encoded, err := json.Marshal(source)
	if err != nil {
		return llm.MessageSource{}
	}
	return llm.MessageSource{Kind: "plugin", Plugin: "compact", ReplayState: encoded}
}

// assertStable dispatches the configured stability rule.
func assertStable(deps RegionDependencies, stability string, sess *session.Session, summarized summarizedCompaction) error {
	if stability == StabilitySelectedSpan {
		return assertSelectedSpanStable(deps, sess, summarized.preparedCompaction)
	}
	return assertWholeSurfaceUnchanged(deps, sess, summarized.preparedCompaction)
}

// assertWholeSurfaceUnchanged rejects a summary prepared against any earlier
// surface generation.
func assertWholeSurfaceUnchanged(deps RegionDependencies, sess *session.Session, prepared preparedCompaction) error {
	current, err := deps.Meter.Measure(sess, nil)
	if err != nil {
		return err
	}
	if !nodesEqual(current.Nodes, prepared.Measurement.Nodes) {
		return &surfaceChangedError{msg: "compaction: session surface changed during summarization"}
	}
	return nil
}

// assertSelectedSpanStable requires only that the selected span remain the
// same present, contiguous, equally priced, balanced replacement target.
// Nodes added outside it remain visible and do not invalidate the summary.
func assertSelectedSpanStable(deps RegionDependencies, sess *session.Session, prepared preparedCompaction) error {
	current, err := ValidateSurfaceRegion(deps, sess, prepared.Start, prepared.End)
	if err != nil {
		return &surfaceChangedError{msg: "compaction: the selected span is no longer a valid replacement target"}
	}
	if !seqsEqual(current.ShadowedSeqs, prepared.ShadowedSeqs) {
		return &surfaceChangedError{msg: "compaction: the selected span changed during summarization"}
	}
	measured, err := deps.Meter.Measure(sess, nil)
	if err != nil {
		return err
	}
	if int64(len(measured.Nodes)) <= current.EndIdx {
		return &surfaceChangedError{msg: "compaction: the selected span was rewritten during summarization"}
	}
	measuredSlice := measured.Nodes[current.StartIdx : current.EndIdx+1]
	if !nodesEqual(measuredSlice, prepared.SelectedNodes) {
		return &surfaceChangedError{msg: "compaction: the selected span was rewritten during summarization"}
	}
	return nil
}

// nodesEqual is the deep node-list equality the source reaches through
// isDeepStrictEqual.
func nodesEqual(left []tokenmeter.TokenSurfaceNode, right []tokenmeter.TokenSurfaceNode) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

// seqsEqual is the deep seq-list equality.
func seqsEqual(left []int64, right []int64) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

// commitCompactionBody appends one completed summary record and replacement
// body without yielding.
func commitCompactionBody(sess *session.Session, startEvent session.Event, summarized summarizedCompaction) (compaction.Result, error) {
	result := compaction.Result{}
	var startPayload compaction.StartPayload
	if err := json.Unmarshal(startEvent.Data, &startPayload); err != nil {
		return result, err
	}
	summaryPayload := compaction.SummaryPayload{
		CompactionID:       startPayload.CompactionID,
		SourceCommandID:    startPayload.SourceCommandID,
		Summary:            summarized.SummaryResult.Summary,
		ShadowedRange:      compaction.SeqRange{Start: summarized.Start, End: summarized.End},
		ShadowedSeqs:       append([]int64{}, summarized.ShadowedSeqs...),
		ShadowedTokenCount: summarized.ShadowedTokenCount,
		Provider:           summarized.SummaryResult.Provider,
		Model:              summarized.SummaryResult.Model,
		MaxTokens:          summarized.SummaryResult.MaxTokens,
		Usage:              summarized.SummaryResult.Usage,
	}
	if summarized.SummaryResult.LlmStreamCall {
		summaryPayload.RawOutput = summarized.SummaryResult.RawOutput
		summaryPayload.LLMStreamCall = true
	} else if summarized.SummaryResult.RawOutput != nil {
		summaryPayload.RawOutput = summarized.SummaryResult.RawOutput
	}
	summaryEvent, err := sess.Append(compaction.EventCompactionSummary, summaryPayload, nil)
	if err != nil {
		return result, err
	}
	sourceSeqs := append([]int64{startEvent.Seq, summaryEvent.Seq}, summarized.ShadowedSeqs...)
	_, err = sess.Append(session.EventUserMessage, summarized.CheckpointMessage, &session.SurfaceIntent{
		SurfaceOp: session.SurfaceOp{
			Kind:  session.SurfaceReplace,
			Start: summarized.Start,
			End:   summarized.End,
		},
		SourceEventSeqs:   sourceSeqs,
		SourceSeqsPresent: true,
	})
	if err != nil {
		return result, err
	}
	return compaction.Result{
		CompactionID:       startPayload.CompactionID,
		SourceCommandID:    startPayload.SourceCommandID,
		StartSeq:           startEvent.Seq,
		SummarySeq:         summaryEvent.Seq,
		Summary:            summarized.SummaryResult.Summary,
		ShadowedRange:      compaction.SeqRange{Start: summarized.Start, End: summarized.End},
		ShadowedSeqs:       append([]int64{}, summarized.ShadowedSeqs...),
		ShadowedTokenCount: summarized.ShadowedTokenCount,
	}, nil
}

// buildSummarizationInput reconstructs the last routed request's cacheable
// prefix for the shadowed region: its system prompt and tool schemas, then
// the region's own derived messages in surface order.
func buildSummarizationInput(sess *session.Session, shadowedSeqs []int64) SummarizationInput {
	input := SummarizationInput{}
	header := sess.RequestHeader()
	if header != nil {
		input.System = header.System
		input.Tools = header.Tools
	}
	events := sess.Events()
	messages := make([]llm.Message, 0, len(shadowedSeqs))
	for _, seq := range shadowedSeqs {
		// Shadowed seqs are current surface seqs, so each is a valid log
		// index.
		if int(seq) >= len(events) {
			continue
		}
		if message := session.DeriveEventMessage(events[seq]); message != nil {
			messages = append(messages, *message)
		}
	}
	input.Messages = messages
	return input
}
