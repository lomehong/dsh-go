package sessionformatv01

import (
	"dshgo/sessionformat"
)

// Cross-event relationships required to construct one current Session
// safely. Port of packages/session/session-format-v0-to-v1/src/relationships.ts.

// RelationshipExtensions carries relationship roles added by a later format
// while reusing the released validator.
type RelationshipExtensions struct {
	// StepEvents must occur inside the current open step.
	StepEvents map[string]bool
	// PreservedSourceTitleRequestText: the title-request model input was
	// source-validated and preserved across sequence remapping, so target
	// validation skips the framed-text comparison.
	PreservedSourceTitleRequestText bool
}

type compactionState struct {
	id              string
	sourceCommandId string
	hasSourceCmd    bool
	turn            any // int64 or nil
	hasTurn         bool
	startSeq        int64
	summarized      bool
}

type ptcStart struct {
	root      string
	parent    string
	name      string
	arguments any
	settled   bool
}

type toolLifecycle struct {
	name      string
	arguments string
	state     string // advertised | started
}

// AssertReleasedArtifactRelationships validates the cross-event invariants
// of one complete normalized v0 or exact current v1 artifact.
func AssertReleasedArtifactRelationships(artifact sessionformat.Artifact, extensions RelationshipExtensions) error {
	var openTurn any // int64 when open
	var openStep any
	var openStepProvider string
	var nextTurn int64 = 1
	var nextStep int64 = 1
	var surface []int64
	var openCompaction *compactionState
	staleCompactionStarts := inheritedOrphanCompactionStarts(artifact.Events)
	retries := []sessionformat.Event{}
	retryStarts := map[string]bool{}
	ptcRoots := map[string]string{}
	ptcStarts := map[string]*ptcStart{}
	toolLifecycles := map[string]*toolLifecycle{}
	commandRuns := map[string]bool{}

	for _, event := range artifact.Events {
		extensionStepEvent := extensions.StepEvents[event.Type]
		if _, known := dispositionFor(event.Type); !known && !extensionStepEvent {
			continue
		}
		label := event.Type + " " + itoa(event.Seq)
		data, err := decodeData(event, label)
		if err != nil {
			return err
		}
		if surfaceTypes[event.Type] {
			surface, err = applySurface(surface, event)
			if err != nil {
				return err
			}
		}
		if (event.Type == "turn/start" || event.Type == "turn/end") && openCompaction != nil &&
			!staleCompactionStarts[openCompaction.startSeq] {
			return sessionformat.FormatErrorf("%s crosses an open compaction", event.Type)
		}
		if extensionStepEvent {
			if err := requireOpenStep(event, data, openTurn, openStep); err != nil {
				return err
			}
			continue
		}

		switch event.Type {
		case "turn/start":
			turn, err := countValue(data["turn"], label+" turn")
			if err != nil {
				return err
			}
			if openTurn != nil || turn != nextTurn {
				return sessionformat.FormatErrorf("turn/start %v does not open expected turn %d", data["turn"], nextTurn)
			}
			openTurn = turn
			openStep = nil
			toolLifecycles = map[string]*toolLifecycle{}
			nextStep = 1
		case "turn/end":
			turn, err := countValue(data["turn"], label+" turn")
			if err != nil {
				return err
			}
			if openTurn == nil || openTurn != turn {
				return sessionformat.FormatErrorf("turn/end %d has no matching open turn", turn)
			}
			if err := assertNoUnresolvedTools(toolLifecycles, "turn/end"); err != nil {
				return err
			}
			if openStep != nil {
				return sessionformat.FormatErrorf("turn/end %d crosses an open step", turn)
			}
			openTurn = nil
			nextTurn++
		case "step/start":
			step, err := countValue(data["step"], label+" step")
			if err != nil {
				return err
			}
			turn, err := countValue(data["turn"], label+" turn")
			if err != nil {
				return err
			}
			if openTurn == nil || openTurn != turn || openStep != nil || step != nextStep {
				return sessionformat.FormatErrorf("%s does not match the open turn and next step", event.Type)
			}
			openStep = step
		case "step/end":
			if err := requireOpenStep(event, data, openTurn, openStep); err != nil {
				return err
			}
			if err := assertNoUnresolvedTools(toolLifecycles, "step/end"); err != nil {
				return err
			}
			toolLifecycles = map[string]*toolLifecycle{}
			openStep = nil
			nextStep++
		case "assistant/chunk":
			if err := requireOpenStep(event, data, openTurn, openStep); err != nil {
				return err
			}
		case "assistant/message":
			if err := requireOpenStep(event, data, openTurn, openStep); err != nil {
				return err
			}
			message, err := recordOf(data["message"], label+" message")
			if err != nil {
				return err
			}
			content, _ := message["content"].([]any)
			for _, blockValue := range content {
				block, ok := blockValue.(map[string]any)
				if !ok || block["type"] != "tool-call" {
					continue
				}
				callId, _ := block["id"].(string)
				if _, exists := toolLifecycles[callId]; exists {
					return sessionformat.FormatErrorf("assistant/message repeats advertised tool call %s", callId)
				}
				name, _ := block["name"].(string)
				arguments, _ := block["arguments"].(string)
				toolLifecycles[callId] = &toolLifecycle{name: name, arguments: arguments, state: "advertised"}
			}
		case "tool/call":
			if err := requireOpenStep(event, data, openTurn, openStep); err != nil {
				return err
			}
			callId, _ := data["callId"].(string)
			name, _ := data["name"].(string)
			arguments, _ := data["arguments"].(string)
			lifecycle, exists := toolLifecycles[callId]
			if !exists || lifecycle.state != "advertised" || lifecycle.name != name || lifecycle.arguments != arguments {
				return sessionformat.FormatErrorf("tool/call %s does not match one advertised tool call", callId)
			}
			lifecycle.state = "started"
		case "tool/result":
			if surfaceOpIsAppend(event) {
				if err := requireOpenStep(event, data, openTurn, openStep); err != nil {
					return err
				}
				message, err := recordOf(data["message"], label+" message")
				if err != nil {
					return err
				}
				source, err := recordOf(message["source"], label+" source")
				if err != nil {
					return err
				}
				callId, _ := source["callId"].(string)
				content, _ := message["content"].([]any)
				var errorRecord map[string]any
				if value, ok := data["error"]; ok {
					errorRecord, err = recordOf(value, label+" error")
					if err != nil {
						return err
					}
				}
				lifecycle, exists := toolLifecycles[callId]
				if !exists {
					return sessionformat.FormatErrorf("tool/result %s has no advertised tool lifecycle", callId)
				}
				if lifecycle.state == "advertised" && !isExactToolNotStartedRepair(event, content, errorRecord) {
					return sessionformat.FormatErrorf("tool/result %s is not the exact TOOL_NOT_STARTED repair", callId)
				}
				delete(toolLifecycles, callId)
			} else if openTurn == nil {
				return sessionformat.FormatErrorf("tool/result replacement is outside an open turn")
			}
		case "request/header":
			if openTurn == nil {
				return sessionformat.FormatErrorf("%s is outside an open turn", event.Type)
			}
			header, _ := data["header"].(map[string]any)
			config, _ := header["config"].(map[string]any)
			provider, _ := config["provider"].(string)
			openStepProvider = provider
		case "request/context":
			if openTurn == nil {
				return sessionformat.FormatErrorf("%s is outside an open turn", event.Type)
			}
		case "tool/code-dispatch-start", "tool/code-dispatch":
			if openTurn == nil {
				return sessionformat.FormatErrorf("%s is outside an open turn", event.Type)
			}
			root, _ := data["rootCallId"].(string)
			parent, _ := data["parentCallId"].(string)
			child, _ := data["subCallId"].(string)
			if known, ok := ptcRoots[child]; ok && known != root {
				return sessionformat.FormatErrorf("%s changes its rootCallId", event.Type)
			}
			if parent != root && ptcRoots[parent] != root {
				return sessionformat.FormatErrorf("%s parentCallId does not belong to rootCallId", event.Type)
			}
			if event.Type == "tool/code-dispatch-start" {
				if _, exists := ptcStarts[child]; exists {
					return sessionformat.FormatErrorf("tool/code-dispatch-start repeats subCallId")
				}
				name, _ := data["name"].(string)
				ptcStarts[child] = &ptcStart{root: root, parent: parent, name: name, arguments: data["arguments"]}
			} else {
				start, exists := ptcStarts[child]
				if !exists || start.settled {
					return sessionformat.FormatErrorf("tool/code-dispatch has no unique start")
				}
				name, _ := data["name"].(string)
				if start.root != root || start.parent != parent || start.name != name ||
					!deepEqualJSON(start.arguments, data["arguments"]) {
					return sessionformat.FormatErrorf("tool/code-dispatch does not match its start")
				}
				start.settled = true
			}
			ptcRoots[child] = root
		case "llm/retry":
			if err := requireOpenStep(event, data, openTurn, openStep); err != nil {
				return err
			}
			provider, _ := data["provider"].(string)
			if provider != openStepProvider {
				return sessionformat.FormatErrorf("llm/retry provider does not match the open request/header")
			}
			if err := assertRetryChain(retries, data); err != nil {
				return err
			}
			retries = append(retries, event)
		case "llm/retry-started":
			retryId, _ := data["retryId"].(string)
			retry, err := countValue(data["retry"], label+" retry")
			if err != nil {
				return err
			}
			var scheduled map[string]any
			var scheduledEvent sessionformat.Event
			for _, candidate := range retries {
				candidateData, err := decodeData(candidate, candidate.Type+" "+itoa(candidate.Seq))
				if err != nil {
					return err
				}
				candidateId, _ := candidateData["retryId"].(string)
				candidateRetry, _ := countValue(candidateData["retry"], label+" retry")
				if candidateId == retryId && candidateRetry == retry {
					scheduled = candidateData
					scheduledEvent = candidate
					break
				}
			}
			if scheduled == nil {
				return sessionformat.FormatErrorf("llm/retry-started pairs no prior scheduled attempt")
			}
			priorTurn, _ := countValue(scheduled["turn"], "prior turn")
			priorStep, _ := countValue(scheduled["step"], "prior step")
			turn, _ := countValue(data["turn"], label+" turn")
			step, _ := countValue(data["step"], label+" step")
			if priorTurn != turn || priorStep != step {
				return sessionformat.FormatErrorf("llm/retry-started does not match its scheduled turn and step")
			}
			key := quoteJSON(retryId) + "\x00" + itoa(retry)
			if retryStarts[key] {
				return sessionformat.FormatErrorf("llm/retry-started repeats one scheduled attempt")
			}
			retryStarts[key] = true
			_ = scheduledEvent
		case "session/title", "session/title-llm-request":
			if err := assertTitleSourcesImpl(artifact.Events, event, data, !extensions.PreservedSourceTitleRequestText); err != nil {
				return err
			}
		case "command/run":
			id, _ := data["commandId"].(string)
			if commandRuns[id] {
				return sessionformat.FormatErrorf("command/run repeats commandId %s", id)
			}
			commandRuns[id] = true
		case "command/done":
			id, _ := data["commandId"].(string)
			if !commandRuns[id] {
				return sessionformat.FormatErrorf("command/done %s has no prior command/run", id)
			}
			if value, ok := data["sourceEventSeq"]; ok {
				sourceSeq, err := countValue(value, label+" sourceEventSeq")
				if err != nil {
					return err
				}
				kind, _ := data["kind"].(string)
				var sourceType string
				if int(sourceSeq) < len(artifact.Events) {
					sourceType = artifact.Events[sourceSeq].Type
				}
				if kind != "success" || sourceType == "command/run" || sourceType == "command/done" {
					return sessionformat.FormatErrorf("command/done %s has invalid sourceEventSeq", id)
				}
			}
		case "session-log-deepseek/delivery-accepted":
			acceptedVersion := any(jsonNumberZero())
			if value, ok := data["sessionFormatVersion"]; ok {
				acceptedVersion = value
			}
			headerVersion, _ := sessionformat.HeaderVersion(artifact.Header)
			accepted, err := countValue(acceptedVersion, label+" sessionFormatVersion")
			if err != nil {
				return err
			}
			if accepted == headerVersion {
				_, hasParent := artifact.Header["parentSession"]
				inherited := hasParent && event.Seq < artifact.InheritedEventCount
				sessionId, _ := data["sessionId"].(string)
				if !inherited && sessionId != artifact.Header["id"] {
					return sessionformat.FormatErrorf("current-generation delivery marker names the wrong Session")
				}
			}
		case "compaction/start":
			if openCompaction != nil {
				return sessionformat.FormatErrorf("compaction/start overlaps an open compaction")
			}
			if err := assertCompactionTurn(data["turn"], openTurn, "compaction/start", true); err != nil {
				return err
			}
			id, _ := data["compactionId"].(string)
			state := &compactionState{id: id, startSeq: event.Seq, turn: data["turn"], hasTurn: true}
			if value, ok := data["sourceCommandId"]; ok {
				state.sourceCommandId, _ = value.(string)
				state.hasSourceCmd = true
			}
			openCompaction = state
		case "compaction/summary":
			if err := assertCompactionOwner(openCompaction, data, "compaction/summary"); err != nil {
				return err
			}
			if err := assertCompactionTurn(openCompaction.turn, openTurn, "compaction/summary", true); err != nil {
				return err
			}
			if openCompaction.summarized {
				return sessionformat.FormatErrorf("compaction/summary repeats")
			}
			if err := assertCurrentSurfaceSpan(surface, data, "compaction/summary"); err != nil {
				return err
			}
			copied := *openCompaction
			copied.summarized = true
			openCompaction = &copied
		case "compaction/end":
			if err := assertCompactionOwner(openCompaction, data, "compaction/end"); err != nil {
				return err
			}
			endTurn, err := countValue(data["turn"], label+" turn")
			if err != nil {
				return err
			}
			if openTurn == nil || openTurn != endTurn {
				return sessionformat.FormatErrorf("compaction/end changes its owner turn")
			}
			if err := assertCompactionTurn(openCompaction.turn, openTurn, "compaction/end", true); err != nil {
				return err
			}
			if _, hasError := data["error"]; !hasError && !openCompaction.summarized {
				return sessionformat.FormatErrorf("successful compaction/end requires one summary")
			}
			openCompaction = nil
		case "compaction/prune":
			if err := assertCurrentSurfaceSpan(surface, data, "compaction/prune"); err != nil {
				return err
			}
		case "user/message":
			source, err := recordOf(data["source"], label+" source")
			if err != nil {
				return err
			}
			if !surfaceOpIsAppend(event) && source["kind"] == "plugin" && source["plugin"] == "compact" {
				checkpointLabel := "compaction checkpoint at seq " + itoa(event.Seq)
				if err := assertCompactionOwner(openCompaction, source, checkpointLabel); err != nil {
					return err
				}
			}
		case "session/end-seed":
			// An unmatched inherited transaction belongs to the ended source
			// lifecycle.
			openCompaction = nil
		}
	}
	return nil
}

