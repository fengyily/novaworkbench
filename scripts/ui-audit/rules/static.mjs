// rules/static.mjs — deterministic findings from the frontend source.
//
// These rules never call a model and never touch a browser: they read the
// files and compare them against the design tokens in index.css. That makes
// them reproducible and cheap enough to run as a CI gate.
//
// Every rule returns findings shaped like:
//   { id, rule, severity, category, title, detail, locations[], evidence[], suggestion }
import fs from 'node:fs';
import path from 'node:path';
import {
  FRONTEND_SRC,
  walk,
  rel,
  readJSON,
  findColorLiterals,
  normalizeColor,
  lineOf,
} from '../util.mjs';
import { ROUTE_SNAPSHOT, readAppRoutes } from '../routes.mjs';

const CSS_FILES = walk(FRONTEND_SRC, [], (p) => p.endsWith('.css'));
const TSX_FILES = walk(FRONTEND_SRC, [], (p) => p.endsWith('.tsx') || p.endsWith('.ts'));

const INDEX_CSS = path.join(FRONTEND_SRC, 'index.css');

/* ─────────────────────────── design tokens ─────────────────────────── */

/** Color-valued custom properties declared in the global stylesheet. */
function readGlobalTokens() {
  const text = fs.readFileSync(INDEX_CSS, 'utf8');
  const tokens = new Map(); // normalized value -> { names[], raw }
  const re = /(--[\w-]+)\s*:\s*([^;]+);/g;
  let m;
  while ((m = re.exec(text))) {
    const name = m[1];
    const raw = m[2].trim();
    for (const lit of findColorLiterals(raw)) {
      const key = lit.normalized;
      if (!tokens.has(key)) tokens.set(key, { names: [], raw: lit.raw, file: rel(INDEX_CSS), line: lineOf(text, m.index) });
      tokens.get(key).names.push(name);
    }
  }
  return tokens;
}

/* ─────────────────────────── source scanning ───────────────────────── */

/** Every class selector defined anywhere in the stylesheets. */
function readDefinedClasses() {
  const defined = new Map(); // class -> [{file, line}]
  for (const file of CSS_FILES) {
    const text = fs.readFileSync(file, 'utf8');
    // Strip block comments so example selectors inside comments do not count.
    const clean = text.replace(/\/\*[\s\S]*?\*\//g, (m) => m.replace(/[^\n]/g, ' '));
    const re = /\.(-?[_a-zA-Z][\w-]*)/g;
    let m;
    while ((m = re.exec(clean))) {
      const cls = m[1];
      if (!defined.has(cls)) defined.set(cls, []);
      defined.get(cls).push({ file: rel(file), line: lineOf(clean, m.index) });
    }
  }
  return defined;
}

/** A usable CSS class name — anything else came from a mis-parsed expression. */
const CLASS_NAME_RE = /^[A-Za-z_][\w-]*$/;

/**
 * Split a template literal into its static text and the contents of its
 * `${...}` holes.
 *
 * The static text is class-name material; a hole usually is not — but a
 * hole can carry a whole class list (`${x ? 'a b' : ''}`), so its quoted
 * literals are inspected too. A trailing "-" in the static text means the
 * class name continues inside the hole (`status-${kind}`), so that token is
 * not a class on its own.
 */
function splitTemplate(text) {
  const statics = [];
  const holeLiterals = [];
  let buf = '';
  let i = 0;
  while (i < text.length) {
    if (text[i] === '$' && text[i + 1] === '{') {
      if (buf) statics.push(buf);
      buf = '';
      let depth = 1;
      const start = i + 2;
      i += 2;
      while (i < text.length && depth > 0) {
        if (text[i] === '{') depth++;
        else if (text[i] === '}') depth--;
        if (depth > 0) i++;
      }
      const hole = text.slice(start, i);
      for (const s of hole.match(/'([^']*)'|"([^"]*)"/g) || []) holeLiterals.push(s.slice(1, -1));
      i++; // consume the closing brace
      continue;
    }
    buf += text[i];
    i++;
  }
  if (buf) statics.push(buf);
  return { statics, holeLiterals };
}

/** Every class name referenced from JSX, with the file/line it came from. */
function readUsedClasses() {
  const used = new Map(); // class -> [{file, line}]
  const add = (cls, file, line) => {
    if (!cls || !CLASS_NAME_RE.test(cls)) return;
    // A token that ends with "-" is the static prefix of a dynamic class
    // (`status-${kind}`); the real name is only known at runtime.
    if (cls.endsWith('-')) return;
    if (!used.has(cls)) used.set(cls, []);
    used.get(cls).push({ file, line });
  };

  for (const file of TSX_FILES) {
    const text = fs.readFileSync(file, 'utf8');
    // className="a b"
    const plain = /className\s*=\s*"([^"]*)"/g;
    let m;
    while ((m = plain.exec(text))) {
      const line = lineOf(text, m.index);
      for (const cls of m[1].split(/\s+/)) add(cls, rel(file), line);
    }
    // className={...} — pull every string literal out of the expression.
    const expr = /className\s*=\s*\{/g;
    while ((m = expr.exec(text))) {
      const start = m.index + m[0].length;
      let depth = 1;
      let i = start;
      while (i < text.length && depth > 0) {
        if (text[i] === '{') depth++;
        else if (text[i] === '}') depth--;
        i++;
      }
      const body = text.slice(start, i - 1);
      const line = lineOf(text, m.index);

      // Template literals: static text plus class-like literals from holes.
      for (const s of body.match(/`([^`]*)`/g) || []) {
        const { statics, holeLiterals } = splitTemplate(s.slice(1, -1));
        for (const st of statics) for (const cls of st.split(/\s+/)) add(cls, rel(file), line);
        for (const lit of holeLiterals) {
          // A single bare word inside a hole is far more often a *value*
          // (`${k.category || 'general'}`) than a class name, so it is only
          // taken when it looks like a class list or a known state class.
          if (lit.includes(' ') || lit.includes('-') || STATE_CLASSES.has(lit)) {
            for (const cls of lit.split(/\s+/)) add(cls, rel(file), line);
          }
        }
      }

      // Plain quoted literals outside any template.
      const withoutTemplates = body.replace(/`[^`]*`/g, '');
      for (const s of withoutTemplates.match(/'([^']*)'|"([^"]*)"/g) || []) {
        for (const cls of s.slice(1, -1).split(/\s+/)) add(cls, rel(file), line);
      }
    }
  }
  return used;
}

