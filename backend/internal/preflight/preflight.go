// Package preflight checks whether the host has the runtime dependencies
// NovaWorkbench needs (Claude CLI, Node/npm for installing Claude, git, docker)
// and can optionally install missing ones. It runs at startup, surfaces a
// snapshot at GET /api/preflight, and streams install progress via the shared
// JobStore + SSE pattern (so the frontend can show a live "正在安装 Claude CLI..."
// panel).
//
// Design principles (mirrors the advisory style of llm.New at gateway.go:53):
//   - CheckAll always reports, never halts the server. The AI features already
//     degrade to stub responses when claude is missing.
//   - EnsureAll installs in the background, also non-blocking. The frontend
//     can trigger an explicit install via POST /api/preflight/install.
//   - Platform Install() implementations prefer user-space paths (brew on mac,
//     nvm on linux) before requiring elevated privileges.
package preflight

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/novaworkbench/backend/internal/store"
)

// Result is the snapshot returned by CheckAll and exposed at /api/preflight.
type Result struct {
	Key       string   `json:"key"`        // "claude" | "node" | "npm" | "git" | "docker"
	Label     string   `json:"label"`      // Chinese display label
	Installed bool     `json:"installed"`
	Version   string   `json:"version,omitempty"`
	Path      string   `json:"path,omitempty"`
	Required  bool     `json:"required"`
	DependsOn []string `json:"depends_on"`
	Err       string   `json:"err,omitempty"`        // human-readable failure
	Manual    string   `json:"manual,omitempty"`     // manual install hint when auto-install is unavailable
}

// ProgressSink is the narrow surface preflight uses to stream install logs
// into a JobStore Job (see internal/store/jobs.go). Any value with this
// signature works — only the wizard/runner/review handlers' *store.Job
// satisfies it today.
type ProgressSink interface {
	Append(line store.LogLine)
}

// Dep is one tracked runtime dependency.
type Dep struct {
	Key       string
	Label     string
	Required  bool
	DependsOn []string

	// Check probes the host for the binary. Returns the resolved absolute path
	// and a best-effort version string ("" if unknown). err != nil means the
	// dependency is missing.
	Check func(ctx context.Context) (path string, version string, err error)

	// Install performs a best-effort install. Each subprocess's stdout/stderr
	// lines should be streamed into sink so the frontend sees progress.
	// Returns nil on success.
	Install func(ctx context.Context, sink ProgressSink) error

	// Manual is the fallback hint shown in the UI when Install is nil
	// (unsupported platform) or fails.
	Manual string
}

// Registry holds the full set of tracked deps plus the claude binary override
// (mirrors the CLAUDE_BIN env the gateway reads at gateway.go:40).
type Registry struct {
	ClaudeBin string   // env override; empty = "claude" on PATH
	Deps      []*Dep
	mu        sync.RWMutex
	last      []Result
}

// New builds the default registry for the current platform. claudeBin is the
// CLAUDE_BIN env value ("" = use PATH).
func New(claudeBin string) *Registry {
	if strings.TrimSpace(claudeBin) == "" {
		claudeBin = "claude"
	}
	r := &Registry{ClaudeBin: claudeBin}
	r.Deps = []*Dep{
		r.depClaude(),
		r.depNode(),
		r.depNpm(),
		r.depGit(),
		r.depDocker(),
	}
	// attachInstalls sets Dep.Install per runtime.GOOS (kept in install.go
	// so the platform matrix stays in one place).
	r.attachInstalls()
	return r
}

// Snapshot returns a copy of the last CheckAll results. Safe for concurrent
// HTTP reads.
func (r *Registry) Snapshot() []Result {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Result, len(r.last))
	copy(out, r.last)
	return out
}

// Lookup returns the Dep with the given key (nil if unknown).
func (r *Registry) Lookup(key string) *Dep {
	for _, d := range r.Deps {
		if d.Key == key {
			return d
		}
	}
	return nil
}

