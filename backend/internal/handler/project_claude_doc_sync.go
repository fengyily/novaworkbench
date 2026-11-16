package handler

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/novaworkbench/backend/internal/model"
)

// ── CLAUDE.md generation: base-branch sync ────────────────────────────────
//
// A generated CLAUDE.md describes the code as it is NOW. When the local
// checkout sits on a stale commit — or on a feature branch that drifted away
// from main — the document describes code the project no longer runs. And
// because the document is injected into every later stage (analysis / design
// / coding / sub-tasks), that staleness compounds: every requirement keeps
// re-learning an outdated project.
//
// So before generating, the base branch is fetched and the generation is
// PINNED to the freshly fetched origin/<base> commit, materialized in a
// throwaway worktree:
//
//   - the user's working tree is NEVER touched (no checkout / merge / reset /
//     stash) — a generation run must not be able to disturb work in progress;
//   - the commit facts (branch, SHA, subject, ahead/behind) are streamed to
//     the UI so the user can judge whether the document was produced from
//     the code they expected.
//
// Every step degrades gracefully: a non-git project, an unreachable remote or
// a failed worktree creation falls back to generating against the local
// checkout, with the reason logged to the stream.

// claudeDocFetchTimeout bounds the `git fetch origin <base>` so an
// unreachable remote cannot hang the generation job. 60s mirrors the other
// fetch paths in this codebase and is generous enough for a large repo.
const claudeDocFetchTimeout = 60 * time.Second

// claudeDocWorktreePrefix names the throwaway worktrees this feature
// creates. It doubles as a safety marker: only directories whose base name
// carries this prefix are ever removed by the cleanup path.
const claudeDocWorktreePrefix = "nova-claude-doc-"

// claudeDocSyncInfo is the git state the generation ran against. Every field
// is best-effort metadata — a missing value means "git could not tell us",
// never "the run failed".
type claudeDocSyncInfo struct {
	// BaseBranch is the branch the document should describe ("main" by
	// default).
	BaseBranch string
	// Ref is the ref the generation is pinned to ("origin/main"), or "" when
	// no usable ref was found (the generation then runs against the local
	// checkout as-is).
	Ref string
	// RefSHA / RefSummary describe Ref: the full SHA and the
	// "<short sha> <subject> (<author>, <date>)" one-liner.
	RefSHA     string
	RefSummary string
	// CurrentBranch / HeadSHA / HeadSummary describe the user's checkout.
	CurrentBranch string
	HeadSHA       string
	HeadSummary   string
	// Behind / Ahead count the commits HEAD misses / has relative to Ref.
	Behind int
	Ahead  int
	// Fetched is true when `git fetch origin <base>` succeeded.
	Fetched bool
	// Note explains a skipped / degraded sync (never an error the run fails
	// on — it is surfaced to the user in the stream).
	Note string
}

// claudeDocBaseBranch returns the branch the document should be generated
// from: the project's configured default branch, or "main" when the project
// has none configured.
func claudeDocBaseBranch(p *model.Project) string {
	if p != nil {
		if b := strings.TrimSpace(p.DefaultBranch); b != "" {
			return b
		}
	}
	return "main"
}

