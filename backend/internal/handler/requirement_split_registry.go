package handler

import (
	"sync"
	"time"

	"github.com/novaworkbench/backend/internal/prompt"
	"github.com/novaworkbench/backend/internal/store"
)

// 「方案拆分」的进程内状态登记处。
//
// 拆分候选本来就是**内存态**（刷新页面即丢，CLAUDE.md 记载「候选列表是临时的」）。
// 但"状态"必须**可发现**：弹窗关掉再打开，用户要能看到上一轮拆分的进度与候选，
// 而不是被迫重跑一次 AI。
//
// 改造前，拆分的事实散在两个互不相通的容器里：
//
//	splitJobResults —— 按 jobID 存候选（问"这次拆分产出了什么"）
//	splitLocks      —— 按 requirementID 存闸门（问"这个需求能不能跑"）
//
// 两者之间没有任何映射，所以**没有任何一方能回答"这个需求最近一次拆分是哪一个
// job"** —— 那正是恢复现场需要的钥匙。
//
// 现在统一成 splitRuns：一次拆分运行的全部事实放在一处，并且按 requirementID
// 可寻址。splitLocks 退化为纯并发闸门，职责不重叠。

// splitRunsCap — 登记处容量上限。
//
// 正常使用量是"一次拆分一个 run"，32 已经很宽松。上限只是为了兜住"后端长期运行
// 且被反复拆分"这种极端场景，避免 map 无界增长（旧实现只增不删）。
const splitRunsCap = 32

// splitRun 是一次拆分运行的登记记录 —— 唯一的权威事实来源。
type splitRun struct {
	// JobID — store.JobStore 里的 job id（job_xxxxxxxx）。
	JobID string
	// RequirementID — 这轮拆分属于哪个需求。
	RequirementID string
	// Status — running | done | error。
	Status store.JobStatus
	// Error — 用户可见的中文失败原因（失败时非空）。
	Error string
	// Candidates — AI 产出的候选子需求（成功时非空）。
	Candidates []prompt.SplitTask
	// References — AI 以架构师视角判断的「互相参考」建议（成功时可能为空：
	// 模型判断确实没有互相引用的子需求对）。
	References []prompt.SplitReference
	// Model — 这一轮实际生效的模型（persona 默认值或用户覆盖值）。
	Model string
	// AgentServerID — 空 = 本机执行；非空 = 跑在那台 Agent Server 上。
	AgentServerID string
	// Consumed — 候选已被用户确认落库，这轮 run 就"用掉了"。
	Consumed bool
	// CreatedAt / FinishedAt — 生命周期时间戳。
	CreatedAt  time.Time
	FinishedAt time.Time
}

// splitRuns 是进程内的拆分登记处。
type splitRuns struct {
	mu sync.RWMutex
	// byJob 按 jobID 索引全部事实。
	byJob map[string]*splitRun
	// latest 按 requirementID 索引"最近一次拆分是哪一个 job"——
	// 这正是"关掉弹窗再点拆分时能找到上次那轮"的钥匙。
	latest map[string]string
	// order 是 FIFO 淘汰序（jobID 的入场顺序）。
	order []string
	// cap 容量上限。
	cap int
}

// newSplitRuns builds an empty registry with the given capacity.
func newSplitRuns(capacity int) *splitRuns {
	if capacity <= 0 {
		capacity = splitRunsCap
	}
	return &splitRuns{
		byJob:  map[string]*splitRun{},
		latest: map[string]string{},
		cap:    capacity,
	}
}

// start registers a new run.
//
// 调用约定：必须在 splitLocks.acquire **成功之后**才调用 —— 否则一个没抢到锁、
// 根本没跑起来的 job 会把 latest[reqID] 覆盖掉，正在跑的那一轮就从"最近一次拆分"
// 里消失了（用户重开弹窗会看到错误的失败态）。
func (r *splitRuns) start(jobID, reqID, model, agentServerID string) *splitRun {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.byJob == nil {
		r.byJob = map[string]*splitRun{}
	}
	if r.latest == nil {
		r.latest = map[string]string{}
	}
	run := &splitRun{
		JobID:         jobID,
		RequirementID: reqID,
		Status:        store.JobRunning,
		Model:         model,
		AgentServerID: agentServerID,
		CreatedAt:     time.Now(),
	}
	r.byJob[jobID] = run
	r.latest[reqID] = jobID
	r.order = append(r.order, jobID)
	r.evictOldestLocked()
	return run
}

