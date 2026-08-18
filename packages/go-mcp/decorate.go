package gomcp

import "encoding/json"

// Cache hints attached to modern discovery and tool-list results. Discovery,
// instructions, and the tool catalog do not vary by caller, so public caching
// for five minutes limits stale metadata after a local upgrade without repeated
// probes within a normal host lifetime.
const (
	cacheTTLMs = 300000
	cacheScope = "public"
)

const serverName = "open-computer-use"

// discoverResult is the deterministic server/discover body before decoration.
func (s *Server) discoverResult() map[string]any {
	return map[string]any{
		"supportedVersions": supportedVersions,
		"capabilities":      map[string]any{"tools": map[string]any{"listChanged": false}},
		"instructions":      s.hooks.ModernInstructions,
		"ttlMs":             cacheTTLMs,
		"cacheScope":        cacheScope,
	}
}

// decorate wraps a successful modern result in a JSON-RPC envelope after adding
// the two wire-only fields every modern result carries: resultType "complete"
// and the server metadata block. The tool-service code never adds these; the
// adapter adds them centrally here. Legacy results never pass through decorate.
func (s *Server) decorate(id any, value any) map[string]any {
	body := asObject(value)
	body["resultType"] = "complete"
	// Merge serverInfo into any _meta the result already carries so other keys
	// survive decoration; only start a fresh map when there is none.
	meta, ok := body["_meta"].(map[string]any)
	if !ok {
		meta = map[string]any{}
	}
	meta[metaPrefix+"serverInfo"] = map[string]any{
		"name":    serverName,
		"version": s.hooks.Version,
	}
	body["_meta"] = meta
	return result(id, body)
}

// asObject returns value as a mutable map. Maps the adapter builds itself are
// returned as-is; a tool result struct is converted through a JSON round-trip so
// its exported fields (content, isError) become map entries the decorator can
// extend.
func asObject(value any) map[string]any {
	if m, ok := value.(map[string]any); ok {
		return m
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return map[string]any{}
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return map[string]any{}
	}
	return out
}
