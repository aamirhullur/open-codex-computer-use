import XCTest
@testable import OpenComputerUseKit

// Cross-platform MCP protocol fixture runner (Swift copy).
//
// Drives StdioMCPServer.handle(line:) (the in-process entry point) against the
// shared golden fixtures under <repo>/tests/mcp-protocol-fixtures and compares
// normalized JSON. The Go binaries run the mirror runner in
// apps/OpenComputerUse{Linux,Windows}/mcp_fixtures_test.go. See the fixtures'
// README.md for the fixture format and normalization rules.
final class MCPProtocolFixtureTests: XCTestCase {

    // Legacy initialize and tools/list responses embed platform-specific
    // instruction text and tool descriptions, so those cases are frozen per
    // platform under legacy/<platform>/. The flat legacy/ directory holds the
    // cases whose bytes are identical on macOS, Linux, and Windows.
    private static let platformDir = "macos"

    private static var fixturesRoot: URL {
        // .../packages/OpenComputerUseKit/Tests/OpenComputerUseKitTests/<thisfile>
        var url = URL(fileURLWithPath: #filePath)
        for _ in 0..<5 {
            url = url.deletingLastPathComponent()
        }
        return url.appendingPathComponent("tests/mcp-protocol-fixtures")
    }

    private static let iso8601Regex = try! NSRegularExpression(
        pattern: #"^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})$"#
    )

    // MARK: - Normalization

    private func normalize(_ value: Any) -> Any {
        if let dict = value as? [String: Any] {
            var out = [String: Any]()
            for (key, val) in dict {
                out[key] = normalize(val)
            }
            // A serverInfo-shaped object carries both name and version; freeze
            // the build version so fixtures survive version bumps.
            if out["name"] != nil, out["version"] != nil {
                out["version"] = "<VERSION>"
            }
            return out
        }
        if let array = value as? [Any] {
            return array.map { normalize($0) }
        }
        if let string = value as? String {
            if string.hasPrefix("ocu_snapshot_v1_") {
                return "<SNAPSHOT_REF>"
            }
            let range = NSRange(string.startIndex..., in: string)
            if Self.iso8601Regex.firstMatch(in: string, range: range) != nil {
                return "<TIMESTAMP>"
            }
            return string
        }
        return value
    }

    private func canonical(_ value: Any?) -> String {
        guard let value = value, !(value is NSNull) else {
            return "null"
        }
        let normalized = normalize(value)
        let data = try! JSONSerialization.data(
            withJSONObject: normalized,
            options: [.sortedKeys, .withoutEscapingSlashes]
        )
        return String(data: data, encoding: .utf8)!
    }

    // MARK: - Fixture execution

    private func runStep(server: StdioMCPServer, step: [String: Any]) -> (actual: String, expected: String) {
        let line: String
        if let raw = step["request_raw"] as? String {
            line = raw
        } else if let request = step["request"] {
            let data = try! JSONSerialization.data(withJSONObject: request, options: [.withoutEscapingSlashes])
            line = String(data: data, encoding: .utf8)!
        } else {
            XCTFail("fixture step has neither request nor request_raw")
            line = ""
        }

        let responseString = server.handle(line: line)
        let actualCanonical: String
        if let responseString = responseString {
            let parsed = try! JSONSerialization.jsonObject(with: Data(responseString.utf8))
            actualCanonical = canonical(parsed)
        } else {
            actualCanonical = "null"
        }
        return (actualCanonical, canonical(step["expect"]))
    }

    private func loadCase(_ url: URL) -> (name: String, steps: [[String: Any]]) {
        let data = try! Data(contentsOf: url)
        guard let object = try! JSONSerialization.jsonObject(with: data) as? [String: Any],
              let name = object["name"] as? String,
              let steps = object["steps"] as? [[String: Any]] else {
            XCTFail("invalid fixture: \(url.path)")
            return ("", [])
        }
        return (name, steps)
    }

    // Fails loudly (like Go's t.Fatalf) if a consulted directory cannot be read
    // or yields no fixtures, so a renamed or missing directory cannot silently
    // skip its goldens while the suite stays green.
    private func fixtureFiles(in dir: URL) -> [URL] {
        let contents: [URL]
        do {
            contents = try FileManager.default.contentsOfDirectory(at: dir, includingPropertiesForKeys: nil)
        } catch {
            XCTFail("cannot read fixture directory \(dir.path): \(error)")
            return []
        }
        let files = contents
            .filter { $0.pathExtension == "json" && !$0.lastPathComponent.hasPrefix("EXPECTED_FAILURES") }
            .sorted { $0.path < $1.path }
        XCTAssertFalse(files.isEmpty, "no fixture files in \(dir.path)")
        return files
    }

    // MARK: - Legacy

    func testLegacyProtocolFixtures() {
        let legacyRoot = Self.fixturesRoot.appendingPathComponent("legacy")
        var files = fixtureFiles(in: legacyRoot)
        files += fixtureFiles(in: legacyRoot.appendingPathComponent(Self.platformDir))
        XCTAssertFalse(files.isEmpty, "no legacy fixtures found")

        for url in files {
            let fixture = loadCase(url)
            let server = StdioMCPServer(service: ComputerUseService())
            for (index, step) in fixture.steps.enumerated() {
                let (actual, expected) = runStep(server: server, step: step)
                XCTAssertEqual(actual, expected, "case \(fixture.name) step \(index)")
            }
        }
    }

    // MARK: - Modern

    private func expectedFailures() -> [String: String] {
        let url = Self.fixturesRoot.appendingPathComponent("modern/EXPECTED_FAILURES.\(Self.platformDir).json")
        let data = try! Data(contentsOf: url)
        let object = try! JSONSerialization.jsonObject(with: data) as? [String: Any]
        return (object?["cases"] as? [String: String]) ?? [:]
    }

    // A case listed in EXPECTED_FAILURES.json is required to currently mismatch
    // its target shape (proving the case executes and the modern era is not yet
    // implemented); if it unexpectedly matches, the test fails so the manifest
    // entry is retired. A modern case absent from the manifest is enforced like
    // a legacy case. The suite is green at M0 and flips to enforcing as M1 lands.
    func testModernProtocolFixtures() {
        let modernRoot = Self.fixturesRoot.appendingPathComponent("modern")
        let files = fixtureFiles(in: modernRoot)
        XCTAssertFalse(files.isEmpty, "no modern fixtures found")
        let manifest = expectedFailures()
        var discovered = Set<String>()

        for url in files {
            let fixture = loadCase(url)
            discovered.insert(fixture.name)
            let server = StdioMCPServer(service: ComputerUseService())
            var allMatch = true
            for step in fixture.steps {
                let (actual, expected) = runStep(server: server, step: step)
                if actual != expected {
                    allMatch = false
                }
            }
            if manifest[fixture.name] != nil {
                XCTAssertFalse(
                    allMatch,
                    "modern case \(fixture.name) now matches its target shape; remove it from modern/EXPECTED_FAILURES.\(Self.platformDir).json"
                )
            } else {
                XCTAssertTrue(
                    allMatch,
                    "modern case \(fixture.name) is not in EXPECTED_FAILURES.\(Self.platformDir).json but does not match its target shape"
                )
            }
        }

        // Guard against manifest rot in the reverse direction: every listed key
        // must name a modern fixture that actually exists.
        for name in manifest.keys where !discovered.contains(name) {
            XCTFail("EXPECTED_FAILURES.\(Self.platformDir).json lists \(name) but no modern fixture has that name")
        }
    }
}
