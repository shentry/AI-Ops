// Package knowledge is what the model is given to read besides evidence: the
// investigation skills in skills/*/SKILL.md, matched to an incident by alert
// name. Skills are reviewed repository content compiled into the binary; they
// describe how to investigate and grant no capability — a skill cannot declare
// a tool implementation, and actions stay outside the model's reach.
package knowledge

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed skills/*/SKILL.md
var skillFiles embed.FS

const (
	maxSkillBody = 4 << 10
	// An incident gets at most maxMatched skills and maxInjected bytes of
	// skill text in front of its evidence.
	maxMatched  = 2
	maxInjected = 6 << 10
)

var skillName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// Skill is one parsed SKILL.md. Tools lists the read-only tools its steps use;
// Alerts are the alert names it is matched on.
type Skill struct {
	Name        string   `yaml:"name" json:"name"`
	Description string   `yaml:"description" json:"description"`
	Alerts      []string `yaml:"alerts" json:"alerts"`
	Tools       []string `yaml:"tools" json:"tools"`
	Body        string   `yaml:"-" json:"body"`
	SHA256      string   `yaml:"-" json:"sha256"`
}

// SkillRef identifies the exact skill text a diagnosis received.
type SkillRef struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}

// Skills is the immutable catalog, sorted by name.
type Skills struct{ list []Skill }

// LoadSkills parses every embedded skill once at startup; any malformed skill
// fails the boot. Alert and tool names are checked against alerts.yml and the
// full tool registry by the repository tests: the content is fixed at build.
func LoadSkills() (*Skills, error) {
	dirs, err := skillFiles.ReadDir("skills")
	if err != nil {
		return nil, fmt.Errorf("knowledge: read skills: %w", err)
	}
	catalog := &Skills{}
	for _, dir := range dirs {
		raw, err := skillFiles.ReadFile(path.Join("skills", dir.Name(), "SKILL.md"))
		if err != nil {
			return nil, fmt.Errorf("knowledge: read skill %s: %w", dir.Name(), err)
		}
		skill, err := parseSkill(raw)
		if err != nil {
			return nil, fmt.Errorf("knowledge: skill %s: %w", dir.Name(), err)
		}
		if skill.Name != dir.Name() {
			return nil, fmt.Errorf("knowledge: skill %s is named %q", dir.Name(), skill.Name)
		}
		catalog.list = append(catalog.list, skill)
	}
	sort.Slice(catalog.list, func(i, j int) bool { return catalog.list[i].Name < catalog.list[j].Name })
	return catalog, nil
}

func parseSkill(raw []byte) (Skill, error) {
	rest, ok := bytes.CutPrefix(raw, []byte("---\n"))
	if !ok {
		return Skill{}, fmt.Errorf("missing frontmatter")
	}
	head, body, ok := bytes.Cut(rest, []byte("\n---\n"))
	if !ok {
		return Skill{}, fmt.Errorf("unterminated frontmatter")
	}
	var skill Skill
	decoder := yaml.NewDecoder(bytes.NewReader(head))
	decoder.KnownFields(true)
	if err := decoder.Decode(&skill); err != nil {
		return Skill{}, fmt.Errorf("frontmatter: %w", err)
	}
	skill.Body = strings.TrimSpace(string(body))
	switch {
	case !skillName.MatchString(skill.Name):
		return Skill{}, fmt.Errorf("name %q must be snake_case", skill.Name)
	case strings.TrimSpace(skill.Description) == "":
		return Skill{}, fmt.Errorf("description is required")
	case skill.Body == "":
		return Skill{}, fmt.Errorf("body is empty")
	case len(skill.Body) > maxSkillBody:
		return Skill{}, fmt.Errorf("body is %d bytes, limit %d", len(skill.Body), maxSkillBody)
	case slices.Contains(skill.Alerts, "") || slices.Contains(skill.Tools, ""):
		return Skill{}, fmt.Errorf("empty alert or tool name")
	}
	sum := sha256.Sum256([]byte(skill.Body))
	skill.SHA256 = hex.EncodeToString(sum[:])
	return skill, nil
}

// List returns every skill in name order.
func (s *Skills) List() []Skill { return slices.Clone(s.list) }

// Match returns the skills whose alerts intersect the incident's, in name
// order, capped at maxMatched skills and maxInjected bytes of body.
func (s *Skills) Match(alerts []string) []Skill {
	if s == nil {
		return nil
	}
	var matched []Skill
	size := 0
	for _, skill := range s.list {
		if len(matched) == maxMatched {
			break
		}
		if !slices.ContainsFunc(skill.Alerts, func(a string) bool { return slices.Contains(alerts, a) }) || size+len(skill.Body) > maxInjected {
			continue
		}
		matched = append(matched, skill)
		size += len(skill.Body)
	}
	return matched
}

// Refs identifies matched skills in a replay record.
func Refs(skills []Skill) []SkillRef {
	refs := make([]SkillRef, 0, len(skills))
	for _, skill := range skills {
		refs = append(refs, SkillRef{Name: skill.Name, SHA256: skill.SHA256})
	}
	return refs
}

// RenderSkills is the section placed before the evidence in the diagnosis
// message; empty when nothing matched.
func RenderSkills(skills []Skill) string {
	if len(skills) == 0 {
		return ""
	}
	var out strings.Builder
	out.WriteString("# 排查技能\n")
	out.WriteString("以下是仓库审阅过的排查步骤，按本次告警名匹配：只说明怎么查、哪些是常见误判，本身不是证据，也不构成执行授权。\n\n")
	for _, skill := range skills {
		fmt.Fprintf(&out, "## 技能 %s：%s\n%s\n\n", skill.Name, skill.Description, skill.Body)
	}
	return out.String()
}
