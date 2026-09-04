package sessionformatv01

import (
	"encoding/json"
	"strings"
	"testing"

	"dshgo/sessionformat"
)

func testHeader(version int64) sessionformat.Header {
	return sessionformat.Header{
		"version": json.Number(itoa(version)), "id": "s1", "createdAt": json.Number("100"),
		"isSeeded": false, "delegationDepth": json.Number("0"),
	}
}

func mustEvent(t *testing.T, raw string) sessionformat.Event {
	t.Helper()
	var event sessionformat.Event
	if err := json.Unmarshal([]byte(raw), &event); err != nil {
		t.Fatalf("event decode: %v", err)
	}
	return event
}

func artifactOf(header sessionformat.Header, events ...sessionformat.Event) sessionformat.Artifact {
	return sessionformat.Artifact{Header: header, Events: events}
}

func TestMigrationNormalizesLegacyShapes(t *testing.T) {
	migration := NewMigration()
	source := artifactOf(testHeader(0),
		// legacy turn/start with trigger
		mustEvent(t, `{"type":"turn/start","seq":0,"time":1,"data":{"turn":1,"trigger":{"kind":"user"}}}`),
		mustEvent(t, `{"type":"step/start","seq":1,"time":2,"data":{"turn":1,"step":1}}`),
		// legacy steering/message without a wrapped message (fixtures carry the
		// append marker on legacy steering, per the official corpus)
		mustEvent(t, `{"type":"steering/message","seq":2,"time":3,"data":{"turn":1,"content":[{"type":"text","text":"hi"}],"source":{"kind":"user"}},"surfaceOp":"append"}`),
		// legacy assistant/message with flat content + provenance
		mustEvent(t, `{"type":"assistant/message","seq":3,"time":4,"data":{"turn":1,"step":1,"content":[{"type":"text","text":"yo"}],"provenance":{"provider":"p","model":"m"}},"surfaceOp":"append"}`),
		mustEvent(t, `{"type":"step/end","seq":4,"time":5,"data":{"turn":1,"step":1}}`),
		// legacy turn/end disposed
		mustEvent(t, `{"type":"turn/end","seq":5,"time":6,"data":{"turn":1,"reason":{"kind":"disposed"}}}`),
	)
	target, err := migration.Migrate(source)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if version, _ := sessionformat.HeaderVersion(target.Header); version != 1 {
		t.Fatalf("target version: %v", version)
	}
	if got := string(target.Events[2].Type); got != "user/message" {
		t.Fatalf("steering must become user/message, got %s", got)
	}
	var steeringData map[string]any
	if err := json.Unmarshal(target.Events[2].Data, &steeringData); err != nil {
		t.Fatal(err)
	}
	if steeringData["id"] != "legacy-message:s1:2" || steeringData["role"] != "user" {
		t.Fatalf("steering normalization: %v", steeringData)
	}
	if _, has := steeringData["turn"]; has {
		t.Fatalf("steering turn must be stripped: %v", steeringData)
	}
	var assistantData map[string]any
	if err := json.Unmarshal(target.Events[3].Data, &assistantData); err != nil {
		t.Fatal(err)
	}
	message, ok := assistantData["message"].(map[string]any)
	if !ok || message["id"] != "legacy-message:s1:3" || message["role"] != "assistant" {
		t.Fatalf("assistant message envelope: %v", assistantData)
	}
	sourceMap, _ := message["source"].(map[string]any)
	if sourceMap["kind"] != "model" || sourceMap["provider"] != "p" {
		t.Fatalf("assistant source normalization: %v", sourceMap)
	}
	if _, has := assistantData["provenance"]; has {
		t.Fatalf("provenance must be consumed: %v", assistantData)
	}
	var endData map[string]any
	if err := json.Unmarshal(target.Events[5].Data, &endData); err != nil {
		t.Fatal(err)
	}
	reason, _ := endData["reason"].(map[string]any)
	if reason["kind"] != "aborted" {
		t.Fatalf("disposed must normalize to aborted: %v", endData)
	}
	cause, _ := reason["reason"].(map[string]any)
	if cause["kind"] != "disposed" {
		t.Fatalf("aborted cause must keep disposed: %v", cause)
	}
}

