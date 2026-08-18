package main

import (
	"encoding/base64"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/iFurySt/open-codex-computer-use/packages/gomcp"
)

// actionRunner is a configurable fake runner. It answers get_app_state/list_apps
// with a canned snapshot and lets each test dictate the action outcome, counting
// only action-tool invocations so tests can assert zero-dispatch and
// single-dispatch guarantees.
type actionRunner struct {
	mu          sync.Mutex
	snap        *appSnapshot
	actionCalls int
	err         error
	resp        *linuxResponse
	block       chan struct{}
}

func (r *actionRunner) run(req linuxRequest) (*linuxResponse, error) {
	if req.Tool == "get_app_state" || req.Tool == "list_apps" {
		return &linuxResponse{OK: true, Snapshot: r.snap}, nil
	}
	r.mu.Lock()
	r.actionCalls++
	r.mu.Unlock()
	if r.block != nil {
		<-r.block
	}
	if r.err != nil {
		return nil, r.err
	}
	if r.resp != nil {
		return r.resp, nil
	}
	return &linuxResponse{OK: true, Snapshot: r.snap}, nil
}

func (r *actionRunner) calls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.actionCalls
}

func newActionService() (*service, *actionRunner) {
	svc := newService()
	fr := &actionRunner{snap: cannedSnapshot()}
	svc.runner = fr.run
	return svc, fr
}

func errorCode(r toolCallResult) string {
	if r.StructuredContent == nil {
		return ""
	}
	envelope, _ := r.StructuredContent["error"].(map[string]any)
	code, _ := envelope["code"].(string)
	return code
}

func errorRetry(r toolCallResult) string {
	envelope, _ := r.StructuredContent["error"].(map[string]any)
	retry, _ := envelope["retry"].(string)
	return retry
}

func mintHandle(t *testing.T, svc *service) string {
	t.Helper()
	res := svc.callTool("get_app_state", map[string]any{"app": "Text Editor"}, true)
	ref, ok := res.StructuredContent["snapshot_ref"].(string)
	if !ok || !gomcp.ValidHandleFormat(ref) {
		t.Fatalf("get_app_state did not mint a handle: %+v", res)
	}
	return ref
}

// allActionTools lists the seven modern action tools with a minimal arg set.
func allActionTools() map[string]map[string]any {
	return map[string]map[string]any{
		"click":                    {"app": "Text Editor", "element_index": 0},
		"perform_secondary_action": {"app": "Text Editor", "element_index": 0, "action": "activate"},
		"scroll":                   {"app": "Text Editor", "element_index": 0, "direction": "down"},
		"drag":                     {"app": "Text Editor", "from_x": 1, "from_y": 1, "to_x": 2, "to_y": 2},
		"type_text":                {"app": "Text Editor", "text": "hi"},
		"press_key":                {"app": "Text Editor", "key": "Return"},
		"set_value":                {"app": "Text Editor", "element_index": 0, "value": "x"},
	}
}

// TestModernActionsRejectMissingRefBeforeSideEffects checks that all seven action
// tools reject a missing snapshot_ref before any runtime invocation.
func TestModernActionsRejectMissingRefBeforeSideEffects(t *testing.T) {
	for tool, args := range allActionTools() {
		t.Run(tool, func(t *testing.T) {
			svc, fr := newActionService()
			res := svc.callTool(tool, args, true)
			if !res.IsError || errorCode(res) != gomcp.ErrSnapshotRefMissing {
				t.Fatalf("%s missing-ref result = %+v", tool, res)
			}
			if res.Content[0].Text != gomcp.MsgSnapshotRefMissing {
				t.Fatalf("%s missing-ref message = %q", tool, res.Content[0].Text)
			}
			if errorRetry(res) != gomcp.RetryNewState {
				t.Fatalf("%s missing-ref retry = %q", tool, errorRetry(res))
			}
			if fr.calls() != 0 {
				t.Fatalf("%s invoked the runtime %d times before a side effect", tool, fr.calls())
			}
			if svc.store.Counters().Minted != 0 {
				t.Fatalf("%s minted a handle on a missing ref", tool)
			}
		})
	}
}

