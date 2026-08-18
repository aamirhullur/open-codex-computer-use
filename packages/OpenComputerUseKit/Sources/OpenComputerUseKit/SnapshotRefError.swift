import Foundation

// Snapshot-reference tool-level error taxonomy. These are ordinary tools/call
// results with isError true (not JSON-RPC protocol errors); the stable code and
// retry hint travel in structuredContent.error. M2 defines the constants and
// serialization; M4 wires them into action enforcement.

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
            return "snapshot_ref is required. Call get_app_state first and pass the snapshot_ref it returns."
        case .malformed:
            return "snapshot_ref is malformed. Call get_app_state and pass the snapshot_ref it returns verbatim."
        case .unknown:
            return "snapshot_ref is unknown. Call get_app_state to capture fresh state."
        case .expired:
            return "snapshot_ref has expired. Call get_app_state to capture fresh state."
        case .stale:
            return "snapshot_ref is stale; a newer snapshot exists. Call get_app_state to capture fresh state."
        case .inUse:
            return "snapshot_ref is in use by another action. Retry with the same snapshot_ref once it completes."
        case .targetChanged:
            return "The app or window changed since this snapshot_ref was captured. Call get_app_state to capture fresh state."
        case .actionOutcomeUncertain:
            return "The action outcome is uncertain, so this snapshot_ref was invalidated. Call get_app_state to capture fresh state."
        }
    }
}
