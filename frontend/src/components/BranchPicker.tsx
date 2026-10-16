// BranchPicker — shared inline branch selector.
//
// Reused by RequirementDetail (draft / design / coding action panels) and
// ProjectCodeReview (branch-mode composer) so the same chip+rail vocabulary
// reads the same way everywhere. The chip+rail layout comes from the
// preflight modal's GIT section (see RequirementDetail.css
// `.preflight-field-card` / `.branch-selector`); only this component wires
// them together with a native <datalist> autocomplete.
//
// The dropdown options are rendered by the browser, not by us — `<datalist>`
// styling is intentionally left to the OS so the suggestions list inherits
// the user's native font and key-binding expectations. What we control here
// is the surrounding card and the trailing hint rail; those are the
// affordances that tell the user "this is the source we're forking from".

import { IconBranch } from './icons';

export interface BranchPickerProps {
  /** input id; must be unique per picker on the page (label/htmlFor too). */
  inputId: string;
  /** datalist id; must match `list=` on the input and be unique per picker. */
  datalistId: string;
  value: string;
  onChange: (v: string) => void;
  placeholder?: string;
  disabled?: boolean;
  /** Branch names offered by the autocomplete (local + origin/...). */
  options: string[];
  /** Short uppercase chip text — e.g. "BASE 设计" / "BASE 开发" / "目标". */
  chipLabel: string;
  /** Project default branch — shown in the hint rail as a baseline anchor. */
  defaultName: string;
  /** Rendered in place of the hint rail when `options` is empty. */
  emptyWarnText: string;
  /** Hide the branch icon inside the chip (default true). */
  showIcon?: boolean;
}

/**
 * Inline base-branch picker used by the design / development action
 * panels AND the code-review branch composer.
 *
 * Reuses the preflight modal's chip+rail vocabulary (cyan GIT rail,
 * uppercase BASE chip, monospace input) so the inline picker and the
 * modal preflight checklist read as one continuous design system.
 *
 * The picker is the SOURCE OF TRUTH for the active branch value; the
 * preflight modal (when it opens in RequirementDetail) seeds its BASE
 * field from this component's value. That way the user can change the
 * branch before committing to one of the CTAs, without having to
 * discover the option inside a popup.
 */
export function BranchPicker({
  inputId,
  datalistId,
  value,
  onChange,
  placeholder,
  disabled,
  options,
  chipLabel,
  defaultName,
  emptyWarnText,
  showIcon = true,
}: BranchPickerProps) {
  return (
    <div className="branch-selector">
      <div className="preflight-field-card preflight-field-card--git">
        <span className="preflight-field-chip" aria-hidden="true">
          {showIcon && <IconBranch />}
          {chipLabel}
        </span>
        <input
          id={inputId}
          list={datalistId}
          className="preflight-field-input"
          value={value}
          onChange={e => onChange(e.target.value)}
          disabled={disabled}
          placeholder={placeholder || defaultName || 'main'}
        />
      </div>
      <datalist id={datalistId}>
        {(options || []).map(b => (
          <option key={b} value={b} />
        ))}
      </datalist>
      {options.length === 0 ? (
        <span className="branch-selector-warn" role="status">
          {emptyWarnText}
        </span>
      ) : (
        <span className="branch-selector-hint">
          {/* i18n: protocol literal — the word "默认" anchors the
              project default branch as the silent baseline. */}
          <span aria-hidden="true">默认</span>
          <code>{defaultName || 'main'}</code>
        </span>
      )}
    </div>
  );
}