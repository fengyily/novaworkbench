/**
 * Unified icon system — single source of truth for every NovaWorkbench icon.
 *
 * Design rules (kept here so anyone adding a new icon stays consistent):
 *
 *   1. 24x24 viewBox on every glyph. Consumers size via the `size` prop
 *      (defaults to 16) or via the surrounding `.nw-icon` container.
 *   2. 1.5 stroke-width on outline icons, with stroke-linecap="round" and
 *      stroke-linejoin="round" for a friendly, modern feel that mirrors
 *      the Heroicons / Lucide style. No filled silhouettes — outline
 *      keeps the look calm next to text and the slate/indigo palette.
 *   3. stroke="currentColor" so the icon takes the surrounding text color
 *      (or whatever color the parent sets). One accent color per stage is
 *      enough — don't ship per-icon palette.
 *   4. `aria-hidden` defaults to true: the icons are decorative; the
 *      adjacent Chinese label is what the screen reader announces.
 *      Pass `title` to surface a tooltip / accessible name.
 *   5. New icons go in the `paths` object below; never inline an <svg>
 *      elsewhere in the app. That single rule is what keeps the system
 *      "整体一致".
 */
import type { CSSProperties, ReactElement, SVGProps } from 'react';

export type IconProps = Omit<SVGProps<SVGSVGElement>, 'children' | 'viewBox'> & {
  /** Pixel size (width = height). Defaults to 16 to match inline text. */
  size?: number;
  /** Optional accessible label; when omitted the icon is `aria-hidden`. */
  title?: string;
  /**
   * Optional CSS color override. Most callers leave this unset so the icon
   * inherits the surrounding text color via currentColor.
   */
  color?: string;
};

const BASE_PROPS = {
  viewBox: '0 0 24 24',
  fill: 'none',
  stroke: 'currentColor',
  strokeWidth: 1.5,
  strokeLinecap: 'round' as const,
  strokeLinejoin: 'round' as const,
  'aria-hidden': true,
};

function makeIcon(d: string) {
  // Single-path glyph. Most icons are a single polyline, so we keep the
  // wrapper minimal. Multi-path glyphs (Folder, Sparkles, Robot) use the
  // richer `IconNode` factory below.
  const Icon = ({ size = 16, title, color, style, ...rest }: IconProps) => (
    <svg
      {...BASE_PROPS}
      width={size}
      height={size}
      role={title ? 'img' : undefined}
      aria-hidden={title ? undefined : true}
      aria-label={title}
      style={{ color, display: 'inline-block', flexShrink: 0, verticalAlign: '-0.125em', ...style }}
      {...rest}
    >
      {title ? <title>{title}</title> : null}
      <path d={d} />
    </svg>
  );
  Icon.displayName = 'Icon';
  return Icon;
}

/** Build an icon from multiple <path>/<circle>/<rect> children for richer glyphs. */
function makeRichIcon(children: (props: { stroke: string }) => ReactElement[]) {
  const Icon = ({ size = 16, title, color, style, ...rest }: IconProps) => (
    <svg
      {...BASE_PROPS}
      width={size}
      height={size}
      role={title ? 'img' : undefined}
      aria-hidden={title ? undefined : true}
      aria-label={title}
      style={{ color, display: 'inline-block', flexShrink: 0, verticalAlign: '-0.125em', ...style }}
      {...rest}
    >
      {title ? <title>{title}</title> : null}
      {children({ stroke: 'currentColor' })}
    </svg>
  );
  Icon.displayName = 'Icon';
  return Icon;
}

// ── Workflow stage icons (RequirementDetail mobile token receipt + stepper) ──
//
// These replace the legacy emoji set (🗂️ / 🔍 / 📐 / ✏️ / 🪄 / 🚀 / 💬 / 🛠️ /
// 🔁 / 🔀 / 🧐). They share a consistent outline + rounded-cap style so the
// receipt cards read as one family regardless of stage color.

/** Card / new requirement intake — was 🗂️ */
export const IconClipboardCreate = makeRichIcon(() => [
  <path key="a" d="M9 4h6a1 1 0 0 1 1 1v1.5h2.25A1.75 1.75 0 0 1 20 8.25v11A1.75 1.75 0 0 1 18.25 21H5.75A1.75 1.75 0 0 1 4 19.25v-11A1.75 1.75 0 0 1 5.75 6.5H8V5a1 1 0 0 1 1-1Z" />,
  <path key="b" d="M9 4.5h6M9.75 12h4.5M9.75 15.25h4.5M9.75 18.5h2.5" />,
]);

