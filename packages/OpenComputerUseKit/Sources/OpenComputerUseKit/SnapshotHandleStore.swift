import CoreGraphics
import Foundation
import Security

// Bounded, in-memory snapshot handle store. Mints opaque snapshot_ref handles for
// captured app state, resolves them into stored records, and enforces TTL,
// per-target single-live-generation, capacity, and tombstone limits. The store
// owns the snapshot payload (screenshot bytes plus native element references) and
// releases it on expiry or supersede so private UI content never outlives the
// handle. Handles carry no user data; they are capability references only.
//
// M3 mints on modern get_app_state and resolves for tests and the runtime seam.
// Action-side consumption (validate/supersede on dispatch) lands in M4.

// Injectable time source so tests can drive TTL boundaries deterministically.
public protocol SnapshotClock: Sendable {
    func now() -> Date
}

public struct SystemSnapshotClock: SnapshotClock {
    public init() {}
    public func now() -> Date { Date() }
}

// Raised when a token source cannot produce random bytes. The modern
// get_app_state path degrades gracefully rather than mint a bogus handle.
public struct SnapshotTokenError: Error {
    public let message: String
    public init(_ message: String) { self.message = message }
}

// Injectable token source. Production draws cryptographically random bytes; tests
// supply a deterministic sequence. Throwing so a source that cannot produce
// entropy fails the mint instead of yielding a predictable handle.
public protocol SnapshotTokenSource: Sendable {
    func nextTokenBytes(count: Int) throws -> [UInt8]
}

public struct SecureRandomTokenSource: SnapshotTokenSource {
    public init() {}

    public func nextTokenBytes(count: Int) throws -> [UInt8] {
        var bytes = [UInt8](repeating: 0, count: count)
        let status = SecRandomCopyBytes(kSecRandomDefault, count, &bytes)
        guard status == errSecSuccess else {
            // Never fall back to a predictable source; surface the failure so the
            // caller delivers the capture with no snapshot_ref.
            throw SnapshotTokenError("SecRandomCopyBytes failed with status \(status)")
        }
        return bytes
    }
}

// Tested constants. Not user configuration in the first release.
public enum SnapshotHandleStoreLimits {
    public static let handlePrefix = "ocu_snapshot_v1_"
    public static let tokenByteCount = 24
    public static let ttl: TimeInterval = 120
    public static let maxLiveTargets = 16
    public static let maxTombstones = 64
}

public enum SnapshotLifecycle: String {
    case live
    case inFlight = "in_flight"
    case superseded
    case expired
}

public enum SnapshotTombstoneReason: String {
    case expired
    case superseded
    case evicted
}

public enum SnapshotStoreMode: String {
    case real
    case fixture
}

// Capture parameters recorded for observability and future revalidation.
public struct SnapshotCaptureOptions: Sendable {
    public let textLimitMaxCount: Int?
    public let maxTreeNodes: Int
    public let maxTreeDepth: Int

    public init(textLimitMaxCount: Int?, maxTreeNodes: Int, maxTreeDepth: Int) {
        self.textLimitMaxCount = textLimitMaxCount
        self.maxTreeNodes = maxTreeNodes
        self.maxTreeDepth = maxTreeDepth
    }
}

// Normalized app identity. targetIdentity is the stable key used for the
// per-target single-live-generation rule; a display-name alias may vary but the
// resolved identity may not.
public struct SnapshotAppIdentity: Hashable {
    public let normalizedName: String
    public let bundleIdentifier: String?
    public let executableIdentity: String?
    public let pid: pid_t

    public var targetIdentity: String {
        if let bundleIdentifier, !bundleIdentifier.isEmpty {
            return bundleIdentifier.lowercased()
        }
        if let executableIdentity, !executableIdentity.isEmpty {
            return executableIdentity.lowercased()
        }
        return normalizedName.lowercased()
    }
}

