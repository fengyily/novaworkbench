// capture.mjs — screenshot + style-probe collection.
//
// Drives a headless Chromium over the route manifest, signs in first so the
// screenshots show the real signed-in UI (not the login wall), and writes
// one PNG plus one probe JSON per route. The manifest records the git
// commit and a sha256 per screenshot so every issue can point at evidence
// that is verifiably the thing it claims to be.
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { chromium } from 'playwright-core';
import { buildRouteList, slugFor } from './routes.mjs';
import { collectProbe } from './probe.mjs';
import { ensureDir, writeJSON, sha256, rel, gitCommit, run } from './util.mjs';

const CHROME_CANDIDATES = [
  '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome',
  '/Applications/Chromium.app/Contents/MacOS/Chromium',
  '/usr/bin/google-chrome',
  '/usr/bin/chromium',
  '/usr/bin/chromium-browser',
];

function findChrome() {
  for (const p of CHROME_CANDIDATES) if (fs.existsSync(p)) return p;
  return null;
}

async function login(baseUrl, username, password) {
  const res = await fetch(new URL('/api/auth/login', baseUrl), {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ username, password }),
  });
  const payload = await res.json().catch(() => null);
  if (!res.ok || !payload?.success) {
    throw new Error(`login failed: HTTP ${res.status} ${payload?.error?.message || ''}`);
  }
  return payload.data.token;
}

async function apiGet(baseUrl, token, pathname) {
  const res = await fetch(new URL(pathname, baseUrl), {
    headers: token ? { Authorization: `Bearer ${token}` } : {},
  });
  const payload = await res.json().catch(() => null);
  if (!res.ok || !payload?.success) throw new Error(`${pathname} -> HTTP ${res.status}`);
  return payload.data;
}

/**
 * Get a session token, or decide that the instance does not need one.
 *
 * A server started with NOVA_AUTH_DISABLED=1 answers every request as
 * admin, so there is nothing to log into. Probing /api/auth/me tells the
 * two cases apart — and refusing to continue otherwise is deliberate: a
 * run that silently captured the login wall would produce a report full of
 * screenshots of a page nobody uses.
 */
async function resolveToken(baseUrl, opts) {
  if (opts.token) return opts.token;
  if (opts.username && opts.password) return login(baseUrl, opts.username, opts.password);
  try {
    const me = await apiGet(baseUrl, '', '/api/auth/me');
    if (me?.user) return '';
  } catch {
    /* fall through to the error below */
  }
  throw new Error(
    `no usable session for ${baseUrl}: pass --token / --username+--password, ` +
    `or start the instance with NOVA_AUTH_DISABLED=1`,
  );
}

async function apiPost(baseUrl, token, pathname, body) {
  const res = await fetch(new URL(pathname, baseUrl), {
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
      ...(token ? { Authorization: `Bearer ${token}` } : {}),
    },
    body: JSON.stringify(body),
  });
  const payload = await res.json().catch(() => null);
  if (!res.ok || !payload?.success) {
    throw new Error(`${pathname} -> HTTP ${res.status} ${payload?.error?.message || ''}`);
  }
  return payload.data;
}

/**
 * Create the minimum fixtures the dynamic routes need.
 *
 * Without a requirement, /requirements/:id has nothing to show and the
 * single largest surface in the app (RequirementDetail) would be missing
 * from the audit. Seeding is opt-in and only ever runs against the
 * instance under audit — which for a self-contained run is a throwaway copy
 * of the database.
 */
async function seedFixtures(baseUrl, token) {
  const seeded = [];
  let projects = await apiGet(baseUrl, token, '/api/projects');
  let list = Array.isArray(projects) ? projects : projects?.items || [];

  // An empty database has no project, and the project detail page is one of
  // the app's biggest surfaces. Create a throwaway one so it gets audited.
  if (!list.length) {
    const dir = path.join(os.tmpdir(), 'nova-ui-audit-fixture');
    fs.mkdirSync(dir, { recursive: true });
    fs.writeFileSync(
      path.join(dir, 'README.md'),
      '# UI audit fixture\n\nA throwaway project used only to give the audit something to render.\n',
    );
    fs.writeFileSync(path.join(dir, 'main.go'), 'package main\n\nfunc main() {}\n');
    // The project service only accepts git repositories.
    await run('git', ['init', '-q', '-b', 'main', dir], { timeout: 30000 });
    try {
      const created = await apiPost(baseUrl, token, '/api/projects', {
        local_path: dir,
        remote_url: '',
        init_git: false,
        branch: 'main',
      });
      if (created?.id) {
        seeded.push({ what: 'project', id: created.id });
        list = [created];
      }
    } catch (e) {
      process.stdout.write(`  seed: project skipped — ${e.message}\n`);
    }
  }

  const project = list[0];
  if (!project) return { seeded, reason: 'no project to attach fixtures to' };

  const existing = await apiGet(baseUrl, token, '/api/requirements');
  const reqs = Array.isArray(existing) ? existing : existing?.items || [];
  if (!reqs.length) {
    try {
      const created = await apiPost(baseUrl, token, '/api/requirements', {
        project_id: project.id,
        title: 'UI 一致性审计夹具',
        description:
          '这是一条由 UI 审计工具创建的夹具需求，用来让 /requirements/:id 有内容可截图。\n\n' +
          '## 标题\n\n一段带 **粗体**、列表和代码块的 Markdown：\n\n' +
          '- 项目一\n- 项目二\n\n```js\nconst x = 1;\n```\n',
        kind: 'requirement',
        skip_organize: true,
      });
      if (created?.id) seeded.push({ what: 'requirement', id: created.id });
    } catch (e) {
      process.stdout.write(`  seed: requirement skipped — ${e.message}\n`);
    }
  }

  // A draft review session is enough to render /reviews/:id.
  try {
    const reviews = await apiGet(baseUrl, token, `/api/projects/${project.id}/reviews`);
    const list2 = Array.isArray(reviews) ? reviews : reviews?.items || [];
    if (!list2.length) {
      const created = await apiPost(baseUrl, token, `/api/projects/${project.id}/reviews`, {
        branch: project.branch || 'main',
        title: 'UI 一致性审计夹具',
        description: '夹具 review 会话，用于让 /reviews/:id 有内容可截图。',
      });
      if (created?.id) seeded.push({ what: 'review', id: created.id });
    }
  } catch (e) {
    process.stdout.write(`  seed: review skipped — ${e.message}\n`);
  }

  return { seeded, project: project.id };
}