func jsonNumberZero() any {
	return mustDecodeNumber("0")
}

func inheritedOrphanCompactionStarts(events []sessionformat.Event) map[int64]bool {
	stale := map[int64]bool{}
	var open *int64
	for _, event := range events {
		switch event.Type {
		case "compaction/start":
			seq := event.Seq
			open = &seq
		case "compaction/end":
			open = nil
		case "session/end-seed":
			if open != nil {
				stale[*open] = true
			}
			open = nil
		}
	}
	return stale
}

func assertRetryChain(retries []sessionformat.Event, data map[string]any) error {
	turn, _ := countValue(data["turn"], "retry turn")
	step, _ := countValue(data["step"], "retry step")
	provider, _ := data["provider"].(string)
	policyKey, _ := data["policyKey"].(string)
	retryId, _ := data["retryId"].(string)
	retry, err := countValue(data["retry"], "retry retry")
	if err != nil {
		return err
	}
	var prior map[string]any
	for i := len(retries) - 1; i >= 0; i-- {
		candidateData, err := decodeData(retries[i], retries[i].Type+" "+itoa(retries[i].Seq))
		if err != nil {
			return err
		}
		candidateTurn, _ := countValue(candidateData["turn"], "retry turn")
		candidateStep, _ := countValue(candidateData["step"], "retry step")
		candidateProvider, _ := candidateData["provider"].(string)
		candidatePolicy, _ := candidateData["policyKey"].(string)
		if candidateTurn == turn && candidateStep == step && candidateProvider == provider && candidatePolicy == policyKey {
			prior = candidateData
			break
		}
	}
	expected := int64(1)
	if prior != nil {
		priorRetry, err := countValue(prior["retry"], "prior retry")
		if err != nil {
			return err
		}
		expected = priorRetry + 1
	}
	if retry != expected {
		return sessionformat.FormatErrorf("llm/retry must use retry %d", expected)
	}
	if prior != nil {
		priorId, _ := prior["retryId"].(string)
		if priorId != retryId {
			return sessionformat.FormatErrorf("llm/retry must preserve retryId across one policy chain")
		}
	} else {
		for _, candidate := range retries {
			candidateData, err := decodeData(candidate, candidate.Type+" "+itoa(candidate.Seq))
			if err != nil {
				return err
			}
			candidateId, _ := candidateData["retryId"].(string)
			if candidateId == retryId {
				return sessionformat.FormatErrorf("llm/retry reuses retryId %q across policy chains", retryId)
			}
		}
	}
	return nil
}