/** Magnifier / analyst — was 🔍 */
export const IconAnalyst = makeRichIcon(() => [
  <circle key="a" cx="10.5" cy="10.5" r="6.25" />,
  <path key="b" d="m20 20-4.35-4.35" />,
  <path key="c" d="M8.5 10.5h4M10.5 8.5v4" />,
]);

/** Set-square / architect — was 📐 */
export const IconArchitect = makeIcon(
  'M4 20 20 4M4 20h4M4 20v-4M7 17l10-10M10.5 13.5l3 3',
);

/** Pencil / refine-doc — was ✏️ */
export const IconRefine = makeIcon(
  'M14.5 4.5 19.5 9.5M5 20l3.5-1 11-11a1.5 1.5 0 0 0-2.12-2.12l-11 11L5 20ZM14.5 6.5 17.5 9.5',
);

/** Sparkles / apply-doc — was 🪄 */
export const IconApply = makeRichIcon(() => [
  <path key="a" d="M12 3v3M12 18v3M3 12h3M18 12h3M5.6 5.6l2.1 2.1M16.3 16.3l2.1 2.1M5.6 18.4l2.1-2.1M16.3 7.7l2.1-2.1" />,
  <path key="b" d="M12 8.5 13.5 11l2.5 1.5L13.5 14 12 16.5 10.5 14 8 12.5 10.5 11 12 8.5Z" />,
]);

/** Rocket / coding — was 🚀 */
export const IconCoding = makeRichIcon(() => [
  <path key="a" d="M14.5 4.5c2.5.5 4.5 2.5 5 5-2 .5-3.7 1.6-5 3.5l-3.5 1L9.5 12.5l1-3.5c1.9-1.3 3-2.5 4-4.5Z" />,
  <path key="b" d="M5 19c.6-1.7 1.7-2.8 3.4-3.4M14 14l-4 4" />,
  <path key="c" d="M14.5 4.5c-.6 1.3-1.5 2.5-3 4" />,
]);

/** Chat bubble / developer chat — was 💬 */
export const IconChat = makeIcon(
  'M4 5.75A1.75 1.75 0 0 1 5.75 4h12.5A1.75 1.75 0 0 1 20 5.75v9.5A1.75 1.75 0 0 1 18.25 17H8l-3.5 3v-3H5.75A1.75 1.75 0 0 1 4 15.25v-9.5ZM8 9.25h8M8 12.25h5',
);

/** Wrench / adjust-coding — was 🛠️ */
export const IconAdjust = makeRichIcon(() => [
  <path key="a" d="M14.5 4.5a4 4 0 0 0-4.95 5.45l-5.3 5.3a1.5 1.5 0 0 0 2.12 2.12l5.3-5.3A4 4 0 0 0 17.12 7.12l-2.12 2.12-2.12-2.12 2.12-2.12Z" />,
]);

/** Refresh arrow / continue-coding — was 🔁 */
export const IconContinue = makeRichIcon(() => [
  <path key="a" d="M4 12a8 8 0 0 1 13.66-5.66L20 8.5" />,
  <path key="b" d="M20 4v4.5h-4.5" />,
  <path key="c" d="M20 12a8 8 0 0 1-13.66 5.66L4 15.5" />,
  <path key="d" d="M4 20v-4.5h4.5" />,
]);

/** Branch arrows / merge — was 🔀 */
export const IconMerge = makeRichIcon(() => [
  <circle key="a" cx="6" cy="5" r="2" />,
  <circle key="b" cx="6" cy="19" r="2" />,
  <circle key="c" cx="18" cy="12" r="2" />,
  <path key="d" d="M6 7v10" />,
  <path key="e" d="M6 12c4 0 6 2 6 6" />,
  <path key="f" d="M6 12c4 0 6-2 6-6" />,
]);

/** Monocle / review — was 🧐 */
export const IconReview = makeRichIcon(() => [
  <circle key="a" cx="11" cy="11" r="6" />,
  <path key="b" d="M20 20l-3.5-3.5" />,
  <circle key="c" cx="11" cy="11" r="2" />,
]);

