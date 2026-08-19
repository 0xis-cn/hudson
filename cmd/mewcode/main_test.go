package main

import (
	"context"
	"strings"
	"testing"

	"mewcode/internal/agent"
	"mewcode/internal/config"
	"mewcode/internal/permission"
	"mewcode/internal/tools"
)

// TestDemoBackendDrivesARealTurn proves the --fake demo works end to end at the
// code level: the demo backend streams text, the agent loop runs a real tool,
// and the turn completes.
func TestDemoBackendDrivesARealTurn(t *testing.T) {
	a := agent.New(demoClient{}, tools.DefaultRegistry(), permission.NewManager(config.ModeAuto))

	var sawText, sawToolOK, finished bool
	done := make(chan struct{})
	go func() {
		for ev := range a.Events() {
			switch e := ev.(type) {
			case agent.AssistantText:
				if e.Text != "" {
					sawText = true
				}
			case agent.ToolFinished:
				if e.Name == "read_file" && !e.IsError {
					sawToolOK = true
				}
			case agent.TurnFinished:
				finished = true
				close(done)
				return
			case agent.ErrorOccurred:
				close(done)
				return
			}
		}
	}()

	a.RunTurn(context.Background(), "please read main.go")
	<-done

	if !sawText {
		t.Error("demo should stream assistant text")
	}
	if !sawToolOK {
		t.Error("demo should run read_file successfully on a real file")
	}
	if !finished {
		t.Error("turn should finish")
	}

	// the real file content should be in the history's tool result
	hist := a.History()
	if len(hist) < 3 || len(hist[2].ToolResults) != 1 {
		t.Fatalf("expected a tool result in history, got %+v", hist)
	}
	if !strings.Contains(hist[2].ToolResults[0].Content, "package main") {
		t.Errorf("tool result should contain real main.go content, got: %q", hist[2].ToolResults[0].Content)
	}
}

// TestPlanDemoReplyRouting checks the keyword router picks sensible tools.
func TestPlanDemoReplyRouting(t *testing.T) {
	cases := []struct {
		input string
		want  string // "" means no tool
	}{
		{"read main.go", "read_file"},
		{"list files", "glob"},
		{"run go version", "bash"},
		{"create a file", "write_file"},
		{"search for func", "grep"},
		{"which directory am I in", "bash"}, // routes to pwd, not a blind read
		{"我现在在哪个目录下", "bash"},               // Chinese "which directory" also routed
		{"hello", ""},                       // unrecognized → text help, no tool
		{"完全不相关的胡话", ""},                    // unrecognized → text help, not a blind main.go read
	}
	for _, c := range cases {
		_, name, _ := planDemoReply(c.input)
		if name != c.want {
			t.Errorf("planDemoReply(%q) tool = %q, want %q", c.input, name, c.want)
		}
	}
}
