import XCTest
@testable import OpenComputerUseKit

// M2: structured results, era-specific catalogs, snapshot-ref error taxonomy,
// and the get_app_state structured-shape builder.
final class M2SchemasStructuredTests: XCTestCase {

    private let actionTools: Set<String> = [
        "click", "drag", "perform_secondary_action", "press_key", "scroll", "set_value", "type_text",
    ]

    private func canonical(_ value: Any) -> String {
        let data = try! JSONSerialization.data(
            withJSONObject: value,
            options: [.sortedKeys, .withoutEscapingSlashes]
        )
        return String(data: data, encoding: .utf8)!
    }

    // MARK: - Catalog eras

    func testLegacyCatalogEqualsToolDefinitionsAll() {
        let legacy = ToolCatalog.forEra(.legacy20250326)
        XCTAssertEqual(legacy.count, ToolDefinitions.all.count)
        for (lhs, rhs) in zip(legacy, ToolDefinitions.all) {
            XCTAssertEqual(canonical(lhs.asDictionary), canonical(rhs.asDictionary))
        }
    }

    func testModernCatalogSameNamesSameOrder() {
        let legacyNames = ToolCatalog.forEra(.legacy20250326).map(\.name)
        let modernNames = ToolCatalog.forEra(.modern20260728).map(\.name)
        XCTAssertEqual(legacyNames, modernNames)
        XCTAssertEqual(modernNames.count, 9)
    }

    func testModernActionToolsRequireSnapshotRef() {
        for tool in ToolCatalog.forEra(.modern20260728) where actionTools.contains(tool.name) {
            let properties = tool.inputSchema["properties"] as? [String: Any] ?? [:]
            let snapshotRef = properties["snapshot_ref"] as? [String: Any]
            XCTAssertEqual(snapshotRef?["type"] as? String, "string", tool.name)
            XCTAssertEqual(
                snapshotRef?["description"] as? String,
                "Snapshot reference returned by the immediately preceding get_app_state or action result for this app.",
                tool.name
            )
            let required = tool.inputSchema["required"] as? [String] ?? []
            XCTAssertEqual(required.last, "snapshot_ref", "snapshot_ref must be appended last for \(tool.name)")
        }
    }

    // The modern action schemas add exactly snapshot_ref to legacy required, in
    // the same relative order, with nothing else changed.
    func testModernActionRequiredIsLegacyPlusSnapshotRef() {
        let legacy = Dictionary(uniqueKeysWithValues: ToolCatalog.forEra(.legacy20250326).map { ($0.name, $0) })
        for tool in ToolCatalog.forEra(.modern20260728) where actionTools.contains(tool.name) {
            let legacyRequired = legacy[tool.name]!.inputSchema["required"] as? [String] ?? []
            let modernRequired = tool.inputSchema["required"] as? [String] ?? []
            XCTAssertEqual(modernRequired, legacyRequired + ["snapshot_ref"], tool.name)
        }
    }

    func testModernActionDescriptionsUnchanged() {
        let legacy = Dictionary(uniqueKeysWithValues: ToolCatalog.forEra(.legacy20250326).map { ($0.name, $0) })
        for tool in ToolCatalog.forEra(.modern20260728) where actionTools.contains(tool.name) {
            XCTAssertEqual(tool.description, legacy[tool.name]!.description, tool.name)
        }
    }

    func testModernStateToolsMentionSnapshotRefWithoutRequiringIt() {
        let modern = Dictionary(uniqueKeysWithValues: ToolCatalog.forEra(.modern20260728).map { ($0.name, $0) })
        for name in ["get_app_state", "list_apps"] {
            let tool = modern[name]!
            XCTAssertTrue(tool.description.contains("snapshot_ref"), name)
            let properties = tool.inputSchema["properties"] as? [String: Any] ?? [:]
            XCTAssertNil(properties["snapshot_ref"], "\(name) must not accept snapshot_ref")
            let required = tool.inputSchema["required"] as? [String] ?? []
            XCTAssertFalse(required.contains("snapshot_ref"), name)
        }
    }

