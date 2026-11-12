// util.mjs — shared helpers for the UI audit tool.
//
// Everything here is dependency-free (node builtins only) so the `check`
// mode can run in CI without installing a browser driver.
import fs from 'node:fs';
import path from 'node:path';
import crypto from 'node:crypto';
import { fileURLToPath } from 'node:url';
import { execFile } from 'node:child_process';

const HERE = path.dirname(fileURLToPath(import.meta.url));

/** Repo root: scripts/ui-audit -> scripts -> <repo> */
export const REPO_ROOT = path.resolve(HERE, '..', '..');
export const FRONTEND_SRC = path.join(REPO_ROOT, 'frontend', 'src');

export function ensureDir(p) {
  fs.mkdirSync(p, { recursive: true });
  return p;
}

export function readJSON(file, fallback = null) {
  try {
    return JSON.parse(fs.readFileSync(file, 'utf8'));
  } catch {
    return fallback;
  }
}

export function writeJSON(file, data) {
  ensureDir(path.dirname(file));
  fs.writeFileSync(file, JSON.stringify(data, null, 2) + '\n');
}

/** Recursively collect files under dir matching `test`. */
export function walk(dir, out = [], test = () => true) {
  if (!fs.existsSync(dir)) return out;
  for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
    const p = path.join(dir, entry.name);
    if (entry.isDirectory()) walk(p, out, test);
    else if (test(p)) out.push(p);
  }
  return out;
}

export function rel(p) {
  return path.relative(REPO_ROOT, p).split(path.sep).join('/');
}

/**
 * Run a command and capture its output. Never throws on non-zero exit —
 * callers inspect `code` and decide whether the failure is fatal.
 */
export function run(cmd, args, opts = {}) {
  return new Promise((resolve) => {
    execFile(
      cmd,
      args,
      {
        cwd: opts.cwd || REPO_ROOT,
        env: { ...process.env, ...(opts.env || {}) },
        maxBuffer: opts.maxBuffer || 32 * 1024 * 1024,
        timeout: opts.timeout || 0,
        encoding: 'utf8',
      },
      (err, stdout, stderr) => {
        resolve({
          code: err ? (err.code ?? 1) : 0,
          stdout: stdout || '',
          stderr: stderr || '',
          error: err || null,
        });
      },
    );
  });
}

export async function git(args, opts = {}) {
  return run('git', args, opts);
}

export async function gitCommit() {
  const r = await git(['rev-parse', 'HEAD']);
  return r.stdout.trim();
}

export function slugify(s) {
  return String(s)
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-+|-+$/g, '')
    .slice(0, 60);
}

/** Stable short hash used for evidence fingerprints. */
export function sha256(buf) {
  return crypto.createHash('sha256').update(buf).digest('hex');
}

/** Normalize a CSS color literal to a comparable form. */
export function normalizeColor(value) {
  if (!value) return '';
  let v = String(value).trim().toLowerCase();
  const rgb = v.match(/^rgba?\(\s*([\d.]+)[\s,]+([\d.]+)[\s,]+([\d.]+)(?:[\s,/]+([\d.%]+))?\s*\)$/);
  if (rgb) {
    const [r, g, b] = [rgb[1], rgb[2], rgb[3]].map((n) => Math.round(Number(n)));
    const a = rgb[4] === undefined ? 1 : rgb[4].endsWith('%') ? Number(rgb[4].slice(0, -1)) / 100 : Number(rgb[4]);
    if (a === 1) return `#${[r, g, b].map((n) => n.toString(16).padStart(2, '0')).join('')}`;
    return `rgba(${r},${g},${b},${Math.round(a * 100) / 100})`;
  }
  const hex = v.match(/^#([0-9a-f]{3,8})$/);
  if (hex) {
    let h = hex[1];
    if (h.length === 3) h = h.split('').map((c) => c + c).join('');
    if (h.length === 6) return `#${h}`;
    if (h.length === 8) return `#${h.slice(0, 6)}@${Math.round((parseInt(h.slice(6), 16) / 255) * 100) / 100}`;
  }
  return v;
}

/** All color literals in a source text, with their offsets. */
export function findColorLiterals(text) {
  const out = [];
  const re = /#[0-9a-fA-F]{3,8}\b|\brgba?\(\s*[\d.]+\s*[, ]\s*[\d.]+\s*[, ]\s*[\d.]+(?:\s*[,/]\s*[\d.%]+)?\s*\)/g;
  let m;
  while ((m = re.exec(text))) out.push({ raw: m[0], index: m.index, normalized: normalizeColor(m[0]) });
  return out;
}

/** line number (1-based) of a character offset */
export function lineOf(text, index) {
  let line = 1;
  for (let i = 0; i < index && i < text.length; i++) if (text[i] === '\n') line++;
  return line;
}

export function lineCount(text) {
  let n = 1;
  for (let i = 0; i < text.length; i++) if (text[i] === '\n') n++;
  return n;
}

/**
 * Map an artifact-relative screenshot path to its path on the evidence
 * branch.
 *
 * Locally a screenshot lives at artifacts/ui-audit/<run-id>/screenshots/x.png;
 * on the branch it is committed to docs/ui-audit/<run-id>/screenshots/x.png.
 * The two prefixes differ, so every raw URL has to go through here.
 */
export function evidenceRelPath(screenshot, runId, prefix = 'docs/ui-audit', artifactsPrefix = 'artifacts/ui-audit') {
  const s = String(screenshot || '');
  const marker = `${artifactsPrefix.replace(/\/+$/, '')}/${runId}/`;
  const i = s.indexOf(marker);
  // Fall back to the last two segments ("screenshots/root.png") — the shape
  // every artifact path has — rather than leaking the local prefix.
  const tail = i >= 0 ? s.slice(i + marker.length) : s.split('/').filter(Boolean).slice(-2).join('/');
  return `${prefix}/${runId}/${tail}`;
}

/** Extract the `--- why ...` trailer block the project asks commits to carry. */
export function whyTrailer(paragraphs) {
  const body = paragraphs.filter(Boolean).join('\n');
  if (!body) return '';
  return `\n\nwhy: ${body.replace(/\n+/g, ' ').trim()}\n`;
}
