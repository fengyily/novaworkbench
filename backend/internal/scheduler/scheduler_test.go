package scheduler

import (
	"context"
	"testing"

	"github.com/novaworkbench/backend/internal/model"
)

// capturingExec records the ctx the scheduler hands to the Executor and
// captures the value the Executor reads back from it. It pins down the
// invariant that the ctx value placed by Scheduler.dispatch is readable
// by the same key/value types — which fails when the producer and
// consumer declare unexported duplicates in two different packages
// (the root cause of "scheduler ctx not provided; Executor must be
// invoked through Scheduler.Dispatch").
type capturingExec struct {
	gotCtx     context.Context
	called     bool
	design     bool
	coding     bool
	designCode bool
	wiki       bool
	wikiParams WikiParams
}

func (c *capturingExec) RunScheduledDesign(ctx context.Context, _ DesignParams) (string, error) {
	c.gotCtx, c.called, c.design = ctx, true, true
	// Mirror the production path: pull schedID out of ctx the same way
	// ScheduledExecutor.dispatchFromCtx does.
	if v, ok := ctx.Value(SchedCtxKey{}).(SchedCtxValue); ok {
		return v.JobID, nil
	}
	return "", nil
}

func (c *capturingExec) RunScheduledCoding(ctx context.Context, _ CodingParams) (string, error) {
	c.gotCtx, c.called, c.coding = ctx, true, true
	if v, ok := ctx.Value(SchedCtxKey{}).(SchedCtxValue); ok {
		return v.JobID, nil
	}
	return "", nil
}

func (c *capturingExec) RunScheduledDesignAndCoding(ctx context.Context, _ DesignCodingParams) (string, error) {
	c.gotCtx, c.called, c.designCode = ctx, true, true
	if v, ok := ctx.Value(SchedCtxKey{}).(SchedCtxValue); ok {
		return v.JobID, nil
	}
	return "", nil
}

func (c *capturingExec) RunScheduledWiki(ctx context.Context, p WikiParams) (string, error) {
	c.gotCtx, c.called, c.wiki, c.wikiParams = ctx, true, true, p
	if v, ok := ctx.Value(SchedCtxKey{}).(SchedCtxValue); ok {
		return v.JobID, nil
	}
	return "", nil
}

// TestSchedCtxKeyRoundTrip guards against re-introducing the bug:
// SchedCtxKey / SchedCtxValue must be the same Go type on the producer
// side (Scheduler.dispatch) and the consumer side (Executor). Before the
// fix these were unexported duplicates in two packages — context.Value
// lookups silently failed and every scheduled task fell through to the
// "scheduler ctx not provided" error.
func TestSchedCtxKeyRoundTrip(t *testing.T) {
	e := &capturingExec{}

	// Build a ctx the same way Scheduler.dispatch does.
	ctx := context.WithValue(context.Background(), SchedCtxKey{}, SchedCtxValue{
		JobID:   "",
		SchedID: "sched_x",
	})

	if _, err := e.RunScheduledDesign(ctx, DesignParams{RequirementID: "req_x"}); err != nil {
		t.Fatalf("RunScheduledDesign returned err: %v", err)
	}
	if !e.called || !e.design {
		t.Fatalf("exec not invoked: called=%v design=%v", e.called, e.design)
	}

	v, ok := e.gotCtx.Value(SchedCtxKey{}).(SchedCtxValue)
	if !ok {
		t.Fatalf("ctx lookup failed (the bug we are guarding against): ok=%v", ok)
	}
	if v.SchedID != "sched_x" {
		t.Fatalf("SchedID round-trip mismatch: got %q want %q", v.SchedID, "sched_x")
	}
}

// TestSchedCtxKeyRoundTripCoding is the symmetric guard for the coding
// path. Both branches of Scheduler.dispatch must inject the value, and
// both must be readable from the same key.
func TestSchedCtxKeyRoundTripCoding(t *testing.T) {
	e := &capturingExec{}
	ctx := context.WithValue(context.Background(), SchedCtxKey{}, SchedCtxValue{
		JobID:   "",
		SchedID: "sched_y",
	})
	if _, err := e.RunScheduledCoding(ctx, CodingParams{RequirementID: "req_y"}); err != nil {
		t.Fatalf("RunScheduledCoding returned err: %v", err)
	}
	v, ok := e.gotCtx.Value(SchedCtxKey{}).(SchedCtxValue)
	if !ok || v.SchedID != "sched_y" {
		t.Fatalf("ctx lookup failed: ok=%v v=%+v", ok, v)
	}
}

// TestDispatchRoutesWikiRow pins that a scheduled_tasks row with
// task_type="wiki" reaches Executor.RunScheduledWiki (and NOT
// RunScheduledDesign, which would write design_docs on a row whose detail
// page only ever renders wiki_docs). Also asserts the design-stage columns
// — model / agent_server_id / sync_mode — map onto WikiParams, since a
// wiki row has exactly one stage and reuses them.
//
// svc is left nil: the success path of dispatch never touches it (only the
// error branch calls Finish), and capturingExec always succeeds.
func TestDispatchRoutesWikiRow(t *testing.T) {
	e := &capturingExec{}
	s := &Scheduler{exec: e}

	s.dispatch(model.ScheduledTask{
		ID:            "sched_wiki",
		TaskType:      model.SchedTypeWiki,
		RequirementID: "req_wiki",
		Model:         "claude-opus-5",
		ReadKnowledge: true,
		AgentServerID: "agent_1",
		SyncMode:      "local",
	})

	if !e.wiki {
		t.Fatalf("wiki row did not reach RunScheduledWiki (design=%v coding=%v designCode=%v)", e.design, e.coding, e.designCode)
	}
	if e.design || e.coding || e.designCode {
		t.Fatalf("wiki row leaked into another executor path: %+v", e)
	}
	want := WikiParams{
		RequirementID: "req_wiki",
		Model:         "claude-opus-5",
		ReadKnowledge: true,
		AgentServerID: "agent_1",
		SyncMode:      "local",
	}
	if e.wikiParams != want {
		t.Fatalf("WikiParams: got %+v, want %+v", e.wikiParams, want)
	}
}
