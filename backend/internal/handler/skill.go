package handler

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
)

const marketCacheTTL = 10 * time.Minute

type marketCacheEntry struct {
	skills    []model.MarketSkill
	cachedAt  time.Time
}

// builtinMarkets is the curated list of skill markets shown in the UI.
var builtinMarkets = []model.SkillMarket{
	{
		ID:          "anthropics",
		Name:        "Anthropic Official",
		Description: "Anthropic 官方 Skills 仓库",
		RepoURL:     "https://github.com/anthropics/skills",
	},
	{
		ID:          "obra-superpowers",
		Name:        "Superpowers",
		Description: "obra 的 agentic skills 框架",
		RepoURL:     "https://github.com/obra/superpowers",
	},
	{
		ID:          "mattpocock",
		Name:        "Matt Pocock Skills",
		Description: "Matt Pocock 的工程实践 Skills",
		RepoURL:     "https://github.com/mattpocock/skills",
	},
	{
		ID:          "karpathy",
		Name:        "Karpathy Guidelines",
		Description: "Andrej Karpathy 编码准则",
		RepoURL:     "https://github.com/multica-ai/andrej-karpathy-skills",
	},
}

type SkillHandler struct {
	svc        *service.SkillService
	httpClient *http.Client
	cacheMu    sync.RWMutex
	cache      map[string]marketCacheEntry
}

func NewSkillHandler(svc *service.SkillService) *SkillHandler {
	return &SkillHandler{
		svc:        svc,
		httpClient: &http.Client{Timeout: 20 * time.Second},
		cache:      make(map[string]marketCacheEntry),
	}
}

func (h *SkillHandler) List(w http.ResponseWriter, r *http.Request) {
	skills, err := h.svc.List()
	if err != nil {
		writeError(w, 500, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, 200, skills)
}

func (h *SkillHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req model.CreateSkillReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "INVALID", "Invalid JSON: "+err.Error())
		return
	}
	if req.Name == "" || req.Slug == "" || req.Content == "" {
		writeError(w, 400, "INVALID", "name, slug, content 不能为空")
		return
	}
	skill, err := h.svc.Create(req)
	if err != nil {
		writeError(w, 500, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, 201, skill)
}

func (h *SkillHandler) Update(w http.ResponseWriter, r *http.Request) {
	var req model.UpdateSkillReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "INVALID", "Invalid JSON: "+err.Error())
		return
	}
	skill, err := h.svc.Update(r.PathValue("id"), req)
	if err != nil {
		writeError(w, 500, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, 200, skill)
}

func (h *SkillHandler) Delete(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.Delete(r.PathValue("id")); err != nil {
		writeError(w, 500, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

// Markets returns the curated list of builtin skill markets.
func (h *SkillHandler) Markets(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, builtinMarkets)
}

// Market fetches skills from a GitHub repo (via GitHub API) or a custom
// manifest URL. Query params: `market` (builtin market ID) or `registry`
// (custom GitHub repo URL or raw manifest URL).
func (h *SkillHandler) Market(w http.ResponseWriter, r *http.Request) {
	repoURL := ""

	if marketID := r.URL.Query().Get("market"); marketID != "" {
		for _, m := range builtinMarkets {
			if m.ID == marketID {
				repoURL = m.RepoURL
				break
			}
		}
		if repoURL == "" {
			writeError(w, 400, "INVALID", "未知市场: "+marketID)
			return
		}
	} else if registry := r.URL.Query().Get("registry"); registry != "" {
		repoURL = registry
	}

	if repoURL == "" {
		writeJSON(w, 200, []model.MarketSkill{})
		return
	}

	cacheKey := repoURL

	// Check cache first.
	h.cacheMu.RLock()
	if entry, ok := h.cache[cacheKey]; ok && time.Since(entry.cachedAt) < marketCacheTTL {
		h.cacheMu.RUnlock()
		writeJSON(w, 200, entry.skills)
		return
	}
	h.cacheMu.RUnlock()

	// Detect GitHub repo URL and use the GitHub API.
	owner, repo, ok := parseGitHubRepo(repoURL)
	if ok {
		skills, err := h.fetchGitHubSkills(owner, repo)
		if err != nil {
			writeError(w, 502, "REGISTRY_ERROR", err.Error())
			return
		}
		h.setCache(cacheKey, skills)
		writeJSON(w, 200, skills)
		return
	}

	// Fall back to raw manifest JSON URL.
	skills, err := h.fetchManifest(repoURL)
	if err != nil {
		writeError(w, 502, "REGISTRY_ERROR", err.Error())
		return
	}
	h.setCache(cacheKey, skills)
	writeJSON(w, 200, skills)
}

func (h *SkillHandler) setCache(key string, skills []model.MarketSkill) {
	h.cacheMu.Lock()
	h.cache[key] = marketCacheEntry{skills: skills, cachedAt: time.Now()}
	h.cacheMu.Unlock()
}

// parseGitHubRepo extracts owner and repo from a GitHub URL.
func parseGitHubRepo(u string) (owner, repo string, ok bool) {
	u = strings.TrimSuffix(u, "/")
	u = strings.TrimPrefix(u, "https://github.com/")
	u = strings.TrimPrefix(u, "http://github.com/")
	parts := strings.SplitN(u, "/", 2)
	if len(parts) == 2 && parts[0] != "" && parts[1] != "" &&
		!strings.Contains(parts[1], "/") {
		return parts[0], parts[1], true
	}
	return "", "", false
}

// githubEntry is a single item from the GitHub contents API.
type githubEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Type string `json:"type"` // "file" or "dir"
}

// fetchGitHubSkills walks <repo>/skills/ via the GitHub API concurrently and
// returns all found SKILL.md files as MarketSkill entries. Handles both flat
// (skills/<name>/SKILL.md) and one-level-nested (skills/<cat>/<name>/SKILL.md).
func (h *SkillHandler) fetchGitHubSkills(owner, repo string) ([]model.MarketSkill, error) {
	entries, err := h.githubList(owner, repo, "skills")
	if err != nil {
		return nil, err
	}

	// sem limits concurrent GitHub API calls to avoid rate-limiting.
	sem := make(chan struct{}, 8)
	var mu sync.Mutex
	var wg sync.WaitGroup
	var skills []model.MarketSkill

	for _, e := range entries {
		if e.Type != "dir" {
			continue
		}
		wg.Add(1)
		go func(entry githubEntry) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			// Try flat: skills/<name>/SKILL.md
			if sk, ok := h.fetchSkillFile(owner, repo, entry.Path+"/SKILL.md", entry.Path); ok {
				mu.Lock()
				skills = append(skills, sk)
				mu.Unlock()
				return
			}

			// Try nested: list subdirectories, then fetch each SKILL.md in parallel.
			subs, err := h.githubList(owner, repo, entry.Path)
			if err != nil {
				return
			}
			var subWg sync.WaitGroup
			for _, s := range subs {
				if s.Type != "dir" {
					continue
				}
				subWg.Add(1)
				go func(sub githubEntry) {
					defer subWg.Done()
					sem <- struct{}{}
					defer func() { <-sem }()
					if sk, ok := h.fetchSkillFile(owner, repo, sub.Path+"/SKILL.md", sub.Path); ok {
						mu.Lock()
						skills = append(skills, sk)
						mu.Unlock()
					}
				}(s)
			}
			subWg.Wait()
		}(e)
	}

	wg.Wait()
	return skills, nil
}

