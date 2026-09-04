package sessionformatv12

import (
	"encoding/json"
	"strings"
	"testing"

	"dshgo/sessionformat"
)

func v1Header() sessionformat.Header {
	return sessionformat.Header{
		"version": json.Number("1"), "id": "s1", "createdAt": json.Number("100"),
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

// successfulStep builds one v1 step with a streamed text chunk run and its
// settled message.
func successfulStep(t *testing.T, seq0, time0 int64) []sessionformat.Event {
	t.Helper()
	return []sessionformat.Event{
		mustEvent(t, `{"type":"step/start","seq":`+itoa(seq0)+`,"time":`+itoa(time0)+`,"data":{"turn":1,"step":1}}`),
		mustEvent(t, `{"type":"assistant/chunk","seq":`+itoa(seq0+1)+`,"time":`+itoa(time0+1)+`,"data":{"turn":1,"step":1,"chunk":{"type":"text-delta","index":0,"text":"he"}}}`),
		mustEvent(t, `{"type":"assistant/chunk","seq":`+itoa(seq0+2)+`,"time":`+itoa(time0+4)+`,"data":{"turn":1,"step":1,"chunk":{"type":"text-delta","index":0,"text":"llo"}}}`),
		mustEvent(t, `{"type":"assistant/message","seq":`+itoa(seq0+3)+`,"time":`+itoa(time0+9)+`,"data":{"turn":1,"step":1,"message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"hello"}],"source":{"kind":"model","provider":"p","model":"m"}}},"sourceEventSeqs":[`+itoa(seq0+1)+`,`+itoa(seq0+2)+`],"surfaceOp":"append"}`),
		mustEvent(t, `{"type":"step/end","seq":`+itoa(seq0+4)+`,"time":`+itoa(time0+10)+`,"data":{"turn":1,"step":1}}`),
	}
}

func TestMigrationEmbedsSuccessfulAttemptStream(t *testing.T) {
	migration := NewMigration()
	source := sessionformat.Artifact{Header: v1Header(), Events: []sessionformat.Event{
		mustEvent(t, `{"type":"turn/start","seq":0,"time":1,"data":{"turn":1}}`),
	}}
	source.Events = append(source.Events, successfulStep(t, 1, 10)...)
	source.Events = append(source.Events,
		mustEvent(t, `{"type":"turn/end","seq":6,"time":21,"data":{"turn":1,"reason":{"kind":"completed"}}}`))
	target, err := migration.Migrate(source)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if version, _ := sessionformat.HeaderVersion(target.Header); version != 2 {
		t.Fatalf("target version: %v", version)
	}
	// 7 v1 events; the two chunks are consumed by the message settlement.
	if len(target.Events) != 5 {
		t.Fatalf("expected 5 dense v2 events, got %d", len(target.Events))
	}
	for index, event := range target.Events {
		if event.Seq != int64(index) {
			t.Fatalf("seq %d at index %d is not dense", event.Seq, index)
		}
	}
	message := target.Events[2]
	if message.Type != "assistant/message" {
		t.Fatalf("expected the message settlement at index 2, got %s", message.Type)
	}
	var data struct {
		Stream []json.RawMessage `json:"stream"`
		Turn   int64             `json:"turn"`
		Step   int64             `json:"step"`
	}
	if err := json.Unmarshal(message.Data, &data); err != nil {
		t.Fatal(err)
	}
	if len(data.Stream) != 1 {
		t.Fatalf("expected one compact run, got %d records", len(data.Stream))
	}
	if !strings.Contains(string(data.Stream[0]), `"texts":["he","llo"]`) ||
		!strings.Contains(string(data.Stream[0]), `"dt":[3]`) {
		t.Fatalf("compact run: %s", data.Stream[0])
	}
	if _, has := message.ExtraField("sourceEventSeqs"); has {
		t.Fatal("v2 assistant/message must not carry chunk sourceEventSeqs")
	}
	// The v2 settlement validates against the released v2 image.
	if err := AssertReleasedV2Artifact(target); err != nil {
		t.Fatalf("target validation: %v", err)
	}
}

func TestMigrationPreservesFailedAttemptAsLogOnly(t *testing.T) {
	migration := NewMigration()
	source := sessionformat.Artifact{Header: v1Header(), Events: []sessionformat.Event{
		mustEvent(t, `{"type":"turn/start","seq":0,"time":1,"data":{"turn":1}}`),
		mustEvent(t, `{"type":"step/start","seq":1,"time":2,"data":{"turn":1,"step":1}}`),
		mustEvent(t, `{"type":"assistant/chunk","seq":2,"time":3,"data":{"turn":1,"step":1,"chunk":{"type":"text-delta","index":0,"text":"par"}}}`),
		// A stream error ends the attempt without a message.
		mustEvent(t, `{"type":"step/end","seq":3,"time":4,"data":{"turn":1,"step":1}}`),
		mustEvent(t, `{"type":"turn/end","seq":4,"time":5,"data":{"turn":1,"reason":{"kind":"error","error":{"message":"x","code":"E"}}}}`),
	}}
	target, err := migration.Migrate(source)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var attemptIndex = -1
	for index, event := range target.Events {
		if event.Type == "assistant/attempt" {
			attemptIndex = index
		}
	}
	if attemptIndex < 0 {
		t.Fatal("unclaimed chunk group must become an assistant/attempt settlement")
	}
	if target.Events[attemptIndex].Seq != 2 {
		t.Fatalf("attempt must sit at the last consumed chunk position, got seq %d", target.Events[attemptIndex].Seq)
	}
	if !strings.Contains(string(target.Events[attemptIndex].Data), `"texts":["par"]`) {
		t.Fatalf("attempt stream: %s", target.Events[attemptIndex].Data)
	}
}

func TestMigrationRemapsReferences(t *testing.T) {
	migration := NewMigration()
	// A compaction summary after the assistant step cites shadowed seqs on
	// the far side of the consumed chunks.
	source := sessionformat.Artifact{Header: v1Header(), Events: append([]sessionformat.Event{
		mustEvent(t, `{"type":"turn/start","seq":0,"time":1,"data":{"turn":1}}`),
	}, successfulStep(t, 1, 10)...)}
	// seq 6: user message; then a full compaction transaction whose
	// checkpoint user/message performs the surface replacement.
	source.Events = append(source.Events,
		mustEvent(t, `{"type":"user/message","seq":6,"time":20,"data":{"id":"u2","role":"user","content":[{"type":"text","text":"again"}],"source":{"kind":"user"}},"surfaceOp":"append"}`),
		mustEvent(t, `{"type":"compaction/start","seq":7,"time":29,"data":{"compactionId":"c1","turn":1}}`),
		mustEvent(t, `{"type":"compaction/summary","seq":8,"time":30,"data":{"compactionId":"c1","summary":[{"type":"text","text":"s"}],"shadowedRange":{"start":4,"end":6},"shadowedSeqs":[4,6],"shadowedTokenCount":10,"provider":"p","model":"m"}}`),
		mustEvent(t, `{"type":"user/message","seq":9,"time":31,"data":{"id":"c1","role":"user","content":[{"type":"text","text":"[checkpoint]"}],"source":{"kind":"plugin","plugin":"compact","form":"notice","summary":"[checkpoint]","compactionId":"c1"}},"surfaceOp":{"op":"replace","start":4,"end":6},"sourceEventSeqs":[7,8,4,6]}`),
		mustEvent(t, `{"type":"compaction/end","seq":10,"time":32,"data":{"compactionId":"c1","turn":1}}`),
		mustEvent(t, `{"type":"turn/end","seq":11,"time":33,"data":{"turn":1,"reason":{"kind":"completed"}}}`),
	)
	target, err := migration.Migrate(source)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// Find the compacted summary in v2.
	for _, event := range target.Events {
		switch event.Type {
		case "compaction/summary":
			var data struct {
				ShadowedRange struct {
					Start int64 `json:"start"`
					End   int64 `json:"end"`
				} `json:"shadowedRange"`
				ShadowedSeqs []int64 `json:"shadowedSeqs"`
			}
			if err := json.Unmarshal(event.Data, &data); err != nil {
				t.Fatal(err)
			}
			// v1 seqs 4(m1),6(u2) map to v2 seqs 2(m1),4(u2): two chunks are
			// consumed between them.
			if len(data.ShadowedSeqs) != 2 || data.ShadowedSeqs[0] != 2 || data.ShadowedSeqs[1] != 4 {
				t.Fatalf("shadowedSeqs remap: %v", data.ShadowedSeqs)
			}
			if data.ShadowedRange.Start != 2 || data.ShadowedRange.End != 4 {
				t.Fatalf("shadowedRange remap: %+v", data.ShadowedRange)
			}
		case "command/done":
			var data struct {
				SourceEventSeq *int64 `json:"sourceEventSeq"`
			}
			if err := json.Unmarshal(event.Data, &data); err != nil {
				t.Fatal(err)
			}
			if data.SourceEventSeq == nil || *data.SourceEventSeq != 4 {
				t.Fatalf("command sourceEventSeq remap: %v", data.SourceEventSeq)
			}
		}
	}
	if err := AssertReleasedV2Artifact(target); err != nil {
		t.Fatalf("target validation: %v", err)
	}
}

func TestMigrationRefusesConsumedChunkReference(t *testing.T) {
	migration := NewMigration()
	// The command/done cites chunk seq 2, which the assistant/message at seq 3
	// consumes into its settlement: the reference cannot be redirected.
	source := sessionformat.Artifact{Header: v1Header(), Events: []sessionformat.Event{
		mustEvent(t, `{"type":"turn/start","seq":0,"time":1,"data":{"turn":1}}`),
		mustEvent(t, `{"type":"step/start","seq":1,"time":2,"data":{"turn":1,"step":1}}`),
		mustEvent(t, `{"type":"assistant/chunk","seq":2,"time":3,"data":{"turn":1,"step":1,"chunk":{"type":"text-delta","index":0,"text":"x"}}}`),
		mustEvent(t, `{"type":"assistant/message","seq":3,"time":4,"data":{"turn":1,"step":1,"message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"x"}],"source":{"kind":"model","provider":"p","model":"m"}}},"sourceEventSeqs":[2],"surfaceOp":"append"}`),
		mustEvent(t, `{"type":"command/run","seq":4,"time":5,"data":{"commandId":"k","name":"name","source":{"kind":"user"}}}`),
		mustEvent(t, `{"type":"command/done","seq":5,"time":6,"data":{"commandId":"k","kind":"success","sourceEventSeq":2}}`),
		mustEvent(t, `{"type":"step/end","seq":6,"time":7,"data":{"turn":1,"step":1}}`),
		mustEvent(t, `{"type":"turn/end","seq":7,"time":8,"data":{"turn":1,"reason":{"kind":"completed"}}}`),
	}}
	_, err := migration.Migrate(source)
	if err == nil || !strings.Contains(err.Error(), "consumed assistant/chunk") {
		t.Fatalf("reference to a consumed chunk must refuse, got %v", err)
	}
}

func TestV2HeaderAndSeedMarkerDiscipline(t *testing.T) {
	codec := ReleasedV2Codec()
	header, err := codec.DecodeHeader(json.RawMessage(`{"type":"session","version":2,"id":"s1","createdAt":100,"isSeeded":true,"delegationDepth":0}`))
	if err != nil {
		t.Fatalf("v2 header decode: %v", err)
	}
	if header["isSeeded"] != true {
		t.Fatalf("isSeeded: %v", header)
	}
	if _, err := codec.DecodeHeader(json.RawMessage(`{"type":"session","version":2,"id":"s1","createdAt":100,"delegationDepth":0,"seedLength":3}`)); err == nil {
		t.Fatal("v2 physical header must refuse a numeric seed cut")
	}
}

func TestV2CodecDerivesCutFromMarker(t *testing.T) {
	codec := ReleasedV2Codec()
	header := json.RawMessage(`{"type":"session","version":2,"id":"s1","createdAt":100,"isSeeded":true,"delegationDepth":0}`)
	rows := []json.RawMessage{
		json.RawMessage(`{"type":"user/message","seq":0,"time":1,"data":{"id":"u1","role":"user","content":[{"type":"text","text":"x"}],"source":{"kind":"user"}},"surfaceOp":"append"}`),
		json.RawMessage(`{"type":"session/end-seed","seq":1,"time":2,"data":{"inherited":true}}`),
		json.RawMessage(`{"type":"subagent/descriptor","seq":2,"time":3,"data":{"mode":"one-shot","version":3,"provider":"p"}}`),
	}
	artifact, err := codec.DecodeArtifact(header, rows)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if artifact.InheritedEventCount != 1 {
		t.Fatalf("cut must derive from the marker, got %d", artifact.InheritedEventCount)
	}
	unseededHeader := json.RawMessage(`{"type":"session","version":2,"id":"s1","createdAt":100,"isSeeded":false,"delegationDepth":0}`)
	if _, err := codec.DecodeArtifact(unseededHeader, rows); err == nil {
		t.Fatal("unseeded artifact with an inherited marker must refuse")
	}
}

func TestV2TargetValidationRefusesObsoleteProvenance(t *testing.T) {
	message := mustEvent(t, `{"type":"assistant/message","seq":0,"time":1,"data":{"turn":1,"step":1,"message":{"id":"m","role":"assistant","content":[{"type":"text","text":"x"}],"source":{"kind":"model","provider":"p","model":"m"}},"stream":[{"type":"text-chunks","time0":1,"index":0,"dt":[],"texts":["x"]}]},"sourceEventSeqs":[0],"surfaceOp":"append"}`)
	artifact := sessionformat.Artifact{Header: sessionformat.Header{
		"version": json.Number("2"), "id": "s1", "createdAt": json.Number("100"),
		"isSeeded": false, "delegationDepth": json.Number("0"),
	}, Events: []sessionformat.Event{message}}
	err := AssertReleasedV2Artifact(artifact)
	if err == nil || !strings.Contains(err.Error(), "obsolete chunk provenance") {
		t.Fatalf("obsolete chunk provenance must refuse: %v", err)
	}
}

func TestV2TargetValidationReassemblesStream(t *testing.T) {
	// The embedded stream must reproduce the message content exactly.
	mismatch := mustEvent(t, `{"type":"assistant/message","seq":0,"time":1,"data":{"turn":1,"step":1,"message":{"id":"m","role":"assistant","content":[{"type":"text","text":"DIFFERENT"}],"source":{"kind":"model","provider":"p","model":"m"}},"stream":[{"type":"text-chunks","time0":1,"index":0,"dt":[],"texts":["x"]}]}}`)
	artifact := sessionformat.Artifact{Header: sessionformat.Header{
		"version": json.Number("2"), "id": "s1", "createdAt": json.Number("100"),
		"isSeeded": false, "delegationDepth": json.Number("0"),
	}, Events: []sessionformat.Event{mismatch}}
	if err := AssertReleasedV2Artifact(artifact); err == nil {
		t.Fatal("content disagreement must refuse")
	} else if !strings.Contains(err.Error(), "content disagrees") {
		t.Fatalf("wrong refusal: %v", err)
	}
}
