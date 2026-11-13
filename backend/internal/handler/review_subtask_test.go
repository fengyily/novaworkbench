package handler

// review_subtask_test.go — guards for the requirement-level Review's scope
// decisions.
//
// The requirement-level Review no longer waits for the requirement to be
// marked "开发完成" — that is a human judgement, and as long as there is code
// to grade the user may Review it. The decisions that follow from that are
// pinned here, because they are the ones that decide WHICH code gets graded.

import (
	"testing"

	"github.com/novaworkbench/backend/internal/model"
)

// TestPreferLocalHeadFor pins the head-ref preference.
//
// The Review grades the requirement's OWN worktree when the requirement was
// developed locally — the local branch may carry commits the user never
// pushed, and diffing against `origin/<branch>` would silently grade the
// pre-push code (the 「修复 → 回归 Review」 loop would never converge).
//
// A requirement developed on an Agent Server keeps origin-first: its code
// lives on the server, so the remote ref is the copy that holds the work.
func TestPreferLocalHeadFor(t *testing.T) {
	proj := "/projects/demo"
	wt := "/projects/demo/.nova-worktrees/req_1"

	cases := []struct {
		name string
		req  *model.Requirement
		dir  string
		want bool
	}{
		{
			name: "locally developed requirement in its own worktree",
			req:  &model.Requirement{ID: "req_1", WorktreePath: wt},
			dir:  wt,
			want: true,
		},
		{
			name: "developed on an agent server — the remote copy holds the code",
			req:  &model.Requirement{ID: "req_2", WorktreePath: wt, AgentServerID: "agent_1"},
			dir:  wt,
			want: false,
		},
		{
			name: "degraded mode — no local worktree, the project checkout is used",
			req:  &model.Requirement{ID: "req_3"},
			dir:  proj,
			want: false,
		},
		{
			name: "worktree path equals the project checkout",
			req:  &model.Requirement{ID: "req_4", WorktreePath: proj},
			dir:  proj,
			want: false,
		},
		{
			name: "no working directory at all",
			req:  &model.Requirement{ID: "req_5", WorktreePath: wt},
			dir:  "",
			want: false,
		},
		{
			name: "nil requirement",
			req:  nil,
			dir:  wt,
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := preferLocalHeadFor(tc.req, tc.dir, proj); got != tc.want {
				t.Fatalf("preferLocalHeadFor = %v, want %v", got, tc.want)
			}
		})
	}
}
