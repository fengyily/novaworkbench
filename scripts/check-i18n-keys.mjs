#!/usr/bin/env node
// check-i18n-keys.mjs — verifies that every translation key referenced in the
// frontend source exists in BOTH resource trees (zh-CN + en-US), and that the
// two trees mirror each other key-for-key.
//
// Usage: node scripts/check-i18n-keys.mjs   (from the repo root)
// Exit 1 on any problem — a missing key renders the raw key string to the
// user, which is worse than an untranslated string.
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { pathToFileURL } from 'node:url';

const root = process.cwd();
const src = path.join(root, 'frontend', 'src');

function walk(dir, out = []) {
  for (const e of fs.readdirSync(dir, { withFileTypes: true })) {
    const p = path.join(dir, e.name);
    if (e.isDirectory()) walk(p, out);
    else if (/\.(tsx?|jsx?)$/.test(e.name)) out.push(p);
  }
  return out;
}

// Literal keys used via t('...') / tk('...') / tr('...') / i18next.t('...')
const used = new Map();
const sourceFiles = walk(src).filter((f) => !f.includes(path.join(src, 'i18n', 'locales')));
for (const f of sourceFiles) {
  const s = fs.readFileSync(f, 'utf8');
  const re = /\b(?:t|tk|tr|i18next\.t)\(\s*['"`]([a-zA-Z][a-zA-Z0-9_.]+)['"`]/g;
  let m;
  while ((m = re.exec(s))) {
    const k = m[1];
    if (!used.has(k)) used.set(k, []);
    used.get(k).push(path.relative(src, f));
  }
}

// Dynamic prefixes: t(`foo.${x}`) — the prefix must exist as a branch.
const prefixes = [];
for (const f of sourceFiles) {
  const s = fs.readFileSync(f, 'utf8');
  const re = /\b(?:t|tk|tr)\(\s*`([a-zA-Z][a-zA-Z0-9_.]*)\$\{/g;
  let m;
  while ((m = re.exec(s))) prefixes.push({ prefix: m[1], file: path.relative(src, f) });
}

// The locale modules import each other without extensions (bundler
// resolution). Node ESM needs explicit extensions, so materialise a copy of
// the trees in a temp dir with ".ts" appended to every relative import.
function materialise(fromDir, toDir) {
  fs.mkdirSync(toDir, { recursive: true });
  for (const e of fs.readdirSync(fromDir, { withFileTypes: true })) {
    const from = path.join(fromDir, e.name);
    const to = path.join(toDir, e.name);
    if (e.isDirectory()) {
      materialise(from, to);
      continue;
    }
    if (!e.name.endsWith('.ts')) continue;
    const text = fs.readFileSync(from, 'utf8');
    const out = text.replace(/(from\s+['"])(\.\/[^'"]+)(['"])/g, '$1$2.ts$3');
    fs.writeFileSync(to, out);
  }
  return toDir;
}

const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'i18nkeys-'));
try {
  const zhDir = materialise(path.join(src, 'i18n/locales'), path.join(tmp, 'zh'));
  const enDir = materialise(path.join(src, 'i18n/locales'), path.join(tmp, 'en'));
  const zh = (await import(pathToFileURL(path.join(zhDir, 'zh-CN.ts')))).default;
  const en = (await import(pathToFileURL(path.join(enDir, 'en-US.ts')))).default;

  const flat = (obj, prefix = '', out = new Set()) => {
    for (const [k, v] of Object.entries(obj ?? {})) {
      const key = prefix ? `${prefix}.${k}` : k;
      if (v && typeof v === 'object') flat(v, key, out);
      else out.add(key);
    }
    return out;
  };
  const zhKeys = flat(zh);
  const enKeys = flat(en);

  let bad = 0;
  const problems = [];
  for (const [key, files] of [...used.entries()].sort()) {
    if (!zhKeys.has(key)) {
      problems.push(`MISSING zh-CN: ${key}  (${files[0]})`);
      bad++;
    } else if (!enKeys.has(key)) {
      problems.push(`MISSING en-US: ${key}  (${files[0]})`);
      bad++;
    }
  }
  for (const { prefix, file } of prefixes) {
    const hit = [...zhKeys].some((k) => k.startsWith(prefix));
    if (!hit) {
      problems.push(`MISSING zh-CN prefix: ${prefix}*  (${file})`);
      bad++;
    }
  }
  // Tree parity (belt & braces with the dev runtime assertion).
  for (const k of zhKeys) if (!enKeys.has(k)) { problems.push(`TREE en-US lacks: ${k}`); bad++; }
  for (const k of enKeys) if (!zhKeys.has(k)) { problems.push(`TREE zh-CN lacks: ${k}`); bad++; }

  if (bad) {
    console.error(`check-i18n-keys: ${bad} problem(s)`);
    for (const line of problems) console.error('  ' + line);
    process.exit(1);
  }
  console.log(`check-i18n-keys: OK (${zhKeys.size} keys, ${used.size} referenced)`);
} finally {
  fs.rmSync(tmp, { recursive: true, force: true });
}
