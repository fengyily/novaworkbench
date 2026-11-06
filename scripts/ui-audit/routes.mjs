// routes.mjs — the auditable route manifest.
//
// The audit walks every route the SPA exposes. Static routes are listed
// here by hand; dynamic ones (`:id`) are resolved at capture time from the
// live API so the screenshots show real data instead of empty states.
//
// ROUTE_SNAPSHOT is a copy of the route strings declared in
// frontend/src/App.tsx. The `detect` stage diffs it against the real
// App.tsx — if someone adds a route and forgets this manifest, the audit
// would silently shrink its own scope, so that becomes a finding (R0)
// rather than a quiet gap.
import fs from 'node:fs';
import path from 'node:path';
import { FRONTEND_SRC, slugify } from './util.mjs';

/** Route strings as declared in frontend/src/App.tsx (settings sub-routes included). */
export const ROUTE_SNAPSHOT = [
  '/login',
  '/knowledge/view/:id',
  '/wizard',
  '/projects/add',
  '/projects',
  '/projects/:id',
  '/projects/:id/issue-monitor',
  '/reviews/:id',
  '/requirements',
  '/requirements/calendar',
  '/requirements/:id',
  '/schedules',
  '/knowledge',
  '/chat',
  '/reports',
  '/settings',
  '/settings/users',
  '/settings/acl',
  '/settings/roles',
  '/settings/agent-servers',
  '/settings/skills',
  '/settings/claude',
  '/settings/llm',
  '/settings/subtask',
  '/settings/git-sync',
  '/settings/database',
  '/settings/preflight',
];

/**
 * Routes reachable without any id. `placeholder: true` marks the two stub
 * pages — they are reported separately as "not built yet" instead of being
 * treated as a consistency defect.
 */
export const STATIC_ROUTES = [
  { path: '/', name: 'dashboard', group: 'core' },
  { path: '/wizard', name: 'wizard', group: 'core' },
  { path: '/projects', name: 'projects-list', group: 'core' },
  { path: '/projects/add', name: 'project-add', group: 'core' },
  { path: '/requirements', name: 'requirements-list', group: 'core' },
  { path: '/requirements/calendar', name: 'requirements-calendar', group: 'core' },
  { path: '/schedules', name: 'schedules', group: 'core' },
  { path: '/knowledge', name: 'knowledge', group: 'core' },
  { path: '/chat', name: 'chat', group: 'stub', placeholder: true },
  { path: '/reports', name: 'reports', group: 'stub', placeholder: true },
  { path: '/settings', name: 'settings-tokens', group: 'settings' },
  { path: '/settings/users', name: 'settings-users', group: 'settings' },
  { path: '/settings/acl', name: 'settings-acl', group: 'settings' },
  { path: '/settings/roles', name: 'settings-roles', group: 'settings' },
  { path: '/settings/agent-servers', name: 'settings-agent-servers', group: 'settings' },
  { path: '/settings/skills', name: 'settings-skills', group: 'settings' },
  { path: '/settings/claude', name: 'settings-claude', group: 'settings' },
  { path: '/settings/llm', name: 'settings-llm', group: 'settings' },
  { path: '/settings/subtask', name: 'settings-subtask', group: 'settings' },
  { path: '/settings/git-sync', name: 'settings-git-sync', group: 'settings' },
  { path: '/settings/database', name: 'settings-database', group: 'settings' },
  { path: '/settings/preflight', name: 'settings-preflight', group: 'settings' },
  { path: '/login', name: 'login', group: 'auth' },
];

/** Dynamic routes: how to discover a concrete id for each. */
export const DYNAMIC_ROUTES = [
  { path: '/projects/:id', name: 'project-detail', group: 'core', needs: 'project' },
  { path: '/projects/:id/issue-monitor', name: 'project-issue-monitor', group: 'core', needs: 'project' },
  { path: '/requirements/:id', name: 'requirement-detail', group: 'core', needs: 'requirement' },
  { path: '/reviews/:id', name: 'code-review-detail', group: 'core', needs: 'review' },
  { path: '/knowledge/view/:id', name: 'knowledge-view', group: 'core', needs: 'knowledge' },
];

export function slugFor(routePath) {
  return slugify(routePath.replace(/^\//, '').replace(/\//g, '-')) || 'root';
}

/** Pull the route path literals out of the real App.tsx. */
export function readAppRoutes() {
  const file = path.join(FRONTEND_SRC, 'App.tsx');
  const text = fs.readFileSync(file, 'utf8');
  const found = new Set();
  const re = /<Route\s[^>]*?path="([^"]+)"/g;
  let m;
  while ((m = re.exec(text))) found.add(m[1]);
  return [...found];
}

/**
 * Discover concrete ids for the dynamic routes by querying the running API.
 * Returns `{ ids, errors }`; a missing id is not fatal — the caller records
 * the route as skipped with a reason instead of screenshotting an empty page.
 */
export async function discoverIds(baseUrl, token) {
  const ids = { project: null, requirement: null, review: null, knowledge: null };
  const errors = [];

  const get = async (pathname) => {
    const res = await fetch(new URL(pathname, baseUrl), {
      headers: token ? { Authorization: `Bearer ${token}` } : {},
    });
    if (!res.ok) throw new Error(`${pathname} -> HTTP ${res.status}`);
    const payload = await res.json();
    if (!payload || payload.success === false) throw new Error(`${pathname} -> ${payload?.error?.code || 'error'}`);
    return payload.data;
  };

  const tryGet = async (key, pathname, pick) => {
    try {
      const data = await get(pathname);
      const value = pick(data);
      if (value) ids[key] = value;
      else errors.push(`${pathname} returned no usable id`);
    } catch (e) {
      errors.push(String(e.message || e));
    }
  };

  await tryGet('project', '/api/projects', (d) => {
    const list = Array.isArray(d) ? d : d?.items || d?.projects || [];
    return list[0]?.id;
  });
  await tryGet('requirement', '/api/requirements', (d) => {
    const list = Array.isArray(d) ? d : d?.items || d?.requirements || [];
    return list[0]?.id;
  });
  await tryGet('knowledge', '/api/knowledge', (d) => {
    const list = Array.isArray(d) ? d : d?.items || d?.knowledge || [];
    return list[0]?.id;
  });

  // Code review sessions hang off a project.
  if (ids.project) {
    try {
      const list = await get(`/api/projects/${ids.project}/reviews`);
      const arr = Array.isArray(list) ? list : list?.items || [];
      if (arr[0]?.id) ids.review = arr[0].id;
      else errors.push(`/api/projects/${ids.project}/reviews returned no usable id`);
    } catch (e) {
      errors.push(String(e.message || e));
    }
  }

  return { ids, errors };
}

/** Build the concrete route list for a capture run. */
export async function buildRouteList(baseUrl, token) {
  const routes = STATIC_ROUTES.map((r) => ({ ...r, path: r.path, dynamic: false }));
  const skipped = [];

  const { ids, errors } = await discoverIds(baseUrl, token);
  for (const dyn of DYNAMIC_ROUTES) {
    const id = ids[dyn.needs];
    if (!id) {
      skipped.push({ path: dyn.path, reason: `no ${dyn.needs} id available` });
      continue;
    }
    routes.push({
      ...dyn,
      path: dyn.path.replace(':id', id),
      dynamic: true,
      needs: undefined,
    });
  }

  for (const e of errors) skipped.push({ path: '(api)', reason: e });

  // The login page is reachable but pointless to compare against the rest of
  // the app; keep it, it is a legitimate surface, but mark it as its own
  // group so cross-page drift rules can exclude it.
  return { routes, skipped, ids };
}