// App identity plus stable window id. Generation is monotonic per target.
public struct SnapshotTargetKey: Hashable {
    public let identity: String
    public let windowID: UInt32?
}

// One stored live handle. The snapshot payload is released on expiry or supersede;
// the lightweight metadata (dims, bounds, generation) survives for correlation.
public final class SnapshotRecord: @unchecked Sendable {
    public let handle: String
    public let target: SnapshotTargetKey
    public let createdAt: Date
    public let expiresAt: Date
    public let generation: Int
    public let app: SnapshotAppIdentity
    public let windowID: UInt32?
    public let windowBounds: CGRect?
    // Screenshot pixel dimensions STORED at capture, never re-derived on resolve.
    public let screenshotPixels: CGSize?
    public let windowLayer: Int?
    public let mode: SnapshotStoreMode
    public let captureOptions: SnapshotCaptureOptions
    // Monotonic mint order across the store. Used as a deterministic secondary key
    // for least-recently-created eviction when createdAt ties (fixed-clock case).
    public let mintSequence: Int

    public private(set) var lifecycle: SnapshotLifecycle
    // The captured payload. AppSnapshot holds the screenshot Data and the element
    // records with their native AXUIElement references; dropping it releases both.
    public private(set) var snapshot: AppSnapshot?

    public var redactedHandle: String { SnapshotHandleStore.redact(handle) }

    init(
        handle: String,
        target: SnapshotTargetKey,
        createdAt: Date,
        expiresAt: Date,
        generation: Int,
        app: SnapshotAppIdentity,
        windowID: UInt32?,
        windowBounds: CGRect?,
        screenshotPixels: CGSize?,
        windowLayer: Int?,
        mode: SnapshotStoreMode,
        captureOptions: SnapshotCaptureOptions,
        mintSequence: Int,
        snapshot: AppSnapshot
    ) {
        self.handle = handle
        self.target = target
        self.createdAt = createdAt
        self.expiresAt = expiresAt
        self.generation = generation
        self.app = app
        self.windowID = windowID
        self.windowBounds = windowBounds
        self.screenshotPixels = screenshotPixels
        self.windowLayer = windowLayer
        self.mode = mode
        self.captureOptions = captureOptions
        self.mintSequence = mintSequence
        self.lifecycle = .live
        self.snapshot = snapshot
    }

    func setLifecycle(_ lifecycle: SnapshotLifecycle) {
        self.lifecycle = lifecycle
    }

    // Release the screenshot bytes and native element references. Called under the
    // store lock when a record leaves the live set.
    func releasePayload() {
        snapshot = nil
    }
}

public struct SnapshotTombstone {
    // Full handle retained only for exact matching on resolve; only the redacted
    // suffix is ever logged or surfaced. No snapshot payload is retained.
    let handle: String
    public let reason: SnapshotTombstoneReason
    public let createdAt: Date
    public let tombstonedAt: Date

    public var suffix: String { SnapshotHandleStore.redact(handle) }
}

public struct SnapshotStoreCounters: Equatable {
    public var minted = 0
    public var resolved = 0
    public var expired = 0
    public var stale = 0
    public var evicted = 0
}

public struct MintedSnapshot {
    public let handle: String
    public let capturedAt: Date
    public let expiresAt: Date
    public let generation: Int
    public let record: SnapshotRecord
}

public final class SnapshotHandleStore: @unchecked Sendable {
    private let lock = NSLock()
    private let clock: SnapshotClock
    private let tokenSource: SnapshotTokenSource
    // Redacted structured lines only. Defaults to gated stderr; tests inject a sink.
    private let logSink: (String) -> Void

    // Live handles by handle string and by target. liveByTarget enforces the
    // single-live-generation rule. generations is monotonic per target and
    // survives supersede/evict so a re-minted target never reuses a generation.
    private var liveRecords: [String: SnapshotRecord] = [:]
    private var liveByTarget: [SnapshotTargetKey: SnapshotRecord] = [:]
    private var tombstones: [SnapshotTombstone] = []
    private var generations: [SnapshotTargetKey: Int] = [:]
    private var counters = SnapshotStoreCounters()
    // Monotonic mint order for the eviction tiebreak; never reset.
    private var mintSequenceCounter = 0