func TestMigrationStripsMessagePrefixAndRefusesLegacyUnsupported(t *testing.T) {
	migration := NewMigration()
	headerEvent := mustEvent(t, `{"type":"request/header","seq":1,"time":2,"data":{"header":{"config":{"provider":"p","model":"m"},"messagePrefix":[]},"reason":"initial"}}`)
	artifact := artifactOf(testHeader(0),
		mustEvent(t, `{"type":"turn/start","seq":0,"time":1,"data":{"turn":1}}`),
		headerEvent,
		mustEvent(t, `{"type":"turn/end","seq":2,"time":3,"data":{"turn":1,"reason":{"kind":"completed"}}}`))
	target, err := migration.Migrate(artifact)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var data map[string]any
	if err := json.Unmarshal(target.Events[1].Data, &data); err != nil {
		t.Fatal(err)
	}
	header, _ := data["header"].(map[string]any)
	if _, has := header["messagePrefix"]; has {
		t.Fatalf("messagePrefix must be stripped: %v", header)
	}

	refused := artifactOf(testHeader(0),
		mustEvent(t, `{"type":"mode/set","seq":0,"time":1,"data":{"mode":"plan"}}`))
	if _, err := migration.Migrate(refused); err == nil {
		t.Fatal("mode/set must be refused as unsupported")
	}
	fallback := artifactOf(testHeader(0),
		mustEvent(t, `{"type":"turn/start","seq":0,"time":1,"data":{"turn":1}}`),
		mustEvent(t, `{"type":"request/header","seq":1,"time":2,"data":{"header":{"config":{"provider":"p","model":"m"}},"reason":"fallback"}}`),
		mustEvent(t, `{"type":"turn/end","seq":2,"time":3,"data":{"turn":1,"reason":{"kind":"completed"}}}`))
	_, err = migration.Migrate(fallback)
	if err == nil || !strings.Contains(err.Error(), "fallback") {
		t.Fatalf("request/header fallback must be refused, got %v", err)
	}
}

func TestMigrationAbortedLegacyAndErrorFlattening(t *testing.T) {
	migration := NewMigration()
	target, err := migration.Migrate(artifactOf(testHeader(0),
		mustEvent(t, `{"type":"turn/start","seq":0,"time":1,"data":{"turn":1}}`),
		mustEvent(t, `{"type":"turn/end","seq":1,"time":2,"data":{"turn":1,"reason":{"kind":"aborted"}}}`),
		mustEvent(t, `{"type":"turn/start","seq":2,"time":3,"data":{"turn":2}}`),
		mustEvent(t, `{"type":"turn/end","seq":3,"time":4,"data":{"turn":2,"reason":{"kind":"error","step":1,"message":"boom"}}}`),
	))
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var aborted map[string]any
	_ = json.Unmarshal(target.Events[1].Data, &aborted)
	reason, _ := aborted["reason"].(map[string]any)
	if reason["kind"] != "aborted" || reason["reason"].(map[string]any)["kind"] != "legacy" {
		t.Fatalf("aborted legacy cause: %v", reason)
	}
	var errored map[string]any
	_ = json.Unmarshal(target.Events[3].Data, &errored)
	errorReason, _ := errored["reason"].(map[string]any)
	if errorReason["kind"] != "error" {
		t.Fatalf("error normalization: %v", errorReason)
	}
	failure, _ := errorReason["error"].(map[string]any)
	if failure["message"] != "boom" || failure["code"] != "UNKNOWN" {
		t.Fatalf("error flattening: %v", failure)
	}
}

func TestReleasedV1ArtifactAcceptsIdentityAndRefusesUnknownType(t *testing.T) {
	artifact := artifactOf(testHeader(1),
		mustEvent(t, `{"type":"turn/start","seq":0,"time":1,"data":{"turn":1}}`),
		mustEvent(t, `{"type":"user/message","seq":1,"time":2,"data":{"id":"m1","role":"user","content":[{"type":"text","text":"hi"}],"source":{"kind":"user"}},"surfaceOp":"append"}`),
	)
	if err := AssertReleasedV1Artifact(artifact); err != nil {
		t.Fatalf("valid v1 artifact refused: %v", err)
	}
	bad := artifactOf(testHeader(1),
		mustEvent(t, `{"type":"mystery/event","seq":0,"time":1,"data":{}}`))
	err := AssertReleasedV1Artifact(bad)
	if err == nil || !strings.Contains(err.Error(), "unknown required event type") {
		t.Fatalf("unknown v1 type must refuse: %v", err)
	}
}

