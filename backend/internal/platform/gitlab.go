package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type gitlabClient struct {
	baseURL string
	token   string
}

// parseProjectPath extracts "owner/repo" from a GitLab remote URL and URL-encodes it.
func (c *gitlabClient) parseProjectPath(repoURL string) (string, error) {
	u := strings.TrimSuffix(repoURL, ".git")

	// SSH: git@gitlab.com:owner/repo
	if strings.HasPrefix(u, "git@") {
		colon := strings.Index(u, ":")
		if colon < 0 {
			return "", fmt.Errorf("cannot parse GitLab SSH URL: %s", repoURL)
		}
		return url.PathEscape(u[colon+1:]), nil
	}

	// HTTPS: https://gitlab.com/owner/repo or https://self-hosted/owner/repo
	parsed, err := url.Parse(u)
	if err != nil {
		return "", err
	}
	path := strings.TrimPrefix(parsed.Path, "/")
	if path == "" {
		return "", fmt.Errorf("cannot parse GitLab URL: %s", repoURL)
	}
	return url.PathEscape(path), nil
}

func (c *gitlabClient) do(ctx context.Context, method, apiURL, body string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, apiURL, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		req.Header.Set("PRIVATE-TOKEN", c.token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	return http.DefaultClient.Do(req)
}

func (c *gitlabClient) ListOpenPRs(ctx context.Context, repoURL string) ([]PR, error) {
	project, err := c.parseProjectPath(repoURL)
	if err != nil {
		return nil, err
	}

	apiURL := fmt.Sprintf("%s/api/v4/projects/%s/merge_requests?state=opened&per_page=50", c.baseURL, project)
	resp, err := c.do(ctx, http.MethodGet, apiURL, "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitLab API error: %s", resp.Status)
	}

	var raw []struct {
		IID    int    `json:"iid"`
		Title  string `json:"title"`
		Desc   string `json:"description"`
		State  string `json:"state"`
		Author struct {
			Username string `json:"username"`
		} `json:"author"`
		SourceBranch string    `json:"source_branch"`
		TargetBranch string    `json:"target_branch"`
		WebURL       string    `json:"web_url"`
		UpdatedAt    time.Time `json:"updated_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}

	prs := make([]PR, 0, len(raw))
	for _, r := range raw {
		prs = append(prs, PR{
			Number:     r.IID,
			Title:      r.Title,
			Body:       r.Desc,
			Author:     r.Author.Username,
			HeadBranch: r.SourceBranch,
			BaseBranch: r.TargetBranch,
			State:      r.State,
			HTMLURL:    r.WebURL,
			UpdatedAt:  r.UpdatedAt,
		})
	}
	return prs, nil
}

func (c *gitlabClient) SubmitComment(ctx context.Context, repoURL string, prNumber int, body string) error {
	project, err := c.parseProjectPath(repoURL)
	if err != nil {
		return err
	}

	apiURL := fmt.Sprintf("%s/api/v4/projects/%s/merge_requests/%d/notes", c.baseURL, project, prNumber)
	payload, _ := json.Marshal(map[string]string{"body": body})
	resp, err := c.do(ctx, http.MethodPost, apiURL, string(payload))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("GitLab API error: %s", resp.Status)
	}
	return nil
}

func (c *gitlabClient) CreatePR(ctx context.Context, repoURL string, base, head, title, body string) (*PR, error) {
	project, err := c.parseProjectPath(repoURL)
	if err != nil {
		return nil, err
	}

	apiURL := fmt.Sprintf("%s/api/v4/projects/%s/merge_requests", c.baseURL, project)
	payload, _ := json.Marshal(map[string]string{
		"title":         title,
		"source_branch": head,
		"target_branch": base,
		"description":   body,
	})
	resp, err := c.do(ctx, http.MethodPost, apiURL, string(payload))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("GitLab API error: %s%s", resp.Status, readErrBody(resp))
	}

	var raw struct {
		IID    int    `json:"iid"`
		WebURL string `json:"web_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}
	return &PR{Number: raw.IID, HTMLURL: raw.WebURL}, nil
}

// ListIssues / PostIssueComment: v0.5.x ships GitHub-only. GitLab stubs
// return a typed error and log a one-line warning so an operator
// clicking "Issue Monitor" on a GitLab project sees a clear message in
// both the backend log and the project error envelope. The endpoints
// (`/api/v4/projects/{p}/issues` and `/api/v4/projects/{p}/issues/{n}/notes`)
// are well-defined; implementing them is left for a follow-up release.
func (c *gitlabClient) ListIssues(ctx context.Context, repoURL, label string) ([]Issue, error) {
	log.Printf("[platform/gitlab] ListIssues not yet implemented (v0.5.x GitHub-only); repo=%s label=%q", repoURL, label)
	return nil, errors.New("gitlab ListIssues is not implemented in v0.5.x (GitHub-only)")
}

func (c *gitlabClient) PostIssueComment(ctx context.Context, repoURL string, issueNumber int, body string) error {
	log.Printf("[platform/gitlab] PostIssueComment not yet implemented (v0.5.x GitHub-only); repo=%s issue=%d", repoURL, issueNumber)
	return errors.New("gitlab PostIssueComment is not implemented in v0.5.x (GitHub-only)")
}

// CreateIssue opens a new Issue via POST /api/v4/projects/{p}/issues.
// Unlike ListIssues/PostIssueComment (stubbed above because the monitor
// polls them continuously), creation is a single user-initiated call, so
// it ships fully implemented on all three platforms. GitLab takes label
// NAMES as a comma-separated string — unlike Gitea, no id lookup needed.
func (c *gitlabClient) CreateIssue(ctx context.Context, repoURL, title, body string, labels []string) (*Issue, error) {
	project, err := c.parseProjectPath(repoURL)
	if err != nil {
		return nil, err
	}

	payload := map[string]any{"title": title, "description": body}
	if len(labels) > 0 {
		payload["labels"] = strings.Join(labels, ",")
	}
	raw, _ := json.Marshal(payload)

	apiURL := fmt.Sprintf("%s/api/v4/projects/%s/issues", c.baseURL, project)
	resp, err := c.do(ctx, http.MethodPost, apiURL, string(raw))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("GitLab API error: %s%s", resp.Status, readErrBody(resp))
	}

	var out struct {
		IID       int      `json:"iid"`
		Title     string   `json:"title"`
		Desc      string   `json:"description"`
		State     string   `json:"state"`
		WebURL    string   `json:"web_url"`
		Author    struct {
			Username string `json:"username"`
		} `json:"author"`
		Labels []string `json:"labels"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if out.Labels == nil {
		out.Labels = []string{}
	}
	return &Issue{
		Number:  out.IID,
		Title:   out.Title,
		Body:    out.Desc,
		Author:  out.Author.Username,
		Labels:  out.Labels,
		State:   out.State,
		HTMLURL: out.WebURL,
	}, nil
}