// syncClaudeDocBase fetches the base branch and collects the commit facts the
// user needs to judge the freshness of the document.
//
// credEnv carries the project's platform token as GIT_ASKPASS pairs (see
// service.ProjectService.BuildGitCredentialEnv); it may be empty, in which
// case the fetch falls back to ambient credentials.
//
// The function never fails: git problems degrade into info.Note plus a log
// line, because a document generated from the local checkout is far better
// than no document at all.
func syncClaudeDocBase(projectPath, baseBranch string, credEnv []string, logf func(string)) claudeDocSyncInfo {
	info := claudeDocSyncInfo{BaseBranch: baseBranch}
	emit := func(s string) {
		if logf == nil {
			return
		}
		if s = strings.TrimSpace(s); s != "" {
			logf(s)
		}
	}
	if strings.TrimSpace(projectPath) == "" {
		info.Note = "项目没有本地目录，跳过主分支同步"
		emit("ℹ️ " + info.Note)
		return info
	}
	if _, err := gitRun(projectPath, "rev-parse", "--is-inside-work-tree"); err != nil {
		info.Note = "项目不是 Git 仓库，跳过主分支同步"
		emit("ℹ️ " + info.Note)
		return info
	}
	if br, err := gitRun(projectPath, "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
		info.CurrentBranch = strings.TrimSpace(br)
	}
	if sha, err := gitRun(projectPath, "rev-parse", "HEAD"); err == nil {
		info.HeadSHA = strings.TrimSpace(sha)
	}
	info.HeadSummary = gitCommitSummary(projectPath, "HEAD")

	remoteRef := "origin/" + baseBranch
	env := append([]string{"GIT_TERMINAL_PROMPT=0"}, credEnv...)
	if _, err := gitRunWithTimeout(projectPath, claudeDocFetchTimeout, env, "fetch", "origin", baseBranch); err != nil {
		info.Note = "主分支同步跳过（fetch 失败）：" + truncateStr(err.Error(), 300)
		emit("ℹ️ " + info.Note)
	} else {
		info.Fetched = true
		emit("🔄 已同步 " + remoteRef)
	}
	// Prefer the freshly fetched remote-tracking ref; fall back to the local
	// base branch when there is no origin/<base> (no remote, or the fetch
	// created no ref).
	if sha, err := gitRun(projectPath, "rev-parse", "--verify", "--quiet", remoteRef); err == nil {
		info.Ref = remoteRef
		info.RefSHA = strings.TrimSpace(sha)
	} else if sha, err := gitRun(projectPath, "rev-parse", "--verify", "--quiet", baseBranch); err == nil {
		info.Ref = baseBranch
		info.RefSHA = strings.TrimSpace(sha)
	}
	if info.Ref != "" {
		info.RefSummary = gitCommitSummary(projectPath, info.Ref)
		emit("📌 生成基准 " + info.Ref + " @ " + shortSHA(info.RefSHA))
		if info.RefSummary != "" {
			emit("📝 " + info.RefSummary)
		}
	}
	if info.CurrentBranch != "" {
		line := "📍 当前检出分支: " + info.CurrentBranch
		if info.HeadSummary != "" {
			line += " — " + info.HeadSummary
		}
		emit(line)
	}
	if info.Ref != "" && info.HeadSHA != "" {
		info.Behind, info.Ahead = claudeDocAheadBehind(projectPath, info.Ref, "HEAD")
		if info.Behind > 0 || info.Ahead > 0 {
			emit(fmt.Sprintf("ℹ️ 当前检出相对 %s：落后 %d 个提交，领先 %d 个提交（本次生成以 %s 为准）",
				info.Ref, info.Behind, info.Ahead, info.Ref))
		}
	}
	return info
}

// claudeDocAheadBehind counts how many commits ref has that other does not
// (behind) and the other way round (ahead). Both are zero when git cannot
// answer (shallow repo, unrelated histories).
func claudeDocAheadBehind(dir, ref, other string) (behind, ahead int) {
	out, err := gitRun(dir, "rev-list", "--left-right", "--count", ref+"..."+other)
	if err != nil {
		return 0, 0
	}
	fields := strings.Fields(out)
	if len(fields) != 2 {
		return 0, 0
	}
	behind, _ = strconv.Atoi(fields[0])
	ahead, _ = strconv.Atoi(fields[1])
	return behind, ahead
}

