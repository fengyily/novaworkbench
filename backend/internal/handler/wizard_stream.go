package handler

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/novaworkbench/backend/internal/llm"
	"github.com/novaworkbench/backend/internal/service"
	"github.com/novaworkbench/backend/internal/store"
)

// toolCallLabel returns a human-readable Chinese label for a tool call event.
func toolCallLabel(toolName string, input map[string]interface{}) string {
	switch toolName {
	case "Read":
		if path, ok := input["file_path"].(string); ok {
			return "📖 读取文件: " + path
		}
	case "Bash":
		if cmd, ok := input["command"].(string); ok {
			short := cmd
			if len(short) > 60 {
				short = short[:60] + "..."
			}
			return "⚡ 执行命令: " + short
		}
	case "Glob":
		if pattern, ok := input["pattern"].(string); ok {
			return "🔍 搜索文件: " + pattern
		}
	case "Grep":
		if pattern, ok := input["pattern"].(string); ok {
			return "🔍 搜索内容: " + pattern
		}
	case "Write":
		if path, ok := input["file_path"].(string); ok {
			return "✏️ 写入文件: " + path
		}
	case "Edit":
		if path, ok := input["file_path"].(string); ok {
			return "✏️ 编辑文件: " + path
		}
	}
	return "🔧 " + toolName
}

// toolResultContent extracts and truncates the content of a tool_result block.
func toolResultContent(b map[string]interface{}) string {
	switch v := b["content"].(type) {
	case string:
		return truncateStr(v, 200)
	case []interface{}:
		var parts []string
		for _, item := range v {
			if m, ok := item.(map[string]interface{}); ok {
				if text, ok := m["text"].(string); ok {
					parts = append(parts, text)
				}
			}
		}
		return truncateStr(strings.Join(parts, " "), 200)
	}
	return ""
}

// extractJSON strips prose and code fences surrounding a JSON object.
// It finds the first '{' and the matching closing '}', returning that substring.
func extractJSON(s string) string {
	s = strings.TrimSpace(s)
	start := strings.Index(s, "{")
	if start == -1 {
		return s
	}
	depth := 0
	inStr := false
	escape := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if escape {
			escape = false
			continue
		}
		if c == '\\' && inStr {
			escape = true
			continue
		}
		if c == '"' {
			inStr = !inStr
			continue
		}
		if inStr {
			continue
		}
		if c == '{' {
			depth++
		} else if c == '}' {
			depth--
			if depth == 0 {
				return strings.TrimSpace(s[start : i+1])
			}
		}
	}
	return strings.TrimSpace(s[start:])
}

// claudeResultError extracts a human-readable error message from a non-success
// claude "result" event. The CLI surfaces auth/transport failures in the
// "error", "api_error_status", or "result" fields; we also log the full event.
func claudeResultError(scope string, evt map[string]interface{}) string {
	msg := "Claude 执行出错"
	if v, ok := evt["error"].(string); ok && v != "" {
		msg = v
	} else if v, ok := evt["result"].(string); ok && v != "" {
		msg = v
	} else if v, ok := evt["api_error_status"].(float64); ok && v != 0 {
		msg = fmt.Sprintf("API error (HTTP %v)", v)
	}
	// An upstream proxy 400 (bad request / not found) usually means the
	// configured base URL / token / model is wrong or the account is out of
	// quota — point the user at the settings rather than blaming the model.
	lower := strings.ToLower(msg)
	if strings.Contains(lower, "400") &&
		(strings.Contains(lower, "bad request") || strings.Contains(lower, "not found")) {
		msg += "\n（这是上游代理返回的 400，通常是 BASE_URL / Token / 模型 配置有误或额度耗尽，请在「设置」里检查 Claude 配置）"
	}
	if raw, err := json.Marshal(evt); err == nil {
		log.Printf("[%s] claude result event: %s", scope, truncateStr(string(raw), 800))
	}
	return msg
}

// extractStreamError pulls every diagnostic field we can out of a top-level
// {"type":"error",...} NDJSON event. The Claude CLI and upstream proxies
// each use their own conventions for the message:
//   - claude CLI: {"type":"error","error":"<string>"} or
//     {"type":"error","error":{"message":"<string>", ...}}
//   - third-party relays sometimes nest as {"type":"error","message":"..."}
//   - some payloads carry a sibling "api_error_status" / "error_type" hint
//
// nova-agent-worker additionally serializes a few extra fields from the
// non-zero child-process exit: code / signal / stderr. The CLI's actual
// error line ("401 Unauthorized", "ENOTFOUND api.anthropic.com", "model
// not found") lives in `stderr` and is by far the most useful diagnostic —
// we surface it after the top-level message, capped to ~1.5KB so a chatty
// CLI can't blow up the SSE envelope. When the message / stderr hint at an
// upstream 400 / not found, append the same proxy-config guidance
// claudeResultError does.
//
// The worker also attaches an `errorCategory` (computed via its
// classifyError) — auth_failed / network_unreachable / model_not_found /
// unrecognized_model / etc. — that we map to a tailored Chinese fix hint
// appended after stderr. This is what turns the previously opaque
// "Claude Code process exited with code 1" into actionable guidance.
func extractStreamError(evt map[string]interface{}) string {
	var msg string
	switch v := evt["error"].(type) {
	case string:
		msg = v
	case map[string]interface{}:
		if s, ok := v["message"].(string); ok && s != "" {
			msg = s
		}
	}
	if msg == "" {
		if s, ok := evt["message"].(string); ok && s != "" {
			msg = s
		}
	}
	if msg == "" {
		return ""
	}

	// Append the CLI's stderr (if the worker captured it) — this is where
	// the actionable diagnostic usually is. Truncate to 1.5KB to keep the
	// SSE message readable; the full text is also available in the backend
	// log via the raw json.Marshal below.
	if stderr, ok := evt["stderr"].(string); ok && strings.TrimSpace(stderr) != "" {
		stderr = strings.TrimSpace(stderr)
		if len(stderr) > 1500 {
			stderr = stderr[:1500] + "\n…[truncated]"
		}
		msg += "\n[stderr]\n" + stderr
	}

	// Append the exit code if present, for quick scanning.
	if code, ok := evt["code"]; ok {
		msg += fmt.Sprintf("\n[exit_code] %v", code)
	} else if code, ok := evt["exitCode"]; ok {
		msg += fmt.Sprintf("\n[exit_code] %v", code)
	}

	// Append nested cause (one level) — sometimes an upstream relay wraps
	// a transport error inside a higher-level Error and only the inner
	// one names the host. Kept for forward-compat with the previous
	// SDK-shaped payloads an older worker may still emit.
	if cause, ok := evt["cause"].(string); ok && cause != "" {
		msg += "\n[cause] " + cause
	}

	// Worker-classified category → tailored fix hint. When the worker emits
	// a preflight failure we hoist the hint above the generic 400 check so
	// the user sees the actionable reason first. Categories must match the
	// strings nova-agent-worker/server.mjs:classifyError emits; if a new
	// category is added there, add a hint here too.
	if cat, _ := evt["errorCategory"].(string); cat != "" {
		if hint := workerCategoryHint(cat, msg); hint != "" {
			msg += "\n[诊断] " + hint
		}
	}

	lower := strings.ToLower(msg)
	if strings.Contains(lower, "400") &&
		(strings.Contains(lower, "bad request") || strings.Contains(lower, "not found")) {
		msg += "\n（这是上游代理返回的 400，通常是 BASE_URL / Token / 模型 配置有误或额度耗尽，请在「设置」里检查 Claude 配置）"
	}
	if raw, err := json.Marshal(evt); err == nil {
		log.Printf("[stream-error] %s", truncateStr(string(raw), 1200))
	}
	return msg
}

