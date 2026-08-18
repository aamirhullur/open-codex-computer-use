package main

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"os"
	"testing"

	"github.com/iFurySt/open-codex-computer-use/packages/gomcp"
)

// fakePNGBase64 builds the minimum PNG prefix (signature + IHDR) carrying the
// given pixel dimensions, standard-base64 encoded like both runtimes emit.
// pngPixelSize reads only the IHDR header, so the pixel data is unnecessary.
func fakePNGBase64(width, height int) string {
	buf := make([]byte, 24)
	copy(buf[0:8], []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a})
	binary.BigEndian.PutUint32(buf[8:12], 13)
	copy(buf[12:16], []byte("IHDR"))
	binary.BigEndian.PutUint32(buf[16:20], uint32(width))
	binary.BigEndian.PutUint32(buf[20:24], uint32(height))
	return base64.StdEncoding.EncodeToString(buf)
}

// fakeSnapshotRunner returns a runner that answers get_app_state with a canned
// snapshot, so the modern capture path is exercised without Windows.
func fakeSnapshotRunner(snapshot *appSnapshot) func(psRequest) (*psResponse, error) {
	return func(req psRequest) (*psResponse, error) {
		return &psResponse{OK: true, Snapshot: snapshot}, nil
	}
}

func cannedSnapshot() *appSnapshot {
	return &appSnapshot{
		App:                 appDescriptor{Name: "Notepad", BundleIdentifier: "Microsoft.WindowsNotepad", PID: 4321},
		WindowTitle:         "Untitled",
		WindowBounds:        &frame{X: 10, Y: 20, Width: 1200, Height: 800},
		ScreenshotPNGBase64: fakePNGBase64(2400, 1600),
		TreeLines:           []string{"[0] window Untitled"},
		Elements:            []elementRecord{{Index: 0, Name: "Untitled"}},
	}
}

// TestModernGetAppStateMintsHandle drives get_app_state in the modern era through
// a fake runner and checks the minted handle, the structured block, and that the
// handle resolves against the service store.
func TestModernGetAppStateMintsHandle(t *testing.T) {
	svc := newService()
	svc.runner = fakeSnapshotRunner(cannedSnapshot())

	result := svc.callTool("get_app_state", map[string]any{"app": "Notepad"}, true)
	if result.IsError {
		t.Fatalf("modern get_app_state errored: %+v", result)
	}

	ref, _ := result.StructuredContent["snapshot_ref"].(string)
	if !gomcp.ValidHandleFormat(ref) {
		t.Fatalf("structuredContent snapshot_ref not a valid handle: %q", ref)
	}
	if result.Content[0].Type != "text" || result.Content[0].Text[:13] != "snapshot_ref:" {
		t.Fatalf("text block does not lead with snapshot_ref: %q", result.Content[0].Text)
	}
	if result.StructuredContent["generation"] != 1 {
		t.Fatalf("generation = %v, want 1", result.StructuredContent["generation"])
	}
	app, _ := result.StructuredContent["app"].(map[string]any)
	if app["name"] != "Notepad" || app["bundle_identifier"] != "Microsoft.WindowsNotepad" || app["pid"] != 4321 {
		t.Fatalf("app block = %+v", app)
	}
	window, _ := result.StructuredContent["window"].(map[string]any)
	px, _ := window["screenshot_pixels"].(map[string]any)
	if px["width"] != 2400 || px["height"] != 1600 {
		t.Fatalf("screenshot_pixels = %+v (want 2400x1600 from PNG header)", px)
	}

	// The minted handle resolves through the shared store to the stored snapshot.
	rec, err := svc.store.Resolve(ref)
	if err != nil {
		t.Fatalf("resolve minted handle: %v", err)
	}
	if rec.Payload == nil || rec.Payload.App.PID != 4321 {
		t.Fatalf("resolved payload = %+v", rec.Payload)
	}
}

// TestModernGetAppStateSupersedesOnReCapture verifies a second modern capture of
// the same target supersedes the first handle and advances the generation.
func TestModernGetAppStateSupersedesOnReCapture(t *testing.T) {
	svc := newService()
	svc.runner = fakeSnapshotRunner(cannedSnapshot())

	first := svc.callTool("get_app_state", map[string]any{"app": "Notepad"}, true)
	second := svc.callTool("get_app_state", map[string]any{"app": "Notepad"}, true)

	firstRef := first.StructuredContent["snapshot_ref"].(string)
	if second.StructuredContent["generation"] != 2 {
		t.Fatalf("second generation = %v, want 2", second.StructuredContent["generation"])
	}
	_, err := svc.store.Resolve(firstRef)
	var re *gomcp.ResolveError
	if err == nil {
		t.Fatal("first handle should be superseded after re-capture")
	}
	if !errors.As(err, &re) || re.Code() != gomcp.ErrSnapshotRefStale {
		t.Fatalf("first handle error = %v, want stale", err)
	}
}