// ── App chrome / common UI icons ──
//
// Used by Layout (sidebar, tab bar) and various small affordances. They match
// the same stroke-width / radius language so a sidebar item and a stage
// stepper read as one icon family.

/** Dashboard tile — was 📊 */
export const IconDashboard = makeRichIcon(() => [
  <rect key="a" x="4" y="4" width="7" height="7" rx="1.5" />,
  <rect key="b" x="13" y="4" width="7" height="4" rx="1.5" />,
  <rect key="c" x="13" y="10" width="7" height="10" rx="1.5" />,
  <rect key="d" x="4" y="13" width="7" height="7" rx="1.5" />,
]);

/** Folder — was 📁 */
export const IconFolder = makeIcon(
  'M3.75 6.5A1.75 1.75 0 0 1 5.5 4.75h3.69a1.75 1.75 0 0 1 1.24.51l1.56 1.56a.25.25 0 0 0 .18.07h6.33A1.75 1.75 0 0 1 20.25 8.6v9.65A1.75 1.75 0 0 1 18.5 20H5.5A1.75 1.75 0 0 1 3.75 18.25v-11.75Z',
);

/** Clipboard / requirements list — was 📋 */
export const IconRequirements = makeRichIcon(() => [
  <path key="a" d="M7 4h7a2 2 0 0 1 2 2v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6a2 2 0 0 1 2-2Z" />,
  <path key="b" d="M9 4.5h6V3a1.5 1.5 0 0 0-1.5-1.5h-3A1.5 1.5 0 0 0 9 3v1.5Z" />,
  <path key="c" d="M8.5 10.5h6M8.5 13.5h6M8.5 16.5h4" />,
]);

/** Document with strokes / weekly report — was 📝 */
export const IconReport = makeRichIcon(() => [
  <path key="a" d="M6 3.5h8.5L18 7v12.5A1.5 1.5 0 0 1 16.5 21h-10A1.5 1.5 0 0 1 5 19.5v-14.5A1.5 1.5 0 0 1 6.5 3.5Z" />,
  <path key="b" d="M14.5 3.5V7H18" />,
  <path key="c" d="M8 11.5h8M8 14.5h8M8 17.5h5" />,
]);

/** Sparkles / "AI" mark — was ✨ */
export const IconSparkles = makeIcon(
  'M12 3v3M12 18v3M3 12h3M18 12h3M5.6 5.6l2.1 2.1M16.3 16.3l2.1 2.1M5.6 18.4l2.1-2.1M16.3 7.7l2.1-2.1',
);

/** Plus — used by "add project" and stepper add affordances */
export const IconPlus = makeIcon('M12 5v14M5 12h14');

/** Trash bin */
export const IconTrash = makeRichIcon(() => [
  <path key="a" d="M5 7h14M9 7V5a1.5 1.5 0 0 1 1.5-1.5h3A1.5 1.5 0 0 1 15 5v2" />,
  <path key="b" d="M6.5 7 7.5 19a1.5 1.5 0 0 0 1.5 1.4h6a1.5 1.5 0 0 0 1.5-1.4L17.5 7" />,
  <path key="c" d="M10 11v6M14 11v6" />,
]);

/** Settings gear */
export const IconSettings = makeRichIcon(() => [
  <circle key="a" cx="12" cy="12" r="3" />,
  <path key="b" d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 0 1-2.83 2.83l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 0 1-4 0v-.09a1.65 1.65 0 0 0-1-1.51 1.65 1.65 0 0 0-1.82.33l-.06.06A2 2 0 1 1 4.3 16.96l.06-.06A1.65 1.65 0 0 0 4.7 15 1.65 1.65 0 0 0 3.18 14H3a2 2 0 0 1 0-4h.09A1.65 1.65 0 0 0 4.7 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 1 1 2.83-2.83l.06.06a1.65 1.65 0 0 0 1.82.33H9a1.65 1.65 0 0 0 1-1.51V3a2 2 0 0 1 4 0v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 1 1 2.83 2.83l-.06.06a1.65 1.65 0 0 0-.33 1.82V9a1.65 1.65 0 0 0 1.51 1H21a2 2 0 0 1 0 4h-.09a1.65 1.65 0 0 0-1.51 1Z" />,
]);

