package platform

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
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

// Issue represents an issue (or, on GitHub, an item that LOOKS like an
// issue but is actually a PR — the consumer MUST filter by PullRequest
// on the GitHub implementation because GitHub returns both via the
// Issues endpoint). Body is the raw Markdown body returned by the API
// (the HTML rendering is a UI thing; the API field is already Markdown).
type Issue struct {
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	Author    string    `json:"author"`
	Labels    []string  `json:"labels"`
	State     string    `json:"state"` // "open" | "closed"
	HTMLURL   string    `json:"html_url"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Client abstracts platform-specific PR + Issue operations. ListIssues
// and PostIssueComment are v0.5.x additions for the issue-monitor feature
// (GitHub impl ships in this release; GitLab/Gitea return a stub "not yet
// implemented" error so the interface compiles and the user sees a clear
// signal instead of a silent failure).
type Client interface {
	ListOpenPRs(ctx context.Context, repoURL string) ([]PR, error)
	SubmitComment(ctx context.Context, repoURL string, prNumber int, body string) error
	CreatePR(ctx context.Context, repoURL string, base, head, title, body string) (*PR, error)

	// ListIssues returns open Issues matching `label` (empty = no label
	// filter) for the given repository. Implementations are expected to
	// cap the page at a reasonable size; pagination is OUT OF SCOPE for
	// v0.5.x — the monitor's poll cadence plus the dedup on
	// (project_id, source_platform, source_external_id) handles churn.
	ListIssues(ctx context.Context, repoURL, label string) ([]Issue, error)
	// PostIssueComment posts a comment on the Issue (or PR — GitHub's
	// `/issues/{n}/comments` endpoint serves both) identified by
	// issueNumber. The issue-monitor's auto-push PR hook calls this with
	// issueNumber = requirements.source_external_id (the GitHub Issue
	// number the requirement was created from) when monitor.comment_on_pr
	// is 1 and monitor.comment_posted is 0.
	PostIssueComment(ctx context.Context, repoURL string, issueNumber int, body string) error

	// CreateIssue opens a NEW Issue on the repository behind repoURL and
	// returns the created row. The automated-testing module uses it to file
	// a bug for a failing test case, so the body normally carries the test
	// title, the failure excerpt and links to the screenshot/trace.
	//
	// `labels` is best-effort: GitHub and GitLab take label names, but
	// Gitea's API only accepts numeric label ids, so its implementation
	// ignores the names and appends an `[autotest]` marker to the body
	// instead. Callers must therefore not rely on the labels round-tripping.
	CreateIssue(ctx context.Context, repoURL, title, body string, labels []string) (*Issue, error)
}

// readErrBody drains up to 2 KB of an error response body for a richer message
// (creation failures carry useful detail like "already exists").
func readErrBody(resp *http.Response) string {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	if len(b) == 0 {
		return ""
	}
	return ": " + string(b)
}

// New returns a Client for the given platform.
// platform: "github" | "gitlab" | "gitea"
// baseURL: empty for GitHub (uses api.github.com); required for self-hosted GitLab/Gitea
func New(platform, baseURL, token string) (Client, error) {
	switch platform {
	case "github":
		base := baseURL
		if base == "" {
			base = "https://api.github.com"
		}
		return &githubClient{baseURL: strings.TrimRight(base, "/"), token: token}, nil
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
