package display

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"mewcode/internal/agent"
	"mewcode/internal/permission"
)

// Backend is the part of the agent loop used by the terminal display.
type Backend interface {
	Events() <-chan agent.Event
	Start(ctx context.Context, input string)
	SubmitDecision(permission.UserDecision)
	SetMode(string)

	ListSessions() []agent.SessionInfo
	CurrentSessionID() string
	LoadSession(id string) error
	NewSession() error
}

// Display runs the interactive terminal flow.
type Display interface {
	Run(ctx context.Context, be Backend, mode string) error
}

type FlowDisplay struct {
	mu      sync.Mutex
	writer  io.Writer
	scanner *bufio.Scanner
}

func New() Display {
	return &FlowDisplay{
		writer:  os.Stdout,
		scanner: bufio.NewScanner(os.Stdin),
	}
}

func (t *FlowDisplay) Run(ctx context.Context, be Backend, mode string) error {
	if err := t.WriteHeader(); err != nil {
		return err
	}

	for {
		if err := t.Prompt(); err != nil {
			return err
		}
		if !t.scanner.Scan() {
			_ = t.WriteGoodbye()
			return t.scanner.Err()
		}

		input := strings.TrimSpace(t.scanner.Text())
		if input == "" {
			continue
		}
		if input == "/exit" || input == "/quit" {
			_ = t.WriteGoodbye()
			return nil
		}

		if strings.HasPrefix(input, "/mode ") {
			newMode := strings.TrimSpace(strings.TrimPrefix(input, "/mode "))
			switch newMode {
			case "plan", "default", "auto":
				mode = newMode
				be.SetMode(mode)
				if err := t.writeLine("\033[90m· mode -> " + mode + "\033[0m"); err != nil {
					return err
				}
			default:
				if err := t.writeLine("\033[31munknown mode " + newMode + " (plan|default|auto)\033[0m"); err != nil {
					return err
				}
			}
			continue
		}

		if err := t.handleSessionCommand(input, be); err != nil {
			return err
		}
		if t.isSessionCommand(input) {
			continue
		}

		if err := t.writeLine("\033[1;34myou\033[0m  " + input); err != nil {
			return err
		}
		be.Start(ctx, input)
		if err := t.consumeEvents(ctx, be); err != nil {
			return err
		}
	}
}

func (t *FlowDisplay) WriteHeader() error {
	return t.write("Hudson CLI Chat\nType /exit to quit.\n\n")
}

func (t *FlowDisplay) Prompt() error {
	return t.write("\033[1;33m> \033[0m")
}

func (t *FlowDisplay) WriteGoodbye() error {
	return t.write("Goodbye.\r\n")
}

func (t *FlowDisplay) consumeEvents(ctx context.Context, be Backend) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-be.Events():
			if !ok {
				return nil
			}
			switch e := ev.(type) {
			case agent.AssistantText:
				if err := t.write(e.Text); err != nil {
					return err
				}
			case agent.ToolStarted:
				if err := t.writeLine(fmt.Sprintf("\n\033[33m[tool] %s\033[0m  %s", e.Name, oneLine(e.Input))); err != nil {
					return err
				}
			case agent.ToolFinished:
				color, label := "32", "OK"
				if e.IsError {
					color, label = "31", "ERROR"
				}
				if err := t.writeLine(fmt.Sprintf("\033[%sm[%s] %s\033[0m  %s", color, label, e.Name, oneLine(e.Result))); err != nil {
					return err
				}
			case agent.PermissionAsked:
				choice, err := t.askPermission(e.Request)
				if err != nil {
					return err
				}
				be.SubmitDecision(permission.UserDecision{
					ToolUseID: e.Request.ToolUseID,
					Choice:    choice,
				})
			case agent.TurnFinished:
				return t.write("\n")
			case agent.ErrorOccurred:
				if err := t.writeLine("\n\033[31merror: " + e.Err.Error() + "\033[0m"); err != nil {
					return err
				}
				return nil
			}
		}
	}
}

// isSessionCommand reports whether input is a /session command (handled
// separately from normal chat input).
func (t *FlowDisplay) isSessionCommand(input string) bool {
	return input == "/session" || input == "/session list" || input == "/session new" ||
		strings.HasPrefix(input, "/session ")
}

// handleSessionCommand lists/selects/creates sessions like `git branch`. It
// returns nil on success and a non-nil error only for I/O failures (not for a
// missing session id, which is reported to the user).
func (t *FlowDisplay) handleSessionCommand(input string, be Backend) error {
	if input == "/session" || input == "/session list" {
		list := be.ListSessions()
		cur := be.CurrentSessionID()
		if len(list) == 0 {
			return t.writeLine("\033[90m· no sessions yet\033[0m")
		}
		for _, s := range list {
			mark := "  "
			if s.ID == cur {
				mark = "* "
			}
			title := s.Title
			if title == "" {
				title = "(empty)"
			}
			if err := t.writeLine(fmt.Sprintf("%s%s  %s  (%d msgs)", mark, s.ID, title, s.Count)); err != nil {
				return err
			}
		}
		return nil
	}

	if input == "/session new" {
		if err := be.NewSession(); err != nil {
			return t.writeLine("\033[31msession new failed: " + err.Error() + "\033[0m")
		}
		return t.writeLine("\033[90m· session -> " + be.CurrentSessionID() + "\033[0m")
	}

	if strings.HasPrefix(input, "/session ") {
		id := strings.TrimSpace(strings.TrimPrefix(input, "/session "))
		if id == "" {
			return t.writeLine("\033[31musage: /session [list|new|<id>]\033[0m")
		}
		if err := be.LoadSession(id); err != nil {
			return t.writeLine("\033[31msession not loaded: " + err.Error() + "\033[0m")
		}
		return t.writeLine("\033[90m· session -> " + id + "\033[0m")
	}
	return nil
}

func (t *FlowDisplay) askPermission(req permission.PendingRequest) (permission.Choice, error) {
	if err := t.writeLine(fmt.Sprintf("\033[1;33m[permission] allow %s?\033[0m  %s", req.ToolName, oneLine(string(req.Input)))); err != nil {
		return permission.Refuse, err
	}
	if err := t.writeLine("  [a] allow once   [s] always allow   [d] deny"); err != nil {
		return permission.Refuse, err
	}
	if !t.scanner.Scan() {
		return permission.Refuse, t.scanner.Err()
	}

	switch strings.ToLower(strings.TrimSpace(t.scanner.Text())) {
	case "a", "1":
		return permission.AllowOnce, nil
	case "s", "2":
		return permission.AlwaysAllow, nil
	default:
		return permission.Refuse, nil
	}
}

func (t *FlowDisplay) write(text string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, err := io.WriteString(t.writer, text)
	return err
}

func (t *FlowDisplay) writeLine(line string) error {
	return t.write(line + "\n")
}

func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.TrimSpace(s)
}