func TestModernActionMalformedRef(t *testing.T) {
	svc, fr := newActionService()
	res := svc.callTool("click", map[string]any{"app": "Text Editor", "element_index": 0, "snapshot_ref": "not-a-handle"}, true)
	if !res.IsError || errorCode(res) != gomcp.ErrSnapshotRefMalformed {
		t.Fatalf("malformed result = %+v", res)
	}
	if res.Content[0].Text != gomcp.MsgSnapshotRefMalformed {
		t.Fatalf("malformed message = %q", res.Content[0].Text)
	}
	if fr.calls() != 0 {
		t.Fatalf("malformed ref dispatched %d times", fr.calls())
	}
}

func TestModernActionUnknownRef(t *testing.T) {
	svc, fr := newActionService()
	token := make([]byte, gomcp.TokenBytes)
	token[0] = 0x5a
	unminted := gomcp.HandlePrefix + base64.RawURLEncoding.EncodeToString(token)
	res := svc.callTool("click", map[string]any{"app": "Text Editor", "element_index": 0, "snapshot_ref": unminted}, true)
	if errorCode(res) != gomcp.ErrSnapshotRefUnknown {
		t.Fatalf("unknown result = %+v", res)
	}
	if fr.calls() != 0 {
		t.Fatalf("unknown ref dispatched %d times", fr.calls())
	}
}

func TestModernActionExpiredRef(t *testing.T) {
	svc, fr := newActionService()
	now := time.Now()
	svc.store = gomcp.NewSnapshotStore[*appSnapshot](func() time.Time { return now }, nil, nil)
	ref := mintHandle(t, svc)
	now = now.Add(gomcp.DefaultTTL + time.Second)

	res := svc.callTool("click", map[string]any{"app": "Text Editor", "element_index": 0, "snapshot_ref": ref}, true)
	if errorCode(res) != gomcp.ErrSnapshotRefExpired {
		t.Fatalf("expired result = %+v", res)
	}
	if fr.calls() != 0 {
		t.Fatalf("expired ref dispatched %d times", fr.calls())
	}
}

func TestModernActionStaleRef(t *testing.T) {
	svc, fr := newActionService()
	first := mintHandle(t, svc)
	// Re-capturing the same target supersedes the first handle.
	_ = mintHandle(t, svc)

	res := svc.callTool("click", map[string]any{"app": "Text Editor", "element_index": 0, "snapshot_ref": first}, true)
	if errorCode(res) != gomcp.ErrSnapshotRefStale {
		t.Fatalf("stale result = %+v", res)
	}
	if fr.calls() != 0 {
		t.Fatalf("stale ref dispatched %d times", fr.calls())
	}
}

// TestModernActionIdentityMismatch verifies a valid handle whose requested app
// resolves to a different identity fails as snapshot_target_changed with no
// dispatch.
func TestModernActionIdentityMismatch(t *testing.T) {
	svc, fr := newActionService()
	ref := mintHandle(t, svc)

	res := svc.callTool("click", map[string]any{"app": "Totally Different App", "element_index": 0, "snapshot_ref": ref}, true)
	if errorCode(res) != gomcp.ErrSnapshotTargetChanged {
		t.Fatalf("identity mismatch result = %+v", res)
	}
	if errorRetry(res) != gomcp.RetryNewState {
		t.Fatalf("identity mismatch retry = %q", errorRetry(res))
	}
	if fr.calls() != 0 {
		t.Fatalf("identity mismatch dispatched %d times", fr.calls())
	}
	if svc.store.Counters().Mismatched != 1 {
		t.Fatalf("mismatched counter = %d, want 1", svc.store.Counters().Mismatched)
	}
	// The handle was restored to live: a matching-app retry succeeds.
	retry := svc.callTool("click", map[string]any{"app": "Text Editor", "element_index": 0, "snapshot_ref": ref}, true)
	if retry.IsError {
		t.Fatalf("retry after identity-mismatch restore errored: %+v", retry)
	}
}

