package handler

import (
	"context"
	"strings"
	"testing"

	"github.com/novaworkbench/backend/internal/llm"
)

// The exact text the claude CLI emits when the model fills its per-response
// output budget. Quoted verbatim from the CLI so a wording change upstream
// shows up as a failing test rather than as a silently dead classifier.
const cliOutputLimitMsg = "API Error: Claude's response exceeded the 128000 output token maximum. " +
	"To configure this behavior, set the CLAUDE_CODE_MAX_OUTPUT_TOKENS environment variable."

// The non-streaming variant of the same condition.
const cliOutputLimitNoStreamMsg = "API Error: Claude's response exceeded the 32000 output token maximum " +
	"for a request sent without streaming. The CLAUDE_CODE_MAX_OUTPUT_TOKENS environment variable cannot raise this limit."

func TestClassifyClaudeFailureOutputTokenLimit(t *testing.T) {
	kind, limit := classifyClaudeFailure(cliOutputLimitMsg)
	if kind != FailureKindOutputTokenLimit {
		t.Fatalf("kind = %q, want %q", kind, FailureKindOutputTokenLimit)
	}
	if limit != 128000 {
		t.Errorf("limit = %d, want 128000", limit)
	}
	hint := claudeFailureHint(kind, limit)
	if hint == "" {
		t.Fatal("hint must not be empty for a known kind")
	}
	if !strings.Contains(hint, "输出") {
		t.Errorf("hint should mention the output limit, got %q", hint)
	}
}

func TestClassifyClaudeFailureOutputTokenLimitNonStreaming(t *testing.T) {
	kind, limit := classifyClaudeFailure(cliOutputLimitNoStreamMsg)
	if kind != FailureKindOutputTokenLimit {
		t.Fatalf("kind = %q, want %q", kind, FailureKindOutputTokenLimit)
	}
	if limit != 32000 {
		t.Errorf("limit = %d, want 32000", limit)
	}
}

func TestClassifyClaudeFailureContextWindow(t *testing.T) {
	kind, _ := classifyClaudeFailure("The model has reached its context window limit.")
	if kind != FailureKindContextWindow {
		t.Fatalf("kind = %q, want %q", kind, FailureKindContextWindow)
	}
	if claudeFailureHint(kind, 0) == "" {
		t.Error("hint must not be empty for the context-window kind")
	}
}

func TestClassifyClaudeFailureDoesNotMisfire(t *testing.T) {
	for _, msg := range []string{
		"",
		"API Error: 400 Bad Request",
		"API Error: 401 Unauthorized",
		"Claude 执行出错",
		"模型这一轮的输出达到了上限",
	} {
		kind, limit := classifyClaudeFailure(msg)
		if kind != "" {
			t.Errorf("msg %q classified as %q, want no kind", msg, kind)
		}
		if limit != 0 {
			t.Errorf("msg %q produced limit %d, want 0", msg, limit)
		}
		if h := claudeFailureHint(kind, limit); h != "" {
			t.Errorf("unknown kind must produce an empty hint, got %q", h)
		}
	}
}

