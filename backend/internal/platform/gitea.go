package platform

import (
	"context"
	"encoding/json"
	"fmt"
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
