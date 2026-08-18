import CoreGraphics
import XCTest
@testable import OpenComputerUseKit

// M4: the modern action transaction. Drives the dispatcher's modern action path
// end to end against a real SnapshotHandleStore, using the ModernActionHooks seam so
// no native input or live capture runs. The dispatch-counting seam is the injected
// hooks.dispatch closure: tests substitute a fake that counts (and can block) every
// native dispatch, so acceptance criteria about "zero native input" and "exactly one
// dispatch" are asserted deterministically.
final class M4TransactionTests: XCTestCase {

    // MARK: - Deterministic fakes

    private final class FakeClock: SnapshotClock, @unchecked Sendable {
        private let lock = NSLock()
        private var current: Date
        init(_ start: Date) { current = start }
        func now() -> Date { lock.lock(); defer { lock.unlock() }; return current }
        func advance(_ interval: TimeInterval) { lock.lock(); current = current.addingTimeInterval(interval); lock.unlock() }
    }

    private final class DeterministicTokenSource: SnapshotTokenSource, @unchecked Sendable {
        private let lock = NSLock()
        private var counter: UInt64 = 0
        func nextTokenBytes(count: Int) throws -> [UInt8] {
            lock.lock(); defer { lock.unlock() }
            counter += 1
            var bytes = [UInt8](repeating: 0, count: count)
            var value = counter
            var index = count - 1
            while value > 0, index >= 0 {
                bytes[index] = UInt8(value & 0xff)
                value >>= 8
                index -= 1
            }
            return bytes
        }
    }

    // The dispatch-counting seam. Increments per native dispatch; can block on a gate
    // so two concurrent uses of one handle can be observed deterministically.
    private final class DispatchCounter: @unchecked Sendable {
        private let lock = NSLock()
        private(set) var count = 0
        func increment() { lock.lock(); count += 1; lock.unlock() }
        var value: Int { lock.lock(); defer { lock.unlock() }; return count }
    }

    // MARK: - Snapshot + store helpers

    private let defaultCaptureOptions = SnapshotCaptureOptions(textLimitMaxCount: 500, maxTreeNodes: 1200, maxTreeDepth: 64)

    private func makeSnapshot(
        name: String = "Example",
        bundleIdentifier: String? = "com.example.app",
        pid: pid_t = 1234,
        windowID: CGWindowID? = 42,
        mode: SnapshotMode = .fixture,
        elements: [Int: ElementRecord] = [:]
    ) -> AppSnapshot {
        AppSnapshot(
            app: RunningAppDescriptor(
                name: name,
                bundleIdentifier: bundleIdentifier,
                pid: pid,
                runningApplication: NSRunningApplication.current
            ),
            windowTitle: name,
            windowBounds: CGRect(x: 0, y: 0, width: 1200, height: 800),
            targetWindowID: windowID,
            targetWindowLayer: 0,
            screenshotPNGData: nil,
            mode: mode,
            treeLines: ["[0] Button \"OK\""],
            focusedSummary: nil,
            focusedElement: nil,
            selectedText: nil,
            elements: elements
        )
    }

    // Mint a snapshot and return its stored record (screenshot pixels 1200x800, so
    // coordinate revalidation has concrete bounds).
    private func mintRecord(in store: SnapshotHandleStore, snapshot: AppSnapshot) -> SnapshotRecord {
        try! store.mint(
            snapshot: snapshot,
            screenshotPixels: CGSize(width: 1200, height: 800),
            captureOptions: defaultCaptureOptions
        ).record
    }

    private func makeStore(clock: SnapshotClock = SystemSnapshotClock()) -> SnapshotHandleStore {
        SnapshotHandleStore(clock: clock, tokenSource: DeterministicTokenSource())
    }

    private func mintHandle(in store: SnapshotHandleStore, windowID: CGWindowID = 42) -> String {
        try! store.mint(
            snapshot: makeSnapshot(windowID: windowID),
            screenshotPixels: CGSize(width: 1200, height: 800),
            captureOptions: defaultCaptureOptions
        ).handle
    }

    // Hooks that pass precheck, count and optionally block dispatch, and recapture a
    // fresh same-target snapshot so the successor mint yields generation n+1.
    private func countingHooks(
        counter: DispatchCounter,
        onDispatch: @escaping () -> Void = {},
        precheck: @escaping (ModernAction, String, SnapshotRecord) throws -> Void = { _, _, _ in },
        dispatchThrows: @escaping () throws -> Void = {},
        recapture: (() throws -> AppSnapshot)? = nil
    ) -> ModernActionHooks {
        ModernActionHooks(
            precheck: precheck,
            dispatch: { _, _ in
                counter.increment()
                onDispatch()
                try dispatchThrows()
            },
            recapture: { _ in
                if let recapture { return try recapture() }
                return self.makeSnapshot()
            }
        )
    }

