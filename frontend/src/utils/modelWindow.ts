/**
 * modelContextWindow — frontend mirror of
 * backend/internal/service/model_window.go::ModelContextWindow.
 *
 * The frontend needs the model's max prompt context window to render the
 * live usage bar on every sub-task card (SubTaskPanel.tsx ContextUsageBar).
 * The backend already ships the canonical lookup table, but for the
 * fallback path — when a `usage` SSE frame hasn't arrived yet but the
 * sub-task row already carries persisted token columns + a model id —
 * the frontend must compute the same window client-side. Both copies
 * MUST stay in sync; any change to the backend table also goes here.
 *
 * Unknown models return 1_000_000 (matches the backend's default; the
 * frontend's computeUsage() then either uses this or falls back to a
 * 200k window when the usage frame carries its own context_window).
 *
 * Mirror of backend/internal/service/model_window.go; update both together.
 */
export function modelContextWindow(model: string): number {
  const m = (model ?? '').toLowerCase().trim();
  if (m === '') return 0;

  // Claude family — 200k across the 3.x / 4.x / 5.x line.
  if (m.includes('claude')) return 200_000;

  if (m.startsWith('deepseek')) return 64_000;
  if (m.includes('gpt-4o') || m.includes('gpt-4.1')) return 128_000;
  if (m.includes('gpt-4-turbo')) return 128_000;
  if (m.startsWith('gpt-4')) return 8_000;
  if (m.includes('gpt-3.5')) return 16_000;
  if (m.includes('gemini-1.5-pro')) return 2_000_000;
  if (m.includes('gemini')) return 1_000_000;
  if (m.includes('qwen')) return 128_000;
  if (m.includes('llama-3.1') || m.includes('llama-3.3')) return 128_000;
  if (m.includes('llama-3')) return 8_000;

  return 1_000_000;
}

export default modelContextWindow;
