package llm

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeLLMConfig is a stub LLMConfigProvider that returns the canned baseURL
// (typically an httptest server URL) and credentials without touching the
// settings table. Mirrors fakeClaudeEnv in gateway_env_test.go.
type fakeLLMConfig struct {
	baseURL, apiKey, model string
}

func (f fakeLLMConfig) LLMConfig() (string, string, string, error) {
	return f.baseURL, f.apiKey, f.model, nil
}

// TestConvertPdfTextToMarkdown is the happy-path test: a mock LLM returns
// JSON-wrapped markdown; the gateway strips the JSON envelope, trims the
// markdown, and propagates token usage. Asserts that the cleaned markdown
// contains the structural elements the prompt asked for (a # heading and a
// list item) and that usage round-trips.
func TestConvertPdfTextToMarkdown(t *testing.T) {
	const wantMarkdown = "# 发票详情\n\n- 编号: 12345\n- 日期: 2026-01-01"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("missing/wrong auth header: %q", got)
		}
		body, _ := io.ReadAll(r.Body)
		// Verify the request shape: system + user, and the user content
		// contains both the filename hint and the raw text.
		var req chatRequest
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("decode request: %v (body=%s)", err, body)
		}
		if len(req.Messages) < 2 || req.Messages[0].Role != "system" || req.Messages[1].Role != "user" {
			t.Errorf("expected system+user messages, got %+v", req.Messages)
		}
		if !strings.Contains(req.Messages[1].Content, "原始文件名：invoice.pdf") {
			t.Errorf("user content missing filename hint: %q", req.Messages[1].Content)
		}
		if !strings.Contains(req.Messages[1].Content, "Invoice 12345") {
			t.Errorf("user content missing raw text: %q", req.Messages[1].Content)
		}
		if req.MaxTokens != 4096 {
			t.Errorf("expected max_tokens=4096, got %d", req.MaxTokens)
		}

		// Return a response where the markdown is wrapped in ```markdown
		// fences — the gateway must strip those before returning.
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(chatResponse{
			Choices: []struct {
				Message struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content,omitempty"`
				} `json:"message"`
			}{
				{Message: struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content,omitempty"`
				}{Content: "```markdown\n" + wantMarkdown + "\n```"}},
			},
			Usage: &chatUsage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150},
		})
	}))
	defer srv.Close()

	g := New(nil, fakeLLMConfig{baseURL: srv.URL, apiKey: "test-key", model: "test-model"})
	rawText := "Invoice 12345\nDate: 2026-01-01\n\nPage 1 of 1\nFooter text"
	md, usage, err := g.ConvertPdfTextToMarkdown(rawText, "invoice.pdf")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.HasPrefix(md, "# ") {
		t.Errorf("expected # heading at start, got: %q", md)
	}
	if !strings.Contains(md, "- ") {
		t.Errorf("expected list item, got: %q", md)
	}
	if md != wantMarkdown {
		t.Errorf("markdown mismatch after fence stripping:\n got: %q\nwant: %q", md, wantMarkdown)
	}
	if usage == nil {
		t.Fatalf("expected non-nil usage")
	}
	if usage.PromptTokens != 100 || usage.CompletionTokens != 50 {
		t.Errorf("usage tokens wrong: %+v", usage)
	}
	if usage.Model != "test-model" {
		t.Errorf("usage model wrong: got %q want %q", usage.Model, "test-model")
	}
}

