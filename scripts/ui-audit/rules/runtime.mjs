// rules/runtime.mjs — cross-route comparison of computed styles.
//
// The static rules see the source. This one sees what the browser actually
// painted: for every semantic role (btn-primary, input-text, card, ...) it
// takes the dominant fingerprint per route and compares the routes against
// each other.
//
// A role that renders differently on two pages is the most direct evidence
// of "the UI is not consistent" — and the screenshots of those two pages
// are the proof.
import fs from 'node:fs';
import path from 'node:path';
import { rel } from '../util.mjs';

/** Roles that carry the app's primary interaction vocabulary. */
const HIGH_SIGNAL_ROLES = new Set([
  'btn-primary', 'btn', 'btn-secondary', 'btn-danger', 'btn-ghost', 'btn-sm',
  'input-text', 'input-search', 'select', 'textarea', 'badge',
]);

/** Style properties a user can actually see. */
const VISIBLE_PROPS = [
  'fontSize', 'lineHeight', 'fontWeight', 'padding', 'borderRadius',
  'border', 'color', 'background', 'height', 'minWidth', 'boxShadow',
];

/** Routes that are allowed to look different from the rest of the app. */
const EXCLUDED_GROUPS = new Set(['auth', 'stub']);

function dominantFingerprints(roles) {
  // roles: { role: { fingerprintKey: {fingerprint, count, samples} } }
  const out = {};
  for (const [role, variants] of Object.entries(roles || {})) {
    const entries = Object.entries(variants);
    if (!entries.length) continue;
    entries.sort((a, b) => b[1].count - a[1].count);
    const [key, val] = entries[0];
    out[role] = { key, fingerprint: val.fingerprint, count: val.count, samples: val.samples };
  }
  return out;
}

export function runRuntimeRules(manifest, probeDir) {
  const findings = [];
  const perRoute = [];

  for (const route of manifest.routes) {
    const probeFile = path.join(probeDir, `${route.slug}.json`);
    if (!fs.existsSync(probeFile)) continue;
    const probe = JSON.parse(fs.readFileSync(probeFile, 'utf8'));
    perRoute.push({
      route,
      roles: dominantFingerprints(probe.roles),
    });
  }

  // role -> [{ route, dominant }]
  const byRole = new Map();
  for (const entry of perRoute) {
    if (EXCLUDED_GROUPS.has(entry.route.group)) continue;
    for (const [role, dom] of Object.entries(entry.roles)) {
      if (!byRole.has(role)) byRole.set(role, []);
      byRole.get(role).push({ route: entry.route, dom });
    }
  }

  for (const [role, entries] of byRole) {
    if (entries.length < 2) continue;
    const distinct = new Map();
    for (const e of entries) {
      if (!distinct.has(e.dom.key)) distinct.set(e.dom.key, { fingerprint: e.dom.fingerprint, routes: [] });
      distinct.get(e.dom.key).routes.push(e);
    }
    if (distinct.size < 2) continue;

    // Which visible properties actually differ between the variants?
    const first = [...distinct.values()][0].fingerprint;
    const differing = VISIBLE_PROPS.filter((p) => {
      const values = new Set([...distinct.values()].map((v) => String(v.fingerprint[p])));
      return values.size > 1;
    });
    if (!differing.length) continue;

    const highSignal = HIGH_SIGNAL_ROLES.has(role);
    const groups = [...distinct.values()].map((v) => ({
      fingerprint: v.fingerprint,
      routes: v.routes.map((e) => ({
        path: e.route.path,
        name: e.route.name,
        screenshot: e.route.screenshot,
        sha256: e.route.sha256,
        count: e.dom.count,
        sample: e.dom.samples[0] || '',
      })),
    }));

    const propLabel = differing.map((p) => {
      const values = groups.map((g) => `${g.routes[0].path}: ${g.fingerprint[p]}`);
      return `- **${p}** — ${values.join(' ｜ ')}`;
    });

    findings.push({
      rule: 'R11',
      severity: highSignal ? 'P1' : 'P2',
      category: 'cross-page-style',
      signature: `R11::cross-page-style::${role}`,
      title: `「${role}」在 ${entries.length} 个页面的计算样式不一致`,
      detail:
        `同一个语义控件 \`${role}\` 在不同页面上被渲染成了 ${distinct.size} 种样式。` +
        `差异出现在：${differing.join('、')}。` +
        `用户在同一趟操作里来回切换页面时，会看到"同一个按钮/输入框忽然换了个尺寸"，` +
        `这是"整体不统一"最直接的观感来源。`,
      locations: groups.flatMap((g) =>
        g.routes.map((r) => ({ file: r.screenshot, line: 0, snippet: `${r.path} → ${r.sample}` })),
      ),
      evidence: groups.flatMap((g) =>
        g.routes.map((r) => ({ route: r.path, screenshot: r.screenshot, sha256: r.sha256, role })),
      ),
      suggestion:
        `把这些差异收敛到 index.css 的同一套类上：把 ${differing.join('/')} 这些属性提到共享规则里，` +
        `页面私有差异用额外的修饰类叠加，而不是各页各写各的。`,
      diff: propLabel,
    });
  }

  // Stub pages: not a consistency defect, but they should not be mistaken for
  // a finished surface either.
  const stubs = manifest.routes.filter((r) => r.placeholder);
  if (stubs.length) {
    findings.push({
      rule: 'R12',
      severity: 'P3',
      category: 'placeholder',
      signature: 'R12::placeholder::stubs',
      title: `${stubs.length} 个路由仍是占位页：${stubs.map((s) => s.path).join('、')}`,
      detail:
        `这几个路由渲染的是 PlaceholderPages 占位内容。它们没有真正的界面，` +
        `因此不参与跨页面样式对比——评审 UI 一致性时应把它们排除在范围外。`,
      locations: stubs.map((s) => ({ file: s.screenshot, line: 0, snippet: s.path })),
      evidence: stubs.map((s) => ({ route: s.path, screenshot: s.screenshot, sha256: s.sha256 })),
      suggestion: `要么实现这两个页面，要么在导航里先隐藏入口，避免用户点到空页面。`,
    });
  }

  findings.forEach((f, i) => {
    f.id = `R11-${String(i + 1).padStart(3, '0')}`;
    f.source = 'runtime';
  });
  return { findings, routesCompared: perRoute.length };
}
