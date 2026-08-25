package service

// DefaultModelContextWindow is the fallback context-window size (in tokens)
// used when the model name is not present in knownContextWindows. Most
// current Claude models (Sonnet 4.x, Opus 4.x, 3.5 Sonnet/Haiku) share a
// 200K window; a non-listed custom model still gets a sensible denominator
// for the usage percentage display rather than crashing or showing 0%.
const DefaultModelContextWindow = 200000

// knownContextWindows maps the public Claude model names we have seen in
// production to their context-window size in tokens. The map is consulted
// by ModelContextWindow; missing entries fall back to DefaultModelContextWindow.
//
// Adding a new entry is intentionally cheap: the wizard's per-turn usage
// bar reads this map synchronously, so we want O(1) lookup with no I/O.
// If Anthropic introduces a model with a different window, just add the row.
var knownContextWindows = map[string]int{
	// Claude 4 family (Opus / Sonnet) — 200K across the board
	"claude-opus-4-5":   200000,
	"claude-opus-4-1":   200000,
	"claude-sonnet-4-5": 200000,
	"claude-sonnet-4-1": 200000,
	// Claude 3.5 family — 200K
	"claude-3-5-sonnet-latest": 200000,
	"claude-3-5-haiku-latest":  200000,
	// Claude 3 Haiku — 200K (newer alias)
	"claude-haiku-4-5": 200000,
	// Custom / proxied models (e.g. DeepSeek, GLM) typically advertise their
	// own window in their API spec. Users running such a model can edit this
	// map (or extend the wizard settings UI) to override the default.
}

// ModelContextWindow returns the context-window size for model in tokens.
// Unknown model names fall back to DefaultModelContextWindow so the usage
// percentage display never divides by zero and never crashes on a typo in
// the model name passed by an upstream proxy.
func ModelContextWindow(model string) int {
	if w, ok := knownContextWindows[model]; ok {
		return w
	}
	return DefaultModelContextWindow
}
