package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/iFurySt/open-codex-computer-use/packages/gomcp"
)

var version = "0.3.1"

var clickMethodValues = []string{"auto", "accessibility", "app_post", "sky_click", "global"}

//go:embed runtime.py
var linuxRuntimeScript string

const serverInstructions = "Computer Use tools let you interact with Linux desktop apps by performing UI actions.\n\nBegin by calling `get_app_state` every turn you want to use Computer Use to get the latest state before acting. The available tools are list_apps, get_app_state, click, perform_secondary_action, scroll, drag, type_text, press_key, and set_value.\n\nPrefer element-targeted interactions over coordinate clicks when an index for the targeted element is available. Linux actions use AT-SPI2 semantic actions and editable text APIs first. Coordinate mouse and key synthesis are best-effort fallbacks and are not a universal Wayland background input model."

// modernServerInstructions describes the explicit snapshot state chain used by
// the modern 2026-07-28 era. Each get_app_state and each action result returns a
// snapshot_ref that the next action must pass, so element and coordinate
// targeting binds to the exact observed state instead of implicit process state.
const modernServerInstructions = "Computer Use tools let you interact with Linux desktop apps by performing UI actions.\n\nBegin by calling `get_app_state` every turn you want to use Computer Use to get the latest state before acting. The available tools are list_apps, get_app_state, click, perform_secondary_action, scroll, drag, type_text, press_key, and set_value.\n\nEach `get_app_state` result returns a `snapshot_ref` that identifies the captured window state. Pass that `snapshot_ref` to every action call (click, perform_secondary_action, scroll, drag, type_text, press_key, set_value) so the action binds to the state you observed. Each successful action returns a fresh `snapshot_ref`; always pass the most recent one, and call `get_app_state` again to recapture whenever a `snapshot_ref` is reported missing, expired, unknown, or stale.\n\nPrefer element-targeted interactions over coordinate clicks when an index for the targeted element is available. Linux actions use AT-SPI2 semantic actions and editable text APIs first. Coordinate mouse and key synthesis are best-effort fallbacks and are not a universal Wayland background input model."

// Modern-era descriptions for the two capture tools. They mention the snapshot_ref
// the action tools require; every other tool description is byte-identical to the
// legacy catalog.
const modernGetAppStateDescription = "Get the state of an already running app's key window and return a screenshot and accessibility tree. This must be called once per assistant turn before interacting with the app. The result includes a `snapshot_ref` that identifies the captured window state; pass it to the action tools so they act on this exact state. This tool is part of plugin `Computer Use`."

const modernListAppsDescription = "List the apps on this computer. Returns the set of apps that are currently running, as well as any that have been used in the last 14 days, including details on usage frequency. Call `get_app_state` next to capture a window and obtain the `snapshot_ref` the action tools require. This tool is part of plugin `Computer Use`."

type toolDefinition struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Annotations map[string]any `json:"annotations,omitempty"`
	InputSchema map[string]any `json:"inputSchema"`
}

type contentItem struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
}

type toolCallResult struct {
	Content           []contentItem  `json:"content"`
	IsError           bool           `json:"isError"`
	StructuredContent map[string]any `json:"structuredContent,omitempty"`
}

func textResult(text string, isError bool) toolCallResult {
	return toolCallResult{Content: []contentItem{{Type: "text", Text: text}}, IsError: isError}
}

type appDescriptor struct {
	Name             string `json:"name"`
	BundleIdentifier string `json:"bundleIdentifier,omitempty"`
	PID              int    `json:"pid"`
}