    private func errorCode(_ result: ToolCallResult) -> String? {
        (result.structuredContent?["error"] as? [String: Any])?["code"] as? String
    }

    private func errorRetry(_ result: ToolCallResult) -> String? {
        (result.structuredContent?["error"] as? [String: Any])?["retry"] as? String
    }

    private func actionArgs(_ tool: String, ref: String?) -> [String: Any] {
        var args: [String: Any] = ["app": "Example"]
        if let ref { args["snapshot_ref"] = ref }
        switch tool {
        case "click": args["x"] = 10; args["y"] = 10
        case "perform_secondary_action": args["element_index"] = "0"; args["action"] = "Raise"
        case "scroll": args["direction"] = "down"; args["element_index"] = "0"
        case "drag": args["from_x"] = 1; args["from_y"] = 1; args["to_x"] = 2; args["to_y"] = 2
        case "type_text": args["text"] = "hi"
        case "press_key": args["key"] = "a"
        case "set_value": args["element_index"] = "0"; args["value"] = "v"
        default: break
        }
        return args
    }

    private static let allModernActionTools = [
        "click", "perform_secondary_action", "scroll", "drag", "type_text", "press_key", "set_value",
    ]

    // MARK: - Missing ref (before any side effect, including app resolution)

    func testAllModernActionsRejectMissingRefBeforeSideEffects() throws {
        for tool in Self.allModernActionTools {
            let store = makeStore()
            let service = ComputerUseService(snapshotHandleStore: store)
            let counter = DispatchCounter()
            // Precheck also counts, so "no app resolution" is provable: it must never run.
            var precheckRan = false
            service.modernActionHooksOverride = countingHooks(
                counter: counter,
                precheck: { _, _, _ in precheckRan = true }
            )
            let dispatcher = ComputerUseToolDispatcher(service: service)

            let result = try dispatcher.callTool(name: tool, arguments: actionArgs(tool, ref: nil), modern: true)

            XCTAssertTrue(result.isError, tool)
            XCTAssertEqual(errorCode(result), "snapshot_ref_missing", tool)
            XCTAssertEqual(result.primaryText, SnapshotRefMessages.missing, tool)
            XCTAssertEqual(errorRetry(result), "new_state", tool)
            XCTAssertEqual(counter.value, 0, "\(tool): no dispatch on missing ref")
            XCTAssertFalse(precheckRan, "\(tool): no app resolution / precheck on missing ref")
            XCTAssertEqual(store.snapshotCounters().resolved, 0, tool)
        }
    }

    func testMissingRefExactMessagePinned() {
        XCTAssertEqual(
            SnapshotRefMessages.missing,
            "Missing required argument: snapshot_ref. Call get_app_state and pass the returned snapshot_ref."
        )
        XCTAssertEqual(
            SnapshotRefMessages.malformed,
            "Malformed snapshot_ref. Call get_app_state and pass the returned snapshot_ref."
        )
    }

    // MARK: - Malformed ref

    func testMalformedRefRejectedBeforeSideEffects() throws {
        let store = makeStore()
        let service = ComputerUseService(snapshotHandleStore: store)
        let counter = DispatchCounter()
        service.modernActionHooksOverride = countingHooks(counter: counter)
        let dispatcher = ComputerUseToolDispatcher(service: service)

        for bad in ["garbage", "ocu_snapshot_v1_short", "ocu_snapshot_v1_" + String(repeating: "!", count: 32)] {
            let result = try dispatcher.callTool(name: "click", arguments: actionArgs("click", ref: bad), modern: true)
            XCTAssertTrue(result.isError)
            XCTAssertEqual(errorCode(result), "snapshot_ref_malformed", bad)
            XCTAssertEqual(result.primaryText, SnapshotRefMessages.malformed, bad)
            XCTAssertEqual(errorRetry(result), "new_state", bad)
        }
        XCTAssertEqual(counter.value, 0)
    }

    // MARK: - Store resolve failures (unknown / expired / stale) -> zero dispatch

