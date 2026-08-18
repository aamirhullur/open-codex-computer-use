import XCTest
@testable import OpenComputerUseKit

// Unit tests for the M1 dual-era protocol adapter: era classification, modern
// envelope validation, response decoration, discover determinism, and the
// catch-scoping fix. The cross-platform wire contract itself is pinned by
// MCPProtocolFixtureTests against the golden fixtures; these tests cover the
// Swift adapter's branching directly.
final class MCPDualEraAdapterTests: XCTestCase {

    // A valid modern _meta block with the required keys and a well-formed
    // clientInfo. Individual tests drop or corrupt one field to exercise a path.
    private let validMeta = #""_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{},"io.modelcontextprotocol/clientInfo":{"name":"probe","version":"0"}}"#

    private func server() -> StdioMCPServer {
        StdioMCPServer(service: ComputerUseService())
    }

    private func decode(_ response: String?) -> [String: Any] {
        guard let response = response, let data = response.data(using: .utf8) else {
            XCTFail("expected a response line")
            return [:]
        }
        return (try? JSONSerialization.jsonObject(with: data) as? [String: Any]) ?? [:]
    }

    private func result(_ response: String?) -> [String: Any]? {
        decode(response)["result"] as? [String: Any]
    }

    private func error(_ response: String?) -> [String: Any]? {
        decode(response)["error"] as? [String: Any]
    }

    // MARK: - Era classification table

