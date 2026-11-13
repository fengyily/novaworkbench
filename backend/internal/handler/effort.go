package handler

import (
	"log"

	"github.com/novaworkbench/backend/internal/llm"
	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
)

// resolveEffort turns "what the user picked for this run" + "which pipeline
// stage is this" into the reasoning-effort tier that goes into
// llm.StreamOpts.Effort.
//
// The whole "user pick > configured stage default > built-in default" chain
// lives in llm.Gateway.ResolveEffort — this wrapper only exists so the 30-odd
// run sites read the same and so a nil gateway (unit tests that wire a
// partial handler) degrades to the built-in default instead of panicking.
func resolveEffort(gw *llm.Gateway, pick, stage string) string {
	if gw == nil {
		return llm.DefaultEffortForStage(stage)
	}
	return gw.ResolveEffort(pick, stage)
}

// stampSubTaskEffort resolves the tier a sub-task will run with and records
// it on the row — in memory (so the runner, which reads st.Effort, sees it
// without another round trip) and in the database (so the card shows it and a
// 重做 / 继续 reproduces it).
//
// The stage argument decides the fallback when the user picked nothing: a
// plain implementation child belongs to 「开发实现」, a Review child to
// 「其他任务」, a push/PR child to 「提交推送」.
func stampSubTaskEffort(gw *llm.Gateway, svc *service.SubTaskService, st *model.SubTask, pick, stage string) {
	if st == nil {
		return
	}
	tier := resolveEffort(gw, pick, stage)
	st.Effort = tier
	if svc == nil || st.ID == "" {
		return
	}
	if err := svc.UpdateEffort(st.ID, tier); err != nil {
		log.Printf("[effort] persist tier for sub-task %s: %v", st.ID, err)
	}
}
