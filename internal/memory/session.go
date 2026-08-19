package memory

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"mewcode/internal/conversation"
)

// Record is one line of a session transcript. role is "user" or "assistant";
// tool results are carried on user-role records, mirroring the conversation
// package and both wire formats.
type Record struct {
	ID          string       `json:"id"`
	TS          string       `json:"ts"`
	Role        string       `json:"role"`
	Content     string       `json:"content"`
	ToolUses    []ToolUse    `json:"tool_uses,omitempty"`
	ToolResults []ToolResult `json:"tool_results,omitempty"`
}

// ToolUse is a persisted assistant tool-call request.
type ToolUse struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// ToolResult is a persisted tool outcome.
type ToolResult struct {
	ToolUseID string `json:"tool_use_id"`
	Content   string `json:"content"`
	IsError   bool   `json:"is_error"`
}

// Diagnostics summarises health problems found while scanning a transcript.
type Diagnostics struct {
	BadLines           int  `json:"bad_lines"`
	TruncatedToolCalls int  `json:"truncated_tool_calls"`
	EstimatedTokens    int  `json:"estimated_tokens"`
	OverTokenLimit     bool `json:"over_token_limit"`
}

// Session is a parsed transcript. RawLines preserves lines that did not parse
// as a Record so malformed data is never silently dropped.
type Session struct {
	ID       string
	Path     string
	Records  []Record
	RawLines []string
	Diag     Diagnostics
}

// SessionSummary is the /session list entry. Title is the first user message
// truncated to 80 runes; Count is the number of valid record lines.
type SessionSummary struct {
	ID    string
	Title string
	Count int
}

const titleMaxRunes = 80

// scanSummary cheaply derives a summary without retaining the whole transcript.
func scanSummary(path string) (SessionSummary, bool) {
	sum := SessionSummary{ID: strings.TrimSuffix(filepath.Base(path), ".jsonl")}
	f, err := os.Open(path)
	if err != nil {
		return sum, false
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if line == "" {
			continue
		}
		var rec Record
		if json.Unmarshal([]byte(line), &rec) != nil {
			continue
		}
		sum.Count++
		if sum.Title == "" && rec.Role == "user" && rec.Content != "" {
			sum.Title = truncateRunes(oneLine(rec.Content), titleMaxRunes)
		}
	}
	return sum, true
}

// scanSession reads a transcript line by line, splitting valid records from
// malformed raw lines and computing diagnostics.
func scanSession(path string) (*Session, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	sess := &Session{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if line == "" {
			continue
		}
		var rec Record
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			sess.RawLines = append(sess.RawLines, line)
			sess.Diag.BadLines++
			continue
		}
		sess.Records = append(sess.Records, rec)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	sess.Diag.TruncatedToolCalls = countTruncatedToolCalls(sess.Records)
	sess.Diag.EstimatedTokens = estimateTokens(sess.Records)
	sess.Diag.OverTokenLimit = sess.Diag.EstimatedTokens > foldTokenLimit
	return sess, nil
}

// countTruncatedToolCalls flags assistant tool_uses that never received a
// matching tool_result anywhere later in the transcript (an interrupted turn).
func countTruncatedToolCalls(recs []Record) int {
	seen := map[string]bool{}
	for _, r := range recs {
		for _, tr := range r.ToolResults {
			seen[tr.ToolUseID] = true
		}
	}
	count := 0
	for _, r := range recs {
		if r.Role != "assistant" {
			continue
		}
		for _, u := range r.ToolUses {
			if !seen[u.ID] {
				count++
			}
		}
	}
	return count
}

// estimateTokens is a cheap character-based estimator shared by diagnostics and
// folding. It is intentionally not a tokenizer.
func estimateTokens(recs []Record) int {
	var chars int
	for _, r := range recs {
		chars += len(r.Content)
		for _, u := range r.ToolUses {
			chars += len(u.Name) + len(u.Input)
		}
		for _, tr := range r.ToolResults {
			chars += len(tr.Content)
		}
	}
	return chars / tokensPerChar
}

// estimateMessageTokens estimates the token weight of in-memory messages.
func estimateMessageTokens(msgs []conversation.Message) int {
	var chars int
	for _, m := range msgs {
		chars += len(m.Content)
		for _, u := range m.ToolUses {
			chars += len(u.Name) + len(u.Input)
		}
		for _, r := range m.ToolResults {
			chars += len(r.Content)
		}
	}
	return chars / tokensPerChar
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.TrimSpace(s)
}
