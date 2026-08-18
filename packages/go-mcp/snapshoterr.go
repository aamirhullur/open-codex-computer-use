package gomcp

// Snapshot tool-level error taxonomy. These are ordinary tools/call results with
// isError true and a structuredContent.error envelope, not JSON-RPC protocol
// errors. M2 defines the codes and serialization; M4 wires them into the action
// transaction.

// Snapshot error codes carried in structuredContent.error.code.
const (
	ErrSnapshotRefMissing             = "snapshot_ref_missing"
	ErrSnapshotRefMalformed           = "snapshot_ref_malformed"
	ErrSnapshotRefUnknown             = "snapshot_ref_unknown"
	ErrSnapshotRefExpired             = "snapshot_ref_expired"
	ErrSnapshotRefStale               = "snapshot_ref_stale"
	ErrSnapshotRefInUse               = "snapshot_ref_in_use"
	ErrSnapshotTargetChanged          = "snapshot_target_changed"
	ErrSnapshotActionOutcomeUncertain = "snapshot_action_outcome_uncertain"
)

// Retry classes carried in structuredContent.error.retry. same_handle means the
// caller may retry with the same snapshot_ref; new_state means the caller must
// call get_app_state to obtain a fresh one.
const (
	RetrySameHandle = "same_handle"
	RetryNewState   = "new_state"
)

// canonicalRetry maps each code to its default retry class. Only an already
// in-flight handle is retryable with the same reference; every other failure
// requires recapturing state.
var canonicalRetry = map[string]string{
	ErrSnapshotRefMissing:             RetryNewState,
	ErrSnapshotRefMalformed:           RetryNewState,
	ErrSnapshotRefUnknown:             RetryNewState,
	ErrSnapshotRefExpired:             RetryNewState,
	ErrSnapshotRefStale:               RetryNewState,
	ErrSnapshotRefInUse:               RetrySameHandle,
	ErrSnapshotTargetChanged:          RetryNewState,
	ErrSnapshotActionOutcomeUncertain: RetryNewState,
}

// CanonicalRetry returns the default retry class for a snapshot error code, or
// the empty string for an unknown code.
func CanonicalRetry(code string) string { return canonicalRetry[code] }

// SnapshotError is a tool-level snapshot failure. Retry is the class the model
// should follow; when left empty, Result fills it from the code's canonical
// mapping.
type SnapshotError struct {
	Code    string
	Message string
	Retry   string
}

// NewSnapshotError builds a SnapshotError with the canonical retry class for the
// given code.
func NewSnapshotError(code, message string) SnapshotError {
	return SnapshotError{Code: code, Message: message, Retry: canonicalRetry[code]}
}

// Result serializes the error to an isError tool result carrying the visible
// message text and a structuredContent.error envelope. The map shape matches the
// toolCallResult wire form the apps produce, so it decorates like any other
// modern tool result.
func (e SnapshotError) Result() map[string]any {
	retry := e.Retry
	if retry == "" {
		retry = canonicalRetry[e.Code]
	}
	// An unknown code has no canonical retry; never let an empty retry reach the
	// wire. new_state is the safe default (recapture before retrying).
	if retry == "" {
		retry = RetryNewState
	}
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": e.Message}},
		"isError": true,
		"structuredContent": map[string]any{
			"error": map[string]any{
				"code":    e.Code,
				"message": e.Message,
				"retry":   retry,
			},
		},
	}
}
