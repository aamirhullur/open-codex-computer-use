package gomcp

import (
	"encoding/json"
	"errors"
	"io"
)

// Hooks are the platform-supplied seams the protocol layer needs. The apps
// provide their instructions/version strings and the tool dispatch callbacks;
// the protocol layer owns everything else.
type Hooks struct {
	// Instructions is the legacy server instructions string surfaced by
	// legacy initialize.
	Instructions string
	// ModernInstructions is the modern server instructions string surfaced by
	// server/discover. It describes the explicit snapshot state chain.
	ModernInstructions string
	// Version is the build version reported in serverInfo.
	Version string
	// ToolCatalog returns the tool catalog value (a marshalable list) for the
	// requested era: modern=false yields the legacy catalog, modern=true yields
	// the modern catalog whose action tools require snapshot_ref.
	ToolCatalog func(modern bool) any
	// CallTool dispatches a tool call and returns its result value (a
	// marshalable tool result). modern reports the connection era so a handler
	// can take the modern path (minting a snapshot handle and returning the
	// structured block) while keeping the legacy result byte-identical.
	CallTool func(name string, args map[string]any, modern bool) any
	// TurnEnded is an optional best-effort cleanup hook for the
	// notifications/turn-ended custom notification. It may be nil.
	TurnEnded func()
}

// Server holds the platform hooks and the connection's era state. A Server is
// classified once, on its first inbound request, and is not safe for concurrent
// use; the stdio loop drives it from a single goroutine.
type Server struct {
	hooks Hooks
	era   protocolEra
}

// NewServer builds a Server bound to the given hooks, starting unclassified.
func NewServer(hooks Hooks) *Server {
	return &Server{hooks: hooks, era: eraUnset}
}

// Run reads newline-delimited JSON-RPC requests from stdin, dispatches each, and
// writes any response to stdout. A payload that is not a JSON-RPC object yields
// a -32700 error; EOF ends the loop. Only JSON-RPC is written to stdout.
func (s *Server) Run(stdin io.Reader, stdout io.Writer) error {
	decoder := json.NewDecoder(stdin)
	encoder := json.NewEncoder(stdout)
	for {
		var request map[string]any
		if err := decoder.Decode(&request); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			_ = encoder.Encode(errorResponse(nil, codeParseError, msgParseError))
			continue
		}
		response := s.Handle(request)
		if response != nil {
			if err := encoder.Encode(response); err != nil {
				return err
			}
		}
	}
}

// Handle dispatches a single decoded JSON-RPC request and returns the response
// map, or nil for notifications and dropped payloads.
func (s *Server) Handle(request map[string]any) map[string]any {
	id := request["id"]
	method, _ := request["method"].(string)
	params, _ := request["params"].(map[string]any)

	// A payload with no method (a stray response) is dropped without effect.
	if method == "" {
		return nil
	}

	// notifications/turn-ended is a best-effort cleanup hook accepted in both
	// eras. It neither classifies the connection nor produces a response.
	if method == "notifications/turn-ended" {
		if s.hooks.TurnEnded != nil {
			s.hooks.TurnEnded()
		}
		return nil
	}

	if s.era == eraUnset {
		s.era = classify(params)
	}

	if s.era == eraModern {
		return s.handleModern(id, method, params)
	}
	return s.handleLegacy(id, method, params)
}

// handleLegacy reproduces the pre-adapter 2025-03-26 behavior byte-for-byte. A
// request bearing modern metadata on a legacy-classified connection is a
// cross-era message; it is rejected with the legacy-shaped -32601 so legacy
// connections only ever emit legacy responses. This never fires for the first
// request (modern metadata classifies the connection modern instead) or for the
// frozen legacy fixtures (none carry modern metadata).
func (s *Server) handleLegacy(id any, method string, params map[string]any) map[string]any {
	if carriesModernMeta(params) {
		return errorResponse(id, codeMethodMissing, "Method not found: "+method)
	}
	switch method {
	case "initialize":
		return result(id, map[string]any{
			"protocolVersion": "2025-03-26",
			"serverInfo": map[string]any{
				"name":    "open-computer-use",
				"version": s.hooks.Version,
			},
			"capabilities": map[string]any{"tools": map[string]any{"listChanged": false}},
			"instructions": s.hooks.Instructions,
		})
	case "notifications/initialized":
		return nil
	case "ping":
		return result(id, map[string]any{})
	case "tools/list":
		return result(id, map[string]any{"tools": s.hooks.ToolCatalog(false)})
	case "tools/call":
		return result(id, s.callTool(params))
	default:
		return errorResponse(id, codeMethodMissing, "Method not found: "+method)
	}
}

// handleModern implements the 2026-07-28 method set: server/discover,
// tools/list, and tools/call. Every modern request must satisfy the envelope
// contract; initialize, ping, and notifications/initialized are not modern
// methods and are rejected. A legacy-form request that reaches a modern
// connection (no modern metadata) is a cross-era message and is rejected.
func (s *Server) handleModern(id any, method string, params map[string]any) map[string]any {
	if !carriesModernMeta(params) {
		return errorResponse(id, codeInvalidReq, msgWrongEra)
	}
	if errResp := validateEnvelope(id, params); errResp != nil {
		return errResp
	}
	switch method {
	case "server/discover":
		return s.decorate(id, s.discoverResult())
	case "tools/list":
		return s.decorate(id, map[string]any{
			"tools":      s.hooks.ToolCatalog(true),
			"ttlMs":      cacheTTLMs,
			"cacheScope": cacheScope,
		})
	case "tools/call":
		return s.decorate(id, s.callTool(params))
	default:
		return errorResponse(id, codeMethodMissing, "Method not found: "+method)
	}
}

// callTool extracts the tool name and arguments and dispatches through the hook,
// passing the connection era so the handler can select the modern path.
func (s *Server) callTool(params map[string]any) any {
	name, _ := params["name"].(string)
	arguments, _ := params["arguments"].(map[string]any)
	if arguments == nil {
		arguments = map[string]any{}
	}
	return s.hooks.CallTool(name, arguments, s.era == eraModern)
}
