package sessionformat

import (
	"encoding/json"
	"errors"
	"testing"
)

// stubMigration is a test double recording calls; the real edges live in
// their own packages.
type stubMigration struct {
	name       string
	from, to   int64
	refuse     bool
	headerFunc func(Header) Header
}

func (m *stubMigration) Name() string       { return m.name }
func (m *stubMigration) FromVersion() int64 { return m.from }
func (m *stubMigration) ToVersion() int64   { return m.to }
func (m *stubMigration) MigrateHeader(header Header) (Header, error) {
	if m.refuse {
		return nil, &FormatError{Message: "bad header"}
	}
	if m.headerFunc != nil {
		return m.headerFunc(header), nil
	}
	next := header.Clone()
	next["version"] = json.Number(int64String(m.to))
	return next, nil
}
func (m *stubMigration) Migrate(artifact Artifact) (Artifact, error) {
	if m.refuse {
		return Artifact{}, &FormatError{Message: "bad artifact"}
	}
	next := Artifact{Header: artifact.Header.Clone(), InheritedEventCount: artifact.InheritedEventCount, Events: artifact.Events}
	next.Header["version"] = json.Number(int64String(m.to))
	return next, nil
}
func (m *stubMigration) ValidateTarget(artifact Artifact) error {
	if m.refuse {
		return &FormatError{Message: "target refused"}
	}
	return nil
}
func (m *stubMigration) ValidateTargetHeader(header Header) error { return nil }

func int64String(value int64) string {
	return json.Number([]byte{}).String()[:0] + func() string {
		digits := ""
		if value == 0 {
			return "0"
		}
		for value > 0 {
			digits = string(rune('0'+value%10)) + digits
			value /= 10
		}
		return digits
	}()
}

// stubCodec accepts one fixed version and echoes rows as events.
type stubCodec struct{ version int64 }

func (c *stubCodec) Version() int64 { return c.version }
func (c *stubCodec) DecodeHeader(headerValue json.RawMessage) (Header, error) {
	return DecodeHeaderValue(headerValue)
}
func (c *stubCodec) DecodeArtifact(headerValue json.RawMessage, rows []json.RawMessage) (Artifact, error) {
	return artifactFromRows(headerValue, rows)
}
func (c *stubCodec) DecodeRecoverableArtifact(headerValue json.RawMessage, rows []json.RawMessage) (Artifact, error) {
	return artifactFromRows(headerValue, rows)
}

func artifactFromRows(headerValue json.RawMessage, rows []json.RawMessage) (Artifact, error) {
	header, err := DecodeHeaderValue(headerValue)
	if err != nil {
		return Artifact{}, err
	}
	events := make([]Event, 0, len(rows))
	for index, row := range rows {
		var event Event
		if err := json.Unmarshal(row, &event); err != nil {
			return Artifact{}, &FormatError{Message: err.Error()}
		}
		if event.Seq == 0 && event.Type == "" {
			event.Seq = int64(index)
			event.Type = "stub/row"
			event.Data = row
		}
		events = append(events, event)
	}
	return Artifact{Header: header, Events: events}, nil
}

func testHeader(version int64) Header {
	return Header{
		"type": "session", "version": json.Number(int64String(version)),
		"id": "s1", "createdAt": json.Number("100"), "isSeeded": false,
		"delegationDepth": json.Number("0"),
	}
}

func restoreCurrent(artifact Artifact) (Artifact, error) { return artifact, nil }
func restoreHeader(header Header) (Header, error)        { return header, nil }

