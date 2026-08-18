package gomcp

import (
	"encoding/json"
	"testing"
)

func TestSnapshotErrorCanonicalRetry(t *testing.T) {
	cases := map[string]string{
		ErrSnapshotRefMissing:             RetryNewState,
		ErrSnapshotRefMalformed:           RetryNewState,
		ErrSnapshotRefUnknown:             RetryNewState,
		ErrSnapshotRefExpired:             RetryNewState,
		ErrSnapshotRefStale:               RetryNewState,
		ErrSnapshotRefInUse:               RetrySameHandle,
		ErrSnapshotTargetChanged:          RetryNewState,
		ErrSnapshotActionOutcomeUncertain: RetryNewState,
	}
	if len(cases) != 8 {
		t.Fatalf("expected 8 codes, have %d", len(cases))
	}
	for code, want := range cases {
		if got := CanonicalRetry(code); got != want {
			t.Errorf("CanonicalRetry(%q) = %q, want %q", code, got, want)
		}
		if got := NewSnapshotError(code, "m").Retry; got != want {
			t.Errorf("NewSnapshotError(%q).Retry = %q, want %q", code, got, want)
		}
	}
}

func TestSnapshotErrorResultShape(t *testing.T) {
	res := NewSnapshotError(ErrSnapshotRefExpired, "snapshot_ref has expired; call get_app_state").Result()
	if res["isError"] != true {
		t.Fatalf("isError = %v", res["isError"])
	}
	content := res["content"].([]map[string]any)
	if content[0]["type"] != "text" || content[0]["text"] != "snapshot_ref has expired; call get_app_state" {
		t.Fatalf("content = %#v", content)
	}
	sc := res["structuredContent"].(map[string]any)
	errObj := sc["error"].(map[string]any)
	if errObj["code"] != ErrSnapshotRefExpired {
		t.Fatalf("code = %v", errObj["code"])
	}
	if errObj["retry"] != RetryNewState {
		t.Fatalf("retry = %v", errObj["retry"])
	}
	if errObj["message"] != "snapshot_ref has expired; call get_app_state" {
		t.Fatalf("message = %v", errObj["message"])
	}
}

// TestSnapshotErrorUnknownCodeRetryFallback confirms an unknown/empty retry never
// reaches the wire: Result defaults it to new_state.
func TestSnapshotErrorUnknownCodeRetryFallback(t *testing.T) {
	// Unknown code with no canonical retry.
	sc := SnapshotError{Code: "not_a_real_code", Message: "m"}.Result()["structuredContent"].(map[string]any)
	if got := sc["error"].(map[string]any)["retry"]; got != RetryNewState {
		t.Fatalf("unknown-code retry = %v, want %s", got, RetryNewState)
	}
	// Known code with an explicitly-blanked retry still falls back canonically.
	sc = SnapshotError{Code: ErrSnapshotRefInUse, Message: "m", Retry: ""}.Result()["structuredContent"].(map[string]any)
	if got := sc["error"].(map[string]any)["retry"]; got != RetrySameHandle {
		t.Fatalf("blanked known-code retry = %v, want %s", got, RetrySameHandle)
	}
}

func TestSnapshotErrorExplicitRetryOverride(t *testing.T) {
	e := SnapshotError{Code: ErrSnapshotRefUnknown, Message: "m", Retry: RetrySameHandle}
	sc := e.Result()["structuredContent"].(map[string]any)
	if sc["error"].(map[string]any)["retry"] != RetrySameHandle {
		t.Fatalf("explicit retry not honored")
	}
}

// TestSnapshotErrorDecoratesAsModernResult confirms a snapshot error result
// carries structuredContent through modern decoration alongside resultType.
func TestSnapshotErrorDecoratesAsModernResult(t *testing.T) {
	s := NewServer(testHooks())
	resp := s.decorate(1, NewSnapshotError(ErrSnapshotRefMissing, "missing").Result())
	body := resp["result"].(map[string]any)
	if body["resultType"] != "complete" {
		t.Fatalf("resultType = %v", body["resultType"])
	}
	// Round-trip through JSON as the wire would, then read the error envelope.
	raw, _ := json.Marshal(body)
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	sc := out["structuredContent"].(map[string]any)
	if sc["error"].(map[string]any)["code"] != ErrSnapshotRefMissing {
		t.Fatalf("error code lost through decoration: %#v", sc)
	}
	if out["isError"] != true {
		t.Fatalf("isError lost")
	}
}