// workerCategoryHint maps nova-agent-worker's errorCategory to a Chinese
// fix hint. Keep the categories in sync with classifyError in server.mjs
// (the worker writes the strings verbatim — a typo here silently breaks
// the hint without surfacing).
//
// Hint scope: actionable and bounded. We deliberately do NOT explain every
// possible root cause (the stderr already does that); we tell the user
// what knob to turn next.
func workerCategoryHint(cat, msg string) string {
	switch cat {
	case "cli_not_found":
		return "Claude CLI 未找到。请在 Agent 服务器上确认 `claude --version` 可执行，或重新「安装依赖」。"
	case "auth_failed":
		return "鉴权失败（401）。请在「设置 → Claude 配置」检查 ANTHROPIC_AUTH_TOKEN 是否已填写并生效。"
	case "auth_forbidden":
		return "权限不足（403）。Token 可能有效但缺少调用该模型的权限，或 base URL 指向了无权访问的端点。"
	case "model_not_found":
		return "上游 API 不认识这个 model（404）。请检查「设置 → Claude 配置」里的 model 与 base URL，或确认模型名拼写正确。"
	case "unrecognized_model":
		// We pin MiniMax-M3 (or a similar custom id) via the --settings env
		// block together with CLAUDE_CODE_DISABLE_UNKNOWN_MODEL_WINDOW_
		// ENFORCEMENT=1, which suppresses Claude Code's "model isn't in my
		// local catalog, I'll assume 200k context and might fail" warning —
		// see gateway.go: settingsEnvOverrides. If the warning still surfaces
		// here, either the env block didn't reach the worker (stale
		// server.mjs) or the CLI version on the agent server doesn't honor
		// that knob.
		//
		// The `[1m]` / `[0m]` markers in the stderr are Claude Code's
		// own ANSI color escapes leaking into the JSON it emits — a CLI
		// bug, not our model name. The actual id is whatever's set in
		// 「设置 → Claude 配置」.
		return "Claude Code 不在本地 model 目录里认识这个 model（自定义 model 走私有 base URL 时常见）。gateway.go 已自动注入 CLAUDE_CODE_DISABLE_UNKNOWN_MODEL_WINDOW_ENFORCEMENT=1 让 CLI 跳过 catalog 检查，但本机仍报此错通常意味着：(1) worker 还在跑旧版 server.mjs，没拿到新的 env（请在「设置 → Agent 服务器」点「安装依赖」）；(2) Agent 服务器上的 Claude Code 版本过旧不识别该 env 变量（请运行 `claude --version` 升级）。详细：stderr 里的 `[1m]`/`[0m` 是 Claude Code CLI 自带的 ANSI 颜色控制符泄漏进 JSON，不是 model 名真的带这些字符。"
	case "rate_limited":
		return "上游限流（429）。请稍候几分钟重试，或降低并发。"
	case "quota_exceeded":
		return "账户额度耗尽。请充值或换用其他 Claude 配置后重试。"
	case "network_unreachable":
		return "Agent 服务器无法访问到上游 API。请检查出口网络/防火墙/代理。"
	case "dns_unresolved":
		return "Agent 服务器 DNS 解析失败。请检查 /etc/resolv.conf 或上游 base URL 域名拼写。"
	case "connection_refused":
		return "上游连接被拒（ECONNREFUSED）。通常是 base URL 端口错，或上游服务未启动。"
	case "connection_reset":
		return "上游连接被重置（ECONNRESET）。通常是代理或防火墙打断了长连接，重试即可。"
	case "permission_denied":
		// When the EACCES path is /var/folders the real cause is almost
		// always that $TMPDIR was inherited from a macOS dev box and
		// forwarded into the SSH session on a Linux/Windows agent host
		// where /var/folders doesn't exist. claude's internal tmpdir
		// setup then EACCESes on the missing parent before --print ping
		// even starts. nova-agent-worker now auto-overrides TMPDIR
		// (existing → $HOME → /tmp → cwd → bare /tmp) before each
		// spawn, and the systemd/launchd unit + nohup launcher all pin
		// TMPDIR=/tmp so the worker process itself starts with a sane
		// tmpdir. This hint therefore usually points at the worker not
		// having picked up the new server.mjs yet (i.e. re-Install is
		// the missing step), or at a stale node process holding the
		// old in-memory code after a partial install.
		if strings.Contains(msg, "/var/folders") {
			return "Claude 进程的 $TMPDIR 指向 /var/folders，但该路径在 Agent 服务器上不存在（开发机是 macOS，$TMPDIR 跟随 SSH 会话转发到了 Linux 远端）。worker 已自动覆盖 TMPDIR（现有 → $HOME → /tmp → 兜底 /tmp），仍报错通常是 worker 还没拉到新版 server.mjs 或 systemd 未重启。请 SSH 到 Agent 服务器执行 `systemctl --user restart nova-agent-worker.service`（或在「设置 → Agent 服务器」点一次「安装依赖」），然后查看 ~/nova-agent-worker/worker.log 中 `[nova-agent-worker] resolved TMPDIR via …` 那行确认 TMPDIR 已切到 /home/<user>/.nova-agent-worker-XXXX 或 /tmp/.nova-agent-worker-XXXX。若日志显示 `/tmp` 兜底分支，说明 $HOME 不可写，请检查 Agent 服务器上 nova-agent-worker 进程对 $HOME 目录是否有写权限。"
		}
		return "本地文件系统权限不足（EACCES）。请检查 worktree 路径对当前 SSH 用户是否可写。"
	case "session_not_found":
		return "找不到要 resume 的会话。本地会话 jsonl 未上传到 Agent 服务器，或 slug 不匹配，建议重新分析或开新会话。"
	case "max_turns":
		return "Claude 达到单轮最大工具调用次数。请把需求拆小，或在提示词里限制工具调用总数。"
	case "preflight_timeout":
		return "preflight 5 秒内未完成（`claude --print ping` 卡住）。通常是 Agent 服务器无法访问 API，请检查网络。"
	case "running_as_root":
		// Claude CLI refuses --dangerously-skip-permissions when the current
		// uid is root (or sudo is in effect) for security reasons. The CLI
		// surfaces this as a non-zero exit before any tool/API call, so the
		// wizard sees an opaque "exit 1" without this classification. The
		// install flow now auto-provisions a non-root user and switches the
		// stored SSH username (see agent_server.go:runInstall), so the first
		// piece of advice is "re-run 安装依赖"; the manual steps stay as the
		// password-auth / already-provisioned fallback.
		return "Claude CLI 出于安全考虑拒绝以 root/sudo 身份执行 --dangerously-skip-permissions。如果这台服务器使用 SSH 私钥认证，回到「设置 → Agent 服务器」重新点「安装依赖」即可自动创建普通用户（nova）并把 SSH 用户名切换过去，无需手动操作；如果使用密码认证，请在服务器上手动创建普通用户并把 NovaWorkbench 所在机器的 SSH 公钥写入其 `~/.ssh/authorized_keys`，然后把该服务器的 SSH 用户名改为该普通用户后再重试。"
	}
	return ""
}

