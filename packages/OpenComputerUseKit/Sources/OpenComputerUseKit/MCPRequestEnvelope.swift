import Foundation

// The parsed modern per-request _meta. Every modern request carries one; it is
// evaluated for that request only, never accumulated into session state.
struct MCPClientInfo {
    let name: String
    let version: String
}

struct MCPRequestEnvelope {
    let protocolVersion: String
    let clientCapabilities: [String: Any]
    let clientInfo: MCPClientInfo?
    let logLevel: String?

    // Validates params._meta for a modern request. Missing or malformed required
    // metadata raises -32602; an unsupported version raises -32022 with the
    // supported/requested data payload. Callers reach this only after the
    // connection is classified modern (params carry a modern namespace key).
    static func parse(params: [String: Any]) throws -> MCPRequestEnvelope {
        let meta = params[MCPProtocol.metaField] as? [String: Any] ?? [:]

        guard let version = meta[MCPProtocol.protocolVersionKey] as? String, !version.isEmpty else {
            throw MCPProtocolFailure.invalidParams
        }
        // The legacy revision is never accepted as per-request modern metadata;
        // legacy semantics begin with initialize, not with modern _meta.
        guard version == MCPProtocol.modernProtocolVersion else {
            throw MCPProtocolFailure.unsupportedVersion(version)
        }

        guard let capabilities = meta[MCPProtocol.clientCapabilitiesKey] as? [String: Any] else {
            throw MCPProtocolFailure.invalidParams
        }

        var clientInfo: MCPClientInfo?
        if let raw = meta[MCPProtocol.clientInfoKey] {
            guard let dict = raw as? [String: Any],
                  let name = dict["name"] as? String,
                  let version = dict["version"] as? String else {
                throw MCPProtocolFailure.invalidParams
            }
            clientInfo = MCPClientInfo(name: name, version: version)
        }

        var logLevel: String?
        if let raw = meta[MCPProtocol.logLevelKey] {
            guard let level = raw as? String, MCPProtocol.logLevels.contains(level) else {
                throw MCPProtocolFailure.invalidParams
            }
            logLevel = level
        }

        return MCPRequestEnvelope(
            protocolVersion: version,
            clientCapabilities: capabilities,
            clientInfo: clientInfo,
            logLevel: logLevel
        )
    }
}
