// Entry-list dialect decoding: the YAML shape of cordis.yml entry rows and
// the `!!js` expression nodes the official loader evaluates at entry
// activation. Source: vendor/include entryListSchema (`!!js` is
// tag:yaml.org,2002:js) and docs/cordis-primer.md "Loader Configuration".
package loader

import (
	"errors"
	"fmt"
	"strconv"

	"gopkg.in/yaml.v3"
)

// Entry is one plugin row, mirroring the official EntryOptions. Fields the
// runtime does not know still ride in Extra: patches may override them, and
// dropping them would silently rewrite configuration.
type Entry struct {
	ID       string
	Name     string
	Group    bool
	Inject   []string
	Provide  []string
	Config   any // for Group entries: []Entry children; otherwise a native value
	Disabled any // bool or RawExpression
	Extra    map[string]any
}

// DecodeEntryList parses one cordis.yml document: a top-level YAML array of
// entry mappings. Every structural violation fails loudly — a config the
// loader cannot read exactly is a misconfiguration, not a best-effort case.
func DecodeEntryList(data []byte) ([]Entry, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("loader: failed to parse config: %w", err)
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 {
		return nil, errors.New("loader: config document is empty")
	}
	list := root.Content[0]
	if list.Tag == "!!null" {
		return nil, nil
	}
	if list.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("loader: config must be a top-level YAML array of loader entries, got %s at line %d", kindName(list), list.Line)
	}
	entries := make([]Entry, 0, len(list.Content))
	for i, item := range list.Content {
		entry, err := decodeEntry(item)
		if err != nil {
			return nil, fmt.Errorf("loader: config entry %d: %w", i+1, err)
		}
		entries = append(entries, *entry)
	}
	return entries, nil
}

func decodeEntry(node *yaml.Node) (*Entry, error) {
	if node.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("must be a mapping (a loader entry), got %s at line %d", kindName(node), node.Line)
	}

	// Two passes: the group flag may appear after config in document order,
	// and it decides how config decodes (child rows vs native value).
	raw := map[string]*yaml.Node{}
	for i := 0; i+1 < len(node.Content); i += 2 {
		keyNode := node.Content[i]
		if _, dup := raw[keyNode.Value]; dup {
			return nil, fmt.Errorf("duplicate field %q at line %d", keyNode.Value, keyNode.Line)
		}
		raw[keyNode.Value] = node.Content[i+1]
	}

	entry := &Entry{Extra: map[string]any{}}
	for key, valueNode := range raw {
		var err error
		switch key {
		case "id":
			entry.ID, err = decodeString(valueNode)
		case "name":
			entry.Name, err = decodeString(valueNode)
		case "group":
			entry.Group, err = decodeBool(valueNode)
		case "inject":
			entry.Inject, err = decodeStringList(valueNode)
		case "provide":
			entry.Provide, err = decodeStringList(valueNode)
		case "config":
			// The group flag decides how config decodes (child rows vs
			// native value); an absent or non-bool flag decodes as native.
			isGroup := false
			if groupNode := raw["group"]; groupNode != nil {
				if flag, flagErr := decodeBool(groupNode); flagErr == nil {
					isGroup = flag
				}
			}
			if isGroup {
				entry.Config, err = decodeChildEntries(valueNode)
			} else {
				entry.Config, err = decodeValue(valueNode)
			}
		case "disabled":
			entry.Disabled, err = decodeValue(valueNode)
		default:
			entry.Extra[key], err = decodeValue(valueNode)
		}
		if err != nil {
			return nil, fmt.Errorf("field %q: %w", key, err)
		}
	}
	return entry, nil
}

func decodeChildEntries(node *yaml.Node) ([]Entry, error) {
	if node.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("group config must be a sequence of child entries, got %s at line %d", kindName(node), node.Line)
	}
	children := make([]Entry, 0, len(node.Content))
	for i, item := range node.Content {
		child, err := decodeEntry(item)
		if err != nil {
			return nil, fmt.Errorf("child entry %d: %w", i+1, err)
		}
		children = append(children, *child)
	}
	return children, nil
}

func decodeString(node *yaml.Node) (string, error) {
	if node.Kind != yaml.ScalarNode {
		return "", fmt.Errorf("must be a scalar string, got %s at line %d", kindName(node), node.Line)
	}
	if isJsTag(node.Tag) {
		return "", fmt.Errorf("must be a plain string, got a !!js expression at line %d", node.Line)
	}
	return node.Value, nil
}

func decodeBool(node *yaml.Node) (bool, error) {
	if node.Kind != yaml.ScalarNode || node.Tag != "!!bool" {
		return false, fmt.Errorf("must be a boolean, got %s %q at line %d", kindName(node), node.Value, node.Line)
	}
	return strconv.ParseBool(node.Value)
}

func decodeStringList(node *yaml.Node) ([]string, error) {
	if node.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("must be a sequence of strings, got %s at line %d", kindName(node), node.Line)
	}
	list := make([]string, 0, len(node.Content))
	for _, item := range node.Content {
		value, err := decodeString(item)
		if err != nil {
			return nil, err
		}
		list = append(list, value)
	}
	return list, nil
}

// decodeValue converts one YAML node into native values, preserving `!!js`
// scalars as RawExpression nodes exactly like the official entry-list schema.
func decodeValue(node *yaml.Node) (any, error) {
	switch node.Kind {
	case yaml.AliasNode:
		if node.Alias == nil {
			return nil, fmt.Errorf("dangling YAML alias at line %d", node.Line)
		}
		return decodeValue(node.Alias)
	case yaml.ScalarNode:
		return decodeScalar(node)
	case yaml.MappingNode:
		out := map[string]any{}
		for i := 0; i+1 < len(node.Content); i += 2 {
			key, err := decodeString(node.Content[i])
			if err != nil {
				return nil, err
			}
			value, err := decodeValue(node.Content[i+1])
			if err != nil {
				return nil, err
			}
			out[key] = value
		}
		return out, nil
	case yaml.SequenceNode:
		out := make([]any, 0, len(node.Content))
		for _, item := range node.Content {
			value, err := decodeValue(item)
			if err != nil {
				return nil, err
			}
			out = append(out, value)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unsupported YAML node %s at line %d", kindName(node), node.Line)
	}
}

func decodeScalar(node *yaml.Node) (any, error) {
	if isJsTag(node.Tag) {
		return RawExpression(node.Value), nil
	}
	switch node.Tag {
	case "!!null":
		return nil, nil
	case "!!bool":
		return strconv.ParseBool(node.Value)
	case "!!int":
		return strconv.ParseInt(node.Value, 10, 64)
	case "!!float":
		return strconv.ParseFloat(node.Value, 64)
	case "!!str", "!!timestamp":
		return node.Value, nil
	default:
		return nil, fmt.Errorf("unsupported scalar tag %q at line %d", node.Tag, node.Line)
	}
}

func kindName(node *yaml.Node) string {
	switch node.Kind {
	case yaml.DocumentNode:
		return "document"
	case yaml.SequenceNode:
		return "sequence"
	case yaml.MappingNode:
		return "mapping"
	case yaml.ScalarNode:
		return "scalar"
	case yaml.AliasNode:
		return "alias"
	default:
		return fmt.Sprintf("node(kind=%d)", node.Kind)
	}
}