    func testUnknownRefRejected() throws {
        let store = makeStore()
        let service = ComputerUseService(snapshotHandleStore: store)
        let counter = DispatchCounter()
        service.modernActionHooksOverride = countingHooks(counter: counter)
        let dispatcher = ComputerUseToolDispatcher(service: service)

        let unknown = "ocu_snapshot_v1_" + SnapshotHandleStore.base64url([UInt8](repeating: 7, count: 24))
        let result = try dispatcher.callTool(name: "click", arguments: actionArgs("click", ref: unknown), modern: true)
        XCTAssertEqual(errorCode(result), "snapshot_ref_unknown")
        XCTAssertEqual(counter.value, 0)
    }

    func testExpiredRefRejected() throws {
        let clock = FakeClock(Date(timeIntervalSince1970: 1_000_000))
        let store = makeStore(clock: clock)
        let service = ComputerUseService(snapshotHandleStore: store)
        let counter = DispatchCounter()
        service.modernActionHooksOverride = countingHooks(counter: counter)
        let dispatcher = ComputerUseToolDispatcher(service: service)

        let handle = mintHandle(in: store)
        clock.advance(SnapshotHandleStoreLimits.ttl)
        let result = try dispatcher.callTool(name: "click", arguments: actionArgs("click", ref: handle), modern: true)
        XCTAssertEqual(errorCode(result), "snapshot_ref_expired")
        XCTAssertEqual(counter.value, 0)
    }

    func testStaleRefRejected() throws {
        let store = makeStore()
        let service = ComputerUseService(snapshotHandleStore: store)
        let counter = DispatchCounter()
        service.modernActionHooksOverride = countingHooks(counter: counter)
        let dispatcher = ComputerUseToolDispatcher(service: service)

        let first = mintHandle(in: store)   // generation 1
        _ = mintHandle(in: store)           // generation 2 supersedes generation 1
        let result = try dispatcher.callTool(name: "click", arguments: actionArgs("click", ref: first), modern: true)
        XCTAssertEqual(errorCode(result), "snapshot_ref_stale")
        XCTAssertEqual(counter.value, 0)
    }

    func testInUseRefRejectedRetrySameHandle() throws {
        let store = makeStore()
        let service = ComputerUseService(snapshotHandleStore: store)
        let counter = DispatchCounter()
        service.modernActionHooksOverride = countingHooks(counter: counter)
        let dispatcher = ComputerUseToolDispatcher(service: service)

        let handle = mintHandle(in: store)
        // Hold the handle in_flight by acquiring it directly and not completing.
        guard case .success = store.beginInFlight(handle) else {
            return XCTFail("first begin should win")
        }
        let result = try dispatcher.callTool(name: "click", arguments: actionArgs("click", ref: handle), modern: true)
        XCTAssertEqual(errorCode(result), "snapshot_ref_in_use")
        XCTAssertEqual(errorRetry(result), "same_handle")
        XCTAssertEqual(counter.value, 0)
        XCTAssertEqual(store.snapshotCounters().concurrent, 1)
    }

    // MARK: - App identity mismatch -> target_changed, zero dispatch

    func testAppIdentityMismatchTargetChangedZeroDispatch() throws {
        let store = makeStore()
        let service = ComputerUseService(snapshotHandleStore: store)
        let counter = DispatchCounter()
        service.modernActionHooksOverride = countingHooks(
            counter: counter,
            precheck: { _, _, _ in
                throw SnapshotRefError.make(.targetChanged, message: SnapshotRefMessages.targetChangedApp)
            }
        )
        let dispatcher = ComputerUseToolDispatcher(service: service)

        let handle = mintHandle(in: store)
        let result = try dispatcher.callTool(name: "click", arguments: actionArgs("click", ref: handle), modern: true)
        XCTAssertEqual(errorCode(result), "snapshot_target_changed")
        XCTAssertEqual(counter.value, 0, "target-changed must not dispatch")
        XCTAssertEqual(store.snapshotCounters().mismatched, 1)

        // Pre-dispatch failure restores the handle to live: it can be acquired again.
        guard case .success = store.beginInFlight(handle) else {
            return XCTFail("handle should be restored to live after a mismatch")
        }
    }

    // MARK: - Concurrency: two uses of one handle -> exactly one dispatch

