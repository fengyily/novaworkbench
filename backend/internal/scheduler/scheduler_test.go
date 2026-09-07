package scheduler

import (
	"context"
	"testing"
)

// capturingExec records the ctx the scheduler hands to the Executor and
// captures the value the Executor reads back from it. It pins down the
// invariant that the ctx value placed by Scheduler.dispatch is readable
// by the same key/value types — which fails when the producer and
// consumer declare unexported duplicates in two different packages
// (the root cause of "scheduler ctx not provided; Executor must be
// invoked through Scheduler.Dispatch").
type capturingExec struct {
	gotCtx  context.Context
	called  bool
	design  bool
	coding  bool
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