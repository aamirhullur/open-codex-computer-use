import AppKit
import CoreGraphics
import Foundation
import OpenComputerUseKit

struct MCPResponse {
    let id: Int?
    let result: [String: Any]?
    let error: [String: Any]?
}

// A modern tools/call result, exposing the fields the modern smoke path asserts:
// the structured snapshot_ref (and error taxonomy), the wire-level resultType, and
// the server-identity _meta the adapter decorates onto every modern result.
struct ModernToolResult {
    let result: [String: Any]
    let error: [String: Any]?

    var isError: Bool { (result["isError"] as? Bool) ?? false }

    var text: String? {
        let content = result["content"] as? [[String: Any]]
        return content?.first(where: { $0["type"] as? String == "text" })?["text"] as? String
    }

    var structuredContent: [String: Any]? { result["structuredContent"] as? [String: Any] }
    var snapshotRef: String? { structuredContent?["snapshot_ref"] as? String }
    private var errorEnvelope: [String: Any]? { structuredContent?["error"] as? [String: Any] }
    var errorCode: String? { errorEnvelope?["code"] as? String }
    var errorRetry: String? { errorEnvelope?["retry"] as? String }
    var resultType: String? { result["resultType"] as? String }
    var serverInfoName: String? { modernServerInfoName(result) }
}

// The io.modelcontextprotocol/serverInfo.name the modern adapter decorates onto
// every modern result and onto server/discover.
func modernServerInfoName(_ result: [String: Any]) -> String? {
    let meta = result["_meta"] as? [String: Any]
    let serverInfo = meta?["io.modelcontextprotocol/serverInfo"] as? [String: Any]
    return serverInfo?["name"] as? String
}

final class MCPClient {
    private let process: Process
    private let stdin: FileHandle
    private let stdout: FileHandle
    private var nextID = 1
    // When true, every request carries the modern per-request _meta and the client
    // drives the 2026-07-28 surface (server/discover first, no initialize).
    private let modern: Bool

    init(executableURL: URL, arguments: [String], environment: [String: String]? = nil, modern: Bool = false) throws {
        self.modern = modern
        process = Process()
        process.executableURL = executableURL
        process.arguments = arguments
        process.environment = environment
        let stdinPipe = Pipe()
        let stdoutPipe = Pipe()
        let stderrPipe = Pipe()
        process.standardInput = stdinPipe
        process.standardOutput = stdoutPipe
        process.standardError = stderrPipe
        try process.run()
        stdin = stdinPipe.fileHandleForWriting
        stdout = stdoutPipe.fileHandleForReading
    }

    // The modern per-request envelope: protocol version, evaluated-per-request
    // client capabilities, and client info. Matched verbatim against the adapter's
    // io.modelcontextprotocol/ namespace keys.
    private func modernMeta() -> [String: Any] {
        [
            "io.modelcontextprotocol/protocolVersion": "2026-07-28",
            "io.modelcontextprotocol/clientCapabilities": [:],
            "io.modelcontextprotocol/clientInfo": [
                "name": "OpenComputerUseSmokeSuite",
                "version": "0.3.1",
            ],
        ]
    }

    // server/discover is the modern stdio compatibility probe: no initialize is sent.
    func discover() throws -> [String: Any] {
        let response = try request(method: "server/discover", params: [:])
        if let error = response.error {
            throw SmokeError.message("server/discover error: \(error)")
        }
        return response.result ?? [:]
    }

    // The full modern tools/list result object (tools plus resultType/_meta/cache).
    func listToolsModern() throws -> [String: Any] {
        let response = try request(method: "tools/list", params: [:])
        if let error = response.error {
            throw SmokeError.message("tools/list error: \(error)")
        }
        return response.result ?? [:]
    }

    // Modern tools/call returning the full result so the caller can assert the
    // structured snapshot_ref, the error taxonomy, resultType, and server _meta.
    func callToolResult(_ name: String, arguments: [String: Any]) throws -> ModernToolResult {
        let response = try request(method: "tools/call", params: [
            "name": name,
            "arguments": arguments,
        ])
        if let error = response.error {
            throw SmokeError.message("JSON-RPC error for \(name): \(error)")
        }
        return ModernToolResult(result: response.result ?? [:], error: response.error)
    }

    func initialize() throws {
        _ = try request(method: "initialize", params: [
            "clientInfo": [
                "name": "OpenComputerUseSmokeSuite",
                "version": "0.3.1",
            ],
            "capabilities": [:],
            "protocolVersion": "2025-03-26",
        ])

        try notify(method: "notifications/initialized", params: [:])
    }