func TestSurfaceMetadataValidation(t *testing.T) {
	base := `{"type":"user/message","seq":1,"time":2,"data":{"id":"m1","role":"user","content":[{"type":"text","text":"x"}],"source":{"kind":"user"}}`
	appendEvent := mustEvent(t, base+`,"surfaceOp":"append"}`)
	if err := AssertReleasedSurfaceMetadata(appendEvent, 1, "user/message", AllowEmptyAssistant); err != nil {
		t.Fatalf("append refused: %v", err)
	}
	replaceEvent := mustEvent(t, base+`,"surfaceOp":{"op":"replace","start":0,"end":0},"sourceEventSeqs":[0]}`)
	if err := AssertReleasedSurfaceMetadata(replaceEvent, 1, "user/message", AllowEmptyAssistant); err != nil {
		t.Fatalf("replace refused: %v", err)
	}
	forwardRef := mustEvent(t, base+`,"surfaceOp":{"op":"replace","start":5,"end":6},"sourceEventSeqs":[5,6]}`)
	if err := AssertReleasedSurfaceMetadata(forwardRef, 1, "user/message", AllowEmptyAssistant); err == nil {
		t.Fatal("forward surface replacement must refuse")
	}
	emptySources := mustEvent(t, base+`,"surfaceOp":"append","sourceEventSeqs":[]}`)
	if err := AssertReleasedSurfaceMetadata(emptySources, 1, "user/message", AllowEmptyAssistant); err == nil {
		t.Fatal("empty sources on user/message must refuse")
	}
}

func TestCodecDecodesPhysicalHeaderWithSeedLength(t *testing.T) {
	codec := ReleasedV0Codec()
	header, err := codec.DecodeHeader(json.RawMessage(`{"type":"session","version":0,"id":"s1","createdAt":100,"delegationDepth":0,"seedLength":3}`))
	if err != nil {
		t.Fatalf("decode header: %v", err)
	}
	if header["isSeeded"] != true {
		t.Fatalf("seedLength must map to isSeeded: %v", header)
	}
	unseeded, err := codec.DecodeHeader(json.RawMessage(`{"type":"session","version":0,"id":"s1","createdAt":100,"delegationDepth":0}`))
	if err != nil {
		t.Fatal(err)
	}
	if unseeded["isSeeded"] != false {
		t.Fatalf("absent seedLength must map to unseeded: %v", unseeded)
	}
	if _, err := codec.DecodeHeader(json.RawMessage(`{"type":"session","version":1,"id":"s1","createdAt":100,"delegationDepth":0}`)); err == nil {
		t.Fatal("wrong version must refuse")
	}
}

