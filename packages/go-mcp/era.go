package gomcp

import "strings"

// protocolEra is the protocol behavior a connection is classified into on its first
// inbound request. It is connection routing state for backward compatibility,
// not an MCP application session.
type protocolEra int

const (
	// eraUnset is the state before the first classifying request.
	eraUnset protocolEra = iota
	// eraLegacy is the 2025-03-26 behavior: initialize/ping and undecorated
	// results.
	eraLegacy
	// eraModern is the 2026-07-28 behavior: server/discover, per-request
	// envelope, and decorated results.
	eraModern
)

// metaPrefix namespaces the modern per-request metadata keys.
const metaPrefix = "io.modelcontextprotocol/"

const (
	keyProtocolVersion    = metaPrefix + "protocolVersion"
	keyClientCapabilities = metaPrefix + "clientCapabilities"
	keyClientInfo         = metaPrefix + "clientInfo"
	keyLogLevel           = metaPrefix + "logLevel"
)

// supportedVersions is the dual-era negotiation set advertised in discovery and
// error diagnostics. The modern version is the only value accepted as
// per-request metadata; the legacy version begins with initialize.
var supportedVersions = []string{"2026-07-28", "2025-03-26"}

const modernVersion = "2026-07-28"

// requestMeta returns the params._meta object when present.
func requestMeta(params map[string]any) (map[string]any, bool) {
	meta, ok := params["_meta"].(map[string]any)
	return meta, ok
}

// carriesModernMeta reports whether params._meta carries any modern metadata
// key. Presence (not validity) is what selects the modern era: a request whose
// _meta omits protocolVersion or names an unsupported one is still modern and is
// answered with a modern envelope error rather than falling through to legacy.
func carriesModernMeta(params map[string]any) bool {
	meta, ok := requestMeta(params)
	if !ok {
		return false
	}
	for k := range meta {
		if strings.HasPrefix(k, metaPrefix) {
			return true
		}
	}
	return false
}

// classify decides the era for a first inbound request. Modern metadata selects
// modern; a bare initialize selects legacy; every other opening request without
// modern metadata is handled as legacy so that a single legacy request (ping,
// an unknown method, a direct tools/call) behaves exactly as the pre-adapter
// server did. This keeps the frozen legacy fixtures byte-identical.
func classify(params map[string]any) protocolEra {
	if carriesModernMeta(params) {
		return eraModern
	}
	return eraLegacy
}