// CheckAll probes every dep and returns the snapshot (also cached for later
// /api/preflight reads). Network-free — only LookPath + `--version`.
func (r *Registry) CheckAll(ctx context.Context) []Result {
	out := make([]Result, 0, len(r.Deps))
	for _, d := range r.Deps {
		res := Result{Key: d.Key, Label: d.Label, Required: d.Required, DependsOn: d.DependsOn, Manual: d.Manual}
		path, version, err := d.Check(ctx)
		if err != nil {
			res.Err = err.Error()
		} else {
			res.Installed = true
			res.Path = path
			res.Version = version
		}
		out = append(out, res)
	}
	r.mu.Lock()
	r.last = out
	r.mu.Unlock()
	return out
}

// EnsureAll installs any missing deps in dependency order. Runs in the
// foreground so the caller controls context cancellation. Streams lines into
// sink. Safe to call concurrently — installs are gated by the CheckAll result
// snapshot taken at call time.
func (r *Registry) EnsureAll(ctx context.Context, sink ProgressSink) {
	if sink == nil {
		sink = noopSink{}
	}
	results := r.Snapshot()
	visited := map[string]bool{}
	for _, res := range results {
		if res.Installed || res.Err == "" {
			continue
		}
		if err := r.installRecursive(ctx, res.Key, sink, visited); err != nil {
			sink.Append(store.LogLine{Type: "error", Content: fmt.Sprintf("[preflight] %s 安装失败: %v", res.Key, err)})
		}
	}
	r.CheckAll(ctx)
}

// Install runs the platform-specific install for one dep (and its deps).
// Returns the first error encountered.
func (r *Registry) Install(ctx context.Context, key string, sink ProgressSink) error {
	if sink == nil {
		sink = noopSink{}
	}
	visited := map[string]bool{}
	err := r.installRecursive(ctx, key, sink, visited)
	// Re-check so the next /api/preflight reads fresh state.
	r.CheckAll(ctx)
	return err
}

func (r *Registry) installRecursive(ctx context.Context, key string, sink ProgressSink, visited map[string]bool) error {
	if visited[key] {
		return nil
	}
	visited[key] = true
	dep := r.Lookup(key)
	if dep == nil {
		return fmt.Errorf("unknown dep: %s", key)
	}
	// Re-check before installing (another goroutine may have just installed it).
	if path, version, err := dep.Check(ctx); err == nil {
		sink.Append(store.LogLine{Type: "message", Content: fmt.Sprintf("[preflight] %s 已存在 (%s %s)", dep.Label, path, version)})
		return nil
	}
	if dep.Install == nil {
		return fmt.Errorf("%s 自动安装不支持当前平台 (%s)", dep.Label, runtime.GOOS)
	}
	// Install dependencies first.
	for _, depKey := range dep.DependsOn {
		if err := r.installRecursive(ctx, depKey, sink, visited); err != nil {
			return err
		}
	}
	sink.Append(store.LogLine{Type: "message", Content: fmt.Sprintf("[preflight] 开始安装 %s ...", dep.Label)})
	if err := dep.Install(ctx, sink); err != nil {
		return err
	}
	if path, version, err := dep.Check(ctx); err == nil {
		sink.Append(store.LogLine{Type: "message", Content: fmt.Sprintf("[preflight] ✅ %s 安装完成 (%s %s)", dep.Label, path, version)})
		return nil
	}
	return fmt.Errorf("%s 安装后仍无法探测到", dep.Label)
}

// ---- dep constructors -----------------------------------------------------

func (r *Registry) depClaude() *Dep {
	return &Dep{
		Key: "claude", Label: "Claude CLI", Required: true, DependsOn: []string{"npm"},
		Check:    checkBinary(r.ClaudeBin, "--version"),
		Manual:   "npm install -g @anthropic-ai/claude-code  （需先安装 Node.js 与 npm）",
	}
}

