package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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
	owner, repo, err := c.parseRepo(repoURL)
	if err != nil {
		return err
	}

	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/issues/%d/comments", owner, repo, prNumber)
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
