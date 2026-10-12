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
