// Package handler tests the fallback session-file scanner used when
// the candidate list derived from project metadata misses the
// cwd-encoded slug the analyst actually used.
//
// Regression guard for req_d92d397bb4ae6286: when an analyst chat
// runs in a subdir / workspace symlink / drifted project path, the
// resulting .jsonl lands in a slug dir the architect stage's
// claudeProjectsSlugDir doesn't probe. findSessionJsonlFallback
// must locate it via full-table scan so the remote --resume can
// succeed.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/novaworkbench/backend/internal/db"
	"github.com/novaworkbench/backend/internal/llm"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
	gossh "github.com/novaworkbench/backend/internal/ssh"
	"github.com/novaworkbench/backend/internal/store"
	"github.com/novaworkbench/backend/internal/util"
)

// TestFindSessionJsonlFallback covers:
//   - empty sid → no-op
//   - no projects dir → no-op
//   - single hit → returned
//   - multiple hits (renamed project) → most-recently-modified wins
//   - non-matching files in the same dir → ignored
//
// Note: claudeSessionHome() reads $CLAUDE_CONFIG_DIR first, then
// $NOVA_CLAUDE_HOME, then ~/.claude. Tests point CLAUDE_CONFIG_DIR
// at t.TempDir() and explicitly clear NOVA_CLAUDE_HOME so that any
// value leaking from the host environment can't shadow the temp dir.
func TestFindSessionJsonlFallback(t *testing.T) {
	const sid = "07ff6a81-312e-4d8e-8489-3fa4d98c47c1"

	t.Run("empty sid returns false", func(t *testing.T) {
		got, ok := findSessionJsonlFallback("")
		if ok || got != "" {
			t.Fatalf("want (\"\", false), got (%q, %v)", got, ok)
		}
	})

	t.Run("no projects dir returns false", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("CLAUDE_CONFIG_DIR", home)
		t.Setenv("NOVA_CLAUDE_HOME", "")
		if _, ok := findSessionJsonlFallback(sid); ok {
			t.Fatal("expected no hit with empty projects dir")
		}
	})

	t.Run("single matching slug dir returns the jsonl", func(t *testing.T) {
		home := t.TempDir()
		slugDir := filepath.Join(home, "projects", "--users-f1-test-proj-p")
		if err := os.MkdirAll(slugDir, 0o755); err != nil {
			t.Fatal(err)
		}
		want := filepath.Join(slugDir, sid+".jsonl")
		if err := os.WriteFile(want, []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("CLAUDE_CONFIG_DIR", home)
		t.Setenv("NOVA_CLAUDE_HOME", "")
		got, ok := findSessionJsonlFallback(sid)
		if !ok {
			t.Fatalf("expected hit, got none")
		}
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("multiple hits prefer most-recently-modified", func(t *testing.T) {
		home := t.TempDir()

		// Stale hit: written first, then we set its mtime back by 1h
		// so the fresh hit is unambiguously newer. Using an explicit
		// time delta avoids flake when both files were written in
		// the same OS time tick.
		staleDir := filepath.Join(home, "projects", "--old-slug")
		if err := os.MkdirAll(staleDir, 0o755); err != nil {
			t.Fatal(err)
		}
		staleFile := filepath.Join(staleDir, sid+".jsonl")
		if err := os.WriteFile(staleFile, []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		past := time.Now().Add(-1 * time.Hour)
		if err := os.Chtimes(staleFile, past, past); err != nil {
			t.Fatalf("chtimes stale: %v", err)
		}

		// Fresh hit: written after, mtime = now. Must be the one
		// the helper returns.
		freshDir := filepath.Join(home, "projects", "--users-f1-test-proj-p--subdir")
		if err := os.MkdirAll(freshDir, 0o755); err != nil {
			t.Fatal(err)
		}
		freshFile := filepath.Join(freshDir, sid+".jsonl")
		if err := os.WriteFile(freshFile, []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		if err := os.Chtimes(freshFile, now, now); err != nil {
			t.Fatalf("chtimes fresh: %v", err)
		}

		t.Setenv("CLAUDE_CONFIG_DIR", home)
		t.Setenv("NOVA_CLAUDE_HOME", "")
		got, ok := findSessionJsonlFallback(sid)
		if !ok {
			t.Fatal("expected hit, got none")
		}
		if got != freshFile {
			t.Fatalf("got %q, want fresh %q", got, freshFile)
		}
	})

	t.Run("non-matching jsonl in same dir ignored", func(t *testing.T) {
		home := t.TempDir()
		slugDir := filepath.Join(home, "projects", "--some-other-slug")
		if err := os.MkdirAll(slugDir, 0o755); err != nil {
			t.Fatal(err)
		}
		other := filepath.Join(slugDir, "different-sid.jsonl")
		if err := os.WriteFile(other, []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("CLAUDE_CONFIG_DIR", home)
		t.Setenv("NOVA_CLAUDE_HOME", "")
		if _, ok := findSessionJsonlFallback(sid); ok {
			t.Fatal("did not expect a hit for sid not present on disk")
		}
	})
}

// --------------------------------------------------------------------
// workerRunRequest: systemPrompt / disallowedTools pass-through
// --------------------------------------------------------------------

// TestWorkerRunRequest_ForwardsWikiToolDenylist is the regression guard
// for the remote-wiki failure mode: wikiDisallowedTools (notably "Task")
// is what stops Claude from fanning out Explore sub-agents and ending its
// turn on a one-line "等待回收中…" preamble — which finalizeWikiRun then
// rejects, so dropping the field would make remote wiki fail every time.
// The worker has understood `disallowedTools` since before this change
// (agent-worker/server.mjs buildClaudeArgs); only the Go side was silent.
func TestWorkerRunRequest_ForwardsWikiToolDenylist(t *testing.T) {
	body := workerRunRequest(llm.StreamOpts{
		WorkDir:         "/tmp/x",
		Prompt:          "hi",
		PermissionMode:  "plan",
		SystemPrompt:    "你是一位资深软件工程师",
		DisallowedTools: wikiDisallowedTools,
	}, nil, nil)

	if body.SystemPrompt != "你是一位资深软件工程师" {
		t.Errorf("SystemPrompt not forwarded: got %q", body.SystemPrompt)
	}
	if len(body.DisallowedTools) != len(wikiDisallowedTools) {
		t.Fatalf("DisallowedTools: got %#v, want %#v", body.DisallowedTools, wikiDisallowedTools)
	}
	hasTask := false
	for _, tool := range body.DisallowedTools {
		if tool == "Task" {
			hasTask = true
		}
	}
	if !hasTask {
		t.Errorf("DisallowedTools must contain \"Task\" (sub-agent ban), got %#v", body.DisallowedTools)
	}

	// Both fields must survive JSON marshaling under the names the worker
	// destructures (`systemPrompt` / `disallowedTools`).
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{`"systemPrompt"`, `"disallowedTools"`, `"Task"`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("wire format missing %s: %s", key, raw)
		}
	}
}

// TestWorkerRunRequest_OmitsToolFieldsForArchitect pins the "architect and
// coding behaviour is byte-identical" half of the change: both callers
// leave SystemPrompt / DisallowedTools at their zero values, and omitempty
// must keep them off the wire entirely so an older worker destructuring a
// fixed field list behaves exactly as before.
func TestWorkerRunRequest_OmitsToolFieldsForArchitect(t *testing.T) {
	body := workerRunRequest(llm.StreamOpts{
		WorkDir:        "/tmp/x",
		Prompt:         "hi",
		PermissionMode: "plan",
	}, nil, nil)
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, key := range []string{"systemPrompt", "disallowedTools"} {
		if strings.Contains(string(raw), key) {
			t.Errorf("%s leaked into the wire format for a non-wiki caller: %s", key, raw)
		}
	}
}

// TestRequirementSessionIDs_IncludesWikiSession pins that a kind=wiki
// requirement's conversation id is part of the SFTP up-sync set. Wiki rows
// never populate design_session_id, so without the explicit entry the
// second and later remote wiki runs would find no jsonl to --resume and
// silently restart with no prior context.
func TestRequirementSessionIDs_IncludesWikiSession(t *testing.T) {
	ids := requirementSessionIDs(&model.Requirement{
		AnalysisSessionID: "sid-analysis",
		WikiSessionID:     "sid-wiki",
	}, "sid-wiki")

	// sourceSID == WikiSessionID must dedupe to one entry.
	want := []string{"sid-analysis", "sid-wiki"}
	if len(ids) != len(want) {
		t.Fatalf("got %#v, want %#v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("index %d: got %q, want %q (full %#v)", i, ids[i], want[i], ids)
		}
	}
}

// ---------------------------------------------------------------------------
// Remote session-dir discovery + session down-sync.
//
// Background (req_6a9b88dd80fe8f01): a requirement developed on an Agent
// Server never got its Claude transcript back to the local machine, so the
// next "追加任务" found no <sid>.jsonl locally and the remote started a
// brand-new session with the whole context lost.
//
// The root cause was that the down-sync COMPUTED the remote session dir
// (~/.claude/projects/<slug-of-cwd>) instead of FINDING it. The computed
// path misses whenever the remote work root changed between the run and the
// download, the CLI resolved a symlink in its cwd (/tmp → /private/tmp), or
// the worker ran with a different HOME — and the miss was silent
// (ErrRemoteDirMissing was swallowed).
// ---------------------------------------------------------------------------

// TestChooseRemoteSessionDir pins the discovery decision table.
//
// The evidence is (sidDirs = dirs that actually hold <sid>.jsonl,
// nameDirs = every remote project dir name); the priority is:
// computed-holds-it → holds-it-elsewhere → name-match → name-contains →
// computed fallback.
func TestChooseRemoteSessionDir(t *testing.T) {
	const (
		projectsRoot = "/home/u/.claude/projects"
		computed     = projectsRoot + "/-tmp-nova-agent-proj-req"
		slug         = "-tmp-nova-agent-proj-req"
	)
	// The realpath-divergence variant: /private/tmp/... encodes to a name
	// that CONTAINS the computed slug.
	divergent := projectsRoot + "/-private-tmp-nova-agent-proj-req"
	// ListRemoteDirs returns the dir NAME (not the full path).
	divergentName := "-private-tmp-nova-agent-proj-req"

	cases := []struct {
		name     string
		sidDirs  []string
		nameDirs []string
		want     string
	}{
		{
			name:     "computed path holds the session file",
			sidDirs:  []string{computed},
			nameDirs: []string{slug},
			want:     computed,
		},
		{
			name:     "session file lives in another dir",
			sidDirs:  []string{divergent},
			nameDirs: []string{divergentName},
			want:     divergent,
		},
		{
			name:     "exact slug match wins over a longer divergent name",
			sidDirs:  []string{divergent, computed},
			nameDirs: []string{divergentName, slug},
			want:     computed,
		},
		{
			name:     "no session file yet but the dir name matches the slug",
			sidDirs:  nil,
			nameDirs: []string{slug, divergentName},
			want:     computed,
		},
		{
			name:     "realpath divergence — dir name contains the slug",
			sidDirs:  nil,
			nameDirs: []string{divergentName},
			want:     divergent,
		},
		{
			name:     "nothing found — fall back to the computed path",
			sidDirs:  nil,
			nameDirs: []string{"-some-other-project"},
			want:     computed,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := chooseRemoteSessionDir(computed, projectsRoot, slug, tc.sidDirs, tc.nameDirs)
			if got.dir != tc.want {
				t.Fatalf("got dir %q, want %q (why=%q)", got.dir, tc.want, got.why)
			}
			if got.why == "" {
				t.Fatal("expected a human-readable reason for the job log")
			}
		})
	}
}

// TestRemotePathHelpers pins the POSIX path splitting the discovery relies on.
func TestRemotePathHelpers(t *testing.T) {
	if got := remoteProjectsRootOf("/home/u/.claude/projects/-tmp-x"); got != "/home/u/.claude/projects" {
		t.Fatalf("remoteProjectsRootOf: got %q", got)
	}
	if got := remoteProjectsRootOf("/"); got != "" {
		t.Fatalf("remoteProjectsRootOf of a bare slash: got %q, want empty", got)
	}
	if got := remoteDirOf("/home/u/.claude/projects/-tmp-x/sid.jsonl"); got != "/home/u/.claude/projects/-tmp-x" {
		t.Fatalf("remoteDirOf: got %q", got)
	}
	if got := remoteBaseName("/home/u/.claude/projects/-tmp-x"); got != "-tmp-x" {
		t.Fatalf("remoteBaseName: got %q", got)
	}
}

// TestSessionSyncTimeout_EnvOverride pins the configurable down-sync budget:
// the old fixed 60s was too tight for the multi-MB transcripts this repo
// produces, and a timeout used to leave the transfer half-done.
func TestSessionSyncTimeout_EnvOverride(t *testing.T) {
	t.Run("default is 5 minutes", func(t *testing.T) {
		t.Setenv("NOVA_SESSION_SYNC_TIMEOUT", "")
		if got := sessionSyncTimeout(); got != 5*time.Minute {
			t.Fatalf("got %v, want 5m", got)
		}
	})
	t.Run("env override wins", func(t *testing.T) {
		t.Setenv("NOVA_SESSION_SYNC_TIMEOUT", "90s")
		if got := sessionSyncTimeout(); got != 90*time.Second {
			t.Fatalf("got %v, want 90s", got)
		}
	})
	t.Run("garbage falls back to the default", func(t *testing.T) {
		t.Setenv("NOVA_SESSION_SYNC_TIMEOUT", "not-a-duration")
		if got := sessionSyncTimeout(); got != 5*time.Minute {
			t.Fatalf("got %v, want 5m", got)
		}
	})
}

// TestSessionFallbackEnabled pins the emergency kill switch that restores the
// old "trust the computed path only" behaviour without a code rollback.
func TestSessionFallbackEnabled(t *testing.T) {
	t.Run("enabled by default", func(t *testing.T) {
		t.Setenv("NOVA_SESSION_SYNC_FALLBACK", "")
		if !sessionFallbackEnabled() {
			t.Fatal("want the discovery enabled by default")
		}
	})
	t.Run("0 disables it", func(t *testing.T) {
		t.Setenv("NOVA_SESSION_SYNC_FALLBACK", "0")
		if sessionFallbackEnabled() {
			t.Fatal("want the discovery disabled")
		}
	})
}

// TestDedupeSessionIDs pins the order-preserving dedupe the down-sync uses to
// build its download list.
func TestDedupeSessionIDs(t *testing.T) {
	got := dedupeSessionIDs([]string{"a", "", "b", "a", "c", "b"})
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("index %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

// testProjectSvc builds a ProjectService backed by a throwaway SQLite DB.
// The session-sync code paths only need it to resolve the requirement's
// project row (claudeProjectsSlugDir); an empty DB exercises the legacy
// "first slug dir on disk" fallback.
func testProjectSvc(t *testing.T) *service.ProjectService {
	t.Helper()
	dir := t.TempDir()
	d, err := db.Init(db.Config{Driver: "sqlite", SQLitePath: filepath.Join(dir, "test.db")})
	if err != nil {
		t.Fatalf("init db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return service.NewProjectService(d, nil, nil)
}

// fakeSessionClient is an in-memory remoteSessionClient used to exercise the
// discovery + download flow without an SSH server.
type fakeSessionClient struct {
	// files maps a remote path to its content. Missing keys = "not there".
	files map[string]string
	// dirs maps a remote dir path to the dir names it contains.
	dirs map[string][]string
	// downloads records every (remote, local) pair that was fetched.
	downloads []string
	// missingFiles are reported as ErrRemoteFileMissing.
	missingFiles map[string]bool
}

func (f *fakeSessionClient) RemoteFileExists(remotePath string) (bool, error) {
	_, ok := f.files[remotePath]
	return ok, nil
}

func (f *fakeSessionClient) ListRemoteDirs(remoteDir string) ([]string, error) {
	return f.dirs[remoteDir], nil
}

func (f *fakeSessionClient) ListRemoteFilesUnder(ctx context.Context, root, fileName string) ([]string, error) {
	var out []string
	for p := range f.files {
		if strings.HasPrefix(p, root+"/") && strings.HasSuffix(p, "/"+fileName) {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (f *fakeSessionClient) DownloadFileMapped(remoteFile, localPath string) error {
	if f.missingFiles[remoteFile] {
		return gossh.ErrRemoteFileMissing
	}
	content, ok := f.files[remoteFile]
	if !ok {
		return gossh.ErrRemoteFileMissing
	}
	f.downloads = append(f.downloads, remoteFile+" -> "+localPath)
	if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(localPath, []byte(content), 0o644)
}

func (f *fakeSessionClient) DownloadDirMapped(remoteDir, localDir string) error {
	return gossh.ErrRemoteDirMissing
}

func (f *fakeSessionClient) ExpandHome(remotePath string) (string, error) {
	return strings.Replace(remotePath, "~", "/home/u", 1), nil
}

// TestDoSyncSessionDown_DiscoversRemoteDirAndWritesBothTargets is the core
// regression guard: the transcript lives in a dir the computed path does not
// name, and it must land in BOTH local candidate dirs (the project-root slug
// the up-sync reads from AND the worktree slug a local run would resume
// from).
func TestDoSyncSessionDown_DiscoversRemoteDirAndWritesBothTargets(t *testing.T) {
	const (
		sid         = "sid-remote-1"
		projectsDir = "/home/u/.claude/projects"
		remoteDir   = projectsDir + "/-private-tmp-nova-agent-proj-req"
		computedDir = projectsDir + "/-tmp-nova-agent-proj-req"
	)
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	t.Setenv("NOVA_CLAUDE_HOME", "")

	// Local project-root slug dir (the slot the up-sync reads from).
	rootSlug := filepath.Join(home, "projects", "-Users-f1-project-novaworkbench")
	if err := os.MkdirAll(rootSlug, 0o755); err != nil {
		t.Fatal(err)
	}
	// The requirement's worktree — its slug dir does NOT exist yet, so the
	// down-sync has to create it.
	worktree := "/Users/f1/.novaworkbench/worktrees/novaworkbench/req_test"
	reqRow := &model.Requirement{ID: "req_test", ProjectID: "proj_1", WorktreePath: worktree}

	fake := &fakeSessionClient{
		files: map[string]string{
			remoteDir + "/" + sid + ".jsonl": "{\"transcript\":true}",
		},
		dirs:         map[string][]string{projectsDir: {"-private-tmp-nova-agent-proj-req"}},
		missingFiles: map[string]bool{},
	}
	h := &WizardHandler{projectSvc: testProjectSvc(t)}
	job := store.NewJobStore(4).Create("req_test")
	msg := h.doSyncSessionDown(context.Background(), false, fake, job, reqRow, computedDir, "/tmp/nova-agent/proj_1/req_test", []string{sid})

	if !strings.Contains(msg, "会话同步回本地") || !strings.Contains(msg, sid) {
		t.Fatalf("unexpected summary line: %q", msg)
	}
	// Both target dirs must hold the transcript.
	worktreeSlug := filepath.Join(home, "projects", util.EncodeClaudeSlug(worktree))
	for _, dir := range []string{rootSlug, worktreeSlug} {
		got, err := os.ReadFile(filepath.Join(dir, sid+".jsonl"))
		if err != nil {
			t.Fatalf("transcript missing from %s: %v", dir, err)
		}
		if string(got) != "{\"transcript\":true}" {
			t.Fatalf("transcript content in %s: %q", dir, got)
		}
	}
	// The download must have come from the DISCOVERED dir, not the computed one.
	if len(fake.downloads) == 0 {
		t.Fatal("expected at least one download")
	}
	for _, d := range fake.downloads {
		if !strings.HasPrefix(d, remoteDir+"/") {
			t.Fatalf("download came from the wrong dir: %q", d)
		}
	}
}

// TestDoSyncSessionDown_MissingSessionIsVisible pins the "no more silent
// loss" requirement: when the remote doesn't hold the transcript, the job
// log must say so explicitly instead of the phase line being the last thing
// the user sees.
func TestDoSyncSessionDown_MissingSessionIsVisible(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	t.Setenv("NOVA_CLAUDE_HOME", "")

	const (
		sid         = "sid-missing"
		projectsDir = "/home/u/.claude/projects"
		computedDir = projectsDir + "/-tmp-nova-agent-proj-req"
	)
	fake := &fakeSessionClient{files: map[string]string{}, dirs: map[string][]string{}, missingFiles: map[string]bool{}}
	h := &WizardHandler{projectSvc: testProjectSvc(t)}
	job := store.NewJobStore(4).Create("req_x")
	reqRow := &model.Requirement{ID: "req_x", ProjectID: "proj_1", WorktreePath: "/tmp/worktrees/req_x"}
	msg := h.doSyncSessionDown(context.Background(), false, fake, job, reqRow, computedDir, "/tmp/nova-agent/proj_1/req_x", []string{sid})

	if !strings.Contains(msg, sid) {
		t.Fatalf("summary must name the missing session: %q", msg)
	}
	if !strings.Contains(msg, "未找到") && !strings.Contains(msg, "缺失") {
		t.Fatalf("summary must report the miss: %q", msg)
	}
	if len(fake.downloads) != 0 {
		t.Fatalf("nothing should have been downloaded: %#v", fake.downloads)
	}
}

// TestClassifyRemoteSessionPreFlight pins the four-way up-sync verdict.
//
// The interesting case is "the local copy is gone but the remote still has
// the transcript": the remote CLI resumes its own file in place, so this is
// NOT a failure and must not be reported as one.
func TestClassifyRemoteSessionPreFlight(t *testing.T) {
	const (
		sid         = "sid-1"
		projectsDir = "/home/u/.claude/projects"
		computedDir = projectsDir + "/-tmp-nova-agent-proj-req"
	)
	remoteFile := computedDir + "/" + sid + ".jsonl"

	t.Run("remote has it and the local copy exists", func(t *testing.T) {
		fake := &fakeSessionClient{files: map[string]string{remoteFile: "{}"}, dirs: map[string][]string{}}
		line, side := (&WizardHandler{}).classifyRemoteSessionPreFlight(fake, []string{computedDir}, sid, nil)
		if side != "" {
			t.Fatalf("want no missing side, got %q (line=%q)", side, line)
		}
		if !strings.Contains(line, "就绪") {
			t.Fatalf("unexpected line %q", line)
		}
	})

	t.Run("local copy missing but the remote still has it — not a failure", func(t *testing.T) {
		fake := &fakeSessionClient{files: map[string]string{remoteFile: "{}"}, dirs: map[string][]string{}}
		line, side := (&WizardHandler{}).classifyRemoteSessionPreFlight(fake, []string{computedDir}, sid, []string{sid})
		if side != "" {
			t.Fatalf("want no missing side, got %q (line=%q)", side, line)
		}
		if !strings.Contains(line, "直接续接") {
			t.Fatalf("unexpected line %q", line)
		}
	})

	t.Run("nowhere to be found — local side", func(t *testing.T) {
		fake := &fakeSessionClient{files: map[string]string{}, dirs: map[string][]string{}}
		line, side := (&WizardHandler{}).classifyRemoteSessionPreFlight(fake, []string{computedDir}, sid, []string{sid})
		if side != "local" {
			t.Fatalf("want side=local, got %q (line=%q)", side, line)
		}
		if !strings.Contains(line, "本地未找到需求会话文件") {
			t.Fatalf("unexpected line %q", line)
		}
	})

	t.Run("remote missing but the local copy existed — remote side", func(t *testing.T) {
		fake := &fakeSessionClient{files: map[string]string{}, dirs: map[string][]string{}}
		line, side := (&WizardHandler{}).classifyRemoteSessionPreFlight(fake, []string{computedDir}, sid, nil)
		if side != "remote" {
			t.Fatalf("want side=remote, got %q (line=%q)", side, line)
		}
	})
}

// TestEnsureSessionAvailableLocally_FallbackToSessionServer pins the
// cross-server pull-back that makes "换一台 Agent Server 也能继续会话" work.
//
// Scenario: the requirement ran on server A, its transcript never came back
// to this machine, and the user now runs on server B. The up-sync has
// nothing to push, so the conversation would be lost — unless the session is
// fetched from A first (requirements.session_server_id names it).
func TestEnsureSessionAvailableLocally_FallbackToSessionServer(t *testing.T) {
	const (
		sid         = "sid-on-server-a"
		projectsDir = "/home/u/.claude/projects"
		remoteDir   = projectsDir + "/-tmp-nova-agent-proj_1-req_1"
	)
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	t.Setenv("NOVA_CLAUDE_HOME", "")

	// Local project-root slug dir exists but holds NO copy of the session.
	rootSlug := filepath.Join(home, "projects", "-Users-f1-project-novaworkbench")
	if err := os.MkdirAll(rootSlug, 0o755); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	d, err := db.Init(db.Config{Driver: "sqlite", SQLitePath: filepath.Join(dir, "test.db")})
	if err != nil {
		t.Fatalf("init db: %v", err)
	}
	defer d.Close()
	srvSvc := service.NewAgentServerService(d)
	srv, err := srvSvc.Create(model.CreateAgentServerReq{
		Name: "server-a", Host: "10.0.0.9", Port: 22, Username: "nova", AuthType: "key",
	})
	if err != nil {
		t.Fatalf("create agent server: %v", err)
	}

	// The dialer is faked: server A "holds" the transcript.
	fake := &fakeSessionClient{
		files:        map[string]string{remoteDir + "/" + sid + ".jsonl": "{\"from\":\"server-a\"}"},
		dirs:         map[string][]string{projectsDir: {"-tmp-nova-agent-proj_1-req_1"}},
		missingFiles: map[string]bool{},
	}
	orig := agentServerDialer
	agentServerDialer = func(ctx context.Context, s *model.AgentServer, authValue string) (remoteSessionClient, error) {
		return fake, nil
	}
	defer func() { agentServerDialer = orig }()

	h := &WizardHandler{
		projectSvc:  service.NewProjectService(d, nil, nil),
		agentSvrSvc: srvSvc,
	}
	job := store.NewJobStore(4).Create("req_1")
	reqRow := &model.Requirement{
		ID:              "req_1",
		ProjectID:       "proj_1",
		WorktreePath:    "/tmp/worktrees/req_1",
		SessionServerID: srv.ID,
	}
	h.ensureSessionAvailableLocally(context.Background(), job, reqRow, []string{sid}, "server-b")

	// The transcript must now be on this machine, in a local slug dir.
	found := false
	filepath.Walk(filepath.Join(home, "projects"), func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		if filepath.Base(path) == sid+".jsonl" {
			found = true
		}
		return nil
	})
	if !found {
		t.Fatalf("the session was not pulled back from server %s", srv.ID)
	}
	// The job log must tell the user where it came from.
	sawTrace := false
	for _, line := range job.Log {
		if strings.Contains(line.Content, "回源") && strings.Contains(line.Content, sid) {
			sawTrace = true
		}
	}
	if !sawTrace {
		t.Fatalf("job log must report the pull-back: %#v", job.Log)
	}
}

// TestEnsureSessionAvailableLocally_SameServerIsNotPulledBack pins the
// short-circuit: when the session server IS the server we are about to run
// on, the remote CLI resumes its own transcript in place — no pull-back.
func TestEnsureSessionAvailableLocally_SameServerIsNotPulledBack(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	t.Setenv("NOVA_CLAUDE_HOME", "")

	dialed := false
	orig := agentServerDialer
	agentServerDialer = func(ctx context.Context, s *model.AgentServer, authValue string) (remoteSessionClient, error) {
		dialed = true
		return nil, errors.New("must not dial")
	}
	defer func() { agentServerDialer = orig }()

	h := &WizardHandler{}
	h.ensureSessionAvailableLocally(context.Background(), nil, &model.Requirement{
		ID: "req_1", SessionServerID: "agent_same",
	}, []string{"sid-1"}, "agent_same")

	if dialed {
		t.Fatal("must not dial the server we are already running on")
	}
}

// TestPersistRemoteCodingSession pins the remote session bookkeeping.
//
// The remote path used to drop the CLI-reported session id entirely, so the
// row kept the pre-minted UUID. Today the CLI honours --session-id so the
// two coincide — but the moment that semantic drifts, the stored id resolves
// to a file that exists nowhere and the next round loses the whole context.
// The DB must follow what the CLI reported, and it must record which host
// holds the transcript so a server switch can find it again.
func TestPersistRemoteCodingSession(t *testing.T) {
	dir := t.TempDir()
	d, err := db.Init(db.Config{Driver: "sqlite", SQLitePath: filepath.Join(dir, "test.db")})
	if err != nil {
		t.Fatalf("init db: %v", err)
	}
	defer d.Close()
	reqID := seedRequirementForSchedule(t, d)
	reqSvc := service.NewRequirementService(d, nil)
	h := &WizardHandler{reqSvc: reqSvc}

	// The CLI reported a DIFFERENT id than the one persisted up front → the
	// row must follow the CLI, and the session server must be recorded.
	if err := reqSvc.UpdateCodingSession(reqID, "sid-preminted"); err != nil {
		t.Fatalf("seed coding session: %v", err)
	}
	h.persistRemoteCodingSession(reqID, "sid-reported", "agent_x")

	got, gerr := reqSvc.Get(reqID)
	if gerr != nil {
		t.Fatalf("get requirement: %v", gerr)
	}
	if got.CodingSessionID != "sid-reported" {
		t.Fatalf("coding_session_id: got %q, want %q", got.CodingSessionID, "sid-reported")
	}
	if got.SessionServerID != "agent_x" {
		t.Fatalf("session_server_id: got %q, want %q", got.SessionServerID, "agent_x")
	}

	// Same id as the one on the row → nothing to correct, but the server
	// pointer is still refreshed.
	h.persistRemoteCodingSession(reqID, "sid-reported", "agent_y")
	got, _ = reqSvc.Get(reqID)
	if got.CodingSessionID != "sid-reported" {
		t.Fatalf("coding_session_id changed unexpectedly: %q", got.CodingSessionID)
	}
	if got.SessionServerID != "agent_y" {
		t.Fatalf("session_server_id: got %q, want %q", got.SessionServerID, "agent_y")
	}
}
