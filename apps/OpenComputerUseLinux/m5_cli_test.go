package main

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/iFurySt/open-codex-computer-use/packages/gomcp"
)

// cliTestApp is the fixture app name whose identity the canned snapshot carries.
const cliTestApp = "Text Editor"

// resultOf pulls the toolCallResult out of one CLI batch output entry.
func resultOf(t *testing.T, entry map[string]any) toolCallResult {
	t.Helper()
	result, ok := entry["result"].(toolCallResult)
	if !ok {
		t.Fatalf("output entry missing toolCallResult: %#v", entry)
	}
	return result
}

// legacyBatch replays the pre-threading batch loop (every call modern=false, no
// minting, stop at first error) so a strict byte-identical comparison can pin the
// legacy path.
func legacyBatch(svc *service, calls []callSpec) []map[string]any {
	var outputs []map[string]any
	for _, call := range calls {
		result := svc.callTool(call.Tool, call.Args, false)
		outputs = append(outputs, map[string]any{"tool": call.Tool, "result": result})
		if result.IsError {
			break
		}
	}
	return outputs
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	encoded, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(encoded)
}

// TestCLIBatchExplicitRefDispatchesModern covers contract (a): a call whose args
// carry a snapshot_ref runs the modern action transaction, returning a successor
// handle in structuredContent, even with strict mode off.
func TestCLIBatchExplicitRefDispatchesModern(t *testing.T) {
	svc, fr := newActionService()
	ref := mintHandle(t, svc)
	before := fr.calls()

	calls := []callSpec{{Tool: "click", Args: map[string]any{"app": cliTestApp, "element_index": 0, "snapshot_ref": ref}}}
	outputs, hasError := executeCallSequence(svc, calls, false)

	if hasError {
		t.Fatalf("explicit-ref click errored: %s", mustJSON(t, outputs))
	}
	if got := fr.calls() - before; got != 1 {
		t.Fatalf("action dispatches = %d, want 1", got)
	}
	result := resultOf(t, outputs[0])
	successor, ok := gomcp.SuccessorRef(result.StructuredContent)
	if !ok {
		t.Fatalf("explicit-ref click returned no successor snapshot_ref: %+v", result)
	}
	if successor == ref {
		t.Fatalf("successor %q must differ from the consumed ref", successor)
	}
}

// TestCLIBatchLegacyByteIdentical covers contract (d): a batch with no refs and
// no strict mode mints nothing and is byte-identical to the pre-threading loop.
func TestCLIBatchLegacyByteIdentical(t *testing.T) {
	calls := []callSpec{
		{Tool: "get_app_state", Args: map[string]any{"app": cliTestApp}},
		{Tool: "type_text", Args: map[string]any{"app": cliTestApp, "text": "hi"}},
	}

	svc, _ := newActionService()
	outputs, hasError := executeCallSequence(svc, calls, false)
	if hasError {
		t.Fatalf("legacy batch errored: %s", mustJSON(t, outputs))
	}
	if minted := svc.store.Counters().Minted; minted != 0 {
		t.Fatalf("legacy batch minted %d handles, want 0", minted)
	}

	legacySvc, _ := newActionService()
	want := legacyBatch(legacySvc, calls)
	if got, wantJSON := mustJSON(t, outputs), mustJSON(t, want); got != wantJSON {
		t.Fatalf("legacy batch not byte-identical:\n got=%s\nwant=%s", got, wantJSON)
	}
}

// TestCLIBatchStrictMissingRefErrors covers contract (b): strict mode with an
// action call that has no ref and nothing to auto-thread fails with the pinned
// missing-ref message and performs no runtime dispatch.
func TestCLIBatchStrictMissingRefErrors(t *testing.T) {
	svc, fr := newActionService()

	calls := []callSpec{{Tool: "click", Args: map[string]any{"app": cliTestApp, "element_index": 0}}}
	outputs, hasError := executeCallSequence(svc, calls, true)

	if !hasError {
		t.Fatalf("strict missing-ref click did not error: %s", mustJSON(t, outputs))
	}
	result := resultOf(t, outputs[0])
	if !result.IsError {
		t.Fatalf("strict missing-ref result not isError: %+v", result)
	}
	if code := errorCode(result); code != gomcp.ErrSnapshotRefMissing {
		t.Fatalf("error code = %q, want %q", code, gomcp.ErrSnapshotRefMissing)
	}
	if text := result.Content[0].Text; text != gomcp.MsgSnapshotRefMissing {
		t.Fatalf("message = %q, want pinned %q", text, gomcp.MsgSnapshotRefMissing)
	}
	if fr.calls() != 0 {
		t.Fatalf("strict missing-ref dispatched %d runtime calls, want 0", fr.calls())
	}
}

