package llm

import (
	"context"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// 推理档位（reasoning effort）—— claude CLI 的 `--effort <level>` 参数。
//
// 与「模型」同构但语义更轻的一等参数：模型决定「用哪颗脑子」，档位决定
// 「这颗脑子想多深」。CLI 侧的取值域是 low / medium / high / xhigh / max，
// 官方语义：
//
//	low     推理最少，适用于简单、范围明确或对延迟敏感的任务。
//	medium  在成本与智能之间折中，减少 token 消耗。
//	high    平衡 token 使用量与推理深度，是许多模型的默认级别。
//	xhigh   比 high 更深入的推理，消耗更多 token。位于 high 与 max 之间。
//	max     最深度的推理级别，适合解决极难问题，但可能产生边际效益递减。
//
// 注意 --effort 是 **纯 CLI flag，没有 env 形式**（不像模型那样能塞进
// --settings 的 env 块）。所以它必须作为一个真正的 argv 元素加进两个独立
// 的 argv 构造器：streamArgs（本机，gateway.go）与 buildClaudeArgs（远端
// Agent Server，agent-worker/server.mjs）。两处必须保持同序。
const (
	// EffortLow 推理最少 —— 简单、范围明确或对延迟敏感的任务。
	EffortLow = "low"
	// EffortMedium 在成本与智能之间折中，减少 token 消耗。
	EffortMedium = "medium"
	// EffortHigh 平衡 token 使用量与推理深度，许多模型的默认级别。
	EffortHigh = "high"
	// EffortXHigh 比 high 更深入的推理，消耗更多 token。
	EffortXHigh = "xhigh"
	// EffortMax 最深度的推理级别，适合极难问题。
	EffortMax = "max"
)

// 流水线阶段 —— 每个阶段有一个「默认档位」。阶段是档位的归属维度：
// 同一个模型在方案设计与在提交推送时应该用不同的推理深度。
//
// 设计 / 开发 / 提交推送 三个阶段的需求来自产品定义（方案设计 high、
// 开发实现 medium、提交推送 low）。StageOther 收拢其余所有运行（分析
// 会话、Review、阶段答疑、上下文压缩、Wiki 生成、周报）—— 它的出厂值
// 是 high，也就是 CLI 自身的默认级别，于是「未配置时行为与改造前等价」。
const (
	// StageDesign 方案设计阶段（方案设计 / 精修文档 / 应用文档 / 方案拆分）。
	StageDesign = "design"
	// StageCoding 开发实现阶段（开始 / 调整 / 继续开发 / 编排 / 普通子任务）。
	StageCoding = "coding"
	// StageCommit 提交推送阶段（push_pr 子任务 / 合并冲突解决 / PR 摘要）。
	StageCommit = "commit"
	// StageOther 其余运行（分析 / Review / 答疑 / 压缩 / Wiki / 周报）。
	StageOther = "other"
)

// 内置默认档位 —— 设置页未配置（或 KV 行缺失 / 非法）时的回落值。
//
// 这三个值就是产品定义的那三个：设计 high、开发 medium、提交推送 low。
// StageOther 用 high 是因为需求原文写明 high「是许多模型的默认级别」——
// 取它当默认值，未配置时各阶段的实际行为与引入 --effort 之前等价。
const (
	// DefaultEffortDesign 方案设计的默认推理档位。
	DefaultEffortDesign = EffortHigh
	// DefaultEffortCoding 开发实现的默认推理档位。
	DefaultEffortCoding = EffortMedium
	// DefaultEffortCommit 提交推送的默认推理档位。
	DefaultEffortCommit = EffortLow
	// DefaultEffortOther 其余运行的默认推理档位。
	DefaultEffortOther = EffortHigh
)

// effortTiers 是 CLI 接受的白名单（claude --help: "--effort <level>  Effort
// level for the current session (low, medium, high, xhigh, max)"）。
var effortTiers = map[string]bool{
	EffortLow:    true,
	EffortMedium: true,
	EffortHigh:   true,
	EffortXHigh:  true,
	EffortMax:    true,
}

// NormalizeEffort 归一化一个档位字符串：去掉首尾空白，命中 CLI 白名单则
// 原样返回，否则返回 ""。
//
// "" 的语义是「不下发 --effort」——由调用方决定是否落到阶段默认（后端
// ResolveEffort）或直接保持 CLI 自身行为（前端 picker 永远发具体档位）。
// 非法档位一律降级为 "" 而不是透传：CLI 遇到未知 --effort 值会直接
// 报错终止整轮 run，一个写错的档位不该让整条流水线停摆。
//
// 注意这里**不做大小写折叠**：CLI 的取值域是全小写的字面量，"HIGH" 不在
// 白名单里，按「未知值」处理（降级为阶段默认），而不是悄悄改写成
// "high"。白名单是安全边界，宁可少认一个也不转发一个 CLI 没保证过的值。
func NormalizeEffort(s string) string {
	v := strings.TrimSpace(s)
	if v == "" {
		return ""
	}
	if !effortTiers[v] {
		return ""
	}
	return v
}

// ValidEffort 报告一个档位字符串是否在 CLI 的白名单里。
func ValidEffort(s string) bool {
	return NormalizeEffort(s) != ""
}

// DefaultEffortForStage 返回某个阶段的出厂默认档位。未识别的阶段返回 ""。
func DefaultEffortForStage(stage string) string {
	switch stage {
	case StageDesign:
		return DefaultEffortDesign
	case StageCoding:
		return DefaultEffortCoding
	case StageCommit:
		return DefaultEffortCommit
	case StageOther:
		return DefaultEffortOther
	}
	return ""
}

// EffortProvider supplies the configurable per-stage reasoning-effort
// defaults. Implemented by service.SettingService (the settings table holds
// the KV rows), wired into the gateway via SetEffortProvider.
//
// The provider is deliberately a separate interface from ClaudeEnvProvider /
// LLMConfigProvider: it is injected with a setter rather than a constructor
// argument so that llm.New's signature — which a dozen tests call as
// llm.New(nil, nil) — stays byte-identical.
//
// ok=false means "the provider has nothing configured / failed to read";
// the caller then falls back to the built-in defaults.
type EffortProvider interface {
	// EffortDefaults returns the master switch plus the per-stage default
	// tiers. ok=false → caller falls back to the built-in defaults.
	EffortDefaults() (enabled bool, design, coding, commit, other string, ok bool)
}

// effortProviderHolder is the nil-safe accessor used by ResolveEffort.
func (g *Gateway) effortProvider() EffortProvider {
	if g == nil {
		return nil
	}
	return g.effortCfg
}

// SetEffortProvider wires the per-stage reasoning-effort defaults into the
// gateway. Passing nil restores the built-in defaults (high / medium / low /
// high) — that is also what every unit test gets, since they never call this.
//
// Why a setter and not a third New() argument: llm.New(nil, nil) appears in
// a dozen test files and changing the signature would ripple through all of
// them for a feature most of them don't exercise.
func (g *Gateway) SetEffortProvider(p EffortProvider) {
	if g == nil {
		return
	}
	g.effortCfg = p
}

// ResolveEffort returns the effective reasoning-effort tier for one run.
//
// Priority (two layers on purpose — a third "per-role default" layer would
// make the precedence chain three-segment and nobody would remember it):
//
//  1. the user's explicit pick for this run (a concrete tier wins)
//  2. the configured per-stage default for the stage
//  3. the built-in per-stage default (high / medium / low / high)
//  4. "" — don't emit --effort at all
//
// The master switch (effort.enabled) short-circuits EVERYTHING to "" —
// including an explicit pick. It is the single escape hatch for a CLI that
// doesn't know the flag, or a third-party gateway that rejects the extra
// parameter, and it has to be unconditional to be usable as one.
func (g *Gateway) ResolveEffort(pick, stage string) string {
	enabled := true
	design, coding, commit, other := "", "", "", ""
	if p := g.effortProvider(); p != nil {
		if en, d, c, cm, o, ok := p.EffortDefaults(); ok {
			enabled, design, coding, commit, other = en, d, c, cm, o
		}
	}
	if !enabled {
		return ""
	}
	if e := NormalizeEffort(pick); e != "" {
		return e
	}
	var configured string
	switch stage {
	case StageDesign:
		configured = design
	case StageCoding:
		configured = coding
	case StageCommit:
		configured = commit
	case StageOther:
		configured = other
	}
	if e := NormalizeEffort(configured); e != "" {
		return e
	}
	return DefaultEffortForStage(stage)
}

// effortSupportCache caches "does this claude binary understand --effort?",
// keyed by the resolved binary path. A single map (not sync.Once) because
// different Gateways may resolve different binaries within one process (the
// CLAUDE_BIN env var differs per test / per agent host).
var effortSupportCache sync.Map // map[string]bool

// effortHelpProbe runs `claude --help` and reports whether the output
// mentions --effort. The second return value is the probe's own error (nil
// when the probe ran to completion).
//
// It is a package-level var rather than an inline call so the unit tests can
// substitute a fake and pin BOTH branches of the degradation policy — the
// "old CLI doesn't know the flag" case and the "probe itself failed" case —
// without needing an actual claude binary that lacks the flag.
var effortHelpProbe = func(bin string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "--help").CombinedOutput()
	if err != nil {
		return false, err
	}
	return strings.Contains(string(out), "--effort"), nil
}

// cliSupportsEffort reports whether the resolved claude CLI accepts the
// --effort flag.
//
// Why this probe exists: --effort is a fairly recent CLI flag. An older
// binary aborts the whole run with "error: unknown option '--effort'" — the
// entire coding job dies because of a reasoning-depth knob. So we ask the
// binary once (claude --help) and simply don't emit the flag when it isn't
// there. The probe result is cached per binary path for the process lifetime.
//
// Failure policy: if the probe itself fails (binary missing, --help times
// out), we FAIL OPEN — assume the flag is supported. Rationale: a probe
// failure almost always means the CLI is unusable anyway, and the run will
// fail with a far more accurate error ("claude: command not found") than a
// silently dropped feature would suggest. The user-facing escape hatch for
// "my CLI/gateway doesn't like --effort" is the effort.enabled master switch
// in the settings page.
func (g *Gateway) cliSupportsEffort() bool {
	if g == nil {
		return false
	}
	bin := g.resolveBin()
	if v, ok := effortSupportCache.Load(bin); ok {
		return v.(bool)
	}
	supported, err := effortHelpProbe(bin)
	if err != nil {
		// Probe failed → fail open (see docstring).
		supported = true
	}
	effortSupportCache.Store(bin, supported)
	return supported
}
