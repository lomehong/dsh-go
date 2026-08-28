package workflow

import (
	"encoding/json"
	"fmt"
	"strings"
)

// metaFields is the recognized meta vocabulary; anything else is a violation.
var metaFields = map[string]bool{
	"name": true, "description": true, "whenToUse": true, "phases": true,
}

// phaseFields is the recognized phase vocabulary.
var phaseFields = map[string]bool{
	"title": true, "detail": true, "provider": true, "model": true,
}

// ValidateMeta validates a caller-provided meta value against the
// WorkflowMeta contract and fails with META_INVALID naming every violation
// (unknown fields, missing/mistyped name/description, malformed phases).
// The returned meta is a NORMALIZED copy built from the validated fields, so
// the engine never aliases the caller's object. Meta arrives as
// schema-checked JSON data, never evaluated script text; evaluating it on
// the host could run getters outside the worker timeout that exists to
// isolate model-written code.
func ValidateMeta(value any) (WorkflowMeta, error) {
	meta, violations := validateMetaShape(value)
	if meta == nil {
		return WorkflowMeta{}, NewWorkflowError(
			fmt.Sprintf("invalid meta: %s", strings.Join(violations, "; ")),
			CodeMetaInvalid, nil, nil)
	}
	return *meta, nil
}

// ValidateMetaJSON validates one raw JSON document (the wire form of the
// model's meta parameter) through the same contract.
func ValidateMetaJSON(data []byte) (WorkflowMeta, error) {
	var decoded any
	if err := json.Unmarshal(data, &decoded); err != nil {
		return WorkflowMeta{}, NewWorkflowError(
			fmt.Sprintf("invalid meta: %s", err.Error()), CodeMetaInvalid, nil, nil)
	}
	return ValidateMeta(decoded)
}

// validateMetaShape collects shape violations for a meta value (plain JSON
// data by the seam contract).
func validateMetaShape(value any) (*WorkflowMeta, []string) {
	var violations []string
	record, ok := value.(map[string]any)
	if !ok {
		return nil, []string{"meta must be an object"}
	}
	for key := range record {
		if !metaFields[key] {
			violations = append(violations, fmt.Sprintf(
				"meta.%s is not a recognized field (name/description/whenToUse/phases)", key))
		}
	}
	name, _ := record["name"].(string)
	if name == "" {
		violations = append(violations, "meta.name must be a non-empty string")
	}
	description, _ := record["description"].(string)
	if description == "" {
		violations = append(violations, "meta.description must be a non-empty string")
	}
	whenToUse := ""
	if raw, present := record["whenToUse"]; present && raw != nil {
		text, ok := raw.(string)
		if !ok {
			violations = append(violations, "meta.whenToUse must be a string")
		} else {
			whenToUse = text
		}
	}
	var phases []WorkflowPhase
	if raw, present := record["phases"]; present && raw != nil {
		items, ok := raw.([]any)
		if !ok {
			violations = append(violations, "meta.phases must be an array")
		} else {
			for index, item := range items {
				phase, phaseViolations := validatePhase(index, item)
				violations = append(violations, phaseViolations...)
				if len(phaseViolations) == 0 {
					phases = append(phases, phase)
				}
			}
		}
	}
	if len(violations) > 0 {
		return nil, violations
	}
	meta := WorkflowMeta{Name: name, Description: description, WhenToUse: whenToUse, Phases: phases}
	return &meta, nil
}

// validatePhase validates one phases[] entry.
func validatePhase(index int, item any) (WorkflowPhase, []string) {
	phasePath := fmt.Sprintf("meta.phases[%d]", index)
	entry, ok := item.(map[string]any)
	if !ok {
		return WorkflowPhase{}, []string{phasePath + " must be an object"}
	}
	var violations []string
	for key := range entry {
		if !phaseFields[key] {
			violations = append(violations, fmt.Sprintf("%s.%s is not a recognized field", phasePath, key))
		}
	}
	title, _ := entry["title"].(string)
	if title == "" {
		violations = append(violations, phasePath+".title must be a non-empty string")
	}
	phase := WorkflowPhase{Title: title}
	if raw, present := entry["detail"]; present && raw != nil {
		if text, ok := raw.(string); ok {
			phase.Detail = text
		} else {
			violations = append(violations, phasePath+".detail must be a string")
		}
	}
	if raw, present := entry["provider"]; present && raw != nil {
		if text, ok := raw.(string); ok {
			phase.Provider = text
		} else {
			violations = append(violations, phasePath+".provider must be a string")
		}
	}
	if raw, present := entry["model"]; present && raw != nil {
		if text, ok := raw.(string); ok {
			phase.Model = text
		} else {
			violations = append(violations, phasePath+".model must be a string")
		}
	}
	return phase, violations
}
