import CoreGraphics
import XCTest
@testable import OpenComputerUseKit

// M5: CLI --calls batch snapshot_ref threading. Covers the four contract cases:
// (a) an explicit snapshot_ref dispatches that call modern; (b) strict mode forces
// get_app_state and actions modern (an un-threadable action -> snapshot_ref_missing);
// (c) a successor snapshot_ref auto-threads into a later action that omits one; and
// (d) a legacy batch (no refs, not strict) stays modern=false and byte-identical.
//
// End-to-end cases drive runOpenComputerUseCall against a real SnapshotHandleStore
// with the ModernActionHooks seam, so no native input or live capture runs. The
// pure decision is unit-tested directly through OpenComputerUseBatchThreader.
final class M5BatchThreadingTests: XCTestCase {

    // MARK: - Deterministic fakes (mirrors the M4 seams)

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

    private final class DispatchCounter: @unchecked Sendable {
        private let lock = NSLock()
        private(set) var count = 0
        func increment() { lock.lock(); count += 1; lock.unlock() }
        var value: Int { lock.lock(); defer { lock.unlock() }; return count }
    }

    private let defaultCaptureOptions = SnapshotCaptureOptions(textLimitMaxCount: 500, maxTreeNodes: 1200, maxTreeDepth: 64)

    private func makeSnapshot(
        name: String = "Example",
        windowID: CGWindowID? = 42,
        mode: SnapshotMode = .fixture,
        elements: [Int: ElementRecord] = [:]
    ) -> AppSnapshot {
        AppSnapshot(
            app: RunningAppDescriptor(
                name: name,
                bundleIdentifier: "com.example.app",
                pid: 1234,
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

    private func makeStore() -> SnapshotHandleStore {
        SnapshotHandleStore(clock: SystemSnapshotClock(), tokenSource: DeterministicTokenSource())
    }

    private func mintHandle(in store: SnapshotHandleStore) -> String {
        try! store.mint(
            snapshot: makeSnapshot(),
            screenshotPixels: CGSize(width: 1200, height: 800),
            captureOptions: defaultCaptureOptions
        ).handle
    }

    // Hooks that pass precheck, count dispatch, and recapture a same-target snapshot
    // so a successful action mints a resolvable successor into the same store.
    private func countingHooks(counter: DispatchCounter) -> ModernActionHooks {
        ModernActionHooks(
            precheck: { _, _, _ in },
            dispatch: { _, _ in counter.increment() },
            recapture: { _ in self.makeSnapshot() }
        )
    }

    private func makeService(counter: DispatchCounter) -> (ComputerUseService, SnapshotHandleStore) {
        let store = makeStore()
        let service = ComputerUseService(snapshotHandleStore: store)
        service.modernActionHooksOverride = countingHooks(counter: counter)
        return (service, store)
    }

    // Extract the structuredContent.error.code from one sequence output entry.
    private func errorCode(_ entry: [String: Any]) -> String? {
        let result = entry["result"] as? [String: Any]
        let structured = result?["structuredContent"] as? [String: Any]
        return (structured?["error"] as? [String: Any])?["code"] as? String
    }

    private func successorRef(_ entry: [String: Any]) -> String? {
        let result = entry["result"] as? [String: Any]
        let structured = result?["structuredContent"] as? [String: Any]
        return structured?["snapshot_ref"] as? String
    }

    private func isError(_ entry: [String: Any]) -> Bool {
        (entry["result"] as? [String: Any])?["isError"] as? Bool ?? false
    }

    // MARK: - (a) explicit snapshot_ref dispatches modern

    func testExplicitSnapshotRefDispatchesSingleCallModern() throws {
        let counter = DispatchCounter()
        let (service, store) = makeService(counter: counter)
        let handle = mintHandle(in: store)

        let calls = "[{\"tool\":\"click\",\"args\":{\"app\":\"Example\",\"snapshot_ref\":\"\(handle)\",\"x\":10,\"y\":10}}]"
        let output = try runOpenComputerUseCall(
            .sequence(callsJSON: calls, callsFile: nil, interCallDelay: 0),
            service: service,
            strictSnapshots: false
        )

        let outputs = try XCTUnwrap(output.jsonObject as? [[String: Any]])
        XCTAssertEqual(outputs.count, 1)
        XCTAssertFalse(isError(outputs[0]), "an explicit valid ref must dispatch the modern transaction")
        XCTAssertNotNil(successorRef(outputs[0]), "a modern action returns a successor snapshot_ref")
        XCTAssertEqual(counter.value, 1, "exactly one native dispatch")
    }

    // MARK: - (c) auto-thread the successor into a later action that omits the ref

    func testSequenceAutoThreadsSuccessorRefIntoLaterActions() throws {
        let counter = DispatchCounter()
        let (service, store) = makeService(counter: counter)
        let handle = mintHandle(in: store)   // generation 1

        // First click carries the explicit ref (case a). The second and third omit
        // it and must inherit the latest successor (case c).
        let calls = """
        [
          {"tool":"click","args":{"app":"Example","snapshot_ref":"\(handle)","x":10,"y":10}},
          {"tool":"type_text","args":{"app":"Example","text":"hi"}},
          {"tool":"click","args":{"app":"Example","x":20,"y":20}}
        ]
        """
        let output = try runOpenComputerUseCall(
            .sequence(callsJSON: calls, callsFile: nil, interCallDelay: 0),
            service: service,
            strictSnapshots: false
        )

        let outputs = try XCTUnwrap(output.jsonObject as? [[String: Any]])
        XCTAssertEqual(outputs.count, 3)
        XCTAssertFalse(output.hasToolError)
        XCTAssertEqual(counter.value, 3, "every call dispatched a modern action")

        let ref1 = try XCTUnwrap(successorRef(outputs[0]))
        let ref2 = try XCTUnwrap(successorRef(outputs[1]))
        let ref3 = try XCTUnwrap(successorRef(outputs[2]))
        XCTAssertNotEqual(ref1, ref2, "each action supersedes and mints a fresh successor")
        XCTAssertNotEqual(ref2, ref3)

        // The chain stayed current: only the final successor resolves live; the
        // earlier refs were superseded as the batch threaded forward.
        guard case .success = store.resolve(ref3) else {
            return XCTFail("the final successor must resolve live")
        }
        guard case let .failure(stale) = store.resolve(ref1) else {
            return XCTFail("the first ref must have been superseded")
        }
        XCTAssertEqual(stale.code, .stale)
    }

    // MARK: - (b) strict mode

    func testStrictModeActionWithoutRefReturnsMissing() throws {
        let counter = DispatchCounter()
        let (service, _) = makeService(counter: counter)

        // No prior successor and no explicit ref: strict forces the action modern, so
        // the transaction surfaces the pinned snapshot_ref_missing message.
        let calls = "[{\"tool\":\"click\",\"args\":{\"app\":\"Example\",\"x\":10,\"y\":10}}]"
        let output = try runOpenComputerUseCall(
            .sequence(callsJSON: calls, callsFile: nil, interCallDelay: 0),
            service: service,
            strictSnapshots: true
        )

        let outputs = try XCTUnwrap(output.jsonObject as? [[String: Any]])
        XCTAssertEqual(outputs.count, 1)
        XCTAssertTrue(output.hasToolError)
        XCTAssertEqual(errorCode(outputs[0]), "snapshot_ref_missing")
        let text = ((outputs[0]["result"] as? [String: Any])?["content"] as? [[String: Any]])?.first?["text"] as? String
        XCTAssertEqual(text, SnapshotRefMessages.missing)
        XCTAssertEqual(counter.value, 0, "no native dispatch on a missing ref")
    }

    // MARK: - (d) legacy batch stays modern=false and byte-identical

    func testLegacyBatchStaysLegacyWithoutRefsOrStrict() throws {
        let counter = DispatchCounter()
        let (service, store) = makeService(counter: counter)

        // list_apps then a legacy click that fails argument validation (accessibility
        // method without element_index) BEFORE any capture. A legacy click must not
        // emit snapshot_ref_missing and must not carry a structuredContent envelope.
        let calls = """
        [
          {"tool":"list_apps"},
          {"tool":"click","args":{"app":"Example","click_method":"accessibility"}}
        ]
        """
        let output = try runOpenComputerUseCall(
            .sequence(callsJSON: calls, callsFile: nil, interCallDelay: 0),
            service: service,
            strictSnapshots: false
        )

        let outputs = try XCTUnwrap(output.jsonObject as? [[String: Any]])
        XCTAssertEqual(outputs.count, 2)
        XCTAssertTrue(output.hasToolError)
        XCTAssertTrue(isError(outputs[1]))
        XCTAssertNotEqual(errorCode(outputs[1]), "snapshot_ref_missing", "legacy click must not require a snapshot_ref")
        XCTAssertNil((outputs[1]["result"] as? [String: Any])?["structuredContent"], "legacy errors carry no structuredContent")
        XCTAssertEqual(counter.value, 0, "no modern dispatch in a legacy batch")
        let counters = store.snapshotCounters()
        XCTAssertEqual(counters.minted, 0, "a legacy batch mints nothing")
        XCTAssertEqual(counters.resolved, 0)
    }

    func testLegacyBatchOutputIsByteIdenticalToPreThreading() throws {
        // Two list_apps calls: the threader must not alter a fully legacy batch. The
        // output is stable and identical to the pre-M5 behavior (no structuredContent,
        // no threading side effects).
        let output = try runOpenComputerUseCall(
            .sequence(
                callsJSON: "[{\"tool\":\"list_apps\"},{\"tool\":\"list_apps\"}]",
                callsFile: nil,
                interCallDelay: 0
            ),
            strictSnapshots: false
        )
        let outputs = try XCTUnwrap(output.jsonObject as? [[String: Any]])
        XCTAssertEqual(outputs.count, 2)
        XCTAssertFalse(output.hasToolError)
        for entry in outputs {
            XCTAssertEqual(entry["tool"] as? String, "list_apps")
            XCTAssertNil((entry["result"] as? [String: Any])?["structuredContent"])
        }
    }

    // MARK: - Single-call path (runOpenComputerUseCall .single routes through the threader)

    func testSingleExplicitSnapshotRefDispatchesModern() throws {
        let counter = DispatchCounter()
        let (service, store) = makeService(counter: counter)
        let handle = mintHandle(in: store)

        let args = "{\"app\":\"Example\",\"snapshot_ref\":\"\(handle)\",\"x\":10,\"y\":10}"
        let output = try runOpenComputerUseCall(
            .single(toolName: "click", argumentsJSON: args, argumentsFile: nil),
            service: service,
            strictSnapshots: false
        )

        // The single-call output is the result dictionary itself, not a sequence array.
        let result = try XCTUnwrap(output.jsonObject as? [String: Any])
        XCTAssertFalse(output.hasToolError)
        XCTAssertEqual(result["isError"] as? Bool, false)
        let structured = try XCTUnwrap(result["structuredContent"] as? [String: Any], "an explicit ref must dispatch the modern transaction")
        XCTAssertNotNil(structured["snapshot_ref"] as? String, "a modern action returns a successor snapshot_ref")
        XCTAssertEqual(counter.value, 1, "exactly one native dispatch")
    }

    func testSingleStrictActionWithoutRefReturnsMissingZeroDispatch() throws {
        let counter = DispatchCounter()
        let (service, store) = makeService(counter: counter)

        let args = "{\"app\":\"Example\",\"x\":10,\"y\":10}"
        let output = try runOpenComputerUseCall(
            .single(toolName: "click", argumentsJSON: args, argumentsFile: nil),
            service: service,
            strictSnapshots: true
        )

        let result = try XCTUnwrap(output.jsonObject as? [String: Any])
        XCTAssertTrue(output.hasToolError)
        let structured = try XCTUnwrap(result["structuredContent"] as? [String: Any])
        XCTAssertEqual((structured["error"] as? [String: Any])?["code"] as? String, "snapshot_ref_missing")
        let text = (result["content"] as? [[String: Any]])?.first?["text"] as? String
        XCTAssertEqual(text, SnapshotRefMessages.missing, "strict single action without a ref surfaces the pinned missing message")
        XCTAssertEqual(counter.value, 0, "no native dispatch on a missing ref")
        XCTAssertEqual(store.snapshotCounters().minted, 0, "a missing-ref action mints nothing")
    }

    func testSingleLegacyCallStaysByteIdentical() throws {
        let counter = DispatchCounter()
        let (service, store) = makeService(counter: counter)

        // A legacy click that fails argument validation (accessibility method without
        // element_index) BEFORE any capture. No ref, no strict env: modern=false.
        let args = "{\"app\":\"Example\",\"click_method\":\"accessibility\"}"
        let output = try runOpenComputerUseCall(
            .single(toolName: "click", argumentsJSON: args, argumentsFile: nil),
            service: service,
            strictSnapshots: false
        )

        let result = try XCTUnwrap(output.jsonObject as? [String: Any])
        XCTAssertTrue(output.hasToolError)
        XCTAssertEqual(result["isError"] as? Bool, true)
        XCTAssertNil(result["structuredContent"], "a legacy single call carries no structuredContent")
        let text = (result["content"] as? [[String: Any]])?.first?["text"] as? String
        XCTAssertNotEqual(text, SnapshotRefMessages.missing, "legacy click must not require a snapshot_ref")

        // Byte-identical: the routed single-call output equals a direct legacy dispatch.
        let expected = ComputerUseToolDispatcher(service: ComputerUseService(snapshotHandleStore: makeStore()))
            .callToolAsResult(name: "click", arguments: ["app": "Example", "click_method": "accessibility"])
        XCTAssertEqual(text, expected.primaryText)
        XCTAssertEqual(result["isError"] as? Bool, expected.isError)
        XCTAssertNil(expected.structuredContent)

        XCTAssertEqual(counter.value, 0, "no modern dispatch in a legacy single call")
        let counters = store.snapshotCounters()
        XCTAssertEqual(counters.minted, 0, "a legacy single call mints nothing")
        XCTAssertEqual(counters.resolved, 0)
    }

    // MARK: - Pure decision (OpenComputerUseBatchThreader)

    func testThreaderDecisionMatrix() {
        // Non-strict: get_app_state and a ref-less action stay legacy; list_apps legacy.
        var legacy = OpenComputerUseBatchThreader(strict: false)
        XCTAssertFalse(legacy.plan(tool: "get_app_state", arguments: ["app": "A"]).modern)
        XCTAssertFalse(legacy.plan(tool: "click", arguments: ["app": "A"]).modern)
        XCTAssertFalse(legacy.plan(tool: "list_apps", arguments: [:]).modern)

        // (a) An explicit ref forces modern even when not strict.
        let explicit = legacy.plan(tool: "click", arguments: ["app": "A", "snapshot_ref": "ocu_x"])
        XCTAssertTrue(explicit.modern)

        // Strict: get_app_state and actions modern; list_apps stays legacy.
        let strict = OpenComputerUseBatchThreader(strict: true)
        XCTAssertTrue(strict.plan(tool: "get_app_state", arguments: ["app": "A"]).modern)
        XCTAssertTrue(strict.plan(tool: "click", arguments: ["app": "A"]).modern)
        XCTAssertFalse(strict.plan(tool: "list_apps", arguments: [:]).modern, "list_apps never takes a handle")
    }

    func testThreaderAutoFillsRecordedSuccessor() {
        var threader = OpenComputerUseBatchThreader(strict: false)

        // Before any successor, a ref-less action is legacy.
        XCTAssertFalse(threader.plan(tool: "click", arguments: ["app": "A"]).modern)

        // Record a get_app_state style successor, then a ref-less action inherits it.
        let minted = ToolCallResult(
            content: [.text("snapshot_ref: ocu_snapshot_v1_abc")],
            structuredContent: ["snapshot_ref": "ocu_snapshot_v1_abc"]
        )
        threader.record(result: minted)
        XCTAssertEqual(threader.latestSnapshotRef, "ocu_snapshot_v1_abc")

        let plan = threader.plan(tool: "type_text", arguments: ["app": "A", "text": "hi"])
        XCTAssertTrue(plan.modern)
        XCTAssertEqual(plan.arguments["snapshot_ref"] as? String, "ocu_snapshot_v1_abc")
    }

    func testThreaderErrorResultDoesNotOverwriteLatestRef() {
        var threader = OpenComputerUseBatchThreader(strict: false)
        let minted = ToolCallResult(
            content: [.text("ok")],
            structuredContent: ["snapshot_ref": "ocu_snapshot_v1_keep"]
        )
        threader.record(result: minted)

        // A subsequent error result (structuredContent.error, no snapshot_ref) must
        // leave the latest successor intact so the next action can still thread.
        let failure = SnapshotRefError.make(.stale).toToolCallResult()
        threader.record(result: failure)
        XCTAssertEqual(threader.latestSnapshotRef, "ocu_snapshot_v1_keep")
    }

    func testStrictEnvGateTruthiness() {
        for truthy in ["1", "true", "TRUE", "yes", "on", " On "] {
            XCTAssertTrue(openComputerUseStrictSnapshotsEnabled(["OPEN_COMPUTER_USE_STRICT_SNAPSHOTS": truthy]), truthy)
        }
        for falsy in ["0", "false", "no", "off", "", "  "] {
            XCTAssertFalse(openComputerUseStrictSnapshotsEnabled(["OPEN_COMPUTER_USE_STRICT_SNAPSHOTS": falsy]), falsy)
        }
        XCTAssertFalse(openComputerUseStrictSnapshotsEnabled([:]), "absent -> disabled")
    }
}
