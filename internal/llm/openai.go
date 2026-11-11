package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"mewcode/internal/config"
	"mewcode/internal/conversation"
)

// deepSeekDefaultBase is used when protocol=openai and no --base-url is given,
// since DeepSeek is the documented target for this backend.
const deepSeekDefaultBase = "https://api.deepseek.com"

// openAIClient speaks the OpenAI-compatible chat/completions API (DeepSeek and
// other OpenAI-format servers). It hand-parses the OpenAI streaming SSE and
// translates it into the same backend-agnostic StreamEvent stream as Anthropic.
type openAIClient struct {
	apiKey      string
	endpoint    string
	model       string
	maxTokens   int
	system      string
	baseBackoff time.Duration
	http        *http.Client
}

var _ Client = (*openAIClient)(nil)

func newOpenAIClient(cfg *config.Config, systemPrompt string) *openAIClient {
	base := strings.TrimRight(cfg.BaseURL, "/")
	if base == "" {
		base = deepSeekDefaultBase
	}
	return &openAIClient{
		apiKey:      cfg.APIKey,
		endpoint:    base + "/chat/completions",
		model:       cfg.Model,
		maxTokens:   cfg.MaxTokens,
		system:      systemPrompt,
		baseBackoff: baseBackoffDefault,
		http:        &http.Client{},
	}
}

type oaiRequest struct {
	Model      string        `json:"model"`
	Messages   []oaiMessage  `json:"messages"`
	Tools      []oaiTool     `json:"tools,omitempty"`
	Stream     bool          `json:"stream"`
	MaxTokens  int           `json:"max_tokens,omitempty"`
	StreamOpts *oaiStreamOpt `json:"stream_options,omitempty"`
}

type oaiStreamOpt struct {
	IncludeUsage bool `json:"include_usage"`
}

type oaiMessage struct {
	Role       string        `json:"role"`
	Content    string        `json:"content"`
	ToolCalls  []oaiToolCall `json:"tool_calls,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"`
}

type oaiToolCall struct {
	ID       string  `json:"id"`
	Type     string  `json:"type"`
	Function oaiFunc `json:"function"`
}

type oaiFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type oaiTool struct {
	Type     string     `json:"type"`
	Function oaiToolDef `json:"function"`
}

type oaiToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

func (c *openAIClient) Stream(ctx context.Context, conv *conversation.Manager, toolSchemas []map[string]any) (<-chan StreamEvent, <-chan error) {
	out := make(chan StreamEvent, 32)
	errc := make(chan error, 1)

	reqBody := oaiRequest{
		Model:      c.model,
		Messages:   buildOpenAIMessages(c.system, conv.GetMessages()),
		Tools:      toOpenAITools(toolSchemas),
		Stream:     true,
		MaxTokens:  c.maxTokens,
		StreamOpts: &oaiStreamOpt{IncludeUsage: true},
	}

	go func() {
		defer close(out)
		body, err := json.Marshal(reqBody)
		if err != nil {
			errc <- &LLMError{Message: "encode request: " + err.Error()}
			return
		}
		resp, err := c.send(ctx, body)
		if err != nil {
			errc <- err
			return
		}
		defer resp.Body.Close()
		parseOpenAISSE(resp.Body, out, errc)
	}()

	return out, errc
}

// send mirrors the Anthropic retry policy but with a Bearer Authorization
// header (OpenAI/DeepSeek auth).
func (c *openAIClient) send(ctx context.Context, body []byte) (*http.Response, error) {
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, &LLMError{Message: "build request: " + err.Error()}
		}
		req.Header.Set("content-type", "application/json")
		req.Header.Set("authorization", "Bearer "+c.apiKey)

		resp, err := c.http.Do(req)
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return nil, &NetworkError{Message: "request cancelled: " + ctx.Err().Error()}
			}
			lastErr = &NetworkError{Message: "request failed: " + err.Error()}
		case resp.StatusCode/100 == 2:
			return resp, nil
		case isRetryableStatus(resp.StatusCode):
			lastErr = classifyHTTPError(resp)
			resp.Body.Close()
		default:
			e := classifyHTTPError(resp)
			resp.Body.Close()
			return nil, e
		}
		if attempt == maxAttempts {
			break
		}
		select {
		case <-ctx.Done():
			return nil, &NetworkError{Message: "request cancelled: " + ctx.Err().Error()}
		case <-time.After(backoffDelay(attempt, c.baseBackoff, resp)):
		}
	}
	return nil, lastErr
}

// toOpenAITools remaps the registry's tool definitions ({name, description,
// input_schema}) into the OpenAI shape ({type:function, function:{...}}).
func toOpenAITools(schemas []map[string]any) []oaiTool {
	out := make([]oaiTool, 0, len(schemas))
	for _, s := range schemas {
		name, _ := s["name"].(string)
		desc, _ := s["description"].(string)
		params, _ := s["input_schema"].(map[string]any)
		out = append(out, oaiTool{
			Type:     "function",
			Function: oaiToolDef{Name: name, Description: desc, Parameters: params},
		})
	}
	return out
}

