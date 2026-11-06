# Publishing an audit run — evidence branch and issues

This is the part that turns a run into work items. It has two halves that
have to stay in sync:

- **the evidence branch** — where the screenshots live,
- **the issues** — where the findings are discussed and fixed.

## The evidence branch

Each run gets its own branch, `audit/ui-<run-id>`, holding the run's
artifacts under `docs/ui-audit/<run-id>/`:

```
docs/ui-audit/<run-id>/
├── screenshots/   # one PNG per route
├── probe/         # one style-fingerprint JSON per route
├── manifest.json  # route → file, sha256, audited git commit
├── findings.json  # every finding, machine readable
└── report.md      # the human-readable report
```

The branch is **not** merged into `main`. Three reasons:

1. The images are big — a full run is several MB — and `main` should not
   carry that weight.
2. The evidence belongs to exactly one commit. `manifest.json` records that
   commit, so the evidence and the code it describes stay paired.
3. A run is a snapshot. When the code moves on, the old branch is history,
   not clutter.

**Do not delete a branch whose issues are still open.** The issue bodies
embed `raw.githubusercontent.com/<owner>/<repo>/<branch>/…` links; deleting
the branch breaks every screenshot in every issue filed from it. When a
branch has to go, close the issues first and note in each one that the
evidence moved.

### How the branch is written

`publishEvidence()` does **not** check out the branch in your working
directory. It creates a temporary git worktree, writes the files there,
commits and pushes, then removes the worktree. Two consequences worth
knowing:

- Your current branch is never switched, and your uncommitted work can
  never end up in the evidence commit.
- The publisher works on a dirty working tree — which matters, because the
  tool that pushes the evidence is itself usually not committed yet when it
  runs for the first time.

### Re-running the publish stage

`publish` is idempotent with respect to the issues: every issue body ends
with

```
<!-- ui-audit-run: 20261025-2015 -->
```

and the publisher looks for that marker before creating anything. Re-running
`publish` for the same run therefore opens no duplicates.

Re-running it with a *different* `--run-id` opens a second set of issues
for the same underlying problem. Close the older ones, or delete the marker
from the body before re-running.

## The issues

One issue per confirmed finding — never one issue for the whole run. Each
body has the same shape:

| section | what it carries |
|---|---|
| the opening paragraph | what a user sees, and why it counts as inconsistent (rewritten by the model during review) |
| `## 证据` | the screenshots, embedded through the raw URL, plus a table of the differing computed values |
| `## 涉及位置` | the files and lines to change |
| `## 复现` | three steps that reproduce it from the audited commit |
| `## 建议修法` | the concrete fix |
| the footer | run id, rule, severity, whether the model opened the screenshot, the evidence branch |

Labels: `ui-audit` plus the severity (`P0`…`P3`). The publisher creates
any label that does not exist yet.

### Which findings become issues

- `status !== 'rejected'` — the model's false-positive verdict removes a
  finding from the issue set, but never from `findings.json`. It stays in
  the report under "被判定为误报" so the decision is reviewable.
- `--min-severity` filters by severity (default `P2`).
- `--max-issues` caps the number per run (default 30). Raise it when the
  run is expected to produce more, otherwise the tail is silently dropped.

## When a raw link stops resolving

The screenshot URL is absolute, so it breaks when the branch is deleted,
the repository is renamed, or the repository is private and the reader has
no access. The evidence itself is still verifiable:

```bash
git fetch origin audit/ui-<run-id>
git show audit/ui-<run-id>:docs/ui-audit/<run-id>/report.md
shasum -a 256 docs/ui-audit/<run-id>/screenshots/root.png
```

The sha256 in `manifest.json` is what makes the evidence a fact rather than
a picture: it says which bytes the issue is talking about.
