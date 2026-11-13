// ReviewIdentitySelect — the "Review identity" dropdown.
//
// The Review identity is the AI persona (a row in the `roles` table) the
// reviewer runs under. It is the product's answer to "who is reviewing my
// code": the default `reviewer` persona, the `architect`, or any persona the
// user wrote themselves.
//
// Like `ExecEnvSelect`, it renders ONLY the <select> element and lets the
// caller own the label / card markup — the composer, the review toolbar and
// any future surface each wrap it differently. Keeping the two dropdowns
// structurally identical is what stops the composer row from looking like a
// pile of unrelated controls.
//
// The control class is `form-input` (the project's one form-control class,
// which styles `select.form-input` explicitly) — not a bespoke class name:
// an unstyled native <select> in the middle of that row is what made the
// first version of this picker look like it didn't belong.

import { useEffect, useMemo, useState } from 'react';
import { rolesApi, type Role } from '../api/client';

export interface ReviewIdentityOption {
  key: string;
  name: string;
}

/**
 * The default persona key. It matches the backend's reviewerRoleKey and the
 * project-level Review's default, so a user who never touches the picker
 * gets exactly the same reviewer in both places.
 */
export const DEFAULT_REVIEW_ROLE_KEY = 'reviewer';

export default function ReviewIdentitySelect({
  value,
  onChange,
  disabled = false,
  className = 'form-input',
  style,
  title,
}: {
  value: string;
  onChange: (key: string) => void;
  disabled?: boolean;
  className?: string;
  style?: React.CSSProperties;
  title?: string;
}) {
  const [roles, setRoles] = useState<Role[] | null>(null);

  useEffect(() => {
    let alive = true;
    rolesApi.list()
      .then((rows) => { if (alive) setRoles(rows); })
      .catch(() => { if (alive) setRoles([]); });
    return () => { alive = false; };
  }, []);

  const options = useMemo<ReviewIdentityOption[]>(() => {
    const list = (roles ?? []).filter((r) => r.enabled !== false);
    // The default persona always has to be selectable, even on an install
    // whose roles table was trimmed — otherwise the picker would silently
    // start the review under some other persona.
    if (list.length === 0) return [];
    if (list.some((r) => r.key === DEFAULT_REVIEW_ROLE_KEY)) {
      return list.map((r) => ({ key: r.key, name: r.name }));
    }
    return [{ key: DEFAULT_REVIEW_ROLE_KEY, name: list[0].name }, ...list.map((r) => ({ key: r.key, name: r.name }))];
  }, [roles]);

  return (
    <select
      className={className}
      value={value || DEFAULT_REVIEW_ROLE_KEY}
      disabled={disabled || options.length === 0}
      onChange={(e) => onChange(e.target.value)}
      title={title}
      style={style}
    >
      {options.map((r) => (
        <option key={r.key} value={r.key}>
          {r.name} ({r.key})
        </option>
      ))}
    </select>
  );
}