func requireOpenStep(event sessionformat.Event, data map[string]any, openTurn, openStep any) error {
	turn, err := countValue(data["turn"], "turn")
	if err != nil {
		return err
	}
	step, err := countValue(data["step"], "step")
	if err != nil {
		return err
	}
	if openTurn == nil || openStep == nil || openTurn != turn || openStep != step {
		return sessionformat.FormatErrorf("%s does not match an open turn and step", event.Type)
	}
	return nil
}

func assertNoUnresolvedTools(lifecycles map[string]*toolLifecycle, boundary string) error {
	for callId := range lifecycles {
		return sessionformat.FormatErrorf("%s leaves unresolved tool call %s", boundary, callId)
	}
	return nil
}

func isExactToolNotStartedRepair(event sessionformat.Event, content []any, errorRecord map[string]any) bool {
	data, err := decodeData(event, event.Type+" "+itoa(event.Seq))
	if err != nil {
		return false
	}
	message, _ := data["message"].(map[string]any)
	if message == nil {
		return false
	}
	source, _ := message["source"].(map[string]any)
	if source == nil {
		return false
	}
	callId, _ := source["callId"].(string)
	_, hasSources := event.ExtraField("sourceEventSeqs")
	if errorRecord == nil {
		return false
	}
	if errorRecord["name"] != "ToolNotStartedError" || errorRecord["code"] != "TOOL_NOT_STARTED" {
		return false
	}
	if hasSources {
		return false
	}
	if message["id"] != "interrupted-tool-result-"+callId+"-"+itoa(event.Seq) {
		return false
	}
	if len(content) == 0 {
		return false
	}
	block, _ := content[0].(map[string]any)
	if block == nil || block["isError"] != true {
		return false
	}
	repairContent, _ := block["content"].([]any)
	if len(repairContent) != 1 {
		return false
	}
	repairBlock, _ := repairContent[0].(map[string]any)
	if repairBlock == nil || repairBlock["type"] != "text" {
		return false
	}
	text, _ := repairBlock["text"].(string)
	return text == "The tool call was interrupted before the Harness recorded it as started. Retry it if it is still needed."
}

