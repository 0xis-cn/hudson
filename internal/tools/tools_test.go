package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// args marshals a map into the json.RawMessage the tools expect.
func args(t *testing.T, m map[string]any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return b
}

// C5: the registry lists exactly the six built-in tools.
func TestDefaultRegistryHasSixNamedTools(t *testing.T) {
	r := DefaultRegistry()
	if r.Len() != 6 {
		t.Fatalf("registry Len = %d, want 6", r.Len())
	}
	defs := r.Definitions()
	if len(defs) != 6 {
		t.Fatalf("Definitions len = %d, want 6", len(defs))
	}
	got := map[string]bool{}
	for _, d := range defs {
		name, _ := d["name"].(string)
		got[name] = true
		if _, ok := d["input_schema"]; !ok {
			t.Errorf("definition %q missing input_schema", name)
		}
	}
	for _, want := range []string{"read", "write", "edit", "bash", "glob", "grep"} {
		found := false
		for name := range got {
			if strings.Contains(name, want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("no tool name contains %q; got %v", want, got)
		}
	}
}

// C5: dispatching an unknown name returns an error result, not a panic.
func TestDispatchUnknownTool(t *testing.T) {
	r := DefaultRegistry()
	out, isErr := r.Dispatch(context.Background(), "nope", json.RawMessage(`{}`))
	if !isErr {
		t.Errorf("unknown tool should be an error result, got %q", out)
	}
}

// C5: reading a missing path is an error result, not a panic.
func TestReadMissingPath(t *testing.T) {
	out, isErr := NewReadTool().Execute(context.Background(),
		args(t, map[string]any{"path": filepath.Join(t.TempDir(), "ghost.txt")}))
	if !isErr {
		t.Errorf("missing path should be an error, got %q", out)
	}
}

// C5: writing creates missing parent directories.
func TestWriteCreatesParentDirs(t *testing.T) {
	target := filepath.Join(t.TempDir(), "a", "b", "c", "hello.txt")
	out, isErr := NewWriteTool().Execute(context.Background(),
		args(t, map[string]any{"path": target, "content": "hi there"}))
	if isErr {
		t.Fatalf("write failed: %s", out)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("file not written: %v", err)
	}
	if string(data) != "hi there" {
		t.Errorf("content = %q, want %q", data, "hi there")
	}
}

// C5: read honours offset/limit and caps oversized output instead of dumping
// the whole file into the model's context.
func TestReadRangeAndCaps(t *testing.T) {
	dir := t.TempDir()
	read := NewReadTool()

	// 5000-line file: default read returns at most readDefaultLimit lines with a
	// "more lines remain" continuation hint.
	big := filepath.Join(dir, "big.txt")
	var sb strings.Builder
	for i := 1; i <= 5000; i++ {
		fmt.Fprintf(&sb, "line %d\n", i)
	}
	if err := os.WriteFile(big, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	out, isErr := read.Execute(context.Background(), args(t, map[string]any{"path": big}))
	if isErr {
		t.Fatalf("read errored: %s", out)
	}
	if strings.Count(out, "line ") > readDefaultLimit {
		t.Errorf("default read returned more than %d lines", readDefaultLimit)
	}
	if !strings.Contains(out, "more lines remain") {
		t.Errorf("expected continuation hint, got tail: %q", out[max(0, len(out)-80):])
	}

	// offset/limit select an exact window.
	out, isErr = read.Execute(context.Background(),
		args(t, map[string]any{"path": big, "offset": 10, "limit": 3}))
	if isErr {
		t.Fatalf("ranged read errored: %s", out)
	}
	if !strings.Contains(out, "line 10\n") || !strings.Contains(out, "line 12\n") || strings.Contains(out, "line 13") {
		t.Errorf("offset/limit window wrong: %q", out)
	}

	// a single pathological long line is truncated, not returned whole.
	longLine := filepath.Join(dir, "min.js")
	if err := os.WriteFile(longLine, []byte(strings.Repeat("x", 10000)), 0o644); err != nil {
		t.Fatal(err)
	}
	out, isErr = read.Execute(context.Background(), args(t, map[string]any{"path": longLine}))
	if isErr {
		t.Fatalf("long-line read errored: %s", out)
	}
	if !strings.Contains(out, "line truncated") || len(out) > readMaxLineBytes+200 {
		t.Errorf("overlong line not truncated, len=%d", len(out))
	}
}

// C5: edit refuses to act when old_string is missing or not unique, and edits
// when it is unique.
func TestEditUniqueness(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(path, []byte("foo bar foo"), 0o644); err != nil {
		t.Fatal(err)
	}
	edit := NewEditTool()

	// not unique
	if out, isErr := edit.Execute(context.Background(),
		args(t, map[string]any{"path": path, "old_string": "foo", "new_string": "X"})); !isErr {
		t.Errorf("non-unique old_string should error, got %q", out)
	}
	// missing
	if out, isErr := edit.Execute(context.Background(),
		args(t, map[string]any{"path": path, "old_string": "zzz", "new_string": "X"})); !isErr {
		t.Errorf("missing old_string should error, got %q", out)
	}
	// the file must be untouched after the two failed edits
	if data, _ := os.ReadFile(path); string(data) != "foo bar foo" {
		t.Errorf("file mutated by a failed edit: %q", data)
	}
	// unique
	if out, isErr := edit.Execute(context.Background(),
		args(t, map[string]any{"path": path, "old_string": "bar", "new_string": "BAZ"})); isErr {
		t.Errorf("unique edit should succeed, got %q", out)
	}
	if data, _ := os.ReadFile(path); string(data) != "foo BAZ foo" {
		t.Errorf("after edit = %q, want %q", data, "foo BAZ foo")
	}
}

// C5: glob returns matching paths.
func TestGlobMatches(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"one.go", "two.go", "skip.txt"} {
		if err := os.WriteFile(filepath.Join(dir, n), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out, isErr := NewGlobTool().Execute(context.Background(),
		args(t, map[string]any{"pattern": filepath.Join(dir, "*.go")}))
	if isErr {
		t.Fatalf("glob errored: %s", out)
	}
	if !strings.Contains(out, "one.go") || !strings.Contains(out, "two.go") || strings.Contains(out, "skip.txt") {
		t.Errorf("glob result = %q", out)
	}
}

// C5: grep finds a match whether or not ripgrep is installed.
func TestGrepFindsMatch(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "doc.txt"), []byte("alpha\nneedle here\nbeta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, isErr := NewGrepTool().Execute(context.Background(),
		args(t, map[string]any{"pattern": "needle", "path": dir}))
	if isErr {
		t.Fatalf("grep errored: %s", out)
	}
	if !strings.Contains(out, "needle") {
		t.Errorf("grep result = %q, want a line containing 'needle'", out)
	}
}

// C5: bash returns stdout via PowerShell, and the timeout path returns an error
// instead of hanging.
func TestBashRunAndTimeout(t *testing.T) {
	bash := NewBashTool(2 * time.Second)
	out, isErr := bash.Execute(context.Background(),
		args(t, map[string]any{"command": "echo mewcode-ok"}))
	if isErr {
		t.Fatalf("bash run failed: %s", out)
	}
	if !strings.Contains(out, "mewcode-ok") {
		t.Errorf("bash output = %q, want it to contain 'mewcode-ok'", out)
	}

	slow := NewBashTool(300 * time.Millisecond)
	out, isErr = slow.Execute(context.Background(),
		args(t, map[string]any{"command": "sleep 5"}))
	if !isErr {
		t.Errorf("slow command should time out, got %q", out)
	}
	if !strings.Contains(out, "timed out") {
		t.Errorf("timeout message = %q, want it to mention 'timed out'", out)
	}

	// a chatty command's output is capped, not dumped whole.
	out, isErr = bash.Execute(context.Background(),
		args(t, map[string]any{"command": "printf 'a%.0s' {1..10000}"}))
	if isErr {
		t.Fatalf("bash run failed: %s", out)
	}
	if len(out) > bashMaxOutputBytes+200 || !strings.Contains(out, "output truncated") {
		t.Errorf("bash output not capped, len=%d", len(out))
	}
}
