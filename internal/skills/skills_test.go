package skills

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeSkill writes a SKILL.md (with frontmatter + body) into dir/name and
// returns the skill directory path.
func writeSkill(t *testing.T, root, name, description, body string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: " + name + "\ndescription: " + description + "\n---\n" + body
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestDiscoverBothScopesAndProjectWins(t *testing.T) {
	user := t.TempDir()    // ~/.hudson
	project := t.TempDir() // <workdir>

	writeSkill(t, filepath.Join(user, "skills"), "alpha", "user alpha", "user body")
	writeSkill(t, filepath.Join(user, "skills"), "shared", "user shared", "user shared body")
	writeSkill(t, filepath.Join(project, ".hudson", "skills"), "shared", "project shared", "project shared body")
	writeSkill(t, filepath.Join(project, ".hudson", "skills"), "beta", "project beta", "project beta body")

	m := NewManager(user, project)
	if m.Len() != 3 {
		t.Fatalf("Len = %d, want 3", m.Len())
	}

	shared, ok := m.Get("shared")
	if !ok {
		t.Fatal("shared skill not found")
	}
	if shared.Description != "project shared" || shared.Scope != "project" {
		t.Errorf("project skill should shadow user skill, got %+v", shared)
	}

	// deterministic order: project scope first (beta, shared), then user (alpha).
	names := []string{}
	for _, s := range m.Skills() {
		names = append(names, s.Name)
	}
	want := []string{"beta", "shared", "alpha"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("order = %v, want %v", names, want)
	}
}

func TestCatalogOnlyHasNameAndDescription(t *testing.T) {
	user := t.TempDir()
	writeSkill(t, filepath.Join(user, "skills"), "alpha", "does alpha things", "secret body that must not leak")
	m := NewManager(user, ".")

	cat := m.Catalog()
	if !strings.Contains(cat, "alpha") || !strings.Contains(cat, "does alpha things") {
		t.Errorf("catalog missing name/description: %q", cat)
	}
	if strings.Contains(cat, "secret body") {
		t.Errorf("catalog must not contain the skill body: %q", cat)
	}
}

func TestLoadRereadsFromDisk(t *testing.T) {
	user := t.TempDir()
	dir := writeSkill(t, filepath.Join(user, "skills"), "alpha", "desc", "v1")
	m := NewManager(user, ".")

	body, err := m.Load("alpha")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if body != "v1" {
		t.Fatalf("body = %q, want v1", body)
	}

	// Change the body on disk without recreating the manager.
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"),
		[]byte("---\nname: alpha\ndescription: desc\n---\nv2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	body, err = m.Load("alpha")
	if err != nil {
		t.Fatalf("Load after edit: %v", err)
	}
	if body != "v2" {
		t.Errorf("body = %q, want v2 (must reread from disk)", body)
	}
}

func TestParseErrorDoesNotInterfere(t *testing.T) {
	user := t.TempDir()
	skillsDir := filepath.Join(user, "skills")
	writeSkill(t, skillsDir, "good", "a good skill", "good body")

	// A malformed skill: missing description.
	bad := filepath.Join(skillsDir, "bad")
	if err := os.MkdirAll(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bad, "SKILL.md"), []byte("---\nname: bad\n---\nno description"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := NewManager(user, ".")
	if m.Len() != 1 {
		t.Fatalf("Len = %d, want 1 (bad skill skipped)", m.Len())
	}
	if _, ok := m.Get("good"); !ok {
		t.Error("good skill should survive a sibling parse error")
	}
	if _, ok := m.Get("bad"); ok {
		t.Error("bad skill should be skipped")
	}
}

func TestActivateToolReturnsInlineBody(t *testing.T) {
	user := t.TempDir()
	writeSkill(t, filepath.Join(user, "skills"), "alpha", "desc", "# Alpha\n\nsteps here")
	m := NewManager(user, ".")

	tool := NewActivateTool(m)
	out, isErr := tool.Execute(context.Background(), json.RawMessage(`{"name":"alpha"}`))
	if isErr {
		t.Fatalf("activate errored: %s", out)
	}
	if !strings.Contains(out, "<skill_content") || !strings.Contains(out, "steps here") {
		t.Errorf("activate output = %q, want wrapped inline body", out)
	}
	if strings.Contains(out, "name: alpha") {
		t.Errorf("activate output should strip frontmatter, got %q", out)
	}
}

func TestActivateToolCarriesArgument(t *testing.T) {
	user := t.TempDir()
	// Skill body with a placeholder variable.
	writeSkill(t, filepath.Join(user, "skills"), "tpl", "desc", "run {{argument}} now")
	m := NewManager(user, ".")
	tool := NewActivateTool(m)

	out, isErr := tool.Execute(context.Background(), json.RawMessage(`{"name":"tpl","argument":"the build"}`))
	if isErr {
		t.Fatalf("activate errored: %s", out)
	}
	if !strings.Contains(out, "run the build now") {
		t.Errorf("argument not substituted: %q", out)
	}

	// Skill body without a placeholder still carries the argument.
	writeSkill(t, filepath.Join(user, "skills"), "plain", "desc", "just do it")
	m = NewManager(user, ".")
	tool = NewActivateTool(m)
	out, isErr = tool.Execute(context.Background(), json.RawMessage(`{"name":"plain","argument":"extra context"}`))
	if isErr {
		t.Fatalf("activate errored: %s", out)
	}
	if !strings.Contains(out, "extra context") {
		t.Errorf("argument should be carried when no variable present: %q", out)
	}
}

func TestActivateToolUnknownSkill(t *testing.T) {
	m := NewManager(t.TempDir(), ".")
	tool := NewActivateTool(m)
	out, isErr := tool.Execute(context.Background(), json.RawMessage(`{"name":"nope"}`))
	if !isErr {
		t.Errorf("unknown skill should be an error, got %q", out)
	}
}
