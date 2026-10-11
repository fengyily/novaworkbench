package handler

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

type FileItem struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	IsDir bool   `json:"is_dir"`
	IsGit bool   `json:"is_git"`
	Size  int64  `json:"size"`
}

// LocalPathProvider returns every active project's local_path. The handler
// treats these as the only allowed roots (besides ~/workspace). Returning
// nil is safe (only ~/workspace will be allowed).
type LocalPathProvider func() []string

type FsHandler struct {
	localPaths LocalPathProvider
}

func NewFsHandler(localPaths LocalPathProvider) *FsHandler {
	if localPaths == nil {
		localPaths = func() []string { return nil }
	}
	return &FsHandler{localPaths: localPaths}
}

func (h *FsHandler) ListDir(w http.ResponseWriter, r *http.Request) {
	rawPath := r.URL.Query().Get("path")
	path, errCode, errMsg := h.resolveAllowedPath(rawPath)
	if errCode != "" {
		writeError(w, http.StatusForbidden, errCode, errMsg)
		return
	}

	entries, err := os.ReadDir(path)
	if err != nil {
		writeError(w, http.StatusBadRequest, "READ_DIR_FAILED", err.Error())
		return
	}

	items := make([]FileItem, 0)
	for _, e := range entries {
		fullPath := filepath.Join(path, e.Name())

		// Only show directories
		if !e.IsDir() {
			continue
		}

		item := FileItem{
			Name:  e.Name(),
			Path:  fullPath,
			IsDir: true,
		}

		// Check if directory is a git repo
		if _, err := os.Stat(filepath.Join(fullPath, ".git")); err == nil {
			item.IsGit = true
		}

		items = append(items, item)
	}

	// Sort: dirs first, then by name
	sort.Slice(items, func(i, j int) bool {
		if items[i].IsDir != items[j].IsDir {
			return items[i].IsDir
		}
		return strings.ToLower(items[i].Name) < strings.ToLower(items[j].Name)
	})

	// Add parent directory entry (unless at root)
	if path != "/" {
		parent := FileItem{
			Name:  "..",
			Path:  filepath.Dir(path),
			IsDir: true,
		}
		items = append([]FileItem{parent}, items...)
	}

	// Add common roots for navigation
	breadcrumb := buildBreadcrumb(path)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"current":    path,
		"items":      items,
		"breadcrumb": breadcrumb,
	})
}

func (h *FsHandler) ValidatePath(w http.ResponseWriter, r *http.Request) {
	rawPath := r.URL.Query().Get("path")
	path, errCode, errMsg := h.resolveAllowedPath(rawPath)
	if errCode != "" {
		status := http.StatusForbidden
		if errCode == "INVALID_PATH" {
			status = http.StatusBadRequest
		}
		writeError(w, status, errCode, errMsg)
		return
	}

	info, err := os.Stat(path)
	if err != nil {
		writeError(w, http.StatusBadRequest, "PATH_NOT_FOUND", "Path does not exist: "+path)
		return
	}

	isGit := false
	if info.IsDir() {
		if _, err := os.Stat(filepath.Join(path, ".git")); err == nil {
			isGit = true
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"path":   path,
		"exists": true,
		"is_dir": info.IsDir(),
		"is_git": isGit,
		"name":   filepath.Base(path),
	})
}

func buildBreadcrumb(path string) []FileItem {
	parts := strings.Split(path, string(os.PathSeparator))
	var items []FileItem
	current := ""
	for _, p := range parts {
		if p == "" {
			current = "/"
			items = append(items, FileItem{Name: "/", Path: "/", IsDir: true})
			continue
		}
		current = filepath.Join(current, p)
		items = append(items, FileItem{Name: p, Path: current, IsDir: true})
	}
	return items
}

// GitBranches returns local + remote branches for a git repo at the given path.
// GET /api/fs/git-branches?path=...
func (h *FsHandler) GitBranches(w http.ResponseWriter, r *http.Request) {
	rawPath := r.URL.Query().Get("path")
	path, errCode, errMsg := h.resolveAllowedPath(rawPath)
	if errCode != "" {
		status := http.StatusForbidden
		if errCode == "INVALID_PATH" {
			status = http.StatusBadRequest
		}
		writeError(w, status, errCode, errMsg)
		return
	}

	out, err := exec.Command("git", "-C", path, "branch", "-a", "--format=%(refname:short)").Output()
	if err != nil {
		writeError(w, http.StatusBadRequest, "GIT_ERROR", "git branch failed: "+err.Error())
		return
	}

	seen := map[string]bool{}
	var branches []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		b := strings.TrimSpace(line)
		if b == "" {
			continue
		}
		// Normalise remote tracking refs: origin/foo -> foo (keep as remote label)
		// We'll return both forms but deduplicate
		if !seen[b] {
			seen[b] = true
			branches = append(branches, b)
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{"branches": branches})
}