// TestModernActionRuntimeElementMismatch verifies the runtime element_mismatch
// marker maps to snapshot_target_changed after a single dispatch.
func TestModernActionRuntimeElementMismatch(t *testing.T) {
	svc, fr := newActionService()
	fr.resp = &linuxResponse{OK: false, ErrorKind: gomcp.ErrorKindElementMismatch, Error: "resolved element no longer matches"}
	ref := mintHandle(t, svc)

	res := svc.callTool("click", map[string]any{"app": "Text Editor", "element_index": 0, "snapshot_ref": ref}, true)
	if errorCode(res) != gomcp.ErrSnapshotTargetChanged {
		t.Fatalf("element mismatch result = %+v", res)
	}
	if res.Content[0].Text != gomcp.MsgSnapshotTargetChangedElement {
		t.Fatalf("element mismatch message = %q", res.Content[0].Text)
	}
	if fr.calls() != 1 {
		t.Fatalf("element mismatch dispatched %d times, want 1", fr.calls())
	}
}

// TestModernActionUncertain verifies a runner transport error mid-dispatch
// supersedes the handle and reports an uncertain outcome.
func TestModernActionUncertain(t *testing.T) {
	svc, fr := newActionService()
	fr.err = errors.New("runtime crashed mid-dispatch")
	ref := mintHandle(t, svc)

	res := svc.callTool("type_text", map[string]any{"app": "Text Editor", "text": "hi", "snapshot_ref": ref}, true)
	if errorCode(res) != gomcp.ErrSnapshotActionOutcomeUncertain {
		t.Fatalf("uncertain result = %+v", res)
	}
	if res.Content[0].Text != gomcp.MsgSnapshotActionOutcomeUncertain {
		t.Fatalf("uncertain message = %q", res.Content[0].Text)
	}
	if svc.store.Counters().Uncertain != 1 {
		t.Fatalf("uncertain counter = %d, want 1", svc.store.Counters().Uncertain)
	}
	// The handle is superseded: reuse is stale.
	if _, err := svc.store.Resolve(ref); err == nil {
		t.Fatal("handle should be superseded after uncertain dispatch")
	}
}

// TestModernActionRefreshFailure verifies dispatch-ok-but-recapture-failed
// supersedes the handle and returns the adjudicated refresh-failed message.
func TestModernActionRefreshFailure(t *testing.T) {
	svc, fr := newActionService()
	fr.resp = &linuxResponse{OK: true, Snapshot: nil}
	ref := mintHandle(t, svc)

	res := svc.callTool("type_text", map[string]any{"app": "Text Editor", "text": "hi", "snapshot_ref": ref}, true)
	if errorCode(res) != gomcp.ErrSnapshotActionOutcomeUncertain {
		t.Fatalf("refresh failure result = %+v", res)
	}
	if res.Content[0].Text != gomcp.MsgSnapshotActionRefreshFailed {
		t.Fatalf("refresh failure message = %q", res.Content[0].Text)
	}
	if svc.store.Counters().RefreshFailed != 1 {
		t.Fatalf("refresh_failed counter = %d, want 1", svc.store.Counters().RefreshFailed)
	}
}

// TestModernActionPreDispatchFailureRestoresLive verifies an out-of-bounds
// coordinate click fails pre-dispatch (no runtime invocation), restores the
// handle to live, and a corrected retry with the same handle succeeds.
func TestModernActionPreDispatchFailureRestoresLive(t *testing.T) {
	svc, fr := newActionService()
	ref := mintHandle(t, svc)

	// Canned screenshot is 2400x1600; bounds are half-open, so a point on the
	// width/height edge is out of bounds. Each rejection restores the handle to
	// live, so the same ref is reused across attempts.
	for _, p := range []struct {
		x, y float64
	}{
		{99999, 99999}, // far outside
		{2400, 100},    // x == width rejected (half-open)
		{100, 1600},    // y == height rejected (half-open)
	} {
		bad := svc.callTool("click", map[string]any{"app": "Text Editor", "x": p.x, "y": p.y, "snapshot_ref": ref}, true)
		if !bad.IsError {
			t.Fatalf("out-of-bounds click (%v,%v) should error: %+v", p.x, p.y, bad)
		}
		if bad.Content[0].Text != gomcp.MsgCoordinatesOutOfBounds {
			t.Fatalf("out-of-bounds message = %q, want pinned", bad.Content[0].Text)
		}
		if bad.StructuredContent != nil {
			t.Fatalf("out-of-bounds carried structuredContent: %+v", bad.StructuredContent)
		}
	}
	if fr.calls() != 0 {
		t.Fatalf("pre-dispatch failures invoked the runtime %d times", fr.calls())
	}

	// A point one pixel inside each edge is valid.
	good := svc.callTool("click", map[string]any{"app": "Text Editor", "x": 2399, "y": 1599, "snapshot_ref": ref}, true)
	if good.IsError {
		t.Fatalf("in-bounds retry after restore errored: %+v", good)
	}
	if fr.calls() != 1 {
		t.Fatalf("retry dispatched %d times, want 1", fr.calls())
	}
	if _, ok := good.StructuredContent["snapshot_ref"].(string); !ok {
		t.Fatalf("successful retry missing successor snapshot_ref: %+v", good.StructuredContent)
	}
}

