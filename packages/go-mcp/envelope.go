package gomcp

import (
	"fmt"
	"os"
)

// logLevels are the RFC-5424-style severities accepted in the optional
// io.modelcontextprotocol/logLevel field. The first implementation emits no MCP
// logging notifications, so the value is only validated and recorded to stderr.
var logLevels = map[string]bool{
	"debug":     true,
	"info":      true,
	"notice":    true,
	"warning":   true,
	"error":     true,
	"critical":  true,
	"alert":     true,
	"emergency": true,
}

// validateEnvelope enforces the modern per-request _meta contract. It returns
// nil when the envelope is valid, or a ready-to-send JSON-RPC error response.
// The caller has already established that the request carries modern metadata.
func validateEnvelope(id any, params map[string]any) map[string]any {
	meta, _ := requestMeta(params)

	version, ok := meta[keyProtocolVersion].(string)
	if !ok || version == "" {
		return errorResponse(id, codeInvalidParams, msgInvalidParams)
	}
	if version != modernVersion {
		return errorResponseData(id, codeUnsupported, msgUnsupported, map[string]any{
			"supported": supportedVersions,
			"requested": version,
		})
	}

	caps, present := meta[keyClientCapabilities]
	if !present {
		return errorResponse(id, codeInvalidParams, msgInvalidParams)
	}
	if _, isObject := caps.(map[string]any); !isObject {
		return errorResponse(id, codeInvalidParams, msgInvalidParams)
	}

	// clientInfo is optional, but when present it must be an object carrying both
	// name and version as strings (matches the Swift MCPRequestEnvelope contract
	// and the MCP Implementation shape). A partial object is malformed metadata.
	if info, present := meta[keyClientInfo]; present {
		obj, isObject := info.(map[string]any)
		if !isObject {
			return errorResponse(id, codeInvalidParams, msgInvalidParams)
		}
		if _, ok := obj["name"].(string); !ok {
			return errorResponse(id, codeInvalidParams, msgInvalidParams)
		}
		if _, ok := obj["version"].(string); !ok {
			return errorResponse(id, codeInvalidParams, msgInvalidParams)
		}
	}

	if level, present := meta[keyLogLevel]; present {
		name, isString := level.(string)
		if !isString || !logLevels[name] {
			return errorResponse(id, codeInvalidParams, msgInvalidParams)
		}
		// Recorded to stderr only; stdout stays JSON-RPC.
		fmt.Fprintf(os.Stderr, "gomcp: client logLevel=%s\n", name)
	}

	return nil
}