    public init(
        clock: SnapshotClock = SystemSnapshotClock(),
        tokenSource: SnapshotTokenSource = SecureRandomTokenSource(),
        logSink: ((String) -> Void)? = nil
    ) {
        self.clock = clock
        self.tokenSource = tokenSource
        self.logSink = logSink ?? SnapshotHandleStore.defaultLogSink
    }

    // MARK: - Minting

    public func mint(
        snapshot: AppSnapshot,
        screenshotPixels: CGSize?,
        captureOptions: SnapshotCaptureOptions
    ) throws -> MintedSnapshot {
        lock.lock()
        defer { lock.unlock() }

        let now = clock.now()
        expireElapsedLocked(now: now)

        let identity = SnapshotHandleStore.identity(for: snapshot)
        let windowID = snapshot.targetWindowID
        let target = SnapshotTargetKey(identity: identity.targetIdentity, windowID: windowID)

        // Draw the handle before mutating live state so a token failure leaves the
        // store untouched (no orphaned supersede, no consumed generation).
        let handle = try mintHandleLocked()

        // Single live generation per target: supersede any existing live record for
        // this target before inserting the successor.
        if let existing = liveByTarget[target] {
            retireLocked(existing, reason: .superseded, now: now)
        }

        // Capacity applies only when adding a NEW target beyond the live cap.
        if liveByTarget[target] == nil, liveByTarget.count >= SnapshotHandleStoreLimits.maxLiveTargets {
            evictOneLocked(now: now)
        }

        let generation = (generations[target] ?? 0) + 1
        generations[target] = generation

        mintSequenceCounter += 1
        let expiresAt = now.addingTimeInterval(SnapshotHandleStoreLimits.ttl)

        let record = SnapshotRecord(
            handle: handle,
            target: target,
            createdAt: now,
            expiresAt: expiresAt,
            generation: generation,
            app: identity,
            windowID: windowID,
            windowBounds: snapshot.windowBounds,
            screenshotPixels: screenshotPixels,
            windowLayer: snapshot.targetWindowLayer,
            mode: snapshot.mode == .fixture ? .fixture : .real,
            captureOptions: captureOptions,
            mintSequence: mintSequenceCounter,
            snapshot: snapshot
        )

        liveRecords[handle] = record
        liveByTarget[target] = record
        counters.minted += 1
        log("minted", handle: handle, extra: "generation=\(generation) live=\(liveByTarget.count)")

        return MintedSnapshot(
            handle: handle,
            capturedAt: now,
            expiresAt: expiresAt,
            generation: generation,
            record: record
        )
    }

    // MARK: - Resolution

    public func resolve(_ handle: String) -> Result<SnapshotRecord, SnapshotRefError> {
        lock.lock()
        defer { lock.unlock() }

        guard SnapshotHandleStore.isWellFormed(handle) else {
            return .failure(.make(.malformed))
        }

        let now = clock.now()
        expireElapsedLocked(now: now)

        // Constant-time scan of the live set (bounded by maxLiveTargets) so a
        // near-miss handle is not confirmed via dictionary timing.
        if let record = lookupLiveLocked(handle) {
            counters.resolved += 1
            log("resolved", handle: handle, extra: "generation=\(record.generation)")
            return .success(record)
        }

        // Failure classes do not bump lifecycle counters; those are accounted at
        // transition time (supersede/expire/evict) to match the design's
        // observability model and the Go store.
        if let tombstone = tombstoneMatchingLocked(handle) {
            switch tombstone.reason {
            case .superseded:
                log("stale", handle: handle, extra: "reason=superseded")
                return .failure(.make(.stale))
            case .expired:
                log("resolve-failed", handle: handle, extra: "reason=expired")
                return .failure(.make(.expired))
            case .evicted:
                // Evicted for capacity; the recovery path is identical to expiry.
                log("resolve-failed", handle: handle, extra: "reason=evicted")
                return .failure(.make(.expired))
            }
        }

        log("unknown", handle: handle, extra: nil)
        return .failure(.make(.unknown))
    }

