import CoreGraphics
import Foundation

// M4: the modern action transaction. A modern action tool consumes a snapshot_ref
// through a seven-step transaction (design "Action transaction"): parse/validate
// the ref before any side effect, resolve + CAS the handle to in_flight, verify the
// requested app still resolves to the handle's identity/PID/window, revalidate the
// concrete target, dispatch EXACTLY ONE native action from the STORED snapshot,
// then recapture, mint the successor generation, and supersede the old handle. The
// dispatcher performs the ref parse + CAS (SnapshotContext); the service performs
// identity/revalidation/dispatch/recapture and owns the handle's terminal state on
// every path.

// The value the dispatcher-level resolve step hands to a service action: the
// resolved record (already moved to in_flight under the store lock), the generation
// it captured, and the store reference used for the terminal transition.
public struct SnapshotContext {
    public let record: SnapshotRecord
    public let generation: Int
    public let store: SnapshotHandleStore

    public init(record: SnapshotRecord, generation: Int, store: SnapshotHandleStore) {
        self.record = record
        self.generation = generation
        self.store = store
    }
}

// A parsed modern action and its arguments, produced by the dispatcher and consumed
// by the transaction. Coordinates are screenshot pixels; element indices are the
// stored snapshot's indices. This carries no snapshot_ref: the ref is already
// resolved into the SnapshotContext before an action value is dispatched.
public enum ModernAction {
    case click(elementIndex: String?, x: Double?, y: Double?, clickCount: Int, mouseButton: String, clickMethod: ClickMethod)
    case performSecondaryAction(elementIndex: String, action: String)
    case scroll(direction: String, elementIndex: String, pages: Double)
    case drag(fromX: Double, fromY: Double, toX: Double, toY: Double)
    case typeText(text: String)
    case pressKey(key: String)
    case setValue(elementIndex: String, value: String)
}

// Thrown by a dispatch that BEGAN native input but cannot confirm the outcome. The
// transaction invalidates the handle (failure class 2) rather than restore it, so a
// possibly-applied action is never silently retried. Production dispatch surfaces
// ordinary validation errors (which occur before any input) as ComputerUseError;
// this type is reserved for genuine mid-flight uncertainty and the test seam.
public struct ModernActionUncertain: Error {
    public let message: String
    public init(_ message: String = SnapshotRefMessages.outcomeUncertain) {
        self.message = message
    }
}

// Thrown by pre-dispatch revalidation for a bad ARGUMENT (not a stale snapshot):
// e.g. coordinates outside the captured screenshot. The handle is still valid, so
// the transaction restores it to live (no mismatched tally) and returns a plain
// isError text result with no structuredContent.error envelope. The caller can fix
// the argument and retry the same snapshot_ref.
public struct ModernActionArgumentError: Error {
    public let message: String
    public init(_ message: String) { self.message = message }
}

// Pinned argument-error text for an out-of-bounds coordinate, shared with Go. Not a
// snapshot-taxonomy message: out-of-bounds coordinates are an argument error, so
// this is returned as plain isError text without a structuredContent.error code.
public let modernCoordinatesOutOfBoundsMessage =
    "Coordinates are outside the captured screenshot bounds. Adjust the coordinates for this snapshot_ref or call get_app_state."

// The three environment-dependent operations of the transaction, injectable so the
// transaction is unit-testable without native input or live capture. Production
// binds these to the service's live native methods; tests supply fakes that count
// dispatches, block for concurrency, or force a specific failure class.
public struct ModernActionHooks {
    // Steps 3+5: identity/PID/window and target revalidation. Throw
    // SnapshotRefError(.targetChanged) to reject before any input.
    public var precheck: (_ action: ModernAction, _ query: String, _ record: SnapshotRecord) throws -> Void
    // Step 6: perform EXACTLY ONE native action from the STORED snapshot. Throw
    // ModernActionUncertain if input began but the outcome is unknown.
    public var dispatch: (_ action: ModernAction, _ record: SnapshotRecord) throws -> Void
    // Step 7: capture post-action state for the successor mint.
    public var recapture: (_ query: String) throws -> AppSnapshot

    public init(
        precheck: @escaping (ModernAction, String, SnapshotRecord) throws -> Void,
        dispatch: @escaping (ModernAction, SnapshotRecord) throws -> Void,
        recapture: @escaping (String) throws -> AppSnapshot
    ) {
        self.precheck = precheck
        self.dispatch = dispatch
        self.recapture = recapture
    }
}

