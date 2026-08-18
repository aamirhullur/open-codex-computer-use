import Foundation

// Snapshot-reference tool-level error taxonomy. These are ordinary tools/call
// results with isError true (not JSON-RPC protocol errors); the stable code and
// retry hint travel in structuredContent.error. M2 defines the constants and
// serialization; M4 wires them into action enforcement.

// Canonical, cross-platform snapshot-ref error messages. Byte-identical to the Go
// Msg* constants in packages/go-mcp/snapshoterr.go; the shared modern fixtures pin
// the missing and malformed wording. Every string names the recovery: recapture
// with get_app_state, or (for in_use) retry the same handle once it frees. Keep
// these identical across platforms. A single code (target_changed,
// action_outcome_uncertain) carries two messages depending on the transition, so
// call sites select the specific constant.
public enum SnapshotRefMessages {
    public static let missing = "Missing required argument: snapshot_ref. Call get_app_state and pass the returned snapshot_ref."
    public static let malformed = "Malformed snapshot_ref. Call get_app_state and pass the returned snapshot_ref."
    public static let unknown = "Unknown snapshot_ref. Call get_app_state and pass the returned snapshot_ref."
    public static let expired = "Expired snapshot_ref. Call get_app_state and pass the returned snapshot_ref."
    public static let stale = "Stale snapshot_ref; it was superseded by a newer snapshot. Call get_app_state and pass the returned snapshot_ref."
    public static let inUse = "snapshot_ref is already in use by an in-flight action. Retry with the same snapshot_ref after the in-flight action completes."
    // target_changed carries two transition-specific messages.
    public static let targetChangedApp = "The requested app no longer matches the snapshot target. Call get_app_state and pass the returned snapshot_ref."
    public static let targetChangedElement = "The targeted element no longer matches the captured snapshot. Call get_app_state and pass the returned snapshot_ref."
    // action_outcome_uncertain carries two transition-specific messages.
    public static let outcomeUncertain = "The action was dispatched but its outcome is unknown. Call get_app_state and pass the returned snapshot_ref before retrying."
    public static let refreshFailed = "The action likely succeeded, but the updated state could not be recaptured. Call get_app_state and pass the returned snapshot_ref before continuing."
}

public enum SnapshotRefErrorCode: String {
    case missing = "snapshot_ref_missing"
    case malformed = "snapshot_ref_malformed"
    case unknown = "snapshot_ref_unknown"
    case expired = "snapshot_ref_expired"
    case stale = "snapshot_ref_stale"
    case inUse = "snapshot_ref_in_use"
    case targetChanged = "snapshot_target_changed"
    case actionOutcomeUncertain = "snapshot_action_outcome_uncertain"
}

// Whether the caller may retry with the same handle or must call get_app_state
// for fresh state. Only in-flight contention is safe to retry on the same
// handle; every other failure requires a new capture.
public enum SnapshotRefRetry: String {
    case sameHandle = "same_handle"
    case newState = "new_state"
}

public struct SnapshotRefError: Error {
    public let code: SnapshotRefErrorCode
    public let message: String
    public let retry: SnapshotRefRetry

    public init(code: SnapshotRefErrorCode, message: String, retry: SnapshotRefRetry) {
        self.code = code
        self.message = message
        self.retry = retry
    }

    // Convenience factory using the code's default message and retry hint. M4 may
    // pass a more specific message; the retry default still applies unless
    // overridden.
    public static func make(
        _ code: SnapshotRefErrorCode,
        message: String? = nil,
        retry: SnapshotRefRetry? = nil
    ) -> SnapshotRefError {
        SnapshotRefError(
            code: code,
            message: message ?? code.defaultMessage,
            retry: retry ?? code.defaultRetry
        )
    }

    public func toToolCallResult() -> ToolCallResult {
        ToolCallResult(
            content: [.text(message)],
            isError: true,
            structuredContent: [
                "error": [
                    "code": code.rawValue,
                    "message": message,
                    "retry": retry.rawValue,
                ],
            ]
        )
    }
}

extension SnapshotRefErrorCode {
    var defaultRetry: SnapshotRefRetry {
        switch self {
        case .inUse:
            return .sameHandle
        case .missing, .malformed, .unknown, .expired, .stale, .targetChanged, .actionOutcomeUncertain:
            return .newState
        }
    }

    var defaultMessage: String {
        switch self {
        case .missing:
            return SnapshotRefMessages.missing
        case .malformed:
            return SnapshotRefMessages.malformed
        case .unknown:
            return SnapshotRefMessages.unknown
        case .expired:
            return SnapshotRefMessages.expired
        case .stale:
            return SnapshotRefMessages.stale
        case .inUse:
            return SnapshotRefMessages.inUse
        case .targetChanged:
            return SnapshotRefMessages.targetChangedApp
        case .actionOutcomeUncertain:
            return SnapshotRefMessages.outcomeUncertain
        }
    }
}
