import { useTranslation } from 'react-i18next';
import {
  EFFORT_TIERS,
  effortTierLabel,
  normalizeEffortTier,
  type EffortTier,
} from '../utils/effort';

// EffortBar — the 推理档位 (reasoning effort) picker.
//
// The control is a five-segment STRIP that reads as an axis of reasoning
// depth: each segment carries its own tier's colour, so the bar as a whole
// looks like a light→deep ramp (low → medium → high → xhigh → max).
//
// Structural debt to the existing segmented controls
// (.sub-session-mode-* in SubTaskPanel): a role="radiogroup" container
// holding one <label> per option, each wrapping a visually-hidden radio
// input and a text span. Reusing that shape is what keeps this control
// from reading as a foreign object next to the session-mode switch —
// keyboard navigation, screen-reader announcement and the "click the
// label" affordance all come for free from the native radio semantics.
//
// TWO RULES THIS COMPONENT ENFORCES:
//
//  1. The selection is never carried by colour alone. The chosen segment
//     gets a deep underline (position), bold text (weight) and a 1px lift
//     (elevation) on top of its ramp colour. A colour-blind or
//     greyscale-printing user still reads the state from the underline and
//     the word inside the segment.
//
//  2. There is always exactly one segment selected, and the value is
//     always a concrete tier. "Don't send --effort at all" is NOT a
//     segment: that capability belongs to the effort.enabled master switch
//     on the settings page, which is the single place where the flag can
//     be turned off wholesale. Having a "no value" segment here would
//     give the same concept two different spellings ("empty means use the
//     stage default" vs "empty means send nothing") and the backend would
//     have to guess which one the caller meant.
//
// The tier strings themselves ("low" / "medium" / …) are the CLI's own
// vocabulary and are deliberately NOT translated — they are the literal
// values sent to `--effort`, and a translated label would hide what the
// backend actually receives. The explanatory sentence under the bar IS
// translated.

interface Props {
  /** The selected tier. Must be one of EFFORT_TIERS. */
  value: EffortTier;
  /** Called with the newly selected tier. */
  onChange: (tier: EffortTier) => void;
  /** Optional visible label above the bar. */
  label?: string;
  /** Optional id for the label element (used as the radiogroup's aria-labelledby). */
  id?: string;
  /** Grey the control out and block interaction. */
  disabled?: boolean;
  /** Extra class on the wrapper (e.g. a layout hook). */
  className?: string;
  /** 'sm' is the compact variant for toolbars; 'md' (default) for forms. */
  size?: 'sm' | 'md';
}

// The tier vocabulary itself lives in utils/effort.ts so this file exports
// nothing but a component (the react/only-export-components lint rule
// requires that for Fast Refresh to work). The types are re-exported here
// because they belong with the component that renders them.
export type { EffortStage, EffortTier } from '../utils/effort';

export default function EffortBar({
  value,
  onChange,
  label,
  id,
  disabled,
  className,
  size = 'md',
}: Props) {
  const { t } = useTranslation();

  const active = normalizeEffortTier(value) || 'medium';
  const labelId = id ? `${id}-label` : undefined;
  const descId = id ? `${id}-desc` : undefined;

  const wrapperClass = [
    'effort-bar',
    size === 'sm' ? 'effort-bar--sm' : '',
    disabled ? 'is-disabled' : '',
    className || '',
  ]
    .filter(Boolean)
    .join(' ');

  return (
    <div className={wrapperClass}>
      {label && (
        <span className="effort-bar-label" id={labelId}>
          {label}
        </span>
      )}
      <div
        className="effort-bar-segments"
        role="radiogroup"
        aria-labelledby={labelId}
        aria-describedby={descId}
        // A radiogroup needs an accessible name; when the caller didn't
        // pass a visible label we fall back to a generic one so the group
        // is never announced as an unnamed list of radios.
        aria-label={label ? undefined : t('components.effortBar.ariaLabel')}
      >
        {EFFORT_TIERS.map((tier) => {
          const isActive = tier === active;
          return (
            <label
              key={tier}
              className={`effort-bar-segment${isActive ? ' is-active' : ''}`}
              data-tier={tier}
              title={t(`components.effortBar.tiers.${tier}Hint`)}
            >
              <input
                type="radio"
                name={id ?? 'effort'}
                value={tier}
                checked={isActive}
                onChange={() => onChange(tier)}
                disabled={disabled}
                aria-label={t(`components.effortBar.tiers.${tier}Hint`)}
              />
              <span className="effort-bar-segment-label">{effortTierLabel(tier)}</span>
            </label>
          );
        })}
      </div>
      <span className="effort-bar-desc" id={descId} role="note">
        {t(`components.effortBar.tiers.${active}Hint`)}
      </span>
    </div>
  );
}