    func testModernMetaFirstRequestClassifiesModern() {
        let response = server().handle(line: #"{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{\#(validMeta)}}"#)
        XCTAssertEqual(result(response)?["resultType"] as? String, "complete")
        XCTAssertNotNil(result(response)?["supportedVersions"])
    }

    func testInitializeWithoutModernMetaClassifiesLegacy() {
        let response = server().handle(line: #"{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{}}}"#)
        XCTAssertEqual(result(response)?["protocolVersion"] as? String, "2025-03-26")
        XCTAssertNil(result(response)?["resultType"], "legacy results are not decorated")
    }

    func testPlainRequestWithoutMetaClassifiesLegacy() {
        let response = server().handle(line: #"{"jsonrpc":"2.0","id":2,"method":"ping"}"#)
        XCTAssertEqual(result(response)?.isEmpty, true, "legacy ping returns an empty object")
        XCTAssertNil(error(response))
    }

    func testEraIsStickyModernRejectsLaterLegacyInitialize() {
        let server = self.server()
        _ = server.handle(line: #"{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{\#(validMeta)}}"#)
        let response = server.handle(line: #"{"jsonrpc":"2.0","id":2,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{}}}"#)
        XCTAssertEqual(error(response)?["code"] as? Int, -32600)
    }

    func testEraIsStickyLegacyRejectsLaterModernRequest() {
        let server = self.server()
        _ = server.handle(line: #"{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{}}}"#)
        let response = server.handle(line: #"{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{\#(validMeta)}}"#)
        XCTAssertEqual(error(response)?["code"] as? Int, -32601)
    }

    // A stray no-method payload (e.g. a JSON-RPC response) is dropped before
    // classification: it produces no output and never selects an era, on a fresh
    // connection or a modern one.
    func testNoMethodPayloadDroppedWithoutClassifyingFreshConnection() {
        let server = self.server()
        XCTAssertNil(server.handle(line: #"{"jsonrpc":"2.0","id":6,"result":{}}"#))
        // Era is still unclassified, so a following modern request classifies modern.
        let response = server.handle(line: #"{"jsonrpc":"2.0","id":7,"method":"server/discover","params":{\#(validMeta)}}"#)
        XCTAssertEqual(result(response)?["resultType"] as? String, "complete")
    }

    func testNoMethodPayloadDroppedOnModernConnectionWithoutChangingEra() {
        let server = self.server()
        _ = server.handle(line: #"{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{\#(validMeta)}}"#)
        XCTAssertNil(server.handle(line: #"{"jsonrpc":"2.0","id":6,"result":{}}"#))
        // Era is unchanged (still modern): a following modern request is served.
        let response = server.handle(line: #"{"jsonrpc":"2.0","id":7,"method":"tools/list","params":{\#(validMeta)}}"#)
        XCTAssertEqual(result(response)?["resultType"] as? String, "complete")
    }

    func testTurnEndedAcceptedInBothErasWithoutResponse() {
        let modern = self.server()
        _ = modern.handle(line: #"{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{\#(validMeta)}}"#)
        XCTAssertNil(modern.handle(line: #"{"jsonrpc":"2.0","method":"notifications/turn-ended","params":{}}"#))

        let legacy = self.server()
        _ = legacy.handle(line: #"{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{}}}"#)
        XCTAssertNil(legacy.handle(line: #"{"jsonrpc":"2.0","method":"notifications/turn-ended","params":{}}"#))
    }

    // MARK: - Envelope validation errors

    func testMissingProtocolVersionReturnsInvalidParams() {
        let response = server().handle(line: #"{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/clientCapabilities":{}}}}"#)
        XCTAssertEqual(error(response)?["code"] as? Int, -32602)
        XCTAssertEqual(error(response)?["message"] as? String, "Invalid params")
    }

    func testMissingClientCapabilitiesReturnsInvalidParams() {
        let response = server().handle(line: #"{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}}"#)
        XCTAssertEqual(error(response)?["code"] as? Int, -32602)
    }

    func testClientCapabilitiesMustBeObject() {
        let response = server().handle(line: #"{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":"nope"}}}"#)
        XCTAssertEqual(error(response)?["code"] as? Int, -32602)
    }

    func testUnsupportedVersionReturnsNegotiationError() {
        let response = server().handle(line: #"{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"1999-01-01","io.modelcontextprotocol/clientCapabilities":{}}}}"#)
        XCTAssertEqual(error(response)?["code"] as? Int, -32022)
        XCTAssertEqual(error(response)?["message"] as? String, "Unsupported protocol version")
        let data = error(response)?["data"] as? [String: Any]
        XCTAssertEqual(data?["supported"] as? [String], ["2026-07-28", "2025-03-26"])
        XCTAssertEqual(data?["requested"] as? String, "1999-01-01")
    }

    func testLegacyVersionIsNotAcceptedAsModernMetadata() {
        let response = server().handle(line: #"{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2025-03-26","io.modelcontextprotocol/clientCapabilities":{}}}}"#)
        XCTAssertEqual(error(response)?["code"] as? Int, -32022)
        XCTAssertEqual((error(response)?["data"] as? [String: Any])?["requested"] as? String, "2025-03-26")
    }

    func testMalformedClientInfoReturnsInvalidParams() {
        let response = server().handle(line: #"{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{},"io.modelcontextprotocol/clientInfo":{"name":"probe"}}}}"#)
        XCTAssertEqual(error(response)?["code"] as? Int, -32602)
    }

    func testInvalidLogLevelReturnsInvalidParams() {
        let response = server().handle(line: #"{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{},"io.modelcontextprotocol/logLevel":"loud"}}}"#)
        XCTAssertEqual(error(response)?["code"] as? Int, -32602)
    }

    func testValidLogLevelIsAccepted() {
        let response = server().handle(line: #"{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{},"io.modelcontextprotocol/logLevel":"debug"}}}"#)
        XCTAssertNil(error(response))
        XCTAssertEqual(result(response)?["resultType"] as? String, "complete")
    }

    // MARK: - Decoration presence/absence per era

    func testModernResultsAreDecorated() {
        let response = server().handle(line: #"{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{\#(validMeta)}}"#)
        let result = self.result(response)
        XCTAssertEqual(result?["resultType"] as? String, "complete")
        XCTAssertEqual(result?["ttlMs"] as? Int, 300000)
        XCTAssertEqual(result?["cacheScope"] as? String, "public")
        let meta = result?["_meta"] as? [String: Any]
        let serverInfo = meta?["io.modelcontextprotocol/serverInfo"] as? [String: Any]
        XCTAssertEqual(serverInfo?["name"] as? String, "open-computer-use")
        XCTAssertNotNil(serverInfo?["version"] as? String)
    }

    func testLegacyResultsAreNotDecorated() {
        let server = self.server()
        _ = server.handle(line: #"{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{}}}"#)
        let response = server.handle(line: #"{"jsonrpc":"2.0","id":2,"method":"tools/list"}"#)
        let result = self.result(response)
        XCTAssertNil(result?["resultType"])
        XCTAssertNil(result?["_meta"])
        XCTAssertNil(result?["ttlMs"])
    }

    func testDecorationMergesIntoExistingMeta() {
        let decorated = server().decorateModern([
            "content": [],
            "_meta": ["custom/key": "keep-me"],
        ])
        XCTAssertEqual(decorated["resultType"] as? String, "complete")
        let meta = decorated["_meta"] as? [String: Any]
        XCTAssertEqual(meta?["custom/key"] as? String, "keep-me", "existing _meta keys are preserved")
        let serverInfo = meta?["io.modelcontextprotocol/serverInfo"] as? [String: Any]
        XCTAssertEqual(serverInfo?["name"] as? String, "open-computer-use")
    }

    // MARK: - server/discover determinism

    func testServerDiscoverIsDeterministic() {
        let first = server().handle(line: #"{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{\#(validMeta)}}"#)
        let second = server().handle(line: #"{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{\#(validMeta)}}"#)
        XCTAssertEqual(first, second)
        XCTAssertEqual(result(first)?["supportedVersions"] as? [String], ["2026-07-28", "2025-03-26"])
    }

    // MARK: - Modern method set

    func testModernRejectsInitialize() {
        let response = server().handle(line: #"{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},\#(validMeta)}}"#)
        XCTAssertEqual(error(response)?["code"] as? Int, -32601)
        XCTAssertEqual(error(response)?["message"] as? String, "Method not found: initialize")
    }

    func testModernRejectsPing() {
        let response = server().handle(line: #"{"jsonrpc":"2.0","id":1,"method":"ping","params":{\#(validMeta)}}"#)
        XCTAssertEqual(error(response)?["code"] as? Int, -32601)
        XCTAssertEqual(error(response)?["message"] as? String, "Method not found: ping")
    }

    // MARK: - Catch-scoping

    // A modern tools/call whose dispatch throws a tool-level error must still be
    // a decorated tool-style isError result, not a JSON-RPC error.
    func testModernToolCallThrowingYieldsDecoratedToolResult() {
        let response = server().handle(line: #"{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_app_state","arguments":{},\#(validMeta)}}"#)
        XCTAssertNil(error(response), "a throwing tool must not become a JSON-RPC error")
        let result = self.result(response)
        XCTAssertEqual(result?["isError"] as? Bool, true)
        XCTAssertEqual(result?["resultType"] as? String, "complete")
        let content = result?["content"] as? [[String: Any]]
        XCTAssertEqual(content?.first?["text"] as? String, "Missing required argument: app")
    }

    // A modern protocol-level failure (malformed envelope) must be a JSON-RPC
    // error, not coerced into a tool-style result.
    func testModernMalformedEnvelopeYieldsProtocolError() {
        let response = server().handle(line: #"{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"get_app_state","arguments":{},"_meta":{"io.modelcontextprotocol/clientCapabilities":{}}}}"#)
        XCTAssertEqual(error(response)?["code"] as? Int, -32602)
        XCTAssertNil(result(response), "a protocol failure must not be a tool result")
    }
}