// TestModernGetAppStateMintFailureDegradesToLegacy injects a failing token
// source so Mint fails; the modern result must degrade to a legacy-shaped
// capture with no snapshot_ref and no structuredContent, never an empty handle.
func TestModernGetAppStateMintFailureDegradesToLegacy(t *testing.T) {
	svc := newService()
	svc.runner = fakeSnapshotRunner(cannedSnapshot())
	svc.store = gomcp.NewSnapshotStore[*appSnapshot](nil,
		func() ([]byte, error) { return nil, errors.New("token source unavailable") }, nil)

	result := svc.callTool("get_app_state", map[string]any{"app": "Notepad"}, true)
	if result.IsError {
		t.Fatalf("mint failure should still deliver the capture: %+v", result)
	}
	if result.StructuredContent != nil {
		t.Fatalf("degraded result must carry no structuredContent: %+v", result.StructuredContent)
	}
	if len(result.Content) == 0 || result.Content[0].Type != "text" {
		t.Fatalf("degraded result missing text block: %+v", result.Content)
	}
	if len(result.Content[0].Text) >= 12 && result.Content[0].Text[:12] == "snapshot_ref" {
		t.Fatalf("degraded result leaked a snapshot_ref line: %q", result.Content[0].Text)
	}
}

// TestSnapshotDebugLineGating verifies the store's stderr sink is silent unless
// OPEN_COMPUTER_USE_DEBUG_INPUT_FALLBACKS is present, matching the Swift sink.
func TestSnapshotDebugLineGating(t *testing.T) {
	if original, had := os.LookupEnv("OPEN_COMPUTER_USE_DEBUG_INPUT_FALLBACKS"); had {
		os.Unsetenv("OPEN_COMPUTER_USE_DEBUG_INPUT_FALLBACKS")
		t.Cleanup(func() { os.Setenv("OPEN_COMPUTER_USE_DEBUG_INPUT_FALLBACKS", original) })
	}

	var buf bytes.Buffer
	writeSnapshotDebugLine(&buf, "snapshot mint handle=ocu_snapshot_v1_...abc123 minted=1")
	if buf.Len() != 0 {
		t.Fatalf("logged without the debug env var: %q", buf.String())
	}

	t.Setenv("OPEN_COMPUTER_USE_DEBUG_INPUT_FALLBACKS", "1")
	writeSnapshotDebugLine(&buf, "line-with-env")
	if buf.Len() == 0 {
		t.Fatal("did not log with the debug env var set")
	}
}

// TestPngPixelSizeRejectsBogusBlob verifies the PNG signature and IHDR length
// guards: a blob with a coincidental "IHDR" at offset 12 but a wrong signature,
// or a wrong IHDR length, degrades to nil, while a real header yields the dims.
func TestPngPixelSizeRejectsBogusBlob(t *testing.T) {
	if got := pngPixelSize(fakePNGBase64(800, 600)); got == nil || got.Width != 800 || got.Height != 600 {
		t.Fatalf("valid PNG header = %+v, want 800x600", got)
	}

	// Coincidental "IHDR" at offset 12 but no PNG signature.
	bogus := make([]byte, 24)
	copy(bogus[12:16], []byte("IHDR"))
	binary.BigEndian.PutUint32(bogus[8:12], 13)
	binary.BigEndian.PutUint32(bogus[16:20], 1234)
	binary.BigEndian.PutUint32(bogus[20:24], 5678)
	if got := pngPixelSize(base64.StdEncoding.EncodeToString(bogus)); got != nil {
		t.Fatalf("bogus blob with fake IHDR = %+v, want nil", got)
	}

	// Valid signature but wrong IHDR chunk length.
	badLen := make([]byte, 24)
	copy(badLen[0:8], []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a})
	binary.BigEndian.PutUint32(badLen[8:12], 99)
	copy(badLen[12:16], []byte("IHDR"))
	if got := pngPixelSize(base64.StdEncoding.EncodeToString(badLen)); got != nil {
		t.Fatalf("wrong IHDR length = %+v, want nil", got)
	}

	if got := pngPixelSize(""); got != nil {
		t.Fatalf("empty screenshot = %+v, want nil", got)
	}
}

// TestLegacyGetAppStateNoStructuredContent confirms the legacy era result is
// unchanged: no handle minted, no structuredContent.
func TestLegacyGetAppStateNoStructuredContent(t *testing.T) {
	svc := newService()
	svc.runner = fakeSnapshotRunner(cannedSnapshot())

	result := svc.callTool("get_app_state", map[string]any{"app": "Notepad"}, false)
	if result.IsError {
		t.Fatalf("legacy get_app_state errored: %+v", result)
	}
	if result.StructuredContent != nil {
		t.Fatalf("legacy result carries structuredContent: %+v", result.StructuredContent)
	}
	if result.Content[0].Text[:13] == "snapshot_ref:" {
		t.Fatal("legacy text block leaked a snapshot_ref line")
	}
	if svc.store.Counters().Minted != 0 {
		t.Fatalf("legacy path minted %d handles, want 0", svc.store.Counters().Minted)
	}
}
