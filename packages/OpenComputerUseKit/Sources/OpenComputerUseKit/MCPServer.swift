import Foundation

let computerUseServerInstructions = """
Computer Use tools let you interact with macOS apps by performing UI actions.

Some apps might have a separate dedicated plugin or skill. You may want to use that plugin or skill instead of Computer Use when it seems like a good fit for the task. While the separate plugin or skill may not expose every feature in the app, if the plugin can perform the task with its available features, prefer it. If the needed capability is not exposed there, use Computer Use may be appropriate for the missing interaction.

Begin by calling `get_app_state` every turn you want to use Computer Use to get the latest state before acting. Codex will automatically stop the session after each assistant turn, so this step is required before interacting with apps in a new assistant turn.

The available tools are list_apps, get_app_state, click, perform_secondary_action, scroll, drag, type_text, press_key, and set_value. If any of these are not available in your environment, use tool_search to surface one before calling any Computer Use action tools.

Computer Use tools allow you to use the user's apps in the background, so while you're using an app, the user can continue to use other apps on their computer. Avoid doing anything that would disrupt the user's active session, such as overwriting the contents of their clipboard, unless they asked you to!

After each action, use the action result or fetch the latest state to verify the UI changed as expected.
Prefer element-targeted interactions over coordinate clicks when an index for the targeted element is available. Note that element indices are the sequential integers from the app state's accessibility tree.
Avoid falling back to AppleScript during a computer use session. Prefer Computer Use tools as much as possible to complete tasks.
Ask the user before taking destructive or externally visible actions such as sending, deleting, or purchasing. If helpful, you can ask follow-up questions before taking action to make sure you’re understanding the user’s request correctly.
"""

public final class StdioMCPServer {
    private let dispatcher: ComputerUseToolDispatcher
    // Per-connection era, decided on the first classifiable request and sticky
    // thereafter. nil until the first non-notification request arrives.
    private var era: ProtocolEra?

    // Injected-dispatcher initializer. The shared automation runtime builds the
    // dispatcher (and its service) and hands it in so a connection owns only its
    // protocol adapter.
    public init(dispatcher: ComputerUseToolDispatcher) {
        self.dispatcher = dispatcher
    }

    // Back-compat convenience initializer. Existing callers (the app agent,
    // OpenComputerUseMain) construct a server from a service without change.
    public convenience init(service: ComputerUseService = ComputerUseService()) {
        self.init(dispatcher: ComputerUseToolDispatcher(service: service))
    }

    public func run() throws {
        while let line = readLine(strippingNewline: true) {
            guard !line.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty else {
                continue
            }

            if let response = handle(line: line) {
                FileHandle.standardOutput.write((response + "\n").data(using: .utf8)!)
            }
        }
    }

    public func handle(line: String) -> String? {
        // Decode. A well-formed JSON value that is not an object is a -32700; a
        // truly unparseable line preserves the pre-existing generic tool-style
        // fallback (its message is environment dependent and intentionally not
        // frozen as a golden case).
        let payload: [String: Any]
        do {
            guard let object = try JSONSerialization.jsonObject(with: Data(line.utf8)) as? [String: Any] else {
                return try? encodeJSONRPCError(id: nil, code: -32700, message: "Invalid JSON-RPC payload")
            }
            payload = object
        } catch {
            let message = (error as? LocalizedError)?.errorDescription ?? String(describing: error)
            return try? encodeJSONRPCResult(
                id: nil,
                result: [
                    "content": [["type": "text", "text": message]],
                    "isError": true,
                ]
            )
        }

        let method = payload["method"] as? String
        let id = payload["id"]
        let params = payload["params"] as? [String: Any] ?? [:]

        // turn-ended is a best-effort custom notification accepted in both eras
        // and never classifies the connection.
        if method == "notifications/turn-ended" {
            VisualCursorSupport.performOnMain {
                SoftwareCursorOverlay.reset()
            }
            return nil
        }

        // A payload with no method (a stray JSON-RPC response) is dropped before
        // classification, so it never selects an era and never draws output in
        // either era. Mirrors the Go handlers.
        guard let method = method, !method.isEmpty else {
            return nil
        }

        let modernMeta = MCPProtocol.hasModernMeta(params: params)

        // Classify on the first classifiable request; sticky thereafter. A modern
        // _meta selects modern; anything else (initialize or a plain legacy
        // request) selects legacy, keeping the legacy path byte-identical.
        if era == nil {
            era = modernMeta ? .modern20260728 : .legacy20250326
        }

        switch era! {
        case .modern20260728:
            return handleModern(method: method, id: id, params: params, modernMeta: modernMeta)
        case .legacy20250326:
            // Cross-era guard: a request carrying modern _meta on a legacy
            // connection is a modern client on the wrong connection.
            if modernMeta {
                return try? encodeJSONRPCError(id: id, code: -32601, message: "Method not found: \(method)")
            }
            return handleLegacy(method: method, id: id, params: params)
        }
    }