// claudeStreamOutcome is the result of running one claude stream-json command to
// completion. finalResult holds the "result" event text on success. On failure
// errMsg is a human-readable message; staleSession is true when the failure was
// specifically a --resume against a conversation that no longer exists on disk,
// so the caller can transparently fall back to a fresh session instead of
// surfacing a hard error.
type claudeStreamOutcome struct {
	finalResult      string
	sessionID        string // session_id of this run, read from the system/init event. For a --fork-session run this is the NEW forked id.
	staleSession     bool
	errMsg           string
	hadStreamEvents  bool   // true if any stream_event/content_block_delta arrived
	eventCount       int    // total NDJSON events parsed (any type)
	streamEventCount int    // subset that are stream_event
	lastEventType    string // type field of the most recent event, used for EOF postmortem
	planContent      string // full markdown captured from a plan-mode Write tool_use to ~/.claude/plans/*.md
	// subTasksJSON is the authoritative sub-task decomposition payload,
	// captured from a Write tool_use whose target path ends with
	// /.novaworkbench/subtasks.json. Unlike the free-text JSON block +
	// [SUBTASKS_READY] sentinel, the Write tool_use input arrives as one
	// structured event — immune to embedded code fences, truncation, or
	// markdown mangling (req_9d24ef181a5ad5c4). tryAutoOrchestrate prefers
	// this over every text-parsing fallback.
	subTasksJSON string
	actualModel  string   // model id returned by the API, captured from the assistant event's message.model
	toolFiles    []string // file paths / patterns touched by Read/Write/Edit/Grep/Glob tool calls (for knowledge-usage evaluation)
	// lastUsage captures the four token counts from the terminal result event
	// (or zero values when the stream ended before reaching a result). The
	// compress-context handler reads this to populate the `done` payload's
	// tokens_used field without a second DB roundtrip.
	lastUsage lastUsageSnapshot
}

// lastUsageSnapshot is the token-count view of a single claude turn, derived
// from the result.usage block. Stored on claudeStreamOutcome so handlers that
// need to surface the cost of a turn (compress-context modal, future SSE
// telemetry) can read it without re-extracting from the original event.
type lastUsageSnapshot struct {
	InputTokens         int
	OutputTokens        int
	CacheCreationTokens int
	CacheReadTokens     int
}

// isStaleSessionError reports whether a non-success result event (optionally
// combined with stderr) indicates the --resume target conversation doesn't exist
// on disk. The claude CLI surfaces this as an "errors" array entry of the form
// "No conversation found with session ID: <uuid>" (subtype error_during_execution).
// evt may be nil, in which case only stderr is checked.
func isStaleSessionError(evt map[string]interface{}, stderr string) bool {
	contains := func(s string) bool { return strings.Contains(s, "No conversation found") }
	if stderr != "" && contains(stderr) {
		return true
	}
	if evt == nil {
		return false
	}
	if s, ok := evt["error"].(string); ok && contains(s) {
		return true
	}
	if s, ok := evt["result"].(string); ok && contains(s) {
		return true
	}
	if s, ok := evt["message"].(string); ok && contains(s) {
		return true
	}
	if errs, ok := evt["errors"].([]interface{}); ok {
		for _, e := range errs {
			if s, ok := e.(string); ok && contains(s) {
				return true
			}
		}
	}
	return false
}

// streamSink consumes one parsed claude stream-json event as a UI log line.
// The SSE sink frames it as a "data: {...}\n\n" SSE event and flushes per line;
// the job sink appends it to a JobStore job so background-job subscribers
// receive it (and survive a page refresh via the job's replay buffer).
type streamSink interface {
	emit(line store.LogLine)
}

// sseSink writes log lines directly to an SSE response, flushed per event.
type sseSink struct {
	w  http.ResponseWriter
	rc *http.ResponseController
}

func (s sseSink) emit(line store.LogLine) {
	if line.At == 0 {
		line.At = time.Now().UnixMilli()
	}
	data, _ := json.Marshal(line)
	fmt.Fprintf(s.w, "data: %s\n\n", string(data))
	s.rc.Flush()
}

// jobSink appends log lines to a JobStore job, fanning them out to subscribers.
type jobSink struct {
	job *store.Job
}

func (s jobSink) emit(line store.LogLine) {
	s.job.Append(line)
}

// silentSink is a streamSink that discards log output. AutoOrchestrate runs
// the orchestrator's main-agent turn inline (synchronously) so the
// streaming output is invisible to the user — only the terminal
// finalResult matters. We still go through runClaudeStream so we get the
// same token-usage recording as developer-chat.
type silentSink struct{}

func (silentSink) emit(line store.LogLine) {}

