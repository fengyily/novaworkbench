package handler

import (
	"context"
	"errors"
	"log"
	"os"
	"time"
)

// Timeout policy for the LOCAL wizard Claude stages (方案设计 / 开发实现 /
// 阶段答疑 / 编排子任务).
//
// Why this file exists
// -------------------
// These stages used to run under a plain 30-minute WALL-CLOCK deadline
// (llm.GenerateCode's "coding jobs floor to 30m" rule, and qaHardTimeout for
// 阶段答疑). A wall-clock deadline is blind to activity: a run that kept
// streaming tool calls for 30 minutes was killed mid-flight at exactly the
// 30-minute mark even though it was making progress — job_8c64ea6b and
// job_9db58f29 (2026-11-13) both died at 耗时=30m0.04s with a tool_call on
// the very same millisecond.
//
// The policy is now two independent deadlines, each with exactly one job:
//
//   - IDLE (activity-based): the deadline SLIDES forward on every stdout
//     event. A busy run is never killed by the clock; a run that goes quiet
//     is killed after stageIdleTimeout of silence. The window starts at
//     t=0, so a stage that never says anything at all is still bounded at
//     the same 30 minutes the old wall-clock deadline gave it.
//   - ABSOLUTE (wall clock): stageMaxTotalTimeout bounds a run that keeps
//     emitting something but never finishes.
//
// Liveness after the first output is additionally policed by the per-stage
// stall watchdog inside runClaudeStream (10m design / 20m coding), which is
// tuned per stage and reaps the whole process group.
//
// The Agent-Server path keeps its own policy (NOVA_AGENT_IDLE_TIMEOUT /
// NOVA_AGENT_MAX_TOTAL_TIMEOUT) — see wizard_remote.go.
const (
	// defaultStageIdleTimeout is the silence window for a local wizard stage.
	// It starts counting at spawn, so a stage that produces no output at all
	// is still bounded at 30 minutes — the same budget the old wall-clock
	// deadline gave it — but a stage that keeps working is never cut off.
	defaultStageIdleTimeout = 30 * time.Minute

	// defaultStageMaxTotalTimeout is the absolute wall-clock cap for a local
	// wizard stage. It only bites on a run that keeps emitting events but
	// never produces a result (an endless tool loop).
	defaultStageMaxTotalTimeout = 8 * time.Hour
)

// errStageIdle marks a stage context cancelled because the run produced no
// output for the whole idle window. It is carried as the context's cancel
// CAUSE so runClaudeStream can tell "our idle window fired" apart from "the
// CLI died" and apart from "the absolute cap fired".
var errStageIdle = errors.New("stage idle timeout")

// stageIdleTimeout is the silence window (activity-based) for local wizard
// stages. Env: NOVA_STAGE_IDLE_TIMEOUT (duration, default 30m).
func stageIdleTimeout() time.Duration {
	return durationFromEnv("NOVA_STAGE_IDLE_TIMEOUT", defaultStageIdleTimeout)
}

// stageMaxTotalTimeout is the absolute wall-clock cap for local wizard
// stages. Env: NOVA_STAGE_MAX_TOTAL_TIMEOUT (duration, default 8h).
func stageMaxTotalTimeout() time.Duration {
	return durationFromEnv("NOVA_STAGE_MAX_TOTAL_TIMEOUT", defaultStageMaxTotalTimeout)
}

// durationFromEnv reads a duration env var, falling back to def when unset or
// unparseable. A non-positive parsed value is ignored (treated as unset) so a
// typo like NOVA_STAGE_IDLE_TIMEOUT=0 cannot silently disable the timeout.
func durationFromEnv(key string, def time.Duration) time.Duration {
	raw := os.Getenv(key)
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		log.Printf("[timeout] ignoring invalid %s=%q, using %v", key, raw, def)
		return def
	}
	return d
}

// stageRun is a local wizard claude stage's lifetime bundle: the context the
// subprocess is bound to, the activity feed that keeps the idle deadline
// sliding, and the cancel function that releases the timers.
type stageRun struct {
	// ctx is the context the claude command was built with. Pass it to
	// runClaudeStream via withCtx so a timeout kill is attributed to us
	// instead of being blamed on the CLI.
	ctx context.Context
	// cancel releases the timers.
	cancel context.CancelFunc
	// hb is the activity feed: the stream reader pushes one value per
	// stdout event and the idle deadline slides forward on each.
	hb chan struct{}
}

// newStageRun builds the context for a local wizard claude stage.
//
// The idle deadline slides on every beat, so "the run is still producing
// output" is what keeps it alive. The absolute cap is the only wall-clock
// deadline and it is deliberately generous.
//
// The returned cancel MUST be deferred by the caller (it releases the timers).
func newStageRun(scope string) *stageRun {
	idle := stageIdleTimeout()
	maxTotal := stageMaxTotalTimeout()

	// WithCancelCause so the idle window can be told apart from every other
	// cancellation reason when the run is attributed after the fact.
	parent, cancelParent := context.WithCancelCause(context.Background())
	beats := make(chan struct{}, 1)

	go func() {
		timer := time.NewTimer(idle)
		defer timer.Stop()
		for {
			select {
			case _, ok := <-beats:
				if !ok {
					return
				}
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(idle)
			case <-timer.C:
				log.Printf("[%s] idle %v without stdout activity; cancelling", scope, idle)
				cancelParent(errStageIdle)
				return
			case <-parent.Done():
				// The caller cancelled the run. Stop watching — the beat
				// channel is owned by the caller and may outlive this
				// stage (the Q&A path retries a stale session in the same
				// job), so we must not rely on it being closed.
				return
			}
		}
	}()

	ctx := parent
	cancelAll := func() { cancelParent(context.Canceled) }
	if maxTotal > 0 {
		absCtx, absCancel := context.WithTimeout(parent, maxTotal)
		ctx = absCtx
		cancelAll = func() {
			absCancel()
			cancelParent(context.Canceled)
		}
	}

	return &stageRun{ctx: ctx, cancel: cancelAll, hb: beats}
}
