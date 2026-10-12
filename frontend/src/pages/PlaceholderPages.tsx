import { useTranslation } from 'react-i18next';

interface PlaceholderPageProps {
  title: string;
  emoji: string;
  description?: string;
}

function PlaceholderPage({ title, emoji, description }: PlaceholderPageProps) {
  const { t } = useTranslation();
  return (
    <div style={{ width: '100%' }}>
      <h1 className="page-title">{emoji} {title}</h1>
      <div style={{
        background: 'var(--color-surface)',
        border: '1px solid var(--color-border)',
        borderRadius: 8,
        padding: 40,
        textAlign: 'center',
        color: 'var(--color-text-secondary)',
      }}>
        <p style={{ fontSize: 16, marginBottom: 8 }}>{description || t('projects.placeholders.developing')}</p>
        <p style={{ fontSize: 13, color: 'var(--color-text-muted)' }}>{t('projects.placeholders.phase')}</p>
      </div>
    </div>
  );
}

// Placeholder takes translation KEYS (not strings) so these stub pages stay
// live against a language switch. `tStrict` is keyed against the resource
// tree, so we widen it to accept the dynamic `${base}.title` keys here.
function Placeholder({ title, emoji }: { title: string; emoji: string }) {
  const { t: tStrict } = useTranslation();
  const t = tStrict as unknown as (key: string) => string;
  return (
    <PlaceholderPage
      title={t(`${title}.title`)}
      emoji={emoji}
      description={t(`${title}.desc`)}
    />
  );
}

export function Requirements() {
  return <Placeholder title="projects.placeholders.requirements" emoji="📋" />;
}

export function Knowledge() {
  return <Placeholder title="projects.placeholders.knowledge" emoji="🧠" />;
}

export function Chat() {
  return <Placeholder title="projects.placeholders.chat" emoji="💬" />;
}

export function Reports() {
  return <Placeholder title="projects.placeholders.reports" emoji="📝" />;
}

export function Settings() {
  return <Placeholder title="projects.placeholders.settings" emoji="⚙️" />;
}