func (r *Registry) depNode() *Dep {
	return &Dep{
		Key: "node", Label: "Node.js", Required: true,
		Check:  checkBinary("node", "--version"),
		Manual: "请访问 https://nodejs.org/ 下载安装 LTS 版本；或使用 brew install node / apt-get install -y nodejs npm / winget install OpenJS.NodeJS",
	}
}

func (r *Registry) depNpm() *Dep {
	return &Dep{
		Key: "npm", Label: "npm", Required: true, DependsOn: []string{"node"},
		Check:  checkBinary("npm", "--version"),
		Manual: "随 Node.js 一起安装；如缺失请重装 Node.js",
	}
}

func (r *Registry) depGit() *Dep {
	return &Dep{
		Key: "git", Label: "Git", Required: false,
		Check:  checkBinary("git", "--version"),
		Manual: "请访问 https://git-scm.com/ 下载安装",
	}
}

func (r *Registry) depDocker() *Dep {
	return &Dep{
		Key: "docker", Label: "Docker (compose)", Required: false,
		// Just lookPath — running docker compose here would spin up the
		// daemon. The runner handler's detectComposeCmd probes it again at
		// use-time (runner.go:50).
		Check: func(ctx context.Context) (string, string, error) {
			p, err := exec.LookPath("docker")
			if err != nil {
				return "", "", errors.New("docker 未安装（运行会话功能不可用）")
			}
			return p, "", nil
		},
		Manual: "Docker 用于项目管理页的「启动会话」功能；非必需。安装：https://docs.docker.com/engine/install/",
	}
}

// ---- shared helpers -------------------------------------------------------

// checkBinary builds a Check fn that runs LookPath + `<bin> --version`.
// Falls back to the LookPath hit if the version probe fails (so a binary that
// rejects --version still counts as installed — better false-positive than
// masking a working CLI).
func checkBinary(bin string, versionArg string) func(ctx context.Context) (string, string, error) {
	return func(ctx context.Context) (string, string, error) {
		path, err := exec.LookPath(bin)
		if err != nil {
			return "", "", fmt.Errorf("%s 不在 PATH 中", bin)
		}
		// 5s is plenty for a --version probe; some CLIs hang on first run.
		vctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(vctx, path, versionArg)
		out, vErr := cmd.Output()
		version := strings.TrimSpace(firstLine(out))
		if vErr != nil && version == "" {
			// Binary exists but won't talk — still treat as installed so the
			// install flow doesn't loop forever on a buggy CLI.
			return path, "", nil
		}
		return path, version, nil
	}
}

func firstLine(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	for i, c := range b {
		if c == '\n' || c == '\r' {
			return string(b[:i])
		}
	}
	return string(b)
}

// runLogged execs cmd in the foreground, streaming each stdout+stderr line
// into sink prefixed with [label]. Uses bufio.Scanner with a large buffer
// (same defensive size as the SSE handlers — wizard.go's scanner.Buffer is
// 256K→4M).
func runLogged(ctx context.Context, sink ProgressSink, label string, cmd *exec.Cmd) error {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = cmd.Stdout // merge stderr into the same pipe
	if err := cmd.Start(); err != nil {
		return err
	}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		sink.Append(store.LogLine{Type: "message", Content: fmt.Sprintf("[%s] %s", label, line)})
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		// Don't fail the install just because the pipe was noisy; the
		// cmd.Wait below returns the real exit status.
		sink.Append(store.LogLine{Type: "message", Content: fmt.Sprintf("[%s] (scanner: %v)", label, err)})
	}
	return cmd.Wait()
}

// noopSink is used when EnsureAll/Install is called with a nil sink (e.g.
// during startup when no JobStore exists yet).
type noopSink struct{}

func (noopSink) Append(store.LogLine) {}

// platformBin returns the absolute path to a binary on the current platform
// (Windows resolves `npm` to `npm.cmd`, which exec.Command cannot run without
// the .cmd extension from Go 1.19+ — but using the full LookPath-resolved
// path avoids that). Falls back to the bare name on lookup failure.
func platformBin(name string) string {
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	return name
}
