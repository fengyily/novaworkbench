package handler

import (
	"log"
	"strings"

	"github.com/novaworkbench/backend/internal/llm"
	promptpkg "github.com/novaworkbench/backend/internal/prompt"
	"github.com/novaworkbench/backend/internal/service"
)

// ── The single entry point for injecting the project-level CLAUDE.md ───────
//
// Before this change every stage that needed the project's conventions
// called claudeDocBlock() itself and spliced the document into the user
// prompt. Seven call sites did that, eight more stages did it not at all,
// and the repository's own CLAUDE.md was loaded by the CLI in parallel — so
// the model could see two (or four) versions of the same document, with the
// winner decided by a sentence in a prompt.
//
// This file is the one place that decides what happens:
//
//   - native (default): the managed copy is materialized as a real
//     CLAUDE.md under NovaWorkbench's data directory and granted to the
//     session with --add-dir. The CLI then loads it through its OWN
//     project-memory channel — the same mechanism that loads a repository's
//     CLAUDE.md — with the CLI's own authoritative framing. The
//     repository's instruction files are excluded at the same time, so
//     exactly ONE project document is in the context.
//
//   - prompt (fallback): the document is spliced into the user prompt, the
//     way it was before. Used when materialization fails (a read-only home
//     directory, a full disk) or when the operator turns the native channel
//     off.
//
// The mode is a setting (service.ClaudeDocInjectionConfig), so it can be
// changed without a redeploy.

// claudeDocInjection is the resolved outcome of the injection decision for
// one run.
type claudeDocInjection struct {
	// Native reports that the document travels through the CLI's native
	// project-memory channel. When false the document is (or would be)
	// spliced into the user prompt.
	Native bool
	// MemDir is the directory holding the materialized CLAUDE.md. It is
	// granted to the session with --add-dir so the CLI loads it. Empty in
	// prompt mode and when the project has no managed document.
	MemDir string
	// ExcludeRepo reports that the repository's own instruction files must
	// be kept out of the CLI's load set, so the managed copy is the only
	// project document in the context.
	ExcludeRepo bool
	// Content is the managed document, used to render the prompt-mode
	// block. Empty when the project has no managed document.
	Content string
	// DiskExists reports whether the directory the CLI runs in holds a
	// CLAUDE.md of its own. It is only consulted for the prompt-mode
	// wording.
	DiskExists bool
}

// Block renders the document as a prompt section.
//
// It returns an empty string in native mode — the document is already in
// the context through the CLI, and repeating it here would put it back to
// two copies.
func (i claudeDocInjection) Block() string {
	if i.Native || strings.TrimSpace(i.Content) == "" {
		return ""
	}
	return promptpkg.BuildProjectClaudeDocBlock(i.Content, i.DiskExists)
}

// ApplyTo stamps the injection onto a claude invocation.
//
// In native mode it grants the memory directory (so the CLI loads the
// managed copy). In prompt mode it does nothing — the document is in the
// prompt instead.
//
// The exclusion of the repository's own files is requested here, but
// COMPUTED by the gateway: only the gateway knows the full set of granted
// directories, and the CLI's env var that turns granted directories into
// "CLAUDE.md dirs" applies to all of them.
func (i claudeDocInjection) ApplyTo(opts *llm.StreamOpts) {
	if opts == nil {
		return
	}
	if i.Native {
		opts.ClaudeMdDir = i.MemDir
	}
	opts.ExcludeRepoClaudeMd = i.ExcludeRepo
}

// claudeDocSettingsReader reads the injection policy. The setting service
// implements it; a nil reader means "use the shipped defaults".
type claudeDocSettingsReader interface {
	ClaudeDocInjectionConfig() (service.ClaudeDocInjectionConfig, error)
}

// resolveClaudeDocInjection decides how the managed project document reaches
// the model for one run.
//
// workDir is the directory the CLI will run in — the requirement worktree for
// most stages, the project checkout for others. It matters for two reasons:
// it is where the CLI looks for the repository's own instruction files (the
// ones to exclude), and it decides whether the repository ships a
// CLAUDE.md at all.
//
// The returned injection is empty (no document, nothing to inject) when the
// project has no managed CLAUDE.md — the pre-change contract that a project
// without a document sees a byte-for-byte identical prompt.
func resolveClaudeDocInjection(
	svc claudeDocReader,
	settings claudeDocSettingsReader,
	projectID, workDir string,
) claudeDocInjection {
	if svc == nil || strings.TrimSpace(projectID) == "" {
		return claudeDocInjection{}
	}
	doc, err := svc.ClaudeDoc(projectID)
	if err != nil {
		log.Printf("[claude-doc] load doc for project %s: %v", projectID, err)
		return claudeDocInjection{}
	}
	if doc == nil || strings.TrimSpace(doc.Content) == "" {
		return claudeDocInjection{}
	}

	cfg := service.DefaultClaudeDocInjectionConfig()
	if settings != nil {
		if c, cerr := settings.ClaudeDocInjectionConfig(); cerr != nil {
			log.Printf("[claude-doc] read injection config for project %s: %v", projectID, cerr)
		} else {
			cfg = c
		}
	}

	inj := claudeDocInjection{
		Content:    doc.Content,
		DiskExists: service.DiskClaudeDocExists(workDir),
		Native:     cfg.InjectMode == service.ClaudeDocInjectModeNative,
	}

	if !inj.Native {
		// Prompt mode is the pre-change behaviour, byte for byte: the
		// document is spliced into the prompt and the CLI keeps loading the
		// repository's own copy as it always has. Excluding the repository
		// copy here would change that behaviour, so the switch does not
		// apply on this path.
		return inj
	}

	// Native mode: the managed copy is the project's only convention
	// document, so the repository's own copy is kept out of the CLI's load
	// set. The switch lets an operator turn that off to compare the two.
	inj.ExcludeRepo = cfg.ExcludeRepoDoc

	// Materialize the managed copy as a real CLAUDE.md. On failure the run
	// degrades to the prompt-level injection: a missing project convention
	// must never block a requirement.
	memDir, merr := service.MaterializeClaudeDoc(projectID, doc.Content)
	if merr != nil || memDir == "" {
		log.Printf("[claude-doc] materialize managed doc for project %s failed, falling back to prompt injection: %v", projectID, merr)
		inj.Native = false
	}
	inj.MemDir = memDir
	return inj
}