/* ─────────────────────────── the rules ─────────────────────────── */

function ruleRouteDrift() {
  const findings = [];
  const literals = new Set(readAppRoutes());
  const declared = new Set(ROUTE_SNAPSHOT);

  // App.tsx declares most routes relative to their parent (children of the
  // layout route are "wizard"/"projects", children of "settings" are
  // "users"/"acl", ...). Resolving the nesting by hand would need a real
  // parser, so coverage is decided by suffix matching instead: a literal is
  // covered when the snapshot holds it directly, holds it with a leading
  // slash, or holds it as the tail of a longer path.
  const isCovered = (lit) => {
    if (declared.has(lit)) return true;
    const withSlash = lit.startsWith('/') ? lit : `/${lit}`;
    if (declared.has(withSlash)) return true;
    for (const d of declared) if (d.endsWith(`/${lit}`)) return true;
    return false;
  };

  const uncovered = [...literals].filter((l) => !isCovered(l));
  if (uncovered.length) {
    findings.push({
      rule: 'R0',
      severity: 'P2',
      category: 'audit-scope',
      signature: `R0::audit-scope::uncovered:${uncovered.slice().sort().join(',')}`,
      title: `${uncovered.length} 个路由未纳入审计清单`,
      detail:
        `frontend/src/App.tsx 里声明了 ${uncovered.map((l) => `"${l}"`).join('、')}，但 scripts/ui-audit/routes.mjs 的 ROUTE_SNAPSHOT 没有覆盖它们。` +
        `这些页面不会被截图，也不会参与跨页面样式对比——审计范围会静默变小。`,
      locations: uncovered.map((l) => ({ file: 'frontend/src/App.tsx', line: 0, snippet: l })),
      evidence: [],
      suggestion: `把 ${uncovered.map((l) => `"${l}"`).join('、')} 加进 scripts/ui-audit/routes.mjs 的 ROUTE_SNAPSHOT（动态路由同时加进 DYNAMIC_ROUTES）。`,
    });
  }

  const stale = [...declared].filter((d) => {
    const tail = d.startsWith('/') ? d.slice(1) : d;
    if (literals.has(d) || literals.has(tail)) return false;
    for (const l of literals) if (d.endsWith(`/${l}`)) return false;
    return true;
  });
  if (stale.length) {
    findings.push({
      rule: 'R0',
      severity: 'P3',
      category: 'audit-scope',
      signature: `R0::audit-scope::stale:${stale.slice().sort().join(',')}`,
      title: `审计清单里的 ${stale.join('、')} 已不存在于 App.tsx`,
      detail: `ROUTE_SNAPSHOT 记录了这些路由，但 frontend/src/App.tsx 里已经没有了，采集时会得到 404 页面。`,
      locations: stale.map((s) => ({ file: 'scripts/ui-audit/routes.mjs', line: 0, snippet: s })),
      evidence: [],
      suggestion: `从 ROUTE_SNAPSHOT 里删除 ${stale.join('、')}。`,
    });
  }
  return findings;
}

