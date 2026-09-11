// i18next type augmentation.
//
// The resource trees are plain `as const` objects. We deliberately do NOT
// attach them to CustomTypeOptions.resources: every page builds keys
// dynamically (code→key maps from api/client.ts, prefix-built keys), and the
// strict key union turned most call sites into casts. Correctness of the
// trees is guarded instead by:
//   - i18n/index.ts assertSameKeys (dev) — zh-CN ↔ en-US parity;
//   - fallbackLng: 'zh-CN' — a missing key never renders as `undefined`;
//   - scripts/check-i18n-cjk.sh — UI copy cannot bypass the resource trees.
import 'i18next';

declare module 'i18next' {
  interface CustomTypeOptions {
    defaultNS: 'translation';
    returnNull: false;
  }
}

export {};