func surfaceOpIsAppend(event sessionformat.Event) bool {
	raw, ok := event.ExtraField("surfaceOp")
	return ok && string(raw) == `"append"`
}

// surfaceOpReplace decodes a replace surfaceOp into its start/end pair.
func surfaceOpReplace(event sessionformat.Event) (start, end int64, ok bool, err error) {
	raw, has := event.ExtraField("surfaceOp")
	if !has {
		return 0, 0, false, nil
	}
	tree, err := sessionformat.DecodeValue(raw)
	if err != nil {
		return 0, 0, false, err
	}
	record, ok := tree.(map[string]any)
	if !ok {
		return 0, 0, false, nil
	}
	if record["op"] != "replace" {
		return 0, 0, false, nil
	}
	start, err = countValue(record["start"], "surface start")
	if err != nil {
		return 0, 0, false, err
	}
	end, err = countValue(record["end"], "surface end")
	if err != nil {
		return 0, 0, false, err
	}
	return start, end, true, nil
}

func applySurface(surface []int64, event sessionformat.Event) ([]int64, error) {
	raw, ok := event.ExtraField("surfaceOp")
	if !ok {
		return nil, sessionformat.FormatErrorf("%s requires a surfaceOp marker", event.Type)
	}
	if string(raw) == `"append"` {
		return append(append([]int64{}, surface...), event.Seq), nil
	}
	tree, err := sessionformat.DecodeValue(raw)
	if err != nil {
		return nil, sessionformat.FormatErrorf("%s replacement range is not on the current surface", event.Type)
	}
	replace, ok := tree.(map[string]any)
	if !ok {
		return nil, sessionformat.FormatErrorf("%s replacement range is not on the current surface", event.Type)
	}
	start, err := countValue(replace["start"], "replace start")
	if err != nil {
		return nil, err
	}
	end, err := countValue(replace["end"], "replace end")
	if err != nil {
		return nil, err
	}
	startIndex := indexOf(surface, start)
	endIndex := indexOf(surface, end)
	if startIndex < 0 || endIndex < startIndex {
		return nil, sessionformat.FormatErrorf("%s replacement range is not on the current surface", event.Type)
	}
	shadowed := append([]int64{}, surface[startIndex:endIndex+1]...)
	sources := map[int64]bool{}
	if sourcesRaw, hasSources := event.ExtraField("sourceEventSeqs"); hasSources {
		tree, err := sessionformat.DecodeValue(sourcesRaw)
		if err != nil {
			return nil, err
		}
		if members, ok := tree.([]any); ok {
			for _, member := range members {
				seq, err := countValue(member, "source")
				if err != nil {
					return nil, err
				}
				sources[seq] = true
			}
		}
	}
	for _, seq := range shadowed {
		if !sources[seq] {
			return nil, sessionformat.FormatErrorf("%s replacement sourceEventSeqs omit a shadowed surface node", event.Type)
		}
	}
	output := append([]int64{}, surface[:startIndex]...)
	output = append(output, event.Seq)
	output = append(output, surface[endIndex+1:]...)
	return output, nil
}

