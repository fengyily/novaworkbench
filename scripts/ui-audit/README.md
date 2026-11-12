# `scripts/ui-audit` — global UI consistency audit

Finds the places where the NovaWorkbench frontend does not look like itself,
pins the evidence with screenshots, and opens **one GitHub issue per
inconsistency**.

The tool never edits the UI. It reads the source and the running app, then
files what it finds.

---

## Quick start

```bash
# 1. install the browser driver (no browser download — it reuses your Chrome)
npm --prefix scripts/ui-audit ci

# 2. make sure the app you want to audit is running, e.g.
cd frontend && npm run dev     # http://localhost:5173
#    or: make build && make run

# 3. run the whole pipeline
node scripts/ui-audit/run.mjs all --base-url http://localhost:9527
```

`make ui-audit` wraps step 3.

## What each stage does

| stage | what it produces | needs a browser? | needs the model? | needs `npm install`? |
|---|---|---|---|---|
| `capture` | one PNG + one style probe per route, plus `manifest.json` with a sha256 per screenshot | yes | no | yes (`playwright-core`) |
| `detect` | `findings.json` — deterministic rule hits | no (runtime rule needs the probes) | no | no |
| `review` | model verdicts: keep / reject, plus user-facing wording | no | yes | no |
| `publish` | evidence branch + one GitHub issue per confirmed finding | no | no | no |
| `check` | CI gate — exits non-zero on *new* violations | no | no | no |

`run.mjs` imports the browser driver lazily, so every stage except `capture`
runs on a bare node — the CI gate needs no `npm install` at all.

## The rules

| id | what it looks for |
|---|---|
| R0 | the route manifest disagrees with `frontend/src/App.tsx` (the audit would silently shrink its own scope) |
| R1 | colour literals that are not in the `:root` palette |
| R2 | token values written as literals instead of `var(--token)` |
| R3 | one colour spelled in several ways (`#fff` / `#ffffff` / `#FFF`) |
| R4 | class names used in JSX that no stylesheet defines |
| R5 | two naming conventions for the same thing (`btn-*` vs `*-btn`) |
| R6 / R7 | several class names for the same container / section heading |
| R8 | parallel status-colour vocabularies, or status colouring that bypasses `.status-badge` |
| R9 | inline `style={{ }}` density high enough to override the shared classes |
| R10 | `<MarkdownRender>` hosts that are not on `.md-prose` |
| R11 | **the same control computing to different styles on different pages** |
| R12 | stub pages (reported separately — they are not consistency defects) |

R11 is the one that matches what a user actually notices: it compares the
*computed* style of each semantic role (`btn-primary`, `input-text`, `card`,
…) across every captured route, and reports the roles whose dominant
fingerprint differs from page to page.

## Authentication

The audit needs a signed-in session, otherwise every screenshot is the login
wall. It looks for a token in this order:

1. `--token <session-token>` / `NOVA_AUDIT_TOKEN`
2. `--username` + `--password` (or `NOVA_AUDIT_USER` / `NOVA_AUDIT_PASS`) —
   it POSTs `/api/auth/login`
3. a live session token from the local database
   (`~/.novaworkbench/data/nova.db`, table `sessions`)

If none of these work the run aborts. It never writes out screenshots of a
page it was not allowed to see — a login wall silently captured as "the
dashboard" would poison the whole report.

## Evidence layout

```
artifacts/ui-audit/<run-id>/          # local, not committed
├── screenshots/<route>.png
├── probe/<route>.json
├── manifest.json                     # route → file, sha256, git commit
├── findings.json
├── report.md
└── issue-bodies/<finding-id>.md

docs/ui-audit/<run-id>/               # pushed to branch audit/ui-<run-id>
├── screenshots/…
├── probe/…
├── manifest.json
├── findings.json
└── report.md
```

The issue body embeds the screenshot through the platform's raw-file URL, so
the evidence stays versioned next to the code it describes while the main
branch stays free of image churn.

## Reusing a capture

The stages are independent, so a run can be stopped and resumed. This is
what you want when only the wording or the set of issues has to change —
re-reviewing does not mean re-driving the browser:

```bash
# already captured: artifacts/ui-audit/<run-id>/manifest.json exists
node scripts/ui-audit/run.mjs review  --run-id 20261025-2015
node scripts/ui-audit/run.mjs publish --run-id 20261025-2015
```

`make ui-audit-review RUN_ID=…` and `make ui-audit-publish RUN_ID=…` wrap
the two commands.

## CI gate

```bash
node scripts/ui-audit/run.mjs check
```

Compares the current violations against `baseline.json` and fails only on
ones that are *not* already baselined. Regenerate the baseline with:

```bash
node scripts/ui-audit/run.mjs check --write-baseline
```

Every rule emits a stable `signature` that identifies the finding itself —
the class name, the semantic role — rather than the files it mentions. That
matters: a signature derived from file paths would report a "new" violation
every time an unrelated file was edited.

The gate runs in CI as the `ui-audit` job. It needs no browser, no model
and no `npm install`.

## Options

```
--base-url <url>        app under audit (default http://localhost:9527)
--run-id <id>           run identifier (default: timestamp)
--username / --password credentials for /api/auth/login
--token <session>       use an existing session token
--artifacts-dir <dir>   local output (default artifacts/ui-audit)
--evidence-prefix <p>   committed evidence path (default docs/ui-audit)
--branch <name>         evidence branch (default audit/ui-<run-id>)
--base-ref <ref>        commit the evidence branch starts from (default HEAD)
--provider gh|local     open issues with gh, or only write the bodies to disk
--min-severity <P0..P3> lowest severity to file (default P2)
--max-issues <n>        cap on issues per run (default 30)
--route <substring>     only audit routes matching this (repeatable)
--seed                  create throwaway fixtures for the dynamic routes
--no-review             skip the model pass
--dry-run               write issue bodies to disk, do not open issues
--width / --height / --scale   viewport (default 1440x900 @2x)
```

See [`PUBLISH.md`](./PUBLISH.md) for how the evidence branch and the issues
fit together, and what to do when a raw link stops resolving.