/** Robot / sub-agent — was 🤖 */
export const IconRobot = makeRichIcon(() => [
  <rect key="a" x="4" y="7" width="16" height="12" rx="2" />,
  <path key="b" d="M12 3v4M9 13h.01M15 13h.01M9.5 16h5" />,
  <circle key="c" cx="8.5" cy="6" r="1" />,
  <circle key="d" cx="15.5" cy="6" r="1" />,
]);

/** Check / done — used in place of ✅ where a more graphic cue is wanted */
export const IconCheck = makeIcon('M5 12.5 10 17.5 19.5 7.5');

/** Cog / generic stage fallback */
export const IconCog = makeRichIcon(() => [
  <circle key="a" cx="12" cy="12" r="3" />,
  <path key="b" d="M12 4v2.5M12 17.5V20M4 12h2.5M17.5 12H20M6.3 6.3l1.8 1.8M15.9 15.9l1.8 1.8M6.3 17.7l1.8-1.8M15.9 8.1l1.8-1.8" />,
]);

/** Mailbox / empty ledger — was 📭 */
export const IconMailbox = makeRichIcon(() => [
  <path key="a" d="M4 13.5 7 6h10l3 7.5" />,
  <path key="b" d="M4 13.5h16v6H4z" />,
  <path key="c" d="M8 16.5h2.5" />,
]);

/** Plug / unconfigured — was 🔌 */
export const IconPlug = makeRichIcon(() => [
  <path key="a" d="M9 3v5M15 3v5" />,
  <rect key="b" x="7" y="8" width="10" height="6" rx="1.5" />,
  <path key="c" d="M12 14v3a3 3 0 0 1-3 3" />,
]);

/** Chevron right */
export const IconChevronRight = makeIcon('M9 5l7 7-7 7');

/** Arrow left */
export const IconArrowLeft = makeIcon('M14 6l-6 6 6 6');

/** Horizontal ellipsis — "更多" overflow menu affordance */
export const IconMore = makeRichIcon(() => [
  <circle key="a" cx="5" cy="12" r="1.25" fill="currentColor" stroke="none" />,
  <circle key="b" cx="12" cy="12" r="1.25" fill="currentColor" stroke="none" />,
  <circle key="c" cx="19" cy="12" r="1.25" fill="currentColor" stroke="none" />,
]);

// ── Status / action icons ─────────────────────────────────────────────────────
//
// Used by requirement detail and other pages for buttons, badges, and inline
// adornments (toasts, copy/open/cleanup buttons). Same outline + rounded-cap
// language as the workflow icons above so a 📋 button and a 🚀 CTA look like
// they came from the same drawer.

/** Copy — clipboard with a small page overlay; replaces 📋 */
export const IconCopy = makeRichIcon(() => [
  <rect key="a" x="8" y="8" width="12" height="13" rx="2" />,
  <path key="b" d="M16 5.5h-7A1.5 1.5 0 0 0 7.5 7v1" />,
  <path key="c" d="M16 8V5.5h.01" />,
]);

/** Open folder — page above a slightly-open folder; replaces 📂 */
export const IconFolderOpen = makeRichIcon(() => [
  <path key="a" d="M3.75 8A1.75 1.75 0 0 1 5.5 6.25h3.69a1.75 1.75 0 0 1 1.24.51l1.06 1.06a.25.25 0 0 0 .18.07H18.5A1.75 1.75 0 0 1 20.25 9.59V10H4.5" />,
  <path key="b" d="M3.75 9.5h17l-1.7 8.5A1.75 1.75 0 0 1 17.32 19.5H5.68a1.75 1.75 0 0 1-1.73-1.5L3.75 9.5Z" />,
]);

/** Broom / cleanup — replaces 🧹 */
export const IconBroom = makeRichIcon(() => [
  <path key="a" d="M14 4.5 19.5 10" />,
  <path key="b" d="m13 5.5 1.5-1.5a1.5 1.5 0 0 1 2.12 2.12L15.12 7.62" />,
  <path key="c" d="M14.5 9 9 14.5l3.5 3.5L18 12.5Z" />,
  <path key="d" d="M7.5 16 4 19.5M5.5 18 4 19.5M9 17.5l-1.5 1.5" />,
]);

