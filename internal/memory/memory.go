// Package memory implements MewCode's persistent memory: AGENTS.md rules,
// session transcripts as JSONL, and long-term notes with a preloadable index.
//
// This round deliberately keeps the design file-based and single-writer:
// sessions live under ~/.hudson/sessions/*.jsonl and notes live under
// ~/.hudson/memory/{user,projects/<key>}/*.md with a single index.json. There
// is no vector database, RAG, or group control yet.
package memory

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"mewcode/internal/conversation"
	"mewcode/internal/llm"
)

const (
	sessionsDir   = "sessions"
	memoryDir     = "memory"
	indexFileName = "index.json"
	userNotesDir  = "user"
	projectDir    = "projects"

	// foldMessageLimit triggers folding when the in-memory history exceeds this
	// many messages. foldTokenLimit is the estimated-token equivalent.
	foldMessageLimit = 64
	foldTokenLimit   = 40_000
	// foldTimeLimit folds a session that has been open longer than this, even if
	// it is short, so wall-clock-long sessions do not grow unbounded.
	foldTimeLimit = 2 * time.Hour
	// foldKeep is the tail size retained in memory after a fold.
	foldKeep = 16

	// indexTokenBudget caps the preloaded note index text.
	indexTokenBudget = 3_000
	// tokensPerChar is the rough estimator for both the index budget and
	// diagnostics; it is intentionally cheap, not a tokenizer.
	tokensPerChar = 4
)

// Store owns the ~/.hudson directory and the current session selection. It is
// the single writer for both session transcripts and notes. LLM-backed work
// (auto-node update and folding) is optional: client may be nil, in which case
// those features are skipped but persistence still works.
type Store struct {
	root        string // ~/.hudson (expanded absolute)
	projectRoot string // absolute working directory being assisted
	projectKey  string // short hash of projectRoot used for project notes
	sessionID   string // current session id == session file base
	client      llm.Client
}

// NewStore creates the on-disk layout and returns a Store with no session
// selected. root may start with "~"; projectRoot is made absolute. The optional
// client is used only for auto-node/folding LLM calls.
func NewStore(root, projectRoot string, client llm.Client) (*Store, error) {
	root, err := expandHome(root)
	if err != nil {
		return nil, err
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("memory root: %w", err)
	}
	absProject, err := filepath.Abs(projectRoot)
	if err != nil {
		return nil, fmt.Errorf("project root: %w", err)
	}

	s := &Store{
		root:        absRoot,
		projectRoot: absProject,
		projectKey:  projectKey(absProject),
		client:      client,
	}
	for _, dir := range []string{
		filepath.Join(absRoot, sessionsDir),
		filepath.Join(absRoot, memoryDir, userNotesDir),
		filepath.Join(absRoot, memoryDir, projectDir, s.projectKey),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create %s: %w", dir, err)
		}
	}
	return s, nil
}

// SetClient installs the LLM client used by auto-node/folding. It exists so the
// caller can construct the store before the memory client (the memory client
// needs its own system prompt, not the chat prompt).
func (s *Store) SetClient(c llm.Client) { s.client = c }

// Root returns the expanded ~/.hudson directory.
func (s *Store) Root() string { return s.root }

// ProjectRoot returns the absolute project directory.
func (s *Store) ProjectRoot() string { return s.projectRoot }

// SessionID returns the current session id.
func (s *Store) SessionID() string { return s.sessionID }

// sessionPath returns the transcript path for the given session id.
func (s *Store) sessionPath(id string) string {
	return filepath.Join(s.root, sessionsDir, id+".jsonl")
}

// CurrentSessionPath returns the transcript path for the current session.
func (s *Store) CurrentSessionPath() string {
	if s.sessionID == "" {
		return ""
	}
	return s.sessionPath(s.sessionID)
}

// OpenLatest selects the most recently modified session transcript (if any) and
// returns its parsed contents. It implements "on opening, load last session".
// If no sessions exist it creates a fresh one and returns a nil session.
func (s *Store) OpenLatest() (*Session, error) {
	pattern := filepath.Join(s.root, sessionsDir, "*.jsonl")
	paths, err := filepath.Glob(pattern)
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, s.NewSession()
	}

	var latestPath string
	var latestTime time.Time
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		if latestPath == "" || fi.ModTime().After(latestTime) {
			latestPath = p
			latestTime = fi.ModTime()
		}
	}
	id := strings.TrimSuffix(filepath.Base(latestPath), ".jsonl")
	sess, err := s.LoadSession(id)
	if err != nil {
		return nil, err
	}
	s.sessionID = id
	return sess, nil
}