    func testConcurrentUsesDispatchExactlyOnce() throws {
        let store = makeStore()
        let service = ComputerUseService(snapshotHandleStore: store)
        let counter = DispatchCounter()
        let entered = DispatchSemaphore(value: 0)
        let proceed = DispatchSemaphore(value: 0)
        service.modernActionHooksOverride = countingHooks(
            counter: counter,
            onDispatch: {
                entered.signal()   // one dispatch has begun and is holding in_flight
                proceed.wait()     // block so the loser observes in_flight
            }
        )
        let dispatcher = ComputerUseToolDispatcher(service: service)
        let handle = mintHandle(in: store)

        var results: [ToolCallResult] = []
        let resultsLock = NSLock()
        let group = DispatchGroup()
        for _ in 0..<2 {
            group.enter()
            DispatchQueue.global().async {
                let result = try! dispatcher.callTool(name: "click", arguments: self.actionArgs("click", ref: handle), modern: true)
                resultsLock.lock(); results.append(result); resultsLock.unlock()
                group.leave()
            }
        }

        // Wait for the winner to enter dispatch, then release it.
        XCTAssertEqual(entered.wait(timeout: .now() + 5), .success)
        proceed.signal()
        XCTAssertEqual(group.wait(timeout: .now() + 5), .success)

        XCTAssertEqual(counter.value, 1, "exactly one native dispatch across two concurrent uses")
        let codes = results.map { errorCode($0) }
        XCTAssertEqual(codes.filter { $0 == "snapshot_ref_in_use" }.count, 1, "loser gets in_use")
        XCTAssertEqual(codes.filter { $0 == nil }.count, 1, "winner returns a success result")
    }

    // MARK: - Pre-dispatch failure restores live; retry then succeeds

    func testPreDispatchFailureRestoresLiveThenRetrySucceeds() throws {
        let store = makeStore()
        let service = ComputerUseService(snapshotHandleStore: store)
        let counter = DispatchCounter()
        var precheckCalls = 0
        service.modernActionHooksOverride = countingHooks(
            counter: counter,
            precheck: { _, _, _ in
                precheckCalls += 1
                if precheckCalls == 1 {
                    throw SnapshotRefError.make(.targetChanged, message: SnapshotRefMessages.targetChangedApp)
                }
            }
        )
        let dispatcher = ComputerUseToolDispatcher(service: service)
        let handle = mintHandle(in: store)

        let first = try dispatcher.callTool(name: "click", arguments: actionArgs("click", ref: handle), modern: true)
        XCTAssertEqual(errorCode(first), "snapshot_target_changed")
        XCTAssertEqual(counter.value, 0)

        // Same handle, restored to live: the retry proceeds through dispatch to a successor.
        let second = try dispatcher.callTool(name: "click", arguments: actionArgs("click", ref: handle), modern: true)
        XCTAssertFalse(second.isError, "retry on the restored handle should succeed")
        XCTAssertEqual(counter.value, 1)
        let successorRef = second.structuredContent?["snapshot_ref"] as? String
        XCTAssertNotNil(successorRef)
    }

    // MARK: - Uncertain dispatch supersedes the handle

    func testUncertainDispatchInvalidatesHandle() throws {
        let store = makeStore()
        let service = ComputerUseService(snapshotHandleStore: store)
        let counter = DispatchCounter()
        service.modernActionHooksOverride = countingHooks(
            counter: counter,
            dispatchThrows: { throw ModernActionUncertain() }
        )
        let dispatcher = ComputerUseToolDispatcher(service: service)
        let handle = mintHandle(in: store)

        let result = try dispatcher.callTool(name: "click", arguments: actionArgs("click", ref: handle), modern: true)
        XCTAssertEqual(errorCode(result), "snapshot_action_outcome_uncertain")
        XCTAssertEqual(errorRetry(result), "new_state")
        XCTAssertEqual(counter.value, 1, "the one dispatch began before going uncertain")
        XCTAssertEqual(store.snapshotCounters().uncertain, 1)

        // The invalidated handle can never dispatch again.
        let retry = try dispatcher.callTool(name: "click", arguments: actionArgs("click", ref: handle), modern: true)
        XCTAssertEqual(errorCode(retry), "snapshot_ref_stale")
        XCTAssertEqual(counter.value, 1)
    }

    // MARK: - Refresh failure supersedes + adjudicated error

