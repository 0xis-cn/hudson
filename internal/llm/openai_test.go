package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mewcode/internal/config"
	"mewcode/internal/conversation"
)

// cannedOpenAIStream is a realistic OpenAI/DeepSeek SSE response: streamed text,
// then a tool call whose arguments arrive across several delta chunks, ending
// with finish_reason, a usage-only chunk, and [DONE].
const cannedOpenAIStream = `data: {"choices":[{"delta":{"role":"assistant","content":""},"finish_reason":null}]}

data: {"choices":[{"delta":{"content":"Let me check "},"finish_reason":null}]}

data: {"choices":[{"delta":{"content":"the weather."},"finish_reason":null}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":""}}]},"finish_reason":null}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"location\":"}}]},"finish_reason":null}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":" \"SF\"}"}}]},"finish_reason":null}]}

data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}

data: {"choices":[],"usage":{"prompt_tokens":50,"completion_tokens":12}}

data: [DONE]

`

// C9: the OpenAI SSE parser assembles streamed text and a tool call whose JSON
// arguments are reconstructed from delta fragments, and the request carries the
// Bearer header, stream:true, the system message, and translated tools.
func TestOpenAIStreamParsesTextAndToolCall(t *testing.T) {
	var gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("content-type", "text/event-stream")
		_, _ = w.Write([]byte(cannedOpenAIStream))
	}))
	defer srv.Close()

	cfg := &config.Config{
		Protocol: config.ProtocolOpenAI, APIKey: "sk-deepseek",
		BaseURL: srv.URL, Model: "deepseek-chat", MaxTokens: 1024,
	}
	client, err := NewClient(cfg, "you are a test")
	if err != nil {
		t.Fatal(err)
	}

	conv := conversation.NewManager()
	conv.AddUserText("weather in SF?")
	tools := []map[string]any{{"name": "get_weather", "description": "wx", "input_schema": map[string]any{"type": "object"}}}
	evs, drainErr := drain(client.Stream(context.Background(), conv, tools))
	if drainErr != nil {
		t.Fatalf("stream error: %v", drainErr)
	}

	if gotAuth != "Bearer sk-deepseek" {
		t.Errorf("authorization header = %q", gotAuth)
	}
	if !strings.Contains(gotBody, `"stream":true`) {
		t.Errorf("body missing stream:true: %s", gotBody)
	}
	if !strings.Contains(gotBody, `"role":"system","content":"you are a test"`) {
		t.Errorf("body missing system message: %s", gotBody)
	}
	if !strings.Contains(gotBody, `"type":"function"`) || !strings.Contains(gotBody, `"name":"get_weather"`) {
		t.Errorf("body missing translated tool: %s", gotBody)
	}

	var text strings.Builder
	var complete *ToolCallComplete
	var end *StreamEnd
	for _, ev := range evs {
		switch e := ev.(type) {
		case TextDelta:
			text.WriteString(e.Text)
		case ToolCallComplete:
			c := e
			complete = &c
		case StreamEnd:
			s := e
			end = &s
		}
	}
	if text.String() != "Let me check the weather." {
		t.Errorf("assembled text = %q", text.String())
	}
	if complete == nil || complete.Name != "get_weather" || complete.ID != "call_1" {
		t.Fatalf("tool call = %+v", complete)
	}
	if string(complete.Input) != `{"location": "SF"}` {
		t.Errorf("assembled tool args = %s", complete.Input)
	}
	if end == nil || end.StopReason != "tool_use" {
		t.Errorf("StreamEnd = %+v", end)
	}
	if end.Usage.InputTokens != 50 || end.Usage.OutputTokens != 12 {
		t.Errorf("usage = %+v", end.Usage)
	}
}

// C9: history translation — tool results become role:"tool" messages keyed by
// tool_call_id; the assistant turn carries tool_calls.
func TestBuildOpenAIMessagesTranslatesToolFlow(t *testing.T) {
	conv := conversation.NewManager()
	conv.AddUserText("read it")
	conv.AddAssistant("sure", []conversation.ToolUseBlock{
		{ID: "call_1", Name: "read_file", Input: json.RawMessage(`{"path":"a"}`)},
	})
	conv.AddToolResults([]conversation.ToolResultBlock{
		{ToolUseID: "call_1", Content: "file body", IsError: false},
	})

	msgs := buildOpenAIMessages("sys", conv.GetMessages())
	// system, user, assistant(+tool_calls), tool
	if len(msgs) != 4 {
		t.Fatalf("got %d messages, want 4: %+v", len(msgs), msgs)
	}
	if msgs[0].Role != "system" {
		t.Errorf("msg0 = %+v", msgs[0])
	}
	if msgs[2].Role != "assistant" || len(msgs[2].ToolCalls) != 1 || msgs[2].ToolCalls[0].Function.Name != "read_file" {
		t.Errorf("assistant tool_calls wrong: %+v", msgs[2])
	}
	if msgs[3].Role != "tool" || msgs[3].ToolCallID != "call_1" || msgs[3].Content != "file body" {
		t.Errorf("tool result message wrong: %+v", msgs[3])
	}
}

// C9: a tool result whose tool call was trimmed away must not become an
// orphaned role:"tool" message, which OpenAI rejects with a 400.
func TestBuildOpenAIMessagesSkipsOrphanedToolResults(t *testing.T) {
	conv := conversation.NewManager()
	// No assistant tool-call message precedes these results.
	conv.AddToolResults([]conversation.ToolResultBlock{
		{ToolUseID: "call_orphan", Content: "stale output", IsError: false},
	})
	conv.AddAssistant("final answer", nil)

	msgs := buildOpenAIMessages("sys", conv.GetMessages())
	// system, user(tool results, skipped), assistant
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2: %+v", len(msgs), msgs)
	}
	for _, m := range msgs {
		if m.Role == "tool" {
			t.Errorf("orphaned tool message should be skipped: %+v", m)
		}
	}
	if msgs[1].Role != "assistant" || msgs[1].Content != "final answer" {
		t.Errorf("assistant message wrong: %+v", msgs[1])
	}
}