    // MARK: - Lifecycle transitions (used directly by tests; M4 wires action paths)

    @discardableResult
    public func supersede(_ handle: String) -> Bool {
        lock.lock()
        defer { lock.unlock() }
        guard let record = liveRecords[handle] else { return false }
        retireLocked(record, reason: .superseded, now: clock.now())
        return true
    }

    @discardableResult
    public func expire(_ handle: String) -> Bool {
        lock.lock()
        defer { lock.unlock() }
        guard let record = liveRecords[handle] else { return false }
        retireLocked(record, reason: .expired, now: clock.now())
        return true
    }

    public func snapshotCounters() -> SnapshotStoreCounters {
        lock.lock()
        defer { lock.unlock() }
        return counters
    }

    public func liveTargetCount() -> Int {
        lock.lock()
        defer { lock.unlock() }
        return liveByTarget.count
    }

    public func tombstoneCount() -> Int {
        lock.lock()
        defer { lock.unlock() }
        return tombstones.count
    }

    // MARK: - Locked helpers

    private func mintHandleLocked() throws -> String {
        // Bounded retries so a degenerate token source cannot spin forever.
        for _ in 0..<8 {
            let bytes = try tokenSource.nextTokenBytes(count: SnapshotHandleStoreLimits.tokenByteCount)
            guard bytes.count == SnapshotHandleStoreLimits.tokenByteCount else {
                throw SnapshotTokenError("token source returned \(bytes.count) bytes, expected \(SnapshotHandleStoreLimits.tokenByteCount)")
            }
            let handle = SnapshotHandleStoreLimits.handlePrefix + SnapshotHandleStore.base64url(bytes)
            if liveRecords[handle] == nil {
                return handle
            }
        }
        throw SnapshotTokenError("token source repeatedly collided with a live handle")
    }

    // Constant-time membership test over the live set. Returns the matching live
    // record, if any, without leaking a near-miss through dictionary lookup timing.
    private func lookupLiveLocked(_ handle: String) -> SnapshotRecord? {
        var match: SnapshotRecord?
        for record in liveRecords.values
        where SnapshotHandleStore.constantTimeEquals(record.handle, handle) {
            match = record
        }
        return match
    }

    private func expireElapsedLocked(now: Date) {
        let expiredHandles = liveRecords.compactMap { key, record in
            now >= record.expiresAt ? key : nil
        }
        for handle in expiredHandles {
            guard let record = liveRecords[handle] else { continue }
            retireLocked(record, reason: .expired, now: now)
        }
    }

    private func evictOneLocked(now: Date) {
        // Expired-first is already handled by expireElapsedLocked before this call;
        // remaining live records are unexpired, so evict least-recently-created.
        // mintSequence breaks createdAt ties deterministically (fixed-clock case).
        guard let victim = liveByTarget.values.min(by: { lhs, rhs in
            if lhs.createdAt != rhs.createdAt {
                return lhs.createdAt < rhs.createdAt
            }
            return lhs.mintSequence < rhs.mintSequence
        }) else {
            return
        }
        retireLocked(victim, reason: .evicted, now: now)
    }

    private func retireLocked(_ record: SnapshotRecord, reason: SnapshotTombstoneReason, now: Date) {
        liveRecords.removeValue(forKey: record.handle)
        if liveByTarget[record.target]?.handle == record.handle {
            liveByTarget.removeValue(forKey: record.target)
        }
        record.setLifecycle(reason == .superseded ? .superseded : .expired)
        record.releasePayload()

        tombstones.append(
            SnapshotTombstone(
                handle: record.handle,
                reason: reason,
                createdAt: record.createdAt,
                tombstonedAt: now
            )
        )
        // Transition-time accounting: each retirement bumps exactly one lifecycle
        // counter here, never again at resolve time.
        switch reason {
        case .superseded:
            counters.stale += 1
        case .expired:
            counters.expired += 1
        case .evicted:
            counters.evicted += 1
        }
        enforceTombstoneCapLocked()
        log(reason.rawValue, handle: record.handle, extra: "retired")
    }