    func testRefreshFailureSupersedesAndReturnsAdjudicatedError() throws {
        let store = makeStore()
        let service = ComputerUseService(snapshotHandleStore: store)
        let counter = DispatchCounter()
        struct RecaptureFailed: Error {}
        service.modernActionHooksOverride = countingHooks(
            counter: counter,
            recapture: { throw RecaptureFailed() }
        )
        let dispatcher = ComputerUseToolDispatcher(service: service)
        let handle = mintHandle(in: store)

        let result = try dispatcher.callTool(name: "click", arguments: actionArgs("click", ref: handle), modern: true)
        XCTAssertEqual(errorCode(result), "snapshot_action_outcome_uncertain")
        XCTAssertEqual(result.primaryText, SnapshotRefMessages.refreshFailed)
        XCTAssertEqual(errorRetry(result), "new_state")
        XCTAssertEqual(counter.value, 1, "dispatch succeeded before the refresh failed")
        XCTAssertEqual(store.snapshotCounters().refreshFailed, 1)

        // The old handle was superseded and cannot be reused.
        let retry = try dispatcher.callTool(name: "click", arguments: actionArgs("click", ref: handle), modern: true)
        XCTAssertEqual(errorCode(retry), "snapshot_ref_stale")
    }

    // MARK: - Successful chain returns a usable successor (fixture-mode end to end)

    func testSuccessfulChainReturnsUsableSuccessor() throws {
        let store = makeStore()
        let service = ComputerUseService(snapshotHandleStore: store)
        let counter = DispatchCounter()
        service.modernActionHooksOverride = countingHooks(counter: counter)
        let dispatcher = ComputerUseToolDispatcher(service: service)

        let handle = mintHandle(in: store)   // generation 1

        let first = try dispatcher.callTool(name: "click", arguments: actionArgs("click", ref: handle), modern: true)
        XCTAssertFalse(first.isError)
        guard let ref1 = first.structuredContent?["snapshot_ref"] as? String else {
            return XCTFail("successful action must return a successor snapshot_ref")
        }
        XCTAssertTrue(SnapshotHandleStore.isWellFormed(ref1))
        XCTAssertEqual(first.structuredContent?["generation"] as? Int, 2)
        XCTAssertTrue(first.primaryText?.hasPrefix("snapshot_ref: \(ref1)") ?? false, "successor ref must also appear in text")

        // The original handle is superseded by the successful action.
        guard case let .failure(oldError) = store.beginInFlight(handle) else {
            return XCTFail("the consumed handle must be stale")
        }
        XCTAssertEqual(oldError.code, .stale)

        // Chain again on the successor: action -> action stays current.
        let second = try dispatcher.callTool(name: "click", arguments: actionArgs("click", ref: ref1), modern: true)
        XCTAssertFalse(second.isError)
        guard let ref2 = second.structuredContent?["snapshot_ref"] as? String else {
            return XCTFail("second action must return a successor snapshot_ref")
        }
        XCTAssertEqual(second.structuredContent?["generation"] as? Int, 3)
        XCTAssertNotEqual(ref1, ref2)
        XCTAssertEqual(counter.value, 2)
        guard case .success = store.resolve(ref2) else {
            return XCTFail("the latest successor must resolve live")
        }
    }

    // MARK: - Legacy actions do not consult the store

    func testLegacyActionDoesNotTouchStore() {
        let store = makeStore()
        let service = ComputerUseService(snapshotHandleStore: store)
        let dispatcher = ComputerUseToolDispatcher(service: service)

        // click_method accessibility without element_index throws inside the legacy
        // service method BEFORE any snapshot capture, proving the legacy path runs
        // without requiring or consulting a snapshot_ref. No native input occurs.
        let result = dispatcher.callToolAsResult(
            name: "click",
            arguments: ["app": "Example", "click_method": "accessibility"]
        )
        XCTAssertTrue(result.isError)
        XCTAssertNil(result.structuredContent, "legacy errors are not snapshot_ref errors")
        let counters = store.snapshotCounters()
        XCTAssertEqual(counters.minted, 0)
        XCTAssertEqual(counters.resolved, 0)
        XCTAssertEqual(counters.concurrent, 0)
        XCTAssertEqual(counters.mismatched, 0)
    }

    func testLegacyActionDoesNotRequireSnapshotRef() throws {
        // The same click without a ref: legacy must NOT emit snapshot_ref_missing.
        let store = makeStore()
        let service = ComputerUseService(snapshotHandleStore: store)
        let dispatcher = ComputerUseToolDispatcher(service: service)
        let legacy = dispatcher.callToolAsResult(name: "click", arguments: ["app": "Example", "click_method": "accessibility"])
        XCTAssertNotEqual(errorCode(legacy), "snapshot_ref_missing")

        // But the modern era does require it.
        let modern = try dispatcher.callTool(name: "click", arguments: ["app": "Example"], modern: true)
        XCTAssertEqual(errorCode(modern), "snapshot_ref_missing")
    }

