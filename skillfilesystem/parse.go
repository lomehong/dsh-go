// Frontmatter parsing for local skills: YAML frontmatter with the
// name/description/whenToUse/invocation contract, tolerant booleans, and
// fail-soft containment — every malformed file is skipped with a warning,
// never fatal to discovery.
package skillfilesystem

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"

	"dshgo/skill"
	"gopkg.in/yaml.v3"
)

// parsedSkill is one successfully parsed local skill file.
type parsedSkill struct {
	name        string
	description string
	whenToUse   string
	invocation  skill.InvocationPolicy
	metadata    map[string]any
	content     string
}

// parseSkillFile reads and parses one skill file. A missing or malformed
// file is absent (nil), with the reason warned; hard IO failures other than
// absence propagate.
func (p *Provider) parseSkillFile(path string, options skill.LookupOptions) (*parsedSkill, error) {
	if err := options.Context.Err(); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if isAbsentPathError(err) {
			return nil, nil
		}
		return nil, err
	}
	frontmatter, body, err := parseFrontmatter(string(raw))
	if err != nil {
		p.warnf("skill file %s ignored: invalid YAML frontmatter: %v", path, err)
		return nil, nil
	}
	if frontmatter == nil {
		p.warnf("skill file %s ignored: missing YAML frontmatter", path)
		return nil, nil
	}
	name, err := stringField(frontmatter, "name")
	if err != nil {
		p.warnf("skill file %s ignored: invalid frontmatter: %v", path, err)
		return nil, nil
	}
	description, err := stringField(frontmatter, "description")
	if err != nil {
		p.warnf("skill file %s ignored: invalid frontmatter: %v", path, err)
		return nil, nil
	}
	if name == nil || description == nil {
		p.warnf("skill file %s ignored: frontmatter requires name and description", path)
		return nil, nil
	}
	if !skill.IsSkillName(*name) {
		p.warnf("skill file %s ignored: invalid skill name %q", path, *name)
		return nil, nil
	}
	invocation, err := parseInvocationPolicy(frontmatter)
	if err != nil {
		p.warnf("skill file %s ignored: invalid invocation frontmatter: %v", path, err)
		return nil, nil
	}
	whenToUse, err := stringField(frontmatter, "whenToUse")
	if err != nil {
		p.warnf("skill file %s ignored: invalid frontmatter: %v", path, err)
		return nil, nil
	}
	parsed := &parsedSkill{
		name:        *name,
		description: *description,
		invocation:  invocation,
		content:     strings.TrimSpace(body),
	}
	if whenToUse != nil {
		parsed.whenToUse = *whenToUse
	}
	if metadata, ok := frontmatter["metadata"].(map[string]any); ok {
		parsed.metadata = metadata
	}
	return parsed, nil
}

// parseFrontmatter splits the leading `---` YAML block from the body. A file
// without frontmatter returns nil; malformed YAML returns an error.
func parseFrontmatter(raw string) (map[string]any, string, error) {
	lines := strings.Split(raw, "\n")
	if len(lines) == 0 || strings.TrimRight(lines[0], "\r") != "---" {
		return nil, "", nil
	}
	closing := -1
	for index := 1; index < len(lines); index++ {
		if strings.TrimRight(lines[index], "\r") == "---" {
			closing = index
			break
		}
	}
	if closing < 0 {
		return nil, "", fmt.Errorf("unterminated frontmatter block")
	}
	block := strings.Join(lines[1:closing], "\n")
	var data map[string]any
	if err := yaml.Unmarshal([]byte(block), &data); err != nil {
		return nil, "", err
	}
	body := strings.Join(lines[closing+1:], "\n")
	return data, body, nil
}

// stringField reads one optional string field; a present non-string fails.
func stringField(data map[string]any, key string) (*string, error) {
	value, present := data[key]
	if !present || value == nil {
		return nil, nil
	}
	text, ok := value.(string)
	if !ok {
		return nil, fmt.Errorf("frontmatter field %q must be a string", key)
	}
	return &text, nil
}

// parseInvocationPolicy resolves the model/user invocation controls,
// rejecting legacy kebab keys with the official guidance and accepting
// tolerant booleans.
func parseInvocationPolicy(data map[string]any) (skill.InvocationPolicy, error) {
	for _, legacy := range []string{"disable-model-invocation", "disableModelInvocation"} {
		if _, present := data[legacy]; present {
			return skill.InvocationPolicy{}, fmt.Errorf("frontmatter field %q is unsupported; use \"invocation\"", legacy)
		}
	}
	invocation := skill.InvocationPolicy{ModelInvocable: true, UserInvocable: true}
	block, present := data["invocation"]
	if !present || block == nil {
		return invocation, nil
	}
	fields, ok := block.(map[string]any)
	if !ok {
		return skill.InvocationPolicy{}, fmt.Errorf("frontmatter field \"invocation\" must be an object")
	}
	for _, legacy := range []string{"disable-model-invocation", "disableModelInvocation"} {
		if _, present := fields[legacy]; present {
			return skill.InvocationPolicy{}, fmt.Errorf("invocation field %q is unsupported; use \"model\" and \"user\"", legacy)
		}
	}
	if value, present := fields["model"]; present {
		flag, err := tolerantBool("model", value)
		if err != nil {
			return skill.InvocationPolicy{}, err
		}
		invocation.ModelInvocable = flag
	}
	if value, present := fields["user"]; present {
		flag, err := tolerantBool("user", value)
		if err != nil {
			return skill.InvocationPolicy{}, err
		}
		invocation.UserInvocable = flag
	}
	return invocation, nil
}

// tolerantBool accepts the boolean spellings YAML authors actually write.
func tolerantBool(key string, value any) (bool, error) {
	switch typed := value.(type) {
	case bool:
		return typed, nil
	case int:
		switch typed {
		case 1:
			return true, nil
		case 0:
			return false, nil
		}
	case float64:
		switch typed {
		case 1:
			return true, nil
		case 0:
			return false, nil
		}
	case string:
		switch strings.ToLower(typed) {
		case "true", "yes", "on":
			return true, nil
		case "false", "no", "off":
			return false, nil
		}
	}
	return false, fmt.Errorf("frontmatter field %q must be a boolean", key)
}

func (p *Provider) warnf(format string, args ...any) {
	if p.logger != nil {
		p.logger.Warn(fmt.Sprintf(format, args...))
	}
}

func isAbsentPathError(err error) bool {
	return os.IsNotExist(err) || errors.Is(err, syscall.ENOTDIR)
}
