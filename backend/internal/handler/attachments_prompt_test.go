package handler

import (
	"errors"
	"strings"
	"testing"

	"github.com/novaworkbench/backend/internal/model"
	"github.com/novaworkbench/backend/internal/service"
)

// withStubbedAttachmentLookup swaps the global attachment-lookup hook
// rewriteAttachmentMarkers uses for the duration of the test. The svc
// argument is ignored — the stub returns pre-canned fixtures instead.
// Pass nil to restore the production lookup (svc.Get).
func withStubbedAttachmentLookup(t *testing.T, stub func(svc *service.AttachmentService, id string) (*model.Attachment, error)) {
	t.Helper()
	prev := attachLookupForMarker
	attachLookupForMarker = stub
	t.Cleanup(func() { attachLookupForMarker = prev })
}

func newFakeSvc() *service.AttachmentService {
	// Zero-value AttachmentService is enough: rewriteAttachmentMarkers only
	// ever calls through attachLookupForMarker, which the test stub ignores.
	// Production wiring goes through service.NewAttachmentService(db).
	return &service.AttachmentService{}
}

// TestRewriteAttachmentMarkers_HappyPath confirms a single placeholder
// pointing at an attachment owned by the current requirement gets rewritten
// to the on-disk basename under the resolved attach dir, and that the
// Markdown image syntax is preserved.
func TestRewriteAttachmentMarkers_HappyPath(t *testing.T) {
	const (
		reqID       = "req_test_owner"
		attachDir   = "/srv/attach/" + "req_test_owner"
		storedName  = "att_abc123__deadbeef__screenshot.png"
		storedOwner = "req_test_owner"
		attID       = "att_abc123def456"
	)
	withStubbedAttachmentLookup(t, func(_ *service.AttachmentService, id string) (*model.Attachment, error) {
		if id != attID {
			t.Errorf("unexpected lookup id: got %q want %q", id, attID)
		}
		return &model.Attachment{
			ID:         attID,
			OwnerID:    storedOwner,
			StoredPath: "req_test_owner/" + storedName,
		}, nil
	})

	in := "看这张图：\n\n![image](nova://attachment/" + attID + ")\n\n照着改。"
	out := rewriteAttachmentMarkers(newFakeSvc(), reqID, attachDir, in)

	wantPath := attachDir + "/" + storedName
	wantMarker := "![image](" + wantPath + ")"
	if !strings.Contains(out, wantMarker) {
		t.Errorf("output missing the rewritten image marker %q; got:\n%s", wantMarker, out)
	}
	if strings.Contains(out, "nova://attachment/") {
		t.Errorf("output still contains the placeholder scheme; got:\n%s", out)
	}
}

// TestRewriteAttachmentMarkers_UnknownIDKeepsPlaceholder verifies that ids
// the lookup cannot resolve fall through unchanged — the AI is better off
// seeing a useless URI than a non-existent path it would try to Read.
func TestRewriteAttachmentMarkers_UnknownIDKeepsPlaceholder(t *testing.T) {
	withStubbedAttachmentLookup(t, func(_ *service.AttachmentService, _ string) (*model.Attachment, error) {
		return nil, errors.New("row not found")
	})

	in := "![image](nova://attachment/att_does_not_exist)"
	out := rewriteAttachmentMarkers(newFakeSvc(), "req_x", "/srv/attach/req_x", in)
	if out != in {
		t.Errorf("placeholder should be unchanged on lookup error; got:\n%s", out)
	}
}

// TestRewriteAttachmentMarkers_WrongOwnerKeepsPlaceholder defends against
// a paste-leaked id from another requirement (or an attacker crafting a
// prompt that pulls in a foreign attachment) silently resolving to a path
// on a different owner's disk — that would be a privilege escalation.
func TestRewriteAttachmentMarkers_WrongOwnerKeepsPlaceholder(t *testing.T) {
	withStubbedAttachmentLookup(t, func(_ *service.AttachmentService, id string) (*model.Attachment, error) {
		return &model.Attachment{
			ID:         id,
			OwnerID:    "req_some_other_owner",
			StoredPath: "req_some_other_owner/att_xx__yyyyyy__secret.png",
		}, nil
	})

	in := "![image](nova://attachment/att_attacker_target)"
	out := rewriteAttachmentMarkers(newFakeSvc(), "req_this_owner", "/srv/attach/req_this_owner", in)
	if out != in {
		t.Errorf("cross-owner placeholder should be unchanged; got:\n%s", out)
	}
}

// TestRewriteAttachmentMarkers_NilSvcReturnsInput pins the no-op fallback
// used by callers that haven't wired an attachment service (older tests,
// minimal NewSubTaskRunner configurations). The placeholder must survive
// verbatim.
func TestRewriteAttachmentMarkers_NilSvcReturnsInput(t *testing.T) {
	in := "before ![image](nova://attachment/att_x) after"
	out := rewriteAttachmentMarkers(nil, "req_x", "/srv/attach/req_x", in)
	if out != in {
		t.Errorf("nil svc should return input unchanged; got:\n%s", out)
	}
}

// TestRewriteAttachmentMarkers_CacheDeduplicatesLookups ensures repeated ids
// in the same body hit attachLookupForMarker only once. Without the cache
// a body with the same screenshot referenced twice (header + body summary)
// would issue N identical SQLite queries — small cost today, real cost if
// the prompt grows.
func TestRewriteAttachmentMarkers_CacheDeduplicatesLookups(t *testing.T) {
	var calls int
	withStubbedAttachmentLookup(t, func(_ *service.AttachmentService, id string) (*model.Attachment, error) {
		calls++
		return &model.Attachment{
			ID:         id,
			OwnerID:    "req_x",
			StoredPath: "req_x/att_y__zzzzzzzz__img.png",
		}, nil
	})

	in := strings.Repeat("![image](nova://attachment/att_same) ", 5)
	_ = rewriteAttachmentMarkers(newFakeSvc(), "req_x", "/srv/attach/req_x", in)
	if calls != 1 {
		t.Errorf("expected exactly one lookup for repeated ids; got %d", calls)
	}
}