// runClaudeStream starts a claude stream-json cmd, parses its events, and
// translates them to UI log lines via the sink (phase / tool_call / message
// frames). It does NOT emit terminal error/done frames — the caller owns those.
// On a non-success result it returns the error message (and flags a stale
// --resume so the caller can recover). The caller must NOT have started cmd.
//
// uctx, when non-nil, records the result event's token usage as a token_usage
// row (best-effort — errors are logged and never break the stream). Pass the
// same uctx to both calls of a stale-fallback retry so each invocation's
// tokens are recorded.
func runClaudeStream(sink streamSink, cmd *exec.Cmd, scope string, uctx *usageCtx) claudeStreamOutcome {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return claudeStreamOutcome{errMsg: "启动 Claude 失败: " + err.Error()}
	}
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf
	// Put the claude subprocess in its own process group so we can kill the
	// whole group (claude + any tool/MCP subprocesses it spawned). Some
	// third-party proxies keep the upstream connection open after the model's
	// final event: the CLI never exits on its own, holding stdout open so a
	// plain scanner.Scan() blocks forever and the job never finishes. Killing
	// the group on completion lets runClaudeStream return promptly.
	//
	// The Setpgid assignment + group-kill logic live in wizard_proc_unix.go
	// (//go:build !windows) because process groups are a POSIX concept. On
	// Windows the helpers in wizard_proc_windows.go fall back to signaling
	// just the direct process — no orphan grand-children issue exists there.
	setProcessGroup(cmd)
	// Single concise startup line in the happy path so the server log doesn't
	// flood with LookPath / EvalSymlinks / stat / magic / ldd dumps every
	// time a sub-task forks. The full diagnostic only fires when Start()
	// fails — that's the case that actually needs the snapshot to pinpoint
	// the cause (ENOENT alone is ambiguous).

	log.Printf("[%s] claude 启动中 (binary=%s args=%d)", scope, filepath.Base(cmd.Path), len(cmd.Args))
	if err := cmd.Start(); err != nil {
		logClaudeExecDiag(scope, cmd)
		log.Printf("[%s] exec diag: cmd.Start() failed: %T %v", scope, err, err)
		return claudeStreamOutcome{errMsg: "启动 Claude 失败: " + err.Error()}
	}
	logClaudeEnvConfig(scope, cmd)
	logClaudeCmd(scope, cmd)

	var out claudeStreamOutcome
	// thinkingTokens tracks the last token count we reported so we can
	// throttle thinking_tokens progress messages (every 50 tokens).
	var lastThinkingReport int
	// gotResult is set when the terminal "result" event arrives; once set we
	// stop reading and tear the process down — there is nothing useful after
	// the result event, and waiting for stdout EOF can hang forever on proxies
	// that don't close the stream.
	gotResult := false
	// Stall watchdog: armed after the first stdout line. If no new line
	// arrives for stallTimeout (proxy went silent without emitting a result
	// event), kill the group so the scan loop unblocks. Without this a proxy
	// that drops the connection mid-stream would wedge the job permanently.
	const stallTimeout = 3 * time.Minute
	heartbeats := make(chan struct{}, 1)
	watchdogDone := make(chan struct{})
	go func() {
		defer close(watchdogDone)
		timer := time.NewTimer(stallTimeout)
		if !timer.Stop() {
			<-timer.C
		}
		var armed bool
		for {
			select {
			case _, ok := <-heartbeats:
				if !ok {
					return
				}
				if !armed {
					armed = true
				}
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(stallTimeout)
			case <-timer.C:
				log.Printf("[%s] stream stalled %v with no new events; terminating", scope, stallTimeout)
				killProcessGroup(cmd)
				return
			}
		}
	}()
	beat := func() {
		select {
		case heartbeats <- struct{}{}:
		default:
		}
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 256*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		beat()
		if line == "" {
			continue
		}
		var evt map[string]interface{}
		if err := json.Unmarshal([]byte(line), &evt); err != nil {
			// stream-json is one JSON object per line; a non-JSON line is usually
			// a fatal error printed by the CLI. Surface it in the logs.
			if strings.TrimSpace(line) != "" {
				log.Printf("[%s] non-json stdout: %s", scope, truncateStr(line, 500))
			}
			continue
		}
		evtType, _ := evt["type"].(string)
		switch evtType {
		case "system":
			sub, _ := evt["subtype"].(string)
			switch sub {
			case "init":
				// The init event arrives as soon as Claude connects (a few seconds
				// in). Some proxies buffer the whole model response and only deliver
				// it at completion, so without this the client would see nothing for
				// the entire wait. Surface it as a phase so the user knows Claude is
				// connected and thinking.
				if sid, ok := evt["session_id"].(string); ok && sid != "" {
					out.sessionID = sid
				}
				sink.emit(store.LogLine{Type: "phase", Content: "🤖 Claude 已连接，正在思考…"})
			case "thinking_tokens":
				// Third-party proxy models (e.g. zai-org/glm) don't stream text
				// token-by-token; they batch everything into the final assistant event.
				// But they DO stream thinking_tokens incrementally, giving us a
				// heartbeat we can use to show progress. Report every 50 tokens so
				// the user sees live activity instead of a frozen spinner.
				if tokens, ok := evt["estimated_tokens"].(float64); ok {
					t := int(tokens)
					if t-lastThinkingReport >= 50 {
						lastThinkingReport = t
						sink.emit(store.LogLine{Type: "phase", Content: fmt.Sprintf("🤔 模型思考中… (%d tokens)", t)})
					}
				}
			}
		case "stream_event":
			// stream-json emits incremental content_block_delta events as Claude
			// generates text token-by-token. Surfacing these gives the user
			// real-time output instead of waiting for the batched "assistant" event.
			inner, _ := evt["event"].(map[string]interface{})
			if inner == nil {
				continue
			}
			innerType, _ := inner["type"].(string)
			switch innerType {
			case "content_block_delta":
				delta, _ := inner["delta"].(map[string]interface{})
				if delta == nil {
					continue
				}
				deltaType, _ := delta["type"].(string)
				switch deltaType {
				case "text_delta":
					text, _ := delta["text"].(string)
					if text == "" {
						continue
					}
					out.hadStreamEvents = true
					sink.emit(store.LogLine{Type: "message", Content: text})
				case "input_json_delta":
					// tool input being assembled — not surfaced to the client
				}
			case "content_block_start":
				// A new tool_use block starting — surface the tool name immediately
				// so the user sees "calling tool X" before the input is assembled.
				block, _ := inner["content_block"].(map[string]interface{})
				if block == nil {
					continue
				}
				if block["type"] == "tool_use" {
					toolName, _ := block["name"].(string)
					if toolName != "" {
						sink.emit(store.LogLine{Type: "tool_call", Content: toolCallLabel(toolName, nil)})
					}
				}
			}
		case "assistant":
			// The "assistant" event is a batched summary of the full turn. When the
			// model supports streaming (stream_event/content_block_delta), text is
			// already delivered incrementally and we skip it here. When the model
			// does NOT emit stream_event (e.g. third-party proxy models), the
			// assistant event carries the only copy of the text, so we emit it now.
			// We track whether any stream_event text arrived to decide which path to take.
			msg, _ := evt["message"].(map[string]interface{})
			// The message carries the model id the API actually served — capture
			// it so the recorded token_usage row matches a model in the config's
			// price list (used for cost attribution; overrides the pre-dispatch
			// role-config model at the result event below).
			if m, ok := msg["model"].(string); ok {
				out.actualModel = m
			}
			content, _ := msg["content"].([]interface{})
			for _, block := range content {
				b, _ := block.(map[string]interface{})
				switch b["type"] {
				case "tool_use":
					toolName, _ := b["name"].(string)
					input, _ := b["input"].(map[string]interface{})
					// Capture the plan file content from a plan-mode Write to
					// ~/.claude/plans/*.md. The assistant event carries the complete
					// tool_use input (unlike stream_event input_json_delta which is
					// fragmentary), so this is the authoritative source.
					if toolName == "Write" && input != nil {
						if fp, ok := input["file_path"].(string); ok {
							if strings.Contains(filepath.ToSlash(fp), "/.claude/plans/") && strings.HasSuffix(fp, ".md") {
								if c, ok := input["content"].(string); ok && c != "" {
									out.planContent = c
								}
							}
							// Primary orchestration channel: the developer main
							// agent is asked to Write its decomposition to
							// .novaworkbench/subtasks.json. Capture the content
							// from the structured tool_use input instead of
							// parsing the assistant's free text.
							if strings.HasSuffix(filepath.ToSlash(fp), "/"+subTasksFileRelPath) {
								if c, ok := input["content"].(string); ok && c != "" {
									out.subTasksJSON = c
								}
							}
						}
					}
					// Harvest the touched path/pattern for the "was the injected
					// knowledge actually used?" evaluation (cheap: no LLM call, just
					// string capture from events that are already flowing).
					if p := inputToolPath(toolName, input); p != "" {
						out.toolFiles = append(out.toolFiles, p)
					}
					// Emit with full input context (content_block_start fired bare label already,
					// but only if stream_event was delivered — safe to emit again, client dedupes by rendering order).
					if !out.hadStreamEvents {
						sink.emit(store.LogLine{Type: "tool_call", Content: toolCallLabel(toolName, input)})
					}
				case "text":
					// Only emit if we never received stream_event deltas for this turn,
					// i.e. the model batches everything into the assistant event.
					if !out.hadStreamEvents {
						text, _ := b["text"].(string)
						if text != "" {
							sink.emit(store.LogLine{Type: "message", Content: text})
						}
					}
				}
			}
		case "result":
			subtype, _ := evt["subtype"].(string)
			// The CLI sometimes emits subtype:"success" even when the upstream
			// proxy rejected the request — it carries api_error_status (a number),
			// is_error:true, terminal_reason:"api_error", and a "result" of
			// "API Error: 400 ...". Treat those as failures so the error surfaces
			// via SSE instead of being returned as if it were the model's output.
			apiErrStatus := 0.0
			if v, ok := evt["api_error_status"].(float64); ok {
				apiErrStatus = v
			}
			isErr, _ := evt["is_error"].(bool)
			terminalReason, _ := evt["terminal_reason"].(string)
			realSuccess := subtype == "success" && !isErr &&
				terminalReason != "api_error" && apiErrStatus == 0
			if realSuccess {
				out.finalResult, _ = evt["result"].(string)
			} else {
				out.errMsg = claudeResultError(scope, evt)
				if isStaleSessionError(evt, stderrBuf.String()) {
					out.staleSession = true
					log.Printf("[%s] stale --resume detected (session not on disk)", scope)
				}
			}
			// Override the pre-dispatch model with the API-returned one so the
			// recorded row costs against the right model entry. No-op when the
			// proxy didn't report a model (fall back to the effective model).
			if out.actualModel != "" && uctx != nil {
				uctx.Model = out.actualModel
			}
			// Record token usage from the result event (success or failure —
			// tokens are consumed either way). Best-effort: recordFrom swallows
			// all errors so a DB hiccup can never break the claude stream.
			uctx.recordFrom(evt)

			// Stash the same counts onto the outcome so handlers (compress-
			// context, future telemetry) can read the cost of THIS turn
			// without going back to the original stream event.
			if inTok, outTok, cc, cr, ok := llm.ParseStreamUsage(evt); ok {
				out.lastUsage = lastUsageSnapshot{
					InputTokens:         inTok,
					OutputTokens:        outTok,
					CacheCreationTokens: cc,
					CacheReadTokens:     cr,
				}
			}

			// Mirror the same usage data through SSE so the wizard chat panel
			// can render a live "上下文 X%" bar without polling /api/usage/*.
			// We emit even on failure paths (realSuccess==false) because the
			// model still spent tokens before erroring out — those tokens
			// count against the user's budget just the same. Only skip when
			// the event carried no usage at all (ParseStreamUsage.ok=false).
			if sink != nil && uctx != nil {
				if inTok, outTok, cc, cr, ok := llm.ParseStreamUsage(evt); ok && (inTok+outTok+cc+cr) > 0 {
					modelName := uctx.Model
					if modelName == "" {
						modelName = out.actualModel
					}
					payload := map[string]any{
						"step":                  uctx.Step,
						"model":                 modelName,
						"input_tokens":          inTok,
						"output_tokens":         outTok,
						"cache_creation_tokens": cc,
						"cache_read_tokens":     cr,
						"context_window":        service.ModelContextWindow(modelName),
					}
					if b, mErr := json.Marshal(payload); mErr == nil {
						sink.emit(store.LogLine{Type: "usage", Content: string(b)})
						// Same payload, second outlet: persist into the
						// requirements.usage_snapshots blob so the frontend can
						// seed its usage bars from the Requirement GET and
						// survive a page refresh / panel collapse instead of
						// dropping to 0%. snapshotStep maps the usage step to
						// the wizard session; "" (compress_* turns) skips. The
						// closure swallows its own errors — never breaks the
						// stream. The SSE payload carries `step` (the usage
						// label) while the snapshot is keyed by session, so we
						// rebuild the snapshot value without `step`.
						if uctx.PersistSnapshot != nil {
							if key := snapshotStep(uctx.Step); key != "" {
								snap := map[string]any{
									"model":                 modelName,
									"input_tokens":          inTok,
									"output_tokens":         outTok,
									"cache_creation_tokens": cc,
									"cache_read_tokens":     cr,
									"context_window":        service.ModelContextWindow(modelName),
								}
								if sb, sErr := json.Marshal(snap); sErr == nil {
									uctx.PersistSnapshot(key, string(sb))
								}
							}
						}
					}
				}
			}
			gotResult = true
		case "error":
			// stream-json sometimes emits a top-level error event before any
			// result/system (e.g. claude CLI exited non-zero on startup, auth
			// rejected at the proxy, model rejected, network DNS failure).
			// Without this case the error body is silently dropped and the
			// EOF branch only knows lastEventType="error" — leaving the user
			// guessing among "API hung", "401", "OOM", "argv truncated". The
			// CLI and any relay both use a string `error` field; some
			// payloads nest it as an object with a `message` sub-field, so
			// accept both shapes.
			msg := extractStreamError(evt)
			if msg != "" {
				if out.errMsg == "" {
					out.errMsg = msg
				}
				sink.emit(store.LogLine{Type: "error", Content: msg})
			}
		}
		if gotResult {
			break
		}
	}

	// Stop the stall watchdog and tear the process group down. The CLI may
	// still be alive (proxy held the connection open), and tool/MCP
	// grandchildren may still hold the stdout pipe open; killing the group
	// unblocks cmd.Wait() and lets us return what we captured.
	close(heartbeats)
	<-watchdogDone
	killProcessGroup(cmd)

	if err := cmd.Wait(); err != nil {
		stderrTrim := strings.TrimSpace(stderrBuf.String())
		log.Printf("[%s] claude exited: %v stderr=%s", scope, err, stderrTrim)
		// A non-zero exit is only fatal if we never got a result; some error
		// events still carry a usable result field.
		if out.finalResult == "" && out.errMsg == "" {
			msg := "Claude 异常退出: " + err.Error()
			if stderrTrim != "" {
				msg = "Claude 异常退出: " + stderrTrim
			}
			out.errMsg = msg
		}
	}
	// Re-check staleness against stderr in case the CLI printed the error there
	// instead of emitting a result event.
	if !out.staleSession && isStaleSessionError(nil, stderrBuf.String()) {
		out.staleSession = true
	}
	if out.staleSession && out.errMsg == "" {
		out.errMsg = "分析对话会话已过期"
	}
	return out
}

