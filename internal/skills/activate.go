package skills

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"mewcode/internal/tools"
)

// activateTool is the dedicated skill-activation tool. It loads a skill's full
// body from disk (fresh on every call) and returns it inline so the model gets
// the instructions without a file-read round trip.
type activateTool struct {
	mgr *Manager
}

// NewActivateTool returns a tools.Tool that activates a skill by name. The body
// is reread from disk on every invocation.
func NewActivateTool(mgr *Manager) tools.Tool {
	return &activateTool{mgr: mgr}
}

func (t *activateTool) Name() string        { return "activate_skill" }
func (t *activateTool) SideEffecting() bool { return false }

func (t *activateTool) Description() string {
	return "Load a skill's full instructions into context. Pass the skill name from the available-skills catalog; optionally pass an argument to the skill."
}

func (t *activateTool) Schema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name":     map[string]any{"type": "string", "description": "Skill name to load, exactly as listed in the available-skills catalog."},
			"argument": map[string]any{"type": "string", "description": "Optional argument/context to pass to the skill."},
		},
		"required": []any{"name"},
	}
}

func (t *activateTool) Execute(_ context.Context, input json.RawMessage) (string, bool) {
	var args struct {
		Name     string `json:"name"`
		Argument string `json:"argument"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return fmt.Sprintf("invalid arguments: %v", err), true
	}
	name := strings.TrimSpace(args.Name)
	if name == "" {
		return "skill name is required", true
	}

	body, err := t.mgr.Load(name)
	if err != nil {
		return err.Error(), true
	}
	if args.Argument != "" {
		body = applyArgument(body, args.Argument)
	}
	return wrapSkill(name, body), false
}

// applyArgument injects the user-provided argument into the skill body. If the
// body declares a known variable placeholder it is substituted; otherwise the
// argument is carried as an appended section so it is never lost.
func applyArgument(body, arg string) string {
	placeholders := []string{"{{argument}}", "{{Argument}}", "{{ARGUMENT}}", "{{input}}", "{{Input}}", "{{INPUT}}"}
	substituted := false
	for _, token := range placeholders {
		if strings.Contains(body, token) {
			body = strings.ReplaceAll(body, token, arg)
			substituted = true
		}
	}
	if !substituted {
		body += "\n\n## User argument\n" + arg + "\n"
	}
	return body
}

func wrapSkill(name, body string) string {
	return fmt.Sprintf("<skill_content name=%q>\n%s\n</skill_content>", name, body)
}