type frame struct {
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

func (f frame) renderedLocalFrame() string {
	return fmt.Sprintf("{{x: %.0f, y: %.0f, width: %.0f, height: %.0f}}", f.X, f.Y, f.Width, f.Height)
}

type elementRecord struct {
	Index                int      `json:"index"`
	RuntimeID            []int    `json:"runtimeId,omitempty"`
	AutomationID         string   `json:"automationId,omitempty"`
	Name                 string   `json:"name,omitempty"`
	ControlType          string   `json:"controlType,omitempty"`
	LocalizedControlType string   `json:"localizedControlType,omitempty"`
	ClassName            string   `json:"className,omitempty"`
	Value                string   `json:"value,omitempty"`
	NativeWindowHandle   int64    `json:"nativeWindowHandle,omitempty"`
	Frame                *frame   `json:"frame,omitempty"`
	Actions              []string `json:"actions,omitempty"`
}

type appSnapshot struct {
	App                 appDescriptor   `json:"app"`
	WindowTitle         string          `json:"windowTitle,omitempty"`
	WindowBounds        *frame          `json:"windowBounds,omitempty"`
	ScreenshotPNGBase64 string          `json:"screenshotPngBase64,omitempty"`
	TreeLines           []string        `json:"treeLines,omitempty"`
	FocusedSummary      string          `json:"focusedSummary,omitempty"`
	SelectedText        string          `json:"selectedText,omitempty"`
	Elements            []elementRecord `json:"elements,omitempty"`
}

func (s *appSnapshot) renderedText() string {
	if s == nil {
		return ""
	}
	appRef := s.App.BundleIdentifier
	if appRef == "" {
		appRef = s.App.Name
	}
	title := s.WindowTitle
	if strings.TrimSpace(title) == "" {
		title = s.App.Name
	}

	lines := []string{
		fmt.Sprintf("App=%s (pid %d)", appRef, s.App.PID),
		fmt.Sprintf("Window: %q, App: %s.", title, s.App.Name),
	}
	lines = append(lines, s.TreeLines...)
	if strings.TrimSpace(s.SelectedText) != "" {
		lines = append(lines, "", fmt.Sprintf("Selected text: [%s]", s.SelectedText))
	} else if strings.TrimSpace(s.FocusedSummary) != "" {
		lines = append(lines, "", fmt.Sprintf("The focused UI element is %s.", s.FocusedSummary))
	}
	return strings.Join(lines, "\n")
}

func (s *appSnapshot) result() toolCallResult {
	result := toolCallResult{
		Content: []contentItem{{Type: "text", Text: s.renderedText()}},
	}
	if s != nil && s.ScreenshotPNGBase64 != "" {
		result.Content = append(result.Content, contentItem{
			Type:     "image",
			Data:     s.ScreenshotPNGBase64,
			MimeType: "image/png",
		})
	}
	return result
}

type linuxRequest struct {
	Tool         string         `json:"tool"`
	App          string         `json:"app,omitempty"`
	Element      *elementRecord `json:"element,omitempty"`
	X            *float64       `json:"x,omitempty"`
	Y            *float64       `json:"y,omitempty"`
	FromX        *float64       `json:"from_x,omitempty"`
	FromY        *float64       `json:"from_y,omitempty"`
	ToX          *float64       `json:"to_x,omitempty"`
	ToY          *float64       `json:"to_y,omitempty"`
	ClickCount   int            `json:"click_count,omitempty"`
	MouseButton  string         `json:"mouse_button,omitempty"`
	ClickMethod  string         `json:"click_method,omitempty"`
	Action       string         `json:"action,omitempty"`
	Direction    string         `json:"direction,omitempty"`
	Pages        float64        `json:"pages,omitempty"`
	Text         string         `json:"text,omitempty"`
	Key          string         `json:"key,omitempty"`
	Value        string         `json:"value,omitempty"`
	WindowBounds *frame         `json:"windowBounds,omitempty"`
	TextLimit    any            `json:"text_limit,omitempty"`
	MaxTreeNodes int            `json:"max_tree_nodes,omitempty"`
	MaxTreeDepth int            `json:"max_tree_depth,omitempty"`
	// ExpectedRole and ExpectedName are the modern-era element revalidation
	// expectations. The runtime resolves the stored runtimeId against a fresh tree
	// and, when these are present, verifies the resolved node still exposes the
	// same role (and stable name) before acting. Legacy requests never set them,
	// so the runtime path stays byte-identical for the legacy era.
	ExpectedRole string `json:"expected_role,omitempty"`
	ExpectedName string `json:"expected_name,omitempty"`
}

type textLimit struct {
	max   bool
	count int
}

func (limit textLimit) runtimeValue() any {
	if limit.max {
		return "max"
	}
	return limit.count
}

type linuxResponse struct {
	OK   bool   `json:"ok"`
	Text string `json:"text,omitempty"`
	// ErrorKind is a structured failure marker. The runtime sets it to
	// gomcp.ErrorKindElementMismatch when the resolved node no longer matches the
	// stored element's role or stable name; the modern action transaction maps
	// that to snapshot_target_changed with no native action performed.
	ErrorKind string       `json:"errorKind,omitempty"`
	Error     string       `json:"error,omitempty"`
	Snapshot  *appSnapshot `json:"snapshot,omitempty"`
}

type service struct {
	snapshots map[string]*appSnapshot
	// store owns the modern-era snapshot handles. It is process-lifetime state;
	// the legacy snapshots map above is untouched and still backs the legacy era.
	store *gomcp.SnapshotStore[*appSnapshot]
	// runner executes a runtime request. Production is runPython; tests inject a
	// canned runner so the modern get_app_state path is exercised without a
	// Linux desktop. The production path stays byte-identical for legacy.
	runner func(linuxRequest) (*linuxResponse, error)
}

// snapshotStoreLogger writes the store's redacted lifecycle lines to stderr,
// gated on OPEN_COMPUTER_USE_DEBUG_INPUT_FALLBACKS exactly like the Swift
// defaultLogSink (any value, including empty, enables it). Stdout stays pure
// JSON-RPC.
func snapshotStoreLogger(line string) {
	writeSnapshotDebugLine(os.Stderr, line)
}

func writeSnapshotDebugLine(w io.Writer, line string) {
	if _, ok := os.LookupEnv("OPEN_COMPUTER_USE_DEBUG_INPUT_FALLBACKS"); !ok {
		return
	}
	fmt.Fprintln(w, line)
}

func newService() *service {
	return &service{
		snapshots: map[string]*appSnapshot{},
		store:     gomcp.NewSnapshotStore[*appSnapshot](nil, nil, snapshotStoreLogger),
		runner:    runPython,
	}
}

func (s *service) callTool(name string, args map[string]any, modern bool) toolCallResult {
	// Modern-era action tools run the M4 transaction: they consume an explicit
	// snapshot_ref and dispatch from the stored snapshot. The legacy switch below
	// keeps the implicit currentSnapshot behavior for the legacy era and the CLI
	// batch path (both pass modern=false).
	if modern && gomcp.IsModernActionTool(name) {
		return s.callModernAction(name, args)
	}
	switch name {
	case "list_apps":
		return s.listApps()
	case "get_app_state":
		maxTreeNodes, err := optionalPositiveInt(args, "max_tree_nodes")
		if err != nil {
			return textResult(err.Error(), true)
		}
		maxTreeDepth, err := optionalPositiveInt(args, "max_tree_depth")
		if err != nil {
			return textResult(err.Error(), true)
		}
		textLimit, err := optionalTextLimit(args, "text_limit")
		if err != nil {
			return textResult(err.Error(), true)
		}
		return s.getAppState(requiredString(args, "app"), textLimit, maxTreeNodes, maxTreeDepth, modern)
	case "click":
		clickMethod, err := parseClickMethod(optionalString(args, "click_method"))
		if err != nil {
			return textResult(err.Error(), true)
		}
		return s.click(
			requiredString(args, "app"),
			optionalElementIndex(args),
			optionalFloat(args, "x"),
			optionalFloat(args, "y"),
			intValue(optionalFloat(args, "click_count"), 1),
			defaultString(optionalString(args, "mouse_button"), "left"),
			clickMethod,
		)
	case "perform_secondary_action":
		return s.performSecondaryAction(
			requiredString(args, "app"),
			requiredElementIndex(args),
			requiredString(args, "action"),
		)
	case "scroll":
		return s.scroll(
			requiredString(args, "app"),
			requiredString(args, "direction"),
			requiredElementIndex(args),
			floatValue(optionalFloat(args, "pages"), 1),
		)
	case "drag":
		return s.drag(
			requiredString(args, "app"),
			requiredFloat(args, "from_x"),
			requiredFloat(args, "from_y"),
			requiredFloat(args, "to_x"),
			requiredFloat(args, "to_y"),
		)
	case "type_text":
		return s.typeText(requiredString(args, "app"), requiredString(args, "text"))
	case "press_key":
		return s.pressKey(requiredString(args, "app"), requiredString(args, "key"))
	case "set_value":
		return s.setValue(requiredString(args, "app"), requiredElementIndex(args), requiredString(args, "value"))
	default:
		return textResult(fmt.Sprintf("unsupportedTool(%q)", name), true)
	}
}

func (s *service) listApps() toolCallResult {
	response, err := s.runner(linuxRequest{Tool: "list_apps"})
	if err != nil {
		return textResult(err.Error(), true)
	}
	if !response.OK {
		return textResult(response.Error, true)
	}
	if strings.TrimSpace(response.Text) == "" {
		response.Text = "No running top-level apps are visible to this Linux runtime."
	}
	return textResult(response.Text, false)
}

func (s *service) getAppState(app string, textLimit *textLimit, maxTreeNodes, maxTreeDepth *int, modern bool) toolCallResult {
	if app == "" {
		return textResult("Missing required argument: app", true)
	}
	request := linuxRequest{Tool: "get_app_state", App: app}
	if textLimit != nil {
		request.TextLimit = textLimit.runtimeValue()
	}
	if maxTreeNodes != nil {
		request.MaxTreeNodes = *maxTreeNodes
	}
	if maxTreeDepth != nil {
		request.MaxTreeDepth = *maxTreeDepth
	}
	snapshot, result := s.refreshSnapshot(app, request)
	if result.IsError {
		return result
	}
	if modern {
		state, err := s.mintSnapshotState(snapshot)
		if err != nil {
			// Minting failed (only when the token source fails). Deliver the
			// capture as a legacy-shaped result with no snapshot_ref rather than
			// emitting an empty handle, and record the store error on the gated
			// debug sink.
			snapshotStoreLogger("snapshot mint failed: " + err.Error())
			return snapshot.result()
		}
		return snapshot.modernStateResult(state)
	}
	return snapshot.result()
}

// mintSnapshotState mints a real handle for the captured snapshot and returns the
// structured state block for the modern get_app_state result. The store owns the
// snapshot payload (screenshot bytes and element records) until it expires or is
// superseded; the metadata kept here carries no accessibility text.
func (s *service) mintSnapshotState(snapshot *appSnapshot) (gomcp.StructuredState, error) {
	target := gomcp.TargetKey{App: snapshotTargetIdentity(snapshot)}
	record, err := s.store.Mint(target, snapshot, snapshotMeta(snapshot))
	if err != nil {
		return gomcp.StructuredState{}, err
	}
	return snapshotState(record.Handle, record.CreatedAt, record.ExpiresAt, record.Generation, record.Meta), nil
}

// snapshotMeta builds the normalized store metadata for a captured snapshot. The
// screenshot bytes and element records stay in the payload; the metadata carries
// no accessibility text.
func snapshotMeta(snapshot *appSnapshot) gomcp.SnapshotMeta {
	return gomcp.SnapshotMeta{
		AppName:          snapshot.App.Name,
		BundleIdentifier: snapshot.App.BundleIdentifier,
		PID:              snapshot.App.PID,
		Bounds:           rectFromFrame(snapshot.WindowBounds),
		ScreenshotPixels: pngPixelSize(snapshot.ScreenshotPNGBase64),
		Mode:             "real",
	}
}

// callModernAction runs the M4 transaction for one of the seven modern action
// tools (design "Action transaction" steps 1-7). It parses snapshot_ref before
// any side effect (including app resolution), moves the handle to in_flight under
// the store lock, verifies the requested app resolves to the stored target,
// builds the runtime request from the STORED snapshot (never the legacy snapshots
// map), dispatches exactly one runtime invocation, and mints a successor handle.
// Every failure class transitions the handle per the pinned cross-platform
// contract and its retry semantics.
func (s *service) callModernAction(name string, args map[string]any) toolCallResult {
	// Step 1: parse and validate snapshot_ref before touching args or the app.
	ref, present := snapshotRefArg(args)
	if !present {
		return snapshotErrorResult(gomcp.ErrSnapshotRefMissing, gomcp.MsgSnapshotRefMissing)
	}
	if !gomcp.ValidHandleFormat(ref) {
		return snapshotErrorResult(gomcp.ErrSnapshotRefMalformed, gomcp.MsgSnapshotRefMalformed)
	}

	// Steps 2 and 4: resolve and CAS live -> in_flight under the store's
	// per-target lock. Unknown/expired/stale/in_use return here with no dispatch.
	rec, err := s.store.BeginAction(ref)
	if err != nil {
		return resolveErrorResult(err)
	}

	// Step 3: verify the requested app resolves to the stored target identity. A
	// name alias is fine; an identity mismatch changes the target.
	app := requiredString(args, "app")
	if app == "" {
		_ = s.store.AbortRestore(ref, gomcp.AbortValidation)
		return textResult("Missing required argument: app", true)
	}
	if !appMatchesRecord(app, rec.Meta) {
		_ = s.store.AbortRestore(ref, gomcp.AbortMismatched)
		return snapshotErrorResult(gomcp.ErrSnapshotTargetChanged, gomcp.MsgSnapshotTargetChangedApp)
	}

	// Steps 5 setup: build the runtime request from the STORED snapshot and run
	// the pre-dispatch validations (coordinates inside the captured screenshot,
	// element index resolvable). A validation failure restores the handle to live.
	request, verr := buildModernRequest(name, app, args, rec.Payload)
	if verr != nil {
		_ = s.store.AbortRestore(ref, gomcp.AbortValidation)
		return textResult(verr.Error(), true)
	}

	// Step 6: dispatch EXACTLY ONE runtime invocation from the stored snapshot.
	response, rerr := s.runner(request)
	if rerr != nil {
		// The dispatch began but its outcome is unknown.
		_ = s.store.AbortSupersede(ref, gomcp.AbortUncertain)
		return snapshotErrorResult(gomcp.ErrSnapshotActionOutcomeUncertain, gomcp.MsgSnapshotActionOutcomeUncertain)
	}
	if !response.OK {
		switch response.ErrorKind {
		case gomcp.ErrorKindElementMismatch:
			// The runtime revalidated the resolved node against the stored record
			// and refused before performing any native action.
			_ = s.store.AbortRestore(ref, gomcp.AbortMismatched)
			return snapshotErrorResult(gomcp.ErrSnapshotTargetChanged, gomcp.MsgSnapshotTargetChangedElement)
		case gomcp.ErrorKindRejectedBeforeInput:
			// The runtime rejected the request from a site that provably precedes
			// any native input; the handle stays live for a same-handle retry.
			_ = s.store.AbortRestore(ref, gomcp.AbortValidation)
			return textResult(response.Error, true)
		default:
			// The runtime attempted the action and reported failure; treat the
			// outcome as uncertain and require a recapture.
			_ = s.store.AbortSupersede(ref, gomcp.AbortUncertain)
			return snapshotErrorResult(gomcp.ErrSnapshotActionOutcomeUncertain, gomcp.MsgSnapshotActionOutcomeUncertain)
		}
	}
	if response.Snapshot == nil {
		// Adjudicated refresh-failure: the action dispatched but the post-action
		// state could not be recaptured. Supersede and instruct the caller to
		// recapture; the action likely succeeded.
		_ = s.store.AbortSupersede(ref, gomcp.AbortRefreshFailed)
		return snapshotErrorResult(gomcp.ErrSnapshotActionOutcomeUncertain, gomcp.MsgSnapshotActionRefreshFailed)
	}

	// Step 7: mint the successor (generation n+1), supersede the old handle, and
	// return the successor snapshot_ref in text and structuredContent.
	succ, ferr := s.store.FinishSuccess(ref, response.Snapshot, snapshotMeta(response.Snapshot))
	if ferr != nil {
		_ = s.store.AbortSupersede(ref, gomcp.AbortRefreshFailed)
		return snapshotErrorResult(gomcp.ErrSnapshotActionOutcomeUncertain, gomcp.MsgSnapshotActionRefreshFailed)
	}
	state := snapshotState(succ.Handle, succ.CreatedAt, succ.ExpiresAt, succ.Generation, succ.Meta)
	return response.Snapshot.modernStateResult(state)
}

// snapshotRefArg extracts a present, non-empty snapshot_ref string argument.
func snapshotRefArg(args map[string]any) (string, bool) {
	value, ok := args[gomcp.SnapshotRefKey]
	if !ok {
		return "", false
	}
	str, _ := value.(string)
	str = strings.TrimSpace(str)
	if str == "" {
		return "", false
	}
	return str, true
}

// appMatchesRecord reports whether the requested app string refers to the same
// app the handle was captured against. The stored identity is the app name, its
// bundle/executable identity, and its PID (the same alias set the legacy cache
// keys on); a case-insensitive match against any of them is an accepted alias.
//
// Accepted platform limitation: this compares the request string against stored
// aliases, so it cannot detect a same-name relaunch (a new PID reusing the old
// name/bundle) before dispatch, since Go resolves identity only at the runtime
// boundary. For element actions the runtime-side runtimeId + role/name
// expectation check (see withExpectations / expectation_mismatch) is the
// mitigating guard: a relaunched process yields a different tree and fails the
// element revalidation as snapshot_target_changed with no native action.
func appMatchesRecord(app string, meta gomcp.SnapshotMeta) bool {
	want := strings.ToLower(strings.TrimSpace(app))
	if want == "" {
		return false
	}
	for _, candidate := range []string{meta.AppName, meta.BundleIdentifier, strconv.Itoa(meta.PID)} {
		if strings.ToLower(strings.TrimSpace(candidate)) == want {
			return true
		}
	}
	return false
}

// snapshotErrorResult builds a modern isError tool result carrying the snapshot
// tool-error envelope in structuredContent.error alongside the visible message.
func snapshotErrorResult(code, message string) toolCallResult {
	return toolCallResult{
		Content: []contentItem{{Type: "text", Text: message}},
		IsError: true,
		StructuredContent: map[string]any{
			"error": map[string]any{
				"code":    code,
				"message": message,
				"retry":   gomcp.CanonicalRetry(code),
			},
		},
	}
}

// resolveErrorResult maps a store resolution/transaction error to a modern tool
// error result with the pinned code, message, and retry class.
func resolveErrorResult(err error) toolCallResult {
	if code, message, ok := gomcp.ResolveErrorCodeMessage(err); ok {
		return snapshotErrorResult(code, message)
	}
	return textResult(err.Error(), true)
}

// buildModernRequest assembles the runtime request for a modern action from the
// STORED snapshot and the call arguments, running the same input validation the
// legacy methods run plus the modern coordinate-in-screenshot checks. It never
// reads the legacy snapshots map.
func buildModernRequest(name, app string, args map[string]any, stored *appSnapshot) (linuxRequest, error) {
	switch name {
	case "click":
		return buildModernClick(app, args, stored)
	case "perform_secondary_action":
		elementIndex := requiredElementIndex(args)
		action := requiredString(args, "action")
		if elementIndex == "" {
			return linuxRequest{}, errors.New("Missing required argument: element_index")
		}
		if action == "" {
			return linuxRequest{}, errors.New("Missing required argument: action")
		}
		record, err := lookupElement(stored, elementIndex)
		if err != nil {
			return linuxRequest{}, err
		}
		req := linuxRequest{Tool: "perform_secondary_action", App: app, Action: action}
		withExpectations(&req, record)
		return req, nil
	case "scroll":
		elementIndex := requiredElementIndex(args)
		if elementIndex == "" {
			return linuxRequest{}, errors.New("Missing required argument: element_index")
		}
		normalized := strings.ToLower(requiredString(args, "direction"))
		if normalized != "up" && normalized != "down" && normalized != "left" && normalized != "right" {
			return linuxRequest{}, errors.New("Invalid scroll direction: " + requiredString(args, "direction"))
		}
		pages := floatValue(optionalFloat(args, "pages"), 1)
		if pages <= 0 {
			return linuxRequest{}, errors.New("pages must be > 0")
		}
		record, err := lookupElement(stored, elementIndex)
		if err != nil {
			return linuxRequest{}, err
		}
		req := linuxRequest{Tool: "scroll", App: app, Direction: normalized, Pages: pages}
		withExpectations(&req, record)
		return req, nil
	case "drag":
		fromX := requiredFloat(args, "from_x")
		fromY := requiredFloat(args, "from_y")
		toX := requiredFloat(args, "to_x")
		toY := requiredFloat(args, "to_y")
		for key, value := range map[string]*float64{"from_x": fromX, "from_y": fromY, "to_x": toX, "to_y": toY} {
			if value == nil {
				return linuxRequest{}, errors.New("Missing required argument: " + key)
			}
		}
		if err := validateStoredPoint(stored, fromX, fromY); err != nil {
			return linuxRequest{}, err
		}
		if err := validateStoredPoint(stored, toX, toY); err != nil {
			return linuxRequest{}, err
		}
		return linuxRequest{Tool: "drag", App: app, FromX: fromX, FromY: fromY, ToX: toX, ToY: toY, WindowBounds: stored.WindowBounds}, nil
	case "type_text":
		text := requiredString(args, "text")
		if text == "" {
			return linuxRequest{}, errors.New("Missing required argument: text")
		}
		return linuxRequest{Tool: "type_text", App: app, Text: text}, nil
	case "press_key":
		key := requiredString(args, "key")
		if key == "" {
			return linuxRequest{}, errors.New("Missing required argument: key")
		}
		return linuxRequest{Tool: "press_key", App: app, Key: key}, nil
	case "set_value":
		elementIndex := requiredElementIndex(args)
		if elementIndex == "" {
			return linuxRequest{}, errors.New("Missing required argument: element_index")
		}
		record, err := lookupElement(stored, elementIndex)
		if err != nil {
			return linuxRequest{}, err
		}
		req := linuxRequest{Tool: "set_value", App: app, Value: requiredString(args, "value")}
		withExpectations(&req, record)
		return req, nil
	default:
		return linuxRequest{}, fmt.Errorf("unsupportedTool(%q)", name)
	}
}

// buildModernClick builds a modern click request, applying the same click_method
// gating the legacy Linux click applies and validating coordinate clicks against
// the stored screenshot dimensions.
func buildModernClick(app string, args map[string]any, stored *appSnapshot) (linuxRequest, error) {
	clickMethod, err := parseClickMethod(optionalString(args, "click_method"))
	if err != nil {
		return linuxRequest{}, err
	}
	elementIndex := optionalElementIndex(args)
	x := optionalFloat(args, "x")
	y := optionalFloat(args, "y")
	if elementIndex == "" && (x == nil || y == nil) {
		return linuxRequest{}, errors.New("click requires either element_index or x/y")
	}
	if clickMethod == "accessibility" && elementIndex == "" {
		return linuxRequest{}, errors.New("click_method 'accessibility' requires element_index")
	}
	if clickMethod == "app_post" {
		return linuxRequest{}, errors.New("click_method 'app_post' is not supported on Linux")
	}
	if clickMethod == "sky_click" {
		return linuxRequest{}, errors.New("click_method 'sky_click' is not supported on Linux")
	}
	if clickMethod == "global" && !globalPointerFallbacksEnabled() {
		return linuxRequest{}, errors.New("click_method 'global' requires OPEN_COMPUTER_USE_ALLOW_GLOBAL_POINTER_FALLBACKS=1 because it may move the system pointer and change foreground focus")
	}
	req := linuxRequest{
		Tool:         "click",
		App:          app,
		X:            x,
		Y:            y,
		ClickCount:   intValue(optionalFloat(args, "click_count"), 1),
		MouseButton:  defaultString(optionalString(args, "mouse_button"), "left"),
		ClickMethod:  clickMethod,
		WindowBounds: stored.WindowBounds,
	}
	if elementIndex != "" {
		record, err := lookupElement(stored, elementIndex)
		if err != nil {
			return linuxRequest{}, err
		}
		withExpectations(&req, record)
	} else if err := validateStoredPoint(stored, x, y); err != nil {
		return linuxRequest{}, err
	}
	return req, nil
}

// withExpectations binds the request to a stored element and carries the modern
// element-revalidation expectations. It sends expected_role whenever the stored
// element has one and expected_name only when the stored name is present and not
// truncated (a truncated name is not a stable equality target).
func withExpectations(req *linuxRequest, record *elementRecord) {
	req.Element = record
	if record.ControlType != "" {
		req.ExpectedRole = record.ControlType
	}
	if record.Name != "" && !strings.HasSuffix(record.Name, "...") {
		req.ExpectedName = record.Name
	}
}

// validateStoredPoint rejects a coordinate action whose point falls outside the
// captured screenshot's pixel dimensions. Bounds are half-open (valid iff
// 0 <= x < width and 0 <= y < height), matching the Swift check. When the stored
// snapshot has no derivable pixel size, the point cannot be validated and is
// allowed through.
func validateStoredPoint(stored *appSnapshot, x, y *float64) error {
	pixels := pngPixelSize(stored.ScreenshotPNGBase64)
	if pixels == nil || x == nil || y == nil {
		return nil
	}
	if *x < 0 || *y < 0 || *x >= float64(pixels.Width) || *y >= float64(pixels.Height) {
		return errors.New(gomcp.MsgCoordinatesOutOfBounds)
	}
	return nil
}

func (s *service) click(app, elementIndex string, x, y *float64, clickCount int, mouseButton, clickMethod string) toolCallResult {
	if app == "" {
		return textResult("Missing required argument: app", true)
	}
	if elementIndex == "" && (x == nil || y == nil) {
		return textResult("click requires either element_index or x/y", true)
	}
	if clickMethod == "accessibility" && elementIndex == "" {
		return textResult("click_method 'accessibility' requires element_index", true)
	}
	if clickMethod == "app_post" {
		return textResult("click_method 'app_post' is not supported on Linux", true)
	}
	if clickMethod == "sky_click" {
		return textResult("click_method 'sky_click' is not supported on Linux", true)
	}
	if clickMethod == "global" && !globalPointerFallbacksEnabled() {
		return textResult("click_method 'global' requires OPEN_COMPUTER_USE_ALLOW_GLOBAL_POINTER_FALLBACKS=1 because it may move the system pointer and change foreground focus", true)
	}
	snapshot := s.currentSnapshot(app)
	if snapshot == nil {
		return textResult("No app state is available for "+app+". Run get_app_state before action tools.", true)
	}
	request := linuxRequest{
		Tool:         "click",
		App:          app,
		X:            x,
		Y:            y,
		ClickCount:   clickCount,
		MouseButton:  mouseButton,
		ClickMethod:  clickMethod,
		WindowBounds: snapshot.WindowBounds,
	}
	if elementIndex != "" {
		record, err := lookupElement(snapshot, elementIndex)
		if err != nil {
			return textResult(err.Error(), true)
		}
		request.Element = record
	}
	return s.actionResult(app, request)
}

func (s *service) performSecondaryAction(app, elementIndex, action string) toolCallResult {
	if app == "" {
		return textResult("Missing required argument: app", true)
	}
	if elementIndex == "" {
		return textResult("Missing required argument: element_index", true)
	}
	if action == "" {
		return textResult("Missing required argument: action", true)
	}
	snapshot := s.currentSnapshot(app)
	if snapshot == nil {
		return textResult("No app state is available for "+app+". Run get_app_state before action tools.", true)
	}
	record, err := lookupElement(snapshot, elementIndex)
	if err != nil {
		return textResult(err.Error(), true)
	}
	return s.actionResult(app, linuxRequest{Tool: "perform_secondary_action", App: app, Element: record, Action: action})
}

func (s *service) scroll(app, direction, elementIndex string, pages float64) toolCallResult {
	if app == "" {
		return textResult("Missing required argument: app", true)
	}
	if elementIndex == "" {
		return textResult("Missing required argument: element_index", true)
	}
	normalized := strings.ToLower(direction)
	if normalized != "up" && normalized != "down" && normalized != "left" && normalized != "right" {
		return textResult("Invalid scroll direction: "+direction, true)
	}
	if pages <= 0 {
		return textResult("pages must be > 0", true)
	}
	snapshot := s.currentSnapshot(app)
	if snapshot == nil {
		return textResult("No app state is available for "+app+". Run get_app_state before action tools.", true)
	}
	record, err := lookupElement(snapshot, elementIndex)
	if err != nil {
		return textResult(err.Error(), true)
	}
	return s.actionResult(app, linuxRequest{Tool: "scroll", App: app, Element: record, Direction: normalized, Pages: pages})
}

func (s *service) drag(app string, fromX, fromY, toX, toY *float64) toolCallResult {
	if app == "" {
		return textResult("Missing required argument: app", true)
	}
	if fromX == nil {
		return textResult("Missing required argument: from_x", true)
	}
	if fromY == nil {
		return textResult("Missing required argument: from_y", true)
	}
	if toX == nil {
		return textResult("Missing required argument: to_x", true)
	}
	if toY == nil {
		return textResult("Missing required argument: to_y", true)
	}
	snapshot := s.currentSnapshot(app)
	if snapshot == nil {
		return textResult("No app state is available for "+app+". Run get_app_state before action tools.", true)
	}
	return s.actionResult(app, linuxRequest{Tool: "drag", App: app, FromX: fromX, FromY: fromY, ToX: toX, ToY: toY, WindowBounds: snapshot.WindowBounds})
}

func (s *service) typeText(app, text string) toolCallResult {
	if app == "" {
		return textResult("Missing required argument: app", true)
	}
	if text == "" {
		return textResult("Missing required argument: text", true)
	}
	if s.currentSnapshot(app) == nil {
		return textResult("No app state is available for "+app+". Run get_app_state before action tools.", true)
	}
	return s.actionResult(app, linuxRequest{Tool: "type_text", App: app, Text: text})
}

func (s *service) pressKey(app, key string) toolCallResult {
	if app == "" {
		return textResult("Missing required argument: app", true)
	}
	if key == "" {
		return textResult("Missing required argument: key", true)
	}
	if s.currentSnapshot(app) == nil {
		return textResult("No app state is available for "+app+". Run get_app_state before action tools.", true)
	}
	return s.actionResult(app, linuxRequest{Tool: "press_key", App: app, Key: key})
}

func (s *service) setValue(app, elementIndex, value string) toolCallResult {
	if app == "" {
		return textResult("Missing required argument: app", true)
	}
	if elementIndex == "" {
		return textResult("Missing required argument: element_index", true)
	}
	snapshot := s.currentSnapshot(app)
	if snapshot == nil {
		return textResult("No app state is available for "+app+". Run get_app_state before action tools.", true)
	}
	record, err := lookupElement(snapshot, elementIndex)
	if err != nil {
		return textResult(err.Error(), true)
	}
	return s.actionResult(app, linuxRequest{Tool: "set_value", App: app, Element: record, Value: value})
}

func (s *service) actionResult(app string, request linuxRequest) toolCallResult {
	snapshot, result := s.refreshSnapshot(app, request)
	if result.IsError {
		return result
	}
	return snapshot.result()
}

// currentSnapshot is the LEGACY implicit-cache lookup: it returns whatever
// snapshot was last cached under the app key. The modern era never calls it (its
// actions dispatch from the store-resolved snapshot_ref instead). This implicit
// path is retained only for the 2025-03-26 compatibility window and is planned
// for removal once modern-host adoption is known (design "Phase D: later
// cleanup").
func (s *service) currentSnapshot(app string) *appSnapshot {
	return s.snapshots[strings.ToLower(app)]
}

func (s *service) refreshSnapshot(app string, request linuxRequest) (*appSnapshot, toolCallResult) {
	response, err := s.runner(request)
	if err != nil {
		return nil, textResult(err.Error(), true)
	}
	if !response.OK {
		return nil, textResult(response.Error, true)
	}
	if response.Snapshot == nil {
		return nil, textResult("Linux runtime did not return an app snapshot.", true)
	}
	s.rememberSnapshot(app, response.Snapshot)
	return response.Snapshot, toolCallResult{}
}

func (s *service) rememberSnapshot(query string, snapshot *appSnapshot) {
	keys := []string{query, snapshot.App.Name, snapshot.App.BundleIdentifier, strconv.Itoa(snapshot.App.PID)}
	for _, key := range keys {
		key = strings.ToLower(strings.TrimSpace(key))
		if key != "" {
			s.snapshots[key] = snapshot
		}
	}
}

func lookupElement(snapshot *appSnapshot, elementIndex string) (*elementRecord, error) {
	index, err := strconv.Atoi(elementIndex)
	if err != nil {
		return nil, fmt.Errorf("unknown element_index %q", elementIndex)
	}
	for _, record := range snapshot.Elements {
		if record.Index == index {
			copy := record
			return &copy, nil
		}
	}
	return nil, fmt.Errorf("unknown element_index %q", elementIndex)
}

func runPython(request linuxRequest) (*linuxResponse, error) {
	if runtime.GOOS != "linux" {
		return nil, errors.New("Linux Computer Use runtime requires python3 on Linux")
	}

	tempDir, err := os.MkdirTemp("", "open-computer-use-linux-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tempDir)

	scriptPath := filepath.Join(tempDir, "runtime.py")
	operationPath := filepath.Join(tempDir, "operation.json")
	if err := os.WriteFile(scriptPath, []byte(linuxRuntimeScript), 0o600); err != nil {
		return nil, err
	}
	operationData, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(operationPath, operationData, 0o600); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "python3", scriptPath, operationPath)
	cmd.Env = linuxRuntimeEnvironment(os.Environ())
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if ctx.Err() == context.DeadlineExceeded {
		return nil, errors.New("Linux runtime timed out after 30s")
	}
	if err != nil {
		text := strings.TrimSpace(stderr.String())
		if text == "" {
			text = strings.TrimSpace(string(output))
		}
		if text == "" {
			text = err.Error()
		}
		return nil, fmt.Errorf("Linux runtime failed: %s", text)
	}

	var response linuxResponse
	if err := json.Unmarshal(output, &response); err != nil {
		return nil, fmt.Errorf("Linux runtime returned invalid JSON: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return &response, nil
}

func linuxRuntimeEnvironment(base []string) []string {
	uid := os.Getuid()
	return linuxRuntimeEnvironmentFrom(base, uid, desktopProcessEnvironments(uid))
}

func linuxRuntimeEnvironmentFrom(base []string, uid int, processEnvs []map[string]string) []string {
	env := envSliceToMap(base)
	runtimeDir := chooseRuntimeDir(env, processEnvs, uid)
	if runtimeDir != "" {
		env["XDG_RUNTIME_DIR"] = runtimeDir
	}

	if value := sessionBusAddress(env["DBUS_SESSION_BUS_ADDRESS"], runtimeDir, processEnvs); value != "" {
		env["DBUS_SESSION_BUS_ADDRESS"] = value
	}
	if value := waylandDisplay(env["WAYLAND_DISPLAY"], runtimeDir, processEnvs); value != "" {
		env["WAYLAND_DISPLAY"] = value
	}

	for _, key := range []string{
		"DISPLAY",
		"XAUTHORITY",
		"XDG_CURRENT_DESKTOP",
		"XDG_SESSION_DESKTOP",
		"XDG_SESSION_TYPE",
		"DESKTOP_SESSION",
		"GDK_BACKEND",
		"QT_QPA_PLATFORMTHEME",
		"AT_SPI_BUS_ADDRESS",
	} {
		if strings.TrimSpace(env[key]) == "" {
			if value := firstSessionValue(processEnvs, key); value != "" {
				env[key] = value
			}
		}
	}

	return envMapToSlice(base, env)
}

func chooseRuntimeDir(env map[string]string, processEnvs []map[string]string, uid int) string {
	candidates := []string{env["XDG_RUNTIME_DIR"]}
	for _, processEnv := range processEnvs {
		candidates = append(candidates, processEnv["XDG_RUNTIME_DIR"])
	}
	candidates = append(candidates, fmt.Sprintf("/run/user/%d", uid))

	seen := map[string]bool{}
	for _, candidate := range candidates {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		candidate = filepath.Clean(candidate)
		if seen[candidate] {
			continue
		}
		seen[candidate] = true
		if validRuntimeDir(candidate, uid) {
			return candidate
		}
	}
	return ""
}

func sessionBusAddress(current, runtimeDir string, processEnvs []map[string]string) string {
	current = strings.TrimSpace(current)
	if runtimeDir != "" {
		busPath := filepath.Join(runtimeDir, "bus")
		if isSocket(busPath) && shouldUseRuntimeBus(current, runtimeDir) {
			return "unix:path=" + busPath
		}
	}
	if current != "" {
		return current
	}
	for _, processEnv := range processEnvs {
		value := strings.TrimSpace(processEnv["DBUS_SESSION_BUS_ADDRESS"])
		if value == "" {
			continue
		}
		if runtimeDir != "" {
			busPath := filepath.Join(runtimeDir, "bus")
			if isSocket(busPath) && strings.Contains(value, busPath) {
				return "unix:path=" + busPath
			}
		}
		return value
	}
	if runtimeDir != "" {
		busPath := filepath.Join(runtimeDir, "bus")
		if isSocket(busPath) {
			return "unix:path=" + busPath
		}
	}
	return ""
}

func shouldUseRuntimeBus(current, runtimeDir string) bool {
	current = strings.TrimSpace(current)
	if current == "" {
		return true
	}
	busPath := filepath.Join(runtimeDir, "bus")
	if strings.Contains(current, busPath) {
		return true
	}
	return strings.Contains(current, "/run/user/") && !strings.Contains(current, runtimeDir)
}

func waylandDisplay(current, runtimeDir string, processEnvs []map[string]string) string {
	if value := normalizeWaylandDisplay(current, runtimeDir); value != "" {
		return value
	}
	for _, processEnv := range processEnvs {
		if value := normalizeWaylandDisplay(processEnv["WAYLAND_DISPLAY"], runtimeDir); value != "" {
			return value
		}
	}
	if runtimeDir == "" {
		return ""
	}
	return firstWaylandSocket(runtimeDir)
}

func normalizeWaylandDisplay(value, runtimeDir string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if runtimeDir == "" {
		return value
	}
	if filepath.IsAbs(value) {
		if isSocket(value) {
			return value
		}
		return ""
	}
	if isSocket(filepath.Join(runtimeDir, value)) {
		return value
	}
	return ""
}

func firstWaylandSocket(runtimeDir string) string {
	for _, name := range []string{"wayland-0", "wayland-1"} {
		if isSocket(filepath.Join(runtimeDir, name)) {
			return name
		}
	}
	matches, err := filepath.Glob(filepath.Join(runtimeDir, "wayland-*"))
	if err != nil {
		return ""
	}
	sort.Strings(matches)
	for _, match := range matches {
		if strings.HasSuffix(match, ".lock") {
			continue
		}
		if isSocket(match) {
			return filepath.Base(match)
		}
	}
	return ""
}

func firstSessionValue(processEnvs []map[string]string, key string) string {
	for _, processEnv := range processEnvs {
		if value := strings.TrimSpace(processEnv[key]); value != "" {
			return value
		}
	}
	return ""
}

type rankedProcessEnv struct {
	env  map[string]string
	rank int
	pid  int
}

func desktopProcessEnvironments(uid int) []map[string]string {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}

	var candidates []rankedProcessEnv
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		procDir := filepath.Join("/proc", entry.Name())
		if !pathOwnedByUID(procDir, uid) {
			continue
		}
		rank := desktopProcessRank(processSearchText(procDir))
		if rank == 0 {
			continue
		}
		processEnv := readProcEnviron(procDir)
		if !hasSessionEnvSignal(processEnv) {
			continue
		}
		candidates = append(candidates, rankedProcessEnv{
			env:  processEnv,
			rank: rank + sessionEnvRank(processEnv),
			pid:  pid,
		})
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].rank != candidates[j].rank {
			return candidates[i].rank > candidates[j].rank
		}
		return candidates[i].pid < candidates[j].pid
	})

	results := make([]map[string]string, 0, len(candidates))
	for _, candidate := range candidates {
		results = append(results, candidate.env)
	}
	return results
}

