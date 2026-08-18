import CoreGraphics
import Foundation

// Builder for the modern get_app_state structured payload. In M2 this is an
// internal seam: it is fed injected handle/timestamp/generation values and is
// not yet driven by a real handle store (M3 wires minting). The shape is the
// pinned cross-platform contract; keys and nullability match every platform.
enum SnapshotStructuredContent {
    // RFC3339 UTC, second precision with a trailing Z (matches the design doc's
    // captured_at/expires_at examples).
    static func rfc3339UTC(_ date: Date) -> String {
        let formatter = ISO8601DateFormatter()
        formatter.timeZone = TimeZone(identifier: "UTC")
        formatter.formatOptions = [.withInternetDateTime]
        return formatter.string(from: date)
    }

    // The structuredContent dictionary for a modern get_app_state result.
    static func build(
        snapshot: AppSnapshot,
        snapshotRef: String,
        capturedAt: Date,
        expiresAt: Date,
        generation: Int,
        screenshotPixels: CGSize?
    ) -> [String: Any] {
        var app: [String: Any] = [
            "name": snapshot.app.name,
            "pid": Int(snapshot.app.pid),
        ]
        app["bundle_identifier"] = snapshot.app.bundleIdentifier ?? NSNull()

        let bounds = snapshot.windowBounds ?? .zero
        var window: [String: Any] = [
            "bounds": [
                "x": Int(bounds.origin.x.rounded()),
                "y": Int(bounds.origin.y.rounded()),
                "width": Int(bounds.size.width.rounded()),
                "height": Int(bounds.size.height.rounded()),
            ],
        ]
        if let windowID = snapshot.targetWindowID {
            window["id"] = String(windowID)
        } else {
            window["id"] = NSNull()
        }
        if let screenshotPixels {
            window["screenshot_pixels"] = [
                "width": Int(screenshotPixels.width.rounded()),
                "height": Int(screenshotPixels.height.rounded()),
            ]
        } else {
            window["screenshot_pixels"] = NSNull()
        }

        return [
            "snapshot_ref": snapshotRef,
            "captured_at": rfc3339UTC(capturedAt),
            "expires_at": rfc3339UTC(expiresAt),
            "generation": generation,
            "app": app,
            "window": window,
        ]
    }

    // The rendered text block with snapshot_ref printed near the top, so a host
    // that cannot expose structuredContent can still thread the handle.
    static func renderedText(snapshot: AppSnapshot, snapshotRef: String) -> String {
        "snapshot_ref: \(snapshotRef)\n\n" + snapshot.renderedText(style: .fullState)
    }

    // Full modern get_app_state ToolCallResult (text with the ref prefix, the
    // optional screenshot, and the structured payload). Unwired in M2.
    static func result(
        snapshot: AppSnapshot,
        snapshotRef: String,
        capturedAt: Date,
        expiresAt: Date,
        generation: Int,
        screenshotPixels: CGSize?
    ) -> ToolCallResult {
        var content = [ToolResultContentItem.text(renderedText(snapshot: snapshot, snapshotRef: snapshotRef))]
        if let screenshotPNGData = snapshot.screenshotPNGData {
            content.append(.pngImage(screenshotPNGData))
        }
        return ToolCallResult(
            content: content,
            isError: false,
            structuredContent: build(
                snapshot: snapshot,
                snapshotRef: snapshotRef,
                capturedAt: capturedAt,
                expiresAt: expiresAt,
                generation: generation,
                screenshotPixels: screenshotPixels
            )
        )
    }
}