function ruleOffPaletteColors(tokens) {
  const byValue = new Map(); // normalized -> { locations[], count }
  for (const file of [...CSS_FILES, ...TSX_FILES]) {
    const text = fs.readFileSync(file, 'utf8');
    for (const lit of findColorLiterals(text)) {
      // A custom-property declaration IS a token, not an off-palette usage.
      const before = text.slice(Math.max(0, lit.index - 60), lit.index);
      if (/--[\w-]+\s*:\s*[^;{]*$/.test(before)) continue;
      if (tokens.has(lit.normalized)) continue;
      if (!byValue.has(lit.normalized)) byValue.set(lit.normalized, { count: 0, locations: [] });
      const bucket = byValue.get(lit.normalized);
      bucket.count++;
      if (bucket.locations.length < 6) {
        bucket.locations.push({ file: rel(file), line: lineOf(text, lit.index), snippet: lit.raw });
      }
    }
  }
  if (!byValue.size) return [];

  const rows = [...byValue.entries()].sort((a, b) => b[1].count - a[1].count);
  const total = rows.reduce((n, [, v]) => n + v.count, 0);
  const locations = [];
  for (const [, v] of rows) for (const l of v.locations) locations.push(l);

  return [{
    rule: 'R1',
    severity: 'P2',
    category: 'color',
    signature: 'R1::color::off-palette',
    title: `${rows.length} 种颜色不在设计令牌里，共 ${total} 处硬编码`,
    detail:
      `frontend/src/index.css 的 :root 只定义了 ${tokens.size} 个色值令牌，但代码里还散落着 ${rows.length} 种调色板外的颜色（合计 ${total} 处）。` +
      `这些色值不会随令牌调整一起变化——改一次主题色，别处不会跟着动，是"同一个东西在不同页面颜色不一样"的主要来源。` +
      `出现最多的几种：${rows.slice(0, 8).map(([v, d]) => `${v}（${d.count} 处）`).join('、')}。`,
    locations: locations.slice(0, 24),
    evidence: rows.slice(0, 30).map(([v, d]) => ({ file: d.locations[0]?.file || '', value: v, count: d.count })),
    suggestion: `在 index.css 的 :root 里为这 ${rows.length} 种颜色补上语义令牌（--color-<用途>），再逐处改成 var(--color-<用途>)。`,
    table: rows.map(([v, d]) => `| \`${v}\` | ${d.count} | ${d.locations.map((l) => `${l.file}:${l.line}`).join('<br>')} |`),
    tableHeader: ['颜色', '出现次数', '位置'],
  }];
}

function ruleTokenValuesWrittenLiterally(tokens) {
  const byName = new Map(); // token name -> { locations[], count }
  for (const file of [...CSS_FILES, ...TSX_FILES]) {
    const text = fs.readFileSync(file, 'utf8');
    for (const lit of findColorLiterals(text)) {
      const token = tokens.get(lit.normalized);
      if (!token) continue;
      const before = text.slice(Math.max(0, lit.index - 60), lit.index);
      // Skip the token's own declaration.
      const declRe = new RegExp(`${token.names[0].replace(/[.*+?^${}()|[\]\\]/g, '\\$&')}\\s*:\\s*[^;{]*$`);
      if (declRe.test(before)) continue;
      for (const name of token.names) {
        if (!byName.has(name)) byName.set(name, { count: 0, locations: [], value: lit.normalized, raw: lit.raw });
        const bucket = byName.get(name);
        bucket.count++;
        if (bucket.locations.length < 8) {
          bucket.locations.push({ file: rel(file), line: lineOf(text, lit.index), snippet: lit.raw });
        }
      }
    }
  }
  if (!byName.size) return [];

  const rows = [...byName.entries()].sort((a, b) => b[1].count - a[1].count);
  const total = rows.reduce((n, [, v]) => n + v.count, 0);
  const locations = [];
  for (const [, v] of rows) for (const l of v.locations) locations.push(l);

  return [{
    rule: 'R2',
    severity: 'P2',
    category: 'color',
    signature: 'R2::color::literal-token',
    title: `${rows.length} 个令牌的值被写死 ${total} 处，而不是引用 var(--token)`,
    detail:
      `这些位置直接写了令牌对应的色值字面量，而不是 var(--token) 引用。` +
      `一旦调整令牌本身，这些位置不会跟着变——正是"改了一处、别处没跟上"的不一致来源。` +
      `其中 index.css 自身就有 ${rows.filter(([, v]) => v.locations.some((l) => l.file === 'frontend/src/index.css')).length} 个令牌被就地写死。`,
    locations: locations.slice(0, 24),
    evidence: rows.map(([name, v]) => ({ file: v.locations[0]?.file || '', value: name, count: v.count })),
    suggestion: `把这 ${total} 处替换成对应的 var(--token) 引用；index.css 里的规则尤其应当走令牌，否则改令牌等于没改。`,
    table: rows.map(([name, v]) => `| \`var(${name})\` | ${v.count} | ${v.locations.map((l) => `${l.file}:${l.line}`).join('<br>')} |`),
    tableHeader: ['应替换为', '出现次数', '位置'],
  }];
}

function ruleDuplicateColorSpellings() {
  const byValue = new Map();
  for (const file of [...CSS_FILES, ...TSX_FILES]) {
    const text = fs.readFileSync(file, 'utf8');
    for (const lit of findColorLiterals(text)) {
      if (!byValue.has(lit.normalized)) byValue.set(lit.normalized, new Map());
      const spellings = byValue.get(lit.normalized);
      if (!spellings.has(lit.raw)) spellings.set(lit.raw, { file: rel(file), line: lineOf(text, lit.index), count: 0 });
      spellings.get(lit.raw).count++;
    }
  }
  const rows = [...byValue.entries()].filter(([, s]) => s.size >= 2);
  if (!rows.length) return [];

  const locations = [];
  for (const [, spellings] of rows) {
    for (const s of spellings.values()) locations.push({ file: s.file, line: s.line, snippet: `${s.count}×` });
  }

  return [{
    rule: 'R3',
    severity: 'P3',
    category: 'color',
    signature: 'R3::color::multi-spelling',
    title: `${rows.length} 个颜色各有多种写法（大小写 / 简写不一致）`,
    detail:
      `同一个颜色值在不同地方用了不同的字面量写法，例如 #fff 与 #ffffff、#64748B 与 #64748b。` +
      `搜索替换时容易漏掉其中一种，也让"这两个色是不是同一个"难以一眼判断。` +
      `例如：${rows.slice(0, 6).map(([v, s]) => `${v} → ${[...s.keys()].join(' / ')}`).join('；')}。`,
    locations: locations.slice(0, 24),
    evidence: rows.slice(0, 20).map(([v]) => ({ value: v })),
    suggestion: `统一为小写 6 位十六进制写法，并让所有位置走同一个 var(--token)。`,
    table: rows.map(([v, s]) => `| \`${v}\` | ${[...s.keys()].map((k) => `\`${k}\``).join(' / ')} |`),
    tableHeader: ['归一化颜色', '出现的写法'],
  }];
}

/** Runtime state classes that are toggled in JS and never appear in CSS. */
const STATE_CLASSES = new Set([
  'active', 'selected', 'open', 'closed', 'disabled', 'loading', 'error', 'hover', 'focus',
  'checked', 'expanded', 'collapsed', 'hidden', 'visible', 'current', 'dragging', 'sortable',
  'editable', 'readonly', 'busy', 'done', 'primary', 'muted', 'danger', 'warning', 'success',
]);

function ruleUndefinedClasses(defined, used) {
  const findings = [];
  const missing = [];
  for (const [cls, locs] of used) {
    if (defined.has(cls)) continue;
    if (STATE_CLASSES.has(cls)) continue;
    missing.push({ cls, locs });
  }
  missing.sort((a, b) => b.locs.length - a.locs.length);
  for (const { cls, locs } of missing) {
    if (locs.length < 3) continue; // one-off typos are noise, not a family
    findings.push({
      rule: 'R4',
      severity: 'P2',
      category: 'missing-style',
      signature: `R4::missing-style::.${cls}`,
      title: `类名 .${cls} 被用了 ${locs.length} 处，但没有任何样式表定义它`,
      detail:
        `frontend/src 里引用了 .${cls}（${locs.length} 处，分布在 ${new Set(locs.map((l) => l.file)).size} 个文件），` +
        `但 frontend/src/**/*.css 里没有任何选择器定义它。用了没有定义的类名，元素会退化成浏览器默认样式——` +
        `同一个控件在不同页面就会呈现出不同的默认外观。`,
      locations: locs.slice(0, 10).map((l) => ({ file: l.file, line: l.line, snippet: cls })),
      evidence: locs.slice(0, 10).map((l) => ({ file: l.file, line: l.line })),
      suggestion: `要么在 index.css 里补上 .${cls} 的定义（并纳入 .btn-* / .card-* 这类通用规范），要么把它换成已有的规范类名。`,
    });
  }
  return findings;
}

function ruleNamingConventionDrift(used) {
  const findings = [];
  const families = [
    { noun: 'btn', prefix: /^btn-/, suffix: /-btn$/ },
    { noun: 'card', prefix: /^card-/, suffix: /-card$/ },
    { noun: 'input', prefix: /^input-/, suffix: /-input$/ },
    { noun: 'panel', prefix: /^panel-/, suffix: /-panel$/ },
    { noun: 'badge', prefix: /^badge-/, suffix: /-badge$/ },
  ];
  for (const fam of families) {
    const suffixStyle = [];
    const prefixStyle = [];
    for (const [cls, locs] of used) {
      if (fam.suffix.test(cls) && !fam.prefix.test(cls)) suffixStyle.push({ cls, locs });
      else if (fam.prefix.test(cls)) prefixStyle.push({ cls, locs });
    }
    if (suffixStyle.length < 2) continue;
    suffixStyle.sort((a, b) => b.locs.length - a.locs.length);
    const prefixCount = prefixStyle.reduce((n, x) => n + x.locs.length, 0);
    const suffixCount = suffixStyle.reduce((n, x) => n + x.locs.length, 0);
    const files = new Set();
    for (const s of suffixStyle) for (const l of s.locs) files.add(l.file);
    const list = suffixStyle.slice(0, 10).map((s) => `.${s.cls}`).join('、') + (suffixStyle.length > 10 ? ' 等' : '');
    const headline = prefixStyle.length
      ? `${fam.noun} 类名存在两套写法：${prefixStyle.length} 个 \`${fam.noun}-*\` vs ${suffixStyle.length} 个 \`*-${fam.noun}\``
      : `${suffixStyle.length} 个 \`*-${fam.noun}\` 后缀式类名，仓库里没有对应的 \`${fam.noun}-*\` 前缀式约定`;
    findings.push({
      rule: 'R5',
      severity: 'P2',
      category: 'naming',
      signature: `R5::naming::${fam.noun}`,
      title: headline,
      detail:
        (prefixStyle.length
          ? `仓库主流是前缀式（${fam.noun}-*，共 ${prefixCount} 处引用），但还有 ${suffixStyle.length} 个后缀式类名（${suffixCount} 处引用，分布在 ${files.size} 个文件）：${list}。` +
            `两套写法并存时，想统一改一个${fam.noun}样式就得同时记住两种类名。`
          : `这些类名都用了后缀式 \`*-${fam.noun}\`（${suffixCount} 处引用，分布在 ${files.size} 个文件）：${list}。` +
            `而仓库里没有任何 \`${fam.noun}-*\` 前缀式类名，也就是说这一类元素从来没有被约定过统一写法——` +
            `每个页面都在自己发明名字，这正是不统一的来源。`) ,
      locations: suffixStyle
        .slice(0, 6)
        .flatMap((s) => s.locs.slice(0, 1).map((l) => ({ file: l.file, line: l.line, snippet: s.cls }))),
      evidence: suffixStyle.slice(0, 10).map((s) => ({ file: s.locs[0].file, line: s.locs[0].line, class: s.cls })),
      suggestion: prefixStyle.length
        ? `把后缀式类名统一改成前缀式（.${fam.noun}-*），或者在 index.css 里为后缀式类名补一份与前缀式等价的定义。`
        : `在 index.css 里定下 \`.${fam.noun}-*\` 前缀式约定，把这 ${suffixStyle.length} 个类名改过去；同名元素只保留一个共享基类。`,
      table: suffixStyle.map((s) => `| \`.${s.cls}\` | ${s.locs.length} | ${s.locs[0].file}:${s.locs[0].line} |`),
      tableHeader: ['类名', '引用次数', '首次出现'],
    });
  }
  return findings;
}

function ruleDuplicateContainerNames(used) {
  const findings = [];
  const groups = [
    { kind: 'box', re: /(^|-)box$/, label: '容器' },
    { kind: 'header', re: /(^|-)header$/, label: '区块标题' },
  ];
  for (const g of groups) {
    const hits = [...used.entries()].filter(([cls]) => g.re.test(cls) && !STATE_CLASSES.has(cls));
    if (hits.length < 3) continue;
    hits.sort((a, b) => b[1].length - a[1].length);
    const total = hits.reduce((n, [, l]) => n + l.length, 0);
    findings.push({
      rule: g.kind === 'box' ? 'R6' : 'R7',
      severity: 'P2',
      category: 'naming',
      signature: `${g.kind === 'box' ? 'R6' : 'R7'}::naming::${g.kind}`,
      title: `${g.label}有 ${hits.length} 套并存的类名（${total} 处引用）`,
      detail:
        `同一类${g.label}用了 ${hits.length} 个不同的类名：` +
        `${hits.slice(0, 10).map(([c, l]) => `.${c}（${l.length}）`).join('、')}${hits.length > 10 ? ' 等' : ''}。` +
        `它们多半想做同一件事（一个带边框圆角阴影的块），但各自定义了一套样式，视觉上就会漂移。`,
      locations: hits
        .slice(0, 8)
        .map(([c, l]) => ({ file: l[0].file, line: l[0].line, snippet: c })),
      evidence: hits.slice(0, 10).map(([c, l]) => ({ file: l[0].file, line: l[0].line, class: c })),
      suggestion: `抽一个通用类（例如 .${g.kind === 'box' ? 'surface-box' : 'section-header'}）写进 index.css，各页面改用它，页面私有差异再叠加自己的修饰类。`,
    });
  }
  return findings;
}

function ruleStatusVocabulary(defined, used) {
  const findings = [];
  const text = fs.readFileSync(INDEX_CSS, 'utf8');
  const badgeRe = /\.status-badge\.status-([a-z-]+)\s*\{/g;
  const names = [];
  let m;
  while ((m = badgeRe.exec(text))) names.push(m[1]);
  if (names.length < 4) return findings;

  // The vocabulary is expected to be one set. A second set of sibling
  // .status-badge.status-* rules is a parallel status system.
  const lifecycle = ['draft', 'analyzing', 'designing', 'designed', 'developing', 'done', 'archived'];
  const parallel = names.filter((n) => !lifecycle.includes(n));
  if (parallel.length >= 2) {
    const locations = [];
    for (const n of parallel) {
      const idx = text.indexOf(`.status-badge.status-${n}`);
      if (idx >= 0) locations.push({ file: 'frontend/src/index.css', line: lineOf(text, idx), snippet: n });
    }
    findings.push({
      rule: 'R8',
      severity: 'P1',
      category: 'status',
      signature: 'R8::status::parallel-vocabulary',
      title: `状态色存在两套并行词汇表（${lifecycle.length} 个需求状态 + ${parallel.length} 个运行状态）`,
      detail:
        `index.css 里 .status-badge.status-* 一共 ${names.length} 个，其中需求生命周期用 ` +
        `${lifecycle.join('/')}，另一套用的是 ${parallel.join('/')}。` +
        `两套共用同一个 .status-badge 基类但语义域不同（例如 "done" 与 "succeeded"、"archived" 与 "canceled" 视觉完全一样），` +
        `用户看到"一样的颜色"却对应不同含义，是最容易被误读的不一致。`,
      locations,
      evidence: locations.map((l) => ({ file: l.file, line: l.line })),
      suggestion: `给两套状态各自一个明确的基类（例如 .status-badge--req / .status-badge--run），或者把视觉上相同的两个状态合并成一个令牌。`,
    });
  }

  // Status-coloured classes that live outside the .status-badge system.
  const outsiders = [];
  for (const [cls, locs] of used) {
    if (!/^status-|^kind-badge/.test(cls)) continue;
    if (cls.startsWith('status-') && badgeRe.test(cls)) continue;
    if (defined.has(cls)) outsiders.push({ cls, locs });
  }
  const badgeReTest = /\bstatus-(?!badge\b)/;
  const realOutsiders = outsiders.filter((o) => !lifecycle.includes(o.cls.replace(/^status-/, '')));
  if (realOutsiders.length >= 2) {
    const locations = realOutsiders
      .slice(0, 10)
      .flatMap((o) => o.locs.slice(0, 1).map((l) => ({ file: l.file, line: l.line, snippet: o.cls })));
    findings.push({
      rule: 'R8',
      severity: 'P2',
      category: 'status',
      signature: `R8::status::outside-badge:${realOutsiders.map((o) => o.cls).sort().join(',')}`,
      title: `${realOutsiders.length} 个状态类名绕开了 .status-badge 体系`,
      detail:
        `这些类名承担状态着色，但不在 index.css 的 .status-badge.status-* 体系内：` +
        `${realOutsiders.map((o) => `.${o.cls}`).join('、')}。` +
        `旁路着色意味着同一状态在不同页面可能是不同颜色。`,
      locations,
      evidence: locations.map((l) => ({ file: l.file, line: l.line })),
      suggestion: `把这些类名收敛到 .status-badge 体系，或者在 index.css 里给它们统一引用状态色令牌。`,
    });
  }
  void badgeReTest;
  return findings;
}

function ruleInlineStyleDensity() {
  const findings = [];
  const rows = [];
  for (const file of TSX_FILES) {
    const text = fs.readFileSync(file, 'utf8');
    const count = (text.match(/style=\{\{/g) || []).length;
    if (count > 0) rows.push({ file: rel(file), count, line: lineOf(text, text.indexOf('style={{')) });
  }
  rows.sort((a, b) => b.count - a.count);
  const total = rows.reduce((n, r) => n + r.count, 0);
  const hot = rows.filter((r) => r.count >= 25);
  if (hot.length) {
    findings.push({
      rule: 'R9',
      severity: 'P2',
      category: 'inline-style',
      signature: 'R9::inline-style::density',
      title: `${hot.length} 个文件里塞了大量内联样式（合计 ${total} 处 style={{ }}）`,
      detail:
        `内联样式优先级高于任何 class 规则，所以同一套 .btn 样式在这些页面上会被就地改写，` +
        `造成"同一个按钮在不同页面内边距/圆角不一样"。` +
        `当前最集中的是：` +
        hot.map((h) => `${h.file}（${h.count} 处）`).join('、') +
        `，三者合计占全部内联样式的 ${Math.round((hot.reduce((n, h) => n + h.count, 0) / total) * 100)}%。`,
      locations: hot.map((h) => ({ file: h.file, line: h.line, snippet: `${h.count} 处 style={{ }}` })),
      evidence: hot.map((h) => ({ file: h.file, count: h.count })),
      suggestion: `把这些内联样式里出现多次的值（尤其是 padding / border-radius / color）提成 index.css 的令牌或修饰类。`,
    });
  }
  return findings;
}

function ruleMarkdownProseHost() {
  const findings = [];
  const hits = [];
  for (const file of TSX_FILES) {
    const text = fs.readFileSync(file, 'utf8');
    const re = /<MarkdownRender/g;
    let m;
    while ((m = re.exec(text))) {
      // The wrapper's className sits BEFORE the <MarkdownRender> tag, so the
      // search has to look backwards. Looking back a generous window keeps
      // the rule conservative: a false negative is a missed finding, a false
      // positive is a wrong issue.
      const back = text.slice(Math.max(0, m.index - 1200), m.index);
      if (/md-prose/.test(back)) continue;
      // Report the nearest enclosing className so the reader can see what
      // styling this render point is actually using instead.
      const classes = [...back.matchAll(/className\s*=\s*"([^"]*)"/g)];
      const nearest = classes.length ? classes[classes.length - 1][1] : '(none)';
      hits.push({ file: rel(file), line: lineOf(text, m.index), nearest });
    }
  }
  if (hits.length) {
    findings.push({
      rule: 'R10',
      severity: 'P2',
      category: 'markdown',
      signature: 'R10::markdown::host',
      title: `${hits.length} 处 <MarkdownRender> 没有挂在 .md-prose 上`,
      detail:
        `frontend/src/styles/markdown-prose.css 是全站唯一的 Markdown 排版来源，CLAUDE.md 里也写明了"不要再抄一份排版"。` +
        `但这些渲染点的外层用的是各自发明的类（${[...new Set(hits.map((h) => h.nearest))].slice(0, 5).map((c) => `\`${c}\``).join('、')}），` +
        `于是同一份 Markdown 在不同页面会呈现出不同的字号、行高与间距。`,
      locations: hits.map((h) => ({ file: h.file, line: h.line, snippet: h.nearest })),
      evidence: hits.map((h) => ({ file: h.file, line: h.line })),
      suggestion: `给这些渲染点的外层元素补上 md-prose 类；页面私有的差异用 --md-prose-size / --md-prose-lh 变量覆盖，不要另起一套排版。`,
      table: hits.map((h) => `| \`${h.nearest}\` | ${h.file}:${h.line} |`),
      tableHeader: ['当前外层类', '位置'],
    });
  }
  return findings;
}

/**
 * A class that reads like part of the shared vocabulary (btn-*, form-*,
 * modal-*, input-*, status-*) but is defined in a page-local stylesheet
 * instead of the global one.
 *
 * This is the sharpest version of "the UI is not consistent": a page that
 * does not import that stylesheet gets no styling at all for the class, so
 * the same button looks different depending on where it is used.
 */
const SHARED_VOCABULARY = /^(btn|form|modal|input|status|card|table|field|action)-/;

function ruleSharedVocabularyOutsideGlobal(defined, used) {
  const findings = [];
  const globalRel = rel(INDEX_CSS);
  const offenders = [];
  for (const [cls, locs] of defined) {
    if (!SHARED_VOCABULARY.test(cls)) continue;
    const files = [...new Set(locs.map((l) => l.file))];
    if (files.length === 1 && files[0] === globalRel) continue;
    if (files.includes(globalRel)) continue; // also in the global sheet — fine
    const uses = used.get(cls) || [];
    if (!uses.length) continue;
    offenders.push({ cls, locs, files, uses });
  }
  if (!offenders.length) return findings;

  offenders.sort((a, b) => b.uses.length - a.uses.length);
  const totalUses = offenders.reduce((n, o) => n + o.uses.length, 0);
  const files = [...new Set(offenders.flatMap((o) => o.files))];

  return [{
    rule: 'R13',
    severity: 'P1',
    category: 'shared-vocabulary',
    signature: 'R13::shared-vocabulary::outside-global',
    title: `${offenders.length} 个「通用」类名被定义在页面私有样式表里，共 ${totalUses} 处引用`,
    detail:
      `这些类名长得像全站通用规范（${offenders.slice(0, 6).map((o) => `.${o.cls}`).join('、')}），` +
      `但它们只定义在 ${files.map((f) => `\`${f}\``).join('、')} 这些页面私有样式表里，而不是全局的 frontend/src/index.css。` +
      `只要某个页面没有 import 那张样式表，它就用不到这条规则——` +
      `同一个 .${offenders[0].cls} 在设置页是圆角带阴影的，在别的页面就是浏览器默认样式。` +
      `这是"同一个按钮在不同页面长得不一样"最直接的成因。`,
    locations: offenders
      .slice(0, 10)
      .flatMap((o) => o.uses.slice(0, 1).map((u) => ({ file: u.file, line: u.line, snippet: `.${o.cls}` }))),
    evidence: offenders.slice(0, 12).map((o) => ({ file: o.files[0], class: o.cls, count: o.uses.length })),
    suggestion:
      `把这些通用类名的定义搬进 frontend/src/index.css（与 .btn-primary / .btn-danger 放在一起），` +
      `页面私有样式表只保留该页面真正独有的修饰类。`,
    table: offenders.map((o) => `| \`.${o.cls}\` | ${o.uses.length} | ${o.files.map((f) => `\`${f}\``).join('<br>')} |`),
    tableHeader: ['类名', '引用次数', '定义位置'],
  }];
}

/* ─────────────────────────── entry point ─────────────────────────── */

export function runStaticRules() {
  const tokens = readGlobalTokens();
  const defined = readDefinedClasses();
  const used = readUsedClasses();

  const findings = [
    ...ruleRouteDrift(),
    ...ruleOffPaletteColors(tokens),
    ...ruleTokenValuesWrittenLiterally(tokens),
    ...ruleDuplicateColorSpellings(),
    ...ruleUndefinedClasses(defined, used),
    ...ruleNamingConventionDrift(used),
    ...ruleDuplicateContainerNames(used),
    ...ruleStatusVocabulary(defined, used),
    ...ruleSharedVocabularyOutsideGlobal(defined, used),
    ...ruleInlineStyleDensity(),
    ...ruleMarkdownProseHost(),
  ];

  findings.forEach((f, i) => {
    f.id = `${f.rule}-${String(i + 1).padStart(3, '0')}`;
    f.source = 'static';
  });
  return { findings, stats: { tokenCount: tokens.size, definedClassCount: defined.size, usedClassCount: used.size } };
}

export { readGlobalTokens, readDefinedClasses, readUsedClasses };