// TestCLIBatchStrictGetAppStateThreads covers contract (b)+(c): strict mode runs
// get_app_state modern to mint a handle, and a following action call omitting the
// ref is auto-threaded from that mint.
func TestCLIBatchStrictGetAppStateThreads(t *testing.T) {
	svc, fr := newActionService()

	calls := []callSpec{
		{Tool: "get_app_state", Args: map[string]any{"app": cliTestApp}},
		{Tool: "click", Args: map[string]any{"app": cliTestApp, "element_index": 0}},
	}
	outputs, hasError := executeCallSequence(svc, calls, true)

	if hasError {
		t.Fatalf("strict threaded batch errored: %s", mustJSON(t, outputs))
	}
	minted, ok := gomcp.SuccessorRef(resultOf(t, outputs[0]).StructuredContent)
	if !ok {
		t.Fatalf("strict get_app_state minted no handle: %+v", resultOf(t, outputs[0]))
	}
	successor, ok := gomcp.SuccessorRef(resultOf(t, outputs[1]).StructuredContent)
	if !ok {
		t.Fatalf("threaded click returned no successor: %+v", resultOf(t, outputs[1]))
	}
	if successor == minted {
		t.Fatalf("threaded click successor %q must differ from minted handle", successor)
	}
	if fr.calls() != 1 {
		t.Fatalf("action dispatches = %d, want 1", fr.calls())
	}
}

// TestCLIBatchAutoThreadingChain covers contract (c): after one explicit-ref
// modern action, later action calls omitting the ref get the latest successor
// auto-filled, without strict mode and without mutating the caller's args.
func TestCLIBatchAutoThreadingChain(t *testing.T) {
	svc, fr := newActionService()
	ref := mintHandle(t, svc)
	before := fr.calls()

	calls := []callSpec{
		{Tool: "click", Args: map[string]any{"app": cliTestApp, "element_index": 0, "snapshot_ref": ref}},
		{Tool: "type_text", Args: map[string]any{"app": cliTestApp, "text": "hi"}},
		{Tool: "press_key", Args: map[string]any{"app": cliTestApp, "key": "Return"}},
	}
	outputs, hasError := executeCallSequence(svc, calls, false)

	if hasError {
		t.Fatalf("auto-thread chain errored: %s", mustJSON(t, outputs))
	}
	if got := fr.calls() - before; got != 3 {
		t.Fatalf("action dispatches = %d, want 3", got)
	}

	// Each call returns a distinct successor; the chain advances generation.
	seen := map[string]bool{ref: true}
	for index := range calls {
		successor, ok := gomcp.SuccessorRef(resultOf(t, outputs[index]).StructuredContent)
		if !ok {
			t.Fatalf("call %d returned no successor snapshot_ref: %+v", index, resultOf(t, outputs[index]))
		}
		if seen[successor] {
			t.Fatalf("call %d reused an earlier handle %q", index, successor)
		}
		seen[successor] = true
	}

	// Auto-threading must not mutate the caller's argument maps.
	if _, present := calls[1].Args[gomcp.SnapshotRefKey]; present {
		t.Fatalf("auto-threading leaked snapshot_ref into caller args: %+v", calls[1].Args)
	}
	if _, present := calls[2].Args[gomcp.SnapshotRefKey]; present {
		t.Fatalf("auto-threading leaked snapshot_ref into caller args: %+v", calls[2].Args)
	}
}

// TestCLISingleExplicitRefDispatchesModern covers single-call parity for
// contract (a): a lone `call <tool> --args {...snapshot_ref...}` runs the modern
// action transaction and returns a successor handle.
func TestCLISingleExplicitRefDispatchesModern(t *testing.T) {
	svc, fr := newActionService()
	ref := mintHandle(t, svc)
	before := fr.calls()

	argsJSON := fmt.Sprintf(`{"app":%q,"element_index":0,"snapshot_ref":%q}`, cliTestApp, ref)
	output, hasError, err := runCallCommand([]string{"click", "--args", argsJSON}, svc)
	if err != nil {
		t.Fatalf("runCallCommand: %v", err)
	}
	if hasError {
		t.Fatalf("explicit-ref single call errored: %s", mustJSON(t, output))
	}
	if got := fr.calls() - before; got != 1 {
		t.Fatalf("action dispatches = %d, want 1", got)
	}
	result, ok := output.(toolCallResult)
	if !ok {
		t.Fatalf("single-call output is not a toolCallResult: %#v", output)
	}
	successor, ok := gomcp.SuccessorRef(result.StructuredContent)
	if !ok {
		t.Fatalf("explicit-ref single call returned no successor snapshot_ref: %+v", result)
	}
	if successor == ref {
		t.Fatalf("successor %q must differ from the consumed ref", successor)
	}
}

