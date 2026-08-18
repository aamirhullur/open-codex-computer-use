// Package gomcp is the shared stdio MCP protocol layer for the Go apps
// (Linux and Windows). It owns JSON-RPC framing, dual-era classification,
// the modern request envelope, modern response decoration, server/discover,
// and the stdio loop. Platform apps supply their instructions/version and the
// tool dispatch callbacks; snapshot, service, and tool code stay in the apps.
package gomcp

// JSON-RPC error codes used on the wire. The four negotiated codes and their
// frozen messages match the protocol golden fixtures; do not reword them.
const (
	codeParseError    = -32700
	codeInvalidReq    = -32600
	codeMethodMissing = -32601
	codeInvalidParams = -32602
	codeUnsupported   = -32022
)

const (
	msgParseError    = "Invalid JSON-RPC payload"
	msgInvalidParams = "Invalid params"
	msgUnsupported   = "Unsupported protocol version"
	// msgWrongEra is returned when a request arrives from the era the connection
	// was not classified into. No fixture freezes this message; only the code
	// is contractual.
	msgWrongEra = "This connection uses the modern protocol; send params._meta with io.modelcontextprotocol/protocolVersion"
)

// result builds a JSON-RPC success envelope.
func result(id any, value any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "result": value}
}

// errorResponse builds a JSON-RPC error envelope with no data field.
func errorResponse(id any, code int, message string) map[string]any {
	return map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": code, "message": message},
	}
}

// errorResponseData builds a JSON-RPC error envelope carrying a data payload.
func errorResponseData(id any, code int, message string, data any) map[string]any {
	return map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": code, "message": message, "data": data},
	}
}
