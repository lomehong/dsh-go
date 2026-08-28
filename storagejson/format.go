package storagejson

import (
	"bytes"
	"encoding/json"
	"fmt"

	"dshgo/storagedomain"
)

// UnitState is the in-memory authoritative state of one unit; the file is
// its projection. Global is nil until first written (the null sentinel).
type UnitState struct {
	Version int
	Global  json.RawMessage
	Tables  map[string]map[string]json.RawMessage
}

// Serialize renders a unit state to file content: pretty-printed JSON with a
// trailing newline. Go adaptation: object keys are marshaled in sorted order
// (encoding/json), not the source's insertion order — the stable key order
// is a legibility nicety, not a parse contract.
func Serialize(name string, state UnitState) ([]byte, error) {
	tables := map[string]map[string]json.RawMessage{}
	for table, records := range state.Tables {
		copied := make(map[string]json.RawMessage, len(records))
		for key, value := range records {
			copied[key] = value
		}
		tables[table] = copied
	}
	document := map[string]any{
		"unit":   map[string]any{"name": name, "version": state.Version},
		"global": nullable(state.Global),
		"tables": tables,
	}
	return marshalPretty(document)
}

// Parse parses file content into unit state, validating shape and version.
func Parse(text []byte, descriptor storagedomain.KvUnitDescriptor) (UnitState, error) {
	var document map[string]json.RawMessage
	if err := json.Unmarshal(text, &document); err != nil {
		return UnitState{}, storagedomain.NewUnitError(storagedomain.CodeMalformedMedium,
			"unit '%s': file is not valid JSON", descriptor.Name)
	}
	if document == nil {
		return UnitState{}, storagedomain.NewUnitError(storagedomain.CodeMalformedMedium,
			"unit '%s': file is not a JSON object", descriptor.Name)
	}
	headerRaw, hasHeader := document["unit"]
	if !hasHeader {
		return UnitState{}, storagedomain.NewUnitError(storagedomain.CodeMalformedMedium,
			"unit '%s': missing or foreign unit header", descriptor.Name)
	}
	var header struct {
		Name    string   `json:"name"`
		Version *float64 `json:"version"`
	}
	if err := json.Unmarshal(headerRaw, &header); err != nil || header.Name != descriptor.Name || header.Version == nil {
		return UnitState{}, storagedomain.NewUnitError(storagedomain.CodeMalformedMedium,
			"unit '%s': missing or foreign unit header", descriptor.Name)
	}
	version := int(*header.Version)
	if float64(version) != *header.Version {
		return UnitState{}, storagedomain.NewUnitError(storagedomain.CodeMalformedMedium,
			"unit '%s': missing or foreign unit header", descriptor.Name)
	}
	if version != descriptor.Version {
		return UnitState{}, storagedomain.NewUnitError(storagedomain.CodeVersionMismatch,
			"unit '%s': stored version %d != expected %d", descriptor.Name, version, descriptor.Version)
	}
	tablesRaw, hasTables := document["tables"]
	if !hasTables {
		return UnitState{}, storagedomain.NewUnitError(storagedomain.CodeMalformedMedium,
			"unit '%s': tables is not an object", descriptor.Name)
	}
	var tables map[string]json.RawMessage
	if err := json.Unmarshal(tablesRaw, &tables); err != nil || tables == nil {
		return UnitState{}, storagedomain.NewUnitError(storagedomain.CodeMalformedMedium,
			"unit '%s': tables is not an object", descriptor.Name)
	}
	state := UnitState{
		Version: version,
		Global:  nullableOrNil(document["global"]),
		Tables:  map[string]map[string]json.RawMessage{},
	}
	for _, table := range descriptor.Tables {
		recordsRaw, present := tables[table]
		if !present {
			state.Tables[table] = map[string]json.RawMessage{}
			continue
		}
		var records map[string]json.RawMessage
		if err := json.Unmarshal(recordsRaw, &records); err != nil || records == nil {
			return UnitState{}, storagedomain.NewUnitError(storagedomain.CodeMalformedMedium,
				"unit '%s': table '%s' is not an object", descriptor.Name, table)
		}
		state.Tables[table] = records
	}
	return state, nil
}

// SerializeRecord renders one per-record document: the unit's version stamp
// plus the record value, pretty-printed like the whole-unit document.
func SerializeRecord(version int, value json.RawMessage) ([]byte, error) {
	return marshalPretty(map[string]any{"version": version, "record": nullable(value)})
}

// ParseRecord parses one per-record document, validating its version stamp.
// A malformed document or one stamped with a different version is FOREIGN
// and reads as absent — the per-record contract: one bad or stale record
// file must not brick the whole unit, and a version bump discards stale
// records instead of migrating them (the whole-unit format rejects instead,
// because there is exactly one document).
func ParseRecord(text []byte, version int) (json.RawMessage, bool) {
	var document struct {
		Version *int            `json:"version"`
		Record  json.RawMessage `json:"record"`
	}
	if err := json.Unmarshal(text, &document); err != nil {
		return nil, false
	}
	if document.Version == nil || *document.Version != version {
		return nil, false
	}
	return document.Record, true
}

// nullable maps the nil sentinel to a JSON null for encoding.
func nullable(value json.RawMessage) any {
	if value == nil {
		return nil
	}
	return value
}

// nullableOrNil maps a JSON null or an absent member back to the nil
// sentinel.
func nullableOrNil(value json.RawMessage) json.RawMessage {
	if len(value) == 0 || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		return nil
	}
	return value
}

// marshalPretty is the shared pretty encoding: two-space indent plus a
// trailing newline.
func marshalPretty(document any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(document); err != nil {
		return nil, fmt.Errorf("serialize unit document: %w", err)
	}
	return buffer.Bytes(), nil
}
