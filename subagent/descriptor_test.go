package subagent

import (
	"encoding/json"
	"testing"

	"dshgo/session"
)

func mustEvent(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return encoded
}

func TestSnapshotOneShotDescriptor(t *testing.T) {
	data, err := SnapshotSubagentDescriptor(DescriptorInput{
		Mode: ModeOneShot, Provider: "spawn", Label: "research", HasLabel: true,
	})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if data.Version != SubagentDescriptorVersion || data.Mode != ModeOneShot || data.Provider != "spawn" {
		t.Fatalf("data = %+v", data)
	}
	if data.Label == nil || *data.Label != "research" {
		t.Fatalf("label = %+v", data.Label)
	}
	if data.AgentModel != nil || data.ToolFilter != nil {
		t.Fatal("one-shot carries no continuable composition")
	}
}

func TestSnapshotContinuableDescriptor(t *testing.T) {
	filter := &ToolRestriction{Deny: []string{"shell"}}
	effort := "high"
	data, err := SnapshotSubagentDescriptor(DescriptorInput{
		Mode: ModeContinuable, Provider: "fork", Label: "pair",
		AgentProvider: "deepseek", AgentModel: "deepseek-chat",
		AgentReasoningEffort: effort, Persona: "You pair.", ToolFilter: filter,
	})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if data.Label == nil || *data.Label != "pair" || data.AgentProvider == nil || data.AgentModel == nil {
		t.Fatalf("data = %+v", data)
	}
	if data.AgentReasoningEffort == nil || *data.AgentReasoningEffort != "high" {
		t.Fatalf("effort = %+v", data.AgentReasoningEffort)
	}
	if data.ToolFilter == nil || len(data.ToolFilter.Deny) != 1 || data.ToolFilter.Deny[0] != "shell" {
		t.Fatalf("filter = %+v", data.ToolFilter)
	}
	// The snapshot is detached: mutating the input must not alias.
	filter.Deny[0] = "mutated"
	if data.ToolFilter.Deny[0] != "shell" {
		t.Fatal("tool filter aliased its input")
	}
}

func TestSnapshotDescriptorValidation(t *testing.T) {
	cases := []struct {
		name  string
		input DescriptorInput
		want  string
	}{
		{"mode", DescriptorInput{Mode: "banana", Provider: "spawn"}, `subagent descriptor mode must be "one-shot" or "continuable"`},
		{"provider", DescriptorInput{Mode: ModeOneShot}, "subagent descriptor provider is required"},
		{"continuable label", DescriptorInput{Mode: ModeContinuable, Provider: "fork"}, "continuable subagent descriptor requires a label"},
		{"one-shot persona", DescriptorInput{Mode: ModeOneShot, Provider: "spawn", Persona: "x"}, "one-shot subagent descriptor accepts no continuable composition fields"},
		{"empty filter", DescriptorInput{Mode: ModeContinuable, Provider: "fork", Label: "l", ToolFilter: &ToolRestriction{}}, "subagent descriptor toolFilter must declare allow and/or deny"},
	}
	for _, testCase := range cases {
		if _, err := SnapshotSubagentDescriptor(testCase.input); err == nil || err.Error() != testCase.want {
			t.Fatalf("%s: err = %v, want %q", testCase.name, err, testCase.want)
		}
	}
}

// descriptorEvent builds one folded log event.
func descriptorEvent(t *testing.T, payload any) session.Event {
	t.Helper()
	return session.Event{Type: EventSubagentDescriptor, Data: mustEvent(t, payload)}
}

func TestFoldDescriptorAbsentAndVersioned(t *testing.T) {
	descriptor, err := FoldSubagentDescriptor(nil)
	if err != nil || descriptor != nil {
		t.Fatalf("empty log = %+v, %v", descriptor, err)
	}
	descriptor, err = FoldSubagentDescriptor([]session.Event{
		descriptorEvent(t, map[string]any{"version": 2, "mode": "one-shot", "provider": "old"}),
	})
	if err != nil || descriptor != nil {
		t.Fatalf("foreign version = %+v, %v", descriptor, err)
	}
}