// resolveAllowedPath expands ~, runs filepath.Abs + Clean + EvalSymlinks, then
// verifies the resolved absolute path is inside ~/workspace or one of the
// registered project local_paths. Returns (resolvedPath, "", "") on success;
// on failure returns ("", errCode, errMsg) for use with writeError.
func (h *FsHandler) resolveAllowedPath(rawPath string) (string, string, string) {
	if rawPath == "" {
		rawPath = "/"
	}
	// Expand a single leading "~" or "~/..." into the user's home directory.
	if strings.HasPrefix(rawPath, "~") {
		home, err := os.UserHomeDir()
		if err == nil {
			rest := strings.TrimPrefix(rawPath, "~")
			if rest == "" || strings.HasPrefix(rest, "/") {
				rawPath = home + rest
			} else {
				rawPath = filepath.Join(home, rest)
			}
		}
	}
	// Reject NUL bytes — they can confuse exec.Command.
	if strings.ContainsRune(rawPath, 0) {
		return "", "INVALID_PATH", "路径包含非法字符"
	}
	abs, err := filepath.Abs(rawPath)
	if err != nil {
		return "", "INVALID_PATH", "无法规范化路径: " + err.Error()
	}
	abs = filepath.Clean(abs)

	// Build the allow-list (both lexical and symlink-resolved forms so that
	// a non-existing target can still match against the lexical form).
	roots := h.collectRoots()

	// For an existing target, the symlink-resolved check is authoritative —
	// it catches ~/workspace/link -> /etc style escapes. Lexical comparison
	// alone would let a symlink under workspace pass even though it points
	// outside.
	if _, statErr := os.Lstat(abs); statErr == nil {
		if resolved, err := filepath.EvalSymlinks(abs); err == nil {
			resolved = filepath.Clean(resolved)
			if isUnderAnyRoot(resolved, roots) {
				return resolved, "", ""
			}
			return "", "PATH_NOT_ALLOWED",
				"禁止访问该目录。仅允许浏览 ~/workspace 与已登记的项目目录。"
		}
		// EvalSymlinks failed on an existing path; fall through to lexical.
	}

	// Lexical fallback: handles non-existing targets (ValidatePath on a not
	// yet-created directory, ~/workspace/../foo when one segment is absent).
	if isUnderAnyRoot(abs, roots) {
		return abs, "", ""
	}
	return "", "PATH_NOT_ALLOWED",
		"禁止访问该目录。仅允许浏览 ~/workspace 与已登记的项目目录。"
}

// isUnderAnyRoot checks whether path equals or sits inside any of the roots
// using filepath.Rel so that /workspace2 cannot match /workspace.
func isUnderAnyRoot(path string, roots []string) bool {
	for _, r := range roots {
		r = filepath.Clean(r)
		if r == "" {
			continue
		}
		rel, err := filepath.Rel(r, path)
		if err != nil {
			continue
		}
		// Reject ".."-leading results (path is outside r).
		if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			continue
		}
		return true
	}
	return false
}

// collectRoots builds the allow-list of every path the handler will accept.
// Each root is included in both its lexical and (when the path exists) its
// symlink-resolved form so that callers with either form can match. Duplicate
// entries are skipped. ~/workspace is included first.
func (h *FsHandler) collectRoots() []string {
	roots := []string{}
	seen := map[string]bool{}
	addRoot := func(p string) {
		p = strings.TrimSpace(p)
		if p == "" {
			return
		}
		abs, err := filepath.Abs(p)
		if err != nil {
			return
		}
		lex := filepath.Clean(abs)
		if !seen[lex] {
			seen[lex] = true
			roots = append(roots, lex)
		}
		if resolved, err := filepath.EvalSymlinks(lex); err == nil {
			r := filepath.Clean(resolved)
			if !seen[r] {
				seen[r] = true
				roots = append(roots, r)
			}
		}
	}
	home, _ := os.UserHomeDir()
	if home != "" {
		addRoot(filepath.Join(home, "workspace"))
	}
	for _, p := range h.localPaths() {
		addRoot(p)
	}
	return roots
}