// ReviewIdentitySelect — the "Review identity" dropdown.
//
// The Review identity is the AI persona (a row in the `roles` table) the
// reviewer runs under. It is the product's answer to "who is reviewing my
// code": the default `reviewer` persona, the `architect`, or any persona the
// user wrote themselves.
//
// It is a plain `<select>` rather than a custom dropdown because the list is
// short, rarely changes, and the platform's own select styling is already
// consistent with the rest of the settings pages.

import { useEffect, useMemo, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { rolesApi, type Role } from '../api/client';

interface Props {
  /** The selected roles.key. '' means "not chosen yet" → falls back to 'reviewer'. */
  value: string;
  onChange: (key: string) => void;
  disabled?: boolean;
  /** Accessible label; defaults to the i18n string. */
  label?: string;
  style?: React.CSSProperties;
}

/**
 * The default persona key. It matches the backend's reviewerRoleKey and the
 * project-level Review's default, so a user who never touches the picker
 * gets exactly the same reviewer in both places.
 */
export const DEFAULT_REVIEW_ROLE_KEY = 'reviewer';

export default function ReviewIdentitySelect({
  value, onChange, disabled, label, style,
}: Props) {
  const { t } = useTranslation();
  const [roles, setRoles] = useState<Role[] | null>(null);

  useEffect(() => {
    let alive = true;
    rolesApi.list()
      .then((rows) => { if (alive) setRoles(rows); })
      .catch(() => { if (alive) setRoles([]); });
    return () => { alive = false; };
  }, []);

  const options = useMemo(() => {
    const list = (roles ?? []).filter((r) => r.enabled !== false);
    // The default persona always has to be selectable, even on an install
    // whose roles table was trimmed — otherwise the picker would silently
    // start the review under some other persona.
    if (list.length === 0) return [] as Role[];
    const hasDefault = list.some((r) => r.key === DEFAULT_REVIEW_ROLE_KEY);
    if (hasDefault) return list;
    return [{ ...list[0], key: DEFAULT_REVIEW_ROLE_KEY, name: list[0].name }, ...list];
  }, [roles]);

  return (
    <label
      className="sub-composer-env"
      style={{ display: 'flex', flexDirection: 'column', gap: 4, fontSize: 12, color: 'var(--color-text-muted)', ...style }}
    >
      <span>{label ?? t('components.subTaskPanel.reviewIdentityLabel')}</span>
      <select
        className="form-select"
        value={value || DEFAULT_REVIEW_ROLE_KEY}
        disabled={disabled || options.length === 0}
        onChange={(e) => onChange(e.target.value)}
      >
        {options.map((r) => (
          <option key={r.key} value={r.key}>
            {r.name} ({r.key})
          </option>
        ))}
      </select>
    </label>
  );
}
