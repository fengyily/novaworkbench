// RequirementFindingsPanel — the 「实施发现」 block on a master requirement's
// detail page.
//
// What it shows: the discoveries its child requirements made while they were
// being designed or implemented — conflicts with the master plan, facts the
// master plan missed, local decisions that the master plan should record.
//
// Why it exists: a master requirement is split into children, and each child
// discovers things the master's plan did not anticipate. Before this panel,
// that knowledge stayed inside the child's own plan: the master plan kept
// describing the world as it looked BEFORE the split, and the later children
// kept designing against that stale picture.
//
// The workflow is the one the rest of the product uses — a human gate:
//
//	child files → pending → human accepts / rejects → accepted
//	                                             → refine-doc folds the
//	                                               accepted ones into the
//	                                               master plan
//
// So the panel is a review queue, not a report. Nothing reaches the master
// plan without someone clicking 「采纳」.

import { useCallback, useEffect, useMemo, useState } from 'react';
import { Link } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import {
  requirementsApi,
  type RequirementFinding,
} from '../api/client';
import './RequirementFindingsPanel.css';

export interface RequirementFindingsPanelProps {
  requirementId: string;
  /** Bumped by the parent when the list may have changed (a child finished
   *  designing, a verdict was recorded). The panel re-fetches on change. */
  reloadToken?: number;
}

type Verdict = 'pending' | 'accepted' | 'rejected';

export function RequirementFindingsPanel({
  requirementId,
  reloadToken,
}: RequirementFindingsPanelProps) {
  const { t } = useTranslation();
  const [findings, setFindings] = useState<RequirementFinding[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState('');
  const [busyId, setBusyId] = useState('');

  const load = useCallback(async () => {
    try {
      const res = await requirementsApi.listFindings(requirementId);
      setFindings(res.findings ?? []);
      setError('');
    } catch {
      setError(t('requirements.findings.errLoad', { msg: '' }));
    } finally {
      setLoading(false);
    }
  }, [requirementId, t]);

  useEffect(() => {
    void load();
  }, [load, reloadToken]);

  const grouped = useMemo(() => {
    const by: Record<Verdict, RequirementFinding[]> = { pending: [], accepted: [], rejected: [] };
    for (const f of findings) {
      const v = (f.status === 'accepted' || f.status === 'rejected' ? f.status : 'pending') as Verdict;
      by[v].push(f);
    }
    return by;
  }, [findings]);

  const setVerdict = useCallback(
    async (finding: RequirementFinding, status: Verdict) => {
      setBusyId(finding.id);
      try {
        const res = await requirementsApi.updateFinding(requirementId, finding.id, { status });
        setFindings(res.findings ?? []);
        setError('');
      } catch {
        setError(t('requirements.findings.errUpdate', { msg: '' }));
      } finally {
        setBusyId('');
      }
    },
    [requirementId, t],
  );

  if (loading) {
    return (
      <div className="req-findings-panel">
        <div className="req-findings-title">{t('requirements.findings.title')}</div>
        <div className="req-findings-empty">{t('requirements.findings.loading')}</div>
      </div>
    );
  }

  if (findings.length === 0) {
    return (
      <div className="req-findings-panel">
        <div className="req-findings-title">{t('requirements.findings.title')}</div>
        <div className="req-findings-empty">{t('requirements.findings.empty')}</div>
      </div>
    );
  }

  const renderItem = (f: RequirementFinding) => (
    <li key={f.id} className={`req-findings-item req-findings-item-${f.severity}`}>
      <div className="req-findings-item-head">
        <span className={`req-findings-severity req-findings-severity-${f.severity}`}>
          {t(`requirements.findings.severity.${f.severity}`)}
        </span>
        <span className="req-findings-item-title">{f.title}</span>
        <Link to={`/requirements/${f.requirement_id}`} className="req-findings-source">
          {t('requirements.findings.fromChild')}
        </Link>
      </div>
      {f.detail && <div className="req-findings-item-detail">{f.detail}</div>}
      <div className="req-findings-actions">
        {f.status !== 'accepted' && (
          <button
            type="button"
            className="btn btn-sm"
            disabled={busyId === f.id}
            onClick={() => void setVerdict(f, 'accepted')}
          >
            {t('requirements.findings.accept')}
          </button>
        )}
        {f.status !== 'rejected' && (
          <button
            type="button"
            className="btn btn-sm"
            disabled={busyId === f.id}
            onClick={() => void setVerdict(f, 'rejected')}
          >
            {t('requirements.findings.reject')}
          </button>
        )}
      </div>
    </li>
  );

  return (
    <div className="req-findings-panel">
      <div className="req-findings-title">
        {t('requirements.findings.title')}
        <span className="req-findings-count">
          {t('requirements.findings.pendingCount', { n: grouped.pending.length })}
        </span>
      </div>
      {error && <div className="req-findings-error">{error}</div>}
      {grouped.pending.length > 0 && (
        <div className="req-findings-section">
          <div className="req-findings-section-title">{t('requirements.findings.pendingTitle')}</div>
          <ul className="req-findings-list">{grouped.pending.map(renderItem)}</ul>
        </div>
      )}
      {grouped.accepted.length > 0 && (
        <div className="req-findings-section">
          <div className="req-findings-section-title">{t('requirements.findings.acceptedTitle')}</div>
          <ul className="req-findings-list">{grouped.accepted.map(renderItem)}</ul>
          <div className="req-findings-hint">{t('requirements.findings.absorbHint')}</div>
        </div>
      )}
      {grouped.rejected.length > 0 && (
        <div className="req-findings-section">
          <div className="req-findings-section-title">{t('requirements.findings.rejectedTitle')}</div>
          <ul className="req-findings-list">{grouped.rejected.map(renderItem)}</ul>
        </div>
      )}
    </div>
  );
}

export default RequirementFindingsPanel;