    // MARK: - Grep-level assertion: modern transaction never reaches the implicit fallback

    func testModernTransactionSourceNeverUsesImplicitCapture() throws {
        // The modern transaction file must never reference currentSnapshot or
        // refreshSnapshot: the modern path dispatches from the STORED snapshot and
        // recaptures only through the explicit seam (liveRecapture).
        var url = URL(fileURLWithPath: #filePath)
        for _ in 0..<3 { url = url.deletingLastPathComponent() }
        let transaction = url
            .appendingPathComponent("Sources/OpenComputerUseKit/ModernActionTransaction.swift")
        // Ignore comment lines: match the CALL forms only, so the intent-describing
        // comments in the transaction file do not trip the grep.
        let codeLines = try String(contentsOf: transaction, encoding: .utf8)
            .split(separator: "\n", omittingEmptySubsequences: false)
            .filter { !$0.trimmingCharacters(in: .whitespaces).hasPrefix("//") }
        let code = codeLines.joined(separator: "\n")
        XCTAssertFalse(code.contains("currentSnapshot("), "modern transaction must not use the implicit currentSnapshot fallback")
        XCTAssertFalse(code.contains("refreshSnapshot("), "modern transaction must recapture only via the explicit seam")
    }

    // MARK: - Production hooks: real dispatch + real revalidation, zero native input

    // Fixture click through the PRODUCTION dispatch hook (override nil): proves
    // liveDispatch runs the real fixture dispatch core. Fixture mode posts to the
    // fixture bridge (a notification), never real input; the visual cursor is
    // disabled so no overlay is created.
    func testProductionDispatchRunsRealFixtureClickCore() {
        setenv("OPEN_COMPUTER_USE_VISUAL_CURSOR", "0", 1)
        defer { unsetenv("OPEN_COMPUTER_USE_VISUAL_CURSOR") }

        let store = makeStore()
        let service = ComputerUseService(snapshotHandleStore: store)   // no hooks override
        let element = ElementRecord(
            index: 0,
            identifier: "fixture-button",
            element: nil,
            localFrame: CGRect(x: 0, y: 0, width: 100, height: 40),
            rawActions: ["AXPress"],
            prettyActions: ["Press"]
        )
        let record = mintRecord(in: store, snapshot: makeSnapshot(mode: .fixture, elements: [0: element]))

        let hooks = service.effectiveModernHooks()
        XCTAssertNoThrow(
            try hooks.dispatch(.click(elementIndex: "0", x: nil, y: nil, clickCount: 1, mouseButton: "left", clickMethod: .auto), record),
            "the production dispatch hook must run the real fixture click core without throwing"
        )
    }

    func testProductionRevalidateCoordinateRejectsOutOfBounds() {
        let store = makeStore()
        let service = ComputerUseService(snapshotHandleStore: store)
        let record = mintRecord(in: store, snapshot: makeSnapshot(mode: .fixture))

        // Out of bounds (>= width / height) -> argument error, plain message.
        XCTAssertThrowsError(
            try service.revalidateModernTarget(.click(elementIndex: nil, x: 5000, y: 5000, clickCount: 1, mouseButton: "left", clickMethod: .auto), record: record)
        ) { error in
            guard let argument = error as? ModernActionArgumentError else {
                return XCTFail("expected a ModernActionArgumentError, got \(error)")
            }
            XCTAssertEqual(argument.message, modernCoordinatesOutOfBoundsMessage)
        }

        // Strict half-open bounds: the far edge (== dimension) is out; edge-1 is in.
        XCTAssertThrowsError(
            try service.revalidateModernTarget(.click(elementIndex: nil, x: 1200, y: 0, clickCount: 1, mouseButton: "left", clickMethod: .auto), record: record)
        )
        XCTAssertNoThrow(
            try service.revalidateModernTarget(.click(elementIndex: nil, x: 1199, y: 799, clickCount: 1, mouseButton: "left", clickMethod: .auto), record: record)
        )
        XCTAssertNoThrow(
            try service.revalidateModernTarget(.click(elementIndex: nil, x: 0, y: 0, clickCount: 1, mouseButton: "left", clickMethod: .auto), record: record)
        )
    }

    func testProductionRevalidateStoredElementRejectsDeadElement() {
        let store = makeStore()
        let service = ComputerUseService(snapshotHandleStore: store)

        // Accessibility mode with a nil backing AXUIElement is a dead element.
        let deadElement = ElementRecord(
            index: 0,
            identifier: nil,
            element: nil,
            localFrame: CGRect(x: 0, y: 0, width: 10, height: 10),
            rawActions: [],
            prettyActions: []
        )
        let record = mintRecord(in: store, snapshot: makeSnapshot(mode: .accessibility, elements: [0: deadElement]))

        XCTAssertThrowsError(
            try service.revalidateModernTarget(.setValue(elementIndex: "0", value: "v"), record: record)
        ) { error in
            XCTAssertEqual((error as? SnapshotRefError)?.code, .targetChanged)
            XCTAssertEqual((error as? SnapshotRefError)?.message, SnapshotRefMessages.targetChangedElement)
        }

        // An index absent from the stored snapshot is likewise a target change.
        XCTAssertThrowsError(
            try service.revalidateModernTarget(.scroll(direction: "down", elementIndex: "7", pages: 1), record: record)
        ) { error in
            XCTAssertEqual((error as? SnapshotRefError)?.code, .targetChanged)
        }
    }

    // MARK: - Out-of-bounds coordinate through the full transaction (item 2)

    func testOutOfBoundsCoordinateReturnsPlainArgumentErrorAndRestoresLive() throws {
        let store = makeStore()
        let service = ComputerUseService(snapshotHandleStore: store)
        let counter = DispatchCounter()
        // Precheck runs ONLY the real revalidation (skip native identity resolution),
        // so the real out-of-bounds path drives the transaction end to end.
        service.modernActionHooksOverride = ModernActionHooks(
            precheck: { action, _, record in try service.revalidateModernTarget(action, record: record) },
            dispatch: { _, _ in counter.increment() },
            recapture: { _ in self.makeSnapshot() }
        )
        let dispatcher = ComputerUseToolDispatcher(service: service)
        let handle = mintHandle(in: store)

        let result = try dispatcher.callTool(
            name: "click",
            arguments: ["app": "Example", "snapshot_ref": handle, "x": 9999, "y": 10],
            modern: true
        )
        XCTAssertTrue(result.isError)
        XCTAssertEqual(result.primaryText, modernCoordinatesOutOfBoundsMessage)
        XCTAssertNil(result.structuredContent, "coordinate errors carry no structuredContent.error envelope")
        XCTAssertEqual(counter.value, 0, "no dispatch on an out-of-bounds coordinate")
        XCTAssertEqual(store.snapshotCounters().mismatched, 0, "argument errors do not bump mismatched")

        // The handle is still valid: fix the coordinate and the same ref succeeds.
        guard case .success = store.beginInFlight(handle) else {
            return XCTFail("the handle must be restored to live after a coordinate argument error")
        }
    }

    // MARK: - windowID comparison (item 3)

    func testVerifyStoredTargetWindowIDComparison() throws {
        let store = makeStore()
        let service = ComputerUseService(snapshotHandleStore: store)
        let record = mintRecord(in: store, snapshot: makeSnapshot(windowID: 42))
        let identity = record.target.identity

        // Same identity/pid but the captured window is no longer on screen -> changed.
        XCTAssertThrowsError(
            try service.verifyStoredTarget(currentIdentity: identity, currentPID: 1234, currentWindowIDs: [99], record: record)
        ) { error in
            XCTAssertEqual((error as? SnapshotRefError)?.code, .targetChanged)
        }
        // Captured window still present -> ok.
        XCTAssertNoThrow(
            try service.verifyStoredTarget(currentIdentity: identity, currentPID: 1234, currentWindowIDs: [42, 99], record: record)
        )
        // Current side exposes no window ids -> skip the window comparison.
        XCTAssertNoThrow(
            try service.verifyStoredTarget(currentIdentity: identity, currentPID: 1234, currentWindowIDs: [], record: record)
        )
        // Identity / PID mismatch always fails.
        XCTAssertThrowsError(
            try service.verifyStoredTarget(currentIdentity: "com.other.app", currentPID: 1234, currentWindowIDs: [42], record: record)
        )
        XCTAssertThrowsError(
            try service.verifyStoredTarget(currentIdentity: identity, currentPID: 9999, currentWindowIDs: [42], record: record)
        )
    }

    func testVerifyStoredTargetSkipsWhenStoredHasNoWindowID() throws {
        let store = makeStore()
        let service = ComputerUseService(snapshotHandleStore: store)
        let record = mintRecord(in: store, snapshot: makeSnapshot(windowID: nil))
        // Stored side lacks a window id -> skip the comparison even if current has some.
        XCTAssertNoThrow(
            try service.verifyStoredTarget(currentIdentity: record.target.identity, currentPID: 1234, currentWindowIDs: [7, 8], record: record)
        )
    }

    // MARK: - Sweep/evict skip in_flight records (item 4)

    func testExpireSweepSkipsInFlightRecord() throws {
        let clock = FakeClock(Date(timeIntervalSince1970: 2_000_000))
        let store = makeStore(clock: clock)
        let handleA = mintHandle(in: store, windowID: 1)
        guard case .success = store.beginInFlight(handleA) else {
            return XCTFail("A should acquire in_flight")
        }

        clock.advance(SnapshotHandleStoreLimits.ttl)   // A is now past TTL while in_flight
        _ = mintHandle(in: store, windowID: 2)         // triggers a lazy expiry sweep

        // A must NOT have been expired underneath the open transaction: a second use
        // still reports in_use (not expired).
        guard case let .failure(error) = store.beginInFlight(handleA) else {
            return XCTFail("A should remain held")
        }
        XCTAssertEqual(error.code, .inUse, "an in_flight record is never swept underneath its transaction")
    }

    func testEvictSkipsInFlightRecord() throws {
        let clock = FakeClock(Date(timeIntervalSince1970: 3_000_000))
        let store = makeStore(clock: clock)

        // Mint the in_flight target first so it is the least-recently-created (the
        // natural eviction victim) - proving eviction skips it.
        clock.advance(1)
        let inFlight = mintHandle(in: store, windowID: 1)
        guard case .success = store.beginInFlight(inFlight) else {
            return XCTFail("in_flight target should acquire")
        }
        var secondHandle = ""
        for windowID in 2...SnapshotHandleStoreLimits.maxLiveTargets {
            clock.advance(1)
            let handle = mintHandle(in: store, windowID: CGWindowID(windowID))
            if windowID == 2 { secondHandle = handle }
        }
        XCTAssertEqual(store.liveTargetCount(), SnapshotHandleStoreLimits.maxLiveTargets)

        clock.advance(1)
        _ = mintHandle(in: store, windowID: 99)   // 17th target forces one eviction

        // The oldest record is in_flight, so eviction skips it and removes the next
        // oldest (windowID 2) instead. A still-in_flight record re-reports in_use;
        // an evicted one reports expired.
        guard case let .failure(survived) = store.beginInFlight(inFlight) else {
            return XCTFail("in_flight record must survive eviction")
        }
        XCTAssertEqual(survived.code, .inUse, "in_flight record must survive eviction")
        guard case let .failure(evicted) = store.beginInFlight(secondHandle) else {
            return XCTFail("the non-in_flight oldest should be evicted")
        }
        XCTAssertEqual(evicted.code, .expired, "an evicted handle recovers like an expired one")
    }

    // MARK: - Whitespace ref -> missing (item 5)

    func testWhitespaceOnlyRefTreatedAsMissing() throws {
        let store = makeStore()
        let service = ComputerUseService(snapshotHandleStore: store)
        service.modernActionHooksOverride = countingHooks(counter: DispatchCounter())
        let dispatcher = ComputerUseToolDispatcher(service: service)

        for blank in ["   ", "\t", "\n", " \n "] {
            let result = try dispatcher.callTool(name: "click", arguments: ["app": "Example", "snapshot_ref": blank], modern: true)
            XCTAssertEqual(errorCode(result), "snapshot_ref_missing", "blank ref should be missing, not malformed")
        }
    }

    func testSurroundingWhitespaceIsTrimmedFromValidRef() throws {
        let store = makeStore()
        let service = ComputerUseService(snapshotHandleStore: store)
        service.modernActionHooksOverride = countingHooks(counter: DispatchCounter())
        let dispatcher = ComputerUseToolDispatcher(service: service)
        let handle = mintHandle(in: store)

        let result = try dispatcher.callTool(
            name: "click",
            arguments: ["app": "Example", "snapshot_ref": "  \(handle)  ", "x": 10, "y": 10],
            modern: true
        )
        XCTAssertFalse(result.isError, "a valid ref with surrounding whitespace must be trimmed and accepted")
        XCTAssertNotNil(result.structuredContent?["snapshot_ref"] as? String)
    }
}