/** Triangle alert — replaces ⚠️ */
export const IconAlert = makeRichIcon(() => [
  <path key="a" d="M12 4 21 19.5H3L12 4Z" />,
  <path key="b" d="M12 10v4" />,
  <circle key="c" cx="12" cy="16.5" r="0.6" fill="currentColor" stroke="none" />,
]);

/** Close / X — replaces ❌ / ✕ */
export const IconClose = makeIcon('M6 6l12 12M18 6 6 18');

/** Save / floppy — replaces 💾 */
export const IconSave = makeRichIcon(() => [
  <path key="a" d="M5.5 4.5h10L19 8v11a1.5 1.5 0 0 1-1.5 1.5h-11A1.5 1.5 0 0 1 5 19V6a1.5 1.5 0 0 1 .5-1.06Z" />,
  <path key="b" d="M8 4.5h6V7a1 1 0 0 1-1 1H9a1 1 0 0 1-1-1V4.5Z" />,
  <path key="c" d="M8 13.5h8V18H8z" />,
]);

/** Open book — replaces 📚 */
export const IconBook = makeRichIcon(() => [
  <path key="a" d="M4 5.5A1.5 1.5 0 0 1 5.5 4H11v15.5H5.5A1.5 1.5 0 0 1 4 18V5.5Z" />,
  <path key="b" d="M20 5.5A1.5 1.5 0 0 0 18.5 4H13v15.5h5.5A1.5 1.5 0 0 0 20 18V5.5Z" />,
  <path key="c" d="M7 8.5h2M7 11.5h2M7 14.5h2M15 8.5h2M15 11.5h2M15 14.5h2" />,
]);

/** Rocket — replaces 🚀 */
export const IconRocket = makeRichIcon(() => [
  <path key="a" d="M14.5 4.5c2.5.5 4.5 2.5 5 5-2 .5-3.7 1.6-5 3.5l-3.5 1L9.5 12.5l1-3.5c1.9-1.3 3-2.5 4-4.5Z" />,
  <path key="b" d="M5 19c.6-1.7 1.7-2.8 3.4-3.4M14 14l-4 4" />,
  <circle key="c" cx="15.25" cy="8.75" r="1.25" />,
]);

/** Document with folded corner — replaces 📄 */
export const IconFileText = makeRichIcon(() => [
  <path key="a" d="M6 3.5h8.5L18 7v12.5A1.5 1.5 0 0 1 16.5 21h-10A1.5 1.5 0 0 1 5 19.5v-14.5A1.5 1.5 0 0 1 6.5 3.5Z" />,
  <path key="b" d="M14.5 3.5V7H18" />,
  <path key="c" d="M8 11h8M8 14h8M8 17h5" />,
]);

/** Hourglass — replaces ⏳ */
export const IconHourglass = makeRichIcon(() => [
  <path key="a" d="M7 4h10M7 20h10" />,
  <path key="b" d="M7 4v2.5a4 4 0 0 0 1.5 3.13L11 11l-2.5 1.37A4 4 0 0 0 7 15.5V20" />,
  <path key="c" d="M17 4v2.5a4 4 0 0 1-1.5 3.13L13 11l2.5 1.37A4 4 0 0 1 17 15.5V20" />,
]);

/** Magnifier — replaces 🔍 (alias of IconAnalyst's body for places that just
 *  need a search affordance outside the analyst stage) */
export const IconMagnifier = makeRichIcon(() => [
  <circle key="a" cx="10.5" cy="10.5" r="6.25" />,
  <path key="b" d="m20 20-4.35-4.35" />,
]);

/** Bug — replaces 🐞 */
export const IconBug = makeRichIcon(() => [
  <path key="a" d="M8.5 7.5a3.5 3.5 0 0 1 7 0v5a3.5 3.5 0 0 1-7 0v-5Z" />,
  <path key="b" d="M6 11h2M16 11h2M6 15h2M16 15h2M8.5 8 7 6.5M15.5 8 17 6.5M12 4.5V3M9 18l-1.5 2.5M15 18l1.5 2.5" />,
]);

/** Refresh — replaces 🔄 */
export const IconRefresh = makeRichIcon(() => [
  <path key="a" d="M20 12a8 8 0 0 0-13.66-5.66L4 8.5" />,
  <path key="b" d="M4 4v4.5h4.5" />,
  <path key="c" d="M4 12a8 8 0 0 0 13.66 5.66L20 15.5" />,
  <path key="d" d="M20 20v-4.5h-4.5" />,
]);

