package llm

import (
	"fmt"
	"time"

	"github.com/novaworkbench/backend/internal/prompt"
)

// ExtractReviewFindingsJSON recovers a Review report's structured findings
// from its BODY, using an LLM.
//
// This is the THIRD and last source of a Review report's grading:
//
//  1. the report's trailing ```json contract block (what the reviewer was
//     asked to emit) — primary, free;
//  2. the report's own P0/P1/P3 sections — deterministic, free, no model;
//  3. this extractor — the last resort, and the only one that costs tokens.
//
// It runs only when the user explicitly asks for it (the handler exposes it
// as a button), because it costs money and can take minutes. The report
// text is never modified by it: the caller writes the extracted findings
// into the STRUCTURED columns only.
//
// Two channels, in the order the rest of the gateway uses them:
//
//   - the direct HTTP channel (OpenAI-compatible), on the HEAVY timeout
//     with an 8192-token budget: fast and cheap, and it is the channel
//     the rest of the "lightweight AI" work uses;
//   - the claude CLI, when the HTTP channel is not configured. That
//     fallback is what keeps the feature usable on an install that has
//     claude but no HTTP LLM configured.
//
// The returned text is the raw model output; the caller decodes it with the
// same tolerant contract decoder the primary path uses.
func (g *Gateway) ExtractReviewFindingsJSON(reportMD string) (string, error) {
	promptText := prompt.BuildReviewExtractPrompt(reportMD)

	// HTTP channel first.
	if g.llmCfg != nil {
		baseURL, apiKey, model, err := g.llmCfg.LLMConfig()
		if err == nil && baseURL != "" && apiKey != "" {
			out, _, cerr := chatCompletion(baseURL, apiKey, model, "", promptText, 8192, HeavyChatTimeout())
			if cerr == nil {
				return stripJSONFences(out), nil
			}
			// The HTTP channel is configured but failed. Fall through to the
			// CLI: a transient API error should not cost the user the one
			// recovery path that still works.
			httpErr := cerr
			out, cliErr := g.runClaudeText(promptText, 120*time.Second)
			if cliErr == nil {
				return stripJSONFences(out), nil
			}
			return "", fmt.Errorf("review extract: http channel failed (%v) and claude CLI failed: %w", httpErr, cliErr)
		}
	}

	// No HTTP channel configured — the claude CLI is the only way.
	out, err := g.runClaudeText(promptText, 120*time.Second)
	if err != nil {
		return "", fmt.Errorf("review extract: claude CLI failed: %w", err)
	}
	return stripJSONFences(out), nil
}