// TestCLISingleStrictMissingRefErrors covers single-call parity for contract (b):
// strict mode with a lone ref-less action call fails with the pinned missing-ref
// message and performs no runtime dispatch.
func TestCLISingleStrictMissingRefErrors(t *testing.T) {
	t.Setenv("OPEN_COMPUTER_USE_STRICT_SNAPSHOTS", "1")
	svc, fr := newActionService()

	argsJSON := fmt.Sprintf(`{"app":%q,"element_index":0}`, cliTestApp)
	output, hasError, err := runCallCommand([]string{"click", "--args", argsJSON}, svc)
	if err != nil {
		t.Fatalf("runCallCommand: %v", err)
	}
	if !hasError {
		t.Fatalf("strict ref-less single call did not error: %s", mustJSON(t, output))
	}
	result, ok := output.(toolCallResult)
	if !ok {
		t.Fatalf("single-call output is not a toolCallResult: %#v", output)
	}
	if code := errorCode(result); code != gomcp.ErrSnapshotRefMissing {
		t.Fatalf("error code = %q, want %q", code, gomcp.ErrSnapshotRefMissing)
	}
	if text := result.Content[0].Text; text != gomcp.MsgSnapshotRefMissing {
		t.Fatalf("message = %q, want pinned %q", text, gomcp.MsgSnapshotRefMissing)
	}
	if fr.calls() != 0 {
		t.Fatalf("strict ref-less single call dispatched %d runtime calls, want 0", fr.calls())
	}
}

// TestCLISingleLegacyByteIdentical covers single-call parity for contract (d): a
// legacy single call (no ref, no env) dispatches modern=false, mints nothing, and
// is byte-identical to the pre-threading direct callTool(..., false). get_app_state
// is used because it needs no prior cached state; a legacy capture must not mint a
// handle nor carry a structuredContent snapshot_ref.
func TestCLISingleLegacyByteIdentical(t *testing.T) {
	argsJSON := fmt.Sprintf(`{"app":%q}`, cliTestApp)

	svc, _ := newActionService()
	output, hasError, err := runCallCommand([]string{"get_app_state", "--args", argsJSON}, svc)
	if err != nil {
		t.Fatalf("runCallCommand: %v", err)
	}
	if hasError {
		t.Fatalf("legacy single call errored: %s", mustJSON(t, output))
	}
	if minted := svc.store.Counters().Minted; minted != 0 {
		t.Fatalf("legacy single call minted %d handles, want 0", minted)
	}
	if result, ok := output.(toolCallResult); ok {
		if _, threaded := gomcp.SuccessorRef(result.StructuredContent); threaded {
			t.Fatalf("legacy single call leaked a snapshot_ref: %+v", result)
		}
	}

	legacySvc, _ := newActionService()
	want := legacySvc.callTool("get_app_state", map[string]any{"app": cliTestApp}, false)
	if got, wantJSON := mustJSON(t, output), mustJSON(t, want); got != wantJSON {
		t.Fatalf("legacy single call not byte-identical:\n got=%s\nwant=%s", got, wantJSON)
	}
}

// TestStrictSnapshotsEnabledEnv checks the env truthy parsing and that
// runCallCommand wires it into the batch path.
func TestStrictSnapshotsEnabledEnv(t *testing.T) {
	for _, value := range []string{"1", "true", "TRUE", "yes", "on", " On "} {
		t.Setenv("OPEN_COMPUTER_USE_STRICT_SNAPSHOTS", value)
		if !strictSnapshotsEnabled() {
			t.Fatalf("strictSnapshotsEnabled() = false for %q, want true", value)
		}
	}
	for _, value := range []string{"", "0", "false", "no", "off", "nope"} {
		t.Setenv("OPEN_COMPUTER_USE_STRICT_SNAPSHOTS", value)
		if strictSnapshotsEnabled() {
			t.Fatalf("strictSnapshotsEnabled() = true for %q, want false", value)
		}
	}

	// runCallCommand reads the env gate: a strict batch with a ref-less action
	// surfaces the pinned missing-ref error and reports hasError.
	t.Setenv("OPEN_COMPUTER_USE_STRICT_SNAPSHOTS", "1")
	svc, _ := newActionService()
	callsJSON := fmt.Sprintf(`[{"tool":"click","args":{"app":%q,"element_index":0}}]`, cliTestApp)
	output, hasError, err := runCallCommand([]string{"--calls", callsJSON}, svc)
	if err != nil {
		t.Fatalf("runCallCommand: %v", err)
	}
	if !hasError {
		t.Fatalf("strict runCallCommand did not report hasError: %s", mustJSON(t, output))
	}
	outputs, _ := output.([]map[string]any)
	if len(outputs) != 1 {
		t.Fatalf("outputs len = %d, want 1", len(outputs))
	}
	if text := resultOf(t, outputs[0]).Content[0].Text; text != gomcp.MsgSnapshotRefMissing {
		t.Fatalf("message = %q, want pinned %q", text, gomcp.MsgSnapshotRefMissing)
	}
}