func processSearchText(procDir string) string {
	var parts []string
	if data, err := os.ReadFile(filepath.Join(procDir, "comm")); err == nil {
		parts = append(parts, string(bytes.TrimSpace(data)))
	}
	if data, err := os.ReadFile(filepath.Join(procDir, "cmdline")); err == nil {
		data = bytes.Trim(data, "\x00")
		parts = append(parts, strings.ReplaceAll(string(data), "\x00", " "))
	}
	return strings.ToLower(strings.Join(parts, " "))
}

func desktopProcessRank(text string) int {
	patterns := []struct {
		needle string
		rank   int
	}{
		{"gnome-session", 100},
		{"gnome-shell", 95},
		{"plasmashell", 95},
		{"kwin_wayland", 95},
		{"kwin_x11", 95},
		{"startplasma", 95},
		{"cinnamon-session", 95},
		{"mate-session", 95},
		{"xfce4-session", 95},
		{"lxqt-session", 95},
		{"sway", 95},
		{"wayfire", 95},
		{"xorg", 80},
		{"xwayland", 75},
		{"gnome-terminal-server", 65},
		{"ptyxis", 65},
		{"kgx", 65},
		{"konsole", 65},
		{"xfce4-terminal", 65},
		{"alacritty", 65},
		{"wezterm", 65},
		{"kitty", 65},
		{"tilix", 65},
		{"codex", 50},
		{"dbus-daemon", 45},
		{"systemd --user", 40},
	}

	rank := 0
	for _, pattern := range patterns {
		if strings.Contains(text, pattern.needle) && pattern.rank > rank {
			rank = pattern.rank
		}
	}
	return rank
}

