// Package skills discovers and loads Agent Skills (https://agentskills.io/specification)
// from ~/.hudson/skills (user scope) and <workdir>/.hudson/skills (project scope).
//
// Discovery is intentionally lenient and isolated: a skill whose SKILL.md cannot
// be parsed is skipped with a diagnostic, and never blocks the other skills or
// the rest of the program. Only the skill's name and description are held in
// memory (for the catalog surfaced in the system prompt); the full body is always
// reread from disk when the skill is activated so edits are picked up live.
package skills

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	skillFileName = "SKILL.md"
	// skillsSubdir is the skills directory inside the user Hudson root.
	skillsSubdir = "skills"
	// projectSkillsSubdir is the skills directory inside the working directory.
	projectSkillsSubdir = ".hudson" + string(filepath.Separator) + "skills"
)

// Skill is the discovered metadata for one skill. Only name and description are
// retained; Body is never cached (see Load).
type Skill struct {
	Name        string
	Description string
	Path        string // absolute path to SKILL.md
	Dir         string // absolute path to the skill directory (parent of Path)
	Scope       string // "project" or "user"
}

// Manager holds the deduplicated skill catalog. Project skills shadow user
// skills with the same name, so every name is distinct.
type Manager struct {
	skills []Skill
	byName map[string]Skill
}

// NewManager scans both scopes and builds the catalog. userHudson is the
// expanded ~/.hudson directory (so user skills live at <userHudson>/skills);
// projectRoot is the working directory (project skills live at
// <projectRoot>/.hudson/skills). It never returns an error: unreadable or
// malformed skills are skipped individually.
func NewManager(userHudson, projectRoot string) *Manager {
	project := scanSkillsDir(filepath.Join(projectRoot, projectSkillsSubdir), "project")
	user := scanSkillsDir(filepath.Join(userHudson, skillsSubdir), "user")
	sortSkills(project)
	sortSkills(user)

	m := &Manager{byName: make(map[string]Skill, len(project)+len(user))}
	for _, s := range project {
		m.add(s)
	}
	for _, s := range user {
		// add ignores a name already claimed by a project skill.
		m.add(s)
	}
	return m
}

func (m *Manager) add(s Skill) {
	if _, exists := m.byName[s.Name]; exists {
		return
	}
	m.byName[s.Name] = s
	m.skills = append(m.skills, s)
}

// Len reports the number of discovered skills.
func (m *Manager) Len() int { return len(m.skills) }

// Skills returns the catalog in deterministic order (project scope first, then
// user scope, each sorted by name).
func (m *Manager) Skills() []Skill {
	out := make([]Skill, len(m.skills))
	copy(out, m.skills)
	return out
}

// Get returns the skill metadata for name.
func (m *Manager) Get(name string) (Skill, bool) {
	s, ok := m.byName[name]
	return s, ok
}

// Load rereads the skill body from disk and returns the markdown after the
// YAML frontmatter. The body is deliberately not cached: every activation reads
// the current file so edits between activations are honoured.
func (m *Manager) Load(name string) (string, error) {
	s, ok := m.byName[name]
	if !ok {
		return "", fmt.Errorf("skill %q not found", name)
	}
	body, err := readBody(s.Path)
	if err != nil {
		return "", fmt.Errorf("load skill %q: %w", name, err)
	}
	return body, nil
}

// Catalog renders the tier-1 disclosure block: name and description for every
// skill, plus the activation instruction. It contains no skill bodies.
func (m *Manager) Catalog() string {
	if len(m.skills) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Available skills\n")
	for _, s := range m.skills {
		fmt.Fprintf(&b, "- %s: %s\n", s.Name, s.Description)
	}
	b.WriteString("\nWhen a task matches a skill's description, call activate_skill with the skill's name to load its full instructions. The skill body is read fresh from disk each time.\n")
	return b.String()
}

// scanSkillsDir returns the skills found in the immediate subdirectories of dir,
// each identified by a SKILL.md file. A missing directory yields no skills; any
// individual parse/read error is logged and that skill is skipped.
func scanSkillsDir(dir, scope string) []Skill {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil // no such directory (or unreadable) is not fatal
	}
	var out []Skill
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		skillDir := filepath.Join(dir, e.Name())
		skillFile := filepath.Join(skillDir, skillFileName)
		s, err := parseSkillFile(skillFile, skillDir, scope)
		if err != nil {
			logSkillErr(skillFile, err)
			continue
		}
		out = append(out, s)
	}
	return out
}

func parseSkillFile(path, dir, scope string) (Skill, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Skill{}, err
	}
	name, desc, _, err := parseSKILLMD(data)
	if err != nil {
		return Skill{}, err
	}
	return Skill{Name: name, Description: desc, Path: path, Dir: dir, Scope: scope}, nil
}

func readBody(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	_, _, body, err := parseSKILLMD(data)
	return body, err
}

// parseSKILLMD extracts the YAML frontmatter (name and description) and the
// markdown body that follows. The parser is intentionally minimal and lenient:
// it reads only top-level "name:" and "description:" keys, tolerates quoted
// scalar values, and treats anything after the closing "---" as the body.
func parseSKILLMD(data []byte) (name, description, body string, err error) {
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	lines := strings.Split(text, "\n")

	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return "", "", "", errors.New("missing opening frontmatter delimiter")
	}
	closeIdx := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			closeIdx = i
			break
		}
	}
	if closeIdx == -1 {
		return "", "", "", errors.New("missing closing frontmatter delimiter")
	}

	for _, line := range lines[1:closeIdx] {
		switch {
		case strings.HasPrefix(line, "name:"):
			name = scalarValue(strings.TrimPrefix(line, "name:"))
		case strings.HasPrefix(line, "description:"):
			description = scalarValue(strings.TrimPrefix(line, "description:"))
		}
	}

	name = strings.TrimSpace(name)
	description = strings.TrimSpace(description)
	if name == "" {
		return "", "", "", errors.New("missing required name")
	}
	if description == "" {
		return "", "", "", errors.New("missing required description")
	}

	body = strings.TrimSpace(strings.Join(lines[closeIdx+1:], "\n"))
	return name, description, body, nil
}

// scalarValue trims whitespace and removes a single pair of matching quotes.
func scalarValue(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			s = s[1 : len(s)-1]
		}
	}
	return s
}

func sortSkills(skills []Skill) {
	sort.Slice(skills, func(i, j int) bool { return skills[i].Name < skills[j].Name })
}

func logSkillErr(what string, err error) {
	fmt.Fprintf(os.Stderr, "skills: %s: %v\n", what, err)
}
