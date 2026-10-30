package llm

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The HTTP LLM channel used to ship ONE hard-coded 30s timeout, tuned for the
// light workloads (title distillation, commit titles). Every later caller
// inherited it — including the plan → sub-task extraction, which ships 24k
// runes and asks for 8192 tokens and therefore died on EVERY attempt with
// "llm http: read response: context deadline exceeded", degrading the whole
// split path to one coarse child.
//
// These tests pin the tiered timeout mechanism that replaced it:
//   - the timeout argument is actually honoured (TestChatCompletionEnforcesTimeout)
//   - step extraction really runs on the HEAVY tier (TestExtractStepsFromPlanUsesHeavyTimeout)
//   - both tiers are overridable per deployment, and a typo can never disable
//     the bound entirely (TestHTTPChatTimeoutEnvOverride)

// slowJSONServer serves an OpenAI-compatible chat completion carrying the
// given assistant content, but only AFTER a delay that starts once the
// response headers are already on the wire.
//
// The header-first ordering is the whole point: it reproduces the production
// failure mode exactly — the upstream accepted the request and started
// generating (200 headers received) but the body did not arrive within the
// client's deadline. A server that slept BEFORE writing the headers would
// instead fail in http.Client.Do, which is a different code path (and a
// different error string) than the one we need to pin.
//
// content is the model's ANSWER (e.g. "ok" or a {"subtasks":[…]} envelope);
// chatCompletion expects it wrapped in the /chat/completions envelope, so the
// wrapping happens here rather than at every call site.
func slowJSONServer(delay time.Duration, content string) *httptest.Server {
	body, err := json.Marshal(map[string]any{
		"choices": []any{map[string]any{
			"message": map[string]any{"content": content},
		}},
	})
	if err != nil {
		panic(err)
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Best-effort flush: the header must reach the client before the
		// delay, otherwise the client fails while waiting for headers.
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(delay)
		_, _ = w.Write(body)
	}))
}

// TestChatCompletionEnforcesTimeout pins that the timeout argument reaches
// the request context. The pre-fix channel hard-coded 30s inside
// chatCompletion, so there was no way for a caller to give the heavy
// step-extraction call more room — the caller's tier is now explicit.
func TestChatCompletionEnforcesTimeout(t *testing.T) {
	srv := slowJSONServer(300*time.Millisecond, "ok")
	defer srv.Close()

	t.Run("short timeout aborts the read", func(t *testing.T) {
		out, _, err := chatCompletion(srv.URL, "k", "m", "sys", "user", 512, 50*time.Millisecond)
		if err == nil {
			t.Fatalf("expected a timeout error, got success (out=%q)", out)
		}
		if !strings.Contains(err.Error(), "deadline exceeded") {
			t.Fatalf("expected the error to mention the deadline, got: %v", err)
		}
	})

	t.Run("generous timeout completes", func(t *testing.T) {
		out, _, err := chatCompletion(srv.URL, "k", "m", "sys", "user", 512, 3*time.Second)
		if err != nil {
			t.Fatalf("expected success under a 3s timeout, got: %v", err)
		}
		if !strings.Contains(out, "ok") {
			t.Fatalf("expected the assistant content back, got: %q", out)
		}
	})
}

// TestExtractStepsFromPlanUsesHeavyTimeout pins that the plan → sub-task
// extraction — the call that made 「拆分子任务」 degrade to a single child —
// runs on the HEAVY tier rather than the 30s light cap.
//
// Pinning heavyChatTimeoutFn keeps the test instant: without it the assertion
// would need a real multi-minute wait, and a passing test would be
// indistinguishable from a slow one.
func TestExtractStepsFromPlanUsesHeavyTimeout(t *testing.T) {
	srv := slowJSONServer(200*time.Millisecond, `{"subtasks":[{"title":"t","prompt":"p"}]}`)
	defer srv.Close()

	// fakeLLMConfig already exists in this package (gateway_test.go) — reuse
	// it instead of declaring a second stub with the same name.
	g := New(nil, fakeLLMConfig{baseURL: srv.URL, apiKey: "k", model: "m"})

	orig := heavyChatTimeoutFn
	t.Cleanup(func() { heavyChatTimeoutFn = orig })

	t.Run("a light-tier budget kills the extraction", func(t *testing.T) {
		heavyChatTimeoutFn = func() time.Duration { return 50 * time.Millisecond }

		out, err := g.ExtractStepsFromPlan("# 计划\n\n1. 一\n2. 二", "")
		if err == nil {
			t.Fatalf("expected the extraction to time out, got success (out=%q)", out)
		}
		if !strings.Contains(err.Error(), "deadline exceeded") {
			t.Fatalf("expected the error to mention the deadline, got: %v", err)
		}
	})

	t.Run("the heavy tier lets it through", func(t *testing.T) {
		heavyChatTimeoutFn = func() time.Duration { return 2 * time.Second }

		out, err := g.ExtractStepsFromPlan("# 计划\n\n1. 一\n2. 二", "")
		if err != nil {
			t.Fatalf("expected success under the heavy timeout, got: %v", err)
		}
		if !strings.Contains(out, "subtasks") {
			t.Fatalf("expected the subtasks envelope back, got: %q", out)
		}
	})
}

// TestHTTPChatTimeoutEnvOverride pins the per-deployment override knobs. An
// operator has to be able to widen (or, during an incident, tighten) the
// heavy tier without a rebuild — and a typo in the environment must fall back
// to the default rather than silently removing the bound altogether.
func TestHTTPChatTimeoutEnvOverride(t *testing.T) {
	t.Run("heavy tier override", func(t *testing.T) {
		t.Setenv(envHTTPHeavyTimeout, "90s")
		if got := HeavyChatTimeout(); got != 90*time.Second {
			t.Fatalf("expected 90s from %s, got %s", envHTTPHeavyTimeout, got)
		}
	})

	t.Run("light tier override", func(t *testing.T) {
		t.Setenv(envHTTPTimeout, "45s")
		if got := LightChatTimeout(); got != 45*time.Second {
			t.Fatalf("expected 45s from %s, got %s", envHTTPTimeout, got)
		}
	})

	t.Run("unparsable value falls back to the default", func(t *testing.T) {
		t.Setenv(envHTTPHeavyTimeout, "abc")
		if got := HeavyChatTimeout(); got != httpChatTimeoutHeavy {
			t.Fatalf("expected the %s default (%s) for an unparsable value, got %s",
				envHTTPHeavyTimeout, httpChatTimeoutHeavy, got)
		}
	})

	t.Run("non-positive value falls back to the default", func(t *testing.T) {
		t.Setenv(envHTTPHeavyTimeout, "0s")
		if got := HeavyChatTimeout(); got != httpChatTimeoutHeavy {
			t.Fatalf("expected the %s default (%s) for a non-positive value, got %s",
				envHTTPHeavyTimeout, httpChatTimeoutHeavy, got)
		}
	})
}
