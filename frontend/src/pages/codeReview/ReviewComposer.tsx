// ReviewComposer — everything the user fills in BEFORE a review runs.
//
// The composer is deliberately three numbered steps, because the three
// questions it asks are different in kind and users kept conflating them:
//
//   ① 范围       — what is being reviewed (a PR, or any branch pair)
//   ② 验证材料   — what "done" means for this change, and the evidence for
//                  checking it: the 改动目标 description plus attachments
//                  (run logs / design docs / zips). This is the input the
//                  reviewer grades 目标达成 against, so it gets the most room.
//   ③ 高级      — model + execution environment, collapsed by default:
//                  real knobs, but not ones you touch per review.
//
// What is NOT here any more: the 「创建草稿」 button. A branch review needs a
// persisted row before attachments can be uploaded (the row IS the
// attachment owner), which used to be exposed as a manual first step — an
// implementation detail the user had to perform. The parent now creates that
// row automatically once a branch is picked, so the only button left is the
// one the user actually came for.

import { useEffect } from 'react';
import { useTranslation } from 'react-i18next';
import type { CodeReview, PR, PRListResponse } from '../../api/client';
import AttachmentPanel from '../../components/AttachmentPanel';
import { BranchPicker } from '../../components/BranchPicker';
import ModelSelect from '../../components/ModelSelect';
import { ExecEnvSelect, type ExecEnvServer } from '../../components/ExecEnvSelect';
import { IconPlug } from '../../components/icons';
import { fmtDate } from '../../utils/intl';

export type Mode = 'pr' | 'branch';

/** The model / claude-config / agent-server picks, shared by both modes. */
export interface RunBindingsProps {
  model: string;
  configId: string;
  agentServerId: string;
  servers: ExecEnvServer[];
  onModelChange: (model: string) => void;
  onConfigChange: (configId: string) => void;
  onAgentServerChange: (id: string) => void;
}

export interface BranchFormProps {
  branches: string[];
  branch: string;
  baseBranch: string;
  /** Project default branch — shown in each picker's hint rail as the
   *  silent baseline ("默认 main"). Empty falls back to "main". */
  defaultBranch: string;
  title: string;
  description: string;
  onBranchChange: (v: string) => void;
  onBaseBranchChange: (v: string) => void;
  onTitleChange: (v: string) => void;
  onDescriptionChange: (v: string) => void;
  /** Flush un-blurred edits to the draft row. */
  onFieldBlur: () => void;
  /** The auto-created draft row; null until a branch has been picked. */
  session: CodeReview | null;
  /** True while that row is being created, so the attachment slot can say so. */
  creating: boolean;
  savedAt: number;
  error: string;
  onStart: () => void;
  onReset: () => void;
  onDelete: () => void;
  /** Manually pull remote branches so the datalist isn't a stale snapshot. */
  onRefreshBranches: () => void;
  /** True while the manual `git fetch origin --prune` is in flight. */
  refreshing: boolean;
}

interface Props {
  mode: Mode;
  onModeChange: (m: Mode) => void;
  running: boolean;
  /** Project default branch — drives the picker's "默认 main" hint rail. */
  defaultBranch: string;
  bindings: RunBindingsProps;
  // PR mode
  prData: PRListResponse | null;
  prsLoading: boolean;
  prsError: string;
  reviewingPR: PR | null;
  extraRequirements: string;
  onExtraRequirementsChange: (v: string) => void;
  onReviewPR: (pr: PR) => void;
  // Branch mode
  branchForm: BranchFormProps;
  // On-demand toggle plumbing (see ProjectCodeReview.showCreate). The
  // composer is mounted only while showCreate is true, so these are the
  // only ways the user has to dismiss it without starting a run.
  /** Render a "collapse" button that flips the parent toggle off. */
  onClose?: () => void;
  /** Fire-once hook so the parent can eagerly provision the draft row
   *  (and thus the attachment owner) the moment the composer opens,
   *  rather than waiting for the user to type a branch. The parent's
   *  own branch-typing effect still owns the 700ms debounce. */
  onEnsureDraft?: () => Promise<CodeReview | null>;
}