// TestModernActionRejectedBeforeInput verifies a runtime rejected_before_input
// marker restores the handle to live (plain isError, no supersede), dispatches
// exactly once, and a same-handle retry then works.
func TestModernActionRejectedBeforeInput(t *testing.T) {
	svc, fr := newActionService()
	fr.resp = &linuxResponse{OK: false, ErrorKind: gomcp.ErrorKindRejectedBeforeInput, Error: "unknown element_index"}
	ref := mintHandle(t, svc)

	res := svc.callTool("set_value", map[string]any{"app": "Text Editor", "element_index": 0, "value": "x", "snapshot_ref": ref}, true)
	if !res.IsError {
		t.Fatalf("rejected_before_input should be an error: %+v", res)
	}
	if res.StructuredContent != nil {
		t.Fatalf("rejected_before_input carried structuredContent: %+v", res.StructuredContent)
	}
	if res.Content[0].Text != "unknown element_index" {
		t.Fatalf("rejected_before_input text = %q, want the runtime error verbatim", res.Content[0].Text)
	}
	if fr.calls() != 1 {
		t.Fatalf("rejected_before_input dispatched %d times, want 1", fr.calls())
	}
	if svc.store.Counters().Uncertain != 0 {
		t.Fatalf("rejected_before_input bumped Uncertain: %d", svc.store.Counters().Uncertain)
	}
	// Handle stays live: it resolves and a retry with the same ref succeeds.
	if _, err := svc.store.Resolve(ref); err != nil {
		t.Fatalf("handle should stay live after rejected_before_input: %v", err)
	}
	fr.resp = nil // the corrected retry succeeds
	retry := svc.callTool("set_value", map[string]any{"app": "Text Editor", "element_index": 0, "value": "x", "snapshot_ref": ref}, true)
	if retry.IsError {
		t.Fatalf("retry after rejected_before_input errored: %+v", retry)
	}
	if fr.calls() != 2 {
		t.Fatalf("rejected + retry dispatched %d times, want 2", fr.calls())
	}
}

// TestModernActionWhitespaceRefIsMissing verifies a whitespace-only snapshot_ref
// is treated as missing, before any side effect.
func TestModernActionWhitespaceRefIsMissing(t *testing.T) {
	svc, fr := newActionService()
	res := svc.callTool("click", map[string]any{"app": "Text Editor", "element_index": 0, "snapshot_ref": "   "}, true)
	if errorCode(res) != gomcp.ErrSnapshotRefMissing {
		t.Fatalf("whitespace ref result = %+v, want missing", res)
	}
	if fr.calls() != 0 {
		t.Fatalf("whitespace ref dispatched %d times", fr.calls())
	}
}