    func listTools() throws -> [[String: Any]] {
        let response = try request(method: "tools/list", params: [:])
        return response.result?["tools"] as? [[String: Any]] ?? []
    }

    func callTool(_ name: String, arguments: [String: Any]) throws -> String {
        let response = try request(method: "tools/call", params: [
            "name": name,
            "arguments": arguments,
        ])

        if let error = response.error {
            throw SmokeError.message("JSON-RPC error: \(error)")
        }

        let result = response.result ?? [:]
        if (result["isError"] as? Bool) == true {
            let text = extractText(from: result) ?? "unknown tool error"
            throw SmokeError.message(text)
        }

        guard let text = extractText(from: result) else {
            throw SmokeError.message("Tool \(name) returned no text content.")
        }

        return text
    }

    func terminate() {
        process.terminate()
    }

    private func notify(method: String, params: [String: Any]) throws {
        let payload: [String: Any] = [
            "jsonrpc": "2.0",
            "method": method,
            "params": params,
        ]
        try write(payload)
    }

    private func request(method: String, params: [String: Any]) throws -> MCPResponse {
        let id = nextID
        nextID += 1

        // Modern connections carry params._meta on every request. Legacy requests
        // are left untouched so the existing smoke path stays byte-identical.
        var effectiveParams = params
        if modern {
            effectiveParams["_meta"] = modernMeta()
        }

        let payload: [String: Any] = [
            "jsonrpc": "2.0",
            "id": id,
            "method": method,
            "params": effectiveParams,
        ]

        try write(payload)
        return try readResponse(expectedID: id)
    }

    private func write(_ payload: [String: Any]) throws {
        let data = try JSONSerialization.data(withJSONObject: payload, options: [.withoutEscapingSlashes])
        stdin.write(data)
        stdin.write(Data([0x0A]))
    }

    private func readResponse(expectedID: Int) throws -> MCPResponse {
        let deadline = Date().addingTimeInterval(20)
        var buffer = Data()

        while Date() < deadline {
            let chunk = try stdout.read(upToCount: 1) ?? Data()
            if chunk.isEmpty {
                Thread.sleep(forTimeInterval: 0.05)
                continue
            }

            buffer.append(chunk)

            if chunk == Data([0x0A]) {
                let lineData = buffer.dropLast()
                buffer.removeAll(keepingCapacity: true)
                guard !lineData.isEmpty else {
                    continue
                }

                let object = try JSONSerialization.jsonObject(with: lineData) as? [String: Any] ?? [:]
                let id = object["id"] as? Int

                if id != expectedID {
                    continue
                }

                return MCPResponse(
                    id: id,
                    result: object["result"] as? [String: Any],
                    error: object["error"] as? [String: Any]
                )
            }
        }

        throw SmokeError.message("Timed out waiting for JSON-RPC response \(expectedID)")
    }

    private func extractText(from result: [String: Any]) -> String? {
        let content = result["content"] as? [[String: Any]]
        return content?.first?["text"] as? String
    }
}

enum SmokeError: Error {
    case message(String)
}

@main
enum OpenComputerUseSmokeSuite {
    static func main() throws {
        let productsDirectory = try locateProductsDirectory()
        let fixtureURL = productsDirectory.appendingPathComponent("OpenComputerUseFixture")
        let serverURL = productsDirectory.appendingPathComponent("OpenComputerUse")
        let appName = "OpenComputerUseFixture"
        let mode = SmokeMode(arguments: CommandLine.arguments)

        terminateExistingFixtures(named: appName)
        try? FileManager.default.removeItem(at: FixtureBridge.stateFileURL)

        let fixture = Process()
        fixture.executableURL = fixtureURL
        fixture.environment = smokeFixtureEnvironment()
        fixture.standardOutput = Pipe()
        fixture.standardError = Pipe()
        try fixture.run()

        defer {
            fixture.terminate()
        }

        Thread.sleep(forTimeInterval: 1.5)

        switch mode {
        case .full:
            try runFullSmoke(serverURL: serverURL, appName: appName)
        case .cursorIdleOnly:
            try runCursorIdleSmoke(serverURL: serverURL, appName: appName)
        case .modern:
            try runModernSmoke(serverURL: serverURL, appName: appName)
        }
    }

