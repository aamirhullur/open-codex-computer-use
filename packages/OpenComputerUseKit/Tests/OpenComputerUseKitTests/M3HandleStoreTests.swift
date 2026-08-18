import CoreGraphics
import ImageIO
import XCTest
@testable import OpenComputerUseKit

// M3: bounded snapshot handle store, modern get_app_state minting, and the
// app-agent ownership-hoist seam (shared runtime outlives a connection).
final class M3HandleStoreTests: XCTestCase {

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

    // Always throws, to prove the mint-failure degradation path.
    private struct ThrowingTokenSource: SnapshotTokenSource {
        func nextTokenBytes(count: Int) throws -> [UInt8] {
            throw SnapshotTokenError("no entropy available")
        }
    }

    private final class LogCollector: @unchecked Sendable {
        private let lock = NSLock()
        private(set) var lines: [String] = []
        func sink(_ line: String) { lock.lock(); lines.append(line); lock.unlock() }
    }

    // MARK: - Snapshot construction helpers

    private func makeSnapshot(
        name: String = "Example",
        bundleIdentifier: String? = "com.example.app",
        pid: pid_t = 1234,
        windowID: CGWindowID? = 42,
        windowBounds: CGRect? = CGRect(x: 0, y: 0, width: 1200, height: 800),
        windowLayer: Int? = 0,
        screenshot: Data? = nil,
        mode: SnapshotMode = .accessibility,
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
            windowBounds: windowBounds,
            targetWindowID: windowID,
            targetWindowLayer: windowLayer,
            screenshotPNGData: screenshot,
            mode: mode,
            treeLines: ["[1] Button \"OK\""],
            focusedSummary: nil,
            focusedElement: nil,
            selectedText: nil,
            elements: elements
        )
    }

    private func makePNG(width: Int, height: Int) -> Data {
        let colorSpace = CGColorSpaceCreateDeviceRGB()
        let context = CGContext(
            data: nil,
            width: width,
            height: height,
            bitsPerComponent: 8,
            bytesPerRow: 0,
            space: colorSpace,
            bitmapInfo: CGImageAlphaInfo.premultipliedLast.rawValue
        )!
        context.setFillColor(CGColor(red: 0.15, green: 0.22, blue: 0.35, alpha: 1))
        context.fill(CGRect(x: 0, y: 0, width: width, height: height))
        for row in 0..<12 {
            for column in 0..<16 {
                let shade = CGFloat((row + column) % 5) / 5.0
                context.setFillColor(CGColor(red: shade, green: 0.5, blue: 1 - shade, alpha: 1))
                context.fill(CGRect(x: column * width / 16, y: row * height / 12, width: width / 32, height: height / 24))
            }
        }
        let image = context.makeImage()!
        let data = NSMutableData()
        let destination = CGImageDestinationCreateWithData(data, "public.png" as CFString, 1, nil)!
        CGImageDestinationAddImage(destination, image, nil)
        CGImageDestinationFinalize(destination)
        return data as Data
    }

    private func makeStore(
        clock: SnapshotClock = SystemSnapshotClock(),
        tokenSource: SnapshotTokenSource = SecureRandomTokenSource(),
        logSink: ((String) -> Void)? = nil
    ) -> SnapshotHandleStore {
        SnapshotHandleStore(clock: clock, tokenSource: tokenSource, logSink: logSink)
    }

    private let defaultCaptureOptions = SnapshotCaptureOptions(textLimitMaxCount: 500, maxTreeNodes: 1200, maxTreeDepth: 64)

    // MARK: - Limit constants (item 5)

    func testLimitConstantsArePinned() {
        XCTAssertEqual(SnapshotHandleStoreLimits.ttl, 120)
        XCTAssertEqual(SnapshotHandleStoreLimits.maxLiveTargets, 16)
        XCTAssertEqual(SnapshotHandleStoreLimits.maxTombstones, 64)
        XCTAssertEqual(SnapshotHandleStoreLimits.tokenByteCount, 24)
        XCTAssertEqual(SnapshotHandleStoreLimits.handlePrefix, "ocu_snapshot_v1_")
    }

    // MARK: - Mint / resolve

    func testMintResolveHappyPath() throws {
        let store = makeStore(tokenSource: DeterministicTokenSource())
        let minted = try store.mint(snapshot: makeSnapshot(), screenshotPixels: CGSize(width: 2400, height: 1600), captureOptions: defaultCaptureOptions)

        XCTAssertTrue(minted.handle.hasPrefix("ocu_snapshot_v1_"))
        XCTAssertEqual(minted.handle.count, "ocu_snapshot_v1_".count + 32)
        XCTAssertEqual(minted.generation, 1)

        guard case let .success(record) = store.resolve(minted.handle) else {
            return XCTFail("expected resolve success")
        }
        XCTAssertEqual(record.handle, minted.handle)
        XCTAssertEqual(record.generation, 1)
        XCTAssertEqual(record.screenshotPixels, CGSize(width: 2400, height: 1600))
        XCTAssertEqual(record.lifecycle, .live)

        let counters = store.snapshotCounters()
        XCTAssertEqual(counters.minted, 1)
        XCTAssertEqual(counters.resolved, 1)
    }

    func testHandleIsWellFormedBase64url() throws {
        let store = makeStore(tokenSource: DeterministicTokenSource())
        let handle = try store.mint(snapshot: makeSnapshot(), screenshotPixels: nil, captureOptions: defaultCaptureOptions).handle
        XCTAssertTrue(SnapshotHandleStore.isWellFormed(handle))
        let body = handle.dropFirst("ocu_snapshot_v1_".count)
        XCTAssertFalse(body.contains("="))
        XCTAssertFalse(body.contains("+"))
        XCTAssertFalse(body.contains("/"))
    }

    func testResolveMalformedHandle() {
        let store = makeStore()
        for bad in ["", "nope", "ocu_snapshot_v1_short", "ocu_snapshot_v1_" + String(repeating: "!", count: 32)] {
            guard case let .failure(error) = store.resolve(bad) else {
                return XCTFail("expected failure for \(bad)")
            }
            XCTAssertEqual(error.code, .malformed)
        }
    }

    // MARK: - Expiry at the TTL boundary

    func testExpiryAtExactTTLBoundary() throws {
        let start = Date(timeIntervalSince1970: 1_000_000)
        let clock = FakeClock(start)
        let store = makeStore(clock: clock, tokenSource: DeterministicTokenSource())
        let handle = try store.mint(snapshot: makeSnapshot(), screenshotPixels: nil, captureOptions: defaultCaptureOptions).handle

        clock.advance(119.999)
        guard case .success = store.resolve(handle) else {
            return XCTFail("expected live just before TTL")
        }

        clock.advance(0.001)
        guard case let .failure(error) = store.resolve(handle) else {
            return XCTFail("expected expiry at TTL boundary")
        }
        XCTAssertEqual(error.code, .expired)
    }

    // MARK: - Capacity eviction order

    func testCapacityEvictionLeastRecentlyCreated() throws {
        let clock = FakeClock(Date(timeIntervalSince1970: 2_000_000))
        let store = makeStore(clock: clock, tokenSource: DeterministicTokenSource())

        var handles: [String] = []
        for windowID in 1...SnapshotHandleStoreLimits.maxLiveTargets {
            clock.advance(1)
            let handle = try store.mint(
                snapshot: makeSnapshot(windowID: CGWindowID(windowID)),
                screenshotPixels: nil,
                captureOptions: defaultCaptureOptions
            ).handle
            handles.append(handle)
        }
        XCTAssertEqual(store.liveTargetCount(), SnapshotHandleStoreLimits.maxLiveTargets)

        clock.advance(1)
        let overflow = try store.mint(
            snapshot: makeSnapshot(windowID: CGWindowID(99)),
            screenshotPixels: nil,
            captureOptions: defaultCaptureOptions
        ).handle

        XCTAssertEqual(store.liveTargetCount(), SnapshotHandleStoreLimits.maxLiveTargets)
        XCTAssertEqual(store.snapshotCounters().evicted, 1)

        guard case let .failure(error) = store.resolve(handles[0]) else {
            return XCTFail("expected first handle evicted")
        }
        XCTAssertEqual(error.code, .expired)
        for handle in handles.dropFirst() {
            guard case .success = store.resolve(handle) else {
                return XCTFail("expected remaining handle live")
            }
        }
        guard case .success = store.resolve(overflow) else {
            return XCTFail("expected newest handle live")
        }
    }

    // Fixed (never-advancing) clock: createdAt ties for every record, so the
    // mint-sequence tiebreak must select the earliest-minted victim deterministically.
    func testCapacityEvictionDeterministicUnderFixedClock() throws {
        let clock = FakeClock(Date(timeIntervalSince1970: 5_000_000))
        let store = makeStore(clock: clock, tokenSource: DeterministicTokenSource())

        var handles: [String] = []
        for windowID in 1...SnapshotHandleStoreLimits.maxLiveTargets {
            let handle = try store.mint(
                snapshot: makeSnapshot(windowID: CGWindowID(windowID)),
                screenshotPixels: nil,
                captureOptions: defaultCaptureOptions
            ).handle
            handles.append(handle)
        }

        // 17th target with no clock movement evicts the first-minted (mintSequence 1).
        let overflow = try store.mint(
            snapshot: makeSnapshot(windowID: CGWindowID(99)),
            screenshotPixels: nil,
            captureOptions: defaultCaptureOptions
        ).handle

        XCTAssertEqual(store.liveTargetCount(), SnapshotHandleStoreLimits.maxLiveTargets)
        guard case let .failure(error) = store.resolve(handles[0]) else {
            return XCTFail("expected earliest-minted handle evicted under fixed clock")
        }
        XCTAssertEqual(error.code, .expired)
        for handle in handles.dropFirst() {
            guard case .success = store.resolve(handle) else {
                return XCTFail("expected remaining handle live")
            }
        }
        guard case .success = store.resolve(overflow) else {
            return XCTFail("expected newest handle live")
        }
    }

    // MARK: - Tombstone: stale vs unknown

    func testTombstoneDistinguishesStaleFromUnknown() throws {
        let store = makeStore(tokenSource: DeterministicTokenSource())
        let handle = try store.mint(snapshot: makeSnapshot(), screenshotPixels: nil, captureOptions: defaultCaptureOptions).handle

        XCTAssertTrue(store.supersede(handle))
        guard case let .failure(stale) = store.resolve(handle) else {
            return XCTFail("expected stale for superseded handle")
        }
        XCTAssertEqual(stale.code, .stale)

        let unknownHandle = "ocu_snapshot_v1_" + SnapshotHandleStore.base64url([UInt8](repeating: 7, count: 24))
        guard case let .failure(unknown) = store.resolve(unknownHandle) else {
            return XCTFail("expected unknown for never-minted handle")
        }
        XCTAssertEqual(unknown.code, .unknown)
    }

    func testTombstoneCapEnforced() throws {
        let store = makeStore(tokenSource: DeterministicTokenSource())
        for _ in 0..<(SnapshotHandleStoreLimits.maxTombstones + 6) {
            _ = try store.mint(snapshot: makeSnapshot(), screenshotPixels: nil, captureOptions: defaultCaptureOptions)
        }
        XCTAssertEqual(store.tombstoneCount(), SnapshotHandleStoreLimits.maxTombstones)
        XCTAssertEqual(store.liveTargetCount(), 1)
    }

    // MARK: - Counter accounting (item 1): transition-time, no resolve-time bumps

    func testCountersAccountedAtTransitionTimeNotResolveTime() throws {
        let clock = FakeClock(Date(timeIntervalSince1970: 4_000_000))
        let store = makeStore(clock: clock, tokenSource: DeterministicTokenSource())

        // Supersede bumps stale at transition time (before any resolve).
        let staleHandle = try store.mint(snapshot: makeSnapshot(windowID: 1), screenshotPixels: nil, captureOptions: defaultCaptureOptions).handle
        _ = try store.mint(snapshot: makeSnapshot(windowID: 1), screenshotPixels: nil, captureOptions: defaultCaptureOptions)
        XCTAssertEqual(store.snapshotCounters().stale, 1)

        // Expire bumps expired at transition time (lazy sweep expires every live
        // record past TTL: windowID 1's live gen2 and windowID 2 both count).
        let expiring = try store.mint(snapshot: makeSnapshot(windowID: 2), screenshotPixels: nil, captureOptions: defaultCaptureOptions).handle
        clock.advance(SnapshotHandleStoreLimits.ttl)
        _ = store.resolve(expiring) // triggers lazy expiry -> expired += 2
        XCTAssertEqual(store.snapshotCounters().expired, 2)

        // Resolving failures repeatedly must NOT bump any counter further.
        let before = store.snapshotCounters()
        for _ in 0..<5 {
            _ = store.resolve(staleHandle) // stale tombstone
            _ = store.resolve(expiring)    // expired tombstone
            _ = store.resolve("ocu_snapshot_v1_" + SnapshotHandleStore.base64url([UInt8](repeating: 9, count: 24)))
        }
        let after = store.snapshotCounters()
        XCTAssertEqual(before.stale, after.stale)
        XCTAssertEqual(before.expired, after.expired)
        XCTAssertEqual(before.evicted, after.evicted)
    }

    // MARK: - Redaction

    func testNoFullHandleAppearsInLogsOrErrors() throws {
        let collector = LogCollector()
        let store = makeStore(tokenSource: DeterministicTokenSource(), logSink: collector.sink)

        let handleA = try store.mint(snapshot: makeSnapshot(windowID: 1), screenshotPixels: nil, captureOptions: defaultCaptureOptions).handle
        let handleB = try store.mint(snapshot: makeSnapshot(windowID: 2), screenshotPixels: nil, captureOptions: defaultCaptureOptions).handle
        _ = store.resolve(handleA)
        store.supersede(handleA)
        store.expire(handleB)

        var errorStrings: [String] = []
        if case let .failure(error) = store.resolve(handleA) { errorStrings.append(error.message) }
        if case let .failure(error) = store.resolve(handleB) { errorStrings.append(error.message) }
        if case let .failure(error) = store.resolve("ocu_snapshot_v1_" + SnapshotHandleStore.base64url([UInt8](repeating: 3, count: 24))) {
            errorStrings.append(error.message)
        }

        XCTAssertFalse(collector.lines.isEmpty)
        for handle in [handleA, handleB] {
            for line in collector.lines {
                XCTAssertFalse(line.contains(handle), "log line leaked full handle: \(line)")
            }
            for message in errorStrings {
                XCTAssertFalse(message.contains(handle), "error leaked full handle: \(message)")
            }
            XCTAssertTrue(collector.lines.contains { $0.contains(SnapshotHandleStore.redact(handle)) })
        }
    }

    // MARK: - Restart equivalent

    func testFreshStoreYieldsUnknownForForeignHandle() throws {
        let first = makeStore(tokenSource: DeterministicTokenSource())
        let handle = try first.mint(snapshot: makeSnapshot(), screenshotPixels: nil, captureOptions: defaultCaptureOptions).handle

        let second = makeStore(tokenSource: DeterministicTokenSource())
        guard case let .failure(error) = second.resolve(handle) else {
            return XCTFail("expected unknown after restart")
        }
        XCTAssertEqual(error.code, .unknown)
    }

    // MARK: - One live generation per target

    func testReMintSameTargetSupersedesAndIncrementsGeneration() throws {
        let store = makeStore(tokenSource: DeterministicTokenSource())
        let first = try store.mint(snapshot: makeSnapshot(), screenshotPixels: nil, captureOptions: defaultCaptureOptions)
        let second = try store.mint(snapshot: makeSnapshot(), screenshotPixels: nil, captureOptions: defaultCaptureOptions)

        XCTAssertEqual(first.generation, 1)
        XCTAssertEqual(second.generation, 2)
        XCTAssertEqual(store.liveTargetCount(), 1)

        guard case let .failure(stale) = store.resolve(first.handle) else {
            return XCTFail("expected first generation superseded")
        }
        XCTAssertEqual(stale.code, .stale)
        guard case .success = store.resolve(second.handle) else {
            return XCTFail("expected second generation live")
        }
    }

    // MARK: - Payload release

    func testPayloadReleasedOnSupersedeAndExpiry() throws {
        let clock = FakeClock(Date(timeIntervalSince1970: 3_000_000))
        let store = makeStore(clock: clock, tokenSource: DeterministicTokenSource())
        let png = makePNG(width: 64, height: 64)

        let superseded = try store.mint(snapshot: makeSnapshot(windowID: 1, screenshot: png), screenshotPixels: nil, captureOptions: defaultCaptureOptions)
        XCTAssertNotNil(superseded.record.snapshot)
        store.supersede(superseded.handle)
        XCTAssertNil(superseded.record.snapshot, "screenshot payload must be released on supersede")
        XCTAssertEqual(superseded.record.lifecycle, .superseded)

        let expiring = try store.mint(snapshot: makeSnapshot(windowID: 2, screenshot: png), screenshotPixels: nil, captureOptions: defaultCaptureOptions)
        XCTAssertNotNil(expiring.record.snapshot)
        clock.advance(SnapshotHandleStoreLimits.ttl)
        _ = store.resolve(expiring.handle)
        XCTAssertNil(expiring.record.snapshot, "screenshot payload must be released on expiry")
        XCTAssertEqual(expiring.record.lifecycle, .expired)
    }

    // MARK: - Modern get_app_state wiring

    func testModernGetAppStateMintsStructuredContent() {
        let store = makeStore(tokenSource: DeterministicTokenSource())
        let service = ComputerUseService(snapshotHandleStore: store)
        let png = makePNG(width: 2400, height: 1600)
        let snapshot = makeSnapshot(screenshot: png)

        let result = service.snapshotStateResult(for: snapshot, modern: true, captureOptions: defaultCaptureOptions)

        guard let structured = result.structuredContent else {
            return XCTFail("modern get_app_state must return structuredContent")
        }
        guard let handle = structured["snapshot_ref"] as? String else {
            return XCTFail("structuredContent must carry snapshot_ref")
        }
        XCTAssertTrue(SnapshotHandleStore.isWellFormed(handle))
        XCTAssertEqual(structured["generation"] as? Int, 1)
        XCTAssertTrue(result.primaryText?.hasPrefix("snapshot_ref: \(handle)") ?? false)

        let window = structured["window"] as? [String: Any]
        let pixels = window?["screenshot_pixels"] as? [String: Any]
        XCTAssertEqual(pixels?["width"] as? Int, 2400)
        XCTAssertEqual(pixels?["height"] as? Int, 1600)

        guard case .success = store.resolve(handle) else {
            return XCTFail("minted handle must resolve")
        }
    }

    func testLegacyGetAppStateReturnsNoStructuredContent() {
        let store = makeStore(tokenSource: DeterministicTokenSource())
        let service = ComputerUseService(snapshotHandleStore: store)
        let result = service.snapshotStateResult(for: makeSnapshot(), modern: false, captureOptions: defaultCaptureOptions)
        XCTAssertNil(result.structuredContent)
        XCTAssertEqual(store.snapshotCounters().minted, 0)
    }

    // Mint failure (item 4): the modern path degrades to a plain capture with no
    // structured block and no snapshot_ref text prefix; nothing is minted.
    func testModernGetAppStateDegradesWhenMintFails() {
        let store = makeStore(tokenSource: ThrowingTokenSource())
        let service = ComputerUseService(snapshotHandleStore: store)
        let result = service.snapshotStateResult(for: makeSnapshot(screenshot: makePNG(width: 64, height: 64)), modern: true, captureOptions: defaultCaptureOptions)

        XCTAssertNil(result.structuredContent, "no structured block when mint fails")
        XCTAssertFalse(result.isError, "the capture itself still succeeds")
        XCTAssertFalse(result.primaryText?.contains("snapshot_ref") ?? true, "no snapshot_ref text prefix when mint fails")
        XCTAssertEqual(store.snapshotCounters().minted, 0)
    }

    // MARK: - Ownership-hoist seam (shared runtime outlives a connection)

    func testHandleSurvivesConnectionCloseButNotRuntimeRestart() {
        let store = makeStore(tokenSource: DeterministicTokenSource())
        let sharedService = ComputerUseService(snapshotHandleStore: store)

        var connectionA: StdioMCPServer? = StdioMCPServer(service: sharedService)
        XCTAssertNotNil(connectionA)
        let minted = sharedService.snapshotStateResult(
            for: makeSnapshot(screenshot: makePNG(width: 64, height: 64)),
            modern: true,
            captureOptions: defaultCaptureOptions
        )
        let handle = minted.structuredContent?["snapshot_ref"] as? String
        XCTAssertNotNil(handle)

        connectionA = nil
        _ = connectionA

        let connectionB = StdioMCPServer(service: sharedService)
        XCTAssertNotNil(connectionB)
        guard case .success = sharedService.handleStore.resolve(handle!) else {
            return XCTFail("handle must survive connection close while shared runtime lives")
        }

        let freshService = ComputerUseService(snapshotHandleStore: makeStore(tokenSource: DeterministicTokenSource()))
        guard case let .failure(error) = freshService.handleStore.resolve(handle!) else {
            return XCTFail("fresh runtime must not resolve a foreign handle")
        }
        XCTAssertEqual(error.code, .unknown)
    }

    // MARK: - Concurrency regression (item 6): one shared store, many threads

    func testConcurrentMintAndResolveInvariants() {
        let store = makeStore(tokenSource: SecureRandomTokenSource())
        let sharedService = ComputerUseService(snapshotHandleStore: store)
        // Two adapters over one shared runtime, mirroring two live socket connections.
        let serverA = StdioMCPServer(service: sharedService)
        let serverB = StdioMCPServer(service: sharedService)
        XCTAssertNotNil(serverA)
        XCTAssertNotNil(serverB)

        let threadCount = 8
        let mintsPerThread = 64
        let mintedHandles = NSMutableArray()
        let handlesLock = NSLock()

        DispatchQueue.concurrentPerform(iterations: threadCount) { thread in
            for iteration in 0..<mintsPerThread {
                // Spread across a bounded set of targets so eviction/supersede churn.
                let windowID = CGWindowID((thread * mintsPerThread + iteration) % 24)
                let result = sharedService.snapshotStateResult(
                    for: makeSnapshot(windowID: windowID, screenshot: nil),
                    modern: true,
                    captureOptions: defaultCaptureOptions
                )
                if let handle = result.structuredContent?["snapshot_ref"] as? String {
                    handlesLock.lock()
                    mintedHandles.add(handle)
                    handlesLock.unlock()
                    _ = store.resolve(handle)
                }
            }
        }

        // Invariants: every mint succeeded, live set stayed within the cap, and the
        // minted counter equals the total number of mint calls (no lost updates).
        let totalMints = threadCount * mintsPerThread
        XCTAssertEqual(mintedHandles.count, totalMints)
        XCTAssertLessThanOrEqual(store.liveTargetCount(), SnapshotHandleStoreLimits.maxLiveTargets)
        XCTAssertLessThanOrEqual(store.tombstoneCount(), SnapshotHandleStoreLimits.maxTombstones)
        XCTAssertEqual(store.snapshotCounters().minted, totalMints)
    }

    // MARK: - Fog F2: stored-snapshot memory estimate

    func testFogF2StoredSnapshotMemoryEstimate() throws {
        let png = makePNG(width: 2400, height: 1600)
        let elementCount = 150
        var elements: [Int: ElementRecord] = [:]
        for index in 0..<elementCount {
            elements[index] = ElementRecord(
                index: index,
                identifier: "element-\(index)",
                element: nil,
                localFrame: CGRect(x: 0, y: index * 10, width: 200, height: 24),
                rawActions: ["AXPress"],
                prettyActions: ["Press"]
            )
        }
        let snapshot = makeSnapshot(screenshot: png, elements: elements)

        let store = makeStore(tokenSource: DeterministicTokenSource())
        _ = try store.mint(snapshot: snapshot, screenshotPixels: CGSize(width: 2400, height: 1600), captureOptions: defaultCaptureOptions)

        let screenshotBytes = png.count
        let approxBytesPerElement = 256
        let perSnapshotBytes = screenshotBytes + elementCount * approxBytesPerElement
        let worstCaseAggregate = perSnapshotBytes * SnapshotHandleStoreLimits.maxLiveTargets

        print("F2-measurement: screenshot_png_bytes=\(screenshotBytes) element_count=\(elementCount) per_snapshot_bytes~=\(perSnapshotBytes) worst_case_16_targets_bytes~=\(worstCaseAggregate) (\(worstCaseAggregate / 1_048_576) MB)")

        XCTAssertLessThan(worstCaseAggregate, 256 * 1_048_576)
    }
}