    func testForEraIsDeterministic() {
        XCTAssertEqual(
            canonical(ToolCatalog.forEra(.modern20260728).map(\.asDictionary)),
            canonical(ToolCatalog.forEra(.modern20260728).map(\.asDictionary))
        )
    }

    // MARK: - structuredContent round-trip

    func testStructuredContentSerializesWhenPresent() {
        let result = ToolCallResult(
            content: [.text("hi")],
            isError: false,
            structuredContent: ["snapshot_ref": "ocu_snapshot_v1_abc", "generation": 3]
        )
        let dict = result.asDictionary
        XCTAssertNotNil(dict["structuredContent"])
        let data = try! JSONSerialization.data(withJSONObject: dict)
        let parsed = try! JSONSerialization.jsonObject(with: data) as! [String: Any]
        let structured = parsed["structuredContent"] as! [String: Any]
        XCTAssertEqual(structured["snapshot_ref"] as? String, "ocu_snapshot_v1_abc")
        XCTAssertEqual(structured["generation"] as? Int, 3)
    }

    func testStructuredContentOmittedWhenNil() {
        let result = ToolCallResult.text("plain")
        XCTAssertNil(result.structuredContent)
        XCTAssertFalse(result.asDictionary.keys.contains("structuredContent"))
    }

    // MARK: - Snapshot-ref error taxonomy

    func testAllErrorCodesSerialize() {
        let expected: [SnapshotRefErrorCode: String] = [
            .missing: "snapshot_ref_missing",
            .malformed: "snapshot_ref_malformed",
            .unknown: "snapshot_ref_unknown",
            .expired: "snapshot_ref_expired",
            .stale: "snapshot_ref_stale",
            .inUse: "snapshot_ref_in_use",
            .targetChanged: "snapshot_target_changed",
            .actionOutcomeUncertain: "snapshot_action_outcome_uncertain",
        ]
        for (code, raw) in expected {
            let result = SnapshotRefError.make(code).toToolCallResult()
            XCTAssertTrue(result.isError)
            let error = result.structuredContent?["error"] as? [String: Any]
            XCTAssertEqual(error?["code"] as? String, raw)
            XCTAssertFalse((error?["message"] as? String ?? "").isEmpty)
            let retry = error?["retry"] as? String
            XCTAssertTrue(retry == "same_handle" || retry == "new_state", raw)
        }
    }

    func testErrorRetryDefaults() {
        XCTAssertEqual(SnapshotRefError.make(.inUse).retry, .sameHandle)
        XCTAssertEqual(SnapshotRefError.make(.missing).retry, .newState)
        XCTAssertEqual(SnapshotRefError.make(.expired).retry, .newState)
    }

    func testErrorCustomMessageAndRetry() {
        let error = SnapshotRefError.make(.unknown, message: "custom", retry: .sameHandle)
        let result = error.toToolCallResult()
        XCTAssertEqual(result.primaryText, "custom")
        let payload = result.structuredContent?["error"] as? [String: Any]
        XCTAssertEqual(payload?["retry"] as? String, "same_handle")
        XCTAssertEqual(payload?["message"] as? String, "custom")
    }

    // MARK: - get_app_state structured shape builder

    private func fixtureSnapshot() -> AppSnapshot {
        AppSnapshot(
            app: RunningAppDescriptor(
                name: "Example",
                bundleIdentifier: "com.example.app",
                pid: 1234,
                runningApplication: NSRunningApplication.current
            ),
            windowTitle: "Example",
            windowBounds: CGRect(x: 0, y: 0, width: 1200, height: 800),
            targetWindowID: 4242,
            targetWindowLayer: 0,
            screenshotPNGData: nil,
            mode: .accessibility,
            treeLines: ["line one", "line two"],
            focusedSummary: nil,
            focusedElement: nil,
            selectedText: nil,
            elements: [:]
        )
    }