    private static func runFullSmoke(serverURL: URL, appName: String) throws {
        let client = try MCPClient(executableURL: serverURL, arguments: ["mcp"], environment: smokeServerEnvironment())
        defer {
            client.terminate()
        }

        try client.initialize()

        let tools = try client.listTools()
        guard tools.count == 9 else {
            throw SmokeError.message("Expected 9 tools, got \(tools.count)")
        }

        print("1. list_apps")
        let apps = try client.callTool("list_apps", arguments: [:])
        try expect(apps.contains(appName), "Fixture app should appear in list_apps output.")

        print("2. get_app_state")
        var state = try client.callTool("get_app_state", arguments: [
            "app": appName,
        ])
        var index = parseElementIndex(state)
        try expect(index.keys.contains("fixture-increment"), "fixture button should be indexed")
        let initialCounter = parseCounterValue(state)

        print("3. click element_index")
        state = try client.callTool("click", arguments: [
            "app": appName,
            "element_index": index["fixture-increment"]!.index,
        ])
        try expect(parseCounterValue(state) == initialCounter + 1, "click should increment the counter")

        print("4. click coordinate")
        index = parseElementIndex(state)
        let buttonFrame = index["fixture-increment"]!.frame
        state = try client.callTool("click", arguments: [
            "app": appName,
            "x": buttonFrame.midX,
            "y": buttonFrame.midY,
        ])
        try expect(parseCounterValue(state) == initialCounter + 2, "coordinate click should increment the counter again")

        print("5. perform_secondary_action")
        let windowIndex = index["fixture-window"]?.index ?? "0"
        _ = try client.callTool("perform_secondary_action", arguments: [
            "app": appName,
            "element_index": windowIndex,
            "action": "Raise",
        ])

        print("6. set_value")
        state = try client.callTool("set_value", arguments: [
            "app": appName,
            "element_index": index["fixture-input"]!.index,
            "value": "set-value-ok",
        ])
        try expect(state.contains("set-value-ok"), "set_value should update the text field")

        print("7. type_text")
        let inputFrame = index["fixture-input"]!.frame
        _ = try client.callTool("click", arguments: [
            "app": appName,
            "x": inputFrame.midX,
            "y": inputFrame.midY,
        ])
        state = try client.callTool("type_text", arguments: [
            "app": appName,
            "text": "-typed",
        ])
        try expect(state.contains("set-value-ok-typed"), "type_text should append literal text to the focused text field")

        print("8. press_key")
        index = parseElementIndex(state)
        let keyCaptureFrame = index["fixture-key-capture"]!.frame
        _ = try client.callTool("click", arguments: [
            "app": appName,
            "x": keyCaptureFrame.midX,
            "y": keyCaptureFrame.midY,
        ])
        state = try client.callTool("press_key", arguments: [
            "app": appName,
            "key": "Return",
        ])
        try expect(state.contains("Last key: Return"), "press_key should update the key capture view")

        print("9. scroll")
        index = parseElementIndex(state)
        let scrollIndex = index["fixture-scroll-view"]!.index
        state = try client.callTool("scroll", arguments: [
            "app": appName,
            "direction": "down",
            "element_index": scrollIndex,
            "pages": 1,
        ])
        try expect(!state.contains("Scroll offset: 0"), "scroll should move the scroll view")

        print("10. drag")
        index = parseElementIndex(state)
        let dragFrame = index["fixture-drag-pad"]!.frame
        state = try client.callTool("drag", arguments: [
            "app": appName,
            "from_x": dragFrame.minX + 30,
            "from_y": dragFrame.minY + 30,
            "to_x": dragFrame.maxX - 30,
            "to_y": dragFrame.maxY - 30,
        ])
        try expect(state.contains("Last drag:"), "drag should update the drag status label")
        try expect(!state.contains("Last drag: none"), "drag should report a captured path")

        print("Smoke suite completed.")
    }