// TestConvertPdfTextToMarkdown_EmptyInput asserts the short-circuit: an empty
// rawText must surface ErrEmptyInput WITHOUT hitting the LLM. The fake
// config would point at a non-existent host, so a non-ErrEmptyInput return
// would surface as a request error and fail the test loudly.
func TestConvertPdfTextToMarkdown_EmptyInput(t *testing.T) {
	g := New(nil, fakeLLMConfig{baseURL: "http://127.0.0.1:1", apiKey: "x", model: "y"})
	_, _, err := g.ConvertPdfTextToMarkdown("", "test.pdf")
	if err == nil {
		t.Fatalf("expected error on empty input, got nil")
	}
	if !errors.Is(err, ErrEmptyInput) {
		t.Errorf("expected ErrEmptyInput, got: %v", err)
	}

	// Whitespace-only must also be rejected (TrimSpace check covers spaces,
	// newlines, tabs — the common "user uploaded a scan with no OCR" case).
	if _, _, err := g.ConvertPdfTextToMarkdown("   \n\t  ", "test.pdf"); !errors.Is(err, ErrEmptyInput) {
		t.Errorf("whitespace-only should also be ErrEmptyInput, got: %v", err)
	}
}

// TestConvertPdfTextToMarkdown_NilConfig covers the misconfiguration path:
// the gateway has no LLMConfigProvider wired in, so it must return an error
// before the chat call rather than nil-panicking on g.llmCfg.LLMConfig.
func TestConvertPdfTextToMarkdown_NilConfig(t *testing.T) {
	g := New(nil, nil)
	_, _, err := g.ConvertPdfTextToMarkdown("some text", "test.pdf")
	if err == nil {
		t.Fatalf("expected error when llmCfg is nil, got nil")
	}
	if errors.Is(err, ErrEmptyInput) {
		t.Errorf("nil-config error must NOT be ErrEmptyInput (caller maps that to 400, not 500): %v", err)
	}
}

// TestConvertPdfTextToMarkdown_TruncatesLargeInput asserts the 60 KiB input
// cap actually fires: a 100 KiB blob must reach the LLM truncated to 60 KiB
// + the suffix marker, NOT verbatim (which would balloon the prompt).
func TestConvertPdfTextToMarkdown_TruncatesLargeInput(t *testing.T) {
	var gotUserContent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req chatRequest
		_ = json.Unmarshal(body, &req)
		if len(req.Messages) >= 2 {
			gotUserContent = req.Messages[1].Content
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(chatResponse{
			Choices: []struct {
				Message struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content,omitempty"`
				} `json:"message"`
			}{
				{Message: struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content,omitempty"`
				}{Content: "# truncated"}},
			},
		})
	}))
	defer srv.Close()

	g := New(nil, fakeLLMConfig{baseURL: srv.URL, apiKey: "k", model: "m"})
	big := strings.Repeat("X", 100*1024) // 100 KiB
	_, _, err := g.ConvertPdfTextToMarkdown(big, "huge.pdf")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The user content carries the filename header (~24 bytes) + the
	// truncated body (60 KiB) + the suffix marker. Anything well above 60 KiB
	// means the cap didn't fire.
	const maxExpected = 65 * 1024
	if len(gotUserContent) > maxExpected {
		t.Errorf("input was not truncated: user content len=%d, want <= %d", len(gotUserContent), maxExpected)
	}
	if !strings.Contains(gotUserContent, "（后文已截断）") {
		t.Errorf("truncation marker missing in user content")
	}
}

