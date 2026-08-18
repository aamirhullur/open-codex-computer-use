package gomcp

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// testHooks returns hooks with a fixed instructions/version and a trivial tool
// dispatcher used across the unit tests.
func testHooks() Hooks {
	return Hooks{
		Instructions:       "test-instructions",
		ModernInstructions: "test-instructions",
		Version:            "9.9.9",
		ToolCatalog: func(modern bool) any {
			return []map[string]any{{"name": "list_apps"}, {"name": "get_app_state"}}
		},
		CallTool: func(name string, args map[string]any) any {
			if name == "get_app_state" {
				if _, ok := args["app"]; !ok {
					return map[string]any{
						"content": []map[string]any{{"type": "text", "text": "Missing required argument: app"}},
						"isError": true,
					}
				}
			}
			return map[string]any{
				"content": []map[string]any{{"type": "text", "text": "ok:" + name}},
				"isError": false,
			}
		},
	}
}

// modernMeta builds a valid modern _meta block.
func modernMeta() map[string]any {
	return map[string]any{
		keyProtocolVersion:    modernVersion,
		keyClientCapabilities: map[string]any{},
		keyClientInfo:         map[string]any{"name": "probe", "version": "0"},
	}
}

func errorCode(t *testing.T, resp map[string]any) int {
	t.Helper()
	errObj, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("response has no error object: %#v", resp)
	}
	return errObj["code"].(int)
}

func TestClassifier(t *testing.T) {
	tests := []struct {
		name   string
		params map[string]any
		want   protocolEra
	}{
		{"modern full meta", map[string]any{"_meta": modernMeta()}, eraModern},
		{"modern meta without version", map[string]any{"_meta": map[string]any{keyClientCapabilities: map[string]any{}}}, eraModern},
		{"legacy initialize", map[string]any{"protocolVersion": "2025-03-26"}, eraLegacy},
		{"legacy bare ping", map[string]any{}, eraLegacy},
		{"meta without modern namespace", map[string]any{"_meta": map[string]any{"progressToken": "x"}}, eraLegacy},
		{"nil params", nil, eraLegacy},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := classify(tc.params); got != tc.want {
				t.Fatalf("classify = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestClassificationIsStickyAcrossRequests(t *testing.T) {
	s := NewServer(testHooks())
	// First request is legacy (bare ping); the connection stays legacy.
	if resp := s.Handle(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "ping"}); resp["result"] == nil {
		t.Fatalf("legacy ping did not return a result: %#v", resp)
	}
	if s.era != eraLegacy {
		t.Fatalf("era = %v, want legacy", s.era)
	}
	// A later modern-form request is a cross-era message on a legacy connection.
	resp := s.Handle(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "server/discover", "params": map[string]any{"_meta": modernMeta()}})
	if code := errorCode(t, resp); code != codeMethodMissing {
		t.Fatalf("cross-era modern method code = %d, want %d", code, codeMethodMissing)
	}
}

func TestLegacyConnectionRejectsLaterModernMetaOnValidMethod(t *testing.T) {
	s := NewServer(testHooks())
	// Classify legacy with a bare initialize.
	s.Handle(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{}})
	// tools/list is a valid legacy method, but carrying modern metadata makes it
	// a cross-era message rejected with the legacy-shaped -32601.
	resp := s.Handle(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list", "params": map[string]any{"_meta": modernMeta()}})
	errObj := resp["error"].(map[string]any)
	if errObj["code"].(int) != codeMethodMissing || errObj["message"] != "Method not found: tools/list" {
		t.Fatalf("cross-era legacy rejection = %#v", errObj)
	}
}

func TestModernAfterLegacyClassificationRejectsBareRequest(t *testing.T) {
	s := NewServer(testHooks())
	s.Handle(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "server/discover", "params": map[string]any{"_meta": modernMeta()}})
	if s.era != eraModern {
		t.Fatalf("era = %v, want modern", s.era)
	}
	// A legacy-form request (no modern meta) on a modern connection is rejected.
	resp := s.Handle(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
	if code := errorCode(t, resp); code != codeInvalidReq {
		t.Fatalf("cross-era legacy request code = %d, want %d", code, codeInvalidReq)
	}
}

func TestEnvelopeValidation(t *testing.T) {
	base := func(meta map[string]any) map[string]any {
		return map[string]any{"_meta": meta}
	}
	tests := []struct {
		name     string
		params   map[string]any
		wantErr  bool
		wantCode int
	}{
		{
			name:    "valid",
			params:  base(modernMeta()),
			wantErr: false,
		},
		{
			name:     "missing version",
			params:   base(map[string]any{keyClientCapabilities: map[string]any{}}),
			wantErr:  true,
			wantCode: codeInvalidParams,
		},
		{
			name:     "missing capabilities",
			params:   base(map[string]any{keyProtocolVersion: modernVersion}),
			wantErr:  true,
			wantCode: codeInvalidParams,
		},
		{
			name:     "capabilities wrong type",
			params:   base(map[string]any{keyProtocolVersion: modernVersion, keyClientCapabilities: "nope"}),
			wantErr:  true,
			wantCode: codeInvalidParams,
		},
		{
			name:     "unsupported version",
			params:   base(map[string]any{keyProtocolVersion: "1999-01-01", keyClientCapabilities: map[string]any{}}),
			wantErr:  true,
			wantCode: codeUnsupported,
		},
		{
			name:     "clientInfo wrong shape",
			params:   base(map[string]any{keyProtocolVersion: modernVersion, keyClientCapabilities: map[string]any{}, keyClientInfo: map[string]any{"name": 5}}),
			wantErr:  true,
			wantCode: codeInvalidParams,
		},
		{
			name:     "clientInfo missing version",
			params:   base(map[string]any{keyProtocolVersion: modernVersion, keyClientCapabilities: map[string]any{}, keyClientInfo: map[string]any{"name": "probe"}}),
			wantErr:  true,
			wantCode: codeInvalidParams,
		},
		{
			name:     "clientInfo missing name",
			params:   base(map[string]any{keyProtocolVersion: modernVersion, keyClientCapabilities: map[string]any{}, keyClientInfo: map[string]any{"version": "0"}}),
			wantErr:  true,
			wantCode: codeInvalidParams,
		},
		{
			name:    "clientInfo complete",
			params:  base(map[string]any{keyProtocolVersion: modernVersion, keyClientCapabilities: map[string]any{}, keyClientInfo: map[string]any{"name": "probe", "version": "0"}}),
			wantErr: false,
		},
		{
			name:     "bad logLevel",
			params:   base(map[string]any{keyProtocolVersion: modernVersion, keyClientCapabilities: map[string]any{}, keyLogLevel: "loud"}),
			wantErr:  true,
			wantCode: codeInvalidParams,
		},
		{
			name:    "good logLevel",
			params:  base(map[string]any{keyProtocolVersion: modernVersion, keyClientCapabilities: map[string]any{}, keyLogLevel: "debug"}),
			wantErr: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := validateEnvelope(7, tc.params)
			if tc.wantErr {
				if resp == nil {
					t.Fatalf("expected error, got nil")
				}
				if code := errorCode(t, resp); code != tc.wantCode {
					t.Fatalf("code = %d, want %d", code, tc.wantCode)
				}
				return
			}
			if resp != nil {
				t.Fatalf("expected no error, got %#v", resp)
			}
		})
	}
}