    func testStructuredShapeMatchesContract() {
        let captured = Date(timeIntervalSince1970: 1_755_086_400)
        let expires = captured.addingTimeInterval(120)
        let structured = SnapshotStructuredContent.build(
            snapshot: fixtureSnapshot(),
            snapshotRef: "ocu_snapshot_v1_TESTHANDLE",
            capturedAt: captured,
            expiresAt: expires,
            generation: 7,
            screenshotPixels: CGSize(width: 2400, height: 1600)
        )

        XCTAssertEqual(structured["snapshot_ref"] as? String, "ocu_snapshot_v1_TESTHANDLE")
        XCTAssertEqual(structured["generation"] as? Int, 7)
        XCTAssertTrue((structured["captured_at"] as? String ?? "").hasSuffix("Z"))
        XCTAssertTrue((structured["expires_at"] as? String ?? "").hasSuffix("Z"))
        XCTAssertNotEqual(structured["captured_at"] as? String, structured["expires_at"] as? String)

        let app = structured["app"] as? [String: Any]
        XCTAssertEqual(app?["name"] as? String, "Example")
        XCTAssertEqual(app?["bundle_identifier"] as? String, "com.example.app")
        XCTAssertEqual(app?["pid"] as? Int, 1234)

        let window = structured["window"] as? [String: Any]
        XCTAssertEqual(window?["id"] as? String, "4242")
        let bounds = window?["bounds"] as? [String: Any]
        XCTAssertEqual(bounds?["x"] as? Int, 0)
        XCTAssertEqual(bounds?["y"] as? Int, 0)
        XCTAssertEqual(bounds?["width"] as? Int, 1200)
        XCTAssertEqual(bounds?["height"] as? Int, 800)
        let pixels = window?["screenshot_pixels"] as? [String: Any]
        XCTAssertEqual(pixels?["width"] as? Int, 2400)
        XCTAssertEqual(pixels?["height"] as? Int, 1600)

        // Whole structure must be JSON-serializable.
        XCTAssertTrue(JSONSerialization.isValidJSONObject(structured))
    }

    func testStructuredShapeNullableFields() {
        var snapshot = fixtureSnapshot()
        snapshot = AppSnapshot(
            app: RunningAppDescriptor(
                name: "NoBundle",
                bundleIdentifier: nil,
                pid: 99,
                runningApplication: NSRunningApplication.current
            ),
            windowTitle: nil,
            windowBounds: nil,
            targetWindowID: nil,
            targetWindowLayer: nil,
            screenshotPNGData: nil,
            mode: .accessibility,
            treeLines: [],
            focusedSummary: nil,
            focusedElement: nil,
            selectedText: nil,
            elements: [:]
        )
        let structured = SnapshotStructuredContent.build(
            snapshot: snapshot,
            snapshotRef: "ocu_snapshot_v1_X",
            capturedAt: Date(timeIntervalSince1970: 0),
            expiresAt: Date(timeIntervalSince1970: 120),
            generation: 1,
            screenshotPixels: nil
        )
        let app = structured["app"] as? [String: Any]
        XCTAssertTrue(app?["bundle_identifier"] is NSNull)
        let window = structured["window"] as? [String: Any]
        XCTAssertTrue(window?["id"] is NSNull)
        XCTAssertTrue(window?["screenshot_pixels"] is NSNull)
        XCTAssertTrue(JSONSerialization.isValidJSONObject(structured))
    }

    func testStructuredResultPrependsSnapshotRefText() {
        let result = SnapshotStructuredContent.result(
            snapshot: fixtureSnapshot(),
            snapshotRef: "ocu_snapshot_v1_HEAD",
            capturedAt: Date(timeIntervalSince1970: 0),
            expiresAt: Date(timeIntervalSince1970: 120),
            generation: 2,
            screenshotPixels: nil
        )
        XCTAssertTrue(result.primaryText?.hasPrefix("snapshot_ref: ocu_snapshot_v1_HEAD") ?? false)
        XCTAssertNotNil(result.structuredContent)
        XCTAssertFalse(result.isError)
    }
}
