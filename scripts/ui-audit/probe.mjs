// probe.mjs — the in-page collector.
//
// This function is serialised into the browser by Playwright, so it must be
// fully self-contained: no imports, no closure over module scope, no
// Node APIs. Everything it needs arrives through its argument.
//
// The probe answers one question: "on THIS route, what does each kind of
// control actually look like?" It groups elements into semantic roles
// (btn-primary, input-text, card, badge, ...) and records a style
// fingerprint for each. The runtime rule then compares fingerprints for
// the same role across routes — that comparison is what surfaces
// "the same button looks different on different pages".

/* eslint-disable no-undef */
export function collectProbe() {
  const MAX_ELEMENTS = 4000;

  function px(value) {
    if (!value) return '';
    const m = String(value).match(/^(-?[\d.]+)px$/);
    return m ? m[1] : String(value);
  }

  function roleOf(el) {
    const tag = el.tagName.toLowerCase();
    const cls = (el.getAttribute('class') || '').split(/\s+/).filter(Boolean);
    const has = (name) => cls.some((c) => c === name || c.includes(name));

    if (tag === 'button' || el.getAttribute('role') === 'button' || has('btn')) {
      if (has('btn-primary')) return 'btn-primary';
      if (has('btn-danger') || has('danger')) return 'btn-danger';
      if (has('btn-ghost') || has('btn-link')) return 'btn-ghost';
      if (has('btn-secondary')) return 'btn-secondary';
      if (has('btn-sm') || has('btn-xs')) return 'btn-sm';
      if (has('btn-icon')) return 'btn-icon';
      if (has('btn')) return 'btn';
      return 'btn';
    }
    if (tag === 'input') {
      const type = (el.getAttribute('type') || 'text').toLowerCase();
      if (type === 'checkbox' || type === 'radio') return 'input-choice';
      if (type === 'submit' || type === 'button' || type === 'reset') return 'input-submit';
      if (type === 'search') return 'input-search';
      return 'input-text';
    }
    if (tag === 'select') return 'select';
    if (tag === 'textarea') return 'textarea';
    if (tag === 'a') return 'link';
    if (has('card')) return 'card';
    if (has('modal')) return 'modal';
    if (has('badge') || has('chip') || has('tag')) return 'badge';
    if (has('table')) return 'table';
    if (has('header')) return 'header';
    if (has('panel') || has('section')) return 'panel';
    return '';
  }

  function fingerprint(el) {
    const cs = getComputedStyle(el);
    return {
      fontSize: px(cs.fontSize),
      fontWeight: cs.fontWeight,
      lineHeight: px(cs.lineHeight),
      fontFamily: (cs.fontFamily || '').split(',')[0].replace(/["']/g, ''),
      padding: [px(cs.paddingTop), px(cs.paddingRight), px(cs.paddingBottom), px(cs.paddingLeft)].join(' '),
      borderRadius: px(cs.borderTopLeftRadius),
      border: [px(cs.borderTopWidth), px(cs.borderRightWidth), px(cs.borderBottomWidth), px(cs.borderLeftWidth)].join(' '),
      borderColor: cs.borderTopColor,
      color: cs.color,
      background: cs.backgroundColor,
      height: px(cs.height),
      minWidth: px(cs.minWidth),
      boxShadow: cs.boxShadow === 'none' ? 'none' : cs.boxShadow,
      display: cs.display,
    };
  }

  function fingerprintKey(fp) {
    return Object.keys(fp).map((k) => `${k}=${fp[k]}`).join('|');
  }

  function describe(el) {
    const cls = (el.getAttribute('class') || '').split(/\s+/).filter(Boolean).slice(0, 3).join('.');
    const text = (el.textContent || '').trim().replace(/\s+/g, ' ').slice(0, 40);
    return `${el.tagName.toLowerCase()}${cls ? '.' + cls : ''}${text ? ` "${text}"` : ''}`;
  }

  const roles = {};
  const all = document.querySelectorAll('*');
  let seen = 0;
  for (const el of all) {
    if (seen >= MAX_ELEMENTS) break;
    const role = roleOf(el);
    if (!role) continue;
    seen++;
    const fp = fingerprint(el);
    const key = fingerprintKey(fp);
    if (!roles[role]) roles[role] = {};
    if (!roles[role][key]) {
      roles[role][key] = { fingerprint: fp, count: 0, samples: [] };
    }
    const bucket = roles[role][key];
    bucket.count++;
    if (bucket.samples.length < 3) bucket.samples.push(describe(el));
  }

  const root = getComputedStyle(document.documentElement);
  const body = document.body ? getComputedStyle(document.body) : null;

  return {
    url: location.href,
    title: document.title,
    page: {
      rootFontSize: px(root.fontSize),
      rootColor: root.color,
      rootBackground: root.backgroundColor,
      bodyFontSize: body ? px(body.fontSize) : '',
      bodyColor: body ? body.color : '',
      bodyBackground: body ? body.backgroundColor : '',
      bodyLineHeight: body ? px(body.lineHeight) : '',
      bodyFontFamily: body ? (body.fontFamily || '').split(',')[0].replace(/["']/g, '') : '',
    },
    elementCount: document.querySelectorAll('*').length,
    textLength: (document.body ? document.body.innerText : '').trim().length,
    roles,
  };
}