// githubList calls the GitHub contents API for a directory path.
// Uses GITHUB_TOKEN env var when set to raise the rate limit from 60 to 5000 req/hr.
func (h *SkillHandler) githubList(owner, repo, path string) ([]githubEntry, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/contents/%s", owner, repo, path)
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := h.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("GitHub API 请求频率超限，请设置 GITHUB_TOKEN 环境变量后重试")
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GitHub API %s: %s", path, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	var entries []githubEntry
	if err := json.Unmarshal(body, &entries); err != nil {
		return nil, err
	}
	return entries, nil
}

// fetchSkillFile fetches SKILL.md via raw.githubusercontent.com (no auth, no
// rate limit) and parses frontmatter. dirPath is used to derive the source URL.
func (h *SkillHandler) fetchSkillFile(owner, repo, filePath, dirPath string) (model.MarketSkill, bool) {
	rawURL := fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/main/%s", owner, repo, filePath)
	resp, err := h.httpClient.Get(rawURL)
	if err != nil || resp.StatusCode != 200 {
		if resp != nil {
			resp.Body.Close()
		}
		return model.MarketSkill{}, false
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 512<<10))
	if err != nil {
		return model.MarketSkill{}, false
	}
	name, desc, content := parseSkillMD(string(body))
	if content == "" {
		return model.MarketSkill{}, false
	}
	parts := strings.Split(strings.TrimSuffix(dirPath, "/"), "/")
	slug := parts[len(parts)-1]
	if name == "" {
		name = slug
	}
	return model.MarketSkill{
		Name:        name,
		Slug:        slug,
		Description: desc,
		Content:     content,
		SourceURL:   fmt.Sprintf("https://github.com/%s/%s/tree/main/%s", owner, repo, dirPath),
	}, true
}

// parseSkillMD extracts name, description, and body from a SKILL.md file.
// Frontmatter is delimited by --- lines at the top of the file.
func parseSkillMD(raw string) (name, description, content string) {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, "---") {
		return "", "", raw
	}
	// Find closing ---
	rest := raw[3:]
	if idx := strings.Index(rest, "\n---"); idx == -1 {
		return "", "", raw
	} else {
		fm := strings.TrimSpace(rest[:idx])
		content = strings.TrimSpace(rest[idx+4:])
		for _, line := range strings.Split(fm, "\n") {
			line = strings.TrimSpace(line)
			if after, ok := strings.CutPrefix(line, "name:"); ok {
				name = strings.TrimSpace(after)
			} else if after, ok := strings.CutPrefix(line, "description:"); ok {
				description = strings.TrimSpace(after)
			}
		}
	}
	return name, description, content
}

// fetchManifest fetches a raw JSON manifest URL.
func (h *SkillHandler) fetchManifest(url string) ([]model.MarketSkill, error) {
	resp, err := h.httpClient.Get(url)
	if err != nil {
		return []model.MarketSkill{}, nil
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusForbidden {
		return []model.MarketSkill{}, nil
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("registry 返回错误状态: %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	var skills []model.MarketSkill
	if err := json.Unmarshal(body, &skills); err != nil {
		return nil, fmt.Errorf("registry 返回格式无效: %w", err)
	}
	return skills, nil
}