func indexOf(values []int64, target int64) int {
	for index, value := range values {
		if value == target {
			return index
		}
	}
	return -1
}

func assertTitleSourcesImpl(events []sessionformat.Event, event sessionformat.Event, data map[string]any, validateFramedText bool) error {
	seqs, ok := data["messageSeqs"].([]any)
	if !ok {
		return sessionformat.FormatErrorf("%s %d messageSeqs must be an array", event.Type, event.Seq)
	}
	if event.Type == "session/title" {
		titleSource, err := recordOf(data["source"], "session/title "+itoa(event.Seq)+" source")
		if err != nil {
			return err
		}
		empty := len(seqs) == 0
		userKind := titleSource["kind"] == "user"
		if empty != userKind {
			return sessionformat.FormatErrorf("session/title %d messageSeqs must be empty exactly for a user title", event.Seq)
		}
	}
	selectedMessages := []map[string]any{}
	for _, member := range seqs {
		seq, err := countValue(member, "messageSeqs member")
		if err != nil {
			return err
		}
		if seq >= int64(len(events)) || events[seq].Type != "user/message" {
			return sessionformat.FormatErrorf("%s %d messageSeqs must cite earlier human user/message events", event.Type, event.Seq)
		}
		source := events[seq]
		sourceData, err := decodeData(source, source.Type+" "+itoa(seq)+" data")
		if err != nil {
			return err
		}
		provenance, err := recordOf(sourceData["source"], source.Type+" "+itoa(seq)+" source")
		if err != nil {
			return err
		}
		if provenance["kind"] != "user" {
			return sessionformat.FormatErrorf("%s %d messageSeqs must cite earlier human user/message events", event.Type, event.Seq)
		}
		content, _ := sourceData["content"].([]any)
		texts := []string{}
		for _, blockValue := range content {
			block, ok := blockValue.(map[string]any)
			if !ok || block["type"] != "text" {
				continue
			}
			if text, ok := block["text"].(string); ok {
				texts = append(texts, text)
			}
		}
		selectedMessages = append(selectedMessages, map[string]any{"seq": numberOf(seq), "text": joinStrings(texts, "\n")})
	}
	if event.Type == "session/title-llm-request" {
		messages, _ := data["messages"].([]any)
		expected := "Generate the session title from this JSON array of human messages:\n" + marshalJSONString(selectedMessages)
		var message map[string]any
		if len(messages) > 0 {
			message, _ = messages[0].(map[string]any)
		}
		var content []any
		var source map[string]any
		if message != nil {
			content, _ = message["content"].([]any)
			decodedSource, sourceErr := recordOf(message["source"], "session/title-llm-request message source")
			if sourceErr != nil {
				return sourceErr
			}
			source = decodedSource
		}
		if len(messages) != 1 || message == nil || message["role"] != "user" || len(content) != 1 ||
			source == nil || source["kind"] != "plugin" || source["plugin"] != "dsh-session-title-llm" {
			return sessionformat.FormatErrorf("session/title-llm-request messages do not represent messageSeqs")
		}
		framed, _ := content[0].(map[string]any)
		if framed == nil || framed["type"] != "text" ||
			(validateFramedText && framed["text"] != expected) {
			return sessionformat.FormatErrorf("session/title-llm-request messages do not represent messageSeqs")
		}
	}
	return nil
}