    private static func runCursorIdleSmoke(serverURL: URL, appName: String) throws {
        print("1. cursor idle observation setup")
        let observationURL = cursorObservationFileURL()
        try? FileManager.default.removeItem(at: observationURL)
        var environment = smokeServerEnvironment()
        environment["OPEN_COMPUTER_USE_VISUAL_CURSOR"] = "1"
        environment["OPEN_COMPUTER_USE_VISUAL_CURSOR_OBSERVATION_FILE"] = observationURL.path

        let client = try MCPClient(executableURL: serverURL, arguments: ["mcp"], environment: environment)
        defer {
            client.terminate()
        }

        try client.initialize()
        _ = try client.callTool("get_app_state", arguments: [
            "app": appName,
        ])

        print("2. trigger click and wait for idle overlay")
        let state = try client.callTool("click", arguments: [
            "app": appName,
            "element_index": "1",
        ])
        try expect(state.contains("Counter:"), "cursor smoke click should still return fixture state")

        let firstIdleSnapshot = try waitForCursorObservation(at: observationURL, phase: "idle")
        Thread.sleep(forTimeInterval: 0.25)
        let secondIdleSnapshot = try waitForCursorObservation(at: observationURL, phase: "idle")

        print("3. assert anchored tip plus changing rotation")
        let firstTip = try expectPoint(firstIdleSnapshot.tipPosition, name: "first tip position")
        let secondTip = try expectPoint(secondIdleSnapshot.tipPosition, name: "second tip position")
        let firstResting = try expectPoint(firstIdleSnapshot.restingTipPosition, name: "first resting tip position")
        let secondResting = try expectPoint(secondIdleSnapshot.restingTipPosition, name: "second resting tip position")
        let firstRotation = try expectValue(firstIdleSnapshot.rotation, name: "first rotation")
        let secondRotation = try expectValue(secondIdleSnapshot.rotation, name: "second rotation")

        try expect(abs(firstTip.x - secondTip.x) < 0.25, "idle cursor should stay anchored horizontally instead of shaking")
        try expect(abs(firstTip.y - secondTip.y) < 0.25, "idle cursor should stay anchored vertically")
        try expect(distanceBetween(firstTip, firstResting) < 0.35, "idle cursor tip should stay near resting position")
        try expect(distanceBetween(secondTip, secondResting) < 0.35, "idle cursor tip should keep resting anchor")
        try expect(abs(secondRotation - firstRotation) > 0.01, "idle cursor should keep a clearly visible tiny rotation wobble")

        print("Cursor idle smoke completed.")
    }