    // MARK: - Legacy (2025-03-26)

    // Byte-identical to the pre-adapter behavior. The catch blocks wrap the whole
    // switch so a throwing tools/call is coerced into a tool-style result, which
    // legacy fixtures pin.
    private func handleLegacy(method: String, id: Any?, params: [String: Any]) -> String? {
        do {
            switch method {
            case "initialize":
                return try encodeJSONRPCResult(
                    id: id,
                    result: [
                        "protocolVersion": "2025-03-26",
                        "serverInfo": [
                            "name": "open-computer-use",
                            "version": openComputerUseVersion,
                        ],
                        "capabilities": [
                            "tools": [
                                "listChanged": false,
                            ],
                        ],
                        "instructions": computerUseServerInstructions,
                    ]
                )
            case "notifications/initialized":
                return nil
            case "ping":
                return try encodeJSONRPCResult(id: id, result: [:])
            case "tools/list":
                return try encodeJSONRPCResult(
                    id: id,
                    result: [
                        "tools": ToolDefinitions.all.map(\.asDictionary),
                    ]
                )
            case "tools/call":
                let name = params["name"] as? String ?? ""
                let arguments = params["arguments"] as? [String: Any] ?? [:]
                let result = try dispatcher.callTool(name: name, arguments: arguments)
                return try encodeJSONRPCResult(
                    id: id,
                    result: result.asDictionary
                )
            default:
                return try encodeJSONRPCError(id: id, code: -32601, message: "Method not found: \(method)")
            }
        } catch let error as ComputerUseError {
            let result = ToolCallResult.text(error.errorDescription ?? String(describing: error), isError: error.toolResultIsError)
            return try? encodeJSONRPCResult(id: id, result: result.asDictionary)
        } catch {
            let message = (error as? LocalizedError)?.errorDescription ?? String(describing: error)
            return try? encodeJSONRPCResult(
                id: id,
                result: [
                    "content": [
                        [
                            "type": "text",
                            "text": message,
                        ],
                    ],
                    "isError": true,
                ]
            )
        }
    }

    // MARK: - Modern (2026-07-28)

    // A modern connection requires per-request _meta on every request. Protocol
    // failures (missing/invalid metadata, unsupported version, unsupported
    // method) return JSON-RPC error objects; only a throwing tools/call is
    // coerced into a tool-style isError result, which is then decorated.
    private func handleModern(method: String, id: Any?, params: [String: Any], modernMeta: Bool) -> String? {
        // Cross-era guard: a legacy-style request (no modern _meta) on a modern
        // connection is rejected without switching semantics.
        guard modernMeta else {
            return try? encodeJSONRPCError(
                id: id,
                code: -32600,
                message: "This connection uses the modern 2026-07-28 protocol; include params._meta on every request."
            )
        }

        let envelope: MCPRequestEnvelope
        do {
            envelope = try MCPRequestEnvelope.parse(params: params)
        } catch let failure as MCPProtocolFailure {
            return try? encodeJSONRPCError(id: id, code: failure.code, message: failure.message, data: failure.data)
        } catch {
            return try? encodeJSONRPCError(id: id, code: -32602, message: "Invalid params")
        }
        recordLogLevel(envelope.logLevel)

        switch method {
        case "server/discover":
            return try? encodeModernResult(id: id, result: serverDiscoverResult())
        case "tools/list":
            return try? encodeModernResult(id: id, result: modernToolsListResult())
        case "tools/call":
            let name = params["name"] as? String ?? ""
            let arguments = params["arguments"] as? [String: Any] ?? [:]
            let result: ToolCallResult
            do {
                result = try dispatcher.callTool(name: name, arguments: arguments)
            } catch let error as ComputerUseError {
                result = ToolCallResult.text(error.errorDescription ?? String(describing: error), isError: error.toolResultIsError)
            } catch {
                let message = (error as? LocalizedError)?.errorDescription ?? String(describing: error)
                result = ToolCallResult.text(message, isError: true)
            }
            return try? encodeModernResult(id: id, result: result.asDictionary)
        default:
            return try? encodeJSONRPCError(id: id, code: -32601, message: "Method not found: \(method)")
        }
    }

