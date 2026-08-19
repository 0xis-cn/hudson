package main

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"mewcode/internal/conversation"
	"mewcode/internal/llm"
)

// demoClient is the offline backend behind --fake. It is NOT the strict
// llm.FakeClient used in tests (which replays a fixed script); instead it reacts
// to each user message so the demo survives a whole conversation. The "model"
// text is canned and streamed word-by-word, but the tools it calls run for real
// — read_file really reads, bash really runs — so the demo shows the genuine
// streaming UI, tool cards, permission prompts and mode switching.
type demoClient struct{}

func (demoClient) Stream(ctx context.Context, conv *conversation.Manager, _ []map[string]any) (<-chan llm.StreamEvent, <-chan error) {
	out := make(chan llm.StreamEvent, 16)
	errc := make(chan error, 1)

	msgs := conv.GetMessages()
	last := msgs[len(msgs)-1]

	go func() {
		defer close(out)

		// Second step of a turn: we are replying to tool results.
		if len(last.ToolResults) > 0 {
			streamWords(ctx, out, "There you go — the result is in the card above. Ask me something else, or type /quit.")
			emit(ctx, out, llm.StreamEnd{StopReason: "end_turn"})
			return
		}

		// First step: react to the user's text and maybe call a tool.
		text, name, input := planDemoReply(last.Content)
		streamWords(ctx, out, text)
		if name == "" {
			emit(ctx, out, llm.StreamEnd{StopReason: "end_turn"})
			return
		}
		emit(ctx, out, llm.ToolCallComplete{ID: "demo-1", Name: name, Input: input})
		emit(ctx, out, llm.StreamEnd{StopReason: "tool_use"})
	}()

	return out, errc
}

// planDemoReply picks a canned reply and a real tool call based on keywords in
// the user's message.
func planDemoReply(user string) (text, toolName string, input json.RawMessage) {
	low := strings.ToLower(user)
	file, hasFile := fileToken(user)
	switch {
	case strings.Contains(low, "write") || strings.Contains(low, "create"):
		return "Sure — I'll create a small file for you.", "write_file",
			json.RawMessage(`{"path":"demo_out.txt","content":"hello from the MewCode demo\n"}`)
	case strings.Contains(low, "directory") || strings.Contains(low, "pwd") || strings.Contains(low, "cwd") ||
		strings.Contains(low, "where am") || strings.Contains(low, "目录") || strings.Contains(low, "哪"):
		return "Let me check the current working directory.", "bash",
			json.RawMessage(`{"command":"Get-Location"}`)
	case strings.Contains(low, "run") || strings.Contains(low, "version") || strings.Contains(low, "bash") || strings.Contains(low, "command"):
		return "Let me run that command.", "bash",
			json.RawMessage(`{"command":"go version"}`)
	case strings.Contains(low, "list") || strings.Contains(low, "glob") || strings.Contains(low, "*.go") || strings.Contains(low, "files"):
		return "Listing the Go source files.", "glob",
			json.RawMessage(`{"pattern":"internal/*/*.go"}`)
	case strings.Contains(low, "search") || strings.Contains(low, "grep") || strings.Contains(low, "find"):
		return "Searching the code.", "grep",
			json.RawMessage(`{"pattern":"func ","path":"internal/agent"}`)
	case hasFile || strings.Contains(low, "read") || strings.Contains(low, "open") || strings.Contains(low, "cat"):
		path := file
		if path == "" {
			path = "main.go"
		}
		return "Let me read that file for you.", "read_file",
			json.RawMessage(`{"path":` + jsonString(path) + `}`)
	default:
		return "Hi! I'm MewCode running offline (canned replies, real tools). Try: " +
			"\"read spec.md\", \"list files\", \"run go version\", \"which directory am I in\", or \"create a file\".", "", nil
	}
}

// fileToken returns the first filename-looking token in the user text.
func fileToken(user string) (string, bool) {
	for _, tok := range strings.Fields(user) {
		t := strings.Trim(tok, "\"'`.,")
		if strings.HasSuffix(t, ".go") || strings.HasSuffix(t, ".txt") || strings.HasSuffix(t, ".md") {
			return t, true
		}
	}
	return "", false
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// streamWords emits text word-by-word with a small delay to make the streaming
// visible in the demo.
func streamWords(ctx context.Context, out chan<- llm.StreamEvent, text string) {
	for i, w := range strings.Fields(text) {
		chunk := w
		if i > 0 {
			chunk = " " + w
		}
		if !emit(ctx, out, llm.TextDelta{Text: chunk}) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(35 * time.Millisecond):
		}
	}
}

// emit sends one event unless the context is cancelled; returns false on cancel.
func emit(ctx context.Context, out chan<- llm.StreamEvent, ev llm.StreamEvent) bool {
	select {
	case <-ctx.Done():
		return false
	case out <- ev:
		return true
	}
}
