import Foundation

// Dual-era MCP protocol adapter.
//
// A single stdio connection speaks exactly one protocol era, decided on its
// first inbound request and sticky thereafter. Legacy is the pre-existing
// 2025-03-26 surface (initialize handshake, ping); modern is the stateless
// 2026-07-28 surface (per-request _meta, server/discover, no ping). Wire parsing
// lives here and in MCPRequestEnvelope; native automation stays in the service.
enum ProtocolEra {
    case legacy20250326
    case modern20260728
}

// Wire constants shared by the adapter. The modern _meta keys use the
// io.modelcontextprotocol/ namespace and are matched verbatim; the error
// message strings match the frozen protocol fixtures exactly.
enum MCPProtocol {
    static let legacyProtocolVersion = "2025-03-26"
    static let modernProtocolVersion = "2026-07-28"
    // Advertised in server/discover and in -32022 diagnostics. Order is frozen.
    static let supportedVersions = ["2026-07-28", "2025-03-26"]

    static let metaField = "_meta"
    static let protocolVersionKey = "io.modelcontextprotocol/protocolVersion"
    static let clientCapabilitiesKey = "io.modelcontextprotocol/clientCapabilities"
    static let clientInfoKey = "io.modelcontextprotocol/clientInfo"
    static let logLevelKey = "io.modelcontextprotocol/logLevel"
    static let serverInfoKey = "io.modelcontextprotocol/serverInfo"

    static let serverName = "open-computer-use"

    // RFC-5424 severities, lowercased. Only validated and recorded; the first
    // implementation emits no MCP logging notifications.
    static let logLevels: Set<String> = [
        "debug", "info", "notice", "warning", "error", "critical", "alert", "emergency",
    ]

    static let cacheTtlMs = 300_000
    static let cacheScopePublic = "public"

    // True when params._meta carries any modern namespace key. This is the sole
    // signal used to classify a connection as modern, so a first request that
    // carries modern _meta but omits a required field still routes to the modern
    // path and returns a modern -32602 rather than being misread as ambiguous.
    static func hasModernMeta(params: [String: Any]) -> Bool {
        guard let meta = params[metaField] as? [String: Any] else { return false }
        return meta.keys.contains { $0.hasPrefix("io.modelcontextprotocol/") }
    }
}

// A protocol-level failure that must surface as a JSON-RPC error object, not a
// tool-style isError result. Envelope validation and era rejections raise this;
// the adapter catches it in the modern path and encodes error{code,message,data}.
struct MCPProtocolFailure: Error {
    let code: Int
    let message: String
    let data: [String: Any]?

    init(code: Int, message: String, data: [String: Any]? = nil) {
        self.code = code
        self.message = message
        self.data = data
    }

    static let invalidParams = MCPProtocolFailure(code: -32602, message: "Invalid params")

    static func methodNotFound(_ method: String) -> MCPProtocolFailure {
        MCPProtocolFailure(code: -32601, message: "Method not found: \(method)")
    }

    static func unsupportedVersion(_ requested: String) -> MCPProtocolFailure {
        MCPProtocolFailure(
            code: -32022,
            message: "Unsupported protocol version",
            data: ["supported": MCPProtocol.supportedVersions, "requested": requested]
        )
    }
}