// NewSession starts a fresh session with an empty transcript file and makes it
// current. The file is created eagerly so it appears in /session listing.
func (s *Store) NewSession() error {
	id := newSessionID()
	s.sessionID = id
	f, err := os.OpenFile(s.sessionPath(id), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	return f.Close()
}

// ListSessions returns session summaries sorted by session id.
func (s *Store) ListSessions() ([]SessionSummary, error) {
	pattern := filepath.Join(s.root, sessionsDir, "*.jsonl")
	paths, err := filepath.Glob(pattern)
	if err != nil {
		return nil, err
	}
	summaries := make([]SessionSummary, 0, len(paths))
	for _, p := range paths {
		sum, ok := scanSummary(p)
		if !ok {
			continue // unreadable file; skip rather than fail the list
		}
		summaries = append(summaries, sum)
	}
	sort.Slice(summaries, func(i, j int) bool { return summaries[i].ID < summaries[j].ID })
	return summaries, nil
}

// LoadSession reads and parses one transcript, preserving malformed lines for
// diagnostics rather than dropping them. It also makes id the current session so
// subsequent appends go to the selected transcript.
func (s *Store) LoadSession(id string) (*Session, error) {
	sess, err := scanSession(s.sessionPath(id))
	if err != nil {
		return nil, err
	}
	sess.ID = id
	sess.Path = s.sessionPath(id)
	s.sessionID = id
	return sess, nil
}

// AppendRecord appends one valid JSON record line to the current session. It is
// best-effort: callers in the agent loop log failures instead of failing.
func (s *Store) AppendRecord(rec Record) error {
	if s.sessionID == "" {
		if err := s.NewSession(); err != nil {
			return err
		}
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("encode record: %w", err)
	}
	f, err := os.OpenFile(s.sessionPath(s.sessionID), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open session: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("append session: %w", err)
	}
	return nil
}

// ---- agent loop recorder ----

// RecordUser appends the user's input as a user record.
func (s *Store) RecordUser(text string) {
	if err := s.AppendRecord(newRecord("user", text, nil, nil)); err != nil {
		logMemoryErr("record user", err)
	}
}

// RecordAssistant appends an assistant turn, converting the backend-agnostic
// tool-use blocks into the transcript schema.
func (s *Store) RecordAssistant(text string, uses []conversation.ToolUseBlock) {
	rec := newRecord("assistant", text, toToolUses(uses), nil)
	if err := s.AppendRecord(rec); err != nil {
		logMemoryErr("record assistant", err)
	}
}

// RecordToolResults appends a tool-result batch as a user-role record.
func (s *Store) RecordToolResults(results []conversation.ToolResultBlock) {
	rec := newRecord("user", "", nil, toToolResults(results))
	if err := s.AppendRecord(rec); err != nil {
		logMemoryErr("record tool results", err)
	}
}

// RecordsToMessages converts parsed transcript records back into the
// backend-agnostic conversation shape for loading a session into the agent.
func RecordsToMessages(recs []Record) []conversation.Message {
	out := make([]conversation.Message, 0, len(recs))
	for _, r := range recs {
		switch r.Role {
		case "assistant":
			out = append(out, conversation.Message{
				Role:     conversation.RoleAssistant,
				Content:  r.Content,
				ToolUses: fromToolUses(r.ToolUses),
			})
		case "user":
			if len(r.ToolResults) > 0 {
				out = append(out, conversation.Message{
					Role:        conversation.RoleUser,
					ToolResults: fromToolResults(r.ToolResults),
				})
			} else {
				out = append(out, conversation.Message{
					Role:    conversation.RoleUser,
					Content: r.Content,
				})
			}
		}
	}
	return out
}

// ---- helpers ----

func expandHome(p string) (string, error) {
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home: %w", err)
		}
		if p == "~" {
			return home, nil
		}
		return filepath.Join(home, p[2:]), nil
	}
	return p, nil
}

func projectKey(absProject string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(absProject)))
	return hex.EncodeToString(sum[:4]) // 8 hex chars
}

func newSessionID() string {
	return time.Now().UTC().Format("20060102T150405") + "-" + randomHex(4)
}

// sessionIDTime recovers the wall-clock creation time encoded in a session id
// ("20060102T150405-xxxx"), used by time-based folding. It falls back to now on
// parse failure.
func sessionIDTime(id string) time.Time {
	if i := strings.IndexByte(id, '-'); i > 0 {
		id = id[:i]
	}
	if t, err := time.Parse("20060102T150405", id); err == nil {
		return t
	}
	return time.Now()
}

func newRecord(role, content string, uses []ToolUse, results []ToolResult) Record {
	return Record{
		ID:          newUUID(),
		TS:          time.Now().UTC().Format(time.RFC3339Nano),
		Role:        role,
		Content:     content,
		ToolUses:    uses,
		ToolResults: results,
	}
}

func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure is catastrophic; fall back to time+random-ish data
		// so the transcript keeps a unique-enough id without aborting the loop.
		sum := sha256.Sum256([]byte(fmt.Sprintf("%d", time.Now().UnixNano())))
		copy(b[:], sum[:16])
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func toToolUses(in []conversation.ToolUseBlock) []ToolUse {
	if len(in) == 0 {
		return nil
	}
	out := make([]ToolUse, len(in))
	for i, u := range in {
		out[i] = ToolUse{ID: u.ID, Name: u.Name, Input: append(json.RawMessage(nil), u.Input...)}
	}
	return out
}

func toToolResults(in []conversation.ToolResultBlock) []ToolResult {
	if len(in) == 0 {
		return nil
	}
	out := make([]ToolResult, len(in))
	for i, r := range in {
		out[i] = ToolResult{ToolUseID: r.ToolUseID, Content: r.Content, IsError: r.IsError}
	}
	return out
}

func fromToolUses(in []ToolUse) []conversation.ToolUseBlock {
	if len(in) == 0 {
		return nil
	}
	out := make([]conversation.ToolUseBlock, len(in))
	for i, u := range in {
		out[i] = conversation.ToolUseBlock{ID: u.ID, Name: u.Name, Input: append(json.RawMessage(nil), u.Input...)}
	}
	return out
}

func fromToolResults(in []ToolResult) []conversation.ToolResultBlock {
	if len(in) == 0 {
		return nil
	}
	out := make([]conversation.ToolResultBlock, len(in))
	for i, r := range in {
		out[i] = conversation.ToolResultBlock{ToolUseID: r.ToolUseID, Content: r.Content, IsError: r.IsError}
	}
	return out
}

func logMemoryErr(what string, err error) {
	fmt.Fprintf(os.Stderr, "memory: %s: %v\n", what, err)
}
