package platform

import (
	"context"
	"fmt"
	"time"
)

// PR represents a pull request / merge request from any platform.
type PR struct {
	Number     int       `json:"number"`
	Title      string    `json:"title"`
	Body       string    `json:"body"`
	Author     string    `json:"author"`
	HeadBranch string    `json:"head_branch"`
	BaseBranch string    `json:"base_branch"`
	State      string    `json:"state"`
	HTMLURL    string    `json:"html_url"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// Client abstracts platform-specific PR operations.
type Client interface {
	ListOpenPRs(ctx context.Context, repoURL string) ([]PR, error)
	SubmitComment(ctx context.Context, repoURL string, prNumber int, body string) error
}

// New returns a Client for the given platform.
// platform: "github" | "gitlab" | "gitea"
// baseURL: empty for GitHub (uses api.github.com); required for self-hosted GitLab/Gitea
func New(platform, baseURL, token string) (Client, error) {
	switch platform {
	case "github":
		return &githubClient{token: token}, nil
	case "gitlab":
		base := baseURL
		if base == "" {
			base = "https://gitlab.com"
		}
		return &gitlabClient{baseURL: base, token: token}, nil
	case "gitea":
		if baseURL == "" {
			return nil, fmt.Errorf("gitea platform requires a base_url")
		}
		return &giteaClient{baseURL: baseURL, token: token}, nil
	default:
		return nil, fmt.Errorf("unsupported platform: %q (must be github, gitlab, or gitea)", platform)
	}
}