func TestFoldDescriptorFirstAuthoritative(t *testing.T) {
	events := []session.Event{
		descriptorEvent(t, map[string]any{"version": 3, "mode": "one-shot", "provider": "first", "label": "one"}),
		descriptorEvent(t, map[string]any{"version": 3, "mode": "continuable", "provider": "second", "label": "two"}),
	}
	descriptor, err := FoldSubagentDescriptor(events)
	if err != nil {
		t.Fatalf("fold: %v", err)
	}
	if descriptor.Provider != "first" || descriptor.Mode != ModeOneShot {
		t.Fatalf("descriptor = %+v, want the first event to win", descriptor)
	}
}

func TestFoldDescriptorRejectsMalformedCurrentPayloads(t *testing.T) {
	cases := []struct {
		name    string
		payload map[string]any
		want    string
	}{
		{"unknown field", map[string]any{"version": 3, "mode": "one-shot", "provider": "p", "persona": "x"},
			`persisted subagent descriptor payload has unknown field "persona"`},
		{"mode", map[string]any{"version": 3, "mode": "banana", "provider": "p"},
			`persisted subagent descriptor mode must be "one-shot" or "continuable"`},
		{"provider type", map[string]any{"version": 3, "mode": "one-shot", "provider": 5},
			"persisted subagent descriptor provider must be a string"},
		{"continuable label absent", map[string]any{"version": 3, "mode": "continuable", "provider": "p"},
			"persisted subagent descriptor label must be a string"},
		{"continuable label type", map[string]any{"version": 3, "mode": "continuable", "provider": "p", "label": 5},
			"persisted subagent descriptor label must be a string"},
		{"optional string type", map[string]any{"version": 3, "mode": "continuable", "provider": "p", "label": "l", "persona": 5},
			"persisted subagent descriptor persona must be a string"},
		{"filter not object", map[string]any{"version": 3, "mode": "continuable", "provider": "p", "label": "l", "toolFilter": 5},
			"persisted subagent descriptor toolFilter must be an object"},
		{"filter empty", map[string]any{"version": 3, "mode": "continuable", "provider": "p", "label": "l", "toolFilter": map[string]any{}},
			"persisted subagent descriptor toolFilter must declare allow and/or deny"},
		{"filter items type", map[string]any{"version": 3, "mode": "continuable", "provider": "p", "label": "l",
			"toolFilter": map[string]any{"allow": []any{"ok", 5}}},
			"persisted subagent descriptor toolFilter.allow must be an array of strings"},
	}
	for _, testCase := range cases {
		_, err := FoldSubagentDescriptor([]session.Event{descriptorEvent(t, testCase.payload)})
		if err == nil || err.Error() != testCase.want {
			t.Fatalf("%s: err = %v, want %q", testCase.name, err, testCase.want)
		}
	}
}

func TestSeedDescriptorTurn(t *testing.T) {
	// Stage the inherited prefix through a detached session, then seed.
	prefix, err := session.NewDetached(session.SessionID("child-1"), nil, &session.SessionHeader{ID: session.SessionID("child-1")}, 0)
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	if _, err := prefix.Append(session.EventTurnStart, session.TurnStartData{Turn: 1}, nil); err != nil {
		t.Fatalf("stage append: %v", err)
	}
	descriptor, err := SnapshotSubagentDescriptor(DescriptorInput{Mode: ModeContinuable, Provider: "fork", Label: "pair"})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	seed, err := SeedDescriptorTurn(session.SessionID("child-1"), prefix.Events(), descriptor)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Contiguous from seq 0: the inherited prefix, the seed-boundary marker
	// the staging session appends, then the descriptor event.
	if len(seed) != 3 {
		t.Fatalf("seed length = %d", len(seed))
	}
	if seed[0].Type != session.EventTurnStart || seed[1].Type != session.EventEndSeed || seed[2].Type != EventSubagentDescriptor {
		t.Fatalf("order = %q, %q, %q", seed[0].Type, seed[1].Type, seed[2].Type)
	}
	if seed[2].Seq != seed[1].Seq+1 {
		t.Fatalf("seq gap: %d then %d", seed[1].Seq, seed[2].Seq)
	}
	folded, err := FoldSubagentDescriptor(seed)
	if err != nil || folded == nil || folded.Provider != "fork" || folded.Mode != ModeContinuable {
		t.Fatalf("fold of seed = %+v, %v", folded, err)
	}
}