    // Modern 2026-07-28 pass. Drives the same server binary + fixture app with
    // stateless semantics: server/discover first (no initialize), per-request _meta
    // on every request, the modern catalog, and a real handle chain that threads a
    // snapshot_ref through get_app_state -> click -> set_value, then proves the
    // superseded ref is rejected as stale with zero effect, and that a ref-less
    // action is rejected as missing.
    private static func runModernSmoke(serverURL: URL, appName: String) throws {
        let client = try MCPClient(
            executableURL: serverURL,
            arguments: ["mcp"],
            environment: smokeServerEnvironment(),
            modern: true
        )
        defer {
            client.terminate()
        }

        print("1. server/discover (no initialize)")
        let discover = try client.discover()
        try expect((discover["resultType"] as? String) == "complete", "server/discover must return resultType complete")
        let supported = discover["supportedVersions"] as? [String] ?? []
        try expect(supported == ["2026-07-28", "2025-03-26"], "server/discover must advertise both revisions in order, got \(supported)")
        try expect(modernServerInfoName(discover) == "open-computer-use", "server/discover _meta must carry serverInfo")

        print("2. tools/list modern catalog")
        let listResult = try client.listToolsModern()
        try expect((listResult["resultType"] as? String) == "complete", "modern tools/list must return resultType complete")
        try expect(modernServerInfoName(listResult) == "open-computer-use", "modern tools/list _meta must carry serverInfo")
        let tools = listResult["tools"] as? [[String: Any]] ?? []
        try expect(tools.count == 9, "modern catalog must expose 9 tools, got \(tools.count)")
        let actionTools: Set<String> = [
            "click", "perform_secondary_action", "scroll", "drag", "type_text", "press_key", "set_value",
        ]
        for tool in tools {
            let name = tool["name"] as? String ?? ""
            let required = ((tool["inputSchema"] as? [String: Any])?["required"] as? [String]) ?? []
            if actionTools.contains(name) {
                try expect(required.contains("snapshot_ref"), "\(name) must require snapshot_ref in the modern catalog")
            } else {
                try expect(!required.contains("snapshot_ref"), "\(name) must not require snapshot_ref")
            }
        }

        print("3. get_app_state mints a snapshot_ref (structuredContent + text)")
        let stateResult = try client.callToolResult("get_app_state", arguments: ["app": appName])
        try expect(!stateResult.isError, "get_app_state should succeed: \(stateResult.text ?? "")")
        try expect(stateResult.resultType == "complete", "modern get_app_state must carry resultType complete")
        try expect(stateResult.serverInfoName == "open-computer-use", "modern get_app_state must carry serverInfo _meta")
        guard let ref0 = stateResult.snapshotRef else {
            throw SmokeError.message("get_app_state must return a snapshot_ref in structuredContent")
        }
        let stateText = stateResult.text ?? ""
        try expect(stateText.contains("snapshot_ref: \(ref0)"), "snapshot_ref must also appear in the text block")
        var index = parseElementIndex(stateText)
        try expect(index.keys.contains("fixture-increment"), "fixture button should be indexed")
        let counterInitial = parseCounterValue(stateText)

        print("4. click with the snapshot_ref returns a differing successor")
        let clickResult = try client.callToolResult("click", arguments: [
            "app": appName,
            "snapshot_ref": ref0,
            "element_index": index["fixture-increment"]!.index,
        ])
        try expect(!clickResult.isError, "modern click should succeed: \(clickResult.text ?? "")")
        try expect(clickResult.resultType == "complete", "modern action result must carry resultType complete")
        try expect(clickResult.serverInfoName == "open-computer-use", "modern action result must carry serverInfo _meta")
        guard let ref1 = clickResult.snapshotRef else {
            throw SmokeError.message("click must return a successor snapshot_ref")
        }
        try expect(ref1 != ref0, "successor snapshot_ref must differ from the consumed one")
        let clickText = clickResult.text ?? ""
        try expect(clickText.contains("snapshot_ref: \(ref1)"), "successor ref must appear in the action text block")
        try expect(parseCounterValue(clickText) == counterInitial + 1, "modern click should increment the counter")
        index = parseElementIndex(clickText)

        print("5. set_value threads the successor forward")
        let setResult = try client.callToolResult("set_value", arguments: [
            "app": appName,
            "snapshot_ref": ref1,
            "element_index": index["fixture-input"]!.index,
            "value": "modern-smoke-ok",
        ])
        try expect(!setResult.isError, "modern set_value should succeed: \(setResult.text ?? "")")
        guard let ref2 = setResult.snapshotRef else {
            throw SmokeError.message("set_value must return a successor snapshot_ref")
        }
        try expect(ref2 != ref1, "set_value successor must differ from its predecessor")
        let setText = setResult.text ?? ""
        try expect(setText.contains("modern-smoke-ok"), "set_value should update the text field")
        let counterAfterSet = parseCounterValue(setText)

        // The stale/missing steps do not depend on the concrete element, but both use
        // the same parsed element_index (with a shared fallback) so a future reorder
        // cannot introduce a type or value inconsistency across these calls.
        let probeIndex = index["fixture-increment"]?.index ?? "1"

        print("6. reusing the superseded snapshot_ref is rejected as stale with zero effect")
        let staleResult = try client.callToolResult("click", arguments: [
            "app": appName,
            "snapshot_ref": ref0,
            "element_index": probeIndex,
        ])
        try expect(staleResult.isError, "reusing a superseded snapshot_ref must be an error")
        try expect(staleResult.errorCode == "snapshot_ref_stale", "expected snapshot_ref_stale, got \(staleResult.errorCode ?? "nil")")
        try expect(staleResult.errorRetry == "new_state", "a stale ref must instruct recapture (retry new_state)")

        let afterState = try client.callToolResult("get_app_state", arguments: ["app": appName])
        try expect(parseCounterValue(afterState.text ?? "") == counterAfterSet, "the stale-ref action must have zero effect on the counter")

        print("7. a modern action without a snapshot_ref is rejected as missing")
        let missingResult = try client.callToolResult("click", arguments: [
            "app": appName,
            "element_index": probeIndex,
        ])
        try expect(missingResult.isError, "a modern action without a snapshot_ref must error")
        try expect(missingResult.errorCode == "snapshot_ref_missing", "expected snapshot_ref_missing, got \(missingResult.errorCode ?? "nil")")

        print("Modern smoke suite completed.")
    }

    private static func smokeServerEnvironment() -> [String: String] {
        var environment = ProcessInfo.processInfo.environment
        environment["OPEN_COMPUTER_USE_DISABLE_APP_AGENT_PROXY"] = "1"
        return environment
    }

    private static func smokeFixtureEnvironment() -> [String: String] {
        var environment = ProcessInfo.processInfo.environment
        environment["OPEN_COMPUTER_USE_FIXTURE_HEADLESS"] = "1"
        return environment
    }