func TestUnsupportedVersionCarriesData(t *testing.T) {
	resp := validateEnvelope(4, map[string]any{"_meta": map[string]any{
		keyProtocolVersion:    "1999-01-01",
		keyClientCapabilities: map[string]any{},
	}})
	errObj := resp["error"].(map[string]any)
	data := errObj["data"].(map[string]any)
	if data["requested"] != "1999-01-01" {
		t.Fatalf("requested = %v", data["requested"])
	}
	supported := data["supported"].([]string)
	if len(supported) != 2 || supported[0] != "2026-07-28" || supported[1] != "2025-03-26" {
		t.Fatalf("supported = %v", supported)
	}
}

func TestDecorationAddsResultTypeAndServerInfo(t *testing.T) {
	s := NewServer(testHooks())
	resp := s.decorate(3, map[string]any{"content": []any{}, "isError": true})
	body := resp["result"].(map[string]any)
	if body["resultType"] != "complete" {
		t.Fatalf("resultType = %v", body["resultType"])
	}
	meta := body["_meta"].(map[string]any)
	info := meta[metaPrefix+"serverInfo"].(map[string]any)
	if info["name"] != serverName || info["version"] != "9.9.9" {
		t.Fatalf("serverInfo = %#v", info)
	}
	// Original fields survive decoration.
	if body["isError"] != true {
		t.Fatalf("isError lost: %#v", body)
	}
}

func TestDecorationMergesExistingMeta(t *testing.T) {
	s := NewServer(testHooks())
	resp := s.decorate(1, map[string]any{
		"tools": []any{},
		"_meta": map[string]any{"vendor/hint": "keep-me"},
	})
	body := resp["result"].(map[string]any)
	meta := body["_meta"].(map[string]any)
	if meta["vendor/hint"] != "keep-me" {
		t.Fatalf("existing _meta key clobbered: %#v", meta)
	}
	info, ok := meta[metaPrefix+"serverInfo"].(map[string]any)
	if !ok || info["name"] != serverName {
		t.Fatalf("serverInfo not merged in: %#v", meta)
	}
}

