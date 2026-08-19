package memory

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"mewcode/internal/conversation"
	"mewcode/internal/llm"
)

// MemorySystemPrompt is the system prompt for the separate memory client used
// by auto-node classification and folding. It is intentionally distinct from the
// chat system prompt.
const MemorySystemPrompt = `You are the memory subsystem of a coding assistant. You extract durable facts from conversations and summarise transcripts. Respond with only the requested JSON or plain text, never markdown fences, never extra commentary.`

// autoNodeResponse is the expected JSON from the auto-node classifier. Type is
// one of preference | correction | norm | reference | none.
type autoNodeResponse struct {
	Type    string `json:"type"`
	Title   string `json:"title"`
	Content string `json:"content"`
	Scope   string `json:"scope"` // "user" or "project"
}

// EndTurn is called by the agent loop after a successful turn. It runs the
// LLM-backed memory work asynchronously and, if the history crossed a fold
// threshold, truncates the in-memory tail immediately (the summarised prefix is
// persisted asynchronously from the snapshot taken here).
func (s *Store) EndTurn(ctx context.Context, conv *conversation.Manager) {
	msgs := conv.GetMessages()

	if s.client != nil {
		go s.updateAutoNode(ctx, msgs)
	}

	if s.shouldFold(msgs) || (time.Since(sessionIDTime(s.sessionID)) > foldTimeLimit && len(msgs) > foldKeep) {
		// msgs is already a deep copy, so the async summary can read it safely
		// after we truncate the in-memory conversation below.
		go s.fold(ctx, msgs)
		conv.TrimToLast(foldKeep)
	}
}

// shouldFold reports whether the in-memory history crossed either fold limit.
func (s *Store) shouldFold(msgs []conversation.Message) bool {
	return len(msgs) > foldMessageLimit || estimateMessageTokens(msgs) > foldTokenLimit
}

// complete runs one non-streaming-ish LLM completion using the memory client's
// own system prompt. It accumulates TextDelta and returns the joined text.
func (s *Store) complete(ctx context.Context, user string) (string, error) {
	conv := conversation.NewManager()
	conv.AddUserText(user)
	out, errc := s.client.Stream(ctx, conv, nil)
	var b strings.Builder
	for ev := range out {
		if td, ok := ev.(llm.TextDelta); ok {
			b.WriteString(td.Text)
		}
	}
	select {
	case err := <-errc:
		if err != nil {
			return "", err
		}
	default:
	}
	return strings.TrimSpace(b.String()), nil
}

// updateAutoNode classifies the latest exchange and persists a note when the
// classifier reports a durable type. It is best-effort and async.
func (s *Store) updateAutoNode(ctx context.Context, msgs []conversation.Message) {
	transcript := transcriptOf(msgs)
	prompt := `Classify whether the following latest conversation exchange contains a durable memory note. ` +
		`Types: "preference" (the user stated a preference), "correction" (the user corrected the assistant), ` +
		`"norm" (a repeatable working convention or rule), "reference" (a fact or context worth remembering), ` +
		`or "none" (nothing durable). Choose scope "user" for user-wide facts or "project" for project-specific facts. ` +
		`Return exactly one JSON object: {"type":"...","title":"...","content":"...","scope":"user|project"}. ` +
		`If none, return {"type":"none"}. ` +
		"Transcript:\n" + transcript

	raw, err := s.complete(ctx, prompt)
	if err != nil {
		logMemoryErr("auto-node classify", err)
		return
	}
	var res autoNodeResponse
	if err := json.Unmarshal([]byte(raw), &res); err != nil {
		logMemoryErr("auto-node parse", err)
		return
	}
	switch res.Type {
	case noteTypePreference, noteTypeCorrection, noteTypeNorm, noteTypeReference:
		scope := res.Scope
		if scope != "user" && scope != "project" {
			scope = "project"
		}
		if err := s.saveNote(scope, res.Type, res.Title, res.Content); err != nil {
			logMemoryErr("auto-node save", err)
		}
	}
}

// fold summarises the older prefix into a project reference note. The in-memory
// truncation has already happened by the time this runs; the snapshot carries
// the dropped prefix.
func (s *Store) fold(ctx context.Context, msgs []conversation.Message) {
	if len(msgs) <= foldKeep {
		return
	}
	prefix := msgs[:len(msgs)-foldKeep]
	prompt := `Summarise the following older conversation turns into a concise project memory note. ` +
		`Keep durable decisions, preferences, corrections and context; drop transient tool output. ` +
		"Return plain text only. Turns:\n" + transcriptOf(prefix)

	summary, err := s.complete(ctx, prompt)
	if err != nil {
		logMemoryErr("fold summarise", err)
		return
	}
	title := "Session fold summary"
	if err := s.saveNote("project", noteTypeReference, title, summary); err != nil {
		logMemoryErr("fold save", err)
	}
}

// transcriptOf renders a compact, lossy transcript for the memory model. It is
// not the full session record; it includes roles and the assistant/user text and
// a count of tool calls/results rather than their full bodies.
func transcriptOf(msgs []conversation.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		switch {
		case m.Role == conversation.RoleUser && len(m.ToolResults) > 0:
			b.WriteString("[tool results: ")
			for i, r := range m.ToolResults {
				if i > 0 {
					b.WriteString(", ")
				}
				if r.IsError {
					b.WriteString("ERROR ")
				}
				b.WriteString(r.Content)
			}
			b.WriteString("]\n")
		case m.Role == conversation.RoleAssistant:
			if m.Content != "" {
				b.WriteString("assistant: ")
				b.WriteString(m.Content)
				b.WriteString("\n")
			}
			for _, u := range m.ToolUses {
				b.WriteString("[tool call: ")
				b.WriteString(u.Name)
				b.WriteString(" ")
				b.WriteString(string(u.Input))
				b.WriteString("]\n")
			}
		default:
			b.WriteString("user: ")
			b.WriteString(m.Content)
			b.WriteString("\n")
		}
	}
	return b.String()
}
