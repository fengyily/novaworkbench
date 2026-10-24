package handler

import "strings"

// maxThinkingBytes caps the reasoning text kept per run so an abnormally long
// chain of thought cannot blow up memory or the database row.
const maxThinkingBytes = 256 * 1024

const thinkingTruncatedMark = "\n…（已截断）"

// thinkingCollector accumulates model reasoning from a stream-json run.
// Streaming runs deliver it as thinking_delta fragments AND again as a batched
// assistant-event thinking block; only one of the two is kept per turn.
type thinkingCollector struct {
	buf       strings.Builder
	sawDelta  bool // a thinking_delta arrived in the current turn
	truncated bool
}

func (c *thinkingCollector) add(s string) {
	if s == "" || c.truncated {
		return
	}
	if room := maxThinkingBytes - c.buf.Len(); len(s) > room {
		if room > 0 {
			// Cut on a rune boundary.
			cut := room
			for cut > 0 && !isRuneStart(s[cut]) {
				cut--
			}
			c.buf.WriteString(s[:cut])
		}
		c.buf.WriteString(thinkingTruncatedMark)
		c.truncated = true
		return
	}
	c.buf.WriteString(s)
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

// addDelta records a streamed thinking_delta fragment.
func (c *thinkingCollector) addDelta(s string) {
	c.sawDelta = true
	c.add(s)
}

// addBlock records a batched assistant-event thinking block; skipped when the
// same turn already streamed its thinking as deltas.
func (c *thinkingCollector) addBlock(s string) {
	if c.sawDelta {
		return
	}
	// Only insert the paragraph break when the previous turn did not already
	// end with one. endTurn() appends "\n\n" after a streamed turn, so an
	// unconditional separator here produced FOUR newlines between a streamed
	// turn and the next batched-only turn.
	if c.buf.Len() > 0 && s != "" && !strings.HasSuffix(c.buf.String(), "\n\n") {
		c.add("\n\n")
	}
	c.add(s)
}

// endTurn marks the end of an assistant message.
func (c *thinkingCollector) endTurn() {
	if c.sawDelta {
		c.add("\n\n")
	}
	c.sawDelta = false
}

// finish copies the accumulated text into out.
func (c *thinkingCollector) finish(out *claudeStreamOutcome) {
	out.thinking = strings.TrimSpace(c.buf.String())
}