func sessionEnvRank(env map[string]string) int {
	rank := 0
	for _, key := range []string{"XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS"} {
		if strings.TrimSpace(env[key]) != "" {
			rank += 20
		}
	}
	for _, key := range []string{"DISPLAY", "WAYLAND_DISPLAY"} {
		if strings.TrimSpace(env[key]) != "" {
			rank += 10
		}
	}
	if strings.TrimSpace(env["XAUTHORITY"]) != "" {
		rank += 5
	}
	return rank
}

func hasSessionEnvSignal(env map[string]string) bool {
	for _, key := range []string{
		"XDG_RUNTIME_DIR",
		"DBUS_SESSION_BUS_ADDRESS",
		"DISPLAY",
		"WAYLAND_DISPLAY",
		"XAUTHORITY",
		"AT_SPI_BUS_ADDRESS",
	} {
		if strings.TrimSpace(env[key]) != "" {
			return true
		}
	}
	return false
}

func readProcEnviron(procDir string) map[string]string {
	data, err := os.ReadFile(filepath.Join(procDir, "environ"))
	if err != nil {
		return nil
	}
	return parseNullEnv(data)
}

func parseNullEnv(data []byte) map[string]string {
	env := map[string]string{}
	for _, entry := range bytes.Split(data, []byte{0}) {
		if len(entry) == 0 {
			continue
		}
		key, value, ok := strings.Cut(string(entry), "=")
		if ok && key != "" {
			env[key] = value
		}
	}
	return env
}

