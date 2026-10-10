package handler

import (
	"strings"
	"testing"
)

// TestSubTaskGitIdentityEnv pins the env-construction contract shared by
// the local sub-task execution paths in
//   - sub_task_runner.go       (SubTaskRunner.Run, local branch)
//   - wizard_orchestration.go  (orchestrated sub-task local branch)
//
// Both sites append the project's bound git committer identity on top of
// the HTTPS askpass env that gitCredentialEnv(...) returns, so Claude's
// Bash-tool `git commit` runs under platform_tokens.git_user_name /
// git_user_email instead of falling back to the host's ~/.gitconfig.
//
// Empty on any miss is intentional: a project with no token, or a token
// whose git_user_name / git_user_email is empty, must NOT emit
// `GIT_AUTHOR_NAME=` (empty string) — git treats that as "set identity to
// ''" and rejects the commit. The fix must keep legacy / unbound projects
// on the ambient gitconfig fallback.
func TestSubTaskGitIdentityEnv(t *testing.T) {
	cases := []struct {
		name         string
		gitName      string
		gitEmail     string
		wantPresent  []string
		wantAbsent   []string // substrings that must NOT appear in the env
	}{
		{
			name:     "full identity (name + email) yields all four",
			gitName:  "Alice",
			gitEmail: "alice@example.com",
			wantPresent: []string{
				"GIT_AUTHOR_NAME=Alice",
				"GIT_COMMITTER_NAME=Alice",
				"GIT_AUTHOR_EMAIL=alice@example.com",
				"GIT_COMMITTER_EMAIL=alice@example.com",
			},
		},
		{
			name:     "email-only identity skips the *_NAME pair",
			gitName:  "",
			gitEmail: "alice@example.com",
			wantPresent: []string{
				"GIT_AUTHOR_EMAIL=alice@example.com",
				"GIT_COMMITTER_EMAIL=alice@example.com",
			},
			wantAbsent: []string{
				"GIT_AUTHOR_NAME",
				"GIT_COMMITTER_NAME",
			},
		},
		{
			name:        "no identity (project has no token / token has empty fields) emits nothing",
			gitName:     "",
			gitEmail:    "",
			wantPresent: []string{},
			wantAbsent: []string{
				"GIT_AUTHOR_NAME",
				"GIT_COMMITTER_NAME",
				"GIT_AUTHOR_EMAIL",
				"GIT_COMMITTER_EMAIL",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := buildIdentityEnv(tc.gitName, tc.gitEmail)
			joined := strings.Join(env, "\n")

			var missing []string
			for _, want := range tc.wantPresent {
				if !strings.Contains(joined, want) {
					missing = append(missing, want)
				}
			}
			for _, forbidden := range tc.wantAbsent {
				if strings.Contains(joined, forbidden) {
					t.Errorf("env must NOT contain %q, but got env=%v", forbidden, env)
				}
			}
			if len(missing) > 0 {
				t.Errorf("missing expected env entries: %v\nactual env=%v", missing, env)
			}
		})
	}
}

// TestSubTaskGitIdentityEnvNilWhenBothEmpty guards against an empty-slice
// regression: appending `[]string(nil)...` is a no-op (good), but the test
// fails loud if someone refactors buildIdentityEnv to return `[]string{}`
// instead of nil — the two are semantically identical here, but a non-nil
// empty slice would mask future bugs where callers branch on
// `len(env) == 0` differently.
func TestSubTaskGitIdentityEnvNilWhenBothEmpty(t *testing.T) {
	env := buildIdentityEnv("", "")
	if env != nil {
		t.Errorf("buildIdentityEnv(\"\", \"\") must return nil slice, got %#v", env)
	}
}