// buildOpenAIMessages translates the history into OpenAI's format: the system
// prompt is the first message; tool results become role:"tool" messages keyed
// by tool_call_id; OpenAI has no is_error flag so failures are marked inline.
func buildOpenAIMessages(system string, msgs []conversation.Message) []oaiMessage {
	out := make([]oaiMessage, 0, len(msgs)+1)
	if system != "" {
		out = append(out, oaiMessage{Role: "system", Content: system})
	}
	seen := map[string]bool{}
	for _, m := range msgs {
		switch {
		case m.Role == conversation.RoleUser && len(m.ToolResults) > 0:
			for _, r := range m.ToolResults {
				// Skip orphaned results whose tool call was trimmed away:
				// OpenAI rejects a role:"tool" message with no preceding
				// assistant tool_calls carrying the same id.
				if !seen[r.ToolUseID] {
					continue
				}
				content := r.Content
				if r.IsError {
					content = "ERROR: " + content
				}
				out = append(out, oaiMessage{Role: "tool", ToolCallID: r.ToolUseID, Content: content})
			}
		case m.Role == conversation.RoleAssistant:
			msg := oaiMessage{Role: "assistant", Content: m.Content}
			for _, tu := range m.ToolUses {
				seen[tu.ID] = true
				args := string(tu.Input)
				if args == "" {
					args = "{}"
				}
				msg.ToolCalls = append(msg.ToolCalls, oaiToolCall{
					ID: tu.ID, Type: "function",
					Function: oaiFunc{Name: tu.Name, Arguments: args},
				})
			}
			out = append(out, msg)
		default:
			out = append(out, oaiMessage{Role: "user", Content: m.Content})
		}
	}
	return out
}

type oaiChunk struct {
	Choices []struct {
		Delta struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

type oaiCallAcc struct {
	id, name string
	args     strings.Builder
	started  bool
}

// parseOpenAISSE reads the OpenAI-style event stream. tool_calls arrive across
// delta chunks keyed by index; we accumulate their argument fragments and emit
// one ToolCallComplete per call when the stream ends ([DONE] or finish_reason).
func parseOpenAISSE(r io.Reader, out chan<- StreamEvent, errc chan<- error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	calls := map[int]*oaiCallAcc{}
	var order []int
	var inputTokens, outputTokens int
	var finish string

	flush := func() {
		sort.Ints(order)
		for _, idx := range order {
			a := calls[idx]
			args := a.args.String()
			if args == "" {
				args = "{}"
			}
			out <- ToolCallComplete{ID: a.id, Name: a.name, Input: json.RawMessage(args)}
		}
		out <- StreamEnd{StopReason: mapFinishReason(finish), Usage: UsageInfo{InputTokens: inputTokens, OutputTokens: outputTokens}}
	}

	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(line[len("data:"):])
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			flush()
			return
		}
		var ch oaiChunk
		if json.Unmarshal([]byte(data), &ch) != nil {
			continue
		}
		if ch.Usage != nil {
			inputTokens = ch.Usage.PromptTokens
			outputTokens = ch.Usage.CompletionTokens
		}
		if len(ch.Choices) == 0 {
			continue
		}
		choice := ch.Choices[0]
		if choice.Delta.Content != "" {
			out <- TextDelta{Text: choice.Delta.Content}
		}
		for _, tc := range choice.Delta.ToolCalls {
			a := calls[tc.Index]
			if a == nil {
				a = &oaiCallAcc{}
				calls[tc.Index] = a
				order = append(order, tc.Index)
			}
			if tc.ID != "" {
				a.id = tc.ID
			}
			if tc.Function.Name != "" {
				a.name = tc.Function.Name
			}
			if a.id != "" && !a.started {
				out <- ToolCallStart{ID: a.id, Name: a.name}
				a.started = true
			}
			if tc.Function.Arguments != "" {
				a.args.WriteString(tc.Function.Arguments)
				out <- ToolCallDelta{ID: a.id, PartialJSON: tc.Function.Arguments}
			}
		}
		if choice.FinishReason != "" {
			finish = choice.FinishReason
		}
	}
	if err := sc.Err(); err != nil {
		errc <- &NetworkError{Message: "stream read error: " + err.Error()}
		return
	}
	flush() // stream closed without an explicit [DONE]
}

func mapFinishReason(fr string) string {
	switch fr {
	case "tool_calls":
		return "tool_use"
	case "length":
		return "max_tokens"
	case "stop":
		return "end_turn"
	default:
		return fr
	}
}

// backoffDelay computes the retry wait (honouring a 429 Retry-After header,
// otherwise exponential). Shared by the OpenAI client; Anthropic has its own.
func backoffDelay(attempt int, base time.Duration, resp *http.Response) time.Duration {
	if resp != nil && resp.StatusCode == http.StatusTooManyRequests {
		if ra := parseRetryAfter(resp.Header.Get("retry-after")); ra > 0 {
			if ra > maxBackoff {
				return maxBackoff
			}
			return ra
		}
	}
	w := base << (attempt - 1)
	if w > maxBackoff {
		return maxBackoff
	}
	return w
}