// finish writes the terminal state of a run.
//
// 调用约定：必须在 job.Finish **之前**调用 —— store.Job.Finish 会关闭所有订阅者
// channel，订阅者醒来时读到的必须是已填好的状态。
func (r *splitRuns) finish(jobID string, status store.JobStatus, errMsg string,
	candidates []prompt.SplitTask, references []prompt.SplitReference) {
	r.mu.Lock()
	defer r.mu.Unlock()
	run := r.byJob[jobID]
	if run == nil {
		return
	}
	run.Status = status
	run.Error = errMsg
	if candidates == nil {
		run.Candidates = nil
	} else {
		run.Candidates = make([]prompt.SplitTask, len(candidates))
		copy(run.Candidates, candidates)
	}
	if references == nil {
		run.References = nil
	} else {
		run.References = make([]prompt.SplitReference, len(references))
		copy(run.References, references)
	}
	run.FinishedAt = time.Now()
}

// update mutates a run in place (e.g. to backfill the effective model).
func (r *splitRuns) update(jobID string, fn func(run *splitRun)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if run := r.byJob[jobID]; run != nil && fn != nil {
		fn(run)
	}
}

// get returns a copy of the run, or nil when it is unknown.
func (r *splitRuns) get(jobID string) *splitRun {
	r.mu.RLock()
	defer r.mu.RUnlock()
	run := r.byJob[jobID]
	if run == nil {
		return nil
	}
	out := *run
	if run.Candidates != nil {
		out.Candidates = make([]prompt.SplitTask, len(run.Candidates))
		copy(out.Candidates, run.Candidates)
	}
	if run.References != nil {
		out.References = make([]prompt.SplitReference, len(run.References))
		copy(out.References, run.References)
	}
	return &out
}

// latestFor returns the most recent run of a requirement.
//
// A consumed run (its candidates were already persisted) is reported as absent —
// reopening the modal must land on the config stage, not on a stale candidate
// table the user could mistake for a fresh batch.
func (r *splitRuns) latestFor(reqID string) *splitRun {
	r.mu.RLock()
	jobID := r.latest[reqID]
	r.mu.RUnlock()
	if jobID == "" {
		return nil
	}
	run := r.get(jobID)
	if run == nil || run.Consumed {
		return nil
	}
	return run
}

// consumeLatest marks the most recent run of a requirement as consumed and
// drops its candidates. Called after the candidates were persisted.
func (r *splitRuns) consumeLatest(reqID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	jobID := r.latest[reqID]
	if jobID == "" {
		return
	}
	if run := r.byJob[jobID]; run != nil {
		run.Consumed = true
		run.Candidates = nil
		run.References = nil
	}
}

// evictOldestLocked drops finished runs until the registry is back under its
// capacity.
//
// 只淘汰**已结束**的 run：正在跑的那一轮永远不能被淘汰，否则用户重开弹窗就接不
// 回正在跑的拆分。若扫完全部 run 仍超容（全是 running），则放弃淘汰 —— 宁可短
// 暫超容也不误伤。
func (r *splitRuns) evictOldestLocked() {
	if r.cap <= 0 {
		return
	}
	for len(r.byJob) > r.cap {
		evicted := false
		for i, jobID := range r.order {
			run := r.byJob[jobID]
			if run == nil || run.Status == store.JobRunning {
				continue
			}
			delete(r.byJob, jobID)
			r.order = append(r.order[:i], r.order[i+1:]...)
			for reqID, latest := range r.latest {
				if latest == jobID {
					delete(r.latest, reqID)
				}
			}
			evicted = true
			break
		}
		if !evicted {
			return
		}
	}
}

// ── splitLocks — 幂等锁：requirementID → 正在跑的 job id ────────────────────
//
// 重复点击 / 多标签页并发拆分会产生两批重复子需求，所以同一需求的拆分在任一
// 时刻只允许一个。锁在 job 结束时释放。
//
// 锁只管**并发闸门**；"最近一次拆分是什么"由 splitRuns 回答。两者职责不重叠。

type splitLocks struct {
	mu    sync.Mutex
	byReq map[string]string
}

func newSplitLocks() *splitLocks {
	return &splitLocks{byReq: map[string]string{}}
}

// acquire returns the existing job id when a split is already running for
// this requirement, and "" when the caller may proceed.
func (l *splitLocks) acquire(reqID, jobID string) (existing string, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.byReq == nil {
		l.byReq = map[string]string{}
	}
	if cur, running := l.byReq[reqID]; running {
		return cur, false
	}
	l.byReq[reqID] = jobID
	return "", true
}

func (l *splitLocks) release(reqID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.byReq, reqID)
}

// peek reports the running job id for a requirement without taking the
// lock. It lets the handler bail out BEFORE creating a job, so a refused
// second split doesn't leave a stray job in the ring buffer.
func (l *splitLocks) peek(reqID string) (jobID string, running bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	jobID, running = l.byReq[reqID]
	return jobID, running
}
