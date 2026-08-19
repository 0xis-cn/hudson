package memory

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Note type names produced by the auto-node classifier.
const (
	noteTypePreference = "preference"
	noteTypeCorrection = "correction"
	noteTypeNorm       = "norm"
	noteTypeReference  = "reference"
)

// NoteMeta is one entry in memory/index.json. Path is relative to the memory
// root, e.g. "user/<id>.md" or "projects/<key>/<id>.md".
type NoteMeta struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Norm   string `json:"norm"`
	Type   string `json:"type"`
	Scope  string `json:"scope"`
	Path   string `json:"path"`
	TS     string `json:"ts"`
	Tokens int    `json:"tokens"`
}

// Index is the on-disk note index. It is the only metadata file; note content
// lives beside it in markdown files.
type Index struct {
	Notes []NoteMeta `json:"notes"`
}

func (s *Store) indexPath() string {
	return filepath.Join(s.root, memoryDir, indexFileName)
}

func (s *Store) loadIndex() (Index, error) {
	var idx Index
	data, err := os.ReadFile(s.indexPath())
	if err != nil {
		if os.IsNotExist(err) {
			return idx, nil
		}
		return idx, err
	}
	if err := json.Unmarshal(data, &idx); err != nil {
		return idx, err
	}
	if idx.Notes == nil {
		idx.Notes = []NoteMeta{}
	}
	return idx, nil
}

func (s *Store) saveIndex(idx Index) error {
	b, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.indexPath(), append(b, '\n'), 0o644)
}

// IndexText renders a preloadable note index block, sorted by scope then title,
// truncated to the token budget so the model sees the notes "as if read" without
// consuming the whole context window.
func (s *Store) IndexText() string {
	idx, err := s.loadIndex()
	if err != nil {
		logMemoryErr("load note index", err)
		return ""
	}
	if len(idx.Notes) == 0 {
		return ""
	}

	notes := append([]NoteMeta(nil), idx.Notes...)
	sort.Slice(notes, func(i, j int) bool {
		if notes[i].Scope != notes[j].Scope {
			return notes[i].Scope < notes[j].Scope
		}
		return notes[i].Title < notes[j].Title
	})

	budget := indexTokenBudget * tokensPerChar
	var b strings.Builder
	b.WriteString("## Long-term memory notes\n")
	used := len(b.String())
	for _, n := range notes {
		excerpt := s.noteExcerpt(n, 180)
		line := fmt.Sprintf("- [%s] %s: %s\n", n.Type, n.Title, excerpt)
		if used+len(line) > budget {
			b.WriteString("- … (more notes omitted)\n")
			break
		}
		b.WriteString(line)
		used += len(line)
	}
	return b.String()
}

// noteExcerpt returns a short content hint (not the title) so the preloaded
// index carries the note's gist without full retrieval. It is bounded and drops
// the first "# title" line.
func (s *Store) noteExcerpt(n NoteMeta, max int) string {
	abs := filepath.Join(s.root, memoryDir, filepath.FromSlash(n.Path))
	data, err := os.ReadFile(abs)
	if err != nil {
		return ""
	}
	text := string(data)
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		text = text[i+1:]
	}
	text = strings.TrimSpace(text)
	return truncateRunes(oneLine(text), max)
}

// saveNote persists or merges one note. unique-ify: the normalized title must
// match, or a token-overlap check must pass, before two notes are merged instead
// of duplicated.
func (s *Store) saveNote(scope, typ, title, content string) error {
	title = strings.TrimSpace(title)
	content = strings.TrimSpace(content)
	if title == "" || content == "" {
		return nil
	}
	norm := normalizeTitle(title)

	idx, err := s.loadIndex()
	if err != nil {
		return err
	}

	if target := s.findMergeTarget(idx, scope, norm, content); target >= 0 {
		meta := &idx.Notes[target]
		path := filepath.Join(s.root, memoryDir, meta.Path)
		if err := appendNote(path, title, content); err != nil {
			return err
		}
		if meta.Title == "" {
			meta.Title = title
		}
		meta.TS = time.Now().UTC().Format(time.RFC3339Nano)
		meta.Tokens = estimateStringTokens(content)
		return s.saveIndex(idx)
	}

	id := newUUID()
	rel, abs := s.notePath(scope, id)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return err
	}
	if err := writeNote(abs, title, content); err != nil {
		return err
	}
	idx.Notes = append(idx.Notes, NoteMeta{
		ID:     id,
		Title:  title,
		Norm:   norm,
		Type:   typ,
		Scope:  scope,
		Path:   rel,
		TS:     time.Now().UTC().Format(time.RFC3339Nano),
		Tokens: estimateStringTokens(content),
	})
	return s.saveIndex(idx)
}

// findMergeTarget returns the index of an existing note to merge into, or -1.
// Merge happens on identical normalized title, or on strong token overlap within
// the same scope.
func (s *Store) findMergeTarget(idx Index, scope, norm, content string) int {
	best := -1
	bestOverlap := 0.0
	for i := range idx.Notes {
		n := &idx.Notes[i]
		if n.Scope != scope {
			continue
		}
		if n.Norm == norm {
			return i // exact title match wins immediately
		}
		if o := tokenOverlap(n.Title, content); o > bestOverlap {
			bestOverlap = o
			best = i
		}
	}
	if bestOverlap >= 0.6 {
		return best
	}
	return -1
}

// notePath returns the index-relative and absolute path for a note.
func (s *Store) notePath(scope, id string) (rel, abs string) {
	if scope == "user" {
		rel = filepath.ToSlash(filepath.Join(userNotesDir, id+".md"))
	} else {
		rel = filepath.ToSlash(filepath.Join(projectDir, s.projectKey, id+".md"))
	}
	abs = filepath.Join(s.root, memoryDir, filepath.FromSlash(rel))
	return rel, abs
}

func writeNote(path, title, content string) error {
	body := "# " + oneLine(title) + "\n\n" + content + "\n"
	return os.WriteFile(path, []byte(body), 0o644)
}

func appendNote(path, title, content string) error {
	block := "\n---\n\n## " + oneLine(title) + "\n\n" + content + "\n"
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(block)
	return err
}

func normalizeTitle(s string) string {
	s = strings.ToLower(s)
	fields := strings.Fields(s)
	return strings.Join(fields, " ")
}

func tokenOverlap(a, b string) float64 {
	ta := tokenSet(a)
	tb := tokenSet(b)
	if len(ta) == 0 || len(tb) == 0 {
		return 0
	}
	inter := 0
	for k := range ta {
		if tb[k] {
			inter++
		}
	}
	smaller := len(ta)
	if len(tb) < smaller {
		smaller = len(tb)
	}
	return float64(inter) / float64(smaller)
}

func tokenSet(s string) map[string]bool {
	fields := strings.Fields(strings.ToLower(s))
	set := make(map[string]bool, len(fields))
	for _, f := range fields {
		f = strings.Trim(f, ".,;:!?\"'()[]{}")
		if len(f) >= 3 {
			set[f] = true
		}
	}
	return set
}

func estimateStringTokens(s string) int {
	return len(s) / tokensPerChar
}
