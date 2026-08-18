package gomcp

import "errors"

// Snapshot tool-level error taxonomy. These are ordinary tools/call results with
// isError true and a structuredContent.error envelope, not JSON-RPC protocol
// errors. M2 defines the codes and serialization; M4 wires them into the action
// transaction.

// ErrorKindElementMismatch is the runtime-side marker a stateless runtime script
// (runtime.py / runtime.ps1) returns when the node it resolved from the stored
// runtimeId path no longer matches the stored element's role (or stable name).
// The Go action transaction maps it to snapshot_target_changed with no retry of
// the same handle. It travels in the runtime response's errorKind field, never on
// the wire to the MCP client.
const ErrorKindElementMismatch = "element_mismatch"

// ErrorKindRejectedBeforeInput is the runtime-side marker a stateless runtime
// script sets on a validation failure raised from a site that provably precedes
// any native input (not settable on Windows, unknown element_index, no clickable
// point, unsupported tool). The Go action transaction maps it to AbortRestore
// (validation): the handle stays live and a retry with the same reference is
// possible, matching the Swift native-side behavior where a pre-input throw
// restores the handle to live. Any unmarked ok:false stays uncertain (fail-safe),
// so a runtime raise site is marked only when its pre-input guarantee is provable.
const ErrorKindRejectedBeforeInput = "rejected_before_input"

// MsgCoordinatesOutOfBounds is the pinned, cross-platform message for a
// coordinate action whose point falls outside the captured screenshot. Bounds are
// half-open: valid iff 0 <= x < width and 0 <= y < height. It is a plain isError
// tool result (not a snapshot taxonomy code); the handle stays live, so the model
// may adjust the coordinates and retry the same snapshot_ref.
const MsgCoordinatesOutOfBounds = "Coordinates are outside the captured screenshot bounds. Adjust the coordinates for this snapshot_ref or call get_app_state."

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

// Pinned tool-error messages. Every message states the retry semantics so the
// model knows whether it may reuse the same snapshot_ref or must recapture. The
// missing and malformed strings are the cross-platform pinned contract and are
// byte-identical to the Swift emission; the others are shared by both Go apps and
// mirrored by Swift. A single code (target_changed, action_outcome_uncertain)
// may carry two messages depending on the transition, so the app selects the
// specific constant rather than a single per-code lookup at those sites.
const (
	MsgSnapshotRefMissing   = "Missing required argument: snapshot_ref. Call get_app_state and pass the returned snapshot_ref."
	MsgSnapshotRefMalformed = "Malformed snapshot_ref. Call get_app_state and pass the returned snapshot_ref."
	MsgSnapshotRefUnknown   = "Unknown snapshot_ref. Call get_app_state and pass the returned snapshot_ref."
	MsgSnapshotRefExpired   = "Expired snapshot_ref. Call get_app_state and pass the returned snapshot_ref."
	MsgSnapshotRefStale     = "Stale snapshot_ref; it was superseded by a newer snapshot. Call get_app_state and pass the returned snapshot_ref."
	MsgSnapshotRefInUse     = "snapshot_ref is already in use by an in-flight action. Retry with the same snapshot_ref after the in-flight action completes."

	MsgSnapshotTargetChangedApp     = "The requested app no longer matches the snapshot target. Call get_app_state and pass the returned snapshot_ref."
	MsgSnapshotTargetChangedElement = "The targeted element no longer matches the captured snapshot. Call get_app_state and pass the returned snapshot_ref."

	MsgSnapshotActionOutcomeUncertain = "The action was dispatched but its outcome is unknown. Call get_app_state and pass the returned snapshot_ref before retrying."
	MsgSnapshotActionRefreshFailed    = "The action likely succeeded, but the updated state could not be recaptured. Call get_app_state and pass the returned snapshot_ref before continuing."
)

// MessageForCode returns the default pinned message for a snapshot error code.
// Codes with a single message (the resolve-class codes and in_use) map directly;
// the two-message codes return their app-identity / mid-dispatch default so a
// caller that has no more specific context still emits a valid message.
func MessageForCode(code string) string {
	switch code {
	case ErrSnapshotRefMissing:
		return MsgSnapshotRefMissing
	case ErrSnapshotRefMalformed:
		return MsgSnapshotRefMalformed
	case ErrSnapshotRefUnknown:
		return MsgSnapshotRefUnknown
	case ErrSnapshotRefExpired:
		return MsgSnapshotRefExpired
	case ErrSnapshotRefStale:
		return MsgSnapshotRefStale
	case ErrSnapshotRefInUse:
		return MsgSnapshotRefInUse
	case ErrSnapshotTargetChanged:
		return MsgSnapshotTargetChangedApp
	case ErrSnapshotActionOutcomeUncertain:
		return MsgSnapshotActionOutcomeUncertain
	default:
		return ""
	}
}

// ResolveErrorCodeMessage extracts the snapshot tool-error code and its pinned
// message from a store error. It reports ok=false for an error that is not a
// *ResolveError (BeginAction/FinishSuccess only ever return *ResolveError for the
// resolve-class failures, so a false here signals an unexpected internal error).
func ResolveErrorCodeMessage(err error) (code, message string, ok bool) {
	var re *ResolveError
	if !errors.As(err, &re) {
		return "", "", false
	}
	code = re.Code()
	return code, MessageForCode(code), true
}

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