func envSliceToMap(items []string) map[string]string {
	env := map[string]string{}
	for _, item := range items {
		key, value, ok := strings.Cut(item, "=")
		if ok && key != "" {
			env[key] = value
		}
	}
	return env
}

func envMapToSlice(base []string, env map[string]string) []string {
	items := make([]string, 0, len(env))
	seen := map[string]bool{}
	for _, item := range base {
		key, _, ok := strings.Cut(item, "=")
		if !ok || key == "" {
			items = append(items, item)
			continue
		}
		if value, ok := env[key]; ok {
			items = append(items, key+"="+value)
			seen[key] = true
		}
	}

	var added []string
	for key := range env {
		if !seen[key] {
			added = append(added, key)
		}
	}
	sort.Strings(added)
	for _, key := range added {
		items = append(items, key+"="+env[key])
	}
	return items
}

func validRuntimeDir(path string, uid int) bool {
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	return int(stat.Uid) == uid
}

func pathOwnedByUID(path string, uid int) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	return int(stat.Uid) == uid
}

func isSocket(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode()&os.ModeSocket != 0
}

func requiredString(args map[string]any, key string) string {
	value, _ := args[key].(string)
	return strings.TrimSpace(value)
}

func optionalString(args map[string]any, key string) string {
	value, _ := args[key].(string)
	return value
}

