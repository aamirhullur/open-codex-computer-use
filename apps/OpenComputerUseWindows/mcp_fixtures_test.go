package main

// Cross-platform MCP protocol fixture runner (Windows copy).
//
// This file is a deliberate mirror copy of the same runner in
// apps/OpenComputerUseLinux/mcp_fixtures_test.go. The only intended
// difference between the two copies is fixturePlatformDir below. The shared
// logic is extracted into a common Go package in M1; until then the duplication
// is intentional so M0 can freeze behavior without introducing a shared module.
//
// The runner drives handleMCPRequest directly (the in-process entry point) and
// compares normalized JSON against golden fixtures under
// ../../tests/mcp-protocol-fixtures. See that directory's README.md for the
// fixture format and normalization rules.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// fixturePlatformDir names the platform subdirectory of legacy/ whose golden
// files this binary owns. Legacy initialize and tools/list responses embed
// platform-specific instruction text and tool descriptions, so those cases are
// frozen per platform; the flat legacy/ directory holds the cases whose bytes
// are identical on macOS, Linux, and Windows.
const fixturePlatformDir = "windows"

const fixturesRoot = "../../tests/mcp-protocol-fixtures"

type fixtureStep struct {
	Request    map[string]any  `json:"request"`
	RequestRaw *string         `json:"request_raw"`
	Expect     json.RawMessage `json:"expect"`
}

type fixtureCase struct {
	Name  string        `json:"name"`
	Steps []fixtureStep `json:"steps"`
}