export default function ReviewComposer(props: Props) {
  const { t } = useTranslation();
  const { mode, onModeChange, running, bindings, branchForm, defaultBranch, onClose, onEnsureDraft } = props;

  // On mount, give the parent a chance to provision the draft row. With no
  // branch picked yet this is a no-op (ensureDraft returns null), but it
  // means that as soon as a branch is selected the row already exists in
  // the same render that picks it — the attachment panel never has to
  // wait on a debounce timer.
  useEffect(() => {
    if (onEnsureDraft) void onEnsureDraft();
    // We only want this on mount; the parent owns subsequent re-runs.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  return (
    <div className="cr-composer-shell" data-show="on-demand">
      {/* On-demand dismiss: only rendered when the parent passes `onClose`,
          which is the new toggle-affordance case (the tab used to always
          show the composer, so there was nothing to close). Kept at the
          top of the shell so it's reachable without scrolling. */}
      {onClose && (
        <div className="cr-composer-head">
          <button
            type="button"
            className="rc-collapse btn btn-secondary"
            onClick={onClose}
          >
            ✕ {t('codeReviews.create.toggleClose')}
          </button>
        </div>
      )}

      {/* ── ① 范围 ── */}
      <section className="cr-step">
        <header className="cr-step-head">
          <span className="cr-step-no">1</span>
          <h4 className="cr-step-title">{t('projects.detail.reviewStepScope')}</h4>
          <span className="cr-hint">
            {mode === 'pr' ? t('projects.detail.reviewModeHintPR') : t('projects.detail.reviewModeHintBranch')}
          </span>
        </header>

        <div className="cr-segmented" role="tablist">
          <button
            role="tab"
            aria-selected={mode === 'pr'}
            className={`cr-segment${mode === 'pr' ? ' active' : ''}`}
            onClick={() => onModeChange('pr')}
            disabled={running}
          >{t('projects.detail.reviewModePR')}</button>
          <button
            role="tab"
            aria-selected={mode === 'branch'}
            className={`cr-segment${mode === 'branch' ? ' active' : ''}`}
            onClick={() => onModeChange('branch')}
            disabled={running}
          >{t('projects.detail.reviewModeBranch')}</button>
        </div>

        {mode === 'pr' ? <PRScope {...props} /> : <BranchScope form={branchForm} running={running} defaultBranch={defaultBranch} />}
      </section>

      {/* ── ② 验证材料 ── Branch mode only: the PR entry point keeps its
          single-line 额外审查要求 field inside the PR table above. */}
      {mode === 'branch' && (
        <section className="cr-step">
          <header className="cr-step-head">
            <span className="cr-step-no">2</span>
            <h4 className="cr-step-title">{t('projects.detail.reviewStepEvidence')}</h4>
            <span className="cr-hint">{t('projects.detail.reviewStepEvidenceHint')}</span>
          </header>

          <div className="cr-field">
            <label className="cr-label">
              {t('projects.detail.reviewDescLabel')}
              {branchForm.savedAt > 0 && <span className="cr-saved">{t('projects.detail.reviewDescSaved')}</span>}
            </label>
            <textarea
              className="form-input cr-desc"
              rows={5}
              placeholder={t('projects.detail.reviewDescPlaceholder')}
              value={branchForm.description}
              onChange={e => branchForm.onDescriptionChange(e.target.value)}
              onBlur={branchForm.onFieldBlur}
              disabled={running}
            />
          </div>

          <div className="cr-attach">
            <div className="cr-attach-head">
              <span className="cr-label">{t('projects.detail.reviewAttachTitle')}</span>
              <span className="cr-hint">{t('projects.detail.reviewAttachHint')}</span>
            </div>
            {/* The attachment panel needs the draft row as its owner id. It
                appears on its own as soon as the parent has created that row
                — picking a branch is all the user has to do. */}
            {branchForm.session ? (
              <AttachmentPanel ownerId={branchForm.session.id} resource="code-reviews" />
            ) : (
              <p className="cr-hint cr-attach-wait">
                {branchForm.creating
                  ? t('projects.detail.reviewAttachPreparing')
                  : t('projects.detail.reviewAttachNeedBranch')}
              </p>
            )}
          </div>
        </section>
      )}

      {/* ── ③ 高级 ── Model + execution environment. Defaults to OPEN: the
          requirement design/development toolbars always expose these, and
          collapsing them here made Review look like it had no knobs.
          The user can still close the section to get the same minimal
          surface as before. */}
      <details className="cr-step cr-advanced" open>
        <summary className="cr-step-head cr-advanced-summary">
          <span className="cr-step-no">{mode === 'branch' ? 3 : 2}</span>
          <h4 className="cr-step-title">{t('projects.detail.reviewStepAdvanced')}</h4>
          <span className="cr-hint">
            {bindings.agentServerId
              ? t('projects.detail.reviewExecEnvRemoteHint')
              : t('projects.detail.reviewStepAdvancedHint')}
          </span>
        </summary>
        <div className="cr-run-bar">
          <div className="cr-run-field">
            <label className="cr-label">{t('projects.detail.reviewModelLabel')}</label>
            <ModelSelect
              value={bindings.model}
              onChange={bindings.onModelChange}
              configId={bindings.configId}
              onConfigChange={bindings.onConfigChange}
              disabled={running}
              title={t('projects.detail.reviewModelHint')}
            />
          </div>
          <div className="cr-run-field">
            <label className="cr-label">{t('projects.detail.reviewExecEnvLabel')}</label>
            <ExecEnvSelect
              servers={bindings.servers}
              value={bindings.agentServerId}
              onChange={bindings.onAgentServerChange}
              disabled={running}
              title={t('projects.detail.reviewExecEnvHint')}
            />
            <span className="cr-hint">
              {bindings.agentServerId
                ? t('projects.detail.reviewExecEnvRemoteHint')
                : t('projects.detail.reviewExecEnvLocalHint')}
            </span>
          </div>
        </div>
      </details>

      {/* ── Start ── One primary action, in both modes. In PR mode the run is
          started from the PR row itself, so only branch mode lands here. */}
      {mode === 'branch' && (
        <>
          {branchForm.error && <div className="cr-error">{branchForm.error}</div>}
          <div className="cr-actions">
            <button
              className="btn btn-primary"
              onClick={branchForm.onStart}
              disabled={running || !branchForm.branch.trim()}
            >
              {running ? t('projects.detail.reviewStartBusy') : t('projects.detail.reviewStartBtn')}
            </button>
            <button className="btn btn-secondary" onClick={branchForm.onReset} disabled={running}>
              {t('projects.detail.reviewResetBtn')}
            </button>
            {branchForm.session && (
              <button className="btn btn-secondary" onClick={branchForm.onDelete} disabled={running}>
                {t('projects.detail.reviewDeleteBtn')}
              </button>
            )}
          </div>
        </>
      )}
    </div>
  );
}

/** Branch + base + title. The datalist offers local and origin branches. */
function BranchScope({ form, running, defaultBranch }: { form: BranchFormProps; running: boolean; defaultBranch: string }) {
  const { t } = useTranslation();
  const localCount = form.branches.filter(b => !b.startsWith('origin/')).length;
  const remoteCount = form.branches.length - localCount;
  return (
    <>
      <div className="cr-field-row cr-branch-pickers">
        <BranchPicker
          inputId="cr-branch-input"
          datalistId="cr-branch-options"
          value={form.branch}
          onChange={form.onBranchChange}
          disabled={running}
          placeholder={t('projects.detail.reviewBranchPlaceholder')}
          options={form.branches}
          chipLabel="目标"
          defaultName={defaultBranch}
          emptyWarnText={t('projects.detail.reviewBranchPickerEmpty')}
        />
        <BranchPicker
          inputId="cr-base-branch-input"
          datalistId="cr-base-branch-options"
          value={form.baseBranch}
          onChange={form.onBaseBranchChange}
          disabled={running}
          placeholder={t('projects.detail.reviewBasePlaceholder')}
          options={form.branches}
          chipLabel="基准"
          defaultName={defaultBranch}
          emptyWarnText={t('projects.detail.reviewBranchPickerEmpty')}
        />
      </div>
      <div className="cr-branch-toolbar">
        <span className="cr-branch-counts" aria-label={t('projects.detail.reviewBranchesHint')}>
          <span>{t('projects.detail.reviewBranchCountLocal', { n: localCount })}</span>
          <span aria-hidden="true">·</span>
          <span>{t('projects.detail.reviewBranchCountRemote', { n: remoteCount })}</span>
        </span>
        <button
          type="button"
          className="btn btn-secondary"
          onClick={form.onRefreshBranches}
          disabled={running || form.refreshing}
          aria-label={t('projects.detail.reviewRefreshBranchesBtn')}
        >
          {form.refreshing
            ? t('projects.detail.reviewRefreshBranchesBusy')
            : '🔄 ' + t('projects.detail.reviewRefreshBranchesBtn')}
        </button>
      </div>

      <div className="cr-field">
        <label className="cr-label">{t('projects.detail.reviewTitleLabel')}</label>
        <input
          className="form-input"
          placeholder={t('projects.detail.reviewTitlePlaceholder')}
          value={form.title}
          onChange={e => form.onTitleChange(e.target.value)}
          onBlur={form.onFieldBlur}
          disabled={running}
        />
      </div>
    </>
  );
}

/** The open-PR table, behaviour unchanged from the original flow. */
function PRScope({ prData, prsLoading, prsError, reviewingPR, running, extraRequirements, onExtraRequirementsChange, onReviewPR }: Props) {
  const { t } = useTranslation();

  if (prsLoading) return <div className="tab-empty">{t('projects.detail.reviewLoading')}</div>;
  if (prsError) return <div className="tab-empty" style={{ color: 'var(--color-error)' }}>{prsError}</div>;
  if (!prData) return null;

  if (!prData.configured && prData.prs.length === 0) {
    return (
      <div className="review-unconfigured">
        <div className="review-unconfigured-icon"><IconPlug size={28} /></div>
        <p>{t('projects.detail.reviewUnconfiguredTitle')}</p>
        <p>
          {t('projects.detail.reviewUnconfiguredHint')}
          <a href="/settings" style={{ color: 'var(--color-primary)', margin: '0 4px' }}>{t('projects.detail.platformSettingsLink')}</a>
          {t('projects.detail.reviewUnconfiguredHint2')}
        </p>
      </div>
    );
  }

  if (prData.prs.length === 0) {
    return <div className="tab-empty"><p>{t('projects.detail.reviewNoPRs')}</p></div>;
  }

  return (
    <>
      <div className="review-requirements-bar">
        <label className="review-requirements-label">{t('projects.detail.reviewExtraLabel')}</label>
        <input
          className="form-input review-requirements-input"
          placeholder={t('projects.detail.reviewExtraPlaceholder')}
          value={extraRequirements}
          onChange={e => onExtraRequirementsChange(e.target.value)}
          disabled={running}
        />
      </div>
      <div className="pr-list">
        <table className="pr-table table-cards">
          <thead>
            <tr>
              <th style={{ width: 52 }}>{t('projects.detail.reviewColNumber')}</th>
              <th>{t('projects.detail.reviewColTitle')}</th>
              <th style={{ width: 260 }}>{t('projects.detail.reviewColBranch')}</th>
              <th style={{ width: 100 }}>{t('projects.detail.reviewColAuthor')}</th>
              <th style={{ width: 90 }}>{t('projects.detail.reviewColUpdatedAt')}</th>
              <th style={{ width: 120 }}>{t('projects.detail.reviewColActions')}</th>
            </tr>
          </thead>
          <tbody>
            {prData.prs.map(pr => (
              <tr key={pr.number} className={reviewingPR?.number === pr.number ? 'pr-row-active' : ''}>
                <td data-label="#" style={{ color: 'var(--color-text-muted)', fontFamily: 'var(--font-mono)', fontSize: 12 }}>#{pr.number}</td>
                <td data-label={t('projects.detail.reviewColTitle')} className="pr-title">
                  <a href={pr.html_url} target="_blank" rel="noreferrer" style={{ color: 'var(--color-primary)' }}>
                    {pr.title}
                  </a>
                </td>
                <td data-label={t('projects.detail.reviewColBranch')}><code className="pr-branch">{pr.head_branch}</code> ← <code className="pr-branch">{pr.base_branch}</code></td>
                <td data-label={t('projects.detail.reviewColAuthor')}>{pr.author}</td>
                <td data-label={t('projects.detail.reviewColUpdatedAt')}>{pr.updated_at ? fmtDate(pr.updated_at) : '—'}</td>
                <td data-label={t('projects.detail.reviewColActions')}>
                  <button
                    className="btn btn-primary btn-sm"
                    onClick={() => onReviewPR(pr)}
                    disabled={running}
                  >
                    {reviewingPR?.number === pr.number ? t('projects.detail.reviewBtnActive') : t('projects.detail.reviewBtn')}
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </>
  );
}
