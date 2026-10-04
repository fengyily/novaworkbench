import { useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import type { GeneratedGPGKey } from '../api/client';

// One-time disclosure of a freshly generated GPG key pair.
//
// The server stores only the AES-256-GCM-encrypted private key and never
// echoes it back again; the public key is not persisted at all. This modal
// is therefore the single moment the user can copy either half, which
// drives two deliberate constraints:
//
//  1. **Nothing is persisted client-side.** The payload lives in React
//     state only — no sessionStorage / localStorage / IndexedDB — so
//     closing the modal is what actually destroys the plaintext copy.
//  2. **Closing requires an explicit acknowledgement.** The ×, the close
//     button and the backdrop are all inert until the checkbox is ticked,
//     so a stray click outside the dialog can't silently discard a key the
//     user has not backed up yet.
interface Props {
  open: boolean;
  payload: GeneratedGPGKey | null;
  // Platform of the token the key belongs to — selects which "upload your
  // public key" deep link we offer.
  platform?: string;
  // Base URL of self-hosted GitLab / Gitea instances. Ignored for github.
  baseUrl?: string;
  onClose: () => void;
}

// publicKeyUploadLink maps a platform to its GPG-keys settings page.
// Self-hosted platforms need the instance base URL; without it we show the
// generic hint instead of a link that would 404.
function publicKeyUploadLink(platform?: string, baseUrl?: string): { href: string; labelKey: string } | null {
  const base = (baseUrl ?? '').replace(/\/+$/, '');
  switch (platform) {
    case 'github':
      // GitHub Enterprise sets base_url to the API root, which is not a
      // usable settings URL; the public page is correct for github.com and
      // a sensible fallback otherwise.
      return { href: 'https://github.com/settings/keys', labelKey: 'settings.tokens.modal.generatedOpenGithub' };
    case 'gitlab':
      return base
        ? { href: `${base}/-/profile/gpg_keys`, labelKey: 'settings.tokens.modal.generatedOpenGitlab' }
        : null;
    case 'gitea':
      return base
        ? { href: `${base}/user/settings/keys`, labelKey: 'settings.tokens.modal.generatedOpenGitea' }
        : null;
    default:
      return null;
  }
}

export default function GeneratedGPGKeyModal({ open, payload, platform, baseUrl, onClose }: Props) {
  const { t } = useTranslation();
  const [acknowledged, setAcknowledged] = useState(false);
  const [revealPrivate, setRevealPrivate] = useState(false);
  const [copied, setCopied] = useState('');
  const [nudge, setNudge] = useState(false);

  // Reset every guard whenever a new key is shown, so a second generation
  // can never inherit the previous "already acknowledged" state.
  useEffect(() => {
    if (open) {
      setAcknowledged(false);
      setRevealPrivate(false);
      setCopied('');
      setNudge(false);
    }
  }, [open, payload]);

  if (!open || !payload) return null;

  // Every close path funnels through here so the acknowledgement guard
  // cannot be bypassed by the backdrop or the × button.
  const handleClose = () => {
    if (!acknowledged) {
      setNudge(true);
      return;
    }
    onClose();
  };

  const copy = async (text: string, which: string) => {
    try {
      await navigator.clipboard.writeText(text);
      setCopied(which);
      setTimeout(() => setCopied(prev => (prev === which ? '' : prev)), 1500);
    } catch {
      // Clipboard access can be denied (non-secure context / permissions).
      // The textarea is selectable, so manual copy still works.
      setCopied('');
    }
  };

  const link = publicKeyUploadLink(platform, baseUrl);

  return (
    <div className="modal-overlay" onClick={handleClose}>
      <div className="modal-box" onClick={e => e.stopPropagation()}>
        <h3>{t('settings.tokens.modal.generatedTitle')}</h3>

        <div className="security-banner">
          {t('settings.tokens.modal.generatedWarning')}
        </div>

        <div className="modal-field">
          <label>{t('settings.tokens.modal.generatedUidLabel')}</label>
          <input className="form-input" readOnly value={payload.uid} />
        </div>

        <div className="modal-field">
          <label>{t('settings.tokens.modal.generatedKeyIdLabel')}</label>
          <input className="form-input" readOnly value={payload.key_id} />
        </div>

        <div className="modal-field">
          <label>{t('settings.tokens.modal.generatedFingerprintLabel')}</label>
          <input className="form-input gpg-key-input" readOnly value={payload.fingerprint} />
        </div>

        <div className="modal-field">
          <label>{t('settings.tokens.modal.generatedPublicKeyLabel')}</label>
          <textarea className="form-input gpg-key-input" rows={6} readOnly value={payload.public_key} />
          <div className="modal-actions btn-row-2col">
            <button className="btn" onClick={() => copy(payload.public_key, 'public')}>
              {copied === 'public'
                ? t('settings.tokens.modal.generatedCopied')
                : t('settings.tokens.modal.generatedCopyPublic')}
            </button>
            {link
              ? <a className="btn" href={link.href} target="_blank" rel="noreferrer">{t(link.labelKey)}</a>
              : <span className="form-hint">{t('settings.tokens.modal.generatedNoLinkHint')}</span>}
          </div>
        </div>

        <div className="modal-field">
          <label>{t('settings.tokens.modal.generatedPrivateKeyLabel')}</label>
          <div className="modal-actions btn-row-2col">
            <button className="btn" onClick={() => setRevealPrivate(v => !v)}>
              {revealPrivate
                ? t('settings.tokens.modal.generatedHidePrivate')
                : t('settings.tokens.modal.generatedRevealPrivate')}
            </button>
            {revealPrivate && (
              <button className="btn" onClick={() => copy(payload.private_key, 'private')}>
                {copied === 'private'
                  ? t('settings.tokens.modal.generatedCopied')
                  : t('settings.tokens.modal.generatedCopyPrivate')}
              </button>
            )}
          </div>
          {revealPrivate && (
            <textarea className="form-input gpg-key-input" rows={8} readOnly value={payload.private_key} />
          )}
        </div>

        <div className="modal-field">
          <label className="checkbox-label">
            <input
              type="checkbox"
              checked={acknowledged}
              onChange={e => {
                setAcknowledged(e.target.checked);
                if (e.target.checked) setNudge(false);
              }}
            />
            {' '}{t('settings.tokens.modal.generatedAck')}
          </label>
          {nudge && !acknowledged && (
            <div className="form-error">{t('settings.tokens.modal.generatedAckRequired')}</div>
          )}
        </div>

        <div className="modal-actions">
          <button className="btn btn-primary" onClick={handleClose} disabled={!acknowledged}>
            {t('settings.tokens.modal.generatedClose')}
          </button>
        </div>
      </div>
    </div>
  );
}
