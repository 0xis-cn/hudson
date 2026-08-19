package systemprompt

import (
	"strings"
	"runtime"
	"os"
	"time"
	"os/exec"
)

type EnvironmentContext struct {
	Dir string
	Os string
	Arch string
	Shell string
	Date string
	IsGitRepo bool
	GitBranch string
	Model string
}

func GetEnvironment(dir string) EnvironmentContext {
	env := EnvironmentContext{
		Dir: dir,
		Os: runtime.GOOS,
		Arch: runtime.GOARCH,
		Shell: os.Getenv("SHELL"),
		Date: time.Now().Format("2006-01-02"),
	}
	if env.Shell == "" {
		env.Shell = "bash"
	}
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--is-inside-working-tree").Output()
	if err == nil && strings.TrimSpace(string(out)) == "true" {
		env.IsGitRepo = true
		if branch, err := exec.Command("git", "-C", dir, "rev-parse", "--abbrev-ref", "HEAD").Output(); err == nil {
			env.GitBranch = strings.TrimSpace(string(branch))
		}
	}
	return env
}

func Get(env EnvironmentContext) string {
	sections := []section{
		oldSection,
		systemSection,
		taskSection,
		actionSection,
		toolSection,
		toneSection,
		outputEfficiencySection,
		getEnvironmentSection(env),
	}
	// if skillSection != "" {
	// 	sections = append(sections, section{"skill", skillSection})
	// }

	var b strings.Builder
	for _, s := range sections {
		b.WriteString(s.Content)
	}
	return b.String()
}