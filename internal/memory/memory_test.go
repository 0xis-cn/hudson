package memory

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mewcode/internal/conversation"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// C: AGENTS.md merges project first, then user rules under a header, and
// @include expands relative files.
func TestLoadAgentsMergeAndInclude(t *testing.T) {
	root := t.TempDir()
	proj := t.TempDir()
	writeFile(t, filepath.Join(proj, "AGENTS.md"), "project rule\n@include sub/rules.md\n")
	writeFile(t, filepath.Join(proj, "sub", "rules.md"), "project include rule\n")
	writeFile(t, filepath.Join(root, "AGENTS.md"), "user rule\n")

	s, err := NewStore(root, proj, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := s.LoadAgents()
	if !strings.Contains(got, "project rule") || !strings.Contains(got, "project include rule") {
		t.Errorf("missing project rules: %q", got)
	}
	if !strings.Contains(got, "## User rules") || !strings.Contains(got, "user rule") {
		t.Errorf("missing user rules: %q", got)
	}
	if strings.Index(got, "project rule") > strings.Index(got, "user rule") {
		t.Errorf("project rules should come before user rules: %q", got)
	}
}

// C: @include is loop-proof via a visited set.
func TestLoadAgentsLoopProof(t *testing.T) {
	proj := t.TempDir()
	root := t.TempDir()
	writeFile(t, filepath.Join(proj, "AGENTS.md"), "@include a.md\n")
	writeFile(t, filepath.Join(proj, "a.md"), "A\n@include b.md\n")
	writeFile(t, filepath.Join(proj, "b.md"), "B\n@include a.md\n")

	s, err := NewStore(root, proj, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := s.LoadAgents()
	if c := strings.Count(got, "A\n"); c > 1 {
		t.Errorf("include loop duplicated A: %q", got)
	}
}

// C: project includes cannot escape the project root.
func TestLoadAgentsConfinedToProject(t *testing.T) {
	proj := t.TempDir()
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.md")
	writeFile(t, outside, "TOP SECRET\n")
	writeFile(t, filepath.Join(proj, "AGENTS.md"), "@include ../"+filepath.Base(filepath.Dir(outside))+"/secret.md\n")

	s, err := NewStore(root, proj, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := s.LoadAgents()
	if strings.Contains(got, "TOP SECRET") {
		t.Errorf("project include escaped its root: %q", got)
	}
}

// C: session transcript appends valid JSONL, preserves malformed lines, and
// derives id/title/count from a scan.
func TestSessionAppendAndScan(t *testing.T) {
	root := t.TempDir()
	s, err := NewStore(root, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.NewSession(); err != nil {
		t.Fatal(err)
	}

	s.RecordUser("hello world")
	s.RecordAssistant("sure", []conversation.ToolUseBlock{{ID: "c1", Name: "read_file", Input: json.RawMessage(`{"path":"a"}`)}})
	s.RecordToolResults([]conversation.ToolResultBlock{{ToolUseID: "c1", Content: "body", IsError: false}})

	sess, err := s.LoadSession(s.SessionID())
	if err != nil {
		t.Fatal(err)
	}
	if len(sess.Records) != 3 {
		t.Fatalf("records = %d, want 3: %+v", len(sess.Records), sess.Records)
	}
	if sess.Diag.BadLines != 0 || sess.Diag.TruncatedToolCalls != 0 {
		t.Errorf("unexpected diagnostics: %+v", sess.Diag)
	}

	// A malformed line is kept as-is and skipped on scan.
	f, err := os.OpenFile(s.sessionPath(s.SessionID()), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{not json}\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	sess, err = s.LoadSession(s.SessionID())
	if err != nil {
		t.Fatal(err)
	}
	if sess.Diag.BadLines != 1 || len(sess.RawLines) != 1 || sess.RawLines[0] != "{not json}" {
		t.Errorf("malformed line not preserved: %+v", sess)
	}

	sum, err := s.ListSessions()
	if err != nil || len(sum) != 1 {
		t.Fatalf("list sessions = %v, %v", sum, err)
	}
	if sum[0].ID != s.SessionID() || sum[0].Title != "hello world" || sum[0].Count != 3 {
		t.Errorf("summary = %+v", sum[0])
	}
}

// C: diagnostics flag assistant tool calls whose results never arrived.
func TestTruncatedToolCallDiagnostic(t *testing.T) {
	recs := []Record{
		{Role: "user", Content: "do it"},
		{Role: "assistant", ToolUses: []ToolUse{{ID: "t1", Name: "read", Input: json.RawMessage(`{}`)}}},
	}
	if got := countTruncatedToolCalls(recs); got != 1 {
		t.Errorf("truncated tool calls = %d, want 1", got)
	}
	recs = append(recs, Record{Role: "user", ToolResults: []ToolResult{{ToolUseID: "t1", Content: "ok"}}})
	if got := countTruncatedToolCalls(recs); got != 0 {
		t.Errorf("truncated tool calls = %d, want 0", got)
	}
}

// C: notes are unique-ified by normalized title and overlap, merging instead of
// duplicating; the index preloads a compact block.
func TestNoteSaveMergeAndIndex(t *testing.T) {
	root := t.TempDir()
	s, err := NewStore(root, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}

	if err := s.saveNote("project", "preference", "Use fish shell", "Always use fish for commands."); err != nil {
		t.Fatal(err)
	}
	if err := s.saveNote("project", "preference", "use fish shell", "Additional: use fish for scripts too."); err != nil {
		t.Fatal(err)
	}

	idx, err := s.loadIndex()
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.Notes) != 1 {
		t.Fatalf("notes = %d, want 1 (merged): %+v", len(idx.Notes), idx.Notes)
	}

	text := s.IndexText()
	if !strings.Contains(strings.ToLower(text), "fish shell") {
		t.Errorf("index text missing note title: %q", text)
	}
}

// C: selecting a session updates the store's current id so later appends target
// the selected transcript.
func TestLoadSessionSelectsCurrent(t *testing.T) {
	root := t.TempDir()
	s, err := NewStore(root, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.NewSession(); err != nil {
		t.Fatal(err)
	}
	first := s.SessionID()
	if err := s.NewSession(); err != nil {
		t.Fatal(err)
	}
	second := s.SessionID()
	if first == second {
		t.Fatalf("expected two distinct session ids, got %q", first)
	}
	if _, err := s.LoadSession(first); err != nil {
		t.Fatal(err)
	}
	if s.SessionID() != first {
		t.Errorf("SessionID = %q, want %q", s.SessionID(), first)
	}
}

// C: RecordsToMessages round-trips roles, tool calls and tool results.
func TestRecordsToMessagesRoundTrip(t *testing.T) {
	recs := []Record{
		{Role: "user", Content: "read it"},
		{Role: "assistant", Content: "ok", ToolUses: []ToolUse{{ID: "c1", Name: "read_file", Input: json.RawMessage(`{"path":"a"}`)}}},
		{Role: "user", ToolResults: []ToolResult{{ToolUseID: "c1", Content: "body", IsError: false}}},
	}
	msgs := RecordsToMessages(recs)
	if len(msgs) != 3 {
		t.Fatalf("msgs = %d, want 3", len(msgs))
	}
	if msgs[0].Role != conversation.RoleUser || msgs[0].Content != "read it" {
		t.Errorf("msg0 = %+v", msgs[0])
	}
	if msgs[1].Role != conversation.RoleAssistant || len(msgs[1].ToolUses) != 1 || msgs[1].ToolUses[0].Name != "read_file" {
		t.Errorf("msg1 = %+v", msgs[1])
	}
	if msgs[2].Role != conversation.RoleUser || len(msgs[2].ToolResults) != 1 || msgs[2].ToolResults[0].ToolUseID != "c1" {
		t.Errorf("msg2 = %+v", msgs[2])
	}
}