// parseStreamJSONFromReader is the io.Reader-only counterpart of
// runClaudeStream. It scans NDJSON events off the supplied reader and emits
// the same LogLine shape the local path uses (phase / tool_call / message /
// usage / knowledge_result). It does NOT own a subprocess; the caller is
// responsible for piping the remote claude's stdout into r and closing it
// after the remote command exits.
//
// All heavy lifting (event dispatch, model pinning, token recording, usage
// persistence) is mirrored from runClaudeStream so the resulting
// claudeStreamOutcome is interchangeable. The differences are:
//   - no process group / killProcessGroup — the remote shell is the parent's
//     equivalent and we don't have access to its pgid over SSH
//   - no stall watchdog — a stuck remote claude is killed by closing the
//     SSH session from the caller's defer (client.Close kills the channel)
//   - no stderr fallback for staleness detection (we never see the remote
//     stderr in this scope)
func parseStreamJSONFromReader(r io.Reader, sink streamSink, scope string, uctx *usageCtx) claudeStreamOutcome {
	var out claudeStreamOutcome
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 256*1024), 4*1024*1024)
	// Track the first few non-JSON lines so the postmortem in runRemoteCoding
	// can show what claude actually printed before the EOF (claude CLI often
	// emits a warning or an error message on stdout/stderr that isn't valid
	// NDJSON — e.g. "NotLoggedIn", "SyntaxError", or proxy banner lines).
	var firstNonJSON []string
	for scanner.Scan() {
		out.eventCount++
		line := scanner.Text()
		if line == "" {
			continue
		}
		var evt map[string]interface{}
		if err := json.Unmarshal([]byte(line), &evt); err != nil {
			if strings.TrimSpace(line) != "" {
				log.Printf("[%s] non-json line: %s", scope, truncateStr(line, 500))
				if len(firstNonJSON) < 3 {
					firstNonJSON = append(firstNonJSON, truncateStr(line, 240))
				}
			}
			continue
		}
		evtType, _ := evt["type"].(string)
		out.lastEventType = evtType
		switch evtType {
		case "system":
			sub, _ := evt["subtype"].(string)
			switch sub {
			case "init":
				if sid, ok := evt["session_id"].(string); ok && sid != "" {
					out.sessionID = sid
				}
				sink.emit(store.LogLine{Type: "phase", Content: "🤖 Claude 已连接（远程），正在思考…"})
			case "thinking_tokens":
				if tokens, ok := evt["estimated_tokens"].(float64); ok {
					sink.emit(store.LogLine{Type: "phase", Content: fmt.Sprintf("🤔 模型思考中… (%d tokens)", int(tokens))})
				}
			}
		case "stream_event":
			out.streamEventCount++
			out.lastEventType = "stream_event"
			inner, _ := evt["event"].(map[string]interface{})
			if inner == nil {
				continue
			}
			switch inner["type"] {
			case "content_block_delta":
				delta, _ := inner["delta"].(map[string]interface{})
				if delta == nil {
					continue
				}
				switch delta["type"] {
				case "text_delta":
					text, _ := delta["text"].(string)
					if text != "" {
						out.hadStreamEvents = true
						sink.emit(store.LogLine{Type: "message", Content: text})
					}
				}
			case "content_block_start":
				block, _ := inner["content_block"].(map[string]interface{})
				if block != nil && block["type"] == "tool_use" {
					if name, _ := block["name"].(string); name != "" {
						sink.emit(store.LogLine{Type: "tool_call", Content: toolCallLabel(name, nil)})
					}
				}
			}
		case "assistant":
			msg, _ := evt["message"].(map[string]interface{})
			if m, ok := msg["model"].(string); ok {
				out.actualModel = m
			}
			content, _ := msg["content"].([]interface{})
			for _, block := range content {
				b, _ := block.(map[string]interface{})
				switch b["type"] {
				case "tool_use":
					toolName, _ := b["name"].(string)
					input, _ := b["input"].(map[string]interface{})
					if toolName == "Write" && input != nil {
						if fp, ok := input["file_path"].(string); ok {
							if strings.Contains(filepath.ToSlash(fp), "/.claude/plans/") && strings.HasSuffix(fp, ".md") {
								if c, ok := input["content"].(string); ok && c != "" {
									out.planContent = c
								}
							}
						}
					}
					if p := inputToolPath(toolName, input); p != "" {
						out.toolFiles = append(out.toolFiles, p)
					}
					if !out.hadStreamEvents {
						sink.emit(store.LogLine{Type: "tool_call", Content: toolCallLabel(toolName, input)})
					}
				case "text":
					if !out.hadStreamEvents {
						if text, _ := b["text"].(string); text != "" {
							sink.emit(store.LogLine{Type: "message", Content: text})
						}
					}
				}
			}
		case "result":
			subtype, _ := evt["subtype"].(string)
			apiErrStatus := 0.0
			if v, ok := evt["api_error_status"].(float64); ok {
				apiErrStatus = v
			}
			isErr, _ := evt["is_error"].(bool)
			terminalReason, _ := evt["terminal_reason"].(string)
			realSuccess := subtype == "success" && !isErr &&
				terminalReason != "api_error" && apiErrStatus == 0
			if realSuccess {
				out.finalResult, _ = evt["result"].(string)
			} else {
				out.errMsg = claudeResultError(scope, evt)
				if isStaleSessionError(evt, "") {
					out.staleSession = true
				}
			}
			if out.actualModel != "" && uctx != nil {
				uctx.Model = out.actualModel
			}
			uctx.recordFrom(evt)
			if inTok, outTok, cc, cr, ok := llm.ParseStreamUsage(evt); ok {
				out.lastUsage = lastUsageSnapshot{InputTokens: inTok, OutputTokens: outTok, CacheCreationTokens: cc, CacheReadTokens: cr}
				if uctx != nil && (inTok+outTok+cc+cr) > 0 {
					modelName := uctx.Model
					if modelName == "" {
						modelName = out.actualModel
					}
					payload := map[string]any{
						"step":                  uctx.Step,
						"model":                 modelName,
						"input_tokens":          inTok,
						"output_tokens":         outTok,
						"cache_creation_tokens": cc,
						"cache_read_tokens":     cr,
						"context_window":        service.ModelContextWindow(modelName),
					}
					if b, mErr := json.Marshal(payload); mErr == nil {
						sink.emit(store.LogLine{Type: "usage", Content: string(b)})
						if uctx.PersistSnapshot != nil {
							if key := snapshotStep(uctx.Step); key != "" {
								snap := map[string]any{
									"model":                 modelName,
									"input_tokens":          inTok,
									"output_tokens":         outTok,
									"cache_creation_tokens": cc,
									"cache_read_tokens":     cr,
									"context_window":        service.ModelContextWindow(modelName),
								}
								if sb, sErr := json.Marshal(snap); sErr == nil {
									uctx.PersistSnapshot(key, string(sb))
								}
							}
						}
					}
				}
			}
			return out
		case "error":
			// nova-agent-worker emits {type:"error", error:"..."} when the
			// spawned claude CLI exits non-zero during initialization (auth
			// rejected at the proxy, DNS to api.anthropic.com failed, model
			// rejected, etc.). Without this case the body is dropped and
			// the EOF branch only knows lastEventType="error" — surfacing
			// the generic hint about API unreachable / OOM / argv
			// truncation, none of which pinpoints the actual cause. Accept
			// both string and object {message:...} shapes since the CLI
			// and any relay disagree.
			msg := extractStreamError(evt)
			if msg != "" {
				if out.errMsg == "" {
					out.errMsg = msg
				}
				sink.emit(store.LogLine{Type: "error", Content: msg})
			}
		case "log":
			// nova-agent-worker emits {type:"log", content:"..."} right before
			// spawning claude, carrying the exact command (auth token already
			// redacted) so a remote coding run is debuggable from the job panel
			// without SSHing into the agent host. Show it as a plain message.
			if content, _ := evt["content"].(string); content != "" {
				sink.emit(store.LogLine{Type: "message", Content: content})
			}
		}
	}
	// EOF without a result event — the remote claude exited before
	// completing the turn (network drop, etc.). The diagnostic now includes
	// stream-event count + last event type + any non-JSON preamble so the
	// user can tell "API hung silently after init" from "claude crashed
	// before producing anything".
	scanErr := scanner.Err()
	if out.finalResult == "" && out.errMsg == "" {
		var summary string
		if out.streamEventCount == 0 {
			summary = fmt.Sprintf("已收到 %d 个 NDJSON 事件（system/init 也未出现），最后类型=%q", out.eventCount, out.lastEventType)
		} else {
			summary = fmt.Sprintf("已收到 %d 个 NDJSON 事件，含 %d 个 stream_event（Claude 在输出文本/工具过程中断），最后类型=%q", out.eventCount, out.streamEventCount, out.lastEventType)
		}
		if len(firstNonJSON) > 0 {
			summary += "；非 JSON 前导输出: " + strings.Join(firstNonJSON, " | ")
		}
		if scanErr != nil {
			summary += "；scanner 错误: " + scanErr.Error()
		}
		out.errMsg = "远程 Claude 未返回结果（流中断 — " + summary + "）。最常见原因：Agent 服务器无法访问 api.anthropic.com（超时/DNS/防火墙），或 claude 进程崩溃/被 OOM kill，或 sshd exec argv 限制触发命令字符串被截断。"
	}
	return out
}

