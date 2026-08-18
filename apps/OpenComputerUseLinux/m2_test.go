package main

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/iFurySt/open-codex-computer-use/packages/gomcp"
)

// TestLegacyCatalogUnchanged pins the legacy era to the exact current
// toolDefinitions() output.
func TestLegacyCatalogUnchanged(t *testing.T) {
	if !reflect.DeepEqual(toolDefinitionsForEra(false), toolDefinitions()) {
		t.Fatal("legacy era catalog diverged from toolDefinitions()")
	}
}

// TestModernCatalogSnapshotRef verifies the modern era adds snapshot_ref to
// exactly the seven action tools (appended last to required), leaves the two
// capture tools without a handle, and keeps the nine-tool order.
func TestModernCatalogSnapshotRef(t *testing.T) {
	legacy := toolDefinitions()
	modern := toolDefinitionsForEra(true)
	if len(modern) != len(legacy) {
		t.Fatalf("modern catalog size = %d, want %d", len(modern), len(legacy))
	}

	withRef := 0
	for i := range modern {
		if modern[i].Name != legacy[i].Name {
			t.Fatalf("tool order diverged at %d: %q vs %q", i, modern[i].Name, legacy[i].Name)
		}
		props, _ := modern[i].InputSchema["properties"].(map[string]any)
		_, hasProp := props[gomcp.SnapshotRefKey]
		req, _ := modern[i].InputSchema["required"].([]string)
		hasReq := len(req) > 0 && req[len(req)-1] == gomcp.SnapshotRefKey

		if gomcp.IsModernActionTool(modern[i].Name) {
			if !hasProp {
				t.Errorf("%s missing snapshot_ref property", modern[i].Name)
			}
			if !hasReq {
				t.Errorf("%s does not require snapshot_ref last: %v", modern[i].Name, req)
			}
			if p := props[gomcp.SnapshotRefKey].(map[string]any); p["description"] != gomcp.SnapshotRefDescription {
				t.Errorf("%s snapshot_ref description = %v", modern[i].Name, p["description"])
			}
			withRef++
		} else {
			if hasProp || hasReq {
				t.Errorf("capture tool %s should not carry snapshot_ref", modern[i].Name)
			}
		}
	}
	if withRef != 7 {
		t.Fatalf("snapshot_ref present on %d tools, want 7", withRef)
	}
}

// TestModernCaptureDescriptions verifies the two capture tools get modern
// descriptions and every other description matches the legacy catalog.
func TestModernCaptureDescriptions(t *testing.T) {
	legacy := toolDefinitions()
	modern := toolDefinitionsForEra(true)
	for i := range modern {
		switch modern[i].Name {
		case "get_app_state":
			if modern[i].Description != modernGetAppStateDescription {
				t.Errorf("get_app_state modern description not applied")
			}
		case "list_apps":
			if modern[i].Description != modernListAppsDescription {
				t.Errorf("list_apps modern description not applied")
			}
		default:
			if modern[i].Description != legacy[i].Description {
				t.Errorf("%s description changed in modern era", modern[i].Name)
			}
		}
	}
}

// TestLegacyBuildUnaffectedByModern confirms building the modern catalog does not
// mutate the shared legacy catalog through aliased maps.
func TestLegacyBuildUnaffectedByModern(t *testing.T) {
	_ = toolDefinitionsForEra(true)
	for _, tool := range toolDefinitionsForEra(false) {
		props := tool.InputSchema["properties"].(map[string]any)
		if _, leaked := props[gomcp.SnapshotRefKey]; leaked {
			t.Fatalf("legacy tool %s leaked snapshot_ref after modern build", tool.Name)
		}
	}
}

// TestStructuredContentOmitEmpty verifies structuredContent is absent on the wire
// when nil and present when populated.
func TestStructuredContentOmitEmpty(t *testing.T) {
	raw, _ := json.Marshal(textResult("hi", false))
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if _, present := out["structuredContent"]; present {
		t.Fatalf("structuredContent should be omitted when nil: %s", raw)
	}

	res := textResult("hi", false)
	res.StructuredContent = map[string]any{"error": map[string]any{"code": "x"}}
	raw, _ = json.Marshal(res)
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if _, present := out["structuredContent"]; !present {
		t.Fatalf("structuredContent should be present when set: %s", raw)
	}
}

// TestModernStateResult verifies the injected structured-shape builder prepends
// the snapshot_ref text line and attaches the pinned structuredContent block.
func TestModernStateResult(t *testing.T) {
	bundle := "org.example.App"
	snap := &appSnapshot{
		App:                 appDescriptor{Name: "Example", BundleIdentifier: bundle, PID: 4321},
		WindowTitle:         "Main",
		WindowBounds:        &frame{X: 0, Y: 0, Width: 1200, Height: 800},
		ScreenshotPNGBase64: "AAA",
		TreeLines:           []string{"[0] window"},
	}
	windowID := "win-1"
	state := gomcp.StructuredState{
		SnapshotRef:      "ocu_snapshot_v1_abc123",
		CapturedAt:       "2026-08-13T12:00:00Z",
		ExpiresAt:        "2026-08-13T12:02:00Z",
		Generation:       3,
		AppName:          snap.App.Name,
		BundleIdentifier: &bundle,
		PID:              snap.App.PID,
		WindowID:         &windowID,
		Bounds:           gomcp.Rect{X: 0, Y: 0, Width: 1200, Height: 800},
		ScreenshotPixels: &gomcp.Size{Width: 1200, Height: 800},
	}
	res := snap.modernStateResult(state)

	if res.Content[0].Type != "text" || res.Content[0].Text[:13] != "snapshot_ref:" {
		t.Fatalf("text does not lead with snapshot_ref: %q", res.Content[0].Text)
	}
	if res.StructuredContent["snapshot_ref"] != "ocu_snapshot_v1_abc123" {
		t.Fatalf("structuredContent snapshot_ref = %v", res.StructuredContent["snapshot_ref"])
	}
	if res.StructuredContent["generation"] != 3 {
		t.Fatalf("generation = %v", res.StructuredContent["generation"])
	}
	// Screenshot image content survives after the prepend.
	if res.Content[1].Type != "image" {
		t.Fatalf("expected image content, got %v", res.Content[1].Type)
	}
}
