package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	agentsFileName = "AGENTS.md"
	includeDepth   = 4
)

// LoadAgents returns the merged AGENTS.md rules for the current project and
// user. Project rules come first; user rules are appended under a header, so
// project prose wins only by position (there is no section-name override this
// round).
func (s *Store) LoadAgents() string {
	visited := map[string]bool{}
	project, err := s.renderAgents(filepath.Join(s.projectRoot, agentsFileName), s.projectRoot, visited, 0)
	if err != nil {
		logMemoryErr("load project AGENTS.md", err)
		project = ""
	}
	user, err := s.renderAgents(filepath.Join(s.root, agentsFileName), s.root, visited, 0)
	if err != nil {
		logMemoryErr("load user AGENTS.md", err)
		user = ""
	}

	var b strings.Builder
	if project != "" {
		b.WriteString(project)
		b.WriteString("\n")
	}
	if user != "" {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString("## User rules\n")
		b.WriteString(user)
	}
	return b.String()
}

// renderAgents reads one AGENTS.md file and recursively expands @include lines.
// root is the confinement root for includes (project root for project rules,
// ~/.hudson for user rules); visited prevents loops; depth caps recursion.
func (s *Store) renderAgents(path, root string, visited map[string]bool, depth int) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	if visited[abs] {
		return "", nil // loop-proof: already included
	}
	visited[abs] = true

	data, err := os.ReadFile(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}

	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "@include ") {
			out = append(out, line)
			continue
		}
		incPath := strings.TrimSpace(strings.TrimPrefix(trimmed, "@include "))
		if incPath == "" {
			out = append(out, line)
			continue
		}
		if depth >= includeDepth {
			out = append(out, line) // keep the literal line; depth exhausted
			continue
		}
		incAbs := filepath.Clean(filepath.Join(filepath.Dir(abs), filepath.FromSlash(incPath)))
		if !confined(incAbs, root) {
			logMemoryErr("include rejected (outside root)", fmt.Errorf("%s", incAbs))
			continue
		}
		child, err := s.renderAgents(incAbs, root, visited, depth+1)
		if err != nil {
			logMemoryErr("include "+incAbs, err)
			continue
		}
		out = append(out, child)
	}
	return strings.Join(out, "\n"), nil
}

// confined reports whether path is equal to root or beneath it. Paths are
// compared in their cleaned absolute form; this is lexical confinement (no
// symlink resolution), which matches the round's simple model.
func confined(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