    private static func locateProductsDirectory() throws -> URL {
        let executableURL = URL(fileURLWithPath: CommandLine.arguments[0]).resolvingSymlinksInPath()
        return executableURL.deletingLastPathComponent()
    }

    private static func expect(_ condition: @autoclosure () -> Bool, _ message: String) throws {
        guard condition() else {
            throw SmokeError.message(message)
        }
    }

    private static func terminateExistingFixtures(named name: String) {
        let candidates = NSWorkspace.shared.runningApplications.filter { app in
            if app.localizedName == name {
                return true
            }

            return app.executableURL?.deletingPathExtension().lastPathComponent == name
        }

        for app in candidates {
            _ = app.terminate()
        }

        for app in candidates where !app.isTerminated {
            _ = app.forceTerminate()
        }

        if !candidates.isEmpty {
            Thread.sleep(forTimeInterval: 0.5)
        }
    }

    private static func parseElementIndex(_ state: String) -> [String: (index: String, frame: CGRect)] {
        var result: [String: (String, CGRect)] = [:]

        for rawLine in state.split(separator: "\n") {
            let line = String(rawLine)
            guard
                let identifierRange = line.range(of: " ID: "),
                let frameRange = line.range(of: " Frame: ")
            else {
                continue
            }

            let prefix = line[..<identifierRange.lowerBound].trimmingCharacters(in: .whitespaces)
            guard let index = prefix.split(separator: " ").first.map(String.init) else {
                continue
            }

            let identifierSlice = line[identifierRange.upperBound..<frameRange.lowerBound]
            let identifier = identifierSlice
                .components(separatedBy: " Value:")
                .first?
                .components(separatedBy: " Secondary Actions:")
                .first?
                .trimmingCharacters(in: .whitespaces) ?? ""
            let frame = parseFrame(String(line[frameRange.upperBound...]))
            result[identifier] = (index, frame)
        }

        return result
    }

    private static func parseFrame(_ text: String) -> CGRect {
        let values = text
            .components(separatedBy: ",")
            .compactMap { part -> Double? in
                part.split(separator: "=").last.flatMap { Double($0.trimmingCharacters(in: .whitespaces)) }
            }

        guard values.count == 4 else {
            return .zero
        }

        return CGRect(x: values[0], y: values[1], width: values[2], height: values[3])
    }

    private static func parseCounterValue(_ state: String) -> Int {
        guard
            let range = state.range(of: "Counter: "),
            let value = state[range.upperBound...].split(whereSeparator: { !$0.isNumber }).first,
            let counter = Int(value)
        else {
            return -1
        }

        return counter
    }

    private static func cursorObservationFileURL() -> URL {
        URL(fileURLWithPath: NSTemporaryDirectory())
            .appendingPathComponent("open-computer-use-smoke", isDirectory: true)
            .appendingPathComponent("visual-cursor-observation.json")
    }

    private static func waitForCursorObservation(at url: URL, phase: String, timeout: TimeInterval = 6) throws -> VisualCursorObservationSnapshot {
        let deadline = Date().addingTimeInterval(timeout)
        var lastSnapshot: VisualCursorObservationSnapshot?

        while Date() < deadline {
            if
                let data = try? Data(contentsOf: url),
                let snapshot = try? JSONDecoder().decode(VisualCursorObservationSnapshot.self, from: data)
            {
                lastSnapshot = snapshot
                if snapshot.phase == phase {
                    return snapshot
                }
            }

            Thread.sleep(forTimeInterval: 0.05)
        }

        throw SmokeError.message("Timed out waiting for cursor observation phase \(phase). Last snapshot: \(String(describing: lastSnapshot))")
    }

    private static func expectPoint(_ point: VisualCursorObservationPoint?, name: String) throws -> CGPoint {
        guard let point else {
            throw SmokeError.message("Missing \(name)")
        }

        return CGPoint(x: point.x, y: point.y)
    }

    private static func expectValue(_ value: Double?, name: String) throws -> Double {
        guard let value else {
            throw SmokeError.message("Missing \(name)")
        }

        return value
    }

    private static func distanceBetween(_ lhs: CGPoint, _ rhs: CGPoint) -> CGFloat {
        hypot(lhs.x - rhs.x, lhs.y - rhs.y)
    }
}

private enum SmokeMode {
    case full
    case cursorIdleOnly
    case modern

    init(arguments: [String]) {
        if arguments.contains("--modern") {
            self = .modern
        } else if arguments.contains("--cursor-idle-only") {
            self = .cursorIdleOnly
        } else {
            self = .full
        }
    }
}
