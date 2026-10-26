// Turning a stage Q&A thread into a document the user can keep.
//
// The thread is the only place where "why did you design it this way" answers
// live — the stage's own session is deliberately never written to. Exporting
// is how that reasoning leaves the app: a `.md` to paste elsewhere and a PDF
// to share. Both formats share ONE builder so they can never drift: `.md`
// downloads the string as a Blob, PDF hands the same string to
// exportDesignPdf, which pre-bakes Mermaid into inline SVG (answers may
// contain diagrams, and the backend sanitises them before persisting).
//
// Only `done` turns are exported. A running turn has no answer yet, and an
// error turn carries a failure message rather than an answer — putting those
// in a document the user keeps would read like the assistant said them.
import i18next from 'i18next';
import { sanitizeFilename } from './exportFilename';
import { questionHeadline, questionOneline } from './qaQuestion';
import type { QAStage, RequirementQATurn } from '../api/client';

export interface QAChatExportInput {
  requirementTitle: string;
  requirementId: string;
  stage: QAStage;
  turns: RequirementQATurn[];
}

/** Bare stage name (方案设计 / 开发实现), used in the metadata block. */
export function qaStageLabel(stage: QAStage): string {
  return stage === 'design'
    ? i18next.t('requirements.qaChat.exportStageDesign')
    : i18next.t('requirements.qaChat.exportStageCoding');
}

/**
 * The document label for one stage ("方案设计答疑" / "开发实现答疑"), used as
 * the H1 suffix and the filename suffix. Kept as two literal keys rather than
 * a prefix + stage name so both languages can order the words naturally.
 */
export function qaExportLabel(stage: QAStage): string {
  return stage === 'design'
    ? i18next.t('requirements.qaChat.exportTitleDesign')
    : i18next.t('requirements.qaChat.exportTitleCoding');
}

/** The document title, also the download filename stem (before the label). */
export function qaExportTitle(
  input: Pick<QAChatExportInput, 'requirementTitle' | 'requirementId' | 'stage'>,
): string {
  const title = input.requirementTitle.trim() || input.requirementId;
  return `${qaExportLabel(input.stage)} — ${title}`;
}

/**
 * Answered turns only, in display order. Shared by the builder and by the
 * UI's "is there anything to export" check so the button can never promise a
 * document that would come out empty.
 */
export function exportableTurns(turns: RequirementQATurn[]): RequirementQATurn[] {
  return turns.filter((t) => t.status === 'done' && t.answer.trim() !== '');
}

/**
 * Reassemble the whole thread as plain Markdown — no CSS classes — so the
 * same string works as a `.md` file, as exportDesignPdf's input, and pasted
 * into an issue or a chat.
 *
 * The exported document gets a table of contents, because that is what makes
 * a long thread readable once it has left the app: the questions are the
 * only landmarks it has. Each entry links to an explicit `<a id>` anchor
 * rather than to a renderer-generated heading slug — GitHub, GitLab and
 * Typora each slugify a heading differently, so a computed `#slug` link
 * would break in at least one of them. A raw anchor is boring and portable.
 */
export function buildQAChatMarkdown(input: QAChatExportInput): string {
  const { requirementTitle, requirementId, stage } = input;
  const answered = exportableTurns(input.turns);
  const parts: string[] = [];

  parts.push(`# ${qaExportTitle(input)}`);

  // Metadata block: enough for the document to stand on its own once it has
  // left the app (which requirement, which stage, how many answers, when).
  const meta: string[] = [];
  const reqLabel = requirementTitle.trim() || requirementId;
  meta.push(`- ${i18next.t('requirements.qaChat.exportMetaRequirement')}: ${reqLabel} (${requirementId})`);
  meta.push(`- ${i18next.t('requirements.qaChat.exportMetaStage')}: ${qaStageLabel(stage)}`);
  meta.push(`- ${i18next.t('requirements.qaChat.exportMetaTurns')}: ${answered.length}`);
  meta.push(`- ${i18next.t('requirements.qaChat.exportMetaTime')}: ${formatStamp(new Date().toISOString())}`);
  parts.push(meta.join('\n'));

  // Table of contents. One entry per answered turn, in the same order as
  // the sections below, so the numbering matches what the reader sees.
  if (answered.length > 0) {
    parts.push(`## ${i18next.t('requirements.qaChat.exportToc')}`);
    parts.push(
      answered
        .map((turn, i) => {
          const no = i + 1;
          return `${no}. [${escapeLinkText(questionHeadline(turn.question))}](#${tocAnchor(no)})`;
        })
        .join('\n'),
    );
    parts.push('---');
  }

  answered.forEach((turn, i) => {
    const prev = i > 0 ? answered[i - 1] : null;
    // The stage was re-run between two answers: the turns below the divider
    // were produced against a different production conversation. Mirrors the
    // divider the panel draws on screen.
    if (prev && prev.source_session_id !== turn.source_session_id) {
      parts.push('---');
      parts.push(`> **${i18next.t('requirements.qaChat.exportRotated')}**`);
    }

    const no = i + 1;
    // The anchor sits INSIDE the heading, not on a line of its own: a
    // standalone HTML block would swallow the heading that follows it in
    // some Markdown parsers, and an empty paragraph before every heading
    // would leave a visible gap in the PDF.
    //
    // The heading carries the WHOLE question (on one line), while the
    // table of contents above carries only its headline — a table of
    // contents wants a label, a section wants the full text.
    const body: string[] = [
      `## <a id="${tocAnchor(no)}"></a>${no}. ${questionOneline(turn.question)}`,
    ];
    if (!turn.resumed) {
      body.push('', `> ${i18next.t('requirements.qaChat.exportFreshNote')}`);
    }
    body.push('', turn.answer.trim());
    const stamp = formatStamp(turn.finished_at || turn.created_at);
    body.push('', `*${stamp}${turn.model ? ` · ${turn.model}` : ''}*`);
    parts.push(body.join('\n'));
  });

  return parts.join('\n\n') + '\n';
}

/** The anchor id of one exported section. Stable across renders. */
function tocAnchor(no: number): string {
  return `qa-${no}`;
}

/**
 * Escape the characters that would break a Markdown link label. A question
 * can contain brackets (e.g. "why `map[key]`?") and an unescaped `]`
 * would end the label early.
 */
function escapeLinkText(text: string): string {
  return text.replace(/[\\[\]]/g, (m) => `\\${m}`);
}

/**
 * Filename stem shared by `.md` and `.pdf`: `<requirement>-<stage label>`.
 * Sanitised with exportDesignPdf's own rule so the two downloads of one
 * thread land next to each other alphabetically.
 */
export function qaExportFilename(input: Pick<QAChatExportInput, 'requirementTitle' | 'requirementId' | 'stage'>, ext: string): string {
  const stem = sanitizeFilename(input.requirementTitle.trim() || input.requirementId, input.requirementId);
  return `${stem}-${qaExportLabel(input.stage)}.${ext}`;
}

/** Download a built Q&A document as a `.md` file. */
export function downloadQAChatMarkdown(input: QAChatExportInput): void {
  const markdown = buildQAChatMarkdown(input);
  const blob = new Blob([markdown], { type: 'text/markdown;charset=utf-8' });
  const url = URL.createObjectURL(blob);
  const a = document.createElement('a');
  a.href = url;
  a.download = qaExportFilename(input, 'md');
  document.body.appendChild(a);
  a.click();
  a.remove();
  // Revoking immediately can race the download in Safari; one tick is enough.
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}

function formatStamp(iso: string): string {
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleString(i18next.language || 'zh-CN', { hour12: false });
}
