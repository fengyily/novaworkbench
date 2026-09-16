package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/novaworkbench/backend/internal/model"
)

// repoPattern validates an `owner/repo` (or `owner/repo` style skill name) so
// values can be passed as discrete exec args without shell interpolation.
var repoPattern = regexp.MustCompile(`^[\w.-]+/[\w.-]+$`)

// skillNamePattern validates an optional single skill name.
var skillNamePattern = regexp.MustCompile(`^[\w.-]+$`)

// localSkillDirs returns the directories scanned for command-installed skills:
// the global ~/.claude/skills (npx skills add -g target) plus, when provided,
// a project-local .claude/skills.
func localSkillDirs(projectPath string) []string {
	var dirs []string
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".claude", "skills"))
	}
	if projectPath != "" {
		dirs = append(dirs, filepath.Join(projectPath, ".claude", "skills"))
	}
	return dirs
}

// scanLocalSkills walks each dir for <dir>/*/SKILL.md and <dir>/*/*/SKILL.md,
// parses frontmatter via parseSkillMD, and returns MarketSkill entries deduped
// by slug (the SKILL.md's containing folder name). source_url holds the local
// absolute path so the UI can show where a skill came from.
func scanLocalSkills(dirs []string) []model.MarketSkill {
	seen := make(map[string]bool)
	var out []model.MarketSkill

	add := func(skillPath string) {
		dir := filepath.Dir(skillPath)
		slug := filepath.Base(dir)
		if slug == "" || seen[slug] {
			return
		}
		raw, err := os.ReadFile(skillPath)
		if err != nil {
			return
		}
		name, desc, content := parseSkillMD(string(raw))
		if content == "" {
			return
		}
		if name == "" {
			name = slug
		}
		seen[slug] = true
		out = append(out, model.MarketSkill{
			Name:        name,
			Slug:        slug,
			Description: desc,
			Content:     content,
			SourceURL:   skillPath,
		})
	}

	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			sub := filepath.Join(dir, e.Name())
			// Flat: <dir>/<slug>/SKILL.md
			if fi, err := os.Stat(filepath.Join(sub, "SKILL.md")); err == nil && !fi.IsDir() {
				add(filepath.Join(sub, "SKILL.md"))
				continue
			}
			// Nested: <dir>/<cat>/<slug>/SKILL.md
			nested, err := os.ReadDir(sub)
			if err != nil {
				continue
			}
			for _, n := range nested {
				if !n.IsDir() {
					continue
				}
				p := filepath.Join(sub, n.Name(), "SKILL.md")
				if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
					add(p)
				}
			}
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Installed returns skills discovered on the local machine (command-installed
// via `npx skills add`) so the user can integrate them into Nova.
func (h *SkillHandler) Installed(w http.ResponseWriter, r *http.Request) {
	skills := scanLocalSkills(localSkillDirs(r.URL.Query().Get("project_path")))
	if skills == nil {
		skills = []model.MarketSkill{}
	}
	writeJSON(w, 200, skills)
}

type installCommandReq struct {
	Repo  string `json:"repo"`
	Skill string `json:"skill"`
}

// InstallCommand runs `npx skills add <repo>` non-interactively to install a
// skill to ~/.claude/skills, then rescans and returns the discovered skills.
func (h *SkillHandler) InstallCommand(w http.ResponseWriter, r *http.Request) {
	var req installCommandReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "INVALID", "Invalid JSON: "+err.Error())
		return
	}
	req.Repo = strings.TrimSpace(req.Repo)
	req.Skill = strings.TrimSpace(req.Skill)
	if !repoPattern.MatchString(req.Repo) {
		writeError(w, 400, "INVALID", "repo 格式无效，应为 owner/repo")
		return
	}
	if req.Skill != "" && !skillNamePattern.MatchString(req.Skill) {
		writeError(w, 400, "INVALID", "skill 名称无效")
		return
	}

	// Build args as discrete values (never a shell string) to avoid injection.
	args := []string{"-y", "skills", "add", req.Repo}
	if req.Skill != "" {
		args = append(args, "--skill", req.Skill)
	}
	args = append(args, "--agent", "claude-code", "--global", "--copy", "--yes")

	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "npx", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		writeError(w, 502, "INSTALL_FAILED", "npx skills add 执行失败: "+msg)
		return
	}

	skills := scanLocalSkills(localSkillDirs(""))
	if skills == nil {
		skills = []model.MarketSkill{}
	}
	writeJSON(w, 200, skills)
}