    private func enforceTombstoneCapLocked() {
        guard tombstones.count > SnapshotHandleStoreLimits.maxTombstones else { return }
        let overflow = tombstones.count - SnapshotHandleStoreLimits.maxTombstones
        tombstones.removeFirst(overflow)
    }

    private func tombstoneMatchingLocked(_ handle: String) -> SnapshotTombstone? {
        // Newest tombstone wins if a handle somehow recurs; scan back to front with
        // constant-time comparison.
        for tombstone in tombstones.reversed()
        where SnapshotHandleStore.constantTimeEquals(tombstone.handle, handle) {
            return tombstone
        }
        return nil
    }

    private func log(_ event: String, handle: String, extra: String?) {
        var line = "snapshot-store event=\(event) handle=\(SnapshotHandleStore.redact(handle))"
        if let extra {
            line += " \(extra)"
        }
        line += " counters=minted:\(counters.minted),resolved:\(counters.resolved),expired:\(counters.expired),stale:\(counters.stale),evicted:\(counters.evicted)"
        logSink(line)
    }

    // MARK: - Static helpers

    static func identity(for snapshot: AppSnapshot) -> SnapshotAppIdentity {
        SnapshotAppIdentity(
            normalizedName: snapshot.app.name,
            bundleIdentifier: snapshot.app.bundleIdentifier,
            executableIdentity: snapshot.app.runningApplication.executableURL?.standardizedFileURL.path,
            pid: snapshot.app.pid
        )
    }

    // Redacted form for logs and errors: fixed prefix plus the final six characters
    // only. The unguessable middle is never emitted.
    public static func redact(_ handle: String) -> String {
        let last6 = String(handle.suffix(6))
        return SnapshotHandleStoreLimits.handlePrefix + "..." + last6
    }

    static func isWellFormed(_ handle: String) -> Bool {
        guard handle.hasPrefix(SnapshotHandleStoreLimits.handlePrefix) else { return false }
        let body = handle.dropFirst(SnapshotHandleStoreLimits.handlePrefix.count)
        // 24 bytes base64url with no padding is exactly 32 characters.
        guard body.count == 32 else { return false }
        let allowed = Set("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_")
        return body.allSatisfy { allowed.contains($0) }
    }

    static func base64url(_ bytes: [UInt8]) -> String {
        Data(bytes).base64EncodedString()
            .replacingOccurrences(of: "+", with: "-")
            .replacingOccurrences(of: "/", with: "_")
            .replacingOccurrences(of: "=", with: "")
    }

    // Length-independent comparison so resolve does not leak a near-miss via timing.
    static func constantTimeEquals(_ lhs: String, _ rhs: String) -> Bool {
        let lhsBytes = Array(lhs.utf8)
        let rhsBytes = Array(rhs.utf8)
        var difference = lhsBytes.count ^ rhsBytes.count
        let count = Swift.max(lhsBytes.count, rhsBytes.count)
        var index = 0
        while index < count {
            let left = index < lhsBytes.count ? Int(lhsBytes[index]) : 0
            let right = index < rhsBytes.count ? Int(rhsBytes[index]) : 0
            difference |= left ^ right
            index += 1
        }
        return difference == 0
    }

    private static func defaultLogSink(_ line: String) -> Void {
        guard ProcessInfo.processInfo.environment["OPEN_COMPUTER_USE_DEBUG_INPUT_FALLBACKS"] != nil else { return }
        FileHandle.standardError.write(Data((line + "\n").utf8))
    }
}
