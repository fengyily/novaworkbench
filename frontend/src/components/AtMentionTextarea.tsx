import { useState, useRef, useEffect, useCallback } from 'react';
import { skillsApi, type Skill } from '../api/client';

interface Props {
  value: string;
  onChange: (value: string) => void;
  onKeyDown?: (e: React.KeyboardEvent<HTMLTextAreaElement>) => void;
  placeholder?: string;
  rows?: number;
  className?: string;
  style?: React.CSSProperties;
  disabled?: boolean;
}

function getAtQuery(text: string, cursor: number): { start: number; query: string } | null {
  let i = cursor - 1;
  while (i >= 0 && /[A-Za-z0-9_-]/.test(text[i])) i--;
  if (i >= 0 && text[i] === '@') {
    return { start: i, query: text.slice(i + 1, cursor) };
  }
  return null;
}

export default function AtMentionTextarea({ value, onChange, onKeyDown, placeholder, rows = 6, className, style, disabled }: Props) {
  const textareaRef = useRef<HTMLTextAreaElement>(null);
  const [skills, setSkills] = useState<Skill[]>([]);
  const [atQuery, setAtQuery] = useState<{ start: number; query: string } | null>(null);
  const [activeIdx, setActiveIdx] = useState(0);

  // Load skills once
  useEffect(() => {
    skillsApi.list().then((data) => setSkills(data ?? [])).catch(() => {});
  }, []);

  const filtered = atQuery
    ? skills.filter(
        (s) =>
          s.name.toLowerCase().includes(atQuery.query.toLowerCase()) ||
          s.slug.toLowerCase().includes(atQuery.query.toLowerCase())
      ).slice(0, 8)
    : [];

  const handleChange = (e: React.ChangeEvent<HTMLTextAreaElement>) => {
    onChange(e.target.value);
    const cursor = e.target.selectionStart ?? 0;
    const q = getAtQuery(e.target.value, cursor);
    setAtQuery(q);
    setActiveIdx(0);
  };

  const insertSkill = useCallback((sk: Skill) => {
    if (!atQuery || !textareaRef.current) return;
    const before = value.slice(0, atQuery.start);
    const after = value.slice(atQuery.start + 1 + atQuery.query.length);
    const next = before + '@' + sk.slug + ' ' + after;
    onChange(next);
    setAtQuery(null);
    // Restore focus and cursor
    requestAnimationFrame(() => {
      const pos = before.length + sk.slug.length + 2; // '@slug '
      textareaRef.current?.focus();
      textareaRef.current?.setSelectionRange(pos, pos);
    });
  }, [atQuery, value, onChange]);

  const handleKeyDown = (e: React.KeyboardEvent<HTMLTextAreaElement>) => {
    if (atQuery && filtered.length > 0) {
      if (e.key === 'ArrowDown') {
        e.preventDefault();
        setActiveIdx((i) => (i + 1) % filtered.length);
        return;
      } else if (e.key === 'ArrowUp') {
        e.preventDefault();
        setActiveIdx((i) => (i - 1 + filtered.length) % filtered.length);
        return;
      } else if (e.key === 'Enter' || e.key === 'Tab') {
        e.preventDefault();
        insertSkill(filtered[activeIdx]);
        return;
      } else if (e.key === 'Escape') {
        setAtQuery(null);
        return;
      }
    }
    onKeyDown?.(e);
  };

  return (
    <div style={{ position: 'relative' }}>
      <textarea
        ref={textareaRef}
        value={value}
        onChange={handleChange}
        onKeyDown={handleKeyDown}
        onBlur={() => setTimeout(() => setAtQuery(null), 150)}
        placeholder={placeholder}
        rows={rows}
        className={className}
        style={style}
        disabled={disabled}
      />
      {atQuery && filtered.length > 0 && (
        <div style={{
          position: 'absolute',
          left: 0,
          bottom: '100%',
          marginBottom: 4,
          background: '#fff',
          border: '1px solid #E2E8F0',
          borderRadius: 8,
          boxShadow: '0 4px 12px rgba(0,0,0,0.1)',
          zIndex: 100,
          minWidth: 240,
          maxWidth: 360,
          overflow: 'hidden',
        }}>
          {filtered.map((sk, idx) => (
            <div
              key={sk.slug}
              onMouseDown={(e) => { e.preventDefault(); insertSkill(sk); }}
              style={{
                padding: '8px 12px',
                cursor: 'pointer',
                background: idx === activeIdx ? '#EEF2FF' : '#fff',
                borderBottom: idx < filtered.length - 1 ? '1px solid #F1F5F9' : 'none',
              }}
            >
              <span style={{ fontWeight: 600, color: '#4F46E5', fontFamily: 'monospace', fontSize: 13 }}>
                @{sk.slug}
              </span>
              {sk.description && (
                <span style={{ color: '#64748B', fontSize: 12, marginLeft: 8 }}>
                  {sk.description}
                </span>
              )}
            </div>
          ))}
        </div>
      )}
    </div>
  );
}