/** Globe — replaces 🌐 */
export const IconGlobe = makeRichIcon(() => [
  <circle key="a" cx="12" cy="12" r="9" />,
  <path key="b" d="M3 12h18" />,
  <path key="c" d="M12 3a13.5 13.5 0 0 1 0 18 13.5 13.5 0 0 1 0-18Z" />,
]);

/** Archive box — replaces 📦 */
export const IconArchive = makeRichIcon(() => [
  <rect key="a" x="3.5" y="4.5" width="17" height="4" rx="1" />,
  <path key="b" d="M5 8.5V19a1.5 1.5 0 0 0 1.5 1.5h11A1.5 1.5 0 0 0 19 19V8.5" />,
  <path key="c" d="M10 13h4" />,
]);

/** Wrench / screwdriver combo — replaces 🔧 */
export const IconWrench = makeRichIcon(() => [
  <path key="a" d="M14.5 4.5a4 4 0 0 0-4.95 5.45l-5.3 5.3a1.5 1.5 0 0 0 2.12 2.12l5.3-5.3A4 4 0 0 0 17.12 7.12l-2.12 2.12-2.12-2.12 2.12-2.12Z" />,
]);

/** Hand (raised / stop) — replaces ✋ */
export const IconHand = makeRichIcon(() => [
  <path key="a" d="M9 11V5.5a1.5 1.5 0 0 1 3 0V11" />,
  <path key="b" d="M12 11V4.5a1.5 1.5 0 0 1 3 0V11" />,
  <path key="c" d="M15 11V5.5a1.5 1.5 0 0 1 3 0V13a6 6 0 0 1-6 6H10a4 4 0 0 1-3.5-2l-2.5-4a1.5 1.5 0 0 1 2.5-1.6L8 13" />,
]);

/** Arrow right — replaces → / ➜ */
export const IconArrowRight = makeIcon('M5 12h14M13 6l6 6-6 6');

/** Arrow left turn-back — replaces ↩ */
export const IconArrowBack = makeIcon('M9 5H5v4M5 9c0-2.5 2.5-4.5 6-4.5 4 0 7 2.5 7 5.5s-3 5.5-7 5.5H8');

/** Numbered list — replaces 🔢 */
export const IconListOrdered = makeRichIcon(() => [
  <path key="a" d="M10.5 6h9M10.5 12h9M10.5 18h9" />,
  <path key="b" d="M5 4v6M5 16l2-1.5V19M4 19h2" />,
]);

/** Send / paper-plane — used for 追问 composer submit */
export const IconSend = makeRichIcon(() => [
  <path key="a" d="M21 4 11 14" />,
  <path key="b" d="M21 4 14 21l-3-7-7-3 17-7Z" />,
]);

/** Pin — replaces 📌 */
export const IconPin = makeRichIcon(() => [
  <path key="a" d="M9 4h6l-1 5 3 3H7l3-3-1-5Z" />,
  <path key="b" d="M12 12v8" />,
]);

/** Play triangle — replaces ▶ / ▶️ */
export const IconPlay = makeIcon('M8 5v14l11-7L8 5Z');

/** Triangle ruler / set-square — replaces 📐 (alias of IconArchitect for
 *  places that just want the glyph as an inline button adornment) */
export const IconTriangle = makeIcon(
  'M4 20 20 4M4 20h4M4 20v-4M7 17l10-10M10.5 13.5l3 3',
);

/** External link / open in new tab — replaces ↗ */
export const IconExternalLink = makeRichIcon(() => [
  <path key="a" d="M13 5h6v6" />,
  <path key="b" d="M19 5 10 14" />,
  <path key="c" d="M19 14v4a1.5 1.5 0 0 1-1.5 1.5h-11A1.5 1.5 0 0 1 5 18V7a1.5 1.5 0 0 1 1.5-1.5H11" />,
]);

/** Database / archive-stack — replaces 🗄️ */
export const IconDatabase = makeRichIcon(() => [
  <ellipse key="a" cx="12" cy="5.5" rx="7" ry="2.5" />,
  <path key="b" d="M5 5.5v6c0 1.4 3.1 2.5 7 2.5s7-1.1 7-2.5v-6" />,
  <path key="c" d="M5 11.5v6c0 1.4 3.1 2.5 7 2.5s7-1.1 7-2.5v-6" />,
]);