// TestConvertPdfTextToMarkdown_EmptyContentFromReasoningModel covers the
// DeepSeek-R1 / o1-style failure mode: the model finished reasoning and
// emitted its full answer into `reasoning_content` but left `content` empty.
// The gateway must surface a clear, actionable error (mentioning reasoning
// models so the operator can switch the model) and NOT silently substitute
// the reasoning trace as user-facing markdown. The reasoning trace is for
// diagnostics, not for the knowledge row.
//
// The error message is also asserted to contain the model name and a body
// snippet so the upload-handler 500 response is self-explanatory in the
// backend log without the operator needing to attach a debugger.
func TestConvertPdfTextToMarkdown_EmptyContentFromReasoningModel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Reasoning model answer: full text in reasoning_content, empty
		// content. The reasoning text is intentionally chatty so we can
		// assert it was NOT surfaced to the caller.
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"","reasoning_content":"the model thought really hard but emitted nothing final"}}],"usage":{"prompt_tokens":10,"completion_tokens":20,"total_tokens":30}}`))
	}))
	defer srv.Close()

	g := New(nil, fakeLLMConfig{baseURL: srv.URL, apiKey: "k", model: "deepseek-reasoner"})
	md, _, err := g.ConvertPdfTextToMarkdown("some raw pdf text", "doc.pdf")
	if err == nil {
		t.Fatalf("expected error on empty content, got nil (md=%q)", md)
	}
	if md != "" {
		t.Errorf("expected empty markdown on error, got %q (must not surface reasoning trace)", md)
	}
	msg := err.Error()
	if !strings.Contains(msg, "empty content") {
		t.Errorf("error message should mention empty content, got: %q", msg)
	}
	if !strings.Contains(msg, "reasoning") {
		t.Errorf("error message should mention reasoning model mitigation, got: %q", msg)
	}
	// Body snippet should be present so the operator can spot the empty
	// content + non-empty reasoning_content in the response.
	if !strings.Contains(msg, "reasoning_content") {
		t.Errorf("error message should include the raw body snippet (got: %q) so the operator can confirm the reasoning-model cause", msg)
	}
}

// TestConvertPdfTextToMarkdown_SalvagesAnswerFromReasoningContent covers
// the salvage path: a reasoning model returns an empty `content` but its
// `reasoning_content` actually contains a polished markdown document
// (heading + list). The gateway must extract that markdown and surface it
// to the caller — otherwise the user gets a 500 on a PDF upload whose
// answer was sitting in the response the whole time.
//
// Mirrors the real-world DeepSeek-V3-flash / R1 behavior: the gateway
// returns everything in reasoning_content, leaves content empty. The
// caller must still produce a knowledge row.
func TestConvertPdfTextToMarkdown_SalvagesAnswerFromReasoningContent(t *testing.T) {
	const reasoningTrace = "We need to produce a Markdown document from the raw extracted PDF text. " +
		"The user uploaded a manual page so the structure should follow the document's chapter order.\n\n" +
		"Final Answer:\n" +
		"# 设备维护手册\n\n" +
		"- 第 1 章：开机自检\n" +
		"- 第 2 章：日常维护\n" +
		"- 第 3 章：故障排查"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Reasoning model: empty content, full answer in reasoning_content
		// after a "Final Answer:" transition.
		resp := `{"choices":[{"message":{"content":"","reasoning_content":` +
			jsonEscape(reasoningTrace) +
			`}}],"usage":{"prompt_tokens":42,"completion_tokens":99,"total_tokens":141}}`
		_, _ = w.Write([]byte(resp))
	}))
	defer srv.Close()

	g := New(nil, fakeLLMConfig{baseURL: srv.URL, apiKey: "k", model: "deepseek-flash"})
	md, usage, err := g.ConvertPdfTextToMarkdown("some raw pdf text", "manual.pdf")
	if err != nil {
		t.Fatalf("expected salvage to succeed, got error: %v", err)
	}
	if !strings.HasPrefix(md, "# 设备维护手册") {
		t.Errorf("expected salvaged heading, got: %q", md)
	}
	if !strings.Contains(md, "- 第 1 章") {
		t.Errorf("expected salvaged list item, got: %q", md)
	}
	// Defensive: scratchpad chatter that came BEFORE the "Final Answer:"
	// marker must NOT leak into the user-facing markdown.
	if strings.Contains(md, "We need to produce") {
		t.Errorf("scratchpad prose leaked into salvaged markdown: %q", md)
	}
	if usage == nil {
		t.Fatalf("expected non-nil usage")
	}
	if usage.CompletionTokens != 99 {
		t.Errorf("usage completion tokens wrong: got %d want 99", usage.CompletionTokens)
	}
}

// TestConvertPdfTextToMarkdown_SalvagesViaLastHeading covers the fallback
// salvage path: reasoning_content has no explicit transition marker but
// the actual markdown answer starts at a heading later in the trace.
// Common when DeepSeek-style models just think-then-write with no
// "Final Answer:" prefix.
func TestConvertPdfTextToMarkdown_SalvagesViaLastHeading(t *testing.T) {
	const reasoningTrace = "Step 1: parse the document.\n" +
		"Step 2: identify chapter boundaries.\n" +
		"Step 3: write the polished markdown.\n\n" +
		"# 产品规格表\n\n" +
		"| 型号 | 尺寸 |\n" +
		"| --- | --- |\n" +
		"| A1 | 10cm |"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := `{"choices":[{"message":{"content":"","reasoning_content":` +
			jsonEscape(reasoningTrace) +
			`}}]}`
		_, _ = w.Write([]byte(resp))
	}))
	defer srv.Close()

	g := New(nil, fakeLLMConfig{baseURL: srv.URL, apiKey: "k", model: "deepseek-flash"})
	md, _, err := g.ConvertPdfTextToMarkdown("raw", "spec.pdf")
	if err != nil {
		t.Fatalf("expected salvage to succeed via heading fallback, got error: %v", err)
	}
	if !strings.HasPrefix(md, "# 产品规格表") {
		t.Errorf("expected heading from last-occurrence fallback, got: %q", md)
	}
	if !strings.Contains(md, "| A1 |") {
		t.Errorf("expected table row from salvaged markdown, got: %q", md)
	}
	if strings.Contains(md, "Step 1") {
		t.Errorf("scratchpad prose leaked into salvaged markdown: %q", md)
	}
}

// TestSalvageMarkdownFromReasoning is the unit test for the helper itself.
// Each case documents a real-world response shape and what the helper
// should do with it.
func TestSalvageMarkdownFromReasoning(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string // expected returned markdown; "" means helper returns empty
	}{
		{
			name: "empty input",
			in:   "",
			want: "",
		},
		{
			name: "whitespace-only input",
			in:   "  \n\t  ",
			want: "",
		},
		{
			name: "pure scratchpad prose, no markdown structure",
			in:   "I need to think about this carefully. The user wants a markdown document but I haven't produced one yet.",
			want: "",
		},
		{
			name: "single heading line, no body — looks like scratchpad",
			in:   "Some thinking\n# Step 1\nMore thinking",
			want: "",
		},
		{
			name: "explicit Final Answer marker with full document",
			in: "We need to produce a markdown document.\n\nFinal Answer:\n" +
				"# Title\n\n- item 1\n- item 2",
			want: "# Title\n\n- item 1\n- item 2",
		},
		{
			name: "explicit Markdown Output marker",
			in: "Thinking trace here.\n\nMarkdown Output:\n# Doc\n\n| a | b |\n| - | - |",
			want: "# Doc\n\n| a | b |\n| - | - |",
		},
		{
			name: "last heading fallback when no transition marker",
			in: "Step 1: parse\nStep 2: structure\n# 报告\n\n- 第一条\n- 第二条",
			want: "# 报告\n\n- 第一条\n- 第二条",
		},
		{
			name: "transition marker but only prose after it — no salvage",
			in:   "Reasoning.\n\nFinal Answer:\nLet me explain the conclusion in prose with no markdown at all.",
			want: "",
		},
		{
			name: "multiple Final Answer markers — take the LAST",
			in: "First guess (wrong):\n\nFinal Answer:\nnot the real answer\n\nBetter analysis:\n\nFinal Answer:\n# Real Answer\n\n- final point",
			want: "# Real Answer\n\n- final point",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := salvageMarkdownFromReasoning(tc.in)
			if got != tc.want {
				t.Errorf("salvage mismatch:\n got: %q\nwant: %q", got, tc.want)
			}
		})
	}
}

// jsonEscape returns a JSON-encoded string value (including the surrounding
// double quotes), so fixture strings can contain any printable or control
// character and still embed cleanly inside a hand-written JSON object.
// Delegates to json.Marshal so all escaping rules (quotes, backslashes,
// control chars, surrogate pairs, etc.) are handled correctly.
func jsonEscape(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic("jsonEscape: " + err.Error())
	}
	return string(b)
}
