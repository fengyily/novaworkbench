import { useTranslation } from 'react-i18next';
import { FullscreenButton } from './FullscreenButton';
import { useFullscreen } from '../utils/useFullscreen';
import './CrossJobPanel.css';

// The output panel of a cross-requirement AI job (presentation only).
//
// It renders the job log as a typed stream and offers the fullscreen toggle;
// it contains NO request logic. Requests, reconnects and recovery all live in
// CrossRequirementDetail.tsx — which is what lets the overall design, the
// project split and the phase verification share this one panel.
//
// Shaped after the "design / coding" panels of the requirement detail page.
// Fullscreen is CSS-only (it just toggles the .is-fullscreen class), so
// entering it neither interrupts the running SSE stream nor remounts the
// component.

/** One line of a job's log stream. */
export interface CrossJobLine {
  type: string;
  content: string;
  at?: number;
}

export interface CrossJobPanelProps {
  title: string;
  running: boolean;
  /** The SSE link dropped and is being re-established — the job is still alive. */
  reconnecting?: boolean;
  /** Reconnecting gave up; the job state is being polled instead. */
  polling?: boolean;
  /** The job could not be found any more (server restarted / job evicted). */
  lost?: boolean;
  lines: CrossJobLine[];
}

/** The line kinds the backend emits. */
function lineClass(type: string): string {
  switch (type) {
    case 'phase':
      return 'xreq-stream-line--phase';
    case 'tool_call':
      return 'xreq-stream-line--tool';
    case 'tool_result':
      return 'xreq-stream-line--tool-result';
    case 'message':
      return 'xreq-stream-line--message';
    case 'error':
      return 'xreq-stream-line--error';
    default:
      return 'xreq-stream-line--plain';
  }
}

export function CrossJobPanel({
  title,
  running,
  reconnecting,
  polling,
  lost,
  lines,
}: CrossJobPanelProps) {
  const { t } = useTranslation();
  const fs = useFullscreen();

  return (
    <section className={`xreq-stream${fs.isFullscreen ? ' is-fullscreen' : ''}`}>
      <div className="xreq-stream-head">
        <span className="xreq-stream-title">{title}</span>
        {running && !reconnecting && !polling && (
          <span className="xreq-stream-badge xreq-stream-badge--running">
            {t('crossRequirements.job.running')}
          </span>
        )}
        {reconnecting && (
          <span className="xreq-stream-badge xreq-stream-badge--warn">
            {t('crossRequirements.job.reconnecting')}
          </span>
        )}
        {polling && (
          <span className="xreq-stream-badge xreq-stream-badge--warn">
            {t('crossRequirements.job.polling')}
          </span>
        )}
        {lost && (
          <span className="xreq-stream-badge xreq-stream-badge--error">
            {t('crossRequirements.job.lost')}
          </span>
        )}
        <FullscreenButton isFullscreen={fs.isFullscreen} onClick={fs.toggle} />
      </div>

      <div className="xreq-stream-body">
        {lines.length === 0 && !running ? (
          <div className="xreq-stream-empty">{t('crossRequirements.job.empty')}</div>
        ) : (
          lines.map((line, i) => (
            <div key={i} className={`xreq-stream-line ${lineClass(line.type)}`}>
              {line.content}
            </div>
          ))
        )}
      </div>

      {fs.isFullscreen && (
        <FullscreenButton isFullscreen onClick={fs.exit} variant="floating" />
      )}
    </section>
  );
}

export default CrossJobPanel;
