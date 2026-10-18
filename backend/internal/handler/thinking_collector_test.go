package handler

import (
	"strings"
	"testing"
)

func TestThinkingCollectorDedupesDeltaAndBlock(t *testing.T) {
	var c thinkingCollector
	c.addDelta("step ")
	c.addDelta("one")
	c.addBlock("step one") // batched copy of the same turn — must be skipped
	c.endTurn()
	c.addBlock("step two") // next turn arrives only as a block
	c.endTurn()
	var out claudeStreamOutcome
	c.finish(&out)
	if want := "step one\n\nstep two"; out.thinking != want {
		t.Fatalf("got %q want %q", out.thinking, want)
	}
}

func TestThinkingCollectorTruncates(t *testing.T) {
	var c thinkingCollector
	c.addDelta(strings.Repeat("思", maxThinkingBytes))
	var out claudeStreamOutcome
	c.finish(&out)
	if !strings.HasSuffix(out.thinking, "（已截断）") {
		t.Fatalf("missing truncation mark")
	}
	if len(out.thinking) > maxThinkingBytes+len(thinkingTruncatedMark) {
		t.Fatalf("not capped: %d", len(out.thinking))
	}
}