var iso8601Re = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})$`)

// normalizeFixtureValue applies the fixture normalization rules: recursively
// sort object keys (handled by json.Marshal at compare time), replace a
// serverInfo version with <VERSION>, replace opaque snapshot handles with
// <SNAPSHOT_REF>, and replace ISO-8601 timestamps with <TIMESTAMP>.
func normalizeFixtureValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = normalizeFixtureValue(val)
		}
		// A serverInfo-shaped object carries both name and version; freeze the
		// build version so fixtures survive version bumps.
		if _, hasName := out["name"]; hasName {
			if _, hasVersion := out["version"]; hasVersion {
				out["version"] = "<VERSION>"
			}
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = normalizeFixtureValue(val)
		}
		return out
	case string:
		if len(t) >= len("ocu_snapshot_v1_") && t[:len("ocu_snapshot_v1_")] == "ocu_snapshot_v1_" {
			return "<SNAPSHOT_REF>"
		}
		if iso8601Re.MatchString(t) {
			return "<TIMESTAMP>"
		}
		return t
	default:
		return v
	}
}

// canonicalFixtureJSON normalizes a value and marshals it with sorted keys.
func canonicalFixtureJSON(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(normalizeFixtureValue(v))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(data)
}

// runFixtureStep executes one step against a fresh-per-case service and returns
// the normalized actual response as canonical JSON, plus the expected canonical
// JSON. A nil response (notifications, dropped payloads) canonicalizes to null.
func runFixtureStep(t *testing.T, svc *service, step fixtureStep) (actual string, expected string) {
	t.Helper()

	var response map[string]any
	if step.RequestRaw != nil {
		// Drive the real runMCP loop so a regression in its decode/parse-error
		// handling is caught here rather than being masked by a reimplementation.
		response = runRawLine(t, *step.RequestRaw)
	} else {
		response = handleMCPRequest(step.Request, svc)
	}

	// Round-trip the response through JSON so Go structs (tool definitions,
	// tool results) become generic maps and slices and normalize the same way
	// the parsed expectation does. A nil response marshals to null.
	raw, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	var actualValue any
	if err := json.Unmarshal(raw, &actualValue); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	actual = canonicalFixtureJSON(t, actualValue)

	var expectValue any
	if len(step.Expect) > 0 {
		if err := json.Unmarshal(step.Expect, &expectValue); err != nil {
			t.Fatalf("unmarshal expect: %v", err)
		}
	}
	expected = canonicalFixtureJSON(t, expectValue)
	return actual, expected
}

// runRawLine feeds a verbatim line through the real runMCP loop (the production
// stdio decode path) and returns the single decoded response, or nil when runMCP
// produced no output line.
func runRawLine(t *testing.T, raw string) map[string]any {
	t.Helper()
	var out bytes.Buffer
	if err := runMCP(strings.NewReader(raw+"\n"), &out); err != nil {
		t.Fatalf("runMCP: %v", err)
	}
	trimmed := bytes.TrimSpace(out.Bytes())
	if len(trimmed) == 0 {
		return nil
	}
	var resp map[string]any
	if err := json.Unmarshal(trimmed, &resp); err != nil {
		t.Fatalf("decode runMCP output %q: %v", string(trimmed), err)
	}
	return resp
}

func loadFixtureCase(t *testing.T, path string) fixtureCase {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var c fixtureCase
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	if c.Name == "" {
		t.Fatalf("fixture %s has no name", path)
	}
	return c
}

func fixtureFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir %s: %v", dir, err)
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if filepath.Ext(name) != ".json" {
			continue
		}
		if name == "EXPECTED_FAILURES.json" {
			continue
		}
		files = append(files, filepath.Join(dir, name))
	}
	sort.Strings(files)
	if len(files) == 0 {
		t.Fatalf("no fixture files in %s", dir)
	}
	return files
}

// TestMCPProtocolFixturesLegacy runs the shared legacy cases plus this
// platform's legacy cases and requires each step to match exactly.
func TestMCPProtocolFixturesLegacy(t *testing.T) {
	dirs := []string{
		filepath.Join(fixturesRoot, "legacy"),
		filepath.Join(fixturesRoot, "legacy", fixturePlatformDir),
	}
	var files []string
	for _, d := range dirs {
		files = append(files, fixtureFiles(t, d)...)
	}
	if len(files) == 0 {
		t.Fatal("no legacy fixtures found")
	}
	for _, path := range files {
		c := loadFixtureCase(t, path)
		t.Run(c.Name, func(t *testing.T) {
			svc := newService()
			for i, step := range c.Steps {
				actual, expected := runFixtureStep(t, svc, step)
				if actual != expected {
					t.Errorf("step %d mismatch\n  expected: %s\n  actual:   %s", i, expected, actual)
				}
			}
		})
	}
}

type expectedFailures struct {
	Cases map[string]string `json:"cases"`
}

func loadExpectedFailures(t *testing.T) expectedFailures {
	t.Helper()
	path := filepath.Join(fixturesRoot, "modern", "EXPECTED_FAILURES.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var ef expectedFailures
	if err := json.Unmarshal(data, &ef); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return ef
}

// TestMCPProtocolFixturesModern runs the modern 2026-07-28 target fixtures.
// A case listed in EXPECTED_FAILURES.json is required to currently mismatch its
// target shape (proving the case executes and the feature is not yet
// implemented); if it unexpectedly matches, the test fails so the manifest entry
// is retired. A modern case absent from the manifest is enforced like a legacy
// case. This keeps the suite green at M0 while flipping to enforcing as M1 lands.
func TestMCPProtocolFixturesModern(t *testing.T) {
	dir := filepath.Join(fixturesRoot, "modern")
	files := fixtureFiles(t, dir)
	if len(files) == 0 {
		t.Fatal("no modern fixtures found")
	}
	ef := loadExpectedFailures(t)
	discovered := map[string]bool{}
	for _, path := range files {
		c := loadFixtureCase(t, path)
		discovered[c.Name] = true
		t.Run(c.Name, func(t *testing.T) {
			svc := newService()
			allMatch := true
			for _, step := range c.Steps {
				actual, expected := runFixtureStep(t, svc, step)
				if actual != expected {
					allMatch = false
				}
			}
			_, expectedFail := ef.Cases[c.Name]
			if expectedFail {
				if allMatch {
					t.Errorf("modern case %q now matches its target shape; remove it from modern/EXPECTED_FAILURES.json", c.Name)
				}
				return
			}
			if !allMatch {
				t.Errorf("modern case %q is not in EXPECTED_FAILURES.json but does not match its target shape", c.Name)
			}
		})
	}
	// Guard against manifest rot in the reverse direction: every listed key must
	// name a modern fixture that actually exists.
	for name := range ef.Cases {
		if !discovered[name] {
			t.Errorf("EXPECTED_FAILURES.json lists %q but no modern fixture has that name", name)
		}
	}
}