func TestOutputLimitResumeOpts(t *testing.T) {
	base := llm.StreamOpts{
		Prompt:              "original prompt",
		WorkDir:             "/tmp/work",
		SystemPrompt:        "developer persona",
		Model:               "claude-x",
		Effort:              "high",
		ClaudeConfigID:      "ccfg_1",
		SessionID:           "old-sid",
		Resume:              false,
		Fork:                true,
		ForkSessionID:       "forked-sid",
		DisallowedTools:     []string{"Write"},
		PermissionMode:      "default",
		AddDirs:             []string{"/tmp/extra"},
		ExtraEnv:            []string{"FOO=bar"},
		ExcludeRepoClaudeMd: true,
	}
	out := outputLimitResumeOpts(base, "resume-sid", "resume prompt")

	if out.SessionID != "resume-sid" {
		t.Errorf("SessionID = %q, want resume-sid", out.SessionID)
	}
	if !out.Resume {
		t.Error("Resume must be true")
	}
	if out.Fork {
		t.Error("Fork must be false — a fork would lose the truncated turn's context")
	}
	if out.ForkSessionID != "" {
		t.Errorf("ForkSessionID = %q, want empty", out.ForkSessionID)
	}
	if out.SystemPrompt != "" {
		t.Errorf("SystemPrompt = %q, want empty (the forked session already carries the persona)", out.SystemPrompt)
	}
	if out.Prompt != "resume prompt" {
		t.Errorf("Prompt = %q, want the resume prompt", out.Prompt)
	}
	// Everything else must survive unchanged.
	if out.WorkDir != base.WorkDir || out.Model != base.Model || out.Effort != base.Effort {
		t.Errorf("execution fields changed: %+v", out)
	}
	if out.ClaudeConfigID != base.ClaudeConfigID {
		t.Errorf("ClaudeConfigID = %q, want %q", out.ClaudeConfigID, base.ClaudeConfigID)
	}
	if len(out.AddDirs) != 1 || out.AddDirs[0] != "/tmp/extra" {
		t.Errorf("AddDirs changed: %v", out.AddDirs)
	}
	if len(out.ExtraEnv) != 1 || out.ExtraEnv[0] != "FOO=bar" {
		t.Errorf("ExtraEnv changed: %v", out.ExtraEnv)
	}
	if !out.ExcludeRepoClaudeMd {
		t.Error("ExcludeRepoClaudeMd must be preserved")
	}
}

func TestOutputLimitResumePrompt(t *testing.T) {
	p := outputLimitResumePrompt()
	for _, want := range []string{"分段", "300 行", "Edit", "工作区"} {
		if !strings.Contains(p, want) {
			t.Errorf("resume prompt is missing %q", want)
		}
	}
}

func TestOutputLimitStillFailedHint(t *testing.T) {
	if !strings.Contains(outputLimitStillFailedHint(), "拆分") {
		t.Error("the still-failed hint should point at splitting the requirement")
	}
}

// TestParseStreamJSONFromReaderClassifiesOutputLimit drives the REAL stream
// parser with the CLI's own failure event, so the classification is proven
// end-to-end rather than only against the pure helper.
//
// The event shape is the one the claude CLI emits: a terminal "result" event
// with subtype="error" and the API error text in the "result" field.
func TestParseStreamJSONFromReaderClassifiesOutputLimit(t *testing.T) {
	line := `{"type":"result","subtype":"error","is_error":true,` +
		`"result":"API Error: Claude's response exceeded the 128000 output token maximum. ` +
		`To configure this behavior, set the CLAUDE_CODE_MAX_OUTPUT_TOKENS environment variable."}` + "\n"

	out := parseStreamJSONFromReader(context.Background(), strings.NewReader(line), silentSink{}, "output-limit-test", nil, nil)

	if out.errKind != FailureKindOutputTokenLimit {
		t.Fatalf("errKind = %q, want %q", out.errKind, FailureKindOutputTokenLimit)
	}
	if out.errMsg == "" {
		t.Fatal("errMsg must not be empty")
	}
	// The English original must be preserved (it is the diagnostic) and the
	// Chinese guidance appended (it is the actionable part).
	if !strings.Contains(out.errMsg, "output token maximum") {
		t.Errorf("errMsg lost the original CLI message: %q", out.errMsg)
	}
	if !strings.Contains(out.errMsg, "输出") {
		t.Errorf("errMsg is missing the Chinese guidance: %q", out.errMsg)
	}
}

// TestParseStreamJSONFromReaderDoesNotClassifySuccess pins that a successful
// turn is never classified — the model quoting the error text in its answer
// must not trigger the recovery.
func TestParseStreamJSONFromReaderDoesNotClassifySuccess(t *testing.T) {
	line := `{"type":"result","subtype":"success","result":"the previous turn hit the ` +
		`128000 output token maximum, here is the fix"}` + "\n"

	out := parseStreamJSONFromReader(context.Background(), strings.NewReader(line), silentSink{}, "success-test", nil, nil)

	if out.errKind != "" {
		t.Errorf("errKind = %q, want empty for a successful turn", out.errKind)
	}
	if out.errMsg != "" {
		t.Errorf("errMsg = %q, want empty for a successful turn", out.errMsg)
	}
}
