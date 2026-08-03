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
	Name    string `json:"name"`
	Path    string `json:"path"`
	IsDir   bool   `json:"is_dir"`
	IsGit   bool   `json:"is_git"`
	Size    int64  `json:"size"`
}

type FsHandler struct{}

func NewFsHandler() *FsHandler { return &FsHandler{} }

func (h *FsHandler) ListDir(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		path = "/"
	}

	// Expand ~
	if strings.HasPrefix(path, "~") {
		home, _ := os.UserHomeDir()
		path = strings.Replace(path, "~", home, 1)
	}

	path = filepath.Clean(path)

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
	path := r.URL.Query().Get("path")
	if path == "" {
		writeError(w, http.StatusBadRequest, "INVALID_PATH", "path is required")
		return
	}

	// Expand ~
	if strings.HasPrefix(path, "~") {
		home, _ := os.UserHomeDir()
		path = strings.Replace(path, "~", home, 1)
	}

	path = filepath.Clean(path)

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
	path := r.URL.Query().Get("path")
	if path == "" {
		writeError(w, http.StatusBadRequest, "INVALID_PATH", "path is required")
		return
	}
	if strings.HasPrefix(path, "~") {
		home, _ := os.UserHomeDir()
		path = strings.Replace(path, "~", home, 1)
	}
	path = filepath.Clean(path)

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
