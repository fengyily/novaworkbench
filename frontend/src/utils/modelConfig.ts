import type { ClaudeConfigItem } from '../api/client';

/**
 * Which Claude config should the picker display for a given model?
 *
 * Frontend counterpart of the backend's `pickConfigForModel`
 * (backend/internal/handler/wizard_common.go): a persisted model may belong to
 * a config that is NOT the currently selected one — typically the global active
 * config, which is where the dropdown falls back to when the parent passes no
 * config id (e.g. a requirement row written before `*_config_id` existed).
 * Leaving that alone renders "当前：a 模型（不在当前配置列表中）" and sends the
 * run to the wrong gateway (config B's base URL with config A's model).
 *
 * Returns the config id to adopt, or null when nothing should change:
 *   - no model selected (caller keeps its own default), or
 *   - the currently selected config already lists the model, or
 *   - no config lists the model (a hand-typed / removed model id — the caller's
 *     selection is as good an answer as any, and the backend falls back the
 *     same way).
 *
 * Ordering note: when several configs list the same model id, the FIRST match
 * wins — the same tie-break as the backend's `List()` order
 * (created_at ASC, id ASC), so the two surfaces agree.
 */
export function configIdForModel(
  configs: ClaudeConfigItem[],
  model: string,
  currentConfigId: string,
): string | null {
  if (!model || configs.length === 0) return null;
  const owns = (c: ClaudeConfigItem | undefined) =>
    !!c && (c.models ?? []).some(m => m.model === model);
  if (owns(configs.find(c => c.id === currentConfigId))) return null;
  return configs.find(owns)?.id ?? null;
}
