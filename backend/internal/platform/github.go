package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type githubClient struct {
	token string
}

func (c *githubClient) parseRepo(repoURL string) (owner, repo string, err error) {
	// Accepts: https://github.com/owner/repo.git  or  git@github.com:owner/repo.git
	url := strings.TrimSuffix(repoURL, ".git")
	if strings.HasPrefix(url, "git@github.com:") {
		parts := strings.SplitN(strings.TrimPrefix(url, "git@github.com:"), "/", 2)
		if len(parts) != 2 {
			return "", "", fmt.Errorf("cannot parse GitHub SSH URL: %s", repoURL)
		}
		return parts[0], parts[1], nil
	}
	// HTTPS
	idx := strings.Index(url, "github.com/")
	if idx < 0 {
		return "", "", fmt.Errorf("not a GitHub URL: %s", repoURL)
	}
	parts := strings.SplitN(url[idx+len("github.com/"):], "/", 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("cannot parse GitHub URL: %s", repoURL)
	}
	return parts[0], parts[1], nil
}

func (c *githubClient) do(ctx context.Context, method, url string, body string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "token "+c.token)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	return http.DefaultClient.Do(req)
}

func (c *githubClient) ListOpenPRs(ctx context.Context, repoURL string) ([]PR, error) {
	owner, repo, err := c.parseRepo(repoURL)
	if err != nil {
		return nil, err
	}

	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/pulls?state=open&per_page=50", owner, repo)
	resp, err := c.do(ctx, http.MethodGet, apiURL, "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API error: %s", resp.Status)
	}

	var raw []struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		Body   string `json:"body"`
		State  string `json:"state"`
		User   struct {
			Login string `json:"login"`
		} `json:"user"`
		Head struct {
			Ref string `json:"ref"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
		HTMLURL   string    `json:"html_url"`
		UpdatedAt time.Time `json:"updated_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}

	prs := make([]PR, 0, len(raw))
	for _, r := range raw {
		prs = append(prs, PR{
			Number:     r.Number,
			Title:      r.Title,
			Body:       r.Body,
			Author:     r.User.Login,
			HeadBranch: r.Head.Ref,
			BaseBranch: r.Base.Ref,
			State:      r.State,
			HTMLURL:    r.HTMLURL,
			UpdatedAt:  r.UpdatedAt,
		})
	}
	return prs, nil
}

func (c *githubClient) SubmitComment(ctx context.Context, repoURL string, prNumber int, body string) error {
	// SubmitComment is the legacy PR-comment entry point; the underlying
	// endpoint is identical for both PR comments and Issue comments
	// (GitHub intentionally exposes /issues/{n}/comments for both, and
	// PRs are themselves issues under the hood). Delegate to the
	// generically-named PostIssueComment so the two share one code path.
	return c.PostIssueComment(ctx, repoURL, prNumber, body)
}

func (c *githubClient) CreatePR(ctx context.Context, repoURL string, base, head, title, body string) (*PR, error) {
	owner, repo, err := c.parseRepo(repoURL)
	if err != nil {
		return nil, err
	}

	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/pulls", owner, repo)
	payload, _ := json.Marshal(map[string]string{"title": title, "head": head, "base": base, "body": body})
	resp, err := c.do(ctx, http.MethodPost, apiURL, string(payload))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("GitHub API error: %s%s", resp.Status, readErrBody(resp))
	}

	var raw struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}
	return &PR{Number: raw.Number, HTMLURL: raw.HTMLURL}, nil
}

// ListIssues returns up to 50 open issues for repoURL filtered by an
// optional GitHub Label. The GitHub API treats PRs as issues, so a row
// with `pull_request != nil` is filtered out explicitly — without this
// the monitor would try to import PRs as Issue-kind requirements. Empty
// label means "no filter" (the API ignores the parameter when blank).
//
// Pre-v0.5.x there was no Issue-level operation in this package, so
// we're intentionally NOT introducing a separate "issues" file — see
// platform.go for the corresponding Client interface.
func (c *githubClient) ListIssues(ctx context.Context, repoURL, label string) ([]Issue, error) {
	owner, repo, err := c.parseRepo(repoURL)
	if err != nil {
		return nil, err
	}

	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/issues?state=open&per_page=50", owner, repo)
	if label != "" {
		// Use github's labels filter (comma-separated for OR). v0.5.x
		// single-label only; multi-label OR is a documented open item.
		apiURL += "&labels=" + url.QueryEscape(label)
	}
	resp, err := c.do(ctx, http.MethodGet, apiURL, "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API error: %s", resp.Status)
	}

	var raw []struct {
		Number      int       `json:"number"`
		Title       string    `json:"title"`
		Body        string    `json:"body"`
		State       string    `json:"state"`
		UpdatedAt   time.Time `json:"updated_at"`
		HTMLURL     string    `json:"html_url"`
		PullRequest *struct{} `json:"pull_request"` // nil ⇒ real issue; non-nil ⇒ it's a PR
		User        struct {
			Login string `json:"login"`
		} `json:"user"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}

	issues := make([]Issue, 0, len(raw))
	for _, r := range raw {
		// Defensive filter: GitHub exposes PRs through /issues and PRs
		// carry a non-null pull_request object. Without this guard the
		// monitor would import PRs as Issue-kind requirements.
		if r.PullRequest != nil {
			continue
		}
		labels := make([]string, 0, len(r.Labels))
		for _, l := range r.Labels {
			labels = append(labels, l.Name)
		}
		issues = append(issues, Issue{
			Number:    r.Number,
			Title:     r.Title,
			Body:      r.Body,
			Author:    r.User.Login,
			Labels:    labels,
			State:     r.State,
			HTMLURL:   r.HTMLURL,
			UpdatedAt: r.UpdatedAt,
		})
	}
	return issues, nil
}

// PostIssueComment posts `body` as a comment on the issue (or PR) with
// the given number. The GitHub API uses the same `/issues/{n}/comments`
// endpoint for both, so this also serves the legacy PR-comment path
// (SubmitComment above forwards here).
func (c *githubClient) PostIssueComment(ctx context.Context, repoURL string, issueNumber int, body string) error {
	owner, repo, err := c.parseRepo(repoURL)
	if err != nil {
		return err
	}

	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/issues/%d/comments", owner, repo, issueNumber)
	payload, _ := json.Marshal(map[string]string{"body": body})
	resp, err := c.do(ctx, http.MethodPost, apiURL, string(payload))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("GitHub API error: %s", resp.Status)
	}
	return nil
}