func TestDecorationConvertsStructResult(t *testing.T) {
	s := NewServer(testHooks())
	type toolResult struct {
		Content []map[string]any `json:"content"`
		IsError bool             `json:"isError"`
	}
	resp := s.decorate(1, toolResult{Content: []map[string]any{{"type": "text", "text": "x"}}, IsError: false})
	body := resp["result"].(map[string]any)
	if body["resultType"] != "complete" {
		t.Fatalf("resultType = %v", body["resultType"])
	}
	if _, ok := body["content"]; !ok {
		t.Fatalf("content lost after struct conversion: %#v", body)
	}
}

func TestDiscoverIsDeterministic(t *testing.T) {
	s := NewServer(testHooks())
	first := s.decorate(1, s.discoverResult())
	second := s.decorate(1, s.discoverResult())
	a, _ := json.Marshal(first)
	b, _ := json.Marshal(second)
	if string(a) != string(b) {
		t.Fatalf("discover not deterministic:\n%s\n%s", a, b)
	}
	body := first["result"].(map[string]any)
	if body["ttlMs"] != cacheTTLMs {
		t.Fatalf("ttlMs = %v", body["ttlMs"])
	}
	if body["cacheScope"] != "public" {
		t.Fatalf("cacheScope = %v", body["cacheScope"])
	}
	caps := body["capabilities"].(map[string]any)
	tools := caps["tools"].(map[string]any)
	if tools["listChanged"] != false {
		t.Fatalf("listChanged = %v", tools["listChanged"])
	}
	if body["instructions"] != "test-instructions" {
		t.Fatalf("instructions = %v", body["instructions"])
	}
}

func TestModernMethodSet(t *testing.T) {
	rejected := []string{"initialize", "ping", "notifications/initialized"}
	for _, method := range rejected {
		t.Run(method, func(t *testing.T) {
			s := NewServer(testHooks())
			resp := s.Handle(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": map[string]any{"_meta": modernMeta()}})
			if code := errorCode(t, resp); code != codeMethodMissing {
				t.Fatalf("%s code = %d, want %d", method, code, codeMethodMissing)
			}
		})
	}
}

func TestTurnEndedAcceptedInBothEras(t *testing.T) {
	called := 0
	hooks := testHooks()
	hooks.TurnEnded = func() { called++ }

	// Legacy connection.
	legacy := NewServer(hooks)
	legacy.Handle(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{}})
	if resp := legacy.Handle(map[string]any{"jsonrpc": "2.0", "method": "notifications/turn-ended"}); resp != nil {
		t.Fatalf("turn-ended returned a response: %#v", resp)
	}

	// Modern connection.
	modern := NewServer(hooks)
	modern.Handle(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "server/discover", "params": map[string]any{"_meta": modernMeta()}})
	if resp := modern.Handle(map[string]any{"jsonrpc": "2.0", "method": "notifications/turn-ended"}); resp != nil {
		t.Fatalf("turn-ended returned a response: %#v", resp)
	}
	if called != 2 {
		t.Fatalf("turn-ended hook called %d times, want 2", called)
	}
}

func TestTurnEndedDoesNotClassify(t *testing.T) {
	s := NewServer(testHooks())
	s.Handle(map[string]any{"jsonrpc": "2.0", "method": "notifications/turn-ended"})
	if s.era != eraUnset {
		t.Fatalf("turn-ended classified the connection: era = %v", s.era)
	}
}

func TestLegacyBehaviorUnchanged(t *testing.T) {
	s := NewServer(testHooks())
	// ping
	if resp := s.Handle(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "ping"}); len(resp["result"].(map[string]any)) != 0 {
		t.Fatalf("ping result not empty: %#v", resp)
	}
	// unknown method
	resp := s.Handle(map[string]any{"jsonrpc": "2.0", "id": 5, "method": "no/such/method"})
	errObj := resp["error"].(map[string]any)
	if errObj["message"] != "Method not found: no/such/method" {
		t.Fatalf("unknown method message = %v", errObj["message"])
	}
}

func TestRunEmitsParseError(t *testing.T) {
	s := NewServer(testHooks())
	var out bytes.Buffer
	if err := s.Run(strings.NewReader("[]\n"), &out); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var resp map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	errObj := resp["error"].(map[string]any)
	if int(errObj["code"].(float64)) != codeParseError {
		t.Fatalf("parse error code = %v", errObj["code"])
	}
}

func TestBareResponseDropped(t *testing.T) {
	s := NewServer(testHooks())
	if resp := s.Handle(map[string]any{"jsonrpc": "2.0", "id": 6, "result": map[string]any{}}); resp != nil {
		t.Fatalf("bare response produced output: %#v", resp)
	}
}
