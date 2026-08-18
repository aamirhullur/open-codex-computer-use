package gomcp

// Era-specific tool catalog mechanics shared by the Go apps. The platform apps
// own the tool descriptions and assemble each catalog; this file owns the
// cross-platform rule that turns the legacy catalog into the modern one: the
// seven action tools gain a required snapshot_ref argument with a pinned,
// byte-identical description. list_apps and get_app_state never take a handle.

// SnapshotRefKey is the argument name every modern action tool requires.
const SnapshotRefKey = "snapshot_ref"

// SnapshotRefDescription is the pinned, cross-platform description of the
// snapshot_ref action argument. It is byte-identical on every platform and every
// action tool; do not reword it.
const SnapshotRefDescription = "Snapshot reference returned by the immediately preceding get_app_state or action result for this app."

// actionTools is the set of modern tools that require snapshot_ref. Exactly
// these seven; list_apps and get_app_state are excluded.
var actionTools = map[string]bool{
	"click":                    true,
	"drag":                     true,
	"perform_secondary_action": true,
	"press_key":                true,
	"scroll":                   true,
	"set_value":                true,
	"type_text":                true,
}

// IsModernActionTool reports whether the named tool requires snapshot_ref in the
// modern catalog.
func IsModernActionTool(name string) bool { return actionTools[name] }

// SnapshotRefProperty returns a fresh copy of the pinned snapshot_ref JSON-schema
// property.
func SnapshotRefProperty() map[string]any {
	return map[string]any{"type": "string", "description": SnapshotRefDescription}
}

// AddSnapshotRefRequirement appends the snapshot_ref property to the schema's
// properties map and appends "snapshot_ref" last to its required list, mutating
// the given JSON-schema object in place. This is the shared era mechanic the apps
// apply to each action tool while assembling the modern catalog; the properties
// map ordering is irrelevant on the wire (JSON sorts map keys) while required is
// a slice whose order is preserved, so snapshot_ref lands after the existing
// required keys.
func AddSnapshotRefRequirement(inputSchema map[string]any) {
	props, ok := inputSchema["properties"].(map[string]any)
	if !ok {
		props = map[string]any{}
		inputSchema["properties"] = props
	}
	props[SnapshotRefKey] = SnapshotRefProperty()

	req, _ := inputSchema["required"].([]string)
	inputSchema["required"] = append(req, SnapshotRefKey)
}
