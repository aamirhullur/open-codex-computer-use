package gomcp

import (
	"encoding/json"
	"testing"
)

func TestStructuredContentShape(t *testing.T) {
	bundle := "com.example.app"
	windowID := "window-7"
	state := StructuredState{
		SnapshotRef:      "ocu_snapshot_v1_abc",
		CapturedAt:       "2026-08-13T12:00:00Z",
		ExpiresAt:        "2026-08-13T12:02:00Z",
		Generation:       7,
		AppName:          "Example",
		BundleIdentifier: &bundle,
		PID:              1234,
		WindowID:         &windowID,
		Bounds:           Rect{X: 0, Y: 0, Width: 1200, Height: 800},
		ScreenshotPixels: &Size{Width: 2400, Height: 1600},
	}
	sc := state.StructuredContent()

	if sc["snapshot_ref"] != "ocu_snapshot_v1_abc" {
		t.Fatalf("snapshot_ref = %v", sc["snapshot_ref"])
	}
	if sc["captured_at"] != "2026-08-13T12:00:00Z" || sc["expires_at"] != "2026-08-13T12:02:00Z" {
		t.Fatalf("timestamps = %v %v", sc["captured_at"], sc["expires_at"])
	}
	if sc["generation"] != 7 {
		t.Fatalf("generation = %v", sc["generation"])
	}
	app := sc["app"].(map[string]any)
	if app["name"] != "Example" || app["bundle_identifier"] != "com.example.app" || app["pid"] != 1234 {
		t.Fatalf("app = %#v", app)
	}
	window := sc["window"].(map[string]any)
	if window["id"] != "window-7" {
		t.Fatalf("window id = %v", window["id"])
	}
	bounds := window["bounds"].(map[string]any)
	if bounds["x"] != 0 || bounds["y"] != 0 || bounds["width"] != 1200 || bounds["height"] != 800 {
		t.Fatalf("bounds = %#v", bounds)
	}
	px := window["screenshot_pixels"].(map[string]any)
	if px["width"] != 2400 || px["height"] != 1600 {
		t.Fatalf("screenshot_pixels = %#v", px)
	}
}

// TestStructuredContentBoundsRounding freezes integer bounds rounded half away
// from zero (matching Swift .toNearestOrAwayFromZero), including negatives.
func TestStructuredContentBoundsRounding(t *testing.T) {
	state := StructuredState{Bounds: Rect{X: -2.5, Y: 0.5, Width: 1199.4, Height: 800.6}}
	bounds := state.StructuredContent()["window"].(map[string]any)["bounds"].(map[string]any)
	if bounds["x"] != -3 {
		t.Errorf("x = %v, want -3", bounds["x"])
	}
	if bounds["y"] != 1 {
		t.Errorf("y = %v, want 1", bounds["y"])
	}
	if bounds["width"] != 1199 {
		t.Errorf("width = %v, want 1199", bounds["width"])
	}
	if bounds["height"] != 801 {
		t.Errorf("height = %v, want 801", bounds["height"])
	}
}

func TestStructuredContentNullsWhenUnavailable(t *testing.T) {
	state := StructuredState{
		SnapshotRef: "ocu_snapshot_v1_abc",
		AppName:     "Example",
		PID:         1,
	}
	raw, err := json.Marshal(state.StructuredContent())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	app := out["app"].(map[string]any)
	if v, ok := app["bundle_identifier"]; !ok || v != nil {
		t.Fatalf("bundle_identifier should be JSON null, got %v (present=%v)", v, ok)
	}
	window := out["window"].(map[string]any)
	if v, ok := window["id"]; !ok || v != nil {
		t.Fatalf("window.id should be JSON null, got %v (present=%v)", v, ok)
	}
	if v, ok := window["screenshot_pixels"]; !ok || v != nil {
		t.Fatalf("window.screenshot_pixels should be JSON null, got %v (present=%v)", v, ok)
	}
}

// TestStructuredContentScreenshotPixelsPresent confirms the block renders as an
// object when the pixel size is available.
func TestStructuredContentScreenshotPixelsPresent(t *testing.T) {
	state := StructuredState{ScreenshotPixels: &Size{Width: 100, Height: 200}}
	px := state.StructuredContent()["window"].(map[string]any)["screenshot_pixels"].(map[string]any)
	if px["width"] != 100 || px["height"] != 200 {
		t.Fatalf("screenshot_pixels = %#v", px)
	}
}

// TestStructuredContentKeySet freezes the exact top-level and nested key sets.
func TestStructuredContentKeySet(t *testing.T) {
	sc := StructuredState{ScreenshotPixels: &Size{}}.StructuredContent()
	assertKeys(t, "structuredContent", sc, []string{"snapshot_ref", "captured_at", "expires_at", "generation", "app", "window"})
	assertKeys(t, "app", sc["app"].(map[string]any), []string{"name", "bundle_identifier", "pid"})
	window := sc["window"].(map[string]any)
	assertKeys(t, "window", window, []string{"id", "bounds", "screenshot_pixels"})
	assertKeys(t, "bounds", window["bounds"].(map[string]any), []string{"x", "y", "width", "height"})
	assertKeys(t, "screenshot_pixels", window["screenshot_pixels"].(map[string]any), []string{"width", "height"})
}

func assertKeys(t *testing.T, label string, m map[string]any, want []string) {
	t.Helper()
	if len(m) != len(want) {
		t.Fatalf("%s key count = %d, want %d: %#v", label, len(m), len(want), m)
	}
	for _, k := range want {
		if _, ok := m[k]; !ok {
			t.Fatalf("%s missing key %q: %#v", label, k, m)
		}
	}
}

func TestSuccessorRef(t *testing.T) {
	// A modern get_app_state / action result carries a non-empty snapshot_ref.
	sc := StructuredState{SnapshotRef: "ocu_snapshot_v1_abc"}.StructuredContent()
	if ref, ok := SuccessorRef(sc); !ok || ref != "ocu_snapshot_v1_abc" {
		t.Fatalf("SuccessorRef(modern) = (%q, %v), want (ocu_snapshot_v1_abc, true)", ref, ok)
	}

	// A nil structuredContent (legacy result) yields no successor.
	if ref, ok := SuccessorRef(nil); ok || ref != "" {
		t.Fatalf("SuccessorRef(nil) = (%q, %v), want (\"\", false)", ref, ok)
	}

	// An error envelope carries "error", not "snapshot_ref".
	errEnvelope := NewSnapshotError(ErrSnapshotRefStale, MsgSnapshotRefStale).Result()["structuredContent"].(map[string]any)
	if ref, ok := SuccessorRef(errEnvelope); ok || ref != "" {
		t.Fatalf("SuccessorRef(error) = (%q, %v), want (\"\", false)", ref, ok)
	}

	// An empty snapshot_ref string is not a usable successor.
	if ref, ok := SuccessorRef(map[string]any{SnapshotRefKey: ""}); ok || ref != "" {
		t.Fatalf("SuccessorRef(empty) = (%q, %v), want (\"\", false)", ref, ok)
	}

	// A non-string snapshot_ref value is rejected.
	if ref, ok := SuccessorRef(map[string]any{SnapshotRefKey: 42}); ok || ref != "" {
		t.Fatalf("SuccessorRef(non-string) = (%q, %v), want (\"\", false)", ref, ok)
	}
}