/** Id-card / spec brief — used by the BRIEF header strip */
export const IconIdCard = makeRichIcon(() => [
  <rect key="a" x="3.5" y="5.5" width="17" height="13" rx="2" />,
  <circle key="b" cx="9" cy="11" r="2" />,
  <path key="c" d="M14 9.5h4M14 13h3" />,
]);

/** Send-out / arrow-up-from-tray — replaces 📤 */
export const IconSendOut = makeRichIcon(() => [
  <path key="a" d="M5 14.5V18a1.5 1.5 0 0 0 1.5 1.5h11A1.5 1.5 0 0 0 19 18v-3.5" />,
  <path key="b" d="M12 4v11" />,
  <path key="c" d="m7 9 5-5 5 5" />,
]);

/** Briefcase / archive-archive — generic "package" glyph */
export const IconBriefcase = makeRichIcon(() => [
  <rect key="a" x="3.5" y="7.5" width="17" height="12" rx="2" />,
  <path key="b" d="M9 7.5V6a1.5 1.5 0 0 1 1.5-1.5h3A1.5 1.5 0 0 1 15 6v1.5" />,
  <path key="c" d="M3.5 12h17" />,
]);

/** Robot — alias used in chat panels */
export const IconBot = IconRobot;

/** Robot / sub-agent in the "Claude 工作中" badge — keeps the analyst/chat
 *  the same outline family while remaining distinguishable from the generic
 *  IconRobot used in the wizard stepper */
export const IconBotBadge = makeRichIcon(() => [
  <rect key="a" x="4" y="7" width="16" height="12" rx="2" />,
  <path key="b" d="M12 3v4M9 13h.01M15 13h.01M9.5 16h5" />,
  <circle key="c" cx="8.5" cy="6" r="1" />,
  <circle key="d" cx="15.5" cy="6" r="1" />,
  <circle key="e" cx="18.5" cy="17" r="2.5" />,
  <path key="f" d="M16.5 17h4M18.5 15v4" />,
]);

/** Sleep / idle — used by the "Claude 空闲" badge (replace 😴) */
export const IconSleep = makeRichIcon(() => [
  <path key="a" d="M20 14.5a8 8 0 1 1-9.5-9.5 6.5 6.5 0 0 0 9.5 9.5Z" />,
  <path key="b" d="M16 4h4M16 4v4M16 4l4 4" />,
]);

/** Trash-out / remove — small "x over bin" used by delete confirmation */
export const IconTrashAlt = makeRichIcon(() => [
  <path key="a" d="M5 7h14M9 7V5a1.5 1.5 0 0 1 1.5-1.5h3A1.5 1.5 0 0 1 15 5v2" />,
  <path key="b" d="M6.5 7 7.5 19a1.5 1.5 0 0 0 1.5 1.4h6a1.5 1.5 0 0 0 1.5-1.4L17.5 7" />,
  <path key="c" d="M3 3l18 18" />,
]);

/**
 * Stage-to-icon resolver. Single source of truth for the requirement detail
 * mobile token receipt and the stage stepper; both call sites share this map
 * so adding a new stage only touches one place.
 */
export const STAGE_ICONS: Record<string, typeof IconAnalyst> = {
  requirement_create: IconClipboardCreate,
  analyst_chat: IconAnalyst,
  architect_design: IconArchitect,
  refine_doc: IconRefine,
  apply_doc: IconApply,
  coding: IconCoding,
  developer_chat: IconChat,
  adjust_coding: IconAdjust,
  continue_coding: IconContinue,
  merge: IconMerge,
  review: IconReview,
};

/**
 * Convenience wrapper — given a stage key, render its icon at the requested
 * size. Falls back to a generic cog so the page never renders a missing-icon
 * gap. Use this in the receipt + stepper; for one-off icons import the
 * specific glyph directly.
 */
export function StageIcon({
  stage,
  size = 16,
  className,
  style,
}: {
  stage: string;
  size?: number;
  className?: string;
  style?: CSSProperties;
}) {
  const Glyph = STAGE_ICONS[stage] ?? IconCog;
  return <Glyph size={size} className={className} style={style} />;
}