func TestCompileChainPlansAdjacentEdges(t *testing.T) {
	chain, err := CompileChain(ChainOptions{
		CurrentVersion: 2,
		Migrations: []Migration{
			&stubMigration{name: "e01", from: 0, to: 1},
			&stubMigration{name: "e12", from: 1, to: 2},
		},
		RestoreCurrent: restoreCurrent, RestoreCurrentHeader: restoreHeader,
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := chain.Plan(0)
	if err != nil || len(plan) != 2 {
		t.Fatalf("plan from 0: %v len=%d", err, len(plan))
	}
	plan, err = chain.Plan(1)
	if err != nil || len(plan) != 1 || plan[0].Name() != "e12" {
		t.Fatalf("plan from 1: %v", err)
	}
	if _, err := chain.Plan(3); err == nil {
		t.Fatal("newer stored version must be refused as unsupported")
	}
}

func TestCompileChainRejectsGapsDuplicatesAndNonAdjacent(t *testing.T) {
	base := ChainOptions{CurrentVersion: 2, RestoreCurrent: restoreCurrent, RestoreCurrentHeader: restoreHeader}
	if _, err := CompileChain(ChainOptions{
		CurrentVersion: 2, Migrations: []Migration{&stubMigration{name: "e01", from: 0, to: 1}},
		RestoreCurrent: restoreCurrent, RestoreCurrentHeader: restoreHeader,
	}); err == nil {
		t.Fatal("missing edge must refuse the chain")
	}
	_ = base
	if _, err := CompileChain(ChainOptions{
		CurrentVersion: 1, Migrations: []Migration{
			&stubMigration{name: "e01", from: 0, to: 1},
			&stubMigration{name: "e01b", from: 0, to: 1},
		},
		RestoreCurrent: restoreCurrent, RestoreCurrentHeader: restoreHeader,
	}); err == nil {
		t.Fatal("duplicate edge must refuse the chain")
	}
	if _, err := CompileChain(ChainOptions{
		CurrentVersion: 1, Migrations: []Migration{&stubMigration{name: "jump", from: 0, to: 2}},
		RestoreCurrent: restoreCurrent, RestoreCurrentHeader: restoreHeader,
	}); err == nil {
		t.Fatal("non-adjacent edge must refuse the chain")
	}
}

func TestChainMigrateRunsEveryEdgeInMemory(t *testing.T) {
	chain, err := CompileChain(ChainOptions{
		CurrentVersion: 2,
		Migrations: []Migration{
			&stubMigration{name: "e01", from: 0, to: 1},
			&stubMigration{name: "e12", from: 1, to: 2},
		},
		RestoreCurrent: restoreCurrent, RestoreCurrentHeader: restoreHeader,
	})
	if err != nil {
		t.Fatal(err)
	}
	artifact := Artifact{Header: testHeader(0), Events: []Event{{Type: "user/message", Seq: 0, Time: 1, Data: json.RawMessage(`{}`)}}}
	migrated, err := chain.Migrate(artifact)
	if err != nil {
		t.Fatal(err)
	}
	version, err := HeaderVersion(migrated.Header)
	if err != nil || version != 2 {
		t.Fatalf("migrated version: %v %v", version, err)
	}
	// The chain must refuse a migration edge failure as unsupported.
	failing, _ := CompileChain(ChainOptions{
		CurrentVersion: 1,
		Migrations:     []Migration{&stubMigration{name: "e01", from: 0, to: 1, refuse: true}},
		RestoreCurrent: restoreCurrent, RestoreCurrentHeader: restoreHeader,
	})
	if _, err := failing.Migrate(artifact); err == nil {
		t.Fatal("edge refusal must fail the migration")
	} else {
		var unsupported *UnsupportedMigrationError
		if !errors.As(err, &unsupported) {
			t.Fatalf("edge refusal must surface unsupported, got %T", err)
		}
	}
}

func TestSnapshotArtifactValidatesSharedCoordinates(t *testing.T) {
	good := Artifact{Header: testHeader(2), InheritedEventCount: 1, Events: []Event{
		{Type: "a", Seq: 0, Time: 1, Data: json.RawMessage(`{}`)},
		{Type: "b", Seq: 1, Time: 2, Data: json.RawMessage(`null`)},
	}}
	if _, err := SnapshotArtifact(good, "label"); err != nil {
		t.Fatalf("valid artifact refused: %v", err)
	}
	gap := good
	gap.Events = []Event{good.Events[0], {Type: "b", Seq: 2, Time: 2, Data: json.RawMessage(`{}`)}}
	if _, err := SnapshotArtifact(gap, "label"); err == nil {
		t.Fatal("non-dense seq must refuse")
	}
	cut := good
	cut.InheritedEventCount = 3
	if _, err := SnapshotArtifact(cut, "label"); err == nil {
		t.Fatal("inherited cut beyond the event count must refuse")
	}
	noData := good
	noData.Events = []Event{{Type: "a", Seq: 0, Time: 1}}
	if _, err := SnapshotArtifact(noData, "label"); err == nil {
		t.Fatal("missing data must refuse")
	}
}

func TestSnapshotHeaderValidatesKnownMembers(t *testing.T) {
	if _, err := SnapshotHeader(testHeader(2), "h"); err != nil {
		t.Fatalf("valid header refused: %v", err)
	}
	bad := testHeader(2)
	bad["isSeeded"] = "no"
	if _, err := SnapshotHeader(bad, "h"); err == nil {
		t.Fatal("non-boolean isSeeded must refuse")
	}
}

func TestLosslessValueRoundTrip(t *testing.T) {
	raw := json.RawMessage(`{"n":9007199254740993,"f":0.1,"s":"x","b":true,"z":null,"a":[1,2]}`)
	tree, err := DecodeValue(raw)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := EncodeValue(tree)
	if err != nil {
		t.Fatal(err)
	}
	// Go maps lose member order (documented adaptation); values and numeric
	// literals must survive exactly.
	original, err := DecodeValue(raw)
	if err != nil {
		t.Fatal(err)
	}
	roundTripped, err := DecodeValue(encoded)
	if err != nil {
		t.Fatal(err)
	}
	number := original.(map[string]any)["n"].(json.Number)
	if number.String() != "9007199254740993" {
		t.Fatalf("safe integer literal must survive exactly: %s", number.String())
	}
	fraction := roundTripped.(map[string]any)["f"].(json.Number)
	if fraction.String() != "0.1" {
		t.Fatalf("fraction literal must survive exactly: %s", fraction.String())
	}
}

func TestFilenameRules(t *testing.T) {
	if name, _ := SessionFormatLogFilename(0); name != "session.jsonl" {
		t.Fatalf("v0 name: %s", name)
	}
	if name, _ := SessionFormatLogFilename(2); name != "session.v2.jsonl" {
		t.Fatalf("v2 name: %s", name)
	}
	if version, ok := ParseSessionFormatLogFilename("session.jsonl"); !ok || version != 0 {
		t.Fatalf("parse v0: %d %v", version, ok)
	}
	if version, ok := ParseSessionFormatLogFilename("session.v12.jsonl"); !ok || version != 12 {
		t.Fatalf("parse v12: %d %v", version, ok)
	}
	for _, name := range []string{"session.v0.jsonl", "session.V2.jsonl", "session.v02.jsonl", "session.v2.jsonl.tmp", "session.v2.jsonl.zstd", "session.v2.jsonl.backup", "other.jsonl"} {
		if _, ok := ParseSessionFormatLogFilename(name); ok {
			t.Fatalf("%s must not be canonical", name)
		}
	}
}

func TestCatalogReadHeaderDispatchesDirectionally(t *testing.T) {
	catalog, err := CompileCatalog(CatalogOptions{
		CurrentVersion: 2,
		Codecs:         []Codec{&stubCodec{0}, &stubCodec{1}, &stubCodec{2}},
		Migrations: []Migration{
			&stubMigration{name: "e01", from: 0, to: 1},
			&stubMigration{name: "e12", from: 1, to: 2},
		},
		RestoreCurrent: restoreCurrent, RestoreCurrentHeader: restoreHeader,
		EncodeCurrentArtifact: func(artifact Artifact) (EncodedArtifact, error) {
			return EncodedArtifact{}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	current, err := catalog.ReadHeader(mustEncode(testHeader(2)))
	if err != nil || current.Status != HeaderCurrent {
		t.Fatalf("current read: %+v %v", current, err)
	}
	older, err := catalog.ReadHeader(mustEncode(testHeader(0)))
	if err != nil || older.Status != HeaderMigrationRequired || older.StoredVersion != 0 {
		t.Fatalf("migration-required read: %+v %v", older, err)
	}
	future, err := catalog.ReadHeader(json.RawMessage(`{"type":"session","version":3,"id":"s","createdAt":1,"isSeeded":false,"delegationDepth":0}`))
	if err != nil || future.Status != HeaderUnsupported {
		t.Fatalf("newer read: %+v %v", future, err)
	}
	malformed, err := catalog.ReadHeader(json.RawMessage(`{"type":"session"}`))
	if err != nil || malformed.Status != HeaderMalformed {
		t.Fatalf("malformed read: %+v %v", malformed, err)
	}
	junk, err := catalog.ReadHeader(json.RawMessage(`[]`))
	if err != nil || junk.Status != HeaderMalformed {
		t.Fatalf("non-object read: %+v %v", junk, err)
	}
}

func TestCatalogCompileRejectsIncompleteCodecs(t *testing.T) {
	if _, err := CompileCatalog(CatalogOptions{
		CurrentVersion: 2,
		Codecs:         []Codec{&stubCodec{0}, &stubCodec{2}},
		RestoreCurrent: restoreCurrent, RestoreCurrentHeader: restoreHeader,
		EncodeCurrentArtifact: func(Artifact) (EncodedArtifact, error) { return EncodedArtifact{}, nil },
	}); err == nil {
		t.Fatal("missing v1 codec must refuse the catalog")
	}
	if _, err := CompileCatalog(CatalogOptions{
		CurrentVersion: 1,
		Codecs:         []Codec{&stubCodec{0}, &stubCodec{1}, &stubCodec{5}},
		RestoreCurrent: restoreCurrent, RestoreCurrentHeader: restoreHeader,
		EncodeCurrentArtifact: func(Artifact) (EncodedArtifact, error) { return EncodedArtifact{}, nil },
	}); err == nil {
		t.Fatal("newer-than-current codec must refuse the catalog")
	}
}

func TestCatalogDecodeArtifactAndEventExtras(t *testing.T) {
	catalog, err := CompileCatalog(CatalogOptions{
		CurrentVersion: 2,
		Codecs:         []Codec{&stubCodec{0}, &stubCodec{1}, &stubCodec{2}},
		Migrations: []Migration{
			&stubMigration{name: "e01", from: 0, to: 1},
			&stubMigration{name: "e12", from: 1, to: 2},
		},
		RestoreCurrent: restoreCurrent, RestoreCurrentHeader: restoreHeader,
		EncodeCurrentArtifact: func(artifact Artifact) (EncodedArtifact, error) {
			return EncodedArtifact{}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	header := mustEncode(testHeader(2))
	rows := []json.RawMessage{
		json.RawMessage(`{"type":"user/message","seq":0,"time":5,"data":{"content":[]},"sourceEventSeqs":[],"surfaceOp":"append"}`),
	}
	artifact, err := catalog.DecodeArtifact(header, rows)
	if err != nil {
		t.Fatal(err)
	}
	if len(artifact.Events) != 1 {
		t.Fatalf("decoded %d events", len(artifact.Events))
	}
	if _, ok := artifact.Events[0].ExtraField("sourceEventSeqs"); !ok {
		t.Fatal("sourceEventSeqs must ride the extras losslessly")
	}
	if _, ok := artifact.Events[0].ExtraField("surfaceOp"); !ok {
		t.Fatal("surfaceOp must ride the extras losslessly")
	}
	reEncoded, err := json.Marshal(artifact.Events[0])
	if err != nil {
		t.Fatal(err)
	}
	// Member order may differ (Go map adaptation); the record must decode
	// back with identical members, and the data payload must stay
	// byte-identical.
	var originalRecord, roundTripped map[string]json.RawMessage
	if err := json.Unmarshal(rows[0], &originalRecord); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(reEncoded, &roundTripped); err != nil {
		t.Fatal(err)
	}
	if len(originalRecord) != len(roundTripped) {
		t.Fatalf("member count changed: %s", reEncoded)
	}
	for key, value := range originalRecord {
		if key == "data" {
			if string(roundTripped[key]) != string(value) {
				t.Fatalf("data payload must stay byte-identical")
			}
			continue
		}
		var originalValue, roundTrippedValue any
		_ = json.Unmarshal(value, &originalValue)
		_ = json.Unmarshal(roundTripped[key], &roundTrippedValue)
		if repr(originalValue) != repr(roundTrippedValue) {
			t.Fatalf("member %s changed: %s -> %s", key, value, roundTripped[key])
		}
	}
}

// repr renders a decoded JSON value in a stable comparable form.
func repr(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "<unmarshalable>"
	}
	return string(encoded)
}