    private func serverDiscoverResult() -> [String: Any] {
        [
            "supportedVersions": MCPProtocol.supportedVersions,
            "capabilities": ["tools": ["listChanged": false]],
            "instructions": computerUseServerInstructions,
            "ttlMs": MCPProtocol.cacheTtlMs,
            "cacheScope": MCPProtocol.cacheScopePublic,
        ]
    }

    private func modernToolsListResult() -> [String: Any] {
        [
            "tools": ToolDefinitions.all.map(\.asDictionary),
            "ttlMs": MCPProtocol.cacheTtlMs,
            "cacheScope": MCPProtocol.cacheScopePublic,
        ]
    }

    private func recordLogLevel(_ level: String?) {
        guard let level = level else { return }
        // Recorded to stderr debug only; stdout stays JSON-RPC. Gated to match
        // the existing service debug convention so normal runs stay quiet.
        guard ProcessInfo.processInfo.environment["OPEN_COMPUTER_USE_DEBUG_INPUT_FALLBACKS"] != nil else { return }
        FileHandle.standardError.write(Data("mcp: client logLevel \(level)\n".utf8))
    }

    // MARK: - Encoding

    // The single modern decoration choke point: adds resultType and the server
    // identity _meta after the handler builds a result. Tool-service code never
    // adds these ad hoc. Internal (not private) so the merge behavior is unit
    // testable directly.
    func decorateModern(_ result: [String: Any]) -> [String: Any] {
        var decorated = result
        decorated["resultType"] = "complete"
        // Merge server identity into any existing _meta so a handler that already
        // set other _meta keys keeps them.
        var meta = (result[MCPProtocol.metaField] as? [String: Any]) ?? [:]
        meta[MCPProtocol.serverInfoKey] = [
            "name": MCPProtocol.serverName,
            "version": resolvedOpenComputerUseVersion(),
        ]
        decorated[MCPProtocol.metaField] = meta
        return decorated
    }

    private func encodeModernResult(id: Any?, result: [String: Any]) throws -> String {
        try encodeJSONRPCResult(id: id, result: decorateModern(result))
    }

    private func encodeJSONRPCResult(id: Any?, result: [String: Any]) throws -> String {
        try encode([
            "jsonrpc": "2.0",
            "id": id ?? NSNull(),
            "result": result,
        ])
    }

    private func encodeJSONRPCError(id: Any?, code: Int, message: String, data: [String: Any]? = nil) throws -> String {
        var error: [String: Any] = [
            "code": code,
            "message": message,
        ]
        if let data = data {
            error["data"] = data
        }
        return try encode([
            "jsonrpc": "2.0",
            "id": id ?? NSNull(),
            "error": error,
        ])
    }

    private func encode(_ object: [String: Any]) throws -> String {
        let data = try JSONSerialization.data(withJSONObject: object, options: [.withoutEscapingSlashes])
        guard let text = String(data: data, encoding: .utf8) else {
            throw ComputerUseError.message("Failed to encode JSON-RPC response.")
        }

        return text
    }
}