func requiredElementIndex(args map[string]any) string {
	return strings.TrimSpace(optionalElementIndex(args))
}

func optionalElementIndex(args map[string]any) string {
	return elementIndexString(args["element_index"])
}

func elementIndexString(value any) string {
	switch value := value.(type) {
	case string:
		return value
	case json.Number:
		if integer, err := value.Int64(); err == nil {
			return strconv.FormatInt(integer, 10)
		}
		if float, err := value.Float64(); err == nil {
			return integerElementIndexFloat(float)
		}
	case float64:
		return integerElementIndexFloat(value)
	case int:
		return strconv.Itoa(value)
	case int64:
		return strconv.FormatInt(value, 10)
	}
	return ""
}

func integerElementIndexFloat(value float64) string {
	if math.IsNaN(value) || math.IsInf(value, 0) || math.Trunc(value) != value {
		return ""
	}
	return strconv.FormatInt(int64(value), 10)
}

func requiredFloat(args map[string]any, key string) *float64 {
	return optionalFloat(args, key)
}

func optionalFloat(args map[string]any, key string) *float64 {
	switch value := args[key].(type) {
	case float64:
		return &value
	case int:
		float := float64(value)
		return &float
	case json.Number:
		float, err := value.Float64()
		if err == nil {
			return &float
		}
	}
	return nil
}

func optionalTextLimit(args map[string]any, key string) (*textLimit, error) {
	value, ok := args[key]
	if !ok {
		return nil, nil
	}
	return textLimitFromValue(value, key)
}

func textLimitFromValue(value any, key string) (*textLimit, error) {
	if stringValue, ok := value.(string); ok {
		if strings.EqualFold(stringValue, "max") {
			return &textLimit{max: true}, nil
		}
		return nil, fmt.Errorf("%s must be a positive integer or max", key)
	}
	integer, err := positiveIntFromValue(value, key)
	if err != nil {
		return nil, fmt.Errorf("%s must be a positive integer or max", key)
	}
	return &textLimit{count: *integer}, nil
}

func optionalPositiveInt(args map[string]any, key string) (*int, error) {
	value, ok := args[key]
	if !ok {
		return nil, nil
	}
	return positiveIntFromValue(value, key)
}

func positiveIntFromValue(value any, key string) (*int, error) {
	switch typed := value.(type) {
	case int:
		return positiveIntFromInt64(int64(typed), key)
	case float64:
		if !isWholeNumber(typed) {
			return nil, fmt.Errorf("%s must be a positive integer", key)
		}
		return positiveIntFromFloat64(typed, key)
	case json.Number:
		integer, err := typed.Int64()
		if err != nil {
			return nil, fmt.Errorf("%s must be a positive integer", key)
		}
		return positiveIntFromInt64(integer, key)
	default:
		return nil, fmt.Errorf("%s must be a positive integer", key)
	}
}

func positiveIntFromFloat64(value float64, key string) (*int, error) {
	if !isWholeNumber(value) || value <= 0 || value > float64(maxInt()) {
		return nil, fmt.Errorf("%s must be a positive integer", key)
	}
	integer := int(value)
	return &integer, nil
}

func positiveIntFromInt64(value int64, key string) (*int, error) {
	if value <= 0 || value > int64(maxInt()) {
		return nil, fmt.Errorf("%s must be a positive integer", key)
	}
	integer := int(value)
	return &integer, nil
}

func isWholeNumber(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && math.Trunc(value) == value
}

func maxInt() int {
	return int(^uint(0) >> 1)
}

func intValue(value *float64, fallback int) int {
	if value == nil {
		return fallback
	}
	return int(*value)
}

func floatValue(value *float64, fallback float64) float64 {
	if value == nil {
		return fallback
	}
	return *value
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func parseClickMethod(value string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(value))
	if normalized == "" {
		return "auto", nil
	}
	for _, candidate := range clickMethodValues {
		if normalized == candidate {
			return normalized, nil
		}
	}
	return "", fmt.Errorf("Invalid click_method %q. Expected one of: %s", value, strings.Join(clickMethodValues, ", "))
}

func globalPointerFallbacksEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("OPEN_COMPUTER_USE_ALLOW_GLOBAL_POINTER_FALLBACKS"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func toolDefinitions() []toolDefinition {
	return []toolDefinition{
		{
			Name:        "click",
			Description: "Click an element by index or pixel coordinates from screenshot. This tool is part of plugin `Computer Use`.",
			Annotations: defaultAnnotations(),
			InputSchema: objectSchema(map[string]any{
				"app":           stringProperty("App name or bundle identifier"),
				"element_index": stringProperty("Element index to click"),
				"x":             numberProperty("X coordinate in screenshot pixel coordinates"),
				"y":             numberProperty("Y coordinate in screenshot pixel coordinates"),
				"click_count":   integerProperty("Number of clicks. Defaults to 1"),
				"mouse_button":  enumStringProperty("Mouse button to click. Defaults to left.", []string{"left", "right", "middle"}),
				"click_method":  enumStringProperty("Click implementation: auto (default), accessibility, app_post, sky_click, or global. Accessibility requires element_index. Linux supports global AT-SPI mouse synthesis and does not currently support app_post or sky_click.", clickMethodValues),
			}, []string{"app"}),
		},
		{
			Name:        "drag",
			Description: "Drag from one point to another using pixel coordinates. This tool is part of plugin `Computer Use`.",
			Annotations: defaultAnnotations(),
			InputSchema: objectSchema(map[string]any{
				"app":    stringProperty("App name or bundle identifier"),
				"from_x": numberProperty("Start X coordinate"),
				"from_y": numberProperty("Start Y coordinate"),
				"to_x":   numberProperty("End X coordinate"),
				"to_y":   numberProperty("End Y coordinate"),
			}, []string{"app", "from_x", "from_y", "to_x", "to_y"}),
		},
		{
			Name:        "get_app_state",
			Description: "Get the state of an already running app's key window and return a screenshot and accessibility tree. This must be called once per assistant turn before interacting with the app. This tool is part of plugin `Computer Use`.",
			Annotations: readOnlyAnnotations(),
			InputSchema: objectSchema(map[string]any{
				"app":            stringProperty("App name or bundle identifier"),
				"text_limit":     textLimitProperty("Maximum text characters to return. Use \"max\" for full text. Defaults to 500."),
				"max_tree_nodes": positiveIntegerProperty("Maximum accessibility tree nodes to render. Defaults to 1200."),
				"max_tree_depth": positiveIntegerProperty("Maximum accessibility tree depth to render. Defaults to 64."),
			}, []string{"app"}),
		},
		{
			Name:        "list_apps",
			Description: "List the apps on this computer. Returns the set of apps that are currently running, as well as any that have been used in the last 14 days, including details on usage frequency. This tool is part of plugin `Computer Use`.",
			Annotations: readOnlyAnnotations(),
			InputSchema: objectSchema(map[string]any{}, nil),
		},
		{
			Name:        "perform_secondary_action",
			Description: "Invoke a secondary accessibility action exposed by an element. This tool is part of plugin `Computer Use`.",
			Annotations: defaultAnnotations(),
			InputSchema: objectSchema(map[string]any{
				"app":           stringProperty("App name or bundle identifier"),
				"element_index": stringProperty("Element identifier"),
				"action":        stringProperty("Secondary accessibility action name"),
			}, []string{"app", "element_index", "action"}),
		},
		{
			Name:        "press_key",
			Description: "Press a key or key-combination on the keyboard, including modifier and navigation keys.\n  - This supports xdotool's `key` syntax.\n  - Examples: \"a\", \"Return\", \"Tab\", \"super+c\", \"Up\", \"KP_0\" (for the numpad 0). This tool is part of plugin `Computer Use`.",
			Annotations: defaultAnnotations(),
			InputSchema: objectSchema(map[string]any{
				"app": stringProperty("App name or bundle identifier"),
				"key": stringProperty("Key or key-combination to press"),
			}, []string{"app", "key"}),
		},
		{
			Name:        "scroll",
			Description: "Scroll an element in a direction by a number of pages. This tool is part of plugin `Computer Use`.",
			Annotations: defaultAnnotations(),
			InputSchema: objectSchema(map[string]any{
				"app":           stringProperty("App name or bundle identifier"),
				"direction":     stringProperty("Scroll direction: up, down, left, or right"),
				"element_index": stringProperty("Element identifier"),
				"pages":         numberProperty("Number of pages to scroll. Fractional values are supported. Defaults to 1"),
			}, []string{"app", "element_index", "direction"}),
		},
		{
			Name:        "set_value",
			Description: "Set the value of a settable accessibility element. This tool is part of plugin `Computer Use`.",
			Annotations: defaultAnnotations(),
			InputSchema: objectSchema(map[string]any{
				"app":           stringProperty("App name or bundle identifier"),
				"element_index": stringProperty("Element identifier"),
				"value":         stringProperty("Value to assign"),
			}, []string{"app", "element_index", "value"}),
		},
		{
			Name:        "type_text",
			Description: "Type literal text using keyboard input. This tool is part of plugin `Computer Use`.",
			Annotations: defaultAnnotations(),
			InputSchema: objectSchema(map[string]any{
				"app":  stringProperty("App name or bundle identifier"),
				"text": stringProperty("Literal text to type"),
			}, []string{"app", "text"}),
		},
	}
}

// toolDefinitionsForEra returns the catalog for the requested era. The legacy
// catalog is the exact current toolDefinitions() output. The modern catalog is
// the same nine tools in the same order, with the two capture tools carrying
// snapshot_ref-aware descriptions and the seven action tools carrying a required
// snapshot_ref argument; the era mechanic (which tools, and appending snapshot_ref
// last to required) lives in the shared gomcp package. Each call rebuilds fresh
// maps, so mutating the modern copy never affects the legacy catalog.
func toolDefinitionsForEra(modern bool) []toolDefinition {
	defs := toolDefinitions()
	if !modern {
		return defs
	}
	for i := range defs {
		switch defs[i].Name {
		case "get_app_state":
			defs[i].Description = modernGetAppStateDescription
		case "list_apps":
			defs[i].Description = modernListAppsDescription
		default:
			if gomcp.IsModernActionTool(defs[i].Name) {
				gomcp.AddSnapshotRefRequirement(defs[i].InputSchema)
			}
		}
	}
	return defs
}

// modernStateResult builds the modern get_app_state tool result: the existing
// rendered text with a snapshot_ref line prepended, the screenshot, and the
// pinned structuredContent block. The state values are injected here; real handle
// minting and wire-level emission land with M3, so this is exercised only by unit
// tests until then.
func (s *appSnapshot) modernStateResult(state gomcp.StructuredState) toolCallResult {
	result := s.result()
	if len(result.Content) > 0 && result.Content[0].Type == "text" {
		result.Content[0].Text = "snapshot_ref: " + state.SnapshotRef + "\n" + result.Content[0].Text
	}
	result.StructuredContent = state.StructuredContent()
	return result
}

// snapshotTargetIdentity is the normalized app identity that keys a store target:
// the bundle/executable identity when present, otherwise the app name, lowercased
// to match the legacy snapshot key normalization.
func snapshotTargetIdentity(snapshot *appSnapshot) string {
	identity := snapshot.App.BundleIdentifier
	if strings.TrimSpace(identity) == "" {
		identity = snapshot.App.Name
	}
	return strings.ToLower(strings.TrimSpace(identity))
}

// snapshotState assembles the modern structuredContent values from a minted
// record's fields and stored metadata.
func snapshotState(handle string, createdAt, expiresAt time.Time, generation int, meta gomcp.SnapshotMeta) gomcp.StructuredState {
	return gomcp.StructuredState{
		SnapshotRef:      handle,
		CapturedAt:       rfc3339UTC(createdAt),
		ExpiresAt:        rfc3339UTC(expiresAt),
		Generation:       generation,
		AppName:          meta.AppName,
		BundleIdentifier: optString(meta.BundleIdentifier),
		PID:              meta.PID,
		WindowID:         optString(meta.WindowID),
		Bounds:           meta.Bounds,
		ScreenshotPixels: meta.ScreenshotPixels,
	}
}

func rfc3339UTC(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func optString(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

func rectFromFrame(f *frame) gomcp.Rect {
	if f == nil {
		return gomcp.Rect{}
	}
	return gomcp.Rect{X: f.X, Y: f.Y, Width: f.Width, Height: f.Height}
}

// pngPixelSize reads the screenshot's pixel dimensions from the PNG IHDR header.
// The Linux runtime returns only base64 PNG bytes with no explicit dimensions, so
// the width and height are derived from the header here. It returns nil when the
// screenshot is absent or not a parseable PNG.
func pngPixelSize(b64 string) *gomcp.Size {
	if b64 == "" {
		return nil
	}
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || len(data) < 24 {
		return nil
	}
	// PNG signature (8 bytes) + IHDR chunk length (4, always 13) + "IHDR" (4)
	// then the big-endian width (4) and height (4). Validate the signature and
	// chunk length so a non-PNG blob with a coincidental "IHDR" at offset 12 is
	// rejected rather than yielding bogus dimensions.
	if !bytes.Equal(data[0:8], []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}) {
		return nil
	}
	if binary.BigEndian.Uint32(data[8:12]) != 13 || string(data[12:16]) != "IHDR" {
		return nil
	}
	width := int(binary.BigEndian.Uint32(data[16:20]))
	height := int(binary.BigEndian.Uint32(data[20:24]))
	if width <= 0 || height <= 0 {
		return nil
	}
	return &gomcp.Size{Width: width, Height: height}
}

func objectSchema(properties map[string]any, required []string) map[string]any {
	schema := map[string]any{
		"type":                 "object",
		"properties":           properties,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func defaultAnnotations() map[string]any {
	return map[string]any{"destructiveHint": false, "openWorldHint": false}
}

func readOnlyAnnotations() map[string]any {
	return map[string]any{"destructiveHint": false, "idempotentHint": true, "openWorldHint": false, "readOnlyHint": true}
}

func stringProperty(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

func enumStringProperty(description string, values []string) map[string]any {
	property := stringProperty(description)
	property["enum"] = values
	return property
}

func numberProperty(description string) map[string]any {
	return map[string]any{"type": "number", "description": description}
}

func integerProperty(description string) map[string]any {
	return map[string]any{"type": "integer", "description": description}
}

func positiveIntegerProperty(description string) map[string]any {
	return map[string]any{"type": "integer", "minimum": 1, "description": description}
}

func textLimitProperty(description string) map[string]any {
	return map[string]any{
		"anyOf": []any{
			map[string]any{"type": "integer", "minimum": 1},
			map[string]any{"type": "string", "enum": []string{"max"}},
		},
		"description": description,
	}
}

func main() {
	if err := runCLI(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runCLI(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(stdout, helpText(""))
		return nil
	}

	switch args[0] {
	case "-h", "--help", "help":
		topic := ""
		if len(args) > 1 {
			topic = args[1]
		}
		fmt.Fprint(stdout, helpText(topic))
		return nil
	case "-v", "--version", "version":
		fmt.Fprintln(stdout, version)
		return nil
	case "mcp":
		return runMCP(os.Stdin, stdout)
	case "doctor":
		fmt.Fprintln(stdout, "Linux runtime: AT-SPI2 and GDK run against the signed-in desktop user's accessibility session. When Codex starts without XDG_RUNTIME_DIR, DBUS_SESSION_BUS_ADDRESS, or display variables, open-computer-use tries to discover the same user's session from /run/user/<uid> and desktop processes.")
		return nil
	case "list-apps":
		result := newService().callTool("list_apps", map[string]any{}, false)
		if result.IsError {
			return errors.New(result.Content[0].Text)
		}
		fmt.Fprintln(stdout, result.Content[0].Text)
		return nil
	case "snapshot":
		app, textLimit, maxTreeNodes, maxTreeDepth, err := parseSnapshotArgs(args[1:])
		if err != nil {
			return err
		}
		toolArgs := map[string]any{
			"app": app,
		}
		if textLimit != nil {
			toolArgs["text_limit"] = textLimit.runtimeValue()
		}
		if maxTreeNodes != nil {
			toolArgs["max_tree_nodes"] = *maxTreeNodes
		}
		if maxTreeDepth != nil {
			toolArgs["max_tree_depth"] = *maxTreeDepth
		}
		result := newService().callTool("get_app_state", toolArgs, false)
		if result.IsError {
			return errors.New(result.Content[0].Text)
		}
		fmt.Fprintln(stdout, result.Content[0].Text)
		return nil
	case "call":
		output, hasError, err := runCallCommand(args[1:], newService())
		if err != nil {
			return err
		}
		encoded, err := json.MarshalIndent(output, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(stdout, string(encoded))
		if hasError {
			return errors.New("tool call returned isError=true")
		}
		return nil
	default:
		return fmt.Errorf("unknown command: %s\n\n%s", args[0], helpText(""))
	}
}

func parseSnapshotArgs(args []string) (string, *textLimit, *int, *int, error) {
	var app string
	var textLimit *textLimit
	var maxTreeNodes *int
	var maxTreeDepth *int
	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch arg {
		case "--text-limit":
			index++
			if index >= len(args) {
				return "", nil, nil, nil, errors.New("--text-limit requires a positive integer or max value")
			}
			value, err := parseTextLimitOption(args[index], "--text-limit")
			if err != nil {
				return "", nil, nil, nil, err
			}
			textLimit = value
		case "--max-tree-nodes":
			index++
			if index >= len(args) {
				return "", nil, nil, nil, errors.New("--max-tree-nodes requires a positive integer value")
			}
			value, err := parsePositiveIntegerOption(args[index], "--max-tree-nodes")
			if err != nil {
				return "", nil, nil, nil, err
			}
			maxTreeNodes = &value
		case "--max-tree-depth":
			index++
			if index >= len(args) {
				return "", nil, nil, nil, errors.New("--max-tree-depth requires a positive integer value")
			}
			value, err := parsePositiveIntegerOption(args[index], "--max-tree-depth")
			if err != nil {
				return "", nil, nil, nil, err
			}
			maxTreeDepth = &value
		default:
			if strings.HasPrefix(arg, "-") {
				return "", nil, nil, nil, fmt.Errorf("unknown snapshot option: %s", arg)
			}
			if app != "" {
				return "", nil, nil, nil, errors.New("snapshot accepts exactly one app name, process name, window title, or pid")
			}
			app = arg
		}
	}
	if app == "" {
		return "", nil, nil, nil, errors.New("snapshot requires an app name, process name, window title, or pid")
	}
	return app, textLimit, maxTreeNodes, maxTreeDepth, nil
}

func parseTextLimitOption(value, option string) (*textLimit, error) {
	if strings.EqualFold(value, "max") {
		return &textLimit{max: true}, nil
	}
	integer, err := strconv.Atoi(value)
	if err != nil || integer <= 0 {
		return nil, fmt.Errorf("%s must be a positive integer or max", option)
	}
	return &textLimit{count: integer}, nil
}

func parsePositiveIntegerOption(value, option string) (int, error) {
	integer, err := strconv.Atoi(value)
	if err != nil || integer <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", option)
	}
	return integer, nil
}

func runCallCommand(args []string, svc *service) (any, bool, error) {
	if len(args) == 0 {
		return nil, false, errors.New("call requires a tool name or --calls/--calls-file")
	}

	var toolName, argsJSON, argsFile, callsJSON, callsFile string
	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch arg {
		case "--args":
			index++
			if index >= len(args) {
				return nil, false, errors.New("--args requires a value")
			}
			argsJSON = args[index]
		case "--args-file":
			index++
			if index >= len(args) {
				return nil, false, errors.New("--args-file requires a value")
			}
			argsFile = args[index]
		case "--calls":
			index++
			if index >= len(args) {
				return nil, false, errors.New("--calls requires a value")
			}
			callsJSON = args[index]
		case "--calls-file":
			index++
			if index >= len(args) {
				return nil, false, errors.New("--calls-file requires a value")
			}
			callsFile = args[index]
		default:
			if strings.HasPrefix(arg, "-") {
				return nil, false, fmt.Errorf("unknown call option: %s", arg)
			}
			if toolName != "" {
				return nil, false, errors.New("call accepts at most one tool name")
			}
			toolName = arg
		}
	}

	if callsJSON != "" || callsFile != "" {
		if toolName != "" || argsJSON != "" || argsFile != "" {
			return nil, false, errors.New("call sequence does not accept a tool name, --args, or --args-file")
		}
		calls, err := readCallSequence(callsJSON, callsFile)
		if err != nil {
			return nil, false, err
		}
		outputs, hasError := executeCallSequence(svc, calls, strictSnapshotsEnabled())
		return outputs, hasError, nil
	}

	if toolName == "" {
		return nil, false, errors.New("call requires a tool name or --calls/--calls-file")
	}
	arguments, err := readArguments(argsJSON, argsFile)
	if err != nil {
		return nil, false, err
	}
	// A single call runs the same per-call era decision as a batch, with no prior
	// successor to thread: an explicit snapshot_ref (or strict mode) selects the
	// modern era; a legacy single call (no ref, no env) stays modern=false and
	// byte-identical.
	modern, arguments := decideCall(toolName, arguments, strictSnapshotsEnabled(), "")
	result := svc.callTool(toolName, arguments, modern)
	return result, result.IsError, nil
}

type callSpec struct {
	Tool string
	Args map[string]any
}

// strictSnapshotsEnabled reports whether the opt-in strict snapshot mode is on.
// It mirrors the truthy parsing used by the other environment gates in this app.
// In strict mode the CLI batch path runs get_app_state and every action call on
// the modern era so an action that lacks a snapshot_ref (and cannot be
// auto-threaded from a prior successor) fails with the pinned missing-ref message
// rather than silently using implicit legacy state.
func strictSnapshotsEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("OPEN_COMPUTER_USE_STRICT_SNAPSHOTS"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// executeCallSequence runs a CLI batch against one shared in-process service,
// stopping at the first isError result. It threads the modern snapshot handle:
//
//	(a) a call whose args carry an explicit snapshot_ref dispatches modern;
//	(b) strict mode dispatches get_app_state and every action call modern, so an
//	    action lacking a ref that cannot be auto-threaded surfaces the pinned
//	    missing-ref error from the modern action transaction;
//	(c) auto-threading fills the latest successor snapshot_ref (from a prior
//	    modern result's structuredContent) into a later action call that omits it;
//	(d) a legacy batch (no refs, no strict mode) mints nothing and threads
//	    nothing: every call dispatches modern=false, byte-identical to before.
func executeCallSequence(svc *service, calls []callSpec, strict bool) ([]map[string]any, bool) {
	var outputs []map[string]any
	hasError := false
	latestRef := ""
	for _, call := range calls {
		modern, args := decideCall(call.Tool, call.Args, strict, latestRef)
		result := svc.callTool(call.Tool, args, modern)
		outputs = append(outputs, map[string]any{"tool": call.Tool, "result": result})
		if ref, ok := gomcp.SuccessorRef(result.StructuredContent); ok {
			latestRef = ref
		}
		if result.IsError {
			hasError = true
			break
		}
	}
	return outputs, hasError
}

// decideCall picks the era for one call and the args to dispatch, given the
// latest successor snapshot_ref threaded so far (empty for a single call). It is
// the shared per-call decision behind both the single-call and batch CLI paths:
//
//	(a) an explicit snapshot_ref binds the call to the modern era;
//	(c) an action tool with a threadable latest successor gets it auto-filled;
//	(b) strict mode runs get_app_state and action tools modern (an action with
//	    nothing to thread then surfaces the pinned missing-ref error);
//	(d) otherwise the call stays legacy (modern=false) with args unchanged.
func decideCall(tool string, args map[string]any, strict bool, latestRef string) (bool, map[string]any) {
	switch {
	case hasSnapshotRefArg(args):
		return true, args
	case gomcp.IsModernActionTool(tool):
		switch {
		case latestRef != "":
			return true, withSnapshotRef(args, latestRef)
		case strict:
			return true, args
		}
	case strict && tool == "get_app_state":
		return true, args
	}
	return false, args
}

// hasSnapshotRefArg reports whether a call carries a usable explicit snapshot_ref.
func hasSnapshotRefArg(args map[string]any) bool {
	_, present := snapshotRefArg(args)
	return present
}

// withSnapshotRef returns a shallow copy of args with snapshot_ref set to ref,
// leaving the caller's map untouched.
func withSnapshotRef(args map[string]any, ref string) map[string]any {
	clone := make(map[string]any, len(args)+1)
	for key, value := range args {
		clone[key] = value
	}
	clone[gomcp.SnapshotRefKey] = ref
	return clone
}

func readArguments(inline, file string) (map[string]any, error) {
	if inline != "" && file != "" {
		return nil, errors.New("Use either inline JSON or a JSON file, not both")
	}
	if inline == "" && file == "" {
		return map[string]any{}, nil
	}
	source, err := readJSONSource(inline, file)
	if err != nil {
		return nil, err
	}
	var args map[string]any
	decoder := json.NewDecoder(strings.NewReader(source))
	decoder.UseNumber()
	if err := decoder.Decode(&args); err != nil {
		return nil, fmt.Errorf("Invalid JSON input: %w", err)
	}
	if args == nil {
		return nil, errors.New("--args must be a JSON object")
	}
	return args, nil
}

func readCallSequence(inline, file string) ([]callSpec, error) {
	if inline != "" && file != "" {
		return nil, errors.New("Use either --calls or --calls-file, not both")
	}
	source, err := readJSONSource(inline, file)
	if err != nil {
		return nil, err
	}
	var raw []map[string]any
	decoder := json.NewDecoder(strings.NewReader(source))
	decoder.UseNumber()
	if err := decoder.Decode(&raw); err != nil {
		return nil, fmt.Errorf("Invalid JSON input: %w", err)
	}
	calls := make([]callSpec, 0, len(raw))
	for index, item := range raw {
		name, _ := item["tool"].(string)
		if name == "" {
			name, _ = item["name"].(string)
		}
		if name == "" {
			return nil, fmt.Errorf("call sequence item #%d requires a non-empty tool", index+1)
		}
		args, _ := item["args"].(map[string]any)
		if args == nil {
			args, _ = item["arguments"].(map[string]any)
		}
		if args == nil {
			args = map[string]any{}
		}
		calls = append(calls, callSpec{Tool: name, Args: args})
	}
	return calls, nil
}

func readJSONSource(inline, file string) (string, error) {
	if inline != "" {
		return inline, nil
	}
	if file == "" {
		return "", errors.New("JSON input is required")
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// mcpServer builds a stdio MCP server bound to a fresh service and this app's
// protocol seams (instructions, version, tool catalog, tool dispatch). The
// shared gomcp package owns era classification, the modern envelope, response
// decoration, server/discover, and the stdio loop.
func mcpServer() *gomcp.Server {
	svc := newService()
	return gomcp.NewServer(gomcp.Hooks{
		Instructions:       serverInstructions,
		ModernInstructions: modernServerInstructions,
		Version:            version,
		ToolCatalog:        func(modern bool) any { return toolDefinitionsForEra(modern) },
		CallTool:           func(name string, args map[string]any, modern bool) any { return svc.callTool(name, args, modern) },
	})
}

func runMCP(stdin io.Reader, stdout io.Writer) error {
	return mcpServer().Run(stdin, stdout)
}

func helpText(command string) string {
	switch command {
	case "mcp":
		return "Usage:\n  open-computer-use mcp\n\nStart the stdio MCP server.\n"
	case "call":
		return "Usage:\n  open-computer-use call <tool> [--args '<json-object>']\n  open-computer-use call --calls '<json-array>'\n\nThe JSON array form keeps all calls in one process so element_index state can be reused.\n"
	case "snapshot":
		return "Usage:\n  open-computer-use snapshot [--text-limit <positive-int|max>] [--max-tree-nodes <positive-int>] [--max-tree-depth <positive-int>] <app>\n\nPrint the current Linux AT-SPI snapshot for the target app.\n"
	default:
		return `Open Computer Use for Linux

Usage:
  open-computer-use [command] [options]

Commands:
  mcp                  Start the stdio MCP server.
  doctor               Print Linux runtime notes.
  list-apps            Print running apps with top-level windows.
  snapshot <app>       Print the current AT-SPI snapshot for an app.
  call <tool>           Call one tool, or run a JSON array of tool calls.
  help [command]       Show general or command-specific help.
  version              Print the CLI version.

Notes:
  The Linux runtime uses AT-SPI2 semantic actions first, then best-effort
  coordinate/key synthesis. Run it in the signed-in desktop session.
`
	}
}