// gitCommitSummary returns the one-line description of a ref:
// "<short sha> <subject> (<author>, <date>)". Empty when git cannot answer.
func gitCommitSummary(dir, ref string) string {
	out, err := gitRun(dir, "log", "-1", ref,
		"--format=%h %s (%an, %ad)", "--date=format:%Y-%m-%d %H:%M")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// shortSHA trims a full SHA down to its conventional 7-character short form.
func shortSHA(sha string) string {
	sha = strings.TrimSpace(sha)
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// claudeDocGenerationRoot materializes ref in a throwaway worktree and
// returns the directory the generation should run in, plus a cleanup func.
//
// The worktree is what makes "sync the latest main branch" safe: it gives
// the model the pinned commit's files WITHOUT touching the user's working
// tree. A failed worktree creation is not fatal — the caller falls back to
// the local checkout.
func claudeDocGenerationRoot(projectPath, ref string) (string, func(), error) {
	noop := func() {}
	if strings.TrimSpace(ref) == "" {
		return "", noop, errors.New("no ref to pin the generation to")
	}
	dir, err := os.MkdirTemp("", claudeDocWorktreePrefix)
	if err != nil {
		return "", noop, err
	}
	// `git worktree add` accepts an existing EMPTY directory, which is
	// exactly what MkdirTemp hands us.
	if _, err := gitRun(projectPath, "worktree", "add", "--detach", dir, ref); err != nil {
		removeClaudeDocWorktreeDir(dir)
		return "", noop, fmt.Errorf("git worktree add --detach %s: %s", ref, truncateStr(err.Error(), 300))
	}
	cleanup := func() {
		if _, err := gitRun(projectPath, "worktree", "remove", "--force", dir); err != nil {
			// A leftover registration is harmless (`git worktree prune`
			// removes it); the directory itself is ours and goes below.
			log.Printf("[claude-doc] failed to remove worktree %s: %v", dir, err)
		}
		removeClaudeDocWorktreeDir(dir)
	}
	return dir, cleanup, nil
}

// removeClaudeDocWorktreeDir deletes a throwaway worktree directory.
//
// The path is recomputed and checked against the temp root AND the
// prefix marker before anything is removed: this is a recursive delete of a
// computed path, so it must be provably ours.
func removeClaudeDocWorktreeDir(dir string) {
	if strings.TrimSpace(dir) == "" {
		return
	}
	if !strings.HasPrefix(filepath.Base(dir), claudeDocWorktreePrefix) {
		log.Printf("[claude-doc] refusing to remove unexpected directory %s", dir)
		return
	}
	if err := os.RemoveAll(dir); err != nil {
		log.Printf("[claude-doc] failed to remove worktree dir %s: %v", dir, err)
	}
}

// sanitizeClaudeDocSnapshotArtifacts removes the snapshot's own traces from a
// generated document.
//
// The prompt already forbids the model from documenting the snapshot, but a
// prompt is a request, not a guarantee: a linked worktree's `.git` is a FILE
// pointing at the real repository's metadata, and that is exactly the kind
// of detail a model dutifully writes down ("the working copy at this path is
// a linked worktree (.git is a file containing gitdir: /tmp/nova-claude-doc-XXXX)").
//
// Such a line is a statement about the scratch checkout, not about the
// project — and the document is stored forever and injected into every later
// AI stage, so the random temp path would outlive the worktree by a long way.
//
// The scrubber is deliberately narrow: only lines that literally carry the
// snapshot's path (or its random directory name) are dropped. A project that
// legitimately uses git worktrees keeps its own wording — the marker is OUR
// directory name, which no project would ever mention.
func sanitizeClaudeDocSnapshotArtifacts(content, snapshotDir string) string {
	if strings.TrimSpace(content) == "" || strings.TrimSpace(snapshotDir) == "" {
		return content
	}
	markers := []string{snapshotDir, filepath.Base(snapshotDir)}
	lines := strings.Split(content, "\n")
	kept := make([]string, 0, len(lines))
	dropped := 0
	for _, line := range lines {
		hit := false
		for _, m := range markers {
			if m != "" && strings.Contains(line, m) {
				hit = true
				break
			}
		}
		if hit {
			dropped++
			continue
		}
		kept = append(kept, line)
	}
	if dropped == 0 {
		return content
	}
	log.Printf("[claude-doc] scrubbed %d line(s) describing the generation snapshot", dropped)
	return strings.Join(kept, "\n")
}