func TestCodecExpandsPackedRows(t *testing.T) {
	codec := ReleasedV1Codec()
	header := json.RawMessage(`{"type":"session","version":1,"id":"s1","createdAt":100,"delegationDepth":0}`)
	rows := []json.RawMessage{
		json.RawMessage(`{"type":"turn/start","seq":0,"time":1,"data":{"turn":1}}`),
		json.RawMessage(`{"type":"text-chunks","seq0":1,"time0":10,"data":{"turn":1,"step":1,"index":0,"dt":[2,3],"texts":["a","b","c"]}}`),
	}
	artifact, err := codec.DecodeArtifact(header, rows)
	if err != nil {
		t.Fatalf("decode artifact: %v", err)
	}
	if len(artifact.Events) != 4 {
		t.Fatalf("packed row must expand to 3 chunk events, got %d", len(artifact.Events))
	}
	chunk := artifact.Events[1]
	if chunk.Type != "assistant/chunk" || chunk.Seq != 1 || chunk.Time != 10 {
		t.Fatalf("first chunk: %+v", chunk)
	}
	var data map[string]any
	if err := json.Unmarshal(chunk.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data["turn"] != json.Number("1") && data["turn"] != "1" {
		t.Logf("turn value: %v (%T)", data["turn"], data["turn"])
	}
	last := artifact.Events[3]
	if last.Time != 15 {
		t.Fatalf("gap-walked time: %d", last.Time)
	}
}

func TestCodecRecoversTornTail(t *testing.T) {
	codec := ReleasedV1Codec()
	header := json.RawMessage(`{"type":"session","version":1,"id":"s1","createdAt":100,"delegationDepth":0}`)
	rows := []json.RawMessage{
		json.RawMessage(`{"type":"turn/start","seq":0,"time":1,"data":{"turn":1}}`),
		json.RawMessage(`{corrupt`),
		json.RawMessage(`{"type":"step/start","seq":2,"time":3,"data":{"turn":1,"step":1}}`),
	}
	if _, err := codec.DecodeArtifact(header, rows); err == nil {
		t.Fatal("strict decode must fail on a corrupt row")
	}
	artifact, err := codec.DecodeRecoverableArtifact(header, rows)
	if err != nil {
		t.Fatalf("recoverable decode: %v", err)
	}
	if len(artifact.Events) != 1 {
		t.Fatalf("recoverable decode must keep the valid prefix, got %d", len(artifact.Events))
	}
}

func TestCodecProvenanceRanges(t *testing.T) {
	codec := ReleasedV1Codec()
	header := json.RawMessage(`{"type":"session","version":1,"id":"s1","createdAt":100,"delegationDepth":0}`)
	rows := []json.RawMessage{
		json.RawMessage(`{"type":"turn/start","seq":0,"time":1,"data":{"turn":1}}`),
		json.RawMessage(`{"type":"user/message","seq":1,"time":2,"data":{"id":"m1","role":"user","content":[{"type":"text","text":"x"}],"source":{"kind":"user"}},"surfaceOp":"append"}`),
		json.RawMessage(`{"type":"user/message","seq":2,"time":3,"data":{"id":"m2","role":"user","content":[{"type":"text","text":"y"}],"source":{"kind":"user"}},"surfaceOp":{"op":"replace","start":1,"end":1},"sourceEventSeqs":[[1,1]]}`),
	}
	artifact, err := codec.DecodeArtifact(header, rows)
	if err != nil {
		t.Fatalf("range decode: %v", err)
	}
	if len(artifact.Events) != 3 {
		t.Fatalf("events: %d", len(artifact.Events))
	}
	// Encode round-trip: provenance re-ranges.
	encoded, err := EncodeArtifact(artifact, EncodeOptions{PackChunks: true}, 1)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if len(encoded.Rows) != 3 {
		t.Fatalf("rows: %d", len(encoded.Rows))
	}
	if !strings.Contains(string(encoded.Rows[2]), `"sourceEventSeqs":[1]`) {
		t.Fatalf("single-seq provenance must encode as a number: %s", encoded.Rows[2])
	}
}

func TestChunkProvenancePacksEncode(t *testing.T) {
	codec := ReleasedV1Codec()
	header := json.RawMessage(`{"type":"session","version":1,"id":"s1","createdAt":100,"delegationDepth":0}`)
	rows := []json.RawMessage{
		json.RawMessage(`{"type":"turn/start","seq":0,"time":1,"data":{"turn":1}}`),
		json.RawMessage(`{"type":"text-chunks","seq0":1,"time0":10,"data":{"turn":1,"step":1,"index":0,"dt":[1,1],"texts":["a","b","c"]}}`),
	}
	artifact, err := codec.DecodeArtifact(header, rows)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := EncodeArtifact(artifact, EncodeOptions{PackChunks: true}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded.Rows) != 2 {
		t.Fatalf("packed encode must fold the run back, got %d rows", len(encoded.Rows))
	}
	if !strings.Contains(string(encoded.Rows[1]), `"texts":["a","b","c"]`) {
		t.Fatalf("packed row shape: %s", encoded.Rows[1])
	}
}