// logClaudeEnvConfig logs where the claude run's auth/config travels: the
// --settings JSON flag (values redacted — see logClaudeCmd) plus the presence
// of any auth keys left on the process env, which should be ABSENT when a
// pin is configured (localEnv strips them so --settings is the single source).
// Never logs values, only presence, so secrets stay out of the logs.
func logClaudeEnvConfig(scope string, cmd *exec.Cmd) {
	has := func(k string) bool {
		for _, kv := range cmd.Env {
			if key, _, _ := strings.Cut(kv, "="); key == k {
				return true
			}
		}
		return false
	}
	hasSettingsFlag := false
	for _, a := range cmd.Args {
		if a == "--settings" {
			hasSettingsFlag = true
			break
		}
	}
	log.Printf("[%s] claude config: --settings flag=%v env ANTHROPIC_AUTH_TOKEN=%v ANTHROPIC_BASE_URL=%v ANTHROPIC_API_KEY=%v",
		scope, hasSettingsFlag, has("ANTHROPIC_AUTH_TOKEN"), has("ANTHROPIC_BASE_URL"), has("ANTHROPIC_API_KEY"))
}

// logClaudeExecDiag dumps a diagnostic snapshot of the binary Go is about to
// exec. The single-line ENOENT returned by cmd.Start() is one of the most
// misleading errors in Go/Linux — the kernel returns ENOENT for at least
// half a dozen distinct causes (file missing, symlink target missing, ELF
// PT_INTERP missing, shebang script interpreter missing, noexec mount, …).
// When the next deploy hits this, we need enough raw state in the log to
// pinpoint the cause without another round-trip. Always log BEFORE Start so
// the failure case still gets the snapshot.
func logClaudeExecDiag(scope string, cmd *exec.Cmd) {
	if cmd.Path == "" {
		log.Printf("[%s] exec diag: cmd.Path is empty (LookPath must have failed earlier)", scope)
		return
	}
	log.Printf("[%s] exec diag: cmd.Path=%q args=%d env=%d", scope, cmd.Path, len(cmd.Args), len(cmd.Env))

	// 1. LookPath — does Go itself still find the path it just resolved?
	if _, err := exec.LookPath(cmd.Path); err != nil {
		log.Printf("[%s] exec diag: LookPath(%q) failed: %v", scope, cmd.Path, err)
	}

	// 2. Resolve symlinks — the kernel uses the resolved path for exec().
	resolved, err := filepath.EvalSymlinks(cmd.Path)
	if err != nil {
		log.Printf("[%s] exec diag: EvalSymlinks(%q) failed: %v", scope, cmd.Path, err)
		resolved = cmd.Path
	} else {
		log.Printf("[%s] exec diag: resolved=%q", scope, resolved)
	}

	// 3. stat the resolved file so we can see mode, size, mtime.
	if info, err := os.Stat(resolved); err != nil {
		log.Printf("[%s] exec diag: stat(%q) failed: %v", scope, resolved, err)
	} else {
		log.Printf("[%s] exec diag: stat mode=%s size=%d mtime=%s", scope, info.Mode(), info.Size(), info.ModTime().Format(time.RFC3339))
	}

	// 4. Magic bytes — distinguish script (#!) from ELF (\\x7fELF) from junk.
	if f, openErr := os.Open(resolved); openErr == nil {
		var head [4]byte
		n, _ := io.ReadFull(f, head[:])
		_ = f.Close()
		switch {
		case n >= 4 && head[0] == 0x7f && head[1] == 'E' && head[2] == 'L' && head[3] == 'F':
			log.Printf("[%s] exec diag: magic=ELF (pkg-bundled binary, no shebang)", scope)
		case n >= 2 && head[0] == '#' && head[1] == '!':
			log.Printf("[%s] exec diag: magic=#! (script), first line likely contains the interpreter path", scope)
		default:
			log.Printf("[%s] exec diag: magic=\\% x (n=%d)", scope, head[:n], n)
		}
	} else {
		log.Printf("[%s] exec diag: open(%q) failed: %v", scope, resolved, openErr)
	}

	// 5. ldd — surfaces missing shared libs (ENOENT-class) and the ELF PT_INTERP.
	if lddOut, lddErr := exec.Command("ldd", resolved).CombinedOutput(); lddErr == nil {
		out := strings.TrimSpace(string(lddOut))
		if out == "" {
			log.Printf("[%s] exec diag: ldd: (no output — binary not dynamic?)", scope)
		} else {
			log.Printf("[%s] exec diag: ldd:\n%s", scope, out)
		}
	} else {
		log.Printf("[%s] exec diag: ldd failed (%v): %s", scope, lddErr, strings.TrimSpace(string(lddOut)))
	}

	// 6. Critical env vars — PATH tells us where exec will look for the
	// interpreter if this is a script; HOME matters for nm/nvm-managed nodes.
	for _, kv := range cmd.Env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		switch k {
		case "PATH", "HOME", "PWD", "NODE_HOME", "NVM_HOME", "NVM_BIN":
			log.Printf("[%s] exec diag: env %s=%s", scope, k, v)
		}
	}

	// 7. CWD — both the server's own cwd and cmd.Dir (the project dir).
	if cwd, err := os.Getwd(); err == nil {
		log.Printf("[%s] exec diag: server cwd=%q", scope, cwd)
	}
	if cmd.Dir != "" {
		if _, err := os.Stat(cmd.Dir); err != nil {
			log.Printf("[%s] exec diag: cmd.Dir=%q DOES NOT EXIST ← likely real ENOENT cause: %v", scope, cmd.Dir, err)
		} else {
			log.Printf("[%s] exec diag: cmd.Dir=%q (exists)", scope, cmd.Dir)
		}
	}

	// 8. uid/gid — the Go process is the same throughout, but past bugs have
	// caught us when a service drops privileges. Cheap to log.
	log.Printf("[%s] exec diag: uid=%d gid=%d euid=%d egid=%d", scope, os.Getuid(), os.Getgid(), os.Geteuid(), os.Getegid())
}