// TestModernActionSuccessChain verifies a successful action returns a usable
// successor and supersedes the old handle (which then resolves stale), and that
// the successor can drive the next action.
func TestModernActionSuccessChain(t *testing.T) {
	svc, fr := newActionService()
	ref1 := mintHandle(t, svc)

	res := svc.callTool("click", map[string]any{"app": "Text Editor", "element_index": 0, "snapshot_ref": ref1}, true)
	if res.IsError {
		t.Fatalf("action errored: %+v", res)
	}
	ref2, ok := res.StructuredContent["snapshot_ref"].(string)
	if !ok || ref2 == ref1 {
		t.Fatalf("successor snapshot_ref = %q (ref1 %q)", ref2, ref1)
	}
	if res.StructuredContent["generation"] != 2 {
		t.Fatalf("successor generation = %v, want 2", res.StructuredContent["generation"])
	}
	if res.Content[0].Type != "text" || res.Content[0].Text[:13] != "snapshot_ref:" {
		t.Fatalf("successor text missing snapshot_ref line: %q", res.Content[0].Text)
	}

	// Old handle is stale.
	old := svc.callTool("click", map[string]any{"app": "Text Editor", "element_index": 0, "snapshot_ref": ref1}, true)
	if errorCode(old) != gomcp.ErrSnapshotRefStale {
		t.Fatalf("old handle after success = %+v, want stale", old)
	}

	// Successor drives the next action.
	next := svc.callTool("click", map[string]any{"app": "Text Editor", "element_index": 0, "snapshot_ref": ref2}, true)
	if next.IsError {
		t.Fatalf("successor action errored: %+v", next)
	}
	if fr.calls() != 2 {
		t.Fatalf("success chain dispatched %d times, want 2", fr.calls())
	}
}

// TestModernActionConcurrentDoubleUse holds the winning action in the runtime
// while a second use of the same handle races it: exactly one dispatch happens
// and the loser gets snapshot_ref_in_use.
func TestModernActionConcurrentDoubleUse(t *testing.T) {
	svc, fr := newActionService()
	ref := mintHandle(t, svc)

	block := make(chan struct{})
	fr.block = block
	done := make(chan toolCallResult, 1)
	go func() {
		done <- svc.callTool("click", map[string]any{"app": "Text Editor", "element_index": 0, "snapshot_ref": ref}, true)
	}()

	// Wait until the winner is inside the runtime (handle in_flight).
	deadline := time.Now().Add(2 * time.Second)
	for fr.calls() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("winner never entered the runtime")
		}
		time.Sleep(time.Millisecond)
	}

	loser := svc.callTool("click", map[string]any{"app": "Text Editor", "element_index": 0, "snapshot_ref": ref}, true)
	if errorCode(loser) != gomcp.ErrSnapshotRefInUse {
		t.Fatalf("loser result = %+v, want in_use", loser)
	}
	if errorRetry(loser) != gomcp.RetrySameHandle {
		t.Fatalf("in_use retry = %q, want same_handle", errorRetry(loser))
	}

	close(block)
	winner := <-done
	if winner.IsError {
		t.Fatalf("winner errored: %+v", winner)
	}
	if fr.calls() != 1 {
		t.Fatalf("concurrent double-use dispatched %d times, want exactly 1", fr.calls())
	}
	if svc.store.Counters().Concurrent != 1 {
		t.Fatalf("concurrent counter = %d, want 1", svc.store.Counters().Concurrent)
	}
}

// TestLegacyActionZeroStoreInteraction verifies the legacy era action path does
// not touch the store and returns the legacy result shape.
func TestLegacyActionZeroStoreInteraction(t *testing.T) {
	svc, fr := newActionService()

	// Legacy get_app_state populates the implicit cache without minting.
	if res := svc.callTool("get_app_state", map[string]any{"app": "Text Editor"}, false); res.IsError {
		t.Fatalf("legacy get_app_state errored: %+v", res)
	}
	res := svc.callTool("click", map[string]any{"app": "Text Editor", "element_index": 0}, false)
	if res.IsError {
		t.Fatalf("legacy click errored: %+v", res)
	}
	if res.StructuredContent != nil {
		t.Fatalf("legacy click carried structuredContent: %+v", res.StructuredContent)
	}
	if svc.store.Counters().Minted != 0 || svc.store.Counters().Resolved != 0 {
		t.Fatalf("legacy path touched the store: %+v", svc.store.Counters())
	}
	if fr.calls() != 1 {
		t.Fatalf("legacy click dispatched %d times, want 1", fr.calls())
	}
	// The legacy request carries no modern expectation fields.
	if res.Content[0].Type != "text" || res.Content[0].Text[:13] == "snapshot_ref:" {
		t.Fatalf("legacy click leaked a snapshot_ref line: %q", res.Content[0].Text)
	}
}
