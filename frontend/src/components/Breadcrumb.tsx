import type { ReactNode } from 'react';
import { Link } from 'react-router-dom';
import { useTranslation } from 'react-i18next';
import './Breadcrumb.css';

export interface BreadcrumbItem {
  label: ReactNode;
  to?: string;
  title?: string;
  mono?: boolean;
}

export function Breadcrumb({ items }: { items: BreadcrumbItem[] }) {
  const { t } = useTranslation();
  return (
    <nav className="breadcrumb" aria-label={t('components.breadcrumb.aria')}>
      <ol>
        {items.map((it, i) => {
          const last = i === items.length - 1;
          const cls = `breadcrumb-text${it.mono ? ' breadcrumb-mono' : ''}`;
          return (
            <li key={i} className="breadcrumb-item">
              {last || !it.to ? (
                <span className={cls} title={it.title} aria-current={last ? 'page' : undefined}>{it.label}</span>
              ) : (
                <Link className={`${cls} breadcrumb-link`} to={it.to} title={it.title}>{it.label}</Link>
              )}
            </li>
          );
        })}
      </ol>
    </nav>
  );
}
