package gomcp

import "math"

// Modern get_app_state structuredContent shape. The key set is pinned and
// byte-identical on every platform; the apps map their own snapshot model into
// StructuredState and call StructuredContent. M2 validates this with injected
// values; real handle minting and wire-level emission land with M3.

// Rect is a window bounds rectangle in the platform's coordinate space.
type Rect struct {
	X      float64
	Y      float64
	Width  float64
	Height float64
}

// Size is a pixel dimension pair for the captured screenshot.
type Size struct {
	Width  int
	Height int
}

// StructuredState carries the injected and snapshot-derived values for a modern
// get_app_state structuredContent block. Timestamps are RFC3339 UTC strings.
// BundleIdentifier, WindowID, and ScreenshotPixels are pointers because the
// platform may not expose them; a nil pointer serializes to JSON null per the
// pinned contract and matches the Swift emission.
type StructuredState struct {
	SnapshotRef      string
	CapturedAt       string
	ExpiresAt        string
	Generation       int
	AppName          string
	BundleIdentifier *string
	PID              int
	WindowID         *string
	Bounds           Rect
	ScreenshotPixels *Size
}

// StructuredContent renders the pinned structuredContent map for modern
// get_app_state. Key set exactly: snapshot_ref, captured_at, expires_at,
// generation, app{name, bundle_identifier, pid}, window{id, bounds{x,y,width,
// height}, screenshot_pixels{width,height}}.
func (s StructuredState) StructuredContent() map[string]any {
	var bundle any
	if s.BundleIdentifier != nil {
		bundle = *s.BundleIdentifier
	}
	var windowID any
	if s.WindowID != nil {
		windowID = *s.WindowID
	}
	var screenshotPixels any
	if s.ScreenshotPixels != nil {
		screenshotPixels = map[string]any{
			"width":  s.ScreenshotPixels.Width,
			"height": s.ScreenshotPixels.Height,
		}
	}
	return map[string]any{
		"snapshot_ref": s.SnapshotRef,
		"captured_at":  s.CapturedAt,
		"expires_at":   s.ExpiresAt,
		"generation":   s.Generation,
		"app": map[string]any{
			"name":              s.AppName,
			"bundle_identifier": bundle,
			"pid":               s.PID,
		},
		"window": map[string]any{
			"id": windowID,
			// Bounds are emitted as integers rounded half away from zero to match
			// the design shape and the Swift emission exactly, including negatives.
			"bounds": map[string]any{
				"x":      roundToInt(s.Bounds.X),
				"y":      roundToInt(s.Bounds.Y),
				"width":  roundToInt(s.Bounds.Width),
				"height": roundToInt(s.Bounds.Height),
			},
			"screenshot_pixels": screenshotPixels,
		},
	}
}

// roundToInt rounds half away from zero, matching Swift's
// .toNearestOrAwayFromZero rounding rule (math.Round has the same behavior).
func roundToInt(v float64) int {
	return int(math.Round(v))
}

// SuccessorRef extracts the successor snapshot_ref a modern get_app_state or
// action result carries in its structuredContent. It returns the handle and true
// only when the "snapshot_ref" key holds a non-empty string; a legacy result (no
// structuredContent) or an error envelope (which carries "error", not
// "snapshot_ref") returns "" and false. The CLI batch path uses it to thread the
// latest successor into subsequent action calls that omit the reference.
func SuccessorRef(structuredContent map[string]any) (string, bool) {
	if structuredContent == nil {
		return "", false
	}
	ref, ok := structuredContent[SnapshotRefKey].(string)
	if !ok || ref == "" {
		return "", false
	}
	return ref, true
}