// logClaudeCmd logs the actual claude CLI invocation as a shell command that
// can be copied and run directly in a terminal. The --settings JSON value is
// the one exception to "verbatim": it carries the configured auth token, so it
// is re-serialized with ANTHROPIC_AUTH_TOKEN / ANTHROPIC_API_KEY blanked (the
// remote worker's renderCommand applies the same rule, keeping the local and
// remote logs diffable on auth / base URL / model).
func logClaudeCmd(scope string, cmd *exec.Cmd) {
	parts := make([]string, 0, len(cmd.Args))
	for i, a := range cmd.Args {
		if i > 0 && cmd.Args[i-1] == "--settings" {
			a = llm.RedactSettings(a)
		}
		if len(a) > 200 {
			a = a[:200] + fmt.Sprintf("…(%d bytes total)", len(a))
		}
		parts = append(parts, shellQuote(a))
	}
	shell := ""
	if cmd.Dir != "" {
		shell = "cd " + shellQuote(cmd.Dir) + " && "
	}
	shell += strings.Join(parts, " ")
	log.Printf("[%s] claude cmd (copy-paste ready):\n%s", scope, shell)
}

// shellQuote wraps s in single quotes, escaping any single quotes inside.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

func streamLines(r io.Reader, w io.Writer, rc *http.ResponseController, streamType string) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		jsonLine, _ := json.Marshal(map[string]string{
			"type":    streamType,
			"content": line,
		})
		fmt.Fprintf(w, "data: %s\n\n", string(jsonLine))
		rc.Flush()
	}
	if err := scanner.Err(); err != nil {
		log.Printf("[wizard] stream error (%s): %v", streamType, err)
	}
}