export async function capture(opts) {
  const baseUrl = opts.baseUrl.replace(/\/$/, '');
  const outDir = path.join(opts.artifactsDir, opts.runId);
  const shotDir = ensureDir(path.join(outDir, 'screenshots'));
  const probeDir = ensureDir(path.join(outDir, 'probe'));

  const token = await resolveToken(baseUrl, opts);

  let seedInfo = { seeded: [] };
  if (opts.seed) {
    try {
      seedInfo = await seedFixtures(baseUrl, token);
      if (seedInfo.seeded?.length) {
        process.stdout.write(`  seeded ${seedInfo.seeded.map((s) => s.what).join(', ')}\n`);
      }
    } catch (e) {
      process.stdout.write(`  seed skipped: ${e.message}\n`);
    }
  }

  const { routes, skipped } = await buildRouteList(baseUrl, token);
  const selected = opts.only?.length
    ? routes.filter((r) => opts.only.some((o) => r.path.includes(o)))
    : routes;

  const executablePath = findChrome();
  const browser = await chromium.launch({
    headless: true,
    ...(executablePath ? { executablePath } : {}),
    args: ['--no-sandbox', '--disable-dev-shm-usage', '--font-render-hinting=none'],
  });

  const context = await browser.newContext({
    viewport: { width: opts.viewportWidth, height: opts.viewportHeight },
    deviceScaleFactor: opts.deviceScaleFactor,
    locale: 'zh-CN',
    colorScheme: 'light',
  });
  await context.addInitScript(
    ([t]) => {
      try {
        window.localStorage.setItem('nova_token', t);
      } catch (e) {
        /* storage unavailable — the app will redirect to /login */
      }
    },
    [token],
  );

  const page = await context.newPage();
  const captured = [];
  const failures = [];

  for (const route of selected) {
    const slug = slugFor(route.path);
    const url = new URL(route.path, baseUrl).toString();
    try {
      await page.goto(url, { waitUntil: 'domcontentloaded', timeout: 30000 });
      // The SPA paints after the API round-trips; networkidle alone can fire
      // before React has laid the page out, so give it a beat to settle.
      await page.waitForLoadState('networkidle', { timeout: 15000 }).catch(() => {});
      await page.waitForTimeout(opts.settleMs);

      const probe = await page.evaluate(collectProbe);
      const file = path.join(shotDir, `${slug}.png`);
      await page.screenshot({ path: file, fullPage: true });

      const buf = fs.readFileSync(file);
      writeJSON(path.join(probeDir, `${slug}.json`), probe);

      captured.push({
        slug,
        path: route.path,
        name: route.name,
        group: route.group,
        placeholder: !!route.placeholder,
        url,
        screenshot: rel(file),
        screenshotBytes: buf.length,
        sha256: sha256(buf),
        textLength: probe.textLength,
        elementCount: probe.elementCount,
        roles: Object.keys(probe.roles || {}),
      });
      process.stdout.write(`  captured ${route.path}\n`);
    } catch (e) {
      failures.push({ path: route.path, reason: String(e.message || e).split('\n')[0] });
      process.stdout.write(`  FAILED   ${route.path}: ${String(e.message || e).split('\n')[0]}\n`);
    }
  }

  await browser.close();

  const manifest = {
    runId: opts.runId,
    baseUrl,
    gitCommit: await gitCommit(),
    capturedAt: new Date().toISOString(),
    viewport: { width: opts.viewportWidth, height: opts.viewportHeight, deviceScaleFactor: opts.deviceScaleFactor },
    browser: executablePath ? path.basename(executablePath) : 'playwright-bundled',
    seeded: seedInfo.seeded || [],
    routes: captured,
    skipped,
    failures,
  };
  writeJSON(path.join(outDir, 'manifest.json'), manifest);
  return { manifest, outDir };
}

export { findChrome };