func assertCompactionOwner(open *compactionState, data map[string]any, opType string) error {
	if open == nil {
		return sessionformat.FormatErrorf("%s has no matching compaction/start", opType)
	}
	id, _ := data["compactionId"].(string)
	sourceCommandId, hasSource := data["sourceCommandId"]
	if id != open.id || hasSource != open.hasSourceCmd ||
		(hasSource && sourceCommandId != open.sourceCommandId) {
		return sessionformat.FormatErrorf("%s has no matching compaction/start", opType)
	}
	return nil
}

func assertCompactionTurn(owner any, openTurn any, opType string, nullable bool) error {
	// Both sides arrive as raw payload members (json.Number) or the
	// validator's int64 open-turn marker; compare through countValue.
	if owner == nil {
		if openTurn != nil {
			return sessionformat.FormatErrorf("%s does not match the open turn", opType)
		}
		return nil
	}
	ownerTurn, err := countValue(owner, opType+" turn")
	if err != nil {
		return err
	}
	if openTurn == nil || openTurn != ownerTurn {
		return sessionformat.FormatErrorf("%s does not match the open turn", opType)
	}
	return nil
}

func assertCurrentSurfaceSpan(surface []int64, data map[string]any, opType string) error {
	rangeRecord, ok := data["shadowedRange"].(map[string]any)
	if !ok {
		return sessionformat.FormatErrorf("%s shadowedRange must be a JSON object", opType)
	}
	start, err := countValue(rangeRecord["start"], "shadowedRange start")
	if err != nil {
		return err
	}
	end, err := countValue(rangeRecord["end"], "shadowedRange end")
	if err != nil {
		return err
	}
	seqsTree, err := sessionformat.DecodeValue(mustEncodeTree(data["shadowedSeqs"]))
	if err != nil {
		return err
	}
	seqMembers, ok := seqsTree.([]any)
	if !ok {
		return sessionformat.FormatErrorf("%s shadowedSeqs must be an array", opType)
	}
	seqs := make([]int64, 0, len(seqMembers))
	for _, member := range seqMembers {
		seq, err := countValue(member, "shadowedSeqs member")
		if err != nil {
			return err
		}
		seqs = append(seqs, seq)
	}
	startIndex := indexOf(surface, start)
	endIndex := indexOf(surface, end)
	var expected []int64
	if startIndex >= 0 && endIndex >= startIndex {
		expected = append(expected, surface[startIndex:endIndex+1]...)
	}
	if len(expected) != len(seqs) {
		return sessionformat.FormatErrorf("%s shadowedSeqs do not name an exact current surface span", opType)
	}
	for index, seq := range seqs {
		if expected[index] != seq {
			return sessionformat.FormatErrorf("%s shadowedSeqs do not name an exact current surface span", opType)
		}
	}
	return nil
}

// deepEqualJSON compares two lossless JSON trees for exact value equality
// (official deepEqualJson; numeric literals compare by value).
func deepEqualJSON(left, right any) bool {
	leftEncoded, err := sessionformat.EncodeValue(left)
	if err != nil {
		return false
	}
	rightEncoded, err := sessionformat.EncodeValue(right)
	if err != nil {
		return false
	}
	return string(leftEncoded) == string(rightEncoded)
}

func quoteJSON(value string) string {
	encoded, _ := sessionformat.EncodeValue(value)
	return string(encoded)
}

func mustDecodeNumber(literal string) any {
	value, err := sessionformat.DecodeValue(jsonRawMessage(literal))
	if err != nil {
		panic(err)
	}
	return value
}

func numberOf(value int64) any { return mustDecodeNumber(itoa(value)) }

func marshalJSONString(value any) string {
	encoded, _ := sessionformat.EncodeValue(value)
	return string(encoded)
}

func mustEncodeTree(value any) jsonRawMessage { return jsonRawMessage(marshalJSONString(value)) }

func joinStrings(values []string, separator string) string {
	out := ""
	for index, value := range values {
		if index > 0 {
			out += separator
		}
		out += value
	}
	return out
}
