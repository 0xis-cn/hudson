package tools

import (
	"context"
	"encoding/json"
	"fmt"
)

// Registry holds the available tools, exposes their definitions to a provider,
// and dispatches a call by name. Insertion order is preserved so the definition
// list is stable across runs.
type Registry struct {
	order  []string
	byName map[string]Tool
}

// NewRegistry builds a registry from the given tools. Duplicate names are
// ignored (first wins); names are expected to be unique.
func NewRegistry(tools ...Tool) *Registry {
	r := &Registry{byName: make(map[string]Tool, len(tools))}
	for _, t := range tools {
		if _, dup := r.byName[t.Name()]; dup {
			continue
		}
		r.byName[t.Name()] = t
		r.order = append(r.order, t.Name())
	}
	return r
}

// DefaultRegistry returns a registry with the six built-in tools, followed by
// any extra tools (e.g. the skill-activation tool) supplied by the caller.
func DefaultRegistry(extra ...Tool) *Registry {
	base := []Tool{
		NewReadTool(),
		NewWriteTool(),
		NewEditTool(),
		NewGlobTool(),
		NewGrepTool(),
		NewBashTool(DefaultBashTimeout),
	}
	return NewRegistry(append(base, extra...)...)
}

// Definitions returns the tool list a provider sends to the model: name,
// description and input schema for each tool, in registration order.
func (r *Registry) Definitions() []map[string]any {
	defs := make([]map[string]any, 0, len(r.order))
	for _, name := range r.order {
		t := r.byName[name]
		defs = append(defs, map[string]any{
			"name":         t.Name(),
			"description":  t.Description(),
			"input_schema": t.Schema(),
		})
	}
	return defs
}

// Dispatch runs the named tool with the given input. An unknown name returns an
// error result rather than failing the whole turn, so the model can recover.
func (r *Registry) Dispatch(ctx context.Context, name string, input json.RawMessage) (content string, isError bool) {
	t, ok := r.byName[name]
	if !ok {
		return fmt.Sprintf("unknown tool %q", name), true
	}
	return t.Execute(ctx, input)
}

// Get returns the named tool and whether it exists, so callers (the agent loop)
// can inspect a tool — e.g. ask SideEffecting() — before dispatching.
func (r *Registry) Get(name string) (Tool, bool) {
	t, ok := r.byName[name]
	return t, ok
}

// Len reports how many tools are registered.
func (r *Registry) Len() int { return len(r.order) }
