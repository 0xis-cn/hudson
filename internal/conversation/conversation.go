package conversation

import "encoding/json"

// Role identifies who produced a message. Tool results are carried on a
// user-role message, mirroring how the Anthropic and OpenAI wire formats expect
// them.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// ToolUseBlock is a model request to invoke a tool. ID is assigned by the
// provider and must be echoed back on the matching result so the model can pair
// them.
type ToolUseBlock struct {
	ID    string          // provider-assigned call id
	Name  string          // tool name to dispatch
	Input json.RawMessage // raw JSON arguments, parsed by the tool
}

// ToolResultBlock is the outcome of a tool call, fed back to the model on the
// next turn. IsError distinguishes a failed or denied call from a successful
// one so the model can self-correct rather than assume success.
type ToolResultBlock struct {
	ToolUseID string // matches ToolUseBlock.ID
	Content   string // textual result, or the error/denial message
	IsError   bool   // true when the tool failed or was refused
}

// Message is one conversational turn in backend-agnostic form. A single message
// may carry assistant text together with one or more tool-use requests, or a
// batch of tool results. Each provider translates these into its own wire shape.
type Message struct {
	Role        Role
	Content     string            // text content ("" when the turn is only tool blocks)
	ToolUses    []ToolUseBlock    // assistant-side tool-call requests
	ToolResults []ToolResultBlock // user-side tool results
}

// Manager owns the in-memory conversation history. It is the only writer of the
// history slice; callers mutate it through the Add* methods and read an
// isolated copy via GetMessages. It is intentionally unlocked: the agent loop
// is a single goroutine that appends serially (see spec non-functional N5).
type Manager struct {
	history []Message
}

// NewManager returns an empty conversation.
func NewManager() *Manager {
	return &Manager{}
}

// AddUserText appends a plain user message.
func (m *Manager) AddUserText(text string) {
	m.history = append(m.history, Message{Role: RoleUser, Content: text})
}

// AddAssistant appends an assistant turn carrying optional text and any tool
// calls the model requested.
func (m *Manager) AddAssistant(text string, toolUses []ToolUseBlock) {
	m.history = append(m.history, Message{
		Role:     RoleAssistant,
		Content:  text,
		ToolUses: cloneToolUses(toolUses),
	})
}

// AddToolResults appends the results of a batch of tool calls as a user message,
// which is how both supported wire formats expect tool output to be returned.
func (m *Manager) AddToolResults(results []ToolResultBlock) {
	m.history = append(m.history, Message{
		Role:        RoleUser,
		ToolResults: cloneToolResults(results),
	})
}

// GetMessages returns a deep copy of the history so callers cannot mutate the
// manager's internal state through the returned slices.
func (m *Manager) GetMessages() []Message {
	out := make([]Message, len(m.history))
	for i, msg := range m.history {
		out[i] = Message{
			Role:        msg.Role,
			Content:     msg.Content,
			ToolUses:    cloneToolUses(msg.ToolUses),
			ToolResults: cloneToolResults(msg.ToolResults),
		}
	}
	return out
}

// Len reports the number of messages in the history.
func (m *Manager) Len() int {
	return len(m.history)
}

// ReplaceAll replaces the entire in-memory history with a deep copy of msgs.
// It is used when loading a persisted session so callers cannot retain a
// reference into the manager's internal slice.
func (m *Manager) ReplaceAll(msgs []Message) {
	out := make([]Message, len(msgs))
	for i, msg := range msgs {
		out[i] = Message{
			Role:        msg.Role,
			Content:     msg.Content,
			ToolUses:    cloneToolUses(msg.ToolUses),
			ToolResults: cloneToolResults(msg.ToolResults),
		}
	}
	m.history = out
}

// TrimToLast keeps only the last n messages, dropping the older prefix. It is
// used by memory folding to truncate the in-memory tail while the summarised
// prefix is persisted as a project note. A non-positive n clears the history.
//
// The cut is snapped back to a clean turn boundary (a user message carrying no
// tool results). Trimming inside a turn would otherwise orphan tool results:
// OpenAI requires a role:"tool" message to directly follow the assistant turn
// that issued the matching tool_calls, and neither wire format accepts a
// retained window that starts with a tool-result user message.
func (m *Manager) TrimToLast(n int) {
	if n <= 0 {
		m.history = nil
		return
	}
	if n >= len(m.history) {
		return
	}
	start := len(m.history) - n
	for start > 0 && !isTurnStart(m.history[start]) {
		start--
	}
	keep := make([]Message, len(m.history)-start)
	copy(keep, m.history[start:])
	m.history = keep
}

// isTurnStart reports whether a message is a safe place to begin a retained
// history window: a plain user turn with no tool results.
func isTurnStart(m Message) bool {
	return m.Role == RoleUser && len(m.ToolResults) == 0
}

func cloneToolUses(in []ToolUseBlock) []ToolUseBlock {
	if len(in) == 0 {
		return nil
	}
	out := make([]ToolUseBlock, len(in))
	for i, b := range in {
		out[i] = ToolUseBlock{ID: b.ID, Name: b.Name, Input: cloneRaw(b.Input)}
	}
	return out
}

func cloneToolResults(in []ToolResultBlock) []ToolResultBlock {
	if len(in) == 0 {
		return nil
	}
	out := make([]ToolResultBlock, len(in))
	copy(out, in)
	return out
}

func cloneRaw(in json.RawMessage) json.RawMessage {
	if in == nil {
		return nil
	}
	out := make(json.RawMessage, len(in))
	copy(out, in)
	return out
}
