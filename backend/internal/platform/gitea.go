package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

type giteaClient struct {
	baseURL string
	token   string
}

func (c *giteaClient) parseRepo(repoURL string) (owner, repo string, err error) {
	u := strings.TrimSuffix(repoURL, ".git")

	// SSH: git@gitea.example.com:owner/repo
	if strings.HasPrefix(u, "git@") {
		colon := strings.Index(u, ":")
		if colon < 0 {
			return "", "", fmt.Errorf("cannot parse Gitea SSH URL: %s", repoURL)
		}
		parts := strings.SplitN(u[colon+1:], "/", 2)
		if len(parts) != 2 {
			return "", "", fmt.Errorf("cannot parse Gitea SSH URL: %s", repoURL)
		}
		return parts[0], parts[1], nil
	}

	// HTTPS: find path after host
	idx := strings.Index(u, "://")
	if idx < 0 {
		return "", "", fmt.Errorf("cannot parse Gitea URL: %s", repoURL)
	}
	rest := u[idx+3:]
	slash := strings.Index(rest, "/")
	if slash < 0 {
		return "", "", fmt.Errorf("cannot parse Gitea URL: %s", repoURL)
	}
	parts := strings.SplitN(rest[slash+1:], "/", 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("cannot parse Gitea URL: %s", repoURL)
	}
	return parts[0], parts[1], nil
}

func (c *giteaClient) do(ctx context.Context, method, apiURL, body string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, apiURL, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "token "+c.token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	return http.DefaultClient.Do(req)
}

func (c *giteaClient) ListOpenPRs(ctx context.Context, repoURL string) ([]PR, error) {
	owner, repo, err := c.parseRepo(repoURL)
	if err != nil {
		return nil, err
	}

	apiURL := fmt.Sprintf("%s/api/v1/repos/%s/%s/pulls?state=open&limit=50", c.baseURL, owner, repo)
	resp, err := c.do(ctx, http.MethodGet, apiURL, "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Gitea API error: %s", resp.Status)
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
			Label string `json:"label"`
		} `json:"head"`
		Base struct {
			Label string `json:"label"`
		} `json:"base"`
		HTMLURL   string    `json:"html_url"`
		UpdatedAt time.Time `json:"updated_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, err
	}

	prs := make([]PR, 0, len(raw))
	for _, r := range raw {
		// Gitea label is "owner:branch" — strip owner prefix
		head := r.Head.Label
		if idx := strings.Index(head, ":"); idx >= 0 {
			head = head[idx+1:]
		}
		base := r.Base.Label
		if idx := strings.Index(base, ":"); idx >= 0 {
			base = base[idx+1:]
		}
		prs = append(prs, PR{
			Number:     r.Number,
			Title:      r.Title,
			Body:       r.Body,
			Author:     r.User.Login,
			HeadBranch: head,
			BaseBranch: base,
			State:      r.State,
			HTMLURL:    r.HTMLURL,
			UpdatedAt:  r.UpdatedAt,
		})
	}
	return prs, nil
}

func (c *giteaClient) SubmitComment(ctx context.Context, repoURL string, prNumber int, body string) error {
	owner, repo, err := c.parseRepo(repoURL)
	if err != nil {
		return err
	}

	apiURL := fmt.Sprintf("%s/api/v1/repos/%s/%s/issues/%d/comments", c.baseURL, owner, repo, prNumber)
	payload, _ := json.Marshal(map[string]string{"body": body})
	resp, err := c.do(ctx, http.MethodPost, apiURL, string(payload))
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("Gitea API error: %s", resp.Status)
	}
	return nil
}

func (c *giteaClient) CreatePR(ctx context.Context, repoURL string, base, head, title, body string) (*PR, error) {
	owner, repo, err := c.parseRepo(repoURL)
	if err != nil {
		return nil, err
	}

	apiURL := fmt.Sprintf("%s/api/v1/repos/%s/%s/pulls", c.baseURL, owner, repo)
	payload, _ := json.Marshal(map[string]string{"title": title, "head": head, "base": base, "body": body})
	resp, err := c.do(ctx, http.MethodPost, apiURL, string(payload))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("Gitea API error: %s%s", resp.Status, readErrBody(resp))
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

// ListIssues / PostIssueComment: v0.5.x ships GitHub-only. Gitea stubs
// follow the same pattern as the GitLab stubs above — typed error plus
// log line so the failure is visible from both the UI and the backend.
func (c *giteaClient) ListIssues(ctx context.Context, repoURL, label string) ([]Issue, error) {
	log.Printf("[platform/gitea] ListIssues not yet implemented (v0.5.x GitHub-only); repo=%s label=%q", repoURL, label)
	return nil, errors.New("gitea ListIssues is not implemented in v0.5.x (GitHub-only)")
}

func (c *giteaClient) PostIssueComment(ctx context.Context, repoURL string, issueNumber int, body string) error {
	log.Printf("[platform/gitea] PostIssueComment not yet implemented (v0.5.x GitHub-only); repo=%s issue=%d", repoURL, issueNumber)
	return errors.New("gitea PostIssueComment is not implemented in v0.5.x (GitHub-only)")
}

// CreateIssue opens a new Issue via POST /api/v1/repos/{owner}/{repo}/issues.
//
// Gitea's label field takes numeric label IDs, not names like GitHub and
// GitLab do. Resolving names to ids would mean an extra GET per label and
// a create-if-missing policy that the automated-testing module has no use
// for, so v1 intentionally drops `labels` and instead prefixes the body
// with an `[autotest]` marker — which is what the caller would have used a
// label for anyway (tagging the issue so humans can find it).
func (c *giteaClient) CreateIssue(ctx context.Context, repoURL, title, body string, labels []string) (*Issue, error) {
	owner, repo, err := c.parseRepo(repoURL)
	if err != nil {
		return nil, err
	}

	payload := map[string]any{"title": title, "body": autotestMarker + body}
	raw, _ := json.Marshal(payload)

	apiURL := fmt.Sprintf("%s/api/v1/repos/%s/%s/issues", c.baseURL, owner, repo)
	resp, err := c.do(ctx, http.MethodPost, apiURL, string(raw))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("Gitea API error: %s%s", resp.Status, readErrBody(resp))
	}

	var out struct {
		Number  int    `json:"number"`
		Title   string `json:"title"`
		Body    string `json:"body"`
		State   string `json:"state"`
		HTMLURL string `json:"html_url"`
		User    struct {
			Login string `json:"login"`
		} `json:"user"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(out.Labels))
	for _, l := range out.Labels {
		names = append(names, l.Name)
	}
	return &Issue{
		Number:  out.Number,
		Title:   out.Title,
		Body:    out.Body,
		Author:  out.User.Login,
		Labels:  names,
		State:   out.State,
		HTMLURL: out.HTMLURL,
	}, nil
}

// autotestMarker prefixes bodies of issues filed by the automated-testing
// module on platforms where we cannot set a real label (see CreateIssue).
const autotestMarker = "[autotest]\n\n"
