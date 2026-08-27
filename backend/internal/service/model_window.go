package service

import "strings"

// ModelContextWindow returns the maximum prompt context window (in tokens)
// for a model id, used by the wizard's live usage bar to compute the
// "used / window" percentage. The id is whatever the active platform reports
// as message.model — a full Claude model id (e.g. "claude-sonnet-4-20250514"),
// a third-party name ("deepseek-chat"), or the role/config default model.
//
// Unknown models return 0, which the frontend treats as a 200k fallback (see
// CodingChat/DeepRefineChat/DocRefineChat), so a model we have no entry for
// still renders a usable bar instead of nothing. The denominators here are
// the documented standard windows; a platform offering an extended-window
// beta simply over-reports pct, which the UI clamps to 0..100.
func ModelContextWindow(model string) int {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" {
		return 0
	}

	// Claude family — 200k across the 3.x / 4.x / 5.x line (1M-window betas
	// are opt-in and not distinguishable from the bare id, so we use 200k).
	if strings.Contains(m, "claude") {
		return 200_000
	}

	// Common third-party models reachable via a custom base URL.
	switch {
	case strings.HasPrefix(m, "deepseek"):
		// deepseek-chat / deepseek-reasoner expose a 64k window.
		return 64_000
	case strings.Contains(m, "gpt-4o"), strings.Contains(m, "gpt-4.1"):
		return 128_000
	case strings.Contains(m, "gpt-4-turbo"):
		return 128_000
	case strings.HasPrefix(m, "gpt-4"):
		return 8_000
	case strings.Contains(m, "gpt-3.5"):
		return 16_000
	case strings.Contains(m, "gemini-1.5-pro"):
		return 2_000_000
	case strings.Contains(m, "gemini"):
		return 1_000_000
	case strings.Contains(m, "qwen"):
		// qwen2.5 / qwen3 open-source variants commonly expose 32k-128k;
		// 128k is the typical ceiling.
		return 128_000
	case strings.Contains(m, "llama-3.1"), strings.Contains(m, "llama-3.3"):
		return 128_000
	case strings.Contains(m, "llama-3"):
		return 8_000
	}

	// Unrecognized — let the frontend fall back to its 200k default.
	return 0
}
