// Command mewcode is a terminal AI coding assistant. With --fake it runs a fully
// offline demo (no API key needed); the real Anthropic/OpenAI backends are wired
// in, plus file-based persistent memory under ~/.hudson.
package main

import (
	"context"
	"fmt"
	"os"

	"mewcode/internal/agent"
	"mewcode/internal/config"
	"mewcode/internal/display"
	"mewcode/internal/llm"
	"mewcode/internal/memory"
	"mewcode/internal/permission"
	"mewcode/internal/systemprompt"
	"mewcode/internal/tools"
)

func main() {
	wd, err := os.Getwd()
	if err != nil {
		wd = "."
	}

	cfg, err := config.Load(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "mewcode:", err)
		os.Exit(1)
	}

	// Build the persistent memory store first so AGENTS.md rules and the note
	// index can be preloaded into the system prompt before the client is built.
	store, err := memory.NewStore("~/.hudson", wd, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mewcode:", err)
		os.Exit(1)
	}

	base := systemprompt.Get(systemprompt.GetEnvironment(wd))
	var prompt string
	if mem := store.LoadAgents(); mem != "" {
		prompt = base + "\n" + mem
	} else {
		prompt = base
	}
	if idx := store.IndexText(); idx != "" {
		prompt += "\n" + idx
	}

	var client llm.Client
	if cfg.Fake {
		client = demoClient{} // offline demo backend, no API key needed
	} else {
		c, err := llm.NewClient(cfg, prompt)
		if err != nil {
			fmt.Fprintln(os.Stderr, "mewcode:", err)
			os.Exit(1)
		}
		client = c
		// The memory backend uses the same provider/model but a dedicated system
		// prompt for note classification and folding.
		memClient, err := llm.NewClient(cfg, memory.MemorySystemPrompt)
		if err != nil {
			fmt.Fprintln(os.Stderr, "mewcode:", err)
			os.Exit(1)
		}
		store.SetClient(memClient)
	}

	a := agent.New(client, tools.DefaultRegistry(), permission.NewManager(cfg.Mode))
	a.SetMemory(store)

	// On opening, load the most recent session into the conversation.
	if sess, err := store.OpenLatest(); err == nil && sess != nil && len(sess.Records) > 0 {
		a.LoadHistory(memory.RecordsToMessages(sess.Records))
	}

	disp := display.New()
	if err := disp.Run(context.Background(), a, cfg.Mode); err != nil {
		fmt.Fprintln(os.Stderr, "mewcode:", err)
		os.Exit(1)
	}
}