extension ComputerUseService {
    // Effective hooks: the test override when present, otherwise the live native
    // implementations. The dispatch default binds to the STORED snapshot on the
    // record, never currentSnapshot/refreshSnapshot, so no modern action reaches the
    // implicit fresh-capture fallback.
    func effectiveModernHooks() -> ModernActionHooks {
        if let override = modernActionHooksOverride {
            return override
        }
        return ModernActionHooks(
            precheck: { [unowned self] action, query, record in
                try self.liveModernPrecheck(action, query: query, record: record)
            },
            dispatch: { [unowned self] action, record in
                guard let snapshot = record.snapshot else {
                    // The stored payload was released out from under an in_flight
                    // record. Treat as uncertain so the handle is invalidated.
                    throw ModernActionUncertain()
                }
                try self.liveDispatch(action, on: snapshot)
            },
            recapture: { [unowned self] query in
                try self.liveRecapture(query: query)
            }
        )
    }

    // Run the modern action transaction against a SnapshotContext whose handle is
    // already in_flight. Owns the handle's terminal state on EVERY path: restore to
    // live on pre-dispatch failure, invalidate on uncertain dispatch, supersede on
    // refresh failure, and supersede + mint successor on success.
    func performModernAction(_ action: ModernAction, appQuery: String, context: SnapshotContext) -> ToolCallResult {
        let store = context.store
        let record = context.record
        let hooks = effectiveModernHooks()

        // Steps 3+5: pre-dispatch validation. Any failure restores the handle to
        // live so it stays usable; no native input has occurred.
        do {
            try hooks.precheck(action, appQuery, record)
        } catch let argument as ModernActionArgumentError {
            // Bad argument (e.g. out-of-bounds coordinate): the handle is still valid,
            // so restore it to live without a mismatch tally and return plain text.
            store.restoreLive(record)
            return ToolCallResult.text(argument.message, isError: true)
        } catch let error as SnapshotRefError {
            store.failMismatchAndRestoreLive(record)
            return error.toToolCallResult()
        } catch {
            store.failMismatchAndRestoreLive(record)
            return SnapshotRefError.make(.targetChanged, message: SnapshotRefMessages.targetChangedApp).toToolCallResult()
        }

        // Step 6: exactly one native dispatch from the stored snapshot.
        do {
            try hooks.dispatch(action, record)
        } catch let uncertain as ModernActionUncertain {
            // Failure class 2: input began, outcome unknown. Invalidate the handle.
            store.invalidateUncertain(record)
            return SnapshotRefError.make(
                .actionOutcomeUncertain,
                message: uncertain.message,
                retry: .newState
            ).toToolCallResult()
        } catch {
            // The dispatch rejected before any input took effect (a validation error
            // from the native path). Restore the handle; the caller may retry it.
            store.restoreLive(record)
            return toolErrorResult(from: error)
        }

        // Step 7: recapture, mint the successor, supersede the old handle.
        do {
            let post = try hooks.recapture(appQuery)
            let successor = try store.supersedeAndMintSuccessor(
                old: record,
                snapshot: post,
                screenshotPixels: snapshotScreenshotPixels(post),
                captureOptions: record.captureOptions
            )
            return SnapshotStructuredContent.result(
                snapshot: post,
                snapshotRef: successor.handle,
                capturedAt: successor.capturedAt,
                expiresAt: successor.expiresAt,
                generation: successor.generation,
                screenshotPixels: snapshotScreenshotPixels(post)
            )
        } catch {
            // Failure class 3: the action landed but state capture failed. Supersede
            // the old handle and instruct the caller to recapture. Adjudicated to the
            // actionOutcomeUncertain code with refresh-specific wording.
            store.failRefresh(record)
            return SnapshotRefError.make(
                .actionOutcomeUncertain,
                message: SnapshotRefMessages.refreshFailed,
                retry: .newState
            ).toToolCallResult()
        }
    }

    // Convert a non-uncertain dispatch error into a tool-style error result,
    // mirroring the adapter's tools/call error coercion.
    private func toolErrorResult(from error: Error) -> ToolCallResult {
        if let computerUseError = error as? ComputerUseError {
            return ToolCallResult.text(
                computerUseError.errorDescription ?? String(describing: computerUseError),
                isError: computerUseError.toolResultIsError
            )
        }
        let message = (error as? LocalizedError)?.errorDescription ?? String(describing: error)
        return ToolCallResult.text(message, isError: true)
    }
}
